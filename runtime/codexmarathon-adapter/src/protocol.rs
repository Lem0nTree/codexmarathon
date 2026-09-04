//! Wire types for CodexMarathon protocol v1.
//!
//! The types in this module intentionally model only the small, stable
//! controller/runtime contract.  They do not mirror Codext's internal
//! request or authentication types.  Account-login snapshots are carried only
//! as opaque JSON values and are never interpreted by this adapter.

use serde::de::DeserializeOwned;
use serde::ser::Error as SerdeError;
use serde::{Deserialize, Deserializer, Serialize, Serializer};
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use std::fmt;

pub const PROTOCOL_VERSION: u32 = 1;
pub const JSONRPC_VERSION: &str = "2.0";

pub const METHOD_NEGOTIATE: &str = "protocol/negotiate";
pub const METHOD_GET_RUNTIME_STATE: &str = "runtime/state/read";
pub const METHOD_PREPARE_AUTH_TRANSITION: &str = "auth/transition/prepare";
pub const METHOD_COMMIT_AUTH_TRANSITION: &str = "auth/transition/commit";
pub const METHOD_CANCEL_AUTH_TRANSITION: &str = "auth/transition/cancel";
pub const METHOD_GET_IDENTITY: &str = "runtime/identity/read";
pub const METHOD_GET_AUTH_GENERATION: &str = "runtime/authGeneration/read";
/// Start one login flow using Codex's native login implementation.
///
/// The response contains an opaque auth snapshot for the controller's
/// protected vault.  It is intentionally not modelled as token fields here;
/// provider-specific parsing remains in Codex's login crate.
pub const METHOD_LOGIN_ACCOUNT: &str = "account/login";
/// Refresh one account snapshot using Codex's native AuthManager path.
pub const METHOD_REFRESH_ACCOUNT: &str = "account/refresh";
/// Read the active opaque auth snapshot so the controller can synchronize
/// refreshed Account A credentials before deploying Account B.
pub const METHOD_READ_AUTH_SNAPSHOT: &str = "account/authSnapshot/read";
/// Authorize Codext's already-parked UsageLimitExceeded recovery turn. The
/// runtime owns the prompt and thread queue; this method only releases the
/// native continuation after a verified account transition.
pub const METHOD_RELEASE_RECOVERY: &str = "recovery/release";
pub const EVENT_NOTIFICATION_METHOD: &str = "codexmarathon/event";

pub const EVENT_RUNTIME_READY: &str = "runtime_ready";
pub const EVENT_RATE_LIMITS_SNAPSHOT: &str = "rate_limits_snapshot";
pub const EVENT_RATE_LIMITS_UPDATED: &str = "rate_limits_updated";
pub const EVENT_TURN_STARTED: &str = "turn_started";
pub const EVENT_TURN_COMPLETED: &str = "turn_completed";
pub const EVENT_SAFE_BOUNDARY_REACHED: &str = "safe_boundary_reached";
pub const EVENT_AUTH_RELOAD_STARTED: &str = "auth_reload_started";
pub const EVENT_AUTH_RELOAD_SUCCEEDED: &str = "auth_reload_succeeded";
pub const EVENT_AUTH_RELOAD_FAILED: &str = "auth_reload_failed";
pub const EVENT_IDENTITY_CHANGED: &str = "identity_changed";
pub const EVENT_RECOVERY_PARKED: &str = "recovery_parked";
pub const EVENT_RECOVERY_STARTED: &str = "recovery_started";
pub const EVENT_RECOVERY_COMPLETED: &str = "recovery_completed";

/// The JSON-RPC request identifier accepted by the v1 contract.
#[derive(Clone, Debug, Eq, Hash, PartialEq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum RequestId {
    String(String),
    Integer(i64),
}

impl RequestId {
    pub fn as_string(&self) -> String {
        match self {
            Self::String(value) => value.clone(),
            Self::Integer(value) => value.to_string(),
        }
    }
}

impl From<&str> for RequestId {
    fn from(value: &str) -> Self {
        Self::String(value.to_string())
    }
}

impl From<String> for RequestId {
    fn from(value: String) -> Self {
        Self::String(value)
    }
}

impl From<i64> for RequestId {
    fn from(value: i64) -> Self {
        Self::Integer(value)
    }
}

/// A decoded JSON-RPC request.  Parameters default to an empty object so
/// callers can construct requests for the read methods without carrying a
/// second unit type.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RpcRequest {
    pub jsonrpc: String,
    pub id: RequestId,
    pub method: String,
    #[serde(default = "empty_object")]
    pub params: Value,
}

impl RpcRequest {
    pub fn new(id: impl Into<RequestId>, method: impl Into<String>, params: Value) -> Self {
        Self {
            jsonrpc: JSONRPC_VERSION.to_string(),
            id: id.into(),
            method: method.into(),
            params,
        }
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.jsonrpc != JSONRPC_VERSION {
            return Err("jsonrpc must be 2.0".to_string());
        }
        if self.method.trim().is_empty() {
            return Err("method is required".to_string());
        }
        if !self.params.is_object() {
            return Err("params must be an object".to_string());
        }
        Ok(())
    }
}

/// A JSON-RPC error object.  The adapter never places credentials in `data`.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RpcError {
    pub code: i32,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<Value>,
}

impl RpcError {
    pub fn new(code: i32, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            data: None,
        }
    }
}

/// A JSON-RPC response.  Exactly one of `result` and `error` is populated by
/// the constructors below.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RpcResponse {
    pub jsonrpc: String,
    pub id: RequestId,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub result: Option<Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<RpcError>,
}

impl RpcResponse {
    pub fn success<T: Serialize>(id: RequestId, result: &T) -> Result<Self, serde_json::Error> {
        Ok(Self {
            jsonrpc: JSONRPC_VERSION.to_string(),
            id,
            result: Some(serde_json::to_value(result)?),
            error: None,
        })
    }

    pub fn error(id: RequestId, error: RpcError) -> Self {
        Self {
            jsonrpc: JSONRPC_VERSION.to_string(),
            id,
            result: None,
            error: Some(error),
        }
    }

    pub fn is_error(&self) -> bool {
        self.error.is_some()
    }
}

/// A server notification.  Runtime events use a flat `params` object as
/// required by protocol/events.json.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RpcNotification {
    pub jsonrpc: String,
    pub method: String,
    pub params: Value,
}

impl RpcNotification {
    pub fn event(event: &RuntimeEvent) -> Result<Self, serde_json::Error> {
        Ok(Self {
            jsonrpc: JSONRPC_VERSION.to_string(),
            method: EVENT_NOTIFICATION_METHOD.to_string(),
            params: serde_json::to_value(event)?,
        })
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VersionNegotiationParams {
    pub supported_versions: Vec<u32>,
}

impl VersionNegotiationParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.supported_versions.is_empty() {
            return Err("supported_versions must not be empty".to_string());
        }
        for (index, version) in self.supported_versions.iter().enumerate() {
            if *version == 0 {
                return Err("supported_versions must be positive".to_string());
            }
            if self.supported_versions[..index].contains(version) {
                return Err(format!("supported version {version} is duplicated"));
            }
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VersionNegotiationResult {
    pub protocol_version: u32,
    pub server_versions: Vec<u32>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RuntimeIdentity {
    pub runtime_id: String,
    pub account_id: Option<String>,
    pub auth_generation: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PendingTransition {
    pub transition_id: String,
    pub target_account_id: String,
    pub expected_generation: u64,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RuntimeState {
    pub identity: RuntimeIdentity,
    pub active_turn_count: u32,
    pub pending_transition: Option<PendingTransition>,
    /// Runtime-owned parked/released recovery metadata. Older peers may omit
    /// this field; an empty list remains a valid state snapshot.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub recoveries: Vec<RecoveryState>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthTransitionParams {
    pub transition_id: String,
    pub target_account_id: String,
    pub expected_generation: u64,
}

impl AuthTransitionParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.transition_id.trim().is_empty() {
            return Err("transition_id is required".to_string());
        }
        if self.target_account_id.trim().is_empty() {
            return Err("target_account_id is required".to_string());
        }
        if self.expected_generation == 0 {
            return Err("expected_generation must be greater than zero".to_string());
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CancelAuthTransitionParams {
    pub transition_id: String,
    pub expected_generation: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

impl CancelAuthTransitionParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.transition_id.trim().is_empty() {
            return Err("transition_id is required".to_string());
        }
        if self.expected_generation == 0 {
            return Err("expected_generation must be greater than zero".to_string());
        }
        if self
            .reason
            .as_deref()
            .map(|value| value.trim().is_empty())
            .unwrap_or(false)
        {
            return Err("reason must not be empty when supplied".to_string());
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum TransitionOutcome {
    Committed,
    Rejected,
    Uncertain,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct TransitionResult {
    pub transition_id: String,
    pub runtime_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub expected_generation: Option<u64>,
    pub auth_generation: u64,
    pub outcome: TransitionOutcome,
    pub account_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_message: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthGenerationResult {
    pub auth_generation: u64,
}

/// Non-secret intent for a native account login.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NativeLoginParams {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub account_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub overwrite: bool,
}

impl NativeLoginParams {
    pub fn validate(&self) -> Result<(), String> {
        if self
            .account_id
            .as_deref()
            .is_some_and(|value| value.trim().is_empty())
        {
            return Err("account_id must not be empty when supplied".to_string());
        }
        if self
            .alias
            .as_deref()
            .is_some_and(|value| value.trim().is_empty())
        {
            return Err("alias must not be empty when supplied".to_string());
        }
        Ok(())
    }
}

/// In-memory handoff of a native login result.
///
/// `auth_json` is opaque by design.  The adapter never logs, journals, or
/// interprets this value; the controller must put it directly into its
/// protected credential vault.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NativeLoginResult {
    pub account_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub alias: Option<String>,
    pub auth_json: Value,
    #[serde(default, skip_serializing_if = "BTreeMap::is_empty")]
    pub metadata: BTreeMap<String, String>,
}

impl NativeLoginResult {
    pub fn validate(&self) -> Result<(), String> {
        if self.account_id.trim().is_empty() {
            return Err("native login returned an empty account_id".to_string());
        }
        if !self.auth_json.is_object() {
            return Err("native login returned a non-object auth_json".to_string());
        }
        Ok(())
    }
}

/// Opaque active-account snapshot read from Codex's native AuthManager.
///
/// This result is an in-memory handoff only. The adapter does not log,
/// journal, or inspect the `auth_json` value.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NativeAuthSnapshotResult {
    pub account_id: String,
    pub auth_json: Value,
}

impl NativeAuthSnapshotResult {
    pub fn validate(&self) -> Result<(), String> {
        if self.account_id.trim().is_empty() {
            return Err("native auth snapshot returned an empty account_id".to_string());
        }
        if !self.auth_json.is_object() {
            return Err("native auth snapshot returned a non-object auth_json".to_string());
        }
        Ok(())
    }
}

/// Opaque account snapshot supplied to the native refresh path.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NativeRefreshParams {
    pub account_id: String,
    pub auth_json: Value,
}

impl NativeRefreshParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.account_id.trim().is_empty() {
            return Err("account_id is required".to_string());
        }
        if !self.auth_json.is_object() {
            return Err("auth_json must be an object".to_string());
        }
        Ok(())
    }
}

/// Native refresh output.  It remains provider-neutral and contains only the
/// fields the controller needs to write back into the opaque snapshot.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NativeRefreshResult {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub access_token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub id_token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub account_id: Option<String>,
}

/// Lifecycle phase for Codext's native UsageLimitExceeded recovery queue.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecoveryPhase {
    Parked,
    Released,
    Started,
    Completed,
}

/// Secret-free recovery metadata exposed for restart reconciliation. Prompt
/// text and credential material are intentionally not representable here.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoveryState {
    pub recovery_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_account_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub transition_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub target_account_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub expected_generation: Option<u64>,
    pub phase: RecoveryPhase,
}

impl RecoveryState {
    pub fn parked(recovery_id: impl Into<String>) -> Self {
        Self {
            recovery_id: recovery_id.into(),
            thread_id: None,
            turn_id: None,
            source_account_id: None,
            transition_id: None,
            target_account_id: None,
            expected_generation: None,
            phase: RecoveryPhase::Parked,
        }
    }
}

/// Controller authorization for one native parked recovery. The transition
/// correlation makes stale releases harmless and lets a restarted controller
/// retry the same release without inventing a second prompt.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoveryReleaseParams {
    pub recovery_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    pub transition_id: String,
    pub expected_generation: u64,
}

impl RecoveryReleaseParams {
    pub fn validate(&self) -> Result<(), String> {
        if self.recovery_id.trim().is_empty() {
            return Err("recovery_id is required".to_string());
        }
        if self.transition_id.trim().is_empty() {
            return Err("transition_id is required".to_string());
        }
        if self.expected_generation == 0 {
            return Err("expected_generation must be greater than zero".to_string());
        }
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecoveryReleaseOutcome {
    Released,
    AlreadyReleased,
    Rejected,
    Uncertain,
}

/// Secret-free result for one recovery release request.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoveryReleaseResult {
    pub recovery_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    pub outcome: RecoveryReleaseOutcome,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error_code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error_message: Option<String>,
}

impl NativeRefreshResult {
    pub fn validate_for(&self, requested_account_id: &str) -> Result<(), String> {
        if requested_account_id.trim().is_empty() {
            return Err("account_id is required".to_string());
        }
        if self.access_token.is_none()
            && self.id_token.is_none()
            && self.refresh_token.is_none()
        {
            return Err("native refresh returned no token fields".to_string());
        }
        if self
            .account_id
            .as_deref()
            .is_some_and(|account_id| account_id != requested_account_id)
        {
            return Err("native refresh returned a different account_id".to_string());
        }
        Ok(())
    }
}

/// The intentionally narrow rate-limit window consumed by the controller.
/// The camelCase names are inherited from Codext's app-server protocol.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RateLimitWindow {
    #[serde(rename = "usedPercent")]
    pub used_percent: f64,
    #[serde(rename = "windowDurationMins")]
    pub window_duration_mins: Option<u64>,
    #[serde(rename = "resetsAt")]
    pub resets_at: Option<u64>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RateLimitSnapshot {
    #[serde(rename = "limitId")]
    pub limit_id: Option<String>,
    #[serde(rename = "limitName")]
    pub limit_name: Option<String>,
    #[serde(rename = "planType")]
    pub plan_type: Option<String>,
    #[serde(rename = "rateLimitReachedType")]
    pub rate_limit_reached_type: Option<String>,
    pub primary: Option<RateLimitWindow>,
    pub secondary: Option<RateLimitWindow>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RateLimitsSnapshotPayload {
    #[serde(rename = "account_id")]
    pub account_id: Option<String>,
    #[serde(rename = "rateLimits")]
    pub rate_limits: RateLimitSnapshot,
    #[serde(rename = "rateLimitsByLimitId")]
    pub rate_limits_by_limit_id: Option<BTreeMap<String, RateLimitSnapshot>>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RateLimitsUpdatedPayload {
    #[serde(rename = "account_id")]
    pub account_id: Option<String>,
    #[serde(rename = "rateLimits")]
    pub rate_limits: RateLimitSnapshot,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct RuntimeReadyPayload {
    pub protocol_version: u32,
    pub account_id: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub capabilities: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct TurnStartedPayload {
    pub turn_id: String,
    pub thread_id: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct TurnCompletedPayload {
    pub turn_id: String,
    pub thread_id: Option<String>,
    pub outcome: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_code: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SafeBoundaryReachedPayload {
    pub reason: String,
    pub turn_id: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct AuthReloadStartedPayload {
    pub target_account_id: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct AuthReloadSucceededPayload {
    pub account_id: Option<String>,
    pub previous_account_id: Option<String>,
    pub changed: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct AuthReloadFailedPayload {
    pub error_code: String,
    pub error_message: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct IdentityChangedPayload {
    pub previous_account_id: Option<String>,
    pub account_id: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecoveryParkedPayload {
    pub recovery_id: String,
    pub reason: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source_account_id: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecoveryStartedPayload {
    pub recovery_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct RecoveryCompletedPayload {
    pub recovery_id: String,
    pub outcome: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thread_id: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub turn_id: Option<String>,
}

/// A lifecycle observation emitted by the native Codext recovery owner.
///
/// This is intentionally not a command and carries no prompt data.  The
/// embedded runtime drains these observations before serving controller
/// requests, which lets the adapter register a parked recovery before a
/// release is attempted.  The enum is kept internal to the Rust composition
/// seam; the wire protocol remains the existing flat event payloads above.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RecoveryLifecycleEvent {
    Parked(RecoveryParkedPayload),
    Started(RecoveryStartedPayload),
    Completed(RecoveryCompletedPayload),
}

impl RecoveryLifecycleEvent {
    pub fn recovery_id(&self) -> &str {
        match self {
            Self::Parked(payload) => &payload.recovery_id,
            Self::Started(payload) => &payload.recovery_id,
            Self::Completed(payload) => &payload.recovery_id,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct EventBase {
    pub event_type: String,
    pub occurred_at: u64,
    pub runtime_id: String,
    pub auth_generation: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub transition_id: Option<String>,
}

impl EventBase {
    pub fn new(
        event_type: impl Into<String>,
        occurred_at: u64,
        runtime_id: impl Into<String>,
        auth_generation: u64,
        transition_id: Option<String>,
    ) -> Self {
        Self {
            event_type: event_type.into(),
            occurred_at,
            runtime_id: runtime_id.into(),
            auth_generation,
            transition_id,
        }
    }

    pub fn validate(&self) -> Result<(), String> {
        if self.event_type.trim().is_empty() {
            return Err("event_type is required".to_string());
        }
        if self.runtime_id.trim().is_empty() {
            return Err("runtime_id is required".to_string());
        }
        let missing_transition_id = self
            .transition_id
            .as_deref()
            .map(|value| value.trim().is_empty())
            .unwrap_or(true);
        if requires_transition_id(&self.event_type) && missing_transition_id {
            return Err(format!("event {} requires transition_id", self.event_type));
        }
        Ok(())
    }
}

/// A flat event object.  Keeping the event-specific fields as a JSON object
/// makes this type forward-compatible with new Codext telemetry fields while
/// the payload helpers provide typed v1 views for known events.
#[derive(Clone, Debug, PartialEq)]
pub struct RuntimeEvent {
    pub base: EventBase,
    pub payload: Value,
}

impl RuntimeEvent {
    pub fn new<T: Serialize>(base: EventBase, payload: &T) -> Result<Self, serde_json::Error> {
        Ok(Self {
            base,
            payload: serde_json::to_value(payload)?,
        })
    }

    pub fn payload_as<T: DeserializeOwned>(&self) -> Result<T, serde_json::Error> {
        serde_json::from_value(self.payload.clone())
    }

    pub fn validate(&self) -> Result<(), String> {
        self.base.validate()?;
        let Some(fields) = self.payload.as_object() else {
            return Err("event payload must be an object".to_string());
        };
        let required: &[&str] = match self.base.event_type.as_str() {
            EVENT_RUNTIME_READY => &["protocol_version", "account_id"],
            EVENT_RATE_LIMITS_SNAPSHOT => &["account_id", "rateLimits", "rateLimitsByLimitId"],
            EVENT_RATE_LIMITS_UPDATED => &["account_id", "rateLimits"],
            EVENT_TURN_STARTED => &["turn_id"],
            EVENT_TURN_COMPLETED => &["turn_id", "outcome"],
            EVENT_SAFE_BOUNDARY_REACHED => &["reason"],
            EVENT_AUTH_RELOAD_STARTED => &["target_account_id"],
            EVENT_AUTH_RELOAD_SUCCEEDED => &["account_id", "changed"],
            EVENT_AUTH_RELOAD_FAILED => &["error_code", "error_message"],
            EVENT_IDENTITY_CHANGED => &["previous_account_id", "account_id"],
            EVENT_RECOVERY_PARKED => &["recovery_id", "reason"],
            EVENT_RECOVERY_STARTED => &["recovery_id"],
            EVENT_RECOVERY_COMPLETED => &["recovery_id", "outcome"],
            _ => &[],
        };
        for field in required {
            if !fields.contains_key(*field) {
                return Err(format!("event {} is missing {field}", self.base.event_type));
            }
        }
        Ok(())
    }
}

impl Serialize for RuntimeEvent {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        let mut fields = match self.payload.clone() {
            Value::Object(fields) => fields,
            _ => return Err(S::Error::custom("event payload must be an object")),
        };
        let base = serde_json::to_value(&self.base).map_err(S::Error::custom)?;
        let Some(base_fields) = base.as_object() else {
            return Err(S::Error::custom("event base must be an object"));
        };
        for (key, value) in base_fields {
            fields.insert(key.clone(), value.clone());
        }
        Value::Object(fields).serialize(serializer)
    }
}

impl<'de> Deserialize<'de> for RuntimeEvent {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = Value::deserialize(deserializer)?;
        let mut fields = match value {
            Value::Object(fields) => fields,
            _ => return Err(serde::de::Error::custom("event must be an object")),
        };
        let base_value = Value::Object(fields.clone());
        let base: EventBase = serde_json::from_value(base_value)
            .map_err(serde::de::Error::custom)?;
        for field in [
            "event_type",
            "occurred_at",
            "runtime_id",
            "auth_generation",
            "transition_id",
        ] {
            fields.remove(field);
        }
        let event = Self {
            base,
            payload: Value::Object(fields),
        };
        event.validate().map_err(serde::de::Error::custom)?;
        Ok(event)
    }
}

pub fn requires_transition_id(event_type: &str) -> bool {
    matches!(
        event_type,
        EVENT_SAFE_BOUNDARY_REACHED
            | EVENT_AUTH_RELOAD_STARTED
            | EVENT_AUTH_RELOAD_SUCCEEDED
            | EVENT_AUTH_RELOAD_FAILED
            | EVENT_IDENTITY_CHANGED
    )
}

fn empty_object() -> Value {
    Value::Object(Map::new())
}

fn is_false(value: &bool) -> bool {
    !*value
}

impl fmt::Display for TransitionOutcome {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Committed => formatter.write_str("committed"),
            Self::Rejected => formatter.write_str("rejected"),
            Self::Uncertain => formatter.write_str("uncertain"),
        }
    }
}
