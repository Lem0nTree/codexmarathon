//! Typed wire protocol and bounded Unix-socket client for `codexmarathon-accountd`.
//!
//! This crate deliberately has no dependency on the daemon or Marathon's
//! storage/runtime crates.  The daemon maps its internal models into these
//! provider-neutral wire DTOs, while callers use the typed request methods.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;
use tokio::io::{AsyncBufRead, AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;

/// Version of the newline-delimited accountd protocol.
pub const PROTOCOL_VERSION: u32 = 1;

/// Maximum request-line payload, excluding its optional newline delimiter.
pub const MAX_REQUEST_BYTES: usize = 64 * 1024;

/// Maximum response-line payload, excluding its optional newline delimiter.
pub const MAX_RESPONSE_BYTES: usize = 4 * 1024 * 1024;

/// Default deadline for one client connection, write, and response read.
pub const DEFAULT_REQUEST_TIMEOUT: Duration = Duration::from_secs(2);

/// Default number of events returned by one replay request.
pub const DEFAULT_EVENT_LIMIT: u32 = 100;

/// Maximum number of events returned by one replay request.
pub const MAX_EVENT_LIMIT: u32 = 1_000;

const DEFAULT_SOCKET_DIRECTORY: &str = "codexmarathon-accountd";
const DEFAULT_SOCKET_FILENAME: &str = "accountd.sock";

/// JSON scalar used to correlate a request with its response.
pub type RequestId = Value;

/// Return whether an optional protocol id is a permitted JSON scalar.
///
/// Arrays and objects are rejected because they make correlation ambiguous and
/// are not needed by this private protocol. `null` remains valid for wire
/// compatibility with the existing daemon envelope.
pub fn is_valid_request_id(id: Option<&Value>) -> bool {
    match id {
        None | Some(Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_)) => true,
        Some(Value::Array(_) | Value::Object(_)) => false,
    }
}

/// Versioned newline-delimited request envelope.
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Request {
    #[serde(rename = "version", alias = "protocol_version")]
    pub version: u32,
    #[serde(default)]
    pub id: Option<RequestId>,
    pub method: String,
    #[serde(default)]
    pub params: Value,
}

impl Request {
    /// Construct a request using the current protocol version.
    pub fn new(id: Option<RequestId>, method: impl Into<String>, params: Value) -> Self {
        Self {
            version: PROTOCOL_VERSION,
            id,
            method: method.into(),
            params,
        }
    }
}

/// Error object in a protocol response.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct ProtocolError {
    pub code: String,
    pub message: String,
}

/// Versioned newline-delimited response envelope.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct Response {
    pub version: u32,
    pub id: Option<RequestId>,
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<ProtocolError>,
}

impl Response {
    /// Construct a successful response.
    pub fn success(id: Option<RequestId>, result: Value) -> Self {
        Self {
            version: PROTOCOL_VERSION,
            id,
            ok: true,
            result: Some(result),
            error: None,
        }
    }

    /// Construct a protocol error response without depending on daemon error
    /// types or leaking internal diagnostics.
    pub fn error(
        id: Option<RequestId>,
        code: impl Into<String>,
        message: impl Into<String>,
    ) -> Self {
        Self {
            version: PROTOCOL_VERSION,
            id,
            ok: false,
            result: None,
            error: Some(ProtocolError {
                code: code.into(),
                message: message.into(),
            }),
        }
    }
}

/// Secret-free credential state exposed by accountd.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum CredentialHealth {
    #[default]
    Unknown,
    Healthy,
    Stale,
    Invalid,
}

/// Conventional quota window kind.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum WindowKind {
    Primary,
    Secondary,
}

/// Freshness marker for a quota observation.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Freshness {
    #[default]
    Fresh,
    Stale,
}

/// One primary or secondary quota window.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct UsageWindow {
    pub kind: WindowKind,
    pub used_percent: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub window_duration_mins: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resets_at: Option<DateTime<Utc>>,
    pub observed_at: DateTime<Utc>,
    #[serde(default)]
    pub freshness: Freshness,
}

/// One provider metered bucket.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct LimitTelemetry {
    pub limit_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub limit_name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub plan_type: String,
    pub windows: Vec<UsageWindow>,
}

/// Provider capability for an immediate quota reset action.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum QuotaResetCapability {
    #[default]
    Unavailable,
    Available {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        credit_id: Option<String>,
    },
}

/// Complete provider-neutral quota telemetry for one account.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct AccountTelemetry {
    pub account_id: String,
    #[serde(default)]
    pub limits: BTreeMap<String, LimitTelemetry>,
    pub observed_at: DateTime<Utc>,
    #[serde(default)]
    pub usable: bool,
    #[serde(default)]
    pub reset_capability: QuotaResetCapability,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
}

impl AccountTelemetry {
    /// Return all quota windows in deterministic bucket order.
    pub fn windows(&self) -> Vec<&UsageWindow> {
        self.limits
            .values()
            .flat_map(|limit| limit.windows.iter())
            .collect()
    }
}

/// Account metadata and optional typed quota telemetry.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct DaemonAccount {
    pub account_id: String,
    pub alias: String,
    pub active: bool,
    pub credential_health: CredentialHealth,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub quota: Option<AccountTelemetry>,
}

/// Result of the `accounts` method.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct AccountsResult {
    pub accounts: Vec<DaemonAccount>,
    pub count: usize,
}

/// Result of the `health` method.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct HealthResult {
    pub service: String,
    pub protocol_version: u32,
    pub status: String,
}

/// Scheduler counts included by the `status` method.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct JobCounts {
    pub queued: i64,
    pub running: i64,
    pub retry_wait: i64,
    pub succeeded: i64,
    pub failed: i64,
    pub cancelled: i64,
}

/// Result of the `status` method.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct StatusResult {
    pub service: String,
    pub protocol_version: u32,
    pub ready: bool,
    pub quota_database: String,
    pub account_count: usize,
    pub latest_event_id: i64,
    pub scheduler_ready: bool,
    pub jobs: JobCounts,
}

/// Metadata-only event kind returned by `events_since`.
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

/// Durable scheduler state included in event metadata.
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

/// One metadata-only event returned by `events_since`.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
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

/// A page from the durable event stream.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct EventPage {
    pub after: i64,
    pub events: Vec<EventRecord>,
    pub next_after: i64,
    pub has_more: bool,
}

#[derive(Clone, Debug, Serialize)]
struct EventsSinceParams {
    after: i64,
    limit: u32,
}

/// Errors returned by the typed accountd client.
#[derive(Debug, thiserror::Error)]
pub enum AccountdClientError {
    #[error("accountd socket path is unavailable")]
    SocketPathUnavailable,

    #[error("accountd request is invalid: {0}")]
    InvalidRequest(&'static str),

    #[error("accountd request id space is exhausted")]
    RequestIdExhausted,

    #[error("accountd request is too large")]
    RequestTooLarge,

    #[error("accountd response is too large")]
    ResponseTooLarge,

    #[error("accountd request timed out")]
    Timeout,

    #[error("accountd response protocol version {actual} is unsupported (expected {expected})")]
    UnsupportedVersion { expected: u32, actual: u32 },

    #[error("accountd response id did not match the request")]
    ResponseIdMismatch,

    #[error("accountd response id is invalid")]
    InvalidResponseId,

    #[error("accountd response envelope is invalid")]
    InvalidResponse,

    #[error("accountd request failed ({code}): {message}")]
    Remote { code: String, message: String },

    #[error("accountd I/O failed: {0}")]
    Io(#[source] std::io::Error),

    #[error("accountd JSON failed: {0}")]
    Json(#[source] serde_json::Error),

    #[error("accountd response payload failed to decode: {0}")]
    Decode(#[source] serde_json::Error),
}

/// Resolve the default per-user accountd socket from `XDG_RUNTIME_DIR`.
pub fn default_socket_path() -> Option<PathBuf> {
    std::env::var_os("XDG_RUNTIME_DIR").map(|directory| {
        PathBuf::from(directory)
            .join(DEFAULT_SOCKET_DIRECTORY)
            .join(DEFAULT_SOCKET_FILENAME)
    })
}

/// A bounded typed client. Each method opens one short-lived Unix connection.
#[derive(Debug)]
pub struct AccountdClient {
    socket_path: PathBuf,
    timeout: Duration,
    next_id: AtomicU64,
}

impl AccountdClient {
    /// Build a client for an explicit owner-only Unix socket path.
    pub fn new(socket_path: impl Into<PathBuf>) -> Self {
        Self {
            socket_path: socket_path.into(),
            timeout: DEFAULT_REQUEST_TIMEOUT,
            next_id: AtomicU64::new(1),
        }
    }

    /// Build a client using the standard `XDG_RUNTIME_DIR` socket location.
    pub fn from_default_socket() -> Result<Self, AccountdClientError> {
        default_socket_path()
            .map(Self::new)
            .ok_or(AccountdClientError::SocketPathUnavailable)
    }

    /// Set the connection/request deadline.
    #[must_use]
    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    /// Return the configured socket path.
    pub fn socket_path(&self) -> &Path {
        &self.socket_path
    }

    /// Return the configured request deadline.
    pub const fn timeout(&self) -> Duration {
        self.timeout
    }

    /// Fetch daemon health through the typed protocol.
    pub async fn health(&self) -> Result<HealthResult, AccountdClientError> {
        self.call("health", Value::Null).await
    }

    /// Fetch daemon status through the typed protocol.
    pub async fn status(&self) -> Result<StatusResult, AccountdClientError> {
        self.call("status", Value::Null).await
    }

    /// Fetch account metadata and typed quota snapshots.
    pub async fn accounts(&self) -> Result<AccountsResult, AccountdClientError> {
        self.call("accounts", Value::Null).await
    }

    /// Replay metadata events strictly after `after`.
    pub async fn events_since(
        &self,
        after: i64,
        limit: u32,
    ) -> Result<EventPage, AccountdClientError> {
        if after < 0 {
            return Err(AccountdClientError::InvalidRequest(
                "event cursor must not be negative",
            ));
        }
        if !(1..=MAX_EVENT_LIMIT).contains(&limit) {
            return Err(AccountdClientError::InvalidRequest(
                "event limit is outside the supported range",
            ));
        }
        self.call(
            "events_since",
            serde_json::to_value(EventsSinceParams { after, limit })
                .map_err(AccountdClientError::Json)?,
        )
        .await
    }

    /// Allocate a valid scalar request id. IDs are monotonic per client.
    pub fn next_request_id(&self) -> Result<RequestId, AccountdClientError> {
        let id = self
            .next_id
            .fetch_update(Ordering::Relaxed, Ordering::Relaxed, |value| {
                value.checked_add(1)
            })
            .map_err(|_| AccountdClientError::RequestIdExhausted)?;
        Ok(Value::from(id))
    }

    async fn call<T>(&self, method: &str, params: Value) -> Result<T, AccountdClientError>
    where
        T: for<'de> Deserialize<'de>,
    {
        let id = self.next_request_id()?;
        let request = Request::new(Some(id.clone()), method, params);
        let mut encoded = serde_json::to_vec(&request).map_err(AccountdClientError::Json)?;
        if encoded.len() > MAX_REQUEST_BYTES {
            return Err(AccountdClientError::RequestTooLarge);
        }
        encoded.push(b'\n');
        let response = tokio::time::timeout(self.timeout, async {
            let stream = UnixStream::connect(&self.socket_path)
                .await
                .map_err(AccountdClientError::Io)?;
            let (reader, mut writer) = stream.into_split();
            writer
                .write_all(&encoded)
                .await
                .map_err(AccountdClientError::Io)?;
            let mut reader = BufReader::new(reader);
            let line = read_line_bounded(&mut reader, MAX_RESPONSE_BYTES).await?;
            let Some(line) = line else {
                return Err(AccountdClientError::InvalidResponse);
            };
            let response =
                serde_json::from_slice::<Response>(&line).map_err(AccountdClientError::Json)?;
            validate_response(&response, &id)?;
            Ok::<_, AccountdClientError>(response)
        })
        .await
        .map_err(|_| AccountdClientError::Timeout)??;

        let result = response
            .result
            .ok_or(AccountdClientError::InvalidResponse)?;
        serde_json::from_value(result).map_err(AccountdClientError::Decode)
    }
}

fn validate_response(response: &Response, request_id: &Value) -> Result<(), AccountdClientError> {
    if response.version != PROTOCOL_VERSION {
        return Err(AccountdClientError::UnsupportedVersion {
            expected: PROTOCOL_VERSION,
            actual: response.version,
        });
    }
    if !is_valid_request_id(response.id.as_ref()) {
        return Err(AccountdClientError::InvalidResponseId);
    }
    if response.id.as_ref() != Some(request_id) {
        return Err(AccountdClientError::ResponseIdMismatch);
    }
    if response.ok {
        if response.result.is_none() || response.error.is_some() {
            return Err(AccountdClientError::InvalidResponse);
        }
    } else {
        let Some(error) = response.error.as_ref() else {
            return Err(AccountdClientError::InvalidResponse);
        };
        return Err(AccountdClientError::Remote {
            code: error.code.clone(),
            message: error.message.clone(),
        });
    }
    Ok(())
}

async fn read_line_bounded<R>(
    reader: &mut R,
    max_bytes: usize,
) -> Result<Option<Vec<u8>>, AccountdClientError>
where
    R: AsyncBufRead + Unpin,
{
    let mut line = Vec::with_capacity(1024.min(max_bytes));
    loop {
        let available = reader.fill_buf().await.map_err(AccountdClientError::Io)?;
        if available.is_empty() {
            if line.is_empty() {
                return Ok(None);
            }
            return Ok(Some(line));
        }
        let newline = available.iter().position(|byte| *byte == b'\n');
        let take = newline.map_or(available.len(), |index| index + 1);
        let content_take = newline.unwrap_or(take);
        if line.len().saturating_add(content_take) > max_bytes {
            return Err(AccountdClientError::ResponseTooLarge);
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

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::os::unix::fs::PermissionsExt;
    use tempfile::tempdir;
    use tokio::net::UnixListener;

    fn block_on<F: std::future::Future>(future: F) -> F::Output {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .expect("runtime")
            .block_on(future)
    }

    #[test]
    fn default_socket_uses_xdg_runtime_dir() {
        let directory = tempdir().expect("directory");
        // SAFETY: this test is single-threaded and restores no shared process
        // state used by the client implementation itself.
        unsafe { std::env::set_var("XDG_RUNTIME_DIR", directory.path()) };
        assert_eq!(
            default_socket_path(),
            Some(
                directory
                    .path()
                    .join("codexmarathon-accountd")
                    .join("accountd.sock")
            )
        );
    }

    #[test]
    fn invalid_request_ids_are_rejected() {
        assert!(!is_valid_request_id(Some(&json!([]))));
        assert!(!is_valid_request_id(Some(&json!({"nested": true}))));
        assert!(is_valid_request_id(Some(&json!(7))));
        assert!(is_valid_request_id(None));
    }

    #[test]
    fn response_version_and_id_are_validated() {
        let request_id = json!(7);
        let mut response = Response::success(Some(request_id.clone()), json!({}));
        response.version = PROTOCOL_VERSION + 1;
        assert!(matches!(
            validate_response(&response, &request_id),
            Err(AccountdClientError::UnsupportedVersion { .. })
        ));
        response.version = PROTOCOL_VERSION;
        response.id = Some(json!({"bad": true}));
        assert!(matches!(
            validate_response(&response, &request_id),
            Err(AccountdClientError::InvalidResponseId)
        ));
        response.id = Some(json!(8));
        assert!(matches!(
            validate_response(&response, &request_id),
            Err(AccountdClientError::ResponseIdMismatch)
        ));
    }

    #[test]
    fn oversized_response_is_rejected_before_decode() {
        block_on(async {
            let input = vec![b'x'; MAX_RESPONSE_BYTES + 1];
            let mut reader = BufReader::new(input.as_slice());
            assert!(matches!(
                read_line_bounded(&mut reader, MAX_RESPONSE_BYTES).await,
                Err(AccountdClientError::ResponseTooLarge)
            ));
        });
    }

    #[test]
    fn timeout_is_bounded() {
        block_on(async {
            let directory = tempdir().expect("directory");
            std::fs::set_permissions(directory.path(), std::fs::Permissions::from_mode(0o700))
                .expect("permissions");
            let path = directory.path().join("accountd.sock");
            let listener = UnixListener::bind(&path).expect("listener");
            let task = tokio::spawn(async move {
                let (_stream, _) = listener.accept().await.expect("accept");
                tokio::time::sleep(Duration::from_millis(100)).await;
            });
            let client = AccountdClient::new(&path).with_timeout(Duration::from_millis(10));
            assert!(matches!(
                client.health().await,
                Err(AccountdClientError::Timeout)
            ));
            task.await.expect("server task");
        });
    }
}
