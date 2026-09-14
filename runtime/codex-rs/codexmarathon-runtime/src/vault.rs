//! Opaque authentication snapshots and their protected vault.
//!
//! The native domain validates only the JSON envelope and optional account
//! identity. Provider-specific token fields remain uninterpreted bytes. The
//! wrapper intentionally has a redacted `Debug` implementation and is not
//! `Serialize`, which prevents accidental journal, tracing, or RPC leakage.

use crate::accounts::validate_account_id;
use crate::errors::{DomainError, DomainResult};
use crate::persistence::{
    atomic_write, ensure_private_dir, open_regular, reject_symlink_components, reject_unsafe_file,
    sync_directory,
};
use crate::state_lock::StateLock;
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::fmt;
use std::fs;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use zeroize::{Zeroize, Zeroizing};

pub const MAX_SNAPSHOT_BYTES: usize = 1024 * 1024;

fn generated_ref(key: &str) -> bool {
    key.len() == 102
        && key.starts_with("cmv1-")
        && key.as_bytes()[37] == b'-'
        && key[5..37].bytes().all(|b| b.is_ascii_hexdigit())
        && key[38..].bytes().all(|b| b.is_ascii_hexdigit())
}

fn check_ref(key: &str, snapshot: &AuthSnapshot) -> DomainResult<()> {
    if generated_ref(key) {
        let id = snapshot
            .embedded_account_id()
            .ok_or(DomainError::SnapshotAccountMismatch)?;
        if key[38..] != format!("{:x}", Sha256::digest(id.as_bytes())) {
            return Err(DomainError::SnapshotAccountMismatch);
        }
    } else {
        AuthSnapshot::for_account(key, snapshot.bytes())?;
    }
    Ok(())
}

/// An opaque auth document returned by the native Codex authentication path.
#[derive(Clone, Eq, PartialEq)]
pub struct AuthSnapshot {
    embedded_account_id: Option<String>,
    bytes: Vec<u8>,
}

impl Drop for AuthSnapshot {
    fn drop(&mut self) {
        self.bytes.zeroize();
    }
}

impl fmt::Debug for AuthSnapshot {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AuthSnapshot")
            .field("embedded_account_id", &self.embedded_account_id)
            .field("bytes", &"<redacted>")
            .finish()
    }
}

impl AuthSnapshot {
    /// Consume a temporary serialization buffer and wipe that allocation after
    /// validating/copying it into the redacted snapshot wrapper.
    pub fn from_owned_bytes(account_id: Option<&str>, bytes: Vec<u8>) -> DomainResult<Self> {
        let bytes = Zeroizing::new(bytes);
        Self::from_bytes(account_id, &bytes)
    }
    /// Validate an opaque object and associate it with a target account.
    ///
    /// `account_id` may be `None` for a legacy auth document that does not
    /// expose an identity. A vault save still requires a concrete key.
    pub fn from_bytes(account_id: Option<&str>, bytes: impl AsRef<[u8]>) -> DomainResult<Self> {
        let bytes = bytes.as_ref();
        if bytes.len() > MAX_SNAPSHOT_BYTES {
            return Err(DomainError::InvalidSnapshot);
        }
        let mut value: Value =
            serde_json::from_slice(bytes).map_err(|_| DomainError::InvalidSnapshot)?;
        let identity = value
            .as_object()
            .ok_or(DomainError::InvalidSnapshot)
            .and_then(extract_account_id);
        wipe_json(&mut value);
        let embedded_account_id = identity?;
        if let Some(account_id) = account_id {
            validate_account_id(account_id)?;
            if embedded_account_id
                .as_deref()
                .is_some_and(|value| value != account_id)
            {
                return Err(DomainError::SnapshotAccountMismatch);
            }
        }
        Ok(Self {
            embedded_account_id,
            bytes: bytes.to_vec(),
        })
    }

    /// Validate a legacy document without assigning a vault key.
    pub fn from_unassigned_bytes(bytes: impl AsRef<[u8]>) -> DomainResult<Self> {
        Self::from_bytes(None, bytes)
    }

    /// Convenience constructor for a concrete account vault entry.
    pub fn for_account(account_id: &str, bytes: impl AsRef<[u8]>) -> DomainResult<Self> {
        Self::from_bytes(Some(account_id), bytes)
    }

    /// Return the identity present in the opaque document, if the provider
    /// format supplied one.
    pub fn embedded_account_id(&self) -> Option<&str> {
        self.embedded_account_id.as_deref()
    }

    /// Borrow the opaque bytes for the native AuthManager boundary.
    pub fn bytes(&self) -> &[u8] {
        &self.bytes
    }

    /// Consume the wrapper and return the opaque bytes to the native boundary.
    pub fn into_bytes(mut self) -> Vec<u8> {
        std::mem::take(&mut self.bytes)
    }

    /// Return a redacted metadata view suitable for diagnostics.
    pub fn metadata(&self) -> SnapshotMetadata {
        SnapshotMetadata {
            embedded_account_id: self.embedded_account_id.clone(),
            byte_len: self.bytes.len(),
        }
    }
}

fn wipe_json(value: &mut Value) {
    match value {
        Value::String(text) => text.zeroize(),
        Value::Array(values) => values.iter_mut().for_each(wipe_json),
        Value::Object(values) => values.values_mut().for_each(wipe_json),
        _ => {}
    }
}

fn extract_account_id(object: &serde_json::Map<String, Value>) -> DomainResult<Option<String>> {
    let top_level = extract_string_field(object.get("account_id"))?;
    let nested = object
        .get("tokens")
        .filter(|tokens| !tokens.is_null())
        .map(|tokens| {
            let tokens = tokens.as_object().ok_or(DomainError::InvalidSnapshot)?;
            extract_string_field(tokens.get("account_id"))
        })
        .transpose()?
        .flatten();
    if top_level.is_some() && nested.is_some() && top_level != nested {
        return Err(DomainError::SnapshotAccountMismatch);
    }
    let identity = top_level.or(nested);
    if let Some(identity) = &identity {
        validate_account_id(identity)?;
    }
    Ok(identity)
}

fn extract_string_field(value: Option<&Value>) -> DomainResult<Option<String>> {
    match value {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) if !value.is_empty() => Ok(Some(value.clone())),
        Some(Value::String(_)) => Err(DomainError::InvalidSnapshot),
        Some(_) => Err(DomainError::InvalidSnapshot),
    }
}

/// Secret-free metadata describing an opaque snapshot.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SnapshotMetadata {
    /// Provider identity if the opaque document exposed one.
    pub embedded_account_id: Option<String>,
    /// Byte length of the document, which is safe for diagnostics.
    pub byte_len: usize,
}

/// Storage-neutral protected snapshot vault.
pub trait SnapshotVault: Send + Sync {
    /// Atomically save or replace a snapshot under an account key.
    fn save(&self, account_id: &str, snapshot: &AuthSnapshot) -> DomainResult<()>;
    /// Read and validate one snapshot.
    fn load(&self, account_id: &str) -> DomainResult<AuthSnapshot>;
    /// Remove one snapshot.
    fn delete(&self, account_id: &str) -> DomainResult<()>;
    /// List keys in deterministic order.
    fn list(&self) -> DomainResult<Vec<String>>;
    /// Test whether a snapshot exists without reading its bytes.
    fn contains(&self, account_id: &str) -> DomainResult<bool>;
}

/// Owner-only, file-backed snapshot vault.
pub struct FileSnapshotVault {
    root: PathBuf,
    gate: Mutex<()>,
}

impl FileSnapshotVault {
    /// Construct a vault without creating its directory.
    pub fn new(root: impl Into<PathBuf>) -> Self {
        Self {
            root: root.into(),
            gate: Mutex::new(()),
        }
    }

    /// Return the vault directory without exposing snapshot contents.
    pub fn root(&self) -> &Path {
        &self.root
    }

    fn path_for(&self, account_id: &str) -> PathBuf {
        self.root.join(format!("{account_id}.json"))
    }

    fn state_dir(&self) -> DomainResult<&Path> {
        self.root.parent().ok_or(DomainError::UnsafePath)
    }

    pub fn load_locked(&self, lock: &StateLock, key: &str) -> DomainResult<AuthSnapshot> {
        lock.check(self.state_dir()?, false)?;
        validate_account_id(key)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::UnsafePath)?;
        let file =
            open_regular(&self.path_for(key), false, false).map_err(|error| match error {
                DomainError::Io(ref io) if io.kind() == std::io::ErrorKind::NotFound => {
                    DomainError::SnapshotNotFound
                }
                other => other,
            })?;
        let mut bytes = Zeroizing::new(Vec::new());
        file.take(MAX_SNAPSHOT_BYTES as u64 + 1)
            .read_to_end(&mut bytes)?;
        let snapshot = AuthSnapshot::from_unassigned_bytes(&bytes)?;
        check_ref(key, &snapshot)?;
        Ok(snapshot)
    }

    pub fn save_locked(
        &self,
        lock: &StateLock,
        key: &str,
        snapshot: &AuthSnapshot,
    ) -> DomainResult<()> {
        lock.check(self.state_dir()?, true)?;
        validate_account_id(key)?;
        check_ref(key, snapshot)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::UnsafePath)?;
        ensure_private_dir(&self.root)?;
        atomic_write(&self.path_for(key), snapshot.bytes())
    }

    /// Write a previously unreferenced snapshot durably. Its random reference
    /// includes a digest binding it to the embedded account identity. Existing
    /// ID-named snapshots retain their original identity validation rules.
    pub fn save_fresh_locked(
        &self,
        lock: &StateLock,
        snapshot: &AuthSnapshot,
    ) -> DomainResult<String> {
        lock.check(self.state_dir()?, true)?;
        let id = snapshot
            .embedded_account_id()
            .ok_or(DomainError::SnapshotAccountMismatch)?;
        let random = rand::random::<[u8; 16]>();
        let nonce: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
        let key = format!("cmv1-{nonce}-{:x}", Sha256::digest(id.as_bytes()));
        if self.path_for(&key).try_exists()? {
            return Err(DomainError::UnsafePath);
        }
        self.save_locked(lock, &key, snapshot)?;
        // Persist creation of the vault directory itself before a registry
        // commit may advertise references inside it.
        sync_directory(self.state_dir()?)?;
        Ok(key)
    }

    /// Only generated, unreferenced entries are collected; legacy filenames
    /// are never guessed to be disposable. Run only under the registry lock.
    pub fn collect_orphans_locked(
        &self,
        lock: &StateLock,
        state: &crate::RegistryState,
    ) -> DomainResult<()> {
        lock.check(self.state_dir()?, true)?;
        reject_symlink_components(&self.root)?;
        let entries = match fs::read_dir(&self.root) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
            Err(error) => return Err(error.into()),
        };
        for entry in entries {
            let entry = entry?;
            let Some(name) = entry.file_name().to_str().map(str::to_owned) else {
                continue;
            };
            let temporary_key = name
                .split_once(".tmp-")
                .filter(|(_, suffix)| {
                    !suffix.is_empty()
                        && suffix
                            .bytes()
                            .all(|byte| byte.is_ascii_digit() || byte == b'-')
                })
                .map(|(key, _)| key);
            let Some(key) = name.strip_suffix(".json").or(temporary_key) else {
                continue;
            };
            if generated_ref(key)
                && (temporary_key.is_some()
                    || !state.accounts.values().any(|account| {
                        account.credential_ref == key
                            || (account.credential_ref.is_empty() && account.id == key)
                    }))
            {
                if !entry.file_type()?.is_file() {
                    return Err(DomainError::UnsafePath);
                }
                fs::remove_file(entry.path())?;
            }
        }
        sync_directory(&self.root)
    }
}

impl SnapshotVault for FileSnapshotVault {
    fn save(&self, account_id: &str, snapshot: &AuthSnapshot) -> DomainResult<()> {
        let lock = StateLock::exclusive(self.state_dir()?)?;
        self.save_locked(&lock, account_id, snapshot)
    }

    fn load(&self, account_id: &str) -> DomainResult<AuthSnapshot> {
        let lock = StateLock::shared(self.state_dir()?)?;
        self.load_locked(&lock, account_id)
    }

    fn delete(&self, account_id: &str) -> DomainResult<()> {
        let _state_lock = StateLock::exclusive(self.state_dir()?)?;
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::UnsafePath)?;
        let path = self.path_for(account_id);
        reject_unsafe_file(&path)?;
        match fs::remove_file(path) {
            Ok(()) => Ok(()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                Err(DomainError::SnapshotNotFound)
            }
            Err(error) => Err(error.into()),
        }
    }

    fn list(&self) -> DomainResult<Vec<String>> {
        let _state_lock = StateLock::shared(self.state_dir()?)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::UnsafePath)?;
        let entries = match fs::read_dir(&self.root) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut account_ids = Vec::new();
        for entry in entries {
            let entry = entry?;
            let file_type = entry.file_type()?;
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if file_type.is_symlink() || !file_type.is_file() || !name.ends_with(".json") {
                continue;
            }
            let account_id = name.trim_end_matches(".json");
            if validate_account_id(account_id).is_ok() {
                account_ids.push(account_id.to_string());
            }
        }
        account_ids.sort();
        Ok(account_ids)
    }

    fn contains(&self, account_id: &str) -> DomainResult<bool> {
        let _state_lock = StateLock::shared(self.state_dir()?)?;
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::UnsafePath)?;
        let path = self.path_for(account_id);
        reject_unsafe_file(&path)?;
        match fs::metadata(path) {
            Ok(metadata) => Ok(metadata.is_file()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(false),
            Err(error) => Err(error.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    const AUTH_JSON: &[u8] = br#"{"account_id":"account-a","tokens":{"access_token":"bearer-secret","refresh_token":"refresh-secret"},"provider_specific":{"keep":true}}"#;

    #[test]
    fn fresh_refs_are_identity_bound_and_uncommitted_entries_are_collectable() {
        use crate::{AccountRecord, FileAccountRegistry};
        let directory = tempdir().expect("tempdir");
        let registry = FileAccountRegistry::new(directory.path().join("accounts.json"));
        let vault = FileSnapshotVault::new(directory.path().join("vault"));
        let lock = registry.lock_exclusive().expect("lock");
        let snapshot = AuthSnapshot::for_account("account-a", AUTH_JSON).expect("snapshot");
        let orphan = vault.save_fresh_locked(&lock, &snapshot).expect("stage");
        let mut state = registry.state_locked(&lock).expect("state");
        assert!(state.accounts.is_empty());
        vault
            .collect_orphans_locked(&lock, &state)
            .expect("recover precommit orphan");
        assert!(!vault.path_for(&orphan).exists());
        let committed = vault
            .save_fresh_locked(&lock, &snapshot)
            .expect("stage committed");
        let mut account = AccountRecord::new("account-a", "A").expect("account");
        account.credential_ref = committed.clone();
        state.accounts.insert(account.id.clone(), account);
        registry.replace_locked(&lock, state).expect("commit");
        let state = registry.state_locked(&lock).expect("reload");
        vault
            .collect_orphans_locked(&lock, &state)
            .expect("recover postcommit");
        assert_eq!(
            vault.load_locked(&lock, &committed).expect("load").bytes(),
            AUTH_JSON
        );
        let wrong = AuthSnapshot::for_account("b", br#"{"account_id":"b"}"#).expect("snapshot");
        assert!(vault.save_locked(&lock, &committed, &wrong).is_err());
    }

    #[test]
    fn snapshot_debug_and_metadata_do_not_expose_tokens() {
        let snapshot = AuthSnapshot::for_account("account-a", AUTH_JSON).expect("snapshot");
        let debug = format!("{snapshot:?}");
        assert!(!debug.contains("bearer-secret"));
        assert_eq!(snapshot.metadata().byte_len, AUTH_JSON.len());
        assert_eq!(snapshot.embedded_account_id(), Some("account-a"));
    }

    #[test]
    fn vault_round_trip_is_opaque_and_rejects_mismatch() {
        let directory = tempdir().expect("tempdir");
        let vault = FileSnapshotVault::new(directory.path().join("vault"));
        let snapshot = AuthSnapshot::for_account("account-a", AUTH_JSON).expect("snapshot");
        vault.save("account-a", &snapshot).expect("save");
        assert_eq!(vault.list().expect("list"), vec!["account-a"]);
        let loaded = vault.load("account-a").expect("load");
        assert_eq!(loaded.bytes(), AUTH_JSON);
        assert!(matches!(
            vault.save("account-b", &snapshot,),
            Err(DomainError::SnapshotAccountMismatch)
        ));
    }

    #[test]
    fn legacy_document_without_identity_can_be_read_but_not_misaddressed() {
        let snapshot =
            AuthSnapshot::from_unassigned_bytes(br#"{"tokens":{"access_token":"secret"}}"#)
                .expect("legacy snapshot");
        assert!(snapshot.embedded_account_id().is_none());
        let directory = tempdir().expect("tempdir");
        let vault = FileSnapshotVault::new(directory.path().join("vault"));
        vault.save("account-a", &snapshot).expect("save");
        assert_eq!(
            vault.load("account-a").expect("load").bytes(),
            snapshot.bytes()
        );
    }
}
