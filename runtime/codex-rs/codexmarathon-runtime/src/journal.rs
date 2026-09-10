//! Append-only, metadata-only transition journal.
//!
//! Journal records intentionally have no field capable of carrying an auth
//! snapshot, token, authorization header, or recovery prompt. Validation also
//! rejects common secret markers in the free-form reason field.

use crate::errors::{DomainError, DomainResult};
use crate::persistence::{ensure_private_dir, reject_unsafe_file, set_private_permissions_file};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::fs::{File, OpenOptions};
use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Stable journal event type names used by transition and recovery code.
pub mod event_types {
    pub const TRANSITION_CREATED: &str = "TransitionCreated";
    pub const TRANSITION_PREPARED: &str = "TransitionPrepared";
    pub const AUTH_DEPLOYED: &str = "AuthDeployed";
    pub const COMMIT_SENT: &str = "CommitSent";
    pub const IDENTITY_CONFIRMED: &str = "IdentityConfirmed";
    pub const TRANSITION_COMMITTED: &str = "TransitionCommitted";
    pub const TRANSITION_UNCERTAIN: &str = "TransitionUncertain";
    pub const TRANSITION_RECONCILED: &str = "TransitionReconciled";
    pub const RUNTIME_CONNECTED: &str = "RuntimeConnected";
    pub const RUNTIME_DISCONNECTED: &str = "RuntimeDisconnected";
    pub const RECOVERY_PARKED: &str = "RecoveryParked";
    pub const RECOVERY_WAITING: &str = "RecoveryWaiting";
    pub const RECOVERY_BOUND: &str = "RecoveryBound";
    pub const RECOVERY_RELEASE_REQUESTED: &str = "RecoveryReleaseRequested";
    pub const RECOVERY_RELEASED: &str = "RecoveryReleased";
    pub const RECOVERY_STARTED: &str = "RecoveryStarted";
    pub const RECOVERY_COMPLETED: &str = "RecoveryCompleted";
    pub const RECOVERY_UNCERTAIN: &str = "RecoveryUncertain";
}

/// A scalar-only journal record.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct JournalEvent {
    /// Event timestamp in UTC.
    pub at: DateTime<Utc>,
    /// Stable event type name.
    #[serde(rename = "type")]
    pub event_type: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub transition_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub recovery_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub thread_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub turn_id: String,
    /// Runtime auth generation observed for this event.
    pub auth_generation: u64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub expected_generation: u64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub runtime_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub from_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub target_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub outcome: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

impl JournalEvent {
    /// Construct an event with the current timestamp and empty metadata.
    pub fn new(event_type: impl Into<String>) -> Self {
        Self {
            at: Utc::now(),
            event_type: event_type.into(),
            transition_id: String::new(),
            recovery_id: String::new(),
            thread_id: String::new(),
            turn_id: String::new(),
            auth_generation: 0,
            expected_generation: 0,
            runtime_id: String::new(),
            account_id: String::new(),
            from_account_id: String::new(),
            target_account_id: String::new(),
            outcome: String::new(),
            reason: String::new(),
        }
    }

    /// Validate the metadata-only event contract.
    pub fn validate(&self) -> DomainResult<()> {
        if self.event_type.trim().is_empty() || self.event_type.len() > 256 {
            return Err(DomainError::InvalidJournalEvent);
        }
        if self.at.timestamp() == 0 && self.at.timestamp_subsec_nanos() == 0 {
            return Err(DomainError::InvalidJournalEvent);
        }
        if is_transition_event(&self.event_type) {
            if self.transition_id.is_empty() || self.runtime_id.is_empty() {
                return Err(DomainError::InvalidJournalEvent);
            }
            if matches!(
                self.event_type.as_str(),
                event_types::TRANSITION_COMMITTED | event_types::TRANSITION_RECONCILED
            ) && self.auth_generation == 0
            {
                return Err(DomainError::InvalidJournalEvent);
            }
        }
        if is_recovery_event(&self.event_type)
            && (self.recovery_id.is_empty() || self.runtime_id.is_empty())
        {
            return Err(DomainError::InvalidJournalEvent);
        }
        for value in [
            &self.transition_id,
            &self.recovery_id,
            &self.thread_id,
            &self.turn_id,
            &self.runtime_id,
            &self.account_id,
            &self.from_account_id,
            &self.target_account_id,
            &self.outcome,
            &self.reason,
        ] {
            if value.len() > 4096
                || value
                    .chars()
                    .any(|character| character == '\0' || character.is_control())
            {
                return Err(DomainError::InvalidJournalEvent);
            }
        }
        if looks_sensitive(&self.reason) {
            return Err(DomainError::SensitiveJournalEvent);
        }
        Ok(())
    }
}

fn is_transition_event(event_type: &str) -> bool {
    matches!(
        event_type,
        event_types::TRANSITION_CREATED
            | event_types::TRANSITION_PREPARED
            | event_types::AUTH_DEPLOYED
            | event_types::COMMIT_SENT
            | event_types::IDENTITY_CONFIRMED
            | event_types::TRANSITION_COMMITTED
            | event_types::TRANSITION_UNCERTAIN
            | event_types::TRANSITION_RECONCILED
    )
}

fn is_recovery_event(event_type: &str) -> bool {
    matches!(
        event_type,
        event_types::RECOVERY_PARKED
            | event_types::RECOVERY_WAITING
            | event_types::RECOVERY_BOUND
            | event_types::RECOVERY_RELEASE_REQUESTED
            | event_types::RECOVERY_RELEASED
            | event_types::RECOVERY_STARTED
            | event_types::RECOVERY_COMPLETED
            | event_types::RECOVERY_UNCERTAIN
    )
}

fn looks_sensitive(value: &str) -> bool {
    let value = value.to_ascii_lowercase();
    [
        "access_token",
        "id_token",
        "refresh_token",
        "authorization:",
        "bearer ",
        "api_key",
        "openai_api_key",
        "password=",
        "secret=",
        "auth.json",
        "\"tokens\"",
    ]
    .iter()
    .any(|marker| value.contains(marker))
}

/// Storage-neutral journal interface.
pub trait Journal: Send + Sync {
    /// Validate and durably append an event.
    fn append(&self, event: JournalEvent) -> DomainResult<()>;
    /// Read and validate all events in order.
    fn read_all(&self) -> DomainResult<Vec<JournalEvent>>;
}

/// Process-safe JSONL journal with owner-only permissions.
pub struct FileJournal {
    path: PathBuf,
    gate: Mutex<()>,
    clock: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

impl FileJournal {
    /// Construct a journal without creating its file.
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self::with_clock(path, Arc::new(Utc::now))
    }

    /// Construct a journal with an injectable clock.
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

    /// Return the journal path without reading records.
    pub fn path(&self) -> &Path {
        &self.path
    }
}

impl Journal for FileJournal {
    fn append(&self, mut event: JournalEvent) -> DomainResult<()> {
        let _guard = self.gate.lock().map_err(|_| DomainError::CorruptJournal)?;
        if event.at.timestamp() == 0 && event.at.timestamp_subsec_nanos() == 0 {
            event.at = (self.clock)();
        }
        event.at = event.at.with_timezone(&Utc);
        event.validate()?;
        reject_unsafe_file(&self.path)?;
        let parent = self.path.parent().ok_or(DomainError::UnsafePath)?;
        ensure_private_dir(parent)?;
        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&self.path)?;
        set_private_permissions_file(&file)?;
        serde_json::to_writer(&mut file, &event)?;
        file.write_all(b"\n")?;
        file.sync_all()?;
        Ok(())
    }

    fn read_all(&self) -> DomainResult<Vec<JournalEvent>> {
        let _guard = self.gate.lock().map_err(|_| DomainError::CorruptJournal)?;
        reject_unsafe_file(&self.path)?;
        let file = match File::open(&self.path) {
            Ok(file) => file,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut records = Vec::new();
        let reader = BufReader::new(file);
        for line in reader.lines() {
            let line = line?;
            if line.trim().is_empty() {
                continue;
            }
            if line.len() > 1024 * 1024 {
                return Err(DomainError::CorruptJournal);
            }
            let event: JournalEvent =
                serde_json::from_str(&line).map_err(|_| DomainError::CorruptJournal)?;
            event.validate().map_err(|_| DomainError::CorruptJournal)?;
            records.push(event);
        }
        Ok(records)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::tempdir;

    #[test]
    fn journal_round_trip_and_secret_guard() {
        let directory = tempdir().expect("tempdir");
        let journal = FileJournal::new(directory.path().join("journal.jsonl"));
        let mut event = JournalEvent::new(event_types::TRANSITION_CREATED);
        event.transition_id = "transition-1".to_string();
        event.runtime_id = "runtime-1".to_string();
        event.reason = "threshold reached".to_string();
        journal.append(event.clone()).expect("append");
        assert_eq!(journal.read_all().expect("read"), vec![event]);

        let mut sensitive = JournalEvent::new("diagnostic");
        sensitive.reason = "Authorization: Bearer secret".to_string();
        assert!(matches!(
            journal.append(sensitive),
            Err(DomainError::SensitiveJournalEvent)
        ));
    }

    #[test]
    fn committed_transition_requires_generation() {
        let mut event = JournalEvent::new(event_types::TRANSITION_COMMITTED);
        event.transition_id = "transition-1".to_string();
        event.runtime_id = "runtime-1".to_string();
        assert!(matches!(
            event.validate(),
            Err(DomainError::InvalidJournalEvent)
        ));
        event.auth_generation = 2;
        assert!(event.validate().is_ok());
    }
}
