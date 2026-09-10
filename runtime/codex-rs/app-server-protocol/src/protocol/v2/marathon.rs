use crate::JsonSchema;
use crate::TS;
use serde::Deserialize;
use serde::Serialize;

/// Secret-free view of one account managed by the native Marathon service.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAccount {
    pub account_id: String,
    pub alias: String,
    pub active: bool,
    pub credential_present: bool,
    pub credential_health: String,
    /// Last native provider observation for the weekly/long quota window.
    /// `null` means the provider has not supplied a weekly window yet.
    #[serde(default)]
    pub weekly_quota_remaining_percent: Option<f64>,
    /// Whether the provider reported an immediate quota-reset action for this
    /// account. This is capability metadata, never an inferred timestamp.
    #[serde(default)]
    pub reset_action_available: bool,
}

/// Status of the in-process Marathon account manager.
///
/// This type intentionally contains identity and lifecycle metadata only. It
/// must never grow fields containing auth snapshots or token material.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonStatusResponse {
    pub enabled: bool,
    pub active_account_id: Option<String>,
    pub current_account_id: Option<String>,
    pub auth_generation: u64,
    pub active_turn_count: u32,
    pub accounts: Vec<MarathonAccount>,
    #[serde(default)]
    pub auto_reset_enabled: bool,
    #[serde(default)]
    pub auto_reset_phase: String,
    #[serde(default)]
    pub auto_reset_last_error: Option<String>,
}

/// Enable or disable automatic use of a provider reset action when every
/// eligible managed account has zero weekly quota.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetSetParams {
    pub enabled: bool,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetSetResponse {
    pub enabled: bool,
    pub phase: String,
}

/// Enable or disable the native Marathon service.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonEnabledSetParams {
    pub enabled: bool,
}

/// Result of changing the native Marathon enabled state.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonEnabledSetResponse {
    pub enabled: bool,
}

/// Request a manual switch to an account id or registered alias.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonSwitchParams {
    pub target: String,
}

#[derive(Serialize, Deserialize, Debug, Clone, Copy, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub enum MarathonSwitchOutcome {
    Committed,
    Rejected,
    Deferred,
}

/// Secret-free result of a manual account switch.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonSwitchResponse {
    pub account_id: Option<String>,
    pub outcome: MarathonSwitchOutcome,
    pub auth_generation: u64,
    pub active_turn_count: u32,
    pub reason: Option<String>,
}

/// Capture the currently authenticated Codex identity in the Marathon vault.
///
/// The auth document is read only through Codex's native AuthManager. Clients
/// never send token material to this request.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonImportParams {
    pub alias: String,
}

/// Secret-free result of importing the current native Codex identity.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonImportResponse {
    pub account_id: String,
    pub alias: String,
    pub active: bool,
    pub replaced: bool,
}
