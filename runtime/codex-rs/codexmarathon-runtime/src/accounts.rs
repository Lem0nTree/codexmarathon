//! Native account records and the non-secret account registry.
//!
//! Account records contain identity and operator metadata only. Authentication
//! material lives behind [`crate::vault::SnapshotVault`] and is referenced by
//! `credential_ref`; keeping the two stores separate prevents a registry or
//! journal read from becoming an accidental token read.

use crate::errors::{DomainError, DomainResult};
use crate::persistence::{atomic_write, ensure_private_dir, open_regular};
use crate::state_lock::StateLock;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Current on-disk account registry schema version.
// v2 preserves the JSON shape but permits fresh, identity-bound vault refs.
// Old v1 binaries reject v2 registries before attempting stale credential writes.
pub const REGISTRY_VERSION: u32 = 2;

/// Secret-free state of the credential snapshot referenced by an account.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum CredentialHealth {
    /// No successful validation or refresh has been recorded.
    #[default]
    Unknown,
    /// The native runtime last accepted the snapshot.
    Healthy,
    /// The snapshot needs a native refresh before it can be used.
    Stale,
    /// The snapshot failed a native validation step.
    Invalid,
}

/// A non-secret account/profile record.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct AccountRecord {
    /// Stable provider or locally assigned identity.
    pub id: String,
    /// Optional operator-facing display label.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub alias: String,
    /// Opaque vault key. Defaults to `id` when persisted.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub credential_ref: String,
    /// Descriptive metadata only. Sensitive keys are rejected.
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub metadata: BTreeMap<String, String>,
    /// Last time account telemetry was observed, if any.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_telemetry_at: Option<DateTime<Utc>>,
    /// Non-secret source label for telemetry diagnostics.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub telemetry_source: String,
    /// Controller knowledge about the referenced snapshot.
    #[serde(default)]
    pub credential_health: CredentialHealth,
    /// Registry creation time.
    pub created_at: DateTime<Utc>,
    /// Last registry mutation time.
    pub updated_at: DateTime<Utc>,
}

impl AccountRecord {
    /// Construct a record with the current UTC time and default vault key.
    pub fn new(id: impl Into<String>, alias: impl Into<String>) -> DomainResult<Self> {
        let id = id.into();
        let alias = alias.into();
        validate_account_id(&id)?;
        validate_alias(&alias)?;
        let now = Utc::now();
        Ok(Self {
            credential_ref: id.clone(),
            id,
            alias,
            metadata: BTreeMap::new(),
            last_telemetry_at: None,
            telemetry_source: String::new(),
            credential_health: CredentialHealth::Unknown,
            created_at: now,
            updated_at: now,
        })
    }

    /// Validate the identity and all non-secret metadata fields.
    pub fn validate(&self) -> DomainResult<()> {
        validate_account_id(&self.id)?;
        validate_alias(&self.alias)?;
        if !self.credential_ref.is_empty() {
            validate_account_id(&self.credential_ref).map_err(|_| {
                DomainError::InvalidAccountRecord("credential_ref is not a valid vault key")
            })?;
        }
        for (key, value) in &self.metadata {
            validate_metadata_entry(key, value)?;
        }
        validate_text_field(&self.telemetry_source, "telemetry_source")?;
        if self.created_at > self.updated_at {
            return Err(DomainError::InvalidAccountRecord(
                "created_at is after updated_at",
            ));
        }
        Ok(())
    }

    /// Return the display label, falling back to the stable identity.
    pub fn display_name(&self) -> &str {
        if self.alias.is_empty() {
            &self.id
        } else {
            &self.alias
        }
    }
}

/// Validate an account ID before using it as a filename component.
pub fn validate_account_id(value: &str) -> DomainResult<()> {
    if value.is_empty() || value == "." || value == ".." || value.len() > 240 {
        return Err(DomainError::InvalidAccountId);
    }
    if value
        .chars()
        .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidAccountId);
    }
    if value
        .chars()
        .any(|character| "/\\<>:\"|?*".contains(character))
        || value.ends_with('.')
        || value.ends_with(' ')
    {
        return Err(DomainError::InvalidAccountId);
    }
    let base = value.split('.').next().unwrap_or("").to_ascii_uppercase();
    if matches!(
        base.as_str(),
        "CON"
            | "PRN"
            | "AUX"
            | "NUL"
            | "COM1"
            | "COM2"
            | "COM3"
            | "COM4"
            | "COM5"
            | "COM6"
            | "COM7"
            | "COM8"
            | "COM9"
            | "LPT1"
            | "LPT2"
            | "LPT3"
            | "LPT4"
            | "LPT5"
            | "LPT6"
            | "LPT7"
            | "LPT8"
            | "LPT9"
    ) {
        return Err(DomainError::InvalidAccountId);
    }
    Ok(())
}

/// Validate an optional, single-line operator-facing alias.
pub fn validate_alias(value: &str) -> DomainResult<()> {
    if value.trim() != value || value.len() > 256 {
        return Err(DomainError::InvalidAlias);
    }
    if value
        .chars()
        .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidAlias);
    }
    Ok(())
}

fn validate_text_field(value: &str, field: &'static str) -> DomainResult<()> {
    if value.len() > 4096
        || value
            .chars()
            .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidAccountRecord(field));
    }
    Ok(())
}

fn validate_metadata_entry(key: &str, value: &str) -> DomainResult<()> {
    if key.trim().is_empty()
        || key
            .chars()
            .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidAccountRecord("invalid metadata key"));
    }
    if value
        .chars()
        .any(|character| character == '\0' || character.is_control())
    {
        return Err(DomainError::InvalidAccountRecord("invalid metadata value"));
    }
    let normalized = key.replace('-', "_").replace(' ', "_").to_ascii_lowercase();
    if [
        "access_token",
        "id_token",
        "refresh_token",
        "authorization",
        "api_key",
        "password",
        "secret",
        "auth_json",
        "credential",
        "private_key",
    ]
    .iter()
    .any(|marker| normalized.contains(marker))
    {
        return Err(DomainError::InvalidAccountRecord(
            "metadata key may contain credential material",
        ));
    }
    Ok(())
}

/// Serializable registry state.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct RegistryState {
    /// Persisted schema version.
    #[serde(default = "default_registry_version")]
    pub version: u32,
    /// Selected account, if one has been activated.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub active_account_id: String,
    /// Whether the native Marathon service may perform account transitions.
    /// New registries enable account management; an explicitly persisted value
    /// continues to win when an existing registry is loaded.
    #[serde(default = "default_registry_enabled")]
    pub enabled: bool,
    /// Records keyed by stable account ID.
    #[serde(default)]
    pub accounts: BTreeMap<String, AccountRecord>,
}

fn default_registry_version() -> u32 {
    REGISTRY_VERSION
}

fn default_registry_enabled() -> bool {
    true
}

impl Default for RegistryState {
    fn default() -> Self {
        Self {
            version: REGISTRY_VERSION,
            active_account_id: String::new(),
            enabled: default_registry_enabled(),
            accounts: BTreeMap::new(),
        }
    }
}

/// Storage-neutral account registry surface.
pub trait AccountStore: Send + Sync {
    /// Insert a new account and reject duplicate IDs.
    fn register(&self, account: AccountRecord) -> DomainResult<()>;
    /// Insert or update an account while preserving omitted vault metadata.
    fn upsert(&self, account: AccountRecord) -> DomainResult<()>;
    /// Read one account record.
    fn get(&self, account_id: &str) -> DomainResult<AccountRecord>;
    /// List records in stable ID order.
    fn list(&self) -> DomainResult<Vec<AccountRecord>>;
    /// Mark an existing account as active.
    fn set_active(&self, account_id: &str) -> DomainResult<()>;
    /// Clear the active marker.
    fn clear_active(&self) -> DomainResult<()>;
    /// Return the active record, if configured.
    fn active(&self) -> DomainResult<Option<AccountRecord>>;
    /// Remove an account. Active removal requires `force`.
    fn remove(&self, account_id: &str, force: bool) -> DomainResult<()>;
}

/// JSON-backed account registry with owner-only persistence and atomic writes.
pub struct FileAccountRegistry {
    path: PathBuf,
    gate: Mutex<()>,
    clock: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

impl FileAccountRegistry {
    /// Construct a registry without creating its file.
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self::with_clock(path, Arc::new(Utc::now))
    }

    /// Construct a registry with an injectable clock for deterministic tests.
    pub fn with_clock(
        path: impl Into<PathBuf>,
        clock: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
    ) -> Self {
        Self {
            path: path.into(),
            gate: Mutex::new(()),
            clock,
        }
    }

    /// Return the registry path without reading its contents.
    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn lock_exclusive(&self) -> DomainResult<StateLock> {
        StateLock::exclusive(self.path.parent().ok_or(DomainError::UnsafePath)?)
    }

    /// Read one atomically published registry for a nonmutating preview.
    /// Does not create directories or a lockfile. Its result is advisory;
    /// writers must reload under StateLock before making any decision.
    pub fn preview_state(&self) -> DomainResult<RegistryState> {
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()
    }

    /// Read within a caller-owned transaction, avoiding nested file locks.
    pub fn state_locked(&self, lock: &StateLock) -> DomainResult<RegistryState> {
        lock.check(self.path.parent().ok_or(DomainError::UnsafePath)?, false)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()
    }

    /// One atomic commit point for a batch. Callers must fsync every new
    /// immutable vault entry before publishing the registry that references it.
    /// An error after rename/fsync may be ambiguous: reload, never roll back or
    /// delete new entries based solely on this method's returned error.
    pub fn replace_locked(&self, lock: &StateLock, state: RegistryState) -> DomainResult<()> {
        lock.check(self.path.parent().ok_or(DomainError::UnsafePath)?, true)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.save_unlocked(state)
    }

    pub fn set_active_locked(&self, lock: &StateLock, account_id: &str) -> DomainResult<()> {
        let mut state = self.state_locked(lock)?;
        if !state.accounts.contains_key(account_id) {
            return Err(DomainError::AccountNotFound);
        }
        state.active_account_id = account_id.to_owned();
        self.replace_locked(lock, state)
    }

    /// Read the full state for diagnostics and migration code.
    pub fn state(&self) -> DomainResult<RegistryState> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()
    }

    /// Return whether native Marathon transitions are enabled.
    pub fn enabled(&self) -> DomainResult<bool> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        Ok(self.load_unlocked()?.enabled)
    }

    /// Persist the native Marathon enabled flag atomically.
    pub fn set_enabled(&self, enabled: bool) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        state.enabled = enabled;
        self.save_unlocked(state)
    }

    fn now(&self) -> DateTime<Utc> {
        (self.clock)()
    }

    fn load_unlocked(&self) -> DomainResult<RegistryState> {
        if self.path.as_os_str().is_empty() {
            return Err(DomainError::InvalidRegistry);
        }
        let file = match open_regular(&self.path, false, false) {
            Ok(file) => file,
            Err(DomainError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(RegistryState {
                    version: REGISTRY_VERSION,
                    ..RegistryState::default()
                });
            }
            Err(error) => return Err(error),
        };
        let mut raw = Vec::new();
        file.take(16 * 1024 * 1024 + 1).read_to_end(&mut raw)?;
        if raw.len() > 16 * 1024 * 1024 {
            return Err(DomainError::InvalidRegistry);
        }
        if raw.is_empty() {
            return Ok(RegistryState {
                version: REGISTRY_VERSION,
                ..RegistryState::default()
            });
        }
        let mut state: RegistryState =
            serde_json::from_slice(&raw).map_err(|_| DomainError::InvalidRegistry)?;
        // Read legacy registries without rewriting during preview/export.
        // The next authorized mutation publishes v2 atomically.
        if state.version == 0 || state.version == 1 {
            state.version = REGISTRY_VERSION;
        }
        if state.version != REGISTRY_VERSION {
            return Err(DomainError::InvalidRegistry);
        }
        for (key, account) in &state.accounts {
            if key != &account.id {
                return Err(DomainError::InvalidRegistry);
            }
            account
                .validate()
                .map_err(|_| DomainError::InvalidRegistry)?;
        }
        if !state.active_account_id.is_empty() {
            validate_account_id(&state.active_account_id)?;
            if !state.accounts.contains_key(&state.active_account_id) {
                return Err(DomainError::InvalidRegistry);
            }
        }
        Ok(state)
    }

    fn save_unlocked(&self, mut state: RegistryState) -> DomainResult<()> {
        state.version = REGISTRY_VERSION;
        for account in state.accounts.values() {
            account.validate()?;
        }
        if state
            .accounts
            .iter()
            .any(|(key, account)| key != &account.id)
        {
            return Err(DomainError::InvalidRegistry);
        }
        if !state.active_account_id.is_empty()
            && !state.accounts.contains_key(&state.active_account_id)
        {
            return Err(DomainError::InvalidRegistry);
        }
        let mut raw = serde_json::to_vec_pretty(&state)?;
        raw.push(b'\n');
        if raw.len() > 16 * 1024 * 1024 {
            return Err(DomainError::InvalidRegistry);
        }
        if let Some(parent) = self.path.parent() {
            ensure_private_dir(parent)?;
        }
        atomic_write(&self.path, &raw)
    }
}

impl AccountStore for FileAccountRegistry {
    fn register(&self, mut account: AccountRecord) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        account.validate()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        if state.accounts.contains_key(&account.id) {
            return Err(DomainError::AccountExists);
        }
        let now = self.now();
        account.credential_ref = if account.credential_ref.is_empty() {
            account.id.clone()
        } else {
            account.credential_ref
        };
        if account.created_at.timestamp() == 0 && account.created_at.timestamp_subsec_nanos() == 0 {
            account.created_at = now;
        }
        account.updated_at = now;
        state.accounts.insert(account.id.clone(), account);
        self.save_unlocked(state)
    }

    fn upsert(&self, mut account: AccountRecord) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        validate_account_id(&account.id)?;
        validate_alias(&account.alias)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        if let Some(existing) = state.accounts.get(&account.id) {
            // A record read before a batch import must not resurrect its old
            // vault reference. Intentional credential replacement uses a full
            // registry transaction with a caller-owned StateLock.
            if !account.credential_ref.is_empty()
                && account.credential_ref != existing.credential_ref
            {
                return Err(DomainError::InvalidAccountRecord(
                    "credential reference changed; retry",
                ));
            }
            if account.credential_ref.is_empty() {
                account.credential_ref = existing.credential_ref.clone();
            }
            if account.created_at.timestamp() == 0
                && account.created_at.timestamp_subsec_nanos() == 0
            {
                account.created_at = existing.created_at;
            }
            if account.last_telemetry_at.is_none() {
                account.last_telemetry_at = existing.last_telemetry_at;
            }
            if account.telemetry_source.is_empty() {
                account.telemetry_source = existing.telemetry_source.clone();
            }
            if account.credential_health == CredentialHealth::Unknown {
                account.credential_health = existing.credential_health;
            }
        }
        if account.credential_ref.is_empty() {
            account.credential_ref = account.id.clone();
        }
        if account.created_at.timestamp() == 0 && account.created_at.timestamp_subsec_nanos() == 0 {
            account.created_at = self.now();
        }
        account.updated_at = self.now();
        account.validate()?;
        state.accounts.insert(account.id.clone(), account);
        self.save_unlocked(state)
    }

    fn get(&self, account_id: &str) -> DomainResult<AccountRecord> {
        let _state_lock = self.lock_exclusive()?;
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()?
            .accounts
            .remove(account_id)
            .ok_or(DomainError::AccountNotFound)
    }

    fn list(&self) -> DomainResult<Vec<AccountRecord>> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        Ok(self.load_unlocked()?.accounts.into_values().collect())
    }

    fn set_active(&self, account_id: &str) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        if !state.accounts.contains_key(account_id) {
            return Err(DomainError::AccountNotFound);
        }
        state.active_account_id = account_id.to_string();
        self.save_unlocked(state)
    }

    fn clear_active(&self) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        state.active_account_id.clear();
        self.save_unlocked(state)
    }

    fn active(&self) -> DomainResult<Option<AccountRecord>> {
        let _state_lock = self.lock_exclusive()?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let state = self.load_unlocked()?;
        Ok(state.accounts.get(&state.active_account_id).cloned())
    }

    fn remove(&self, account_id: &str, force: bool) -> DomainResult<()> {
        let _state_lock = self.lock_exclusive()?;
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        if !state.accounts.contains_key(account_id) {
            return Err(DomainError::AccountNotFound);
        }
        if state.active_account_id == account_id && !force {
            return Err(DomainError::AccountIsActive);
        }
        state.accounts.remove(account_id);
        if state.active_account_id == account_id {
            state.active_account_id.clear();
        }
        self.save_unlocked(state)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn preview_does_not_create_state_and_stable_lock_excludes_other_instances() {
        let directory = tempdir().expect("tempdir");
        let state_dir = directory.path().join("missing");
        let first = FileAccountRegistry::new(state_dir.join("accounts.json"));
        assert!(first.preview_state().expect("preview").accounts.is_empty());
        assert!(!state_dir.exists());
        let second = FileAccountRegistry::new(first.path());
        let lock = first.lock_exclusive().expect("lock");
        assert!(
            second
                .register(AccountRecord::new("b", "B").expect("record"))
                .is_err()
        );
        drop(lock);
        second
            .register(AccountRecord::new("b", "B").expect("record"))
            .expect("unlocked");
        assert_eq!(first.state().expect("state").accounts.len(), 1);
    }

    #[test]
    fn stale_upsert_cannot_resurrect_a_replaced_credential_reference() {
        let directory = tempdir().expect("tempdir");
        let registry = FileAccountRegistry::new(directory.path().join("accounts.json"));
        let old = AccountRecord::new("a", "A").expect("record");
        registry.register(old.clone()).expect("register");
        let lock = registry.lock_exclusive().expect("lock");
        let mut state = registry.state_locked(&lock).expect("state");
        state.accounts.get_mut("a").expect("account").credential_ref = "new-reference".into();
        registry.replace_locked(&lock, state).expect("commit");
        drop(lock);
        assert!(registry.upsert(old).is_err());
        assert_eq!(
            registry.get("a").expect("account").credential_ref,
            "new-reference"
        );
    }

    #[test]
    fn legacy_v1_reads_without_mutation_and_next_write_commits_v2() {
        let directory = tempdir().expect("tempdir");
        let path = directory.path().join("accounts.json");
        let original = br#"{"version":1,"accounts":{},"enabled":false}"#;
        std::fs::write(&path, original).expect("legacy fixture");
        let registry = FileAccountRegistry::new(&path);
        let preview = registry.preview_state().expect("v1 preview");
        assert_eq!(preview.version, 2);
        assert!(!preview.enabled);
        assert_eq!(std::fs::read(&path).expect("unchanged file"), original);
        registry
            .register(AccountRecord::new("a", "A").expect("record"))
            .expect("mutation");
        let saved: RegistryState =
            serde_json::from_slice(&std::fs::read(&path).expect("saved")).expect("JSON");
        assert_eq!(saved.version, 2);
        assert_ne!(
            saved.version, 1,
            "a v1-only loader must reject this registry"
        );
        let mut future = saved;
        future.version = 3;
        std::fs::write(&path, serde_json::to_vec(&future).expect("future JSON"))
            .expect("future fixture");
        assert!(matches!(
            registry.preview_state(),
            Err(DomainError::InvalidRegistry)
        ));
    }

    #[test]
    fn identifier_and_alias_rules_reject_path_and_control_data() {
        assert!(validate_account_id("account-a").is_ok());
        assert!(validate_account_id("../account").is_err());
        assert!(validate_account_id("CON.txt").is_err());
        assert!(validate_alias("work account").is_ok());
        assert!(validate_alias(" work").is_err());
        assert!(validate_alias("work\naccount").is_err());
    }

    #[test]
    fn registry_keeps_active_selection_and_stable_order() {
        let directory = tempdir().expect("tempdir");
        let registry = FileAccountRegistry::new(directory.path().join("accounts.json"));
        let mut second = AccountRecord::new("b", "B").expect("record");
        second
            .metadata
            .insert("team".to_string(), "dev".to_string());
        registry.register(second).expect("register b");
        registry
            .register(AccountRecord::new("a", "A").expect("record"))
            .expect("register a");
        let ids: Vec<_> = registry
            .list()
            .expect("list")
            .into_iter()
            .map(|account| account.id)
            .collect();
        assert_eq!(ids, vec!["a", "b"]);
        registry.set_active("b").expect("activate");
        assert_eq!(
            registry.active().expect("active").map(|a| a.id),
            Some("b".to_string())
        );
        assert!(matches!(
            registry.remove("b", false),
            Err(DomainError::AccountIsActive)
        ));
        registry.remove("b", true).expect("forced remove");
        assert!(registry.active().expect("active").is_none());
    }

    #[test]
    fn sensitive_metadata_is_rejected() {
        let mut record = AccountRecord::new("account-a", "A").expect("record");
        record
            .metadata
            .insert("access_token".to_string(), "secret".to_string());
        assert!(record.validate().is_err());
    }
}
