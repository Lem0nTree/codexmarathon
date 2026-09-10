//! Errors shared by the native CodexMarathon domain.
//!
//! The error types in this module deliberately avoid carrying credential
//! bytes or provider responses.  Callers can attach an operation context at
//! the application boundary, but the domain itself never formats an opaque
//! authentication snapshot into an error.

use std::io;

/// Result type used by the native Marathon domain modules.
pub type DomainResult<T> = Result<T, DomainError>;

/// Errors returned by account, vault, journal, policy, and transition code.
#[derive(Debug, thiserror::Error)]
pub enum DomainError {
    /// A stable account identifier failed the path-safe identifier contract.
    #[error("invalid account id")]
    InvalidAccountId,

    /// An operator-facing alias failed the single-line label contract.
    #[error("invalid account alias")]
    InvalidAlias,

    /// An account record failed validation.
    #[error("invalid account record: {0}")]
    InvalidAccountRecord(&'static str),

    /// The requested account does not exist in the registry.
    #[error("account not found")]
    AccountNotFound,

    /// Registering an existing account was not permitted.
    #[error("account already exists")]
    AccountExists,

    /// Removing the active account requires an explicit force operation.
    #[error("account is active")]
    AccountIsActive,

    /// The persisted account registry could not be decoded or validated.
    #[error("invalid account registry")]
    InvalidRegistry,

    /// An opaque auth snapshot was not a JSON object.
    #[error("invalid auth snapshot")]
    InvalidSnapshot,

    /// A snapshot was addressed to an account other than its embedded
    /// identity.
    #[error("auth snapshot account mismatch")]
    SnapshotAccountMismatch,

    /// No snapshot exists for the requested account.
    #[error("auth snapshot not found")]
    SnapshotNotFound,

    /// A vault path or journal path was a symbolic link or directory where a
    /// regular file was required.
    #[error("unsafe persistence path")]
    UnsafePath,

    /// The append-only journal contains invalid or truncated data.
    #[error("corrupt journal")]
    CorruptJournal,

    /// A journal event failed the metadata-only contract.
    #[error("invalid journal event")]
    InvalidJournalEvent,

    /// A journal reason or metadata field looked like credential material.
    #[error("journal event contains sensitive material")]
    SensitiveJournalEvent,

    /// A transition state or transition result was internally inconsistent.
    #[error("invalid transition state: {0}")]
    InvalidTransitionState(&'static str),

    /// Runtime/domain paths or policy settings are inconsistent.
    #[error("invalid Marathon configuration: {0}")]
    InvalidConfig(&'static str),

    /// Persisted automatic reset state failed validation.
    #[error("invalid automatic reset state")]
    InvalidAutoResetState,

    /// A completion attempted to update a different reset attempt.
    #[error("automatic reset attempt mismatch")]
    AutoResetAttemptMismatch,

    /// A transition identifier did not refer to the expected state.
    #[error("transition not found")]
    TransitionNotFound,

    /// A policy input did not contain enough complete telemetry to decide.
    #[error("insufficient telemetry")]
    InsufficientTelemetry,

    /// A filesystem operation failed.  The path may be included by the
    /// standard I/O error, but opaque snapshot data is never part of it.
    #[error("persistence I/O failed: {0}")]
    Io(#[from] io::Error),

    /// JSON decoding failed without retaining the source bytes.
    #[error("persistence encoding failed")]
    Json(#[from] serde_json::Error),
}

impl DomainError {
    /// Return a stable, secret-free diagnostic code for UI and logs.
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InvalidAccountId => "invalid_account_id",
            Self::InvalidAlias => "invalid_alias",
            Self::InvalidAccountRecord(_) => "invalid_account_record",
            Self::AccountNotFound => "account_not_found",
            Self::AccountExists => "account_exists",
            Self::AccountIsActive => "account_is_active",
            Self::InvalidRegistry => "invalid_registry",
            Self::InvalidSnapshot => "invalid_snapshot",
            Self::SnapshotAccountMismatch => "snapshot_account_mismatch",
            Self::SnapshotNotFound => "snapshot_not_found",
            Self::UnsafePath => "unsafe_path",
            Self::CorruptJournal => "corrupt_journal",
            Self::InvalidJournalEvent => "invalid_journal_event",
            Self::SensitiveJournalEvent => "sensitive_journal_event",
            Self::InvalidTransitionState(_) => "invalid_transition_state",
            Self::InvalidConfig(_) => "invalid_config",
            Self::InvalidAutoResetState => "invalid_auto_reset_state",
            Self::AutoResetAttemptMismatch => "auto_reset_attempt_mismatch",
            Self::TransitionNotFound => "transition_not_found",
            Self::InsufficientTelemetry => "insufficient_telemetry",
            Self::Io(_) => "io_error",
            Self::Json(_) => "encoding_error",
        }
    }
}
