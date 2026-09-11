//! Native account records and the non-secret account registry.
//!
//! Account records contain identity and operator metadata only. Authentication
//! material lives behind [`crate::vault::SnapshotVault`] and is referenced by
//! `credential_ref`; keeping the two stores separate prevents a registry or
//! journal read from becoming an accidental token read.

use crate::errors::{DomainError, DomainResult};
use crate::persistence::{atomic_write, ensure_private_dir, reject_unsafe_file};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Current on-disk account registry schema version.
pub const REGISTRY_VERSION: u32 = 1;

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

    /// Read the full state for diagnostics and migration code.
    pub fn state(&self) -> DomainResult<RegistryState> {
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()
    }

    /// Return whether native Marathon transitions are enabled.
    pub fn enabled(&self) -> DomainResult<bool> {
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        Ok(self.load_unlocked()?.enabled)
    }

    /// Persist the native Marathon enabled flag atomically.
    pub fn set_enabled(&self, enabled: bool) -> DomainResult<()> {
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
        reject_unsafe_file(&self.path)?;
        let raw = match fs::read(&self.path) {
            Ok(raw) => raw,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(RegistryState {
                    version: REGISTRY_VERSION,
                    ..RegistryState::default()
                });
            }
            Err(error) => return Err(error.into()),
        };
        if raw.is_empty() {
            return Ok(RegistryState {
                version: REGISTRY_VERSION,
                ..RegistryState::default()
            });
        }
        let mut state: RegistryState =
            serde_json::from_slice(&raw).map_err(|_| DomainError::InvalidRegistry)?;
        if state.version == 0 {
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
        if !state.active_account_id.is_empty()
            && !state.accounts.contains_key(&state.active_account_id)
        {
            return Err(DomainError::InvalidRegistry);
        }
        let mut raw = serde_json::to_vec_pretty(&state)?;
        raw.push(b'\n');
        if let Some(parent) = self.path.parent() {
            ensure_private_dir(parent)?;
        }
        atomic_write(&self.path, &raw)
    }
}

impl AccountStore for FileAccountRegistry {
    fn register(&self, mut account: AccountRecord) -> DomainResult<()> {
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
        validate_account_id(&account.id)?;
        validate_alias(&account.alias)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        if let Some(existing) = state.accounts.get(&account.id) {
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
        validate_account_id(account_id)?;
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        self.load_unlocked()?
            .accounts
            .remove(account_id)
            .ok_or(DomainError::AccountNotFound)
    }

    fn list(&self) -> DomainResult<Vec<AccountRecord>> {
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        Ok(self.load_unlocked()?.accounts.into_values().collect())
    }

    fn set_active(&self, account_id: &str) -> DomainResult<()> {
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
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let mut state = self.load_unlocked()?;
        state.active_account_id.clear();
        self.save_unlocked(state)
    }

    fn active(&self) -> DomainResult<Option<AccountRecord>> {
        let _guard = self.gate.lock().map_err(|_| DomainError::InvalidRegistry)?;
        let state = self.load_unlocked()?;
        Ok(state.accounts.get(&state.active_account_id).cloned())
    }

    fn remove(&self, account_id: &str, force: bool) -> DomainResult<()> {
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
