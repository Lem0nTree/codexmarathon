use super::TurnError;
use crate::JsonSchema;
use crate::RequestId;
use crate::TS;
use serde::Deserialize;
use serde::Serialize;

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct AuthRecoveryNotification {
    pub thread_id: String,
    pub turn_id: String,
    pub provider: String,
    pub message: String,
}

/// Internal command from the embedded Marathon authority to the Codex TUI.
///
/// The TUI already owns the parked `UsageLimitExceeded` continuation.  This
/// notification is only an authorization hand-off; it does not carry prompt
/// text and must never be interpreted as a request to create another turn.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct CodexMarathonRecoveryReleaseNotification {
    pub recovery_id: String,
    pub thread_id: Option<String>,
    pub transition_id: String,
    #[ts(type = "number")]
    pub expected_generation: u64,
}

/// Result sent by the TUI after applying a Marathon release command to the
/// session-owned pending recovery state.
///
/// The result is intentionally string-shaped at this upstream protocol seam;
/// the Marathon adapter maps it to its typed release outcome without making
/// the Codex app-server protocol depend on Marathon crates.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct CodexMarathonRecoveryReleaseResultNotification {
    pub recovery_id: String,
    pub thread_id: Option<String>,
    pub transition_id: String,
    #[ts(type = "number")]
    pub expected_generation: u64,
    pub outcome: String,
    pub error_code: Option<String>,
    pub error_message: Option<String>,
}

/// Lifecycle observation emitted by the embedded Codex TUI for the native
/// Marathon recovery seam. The TUI sends this only after its existing pending
/// synthetic turn is created, dispatched, or completed. It carries metadata,
/// never prompt text or credentials.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct CodexMarathonRecoveryLifecycleNotification {
    /// One of `recovery_parked`, `recovery_started`, or
    /// `recovery_completed`.
    pub event_type: String,
    pub recovery_id: String,
    pub thread_id: Option<String>,
    pub turn_id: Option<String>,
    pub source_account_id: Option<String>,
    pub reason: Option<String>,
    pub outcome: Option<String>,
    pub error_code: Option<String>,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct DeprecationNoticeNotification {
    /// Concise summary of what is deprecated.
    pub summary: String,
    /// Optional extra guidance, such as migration steps or rationale.
    pub details: Option<String>,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct WarningNotification {
    /// Optional thread target when the warning applies to a specific thread.
    pub thread_id: Option<String>,
    /// Concise warning message for the user.
    pub message: String,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct GuardianWarningNotification {
    /// Thread target for the guardian warning.
    pub thread_id: String,
    /// Concise guardian warning message for the user.
    pub message: String,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct StrictReviewRequiredNotification {
    pub thread_id: String,
    pub turn_id: String,
    /// Unix timestamp (in milliseconds) when this review started.
    #[ts(type = "number")]
    pub started_at_ms: i64,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct ErrorNotification {
    pub error: TurnError,
    // Set to true if the error is transient and the app-server process will automatically retry.
    // If true, this will not interrupt a turn.
    pub will_retry: bool,
    pub thread_id: String,
    pub turn_id: String,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(export_to = "v2/")]
pub struct ServerRequestResolvedNotification {
    pub thread_id: String,
    pub request_id: RequestId,
}
