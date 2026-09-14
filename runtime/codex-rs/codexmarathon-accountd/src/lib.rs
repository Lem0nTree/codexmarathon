//! A small, user-scoped metadata daemon for CodexMarathon.
//!
//! `codexmarathon-accountd` is intentionally narrower than the native Codex
//! runtime.  It reads provider-neutral quota snapshots from
//! [`codexmarathon_runtime::QuotaSnapshotStore`], exposes those snapshots over
//! an owner-only Unix stream socket, and owns durable metadata about events
//! and scheduler jobs.  It never reads credentials, accepts prompts, or
//! executes commands.

#![expect(
    clippy::disallowed_methods,
    reason = "the daemon owns its private SQLite connection configuration"
)]

use chrono::{DateTime, Utc};
use codexmarathon_accountd_client as wire;
use codexmarathon_home::HomeError;
use codexmarathon_runtime::{
    AccountTelemetry, CredentialHealth, DomainError, FileAccountRegistry, QuotaSnapshotStore,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions, SqliteSynchronous};
use sqlx::{ConnectOptions, Row, Sqlite, SqlitePool, Transaction};
use std::collections::BTreeMap;
use std::future::Future;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncWrite, AsyncWriteExt, BufReader};
use tokio::net::{UnixListener, UnixStream};
use tokio::sync::{OnceCell, watch};
use tokio::task::JoinSet;

pub use codexmarathon_accountd_client::{
    DEFAULT_EVENT_LIMIT, MAX_EVENT_LIMIT, MAX_REQUEST_BYTES, MAX_RESPONSE_BYTES, PROTOCOL_VERSION,
    ProtocolError, Request, Response,
};

#[cfg(target_os = "linux")]
use std::os::linux::net::SocketAddrExt;
#[cfg(unix)]
use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};

/// Default deadline for one request read, handler operation, or response write.
pub const DEFAULT_REQUEST_TIMEOUT: Duration = Duration::from_secs(5);

/// Current schema owned by the accountd metadata tables.
pub const ACCOUNTD_SCHEMA_VERSION: i64 = 1;

const SQLITE_BUSY_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_IDENTIFIER_BYTES: usize = 256;
const MAX_DIAGNOSTIC_BYTES: usize = 256;
const MAX_EVENT_KIND_BYTES: usize = 64;
const RETRY_BACKOFF: Duration = Duration::from_secs(30);
const SERVICE_NAME: &str = "codexmarathon-accountd";
const ALLOWED_JOB_KINDS: &[&str] = &["account_observe", "quota_refresh"];

#[cfg(unix)]
fn current_uid() -> u32 {
    // SAFETY: geteuid has no preconditions and only reads this process's
    // effective user identity.
    unsafe { libc::geteuid() }
}

const CREATE_SCHEMA_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS accountd_schema (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    version INTEGER NOT NULL CHECK (version > 0)
)
"#;

const CREATE_EVENTS_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS accountd_events (
    event_id INTEGER PRIMARY KEY AUTOINCREMENT,
    kind TEXT NOT NULL,
    occurred_at_ms INTEGER NOT NULL,
    account_id TEXT,
    job_id TEXT,
    state TEXT,
    generation INTEGER,
    attempt INTEGER,
    diagnostic_code TEXT
)
"#;

const CREATE_JOBS_TABLE: &str = r#"
CREATE TABLE IF NOT EXISTS accountd_jobs (
    job_id TEXT PRIMARY KEY NOT NULL,
    kind TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    account_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('queued', 'running', 'retry_wait', 'succeeded', 'failed', 'cancelled')),
    generation INTEGER NOT NULL CHECK (generation >= 0),
    attempt INTEGER NOT NULL CHECK (attempt >= 0),
    run_after_ms INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    diagnostic_code TEXT
)
"#;

/// Errors returned by the daemon library.
///
/// The variants retain enough information for local startup diagnostics while
/// [`Self::public_code`] and [`Self::public_message`] deliberately collapse
/// storage and protocol details before they cross the socket boundary.
#[derive(Debug, thiserror::Error)]
pub enum AccountdError {
    #[error("accountd state is unavailable")]
    StateUnavailable,

    #[error("accountd configuration is invalid: {0}")]
    InvalidConfig(&'static str),

    #[error("accountd Codex home configuration is invalid")]
    Home(#[source] HomeError),

    #[error("accountd protocol request is invalid")]
    InvalidRequest,

    #[error("accountd protocol version is unsupported")]
    UnsupportedVersion,

    #[error("accountd request is too large")]
    RequestTooLarge,

    #[error("accountd request timed out")]
    RequestTimeout,

    #[error("accountd response is too large")]
    ResponseTooLarge,

    #[error("accountd socket is unavailable")]
    SocketUnavailable,

    #[error("accountd socket configuration is invalid: {0}")]
    SocketConfig(&'static str),

    #[error("accountd socket is already in use")]
    SocketInUse,

    #[error("accountd persistence failed")]
    Persistence(#[source] sqlx::Error),

    #[error("accountd persistence contains invalid metadata")]
    CorruptState,

    #[error("accountd quota state is unavailable")]
    Quota(#[source] DomainError),

    #[error("accountd I/O failed")]
    Io(#[source] io::Error),
}

impl AccountdError {
    /// Stable, secret-free error code for the wire protocol.
    pub const fn public_code(&self) -> &'static str {
        match self {
            Self::StateUnavailable | Self::Persistence(_) | Self::CorruptState => {
                "state_unavailable"
            }
            Self::InvalidConfig(_) | Self::Home(_) | Self::SocketConfig(_) => "invalid_config",
            Self::InvalidRequest => "invalid_request",
            Self::UnsupportedVersion => "unsupported_version",
            Self::RequestTooLarge => "request_too_large",
            Self::RequestTimeout => "request_timeout",
            Self::ResponseTooLarge => "response_too_large",
            Self::SocketUnavailable | Self::SocketInUse | Self::Io(_) => "socket_unavailable",
            Self::Quota(_) => "quota_unavailable",
        }
    }

    /// Stable, secret-free message for the wire protocol.
    pub const fn public_message(&self) -> &'static str {
        match self {
            Self::StateUnavailable | Self::Persistence(_) | Self::CorruptState => {
                "account metadata state is unavailable"
            }
            Self::InvalidConfig(_) | Self::Home(_) | Self::SocketConfig(_) => {
                "accountd configuration is invalid"
            }
            Self::InvalidRequest => "request is invalid",
            Self::UnsupportedVersion => "protocol version is unsupported",
            Self::RequestTooLarge => "request exceeds the size limit",
            Self::RequestTimeout => "request timed out",
            Self::ResponseTooLarge => "response exceeds the size limit",
            Self::SocketUnavailable | Self::SocketInUse | Self::Io(_) => "socket is unavailable",
            Self::Quota(_) => "quota metadata is unavailable",
        }
    }
}

impl From<sqlx::Error> for AccountdError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(error)
    }
}

impl From<DomainError> for AccountdError {
    fn from(error: DomainError) -> Self {
        Self::Quota(error)
    }
}

impl From<HomeError> for AccountdError {
    fn from(error: HomeError) -> Self {
        Self::Home(error)
    }
}

impl From<io::Error> for AccountdError {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

/// Metadata event vocabulary.  Event payloads are represented by bounded,
/// typed columns rather than an arbitrary JSON document.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum EventKind {
    #[serde(rename = "account.observed")]
    AccountObserved,
    #[serde(rename = "quota.observed")]
    QuotaObserved,
    #[serde(rename = "job.queued")]
    JobQueued,
    #[serde(rename = "job.started")]
    JobStarted,
    #[serde(rename = "job.retry_wait")]
    JobRetryWait,
    #[serde(rename = "job.succeeded")]
    JobSucceeded,
    #[serde(rename = "job.failed")]
    JobFailed,
    #[serde(rename = "job.cancelled")]
    JobCancelled,
    #[serde(rename = "executor.ready")]
    ExecutorReady,
    #[serde(rename = "executor.unavailable")]
    ExecutorUnavailable,
    #[serde(rename = "autospin.blocked")]
    AutospinBlocked,
}

impl EventKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::AccountObserved => "account.observed",
            Self::QuotaObserved => "quota.observed",
            Self::JobQueued => "job.queued",
            Self::JobStarted => "job.started",
            Self::JobRetryWait => "job.retry_wait",
            Self::JobSucceeded => "job.succeeded",
            Self::JobFailed => "job.failed",
            Self::JobCancelled => "job.cancelled",
            Self::ExecutorReady => "executor.ready",
            Self::ExecutorUnavailable => "executor.unavailable",
            Self::AutospinBlocked => "autospin.blocked",
        }
    }

    fn parse(value: &str) -> Result<Self, AccountdError> {
        match value {
            "account.observed" => Ok(Self::AccountObserved),
            "quota.observed" => Ok(Self::QuotaObserved),
            "job.queued" => Ok(Self::JobQueued),
            "job.started" => Ok(Self::JobStarted),
            "job.retry_wait" => Ok(Self::JobRetryWait),
            "job.succeeded" => Ok(Self::JobSucceeded),
            "job.failed" => Ok(Self::JobFailed),
            "job.cancelled" => Ok(Self::JobCancelled),
            "executor.ready" => Ok(Self::ExecutorReady),
            "executor.unavailable" => Ok(Self::ExecutorUnavailable),
            "autospin.blocked" => Ok(Self::AutospinBlocked),
            _ => Err(AccountdError::CorruptState),
        }
    }

    fn to_wire(self) -> wire::EventKind {
        match self {
            Self::AccountObserved => wire::EventKind::AccountObserved,
            Self::QuotaObserved => wire::EventKind::QuotaObserved,
            Self::JobQueued => wire::EventKind::JobQueued,
            Self::JobStarted => wire::EventKind::JobStarted,
            Self::JobRetryWait => wire::EventKind::JobRetryWait,
            Self::JobSucceeded => wire::EventKind::JobSucceeded,
            Self::JobFailed => wire::EventKind::JobFailed,
            Self::JobCancelled => wire::EventKind::JobCancelled,
            Self::ExecutorReady => wire::EventKind::ExecutorReady,
            Self::ExecutorUnavailable => wire::EventKind::ExecutorUnavailable,
            Self::AutospinBlocked => wire::EventKind::AutospinBlocked,
        }
    }
}

/// Durable scheduler state for one job.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum JobState {
    Queued,
    Running,
    RetryWait,
    Succeeded,
    Failed,
    Cancelled,
}

impl JobState {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Running => "running",
            Self::RetryWait => "retry_wait",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, AccountdError> {
        match value {
            "queued" => Ok(Self::Queued),
            "running" => Ok(Self::Running),
            "retry_wait" => Ok(Self::RetryWait),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(AccountdError::CorruptState),
        }
    }

    fn to_wire(self) -> wire::JobState {
        match self {
            Self::Queued => wire::JobState::Queued,
            Self::Running => wire::JobState::Running,
            Self::RetryWait => wire::JobState::RetryWait,
            Self::Succeeded => wire::JobState::Succeeded,
            Self::Failed => wire::JobState::Failed,
            Self::Cancelled => wire::JobState::Cancelled,
        }
    }
}

/// Metadata-only event returned by `events_since`.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct EventRecord {
    pub event_id: i64,
    pub kind: EventKind,
    pub occurred_at: DateTime<Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub job_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub state: Option<JobState>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub generation: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub attempt: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub diagnostic_code: Option<String>,
}

/// Bounded event input used by internal metadata producers.
#[derive(Clone, Debug)]
pub struct EventInput {
    pub kind: EventKind,
    pub account_id: Option<String>,
    pub job_id: Option<String>,
    pub state: Option<JobState>,
    pub generation: Option<i64>,
    pub attempt: Option<i64>,
    pub diagnostic_code: Option<String>,
    pub occurred_at: DateTime<Utc>,
}

impl Default for EventInput {
    fn default() -> Self {
        Self {
            kind: EventKind::ExecutorUnavailable,
            account_id: None,
            job_id: None,
            state: None,
            generation: None,
            attempt: None,
            diagnostic_code: None,
            occurred_at: Utc::now(),
        }
    }
}

impl EventInput {
    /// Construct an event with the current wall-clock timestamp.
    pub fn now(kind: EventKind) -> Self {
        Self {
            kind,
            occurred_at: Utc::now(),
            ..Self::default()
        }
    }
}

/// One internally queued scheduler job.  Job writes are intentionally not
/// reachable through the socket protocol; this type is for a future vetted
/// executor boundary and local tests.
#[derive(Clone, Debug, PartialEq)]
pub struct JobSpec {
    pub job_id: String,
    pub kind: String,
    pub idempotency_key: String,
    pub account_id: Option<String>,
    pub run_after: DateTime<Utc>,
}

/// Durable scheduler job metadata.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct JobRecord {
    pub job_id: String,
    pub kind: String,
    pub idempotency_key: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_id: Option<String>,
    pub state: JobState,
    pub generation: i64,
    pub attempt: i64,
    pub run_after: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub diagnostic_code: Option<String>,
}

/// Counts used in the status response and scheduler diagnostics.
#[derive(Clone, Debug, Default, PartialEq, Serialize)]
pub struct JobCounts {
    pub queued: i64,
    pub running: i64,
    pub retry_wait: i64,
    pub succeeded: i64,
    pub failed: i64,
    pub cancelled: i64,
}

/// A page from the durable event stream.
#[derive(Clone, Debug, PartialEq, Serialize)]
pub struct EventPage {
    pub after: i64,
    pub events: Vec<EventRecord>,
    pub next_after: i64,
    pub has_more: bool,
}

/// SQLite-backed event and scheduler metadata store sharing the quota DB.
#[derive(Clone)]
pub struct EventStore {
    path: PathBuf,
    pool: Arc<OnceCell<SqlitePool>>,
}

impl EventStore {
    /// Construct without touching the filesystem.
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self {
            path: path.into(),
            pool: Arc::new(OnceCell::new()),
        }
    }

    /// Open the shared DB, create accountd tables, and reconcile interrupted
    /// jobs before the daemon reports readiness.
    pub async fn open(path: impl Into<PathBuf>) -> Result<Self, AccountdError> {
        let store = Self::new(path);
        store.initialize().await?;
        store.reconcile_running_jobs().await?;
        Ok(store)
    }

    /// Database path used by this store.
    pub fn path(&self) -> &Path {
        &self.path
    }

    /// Initialize the accountd-owned schema exactly once per process.
    pub async fn initialize(&self) -> Result<(), AccountdError> {
        let path = self.path.clone();
        self.pool
            .get_or_try_init(|| async move { open_pool(&path).await })
            .await
            .map(|_| ())
    }

    async fn pool(&self) -> Result<&SqlitePool, AccountdError> {
        self.initialize().await?;
        self.pool.get().ok_or(AccountdError::StateUnavailable)
    }

    /// Append one bounded metadata event.
    ///
    /// This is a library seam for vetted producers only.  The socket handler
    /// does not expose an event-write method.
    pub async fn append_event(&self, input: EventInput) -> Result<EventRecord, AccountdError> {
        validate_event_input(&input)?;
        let pool = self.pool().await?;
        let mut transaction = pool.begin().await?;
        let event_id = insert_event(&mut transaction, &input).await?;
        transaction.commit().await?;
        Ok(EventRecord {
            event_id,
            kind: input.kind,
            occurred_at: input.occurred_at,
            account_id: input.account_id,
            job_id: input.job_id,
            state: input.state,
            generation: input.generation,
            attempt: input.attempt,
            diagnostic_code: input.diagnostic_code,
        })
    }

    /// Read events strictly after `after`, in monotonic event-id order.
    pub async fn events_since(&self, after: i64, limit: u32) -> Result<EventPage, AccountdError> {
        if after < 0 {
            return Err(AccountdError::InvalidRequest);
        }
        let limit = limit.clamp(1, MAX_EVENT_LIMIT);
        let pool = self.pool().await?;
        let rows = sqlx::query(
            r#"
SELECT event_id,
       kind,
       occurred_at_ms,
       account_id,
       job_id,
       state,
       generation,
       attempt,
       diagnostic_code
FROM accountd_events
WHERE event_id > ?
ORDER BY event_id ASC
LIMIT ?
            "#,
        )
        .bind(after)
        .bind(i64::from(limit) + 1)
        .fetch_all(pool)
        .await?;

        let has_more =
            rows.len() > usize::try_from(limit).map_err(|_| AccountdError::CorruptState)?;
        let records = rows
            .into_iter()
            .take(usize::try_from(limit).map_err(|_| AccountdError::CorruptState)?)
            .map(row_to_event)
            .collect::<Result<Vec<_>, _>>()?;
        let next_after = records.last().map_or(after, |event| event.event_id);
        Ok(EventPage {
            after,
            events: records,
            next_after,
            has_more,
        })
    }

    /// Return the largest committed event id, or zero for an empty stream.
    pub async fn latest_event_id(&self) -> Result<i64, AccountdError> {
        let pool = self.pool().await?;
        sqlx::query_scalar("SELECT COALESCE(MAX(event_id), 0) FROM accountd_events")
            .fetch_one(pool)
            .await
            .map_err(AccountdError::from)
    }

    /// Return current scheduler job counts without exposing job payloads.
    pub async fn job_counts(&self) -> Result<JobCounts, AccountdError> {
        let pool = self.pool().await?;
        let rows = sqlx::query("SELECT state, COUNT(*) AS count FROM accountd_jobs GROUP BY state")
            .fetch_all(pool)
            .await?;
        let mut counts = JobCounts::default();
        for row in rows {
            let state: String = row.try_get("state")?;
            let count: i64 = row.try_get("count")?;
            match JobState::parse(&state)? {
                JobState::Queued => counts.queued = count,
                JobState::Running => counts.running = count,
                JobState::RetryWait => counts.retry_wait = count,
                JobState::Succeeded => counts.succeeded = count,
                JobState::Failed => counts.failed = count,
                JobState::Cancelled => counts.cancelled = count,
            }
        }
        Ok(counts)
    }

    /// Queue a metadata-only job for a future vetted executor.
    pub async fn queue_job(&self, spec: JobSpec) -> Result<JobRecord, AccountdError> {
        validate_job_spec(&spec)?;
        let pool = self.pool().await?;
        let now = Utc::now();
        let mut transaction = pool.begin().await?;
        sqlx::query(
            r#"
INSERT INTO accountd_jobs (
    job_id, kind, idempotency_key, account_id, state, generation, attempt,
    run_after_ms, created_at_ms, updated_at_ms, diagnostic_code
)
VALUES (?, ?, ?, ?, 'queued', 0, 0, ?, ?, ?, NULL)
            "#,
        )
        .bind(&spec.job_id)
        .bind(&spec.kind)
        .bind(&spec.idempotency_key)
        .bind(&spec.account_id)
        .bind(spec.run_after.timestamp_millis())
        .bind(now.timestamp_millis())
        .bind(now.timestamp_millis())
        .execute(&mut *transaction)
        .await?;
        let input = EventInput {
            kind: EventKind::JobQueued,
            job_id: Some(spec.job_id.clone()),
            account_id: spec.account_id.clone(),
            state: Some(JobState::Queued),
            generation: Some(0),
            attempt: Some(0),
            occurred_at: now,
            ..EventInput::default()
        };
        insert_event(&mut transaction, &input).await?;
        transaction.commit().await?;
        Ok(JobRecord {
            job_id: spec.job_id,
            kind: spec.kind,
            idempotency_key: spec.idempotency_key,
            account_id: spec.account_id,
            state: JobState::Queued,
            generation: 0,
            attempt: 0,
            run_after: spec.run_after,
            created_at: now,
            updated_at: now,
            diagnostic_code: None,
        })
    }

    /// Mark a queued job running and record the corresponding event.
    pub async fn mark_job_running(&self, job_id: &str) -> Result<bool, AccountdError> {
        self.transition_job(
            job_id,
            JobState::Queued,
            JobState::Running,
            None,
            EventKind::JobStarted,
        )
        .await
    }

    /// Mark a running job succeeded and record the corresponding event.
    pub async fn mark_job_succeeded(&self, job_id: &str) -> Result<bool, AccountdError> {
        self.transition_job(
            job_id,
            JobState::Running,
            JobState::Succeeded,
            None,
            EventKind::JobSucceeded,
        )
        .await
    }

    /// Mark a running job failed with one bounded diagnostic code.
    pub async fn mark_job_failed(
        &self,
        job_id: &str,
        diagnostic_code: &str,
    ) -> Result<bool, AccountdError> {
        validate_diagnostic(diagnostic_code)?;
        self.transition_job(
            job_id,
            JobState::Running,
            JobState::Failed,
            Some(diagnostic_code.to_string()),
            EventKind::JobFailed,
        )
        .await
    }

    /// Cancel a queued or retry-waiting job without interrupting a running
    /// native side effect.
    pub async fn cancel_job(&self, job_id: &str) -> Result<bool, AccountdError> {
        validate_identifier(job_id, MAX_IDENTIFIER_BYTES)?;
        let pool = self.pool().await?;
        let now = Utc::now();
        let mut transaction = pool.begin().await?;
        let row = sqlx::query(
            r#"
SELECT kind, account_id, generation, attempt
FROM accountd_jobs
WHERE job_id = ? AND state IN ('queued', 'retry_wait')
            "#,
        )
        .bind(job_id)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            transaction.rollback().await?;
            return Ok(false);
        };
        let account_id: Option<String> = row.try_get("account_id")?;
        let generation: i64 = row.try_get("generation")?;
        let attempt: i64 = row.try_get("attempt")?;
        let updated = sqlx::query(
            "UPDATE accountd_jobs SET state = 'cancelled', updated_at_ms = ? WHERE job_id = ? AND state IN ('queued', 'retry_wait')",
        )
        .bind(now.timestamp_millis())
        .bind(job_id)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() == 0 {
            transaction.rollback().await?;
            return Ok(false);
        }
        let input = EventInput {
            kind: EventKind::JobCancelled,
            job_id: Some(job_id.to_string()),
            account_id,
            state: Some(JobState::Cancelled),
            generation: Some(generation),
            attempt: Some(attempt),
            occurred_at: now,
            ..EventInput::default()
        };
        insert_event(&mut transaction, &input).await?;
        transaction.commit().await?;
        Ok(true)
    }

    /// Reconcile jobs left in `running` after a process interruption.
    ///
    /// A non-empty idempotency key proves that retry can be made safely at the
    /// future executor boundary, so those jobs become `retry_wait`.  A
    /// malformed or missing key is conservatively terminally failed instead
    /// of guessed successful.
    pub async fn reconcile_running_jobs(&self) -> Result<usize, AccountdError> {
        self.reconcile_running_jobs_at(Utc::now()).await
    }

    /// Deterministic-clock variant used by startup tests.
    pub async fn reconcile_running_jobs_at(
        &self,
        now: DateTime<Utc>,
    ) -> Result<usize, AccountdError> {
        let pool = self.pool().await?;
        let rows = sqlx::query(
            r#"
SELECT job_id, idempotency_key, account_id, generation, attempt
FROM accountd_jobs
WHERE state = 'running'
ORDER BY job_id
            "#,
        )
        .fetch_all(pool)
        .await?;
        let mut reconciled = 0;
        for row in rows {
            let job_id: String = row.try_get("job_id")?;
            let idempotency_key: String = row.try_get("idempotency_key")?;
            let account_id: Option<String> = row.try_get("account_id")?;
            let generation: i64 = row.try_get("generation")?;
            let attempt: i64 = row.try_get("attempt")?;
            let retry_safe = validate_idempotency_key(&idempotency_key).is_ok();
            let (new_state, kind, diagnostic, run_after, next_attempt) = if retry_safe {
                (
                    JobState::RetryWait,
                    EventKind::JobRetryWait,
                    None,
                    now + chrono::Duration::from_std(RETRY_BACKOFF)
                        .map_err(|_| AccountdError::CorruptState)?,
                    attempt.saturating_add(1),
                )
            } else {
                (
                    JobState::Failed,
                    EventKind::JobFailed,
                    Some("unsafe_retry"),
                    now,
                    attempt,
                )
            };
            let mut transaction = pool.begin().await?;
            let update = sqlx::query(
                r#"
UPDATE accountd_jobs
SET state = ?, attempt = ?, run_after_ms = ?, updated_at_ms = ?, diagnostic_code = ?
WHERE job_id = ? AND state = 'running'
                "#,
            )
            .bind(new_state.as_str())
            .bind(next_attempt)
            .bind(run_after.timestamp_millis())
            .bind(now.timestamp_millis())
            .bind(diagnostic)
            .bind(&job_id)
            .execute(&mut *transaction)
            .await?;
            if update.rows_affected() == 0 {
                transaction.rollback().await?;
                continue;
            }
            let input = EventInput {
                kind,
                account_id,
                job_id: Some(job_id),
                state: Some(new_state),
                generation: Some(generation),
                attempt: Some(next_attempt),
                diagnostic_code: diagnostic.map(str::to_string),
                occurred_at: now,
            };
            insert_event(&mut transaction, &input).await?;
            transaction.commit().await?;
            reconciled += 1;
        }
        Ok(reconciled)
    }

    async fn transition_job(
        &self,
        job_id: &str,
        expected: JobState,
        next: JobState,
        diagnostic: Option<String>,
        event_kind: EventKind,
    ) -> Result<bool, AccountdError> {
        validate_identifier(job_id, MAX_IDENTIFIER_BYTES)?;
        let pool = self.pool().await?;
        let now = Utc::now();
        let mut transaction = pool.begin().await?;
        let row = sqlx::query(
            "SELECT account_id, generation, attempt FROM accountd_jobs WHERE job_id = ? AND state = ?",
        )
        .bind(job_id)
        .bind(expected.as_str())
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            transaction.rollback().await?;
            return Ok(false);
        };
        let account_id: Option<String> = row.try_get("account_id")?;
        let generation: i64 = row.try_get("generation")?;
        let attempt: i64 = row.try_get("attempt")?;
        let updated = sqlx::query(
            "UPDATE accountd_jobs SET state = ?, updated_at_ms = ?, diagnostic_code = ? WHERE job_id = ? AND state = ?",
        )
        .bind(next.as_str())
        .bind(now.timestamp_millis())
        .bind(&diagnostic)
        .bind(job_id)
        .bind(expected.as_str())
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() == 0 {
            transaction.rollback().await?;
            return Ok(false);
        }
        let input = EventInput {
            kind: event_kind,
            job_id: Some(job_id.to_string()),
            account_id,
            state: Some(next),
            generation: Some(generation),
            attempt: Some(attempt),
            diagnostic_code: diagnostic,
            occurred_at: now,
        };
        insert_event(&mut transaction, &input).await?;
        transaction.commit().await?;
        Ok(true)
    }
}

async fn open_pool(path: &Path) -> Result<SqlitePool, AccountdError> {
    if path.as_os_str().is_empty() {
        return Err(AccountdError::InvalidConfig(
            "metadata database path is empty",
        ));
    }
    let parent = path.parent().unwrap_or_else(|| Path::new("."));
    ensure_private_dir(parent)?;
    reject_unsafe_database_file(path)?;
    let options = SqliteConnectOptions::new()
        .filename(path)
        .create_if_missing(true)
        .journal_mode(SqliteJournalMode::Wal)
        .synchronous(SqliteSynchronous::Normal)
        .foreign_keys(true)
        .busy_timeout(SQLITE_BUSY_TIMEOUT)
        .log_statements(log::LevelFilter::Off);
    let pool = SqlitePoolOptions::new()
        .max_connections(4)
        .connect_with(options)
        .await?;
    set_private_file_permissions(path)?;
    if let Err(error) = initialize_schema(&pool).await {
        pool.close().await;
        return Err(error);
    }
    set_private_file_permissions(path)?;
    Ok(pool)
}

async fn initialize_schema(pool: &SqlitePool) -> Result<(), AccountdError> {
    let mut transaction = pool.begin().await?;
    sqlx::query(CREATE_SCHEMA_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(CREATE_EVENTS_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(CREATE_JOBS_TABLE)
        .execute(&mut *transaction)
        .await?;
    sqlx::query(
        "INSERT INTO accountd_schema (singleton, version) VALUES (1, ?) ON CONFLICT(singleton) DO NOTHING",
    )
    .bind(ACCOUNTD_SCHEMA_VERSION)
    .execute(&mut *transaction)
    .await?;
    let version: i64 =
        sqlx::query_scalar("SELECT version FROM accountd_schema WHERE singleton = 1")
            .fetch_one(&mut *transaction)
            .await?;
    if version != ACCOUNTD_SCHEMA_VERSION {
        transaction.rollback().await?;
        return Err(AccountdError::StateUnavailable);
    }
    transaction.commit().await?;
    Ok(())
}

async fn insert_event(
    transaction: &mut Transaction<'_, Sqlite>,
    input: &EventInput,
) -> Result<i64, AccountdError> {
    validate_event_input(input)?;
    let result = sqlx::query(
        r#"
INSERT INTO accountd_events (
    kind, occurred_at_ms, account_id, job_id, state, generation, attempt, diagnostic_code
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        "#,
    )
    .bind(input.kind.as_str())
    .bind(input.occurred_at.timestamp_millis())
    .bind(&input.account_id)
    .bind(&input.job_id)
    .bind(input.state.map(JobState::as_str))
    .bind(input.generation)
    .bind(input.attempt)
    .bind(&input.diagnostic_code)
    .execute(&mut **transaction)
    .await?;
    Ok(result.last_insert_rowid())
}

fn row_to_event(row: sqlx::sqlite::SqliteRow) -> Result<EventRecord, AccountdError> {
    let event_id: i64 = row.try_get("event_id")?;
    let kind = EventKind::parse(row.try_get::<String, _>("kind")?.as_str())?;
    let occurred_at = DateTime::from_timestamp_millis(row.try_get("occurred_at_ms")?)
        .ok_or(AccountdError::CorruptState)?;
    let state = row
        .try_get::<Option<String>, _>("state")?
        .map(|value| JobState::parse(&value))
        .transpose()?;
    Ok(EventRecord {
        event_id,
        kind,
        occurred_at,
        account_id: row.try_get("account_id")?,
        job_id: row.try_get("job_id")?,
        state,
        generation: row.try_get("generation")?,
        attempt: row.try_get("attempt")?,
        diagnostic_code: row.try_get("diagnostic_code")?,
    })
}

fn validate_event_input(input: &EventInput) -> Result<(), AccountdError> {
    validate_identifier(input.kind.as_str(), MAX_EVENT_KIND_BYTES)?;
    if let Some(account_id) = input.account_id.as_deref() {
        validate_identifier(account_id, MAX_IDENTIFIER_BYTES)?;
    }
    if let Some(job_id) = input.job_id.as_deref() {
        validate_identifier(job_id, MAX_IDENTIFIER_BYTES)?;
    }
    if let Some(diagnostic) = input.diagnostic_code.as_deref() {
        validate_diagnostic(diagnostic)?;
    }
    if input.generation.is_some_and(|value| value < 0)
        || input.attempt.is_some_and(|value| value < 0)
    {
        return Err(AccountdError::InvalidRequest);
    }
    Ok(())
}

fn validate_job_spec(spec: &JobSpec) -> Result<(), AccountdError> {
    validate_identifier(&spec.job_id, MAX_IDENTIFIER_BYTES)?;
    if !ALLOWED_JOB_KINDS.contains(&spec.kind.as_str()) {
        return Err(AccountdError::InvalidRequest);
    }
    validate_identifier(&spec.kind, MAX_IDENTIFIER_BYTES)?;
    validate_idempotency_key(&spec.idempotency_key)?;
    if let Some(account_id) = spec.account_id.as_deref() {
        validate_identifier(account_id, MAX_IDENTIFIER_BYTES)?;
    }
    Ok(())
}

fn validate_idempotency_key(value: &str) -> Result<(), AccountdError> {
    validate_identifier(value, MAX_IDENTIFIER_BYTES)?;
    if !value
        .bytes()
        .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'_' | b':'))
    {
        return Err(AccountdError::InvalidRequest);
    }
    Ok(())
}

fn validate_diagnostic(value: &str) -> Result<(), AccountdError> {
    validate_identifier(value, MAX_DIAGNOSTIC_BYTES)
}

fn validate_identifier(value: &str, max_bytes: usize) -> Result<(), AccountdError> {
    if value.is_empty()
        || value.len() > max_bytes
        || value
            .chars()
            .any(|character| character == '\0' || character.is_control())
    {
        return Err(AccountdError::InvalidRequest);
    }
    Ok(())
}

/// Scheduler lifecycle state.  The initial daemon has no executor and thus
/// deliberately leaves queued work untouched; it still owns reconciliation
/// and the future polling boundary.
#[derive(Clone)]
pub struct Scheduler {
    store: EventStore,
}

impl Scheduler {
    /// Recover interrupted jobs and return a scheduler ready for polling.
    pub async fn initialize(store: EventStore) -> Result<Self, AccountdError> {
        store.reconcile_running_jobs().await?;
        Ok(Self { store })
    }

    /// True once startup reconciliation has completed.
    pub const fn is_ready(&self) -> bool {
        true
    }

    /// Run the conservative polling loop until shutdown.  No external
    /// executor is called by this foundation; due jobs remain durable and
    /// visible for the later constrained executor boundary.
    pub async fn run(&self, mut shutdown: watch::Receiver<bool>) {
        loop {
            tokio::select! {
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        break;
                    }
                }
                _ = tokio::time::sleep(Duration::from_secs(30)) => {
                    let _ = self.store.reconcile_running_jobs().await;
                }
            }
        }
    }
}

#[derive(Clone, Debug, Deserialize, Default)]
struct EventsSinceParams {
    #[serde(
        default,
        alias = "since",
        alias = "cursor",
        alias = "from_event_id",
        alias = "since_event_id"
    )]
    after: Option<i64>,
    #[serde(default)]
    limit: Option<u32>,
}

fn protocol_error_response(id: Option<wire::RequestId>, error: &AccountdError) -> Response {
    Response::error(id, error.public_code(), error.public_message())
}

fn wire_credential_health(health: CredentialHealth) -> wire::CredentialHealth {
    match health {
        CredentialHealth::Unknown => wire::CredentialHealth::Unknown,
        CredentialHealth::Healthy => wire::CredentialHealth::Healthy,
        CredentialHealth::Stale => wire::CredentialHealth::Stale,
        CredentialHealth::Invalid => wire::CredentialHealth::Invalid,
    }
}

fn wire_telemetry(telemetry: AccountTelemetry) -> wire::AccountTelemetry {
    wire::AccountTelemetry {
        account_id: telemetry.account_id,
        limits: telemetry
            .limits
            .into_iter()
            .map(|(key, limit)| {
                (
                    key,
                    wire::LimitTelemetry {
                        limit_id: limit.limit_id,
                        limit_name: limit.limit_name,
                        plan_type: limit.plan_type,
                        windows: limit
                            .windows
                            .into_iter()
                            .map(|window| wire::UsageWindow {
                                kind: match window.kind {
                                    codexmarathon_runtime::WindowKind::Primary => {
                                        wire::WindowKind::Primary
                                    }
                                    codexmarathon_runtime::WindowKind::Secondary => {
                                        wire::WindowKind::Secondary
                                    }
                                },
                                used_percent: window.used_percent,
                                window_duration_mins: window.window_duration_mins,
                                resets_at: window.resets_at,
                                observed_at: window.observed_at,
                                freshness: match window.freshness {
                                    codexmarathon_runtime::Freshness::Fresh => {
                                        wire::Freshness::Fresh
                                    }
                                    codexmarathon_runtime::Freshness::Stale => {
                                        wire::Freshness::Stale
                                    }
                                },
                            })
                            .collect(),
                    },
                )
            })
            .collect(),
        observed_at: telemetry.observed_at,
        usable: telemetry.usable,
        reset_capability: match telemetry.reset_capability {
            codexmarathon_runtime::QuotaResetCapability::Unavailable => {
                wire::QuotaResetCapability::Unavailable
            }
            codexmarathon_runtime::QuotaResetCapability::Available { credit_id } => {
                wire::QuotaResetCapability::Available { credit_id }
            }
        },
        source: telemetry.source,
    }
}

fn wire_job_counts(counts: JobCounts) -> wire::JobCounts {
    wire::JobCounts {
        queued: counts.queued,
        running: counts.running,
        retry_wait: counts.retry_wait,
        succeeded: counts.succeeded,
        failed: counts.failed,
        cancelled: counts.cancelled,
    }
}

fn wire_event_page(page: EventPage) -> wire::EventPage {
    wire::EventPage {
        after: page.after,
        events: page
            .events
            .into_iter()
            .map(|event| wire::EventRecord {
                event_id: event.event_id,
                kind: event.kind.to_wire(),
                occurred_at: event.occurred_at,
                account_id: event.account_id,
                job_id: event.job_id,
                state: event.state.map(JobState::to_wire),
                generation: event.generation,
                attempt: event.attempt,
                diagnostic_code: event.diagnostic_code,
            })
            .collect(),
        next_after: page.next_after,
        has_more: page.has_more,
    }
}

/// Shared daemon state used by each client connection.
#[derive(Clone)]
pub struct AccountDaemon {
    quota_store: Arc<QuotaSnapshotStore>,
    registry: Option<Arc<FileAccountRegistry>>,
    event_store: EventStore,
    scheduler: Scheduler,
    request_timeout: Duration,
}

impl AccountDaemon {
    /// Open the quota store and accountd metadata store at one DB path.
    pub async fn open(
        quota_db_path: impl Into<PathBuf>,
        request_timeout: Duration,
    ) -> Result<Self, AccountdError> {
        if request_timeout.is_zero() {
            return Err(AccountdError::InvalidConfig(
                "request timeout must be positive",
            ));
        }
        let quota_db_path = quota_db_path.into();
        let quota_store = Arc::new(
            QuotaSnapshotStore::open(&quota_db_path)
                .await
                .map_err(AccountdError::Quota)?,
        );
        let event_store = EventStore::open(quota_store.path().to_path_buf()).await?;
        let registry = quota_db_path
            .parent()
            .map(|parent| Arc::new(FileAccountRegistry::new(parent.join("accounts.json"))));
        let scheduler = Scheduler::initialize(event_store.clone()).await?;
        Ok(Self {
            quota_store,
            registry,
            event_store,
            scheduler,
            request_timeout,
        })
    }

    /// Build a daemon around already-opened stores, primarily for tests.
    pub async fn from_stores(
        quota_store: Arc<QuotaSnapshotStore>,
        event_store: EventStore,
        request_timeout: Duration,
    ) -> Result<Self, AccountdError> {
        if request_timeout.is_zero() {
            return Err(AccountdError::InvalidConfig(
                "request timeout must be positive",
            ));
        }
        let scheduler = Scheduler::initialize(event_store.clone()).await?;
        Ok(Self {
            quota_store,
            registry: None,
            event_store,
            scheduler,
            request_timeout,
        })
    }

    /// Configured per-request deadline.
    pub const fn request_timeout(&self) -> Duration {
        self.request_timeout
    }

    /// Handle one decoded request without exposing storage errors or arbitrary
    /// request data in the response.
    pub async fn handle_request(&self, request: Request) -> Response {
        if !codexmarathon_accountd_client::is_valid_request_id(request.id.as_ref()) {
            return protocol_error_response(None, &AccountdError::InvalidRequest);
        }
        let id = request.id;
        if request.version != PROTOCOL_VERSION {
            return protocol_error_response(id, &AccountdError::UnsupportedVersion);
        }
        if request.method.is_empty() || request.method.len() > MAX_IDENTIFIER_BYTES {
            return protocol_error_response(id, &AccountdError::InvalidRequest);
        }
        let result = match request.method.as_str() {
            "health" => serde_json::to_value(wire::HealthResult {
                service: SERVICE_NAME.to_owned(),
                protocol_version: PROTOCOL_VERSION,
                status: "ok".to_owned(),
            })
            .map_err(|_| AccountdError::StateUnavailable),
            "status" => self.status_result().await.and_then(|value| {
                serde_json::to_value(value).map_err(|_| AccountdError::StateUnavailable)
            }),
            "accounts" => self.accounts_result().await.and_then(|value| {
                serde_json::to_value(value).map_err(|_| AccountdError::StateUnavailable)
            }),
            "events_since" => self
                .events_since_result(request.params)
                .await
                .and_then(|value| {
                    serde_json::to_value(value).map_err(|_| AccountdError::StateUnavailable)
                }),
            _ => Err(AccountdError::InvalidRequest),
        };
        match result {
            Ok(value) => Response::success(id, value),
            Err(error) => protocol_error_response(id, &error),
        }
    }

    async fn status_result(&self) -> Result<wire::StatusResult, AccountdError> {
        let account_count = self.accounts_result().await?.count;
        Ok(wire::StatusResult {
            service: SERVICE_NAME.to_owned(),
            protocol_version: PROTOCOL_VERSION,
            ready: true,
            quota_database: "ready".to_owned(),
            account_count,
            latest_event_id: self.event_store.latest_event_id().await?,
            scheduler_ready: self.scheduler.is_ready(),
            jobs: wire_job_counts(self.event_store.job_counts().await?),
        })
    }

    async fn accounts_result(&self) -> Result<wire::AccountsResult, AccountdError> {
        let telemetry = self
            .quota_store
            .list()
            .await
            .map_err(AccountdError::Quota)?;
        let mut quota_by_id: BTreeMap<_, _> = telemetry
            .into_iter()
            .map(|snapshot| (snapshot.account_id.clone(), snapshot))
            .collect();
        let mut accounts = Vec::new();
        if let Some(registry) = &self.registry {
            let state = registry.state().map_err(AccountdError::Quota)?;
            for record in state.accounts.values() {
                accounts.push(wire::DaemonAccount {
                    account_id: record.id.clone(),
                    alias: record.display_name().to_owned(),
                    active: record.id == state.active_account_id,
                    credential_health: wire_credential_health(record.credential_health),
                    quota: quota_by_id.remove(&record.id).map(wire_telemetry),
                });
            }
        }
        accounts.extend(quota_by_id.into_values().map(|quota| wire::DaemonAccount {
            alias: quota.account_id.clone(),
            account_id: quota.account_id.clone(),
            active: false,
            credential_health: wire::CredentialHealth::Unknown,
            quota: Some(wire_telemetry(quota)),
        }));
        let count = accounts.len();
        Ok(wire::AccountsResult { accounts, count })
    }

    async fn events_since_result(&self, params: Value) -> Result<wire::EventPage, AccountdError> {
        let params = if params.is_null() {
            EventsSinceParams::default()
        } else {
            serde_json::from_value(params).map_err(|_| AccountdError::InvalidRequest)?
        };
        let after = params.after.unwrap_or(0);
        let limit = params.limit.unwrap_or(DEFAULT_EVENT_LIMIT);
        if limit == 0 {
            return Err(AccountdError::InvalidRequest);
        }
        Ok(wire_event_page(
            self.event_store.events_since(after, limit).await?,
        ))
    }

    /// Serve accepted connections until the supplied shutdown future resolves.
    pub async fn serve<F>(&self, listener: DaemonListener, shutdown: F) -> Result<(), AccountdError>
    where
        F: Future<Output = ()> + Send,
    {
        let (shutdown_tx, shutdown_rx) = watch::channel(false);
        let scheduler_task = {
            let scheduler = self.scheduler.clone();
            tokio::spawn(async move { scheduler.run(shutdown_rx).await })
        };
        let mut connections = JoinSet::new();
        tokio::pin!(shutdown);
        loop {
            tokio::select! {
                _ = &mut shutdown => break,
                accepted = listener.accept() => {
                    let (stream, _) = accepted?;
                    let daemon = self.clone();
                    connections.spawn(async move {
                        let _ = serve_connection(stream, daemon).await;
                    });
                }
            }
        }
        let _ = shutdown_tx.send(true);
        drop(listener);
        let _ = tokio::time::timeout(Duration::from_secs(1), async {
            while connections.join_next().await.is_some() {}
        })
        .await;
        let _ = scheduler_task.await;
        Ok(())
    }
}

async fn serve_connection(stream: UnixStream, daemon: AccountDaemon) -> Result<(), AccountdError> {
    let (reader, mut writer) = stream.into_split();
    let mut reader = BufReader::new(reader);
    loop {
        let line = match tokio::time::timeout(
            daemon.request_timeout,
            read_line_bounded(&mut reader),
        )
        .await
        {
            Ok(Ok(Some(line))) => line,
            Ok(Ok(None)) => return Ok(()),
            Ok(Err(error)) => {
                let response = protocol_error_response(None, &error);
                write_response(&mut writer, &daemon, &response).await?;
                return Ok(());
            }
            Err(_) => {
                let response = protocol_error_response(None, &AccountdError::RequestTimeout);
                write_response(&mut writer, &daemon, &response).await?;
                return Ok(());
            }
        };
        let response = match serde_json::from_slice::<Request>(&line) {
            Ok(request) => {
                match tokio::time::timeout(daemon.request_timeout, daemon.handle_request(request))
                    .await
                {
                    Ok(response) => response,
                    Err(_) => protocol_error_response(None, &AccountdError::RequestTimeout),
                }
            }
            Err(_) => protocol_error_response(None, &AccountdError::InvalidRequest),
        };
        write_response(&mut writer, &daemon, &response).await?;
    }
}

async fn read_line_bounded<R>(reader: &mut R) -> Result<Option<Vec<u8>>, AccountdError>
where
    R: AsyncBufRead + Unpin,
{
    let mut line = Vec::with_capacity(1024);
    loop {
        let available = reader.fill_buf().await?;
        if available.is_empty() {
            if line.is_empty() {
                return Ok(None);
            }
            return Ok(Some(line));
        }
        let newline = available.iter().position(|byte| *byte == b'\n');
        let take = newline.map_or(available.len(), |index| index + 1);
        let content_take = newline.unwrap_or(take);
        if line.len().saturating_add(content_take) > MAX_REQUEST_BYTES {
            return Err(AccountdError::RequestTooLarge);
        }
        line.extend_from_slice(&available[..take]);
        reader.consume(take);
        if newline.is_some() {
            if line.last() == Some(&b'\n') {
                line.pop();
            }
            if line.last() == Some(&b'\r') {
                line.pop();
            }
            return Ok(Some(line));
        }
    }
}

async fn write_response<W>(
    writer: &mut W,
    daemon: &AccountDaemon,
    response: &Response,
) -> Result<(), AccountdError>
where
    W: AsyncWrite + Unpin,
{
    let mut bytes = serde_json::to_vec(response).map_err(|_| AccountdError::StateUnavailable)?;
    if bytes.len() > MAX_RESPONSE_BYTES {
        bytes = serde_json::to_vec(&protocol_error_response(
            None,
            &AccountdError::ResponseTooLarge,
        ))
        .map_err(|_| AccountdError::StateUnavailable)?;
    }
    bytes.push(b'\n');
    tokio::time::timeout(daemon.request_timeout, writer.write_all(&bytes))
        .await
        .map_err(|_| AccountdError::RequestTimeout)??;
    Ok(())
}

/// A listener plus ownership-aware cleanup for explicitly bound sockets.
pub struct DaemonListener {
    listener: UnixListener,
    cleanup: Option<SocketCleanup>,
}

impl DaemonListener {
    /// Adopt systemd fd 3 when present, otherwise bind the explicit path.
    #[cfg(unix)]
    pub async fn from_systemd_or_path(socket_path: Option<PathBuf>) -> Result<Self, AccountdError> {
        let activated = activated_listener()?;
        if activated.is_some() && socket_path.is_some() {
            return Err(AccountdError::SocketConfig(
                "do not combine socket activation with --socket",
            ));
        }
        if let Some(listener) = activated {
            listener.set_nonblocking(true).map_err(AccountdError::Io)?;
            validate_activated_listener(&listener)?;
            return UnixListener::from_std(listener)
                .map(|listener| Self {
                    listener,
                    cleanup: None,
                })
                .map_err(AccountdError::Io);
        }
        let Some(socket_path) = socket_path else {
            return Err(AccountdError::SocketConfig(
                "--socket is required when socket activation is unavailable",
            ));
        };
        Self::bind_explicit(socket_path).await
    }

    /// Bind an owner-only Unix socket for foreground and development use.
    #[cfg(unix)]
    pub async fn bind_explicit(socket_path: PathBuf) -> Result<Self, AccountdError> {
        validate_socket_path(&socket_path)?;
        let parent = socket_path.parent().unwrap_or_else(|| Path::new("."));
        ensure_private_dir(parent)?;
        prepare_socket_path(&socket_path)?;
        let listener = std::os::unix::net::UnixListener::bind(&socket_path).map_err(|error| {
            match error.kind() {
                io::ErrorKind::AddrInUse => AccountdError::SocketInUse,
                _ => AccountdError::Io(error),
            }
        })?;
        listener.set_nonblocking(true).map_err(AccountdError::Io)?;
        set_private_socket_permissions(&socket_path)?;
        let metadata = std::fs::symlink_metadata(&socket_path).map_err(AccountdError::Io)?;
        let cleanup = SocketCleanup {
            path: socket_path,
            dev: metadata.dev(),
            ino: metadata.ino(),
        };
        let listener = UnixListener::from_std(listener).map_err(AccountdError::Io)?;
        Ok(Self {
            listener,
            cleanup: Some(cleanup),
        })
    }

    /// Accept one local client.
    pub async fn accept(
        &self,
    ) -> Result<(UnixStream, tokio::net::unix::SocketAddr), AccountdError> {
        self.listener.accept().await.map_err(AccountdError::Io)
    }
}

impl Drop for DaemonListener {
    fn drop(&mut self) {
        #[cfg(unix)]
        if let Some(cleanup) = self.cleanup.take() {
            if let Ok(metadata) = std::fs::symlink_metadata(&cleanup.path)
                && metadata.dev() == cleanup.dev
                && metadata.ino() == cleanup.ino
            {
                let _ = std::fs::remove_file(cleanup.path);
            }
        }
    }
}

#[cfg(not(unix))]
impl DaemonListener {
    /// Unix sockets are unavailable on non-Unix targets.
    pub async fn from_systemd_or_path(
        _socket_path: Option<PathBuf>,
    ) -> Result<Self, AccountdError> {
        Err(AccountdError::SocketConfig(
            "accountd requires a Unix target",
        ))
    }
}

#[cfg(unix)]
struct SocketCleanup {
    path: PathBuf,
    dev: u64,
    ino: u64,
}

#[cfg(unix)]
fn validate_socket_path(path: &Path) -> Result<(), AccountdError> {
    if path.as_os_str().is_empty() || path.as_os_str().len() > 100 {
        return Err(AccountdError::SocketConfig(
            "socket path is empty or too long",
        ));
    }
    Ok(())
}

#[cfg(unix)]
fn activated_listener() -> Result<Option<std::os::unix::net::UnixListener>, AccountdError> {
    let listen_pid = std::env::var_os("LISTEN_PID");
    let listen_fds = std::env::var_os("LISTEN_FDS");
    if listen_pid.is_none() && listen_fds.is_none() {
        return Ok(None);
    }
    let Some(pid) = listen_pid else {
        return Err(AccountdError::SocketConfig("LISTEN_PID is missing"));
    };
    let Some(fds) = listen_fds else {
        return Err(AccountdError::SocketConfig("LISTEN_FDS is missing"));
    };
    let pid = pid
        .to_str()
        .and_then(|value| value.parse::<u32>().ok())
        .ok_or(AccountdError::SocketConfig("LISTEN_PID is invalid"))?;
    if pid != std::process::id() {
        return Err(AccountdError::SocketConfig(
            "LISTEN_PID does not match accountd",
        ));
    }
    let fds = fds
        .to_str()
        .and_then(|value| value.parse::<u32>().ok())
        .ok_or(AccountdError::SocketConfig("LISTEN_FDS is invalid"))?;
    if fds != 1 {
        return Err(AccountdError::SocketConfig(
            "accountd requires exactly one activated socket",
        ));
    }
    use std::os::fd::FromRawFd;
    // SAFETY: systemd's LISTEN_FDS contract places the first listener at fd 3;
    // ownership is transferred to this UnixListener immediately.
    let listener = unsafe { std::os::unix::net::UnixListener::from_raw_fd(3) };
    Ok(Some(listener))
}

#[cfg(unix)]
fn validate_activated_listener(
    listener: &std::os::unix::net::UnixListener,
) -> Result<(), AccountdError> {
    let address = listener.local_addr().map_err(AccountdError::Io)?;
    let Some(path) = address.as_pathname() else {
        return Err(AccountdError::SocketConfig(
            "activated socket must have a filesystem pathname",
        ));
    };
    let metadata = std::fs::symlink_metadata(path).map_err(AccountdError::Io)?;
    if !metadata.file_type().is_socket()
        || metadata.uid() != current_uid()
        || metadata.mode() & 0o077 != 0
    {
        return Err(AccountdError::SocketConfig(
            "activated socket is not owner-only",
        ));
    }
    Ok(())
}

#[cfg(unix)]
fn prepare_socket_path(path: &Path) -> Result<(), AccountdError> {
    let Ok(metadata) = std::fs::symlink_metadata(path) else {
        return Ok(());
    };
    if metadata.file_type().is_symlink() || !metadata.file_type().is_socket() {
        return Err(AccountdError::SocketConfig(
            "socket path is not a regular Unix socket entry",
        ));
    }
    if metadata.uid() != current_uid() || metadata.mode() & 0o077 != 0 {
        return Err(AccountdError::SocketConfig(
            "existing socket is not owner-only",
        ));
    }
    match std::os::unix::net::UnixStream::connect(path) {
        Ok(_) => Err(AccountdError::SocketInUse),
        Err(error)
            if matches!(
                error.kind(),
                io::ErrorKind::ConnectionRefused | io::ErrorKind::NotFound
            ) =>
        {
            std::fs::remove_file(path).map_err(AccountdError::Io)
        }
        Err(error) => Err(AccountdError::Io(error)),
    }
}

fn ensure_private_dir(path: &Path) -> Result<(), AccountdError> {
    if path.as_os_str().is_empty() {
        return Err(AccountdError::InvalidConfig(
            "private directory path is empty",
        ));
    }
    if !path.exists() {
        std::fs::create_dir_all(path).map_err(AccountdError::Io)?;
    }
    let metadata = std::fs::symlink_metadata(path).map_err(AccountdError::Io)?;
    if !metadata.is_dir() {
        return Err(AccountdError::InvalidConfig(
            "private directory is not a directory",
        ));
    }
    #[cfg(unix)]
    {
        if metadata.uid() != current_uid() || metadata.mode() & 0o077 != 0 {
            return Err(AccountdError::InvalidConfig(
                "private directory is accessible by another user",
            ));
        }
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
            .map_err(AccountdError::Io)?;
    }
    Ok(())
}

fn reject_unsafe_database_file(path: &Path) -> Result<(), AccountdError> {
    let Ok(metadata) = std::fs::symlink_metadata(path) else {
        return Ok(());
    };
    if metadata.file_type().is_symlink() || !metadata.file_type().is_file() {
        return Err(AccountdError::InvalidConfig(
            "metadata database path is not a regular file",
        ));
    }
    #[cfg(unix)]
    if metadata.uid() != current_uid() || metadata.mode() & 0o077 != 0 {
        return Err(AccountdError::InvalidConfig(
            "metadata database is accessible by another user",
        ));
    }
    Ok(())
}

fn set_private_file_permissions(path: &Path) -> Result<(), AccountdError> {
    #[cfg(unix)]
    {
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
            .map_err(AccountdError::Io)?;
    }
    Ok(())
}

#[cfg(unix)]
fn set_private_socket_permissions(path: &Path) -> Result<(), AccountdError> {
    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
        .map_err(AccountdError::Io)
}

/// Send a systemd notification when `NOTIFY_SOCKET` is configured.
///
/// Returns `Ok(false)` when no notification socket is configured.  The
/// function uses only the standard library, including support for systemd's
/// abstract-namespace `@name` addresses on Unix.
#[cfg(unix)]
pub fn sd_notify(message: &str) -> Result<bool, AccountdError> {
    if message.is_empty() || message.as_bytes().contains(&0) {
        return Err(AccountdError::InvalidConfig("invalid sd_notify message"));
    }
    let Some(socket) = std::env::var_os("NOTIFY_SOCKET") else {
        return Ok(false);
    };
    let socket = socket
        .to_str()
        .ok_or(AccountdError::InvalidConfig("NOTIFY_SOCKET is not UTF-8"))?;
    let datagram = std::os::unix::net::UnixDatagram::unbound().map_err(AccountdError::Io)?;
    if let Some(abstract_name) = socket.strip_prefix('@') {
        #[cfg(not(target_os = "linux"))]
        let _ = abstract_name;
        #[cfg(not(target_os = "linux"))]
        return Err(AccountdError::SocketConfig(
            "abstract sd_notify sockets require Linux",
        ));
        #[cfg(target_os = "linux")]
        {
            let address =
                std::os::unix::net::SocketAddr::from_abstract_name(abstract_name.as_bytes())
                    .map_err(AccountdError::Io)?;
            datagram
                .send_to_addr(message.as_bytes(), &address)
                .map_err(AccountdError::Io)?;
        }
    } else {
        datagram
            .send_to(message.as_bytes(), socket)
            .map_err(AccountdError::Io)?;
    }
    Ok(true)
}

#[cfg(not(unix))]
pub fn sd_notify(_message: &str) -> Result<bool, AccountdError> {
    Ok(false)
}

/// Resolve `$CODEX_HOME/marathon/quota.sqlite` using the shared CodexMarathon
/// home contract. An explicit path (for example, `--codex-home`) remains
/// authoritative and does not consult ambient configuration.
pub fn default_quota_db_path(
    explicit_codex_home: Option<PathBuf>,
) -> Result<PathBuf, AccountdError> {
    let resolved = match explicit_codex_home {
        Some(path) => codexmarathon_home::resolve_explicit(path),
        None => codexmarathon_home::resolve(),
    }
    .map_err(AccountdError::Home)?;
    codexmarathon_runtime::MarathonConfig::new(resolved.codex_home().to_path_buf())
        .map(|config| config.quota_db_path().to_path_buf())
        .map_err(AccountdError::Quota)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::collections::BTreeMap;
    use tempfile::tempdir;

    fn block_on<F: Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .map(|runtime| runtime.block_on(future))
            .unwrap_or_else(|error| panic!("test runtime: {error}"))
    }

    fn request(method: &str, params: Value) -> Request {
        Request {
            version: PROTOCOL_VERSION,
            id: Some(Value::from(7)),
            method: method.to_string(),
            params,
        }
    }

    #[cfg(unix)]
    fn make_private(path: &Path) {
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))
            .expect("private temporary directory");
    }

    fn snapshot(account_id: &str) -> AccountTelemetry {
        AccountTelemetry {
            account_id: account_id.to_string(),
            limits: BTreeMap::new(),
            observed_at: Utc::now(),
            usable: true,
            reset_capability: Default::default(),
            source: "test".to_string(),
        }
    }

    #[test]
    fn protocol_health_is_versioned_and_typed() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let daemon = AccountDaemon::open(
                directory.path().join("quota.sqlite"),
                DEFAULT_REQUEST_TIMEOUT,
            )
            .await
            .expect("daemon");
            let response = daemon.handle_request(request("health", Value::Null)).await;
            assert!(response.ok);
            let result = response.result.expect("health result");
            assert_eq!(result["protocol_version"], PROTOCOL_VERSION);
            assert_eq!(result["status"], "ok");
        });
    }

    #[test]
    fn accounts_reads_only_typed_quota_metadata() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let path = directory.path().join("quota.sqlite");
            let store = Arc::new(QuotaSnapshotStore::open(&path).await.expect("quota store"));
            store
                .upsert(&snapshot("account-a"))
                .await
                .expect("snapshot");
            let events = EventStore::open(&path).await.expect("event store");
            let daemon = AccountDaemon::from_stores(store, events, DEFAULT_REQUEST_TIMEOUT)
                .await
                .expect("daemon");
            let response = daemon
                .handle_request(request("accounts", Value::Null))
                .await;
            assert!(response.ok);
            assert_eq!(response.result.expect("accounts")["count"], 1);
        });
    }

    #[test]
    fn request_bounds_and_protocol_errors_are_stable() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            let daemon = AccountDaemon::open(
                directory.path().join("quota.sqlite"),
                DEFAULT_REQUEST_TIMEOUT,
            )
            .await
            .expect("daemon");
            let mut invalid = request("health", Value::Null);
            invalid.version = PROTOCOL_VERSION + 1;
            let response = daemon.handle_request(invalid).await;
            assert!(!response.ok);
            assert_eq!(response.error.expect("error").code, "unsupported_version");
            let response = daemon
                .handle_request(request("events_since", json!({"after": 0, "limit": 0})))
                .await;
            assert!(!response.ok);
            assert_eq!(response.error.expect("error").code, "invalid_request");
        });
    }

    #[test]
    fn event_ids_are_monotonic_and_replay_is_cursor_based() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            make_private(directory.path());
            let store = EventStore::open(directory.path().join("quota.sqlite"))
                .await
                .expect("event store");
            let first = store
                .append_event(EventInput::now(EventKind::ExecutorReady))
                .await
                .expect("first event");
            let second = store
                .append_event(EventInput::now(EventKind::AutospinBlocked))
                .await
                .expect("second event");
            assert!(second.event_id > first.event_id);
            let page = store.events_since(first.event_id, 100).await.expect("page");
            assert_eq!(page.events.len(), 1);
            assert_eq!(page.events[0].event_id, second.event_id);
            assert_eq!(page.next_after, second.event_id);
        });
    }

    #[test]
    fn startup_reconciliation_retries_idempotent_jobs_and_fails_unsafe_jobs() {
        block_on(async {
            let directory = tempdir().expect("temporary directory");
            make_private(directory.path());
            let path = directory.path().join("quota.sqlite");
            let store = EventStore::open(&path).await.expect("event store");
            let retry = store
                .queue_job(JobSpec {
                    job_id: "job-retry".to_string(),
                    kind: "quota_refresh".to_string(),
                    idempotency_key: "idem-retry".to_string(),
                    account_id: Some("account-a".to_string()),
                    run_after: Utc::now(),
                })
                .await
                .expect("queue retry");
            assert!(
                store
                    .mark_job_running(&retry.job_id)
                    .await
                    .expect("running")
            );
            let pool = store.pool().await.expect("pool");
            sqlx::query(
                "INSERT INTO accountd_jobs (job_id, kind, idempotency_key, state, generation, attempt, run_after_ms, created_at_ms, updated_at_ms) VALUES ('job-unsafe', 'quota_refresh', '', 'running', 0, 0, 0, 0, 0)",
            )
            .execute(pool)
            .await
            .expect("unsafe row");
            assert_eq!(
                store
                    .reconcile_running_jobs_at(Utc::now())
                    .await
                    .expect("reconcile"),
                2
            );
            let counts = store.job_counts().await.expect("counts");
            assert_eq!(counts.retry_wait, 1);
            assert_eq!(counts.failed, 1);
            let page = store
                .events_since(0, MAX_EVENT_LIMIT)
                .await
                .expect("events");
            assert!(
                page.events
                    .iter()
                    .any(|event| event.kind == EventKind::JobRetryWait)
            );
            assert!(
                page.events
                    .iter()
                    .any(|event| event.kind == EventKind::JobFailed)
            );
        });
    }

    #[test]
    fn oversized_line_is_rejected_before_unbounded_growth() {
        block_on(async {
            let input = vec![b'x'; MAX_REQUEST_BYTES + 1];
            let mut reader = BufReader::new(input.as_slice());
            assert!(matches!(
                read_line_bounded(&mut reader).await,
                Err(AccountdError::RequestTooLarge)
            ));
        });
    }

    #[test]
    fn explicit_codex_home_remains_authoritative_for_default_quota_path() {
        let directory = tempdir().expect("temporary directory");
        let explicit_home = directory.path().join("explicit-codex-home");
        let quota_path = default_quota_db_path(Some(explicit_home.clone())).expect("quota path");
        assert_eq!(
            quota_path,
            explicit_home.join("marathon").join("quota.sqlite")
        );
    }
}
