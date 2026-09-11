//! Read-only legacy auth-file inspection and explicit import scaffolding.
//!
//! Legacy support is intentionally one-way and opt-in: it reads an existing
//! Codex auth document, validates its opaque JSON envelope, and stores a copy
//! in the protected Marathon vault. It never replaces the active Codex auth
//! file and never invokes a second login implementation.

use crate::accounts::{AccountRecord, AccountStore, validate_account_id, validate_alias};
use crate::errors::{DomainError, DomainResult};
use crate::persistence::reject_unsafe_file;
use crate::vault::{AuthSnapshot, SnapshotVault};
use std::fs;
use std::path::Path;

/// Result of an explicit legacy snapshot import.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LegacyImportResult {
    /// Stable account key used for the imported snapshot.
    pub account_id: String,
    /// Display alias written to the non-secret registry.
    pub alias: String,
    /// Whether the account became the selected registry account.
    pub activated: bool,
    /// Whether an existing vault snapshot was replaced.
    pub replaced: bool,
}

/// Read and validate a legacy Codex auth document as opaque bytes.
pub fn read_legacy_auth(path: impl AsRef<Path>) -> DomainResult<AuthSnapshot> {
    let path = path.as_ref();
    reject_unsafe_file(path)?;
    let bytes = fs::read(path)?;
    AuthSnapshot::from_unassigned_bytes(bytes)
}

/// Import a legacy auth file into the protected vault and non-secret registry.
///
/// `account_id` may be omitted only when the auth document contains an
/// embedded account identity. The operation never writes the source path.
pub fn import_legacy_auth(
    path: impl AsRef<Path>,
    account_id: Option<&str>,
    alias: Option<&str>,
    activate: bool,
    overwrite: bool,
    registry: &dyn AccountStore,
    vault: &dyn SnapshotVault,
) -> DomainResult<LegacyImportResult> {
    let snapshot = read_legacy_auth(path)?;
    let account_id = account_id
        .or(snapshot.embedded_account_id())
        .ok_or(DomainError::InvalidAccountId)?;
    validate_account_id(account_id)?;
    if let Some(alias) = alias {
        validate_alias(alias)?;
    }

    let had_snapshot = vault.contains(account_id)?;
    let previous_record = match registry.get(account_id) {
        Ok(record) => Some(record),
        Err(DomainError::AccountNotFound) => None,
        Err(error) => return Err(error),
    };
    let had_record = previous_record.is_some();
    if !overwrite && (had_snapshot || had_record) {
        return Err(DomainError::AccountExists);
    }

    let previous_snapshot = if had_snapshot {
        Some(vault.load(account_id)?)
    } else {
        None
    };
    vault.save(account_id, &snapshot)?;

    let mut record = if let Some(previous_record) = &previous_record {
        previous_record.clone()
    } else {
        AccountRecord::new(account_id, alias.unwrap_or(account_id))?
    };
    if let Some(alias) = alias {
        record.alias = alias.to_string();
    }
    record.credential_ref = account_id.to_string();
    let registry_result = if had_record {
        registry.upsert(record)
    } else {
        registry.register(record)
    };
    if let Err(error) = registry_result {
        rollback_snapshot(account_id, previous_snapshot.as_ref(), vault);
        return Err(error);
    }

    if activate {
        if let Err(error) = registry.set_active(account_id) {
            if let Some(previous_record) = &previous_record {
                let _ = registry.upsert(previous_record.clone());
            } else {
                let _ = registry.remove(account_id, true);
            }
            rollback_snapshot(account_id, previous_snapshot.as_ref(), vault);
            return Err(error);
        }
    }

    Ok(LegacyImportResult {
        account_id: account_id.to_string(),
        alias: alias
            .map(str::to_string)
            .or_else(|| previous_record.map(|record| record.alias))
            .unwrap_or_else(|| account_id.to_string()),
        activated: activate,
        replaced: had_snapshot,
    })
}

fn rollback_snapshot(
    account_id: &str,
    previous_snapshot: Option<&AuthSnapshot>,
    vault: &dyn SnapshotVault,
) {
    if let Some(previous_snapshot) = previous_snapshot {
        let _ = vault.save(account_id, previous_snapshot);
    } else {
        let _ = vault.delete(account_id);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::accounts::FileAccountRegistry;
    use crate::vault::FileSnapshotVault;
    use tempfile::tempdir;

    #[test]
    fn import_reads_legacy_file_without_replacing_source() {
        let directory = tempdir().expect("tempdir");
        let source = directory.path().join("auth.json");
        std::fs::write(
            &source,
            br#"{"tokens":{"account_id":"account-a","access_token":"secret"}}"#,
        )
        .expect("write source");
        let registry = FileAccountRegistry::new(directory.path().join("accounts.json"));
        let vault = FileSnapshotVault::new(directory.path().join("vault"));
        let result =
            import_legacy_auth(&source, None, Some("work"), true, false, &registry, &vault)
                .expect("import");
        assert_eq!(result.account_id, "account-a");
        assert_eq!(
            registry.active().expect("active").map(|a| a.id),
            Some("account-a".to_string())
        );
        assert!(source.exists());
        assert!(vault.contains("account-a").expect("contains"));
    }
}
