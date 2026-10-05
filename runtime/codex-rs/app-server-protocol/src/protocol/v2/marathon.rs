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
    /// Daemon-authoritative expiry automation; absent when accountd is unavailable.
    #[serde(default)]
    pub auto_reset_expiry: Option<MarathonAutoResetExpiryStatus>,
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

/// Configure use of banked reset credits fifteen minutes before their expiry.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetExpirySetParams {
    pub enabled: bool,
}

/// Secret-free state of one durable expiry job.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetExpiryJob {
    pub job_id: String,
    pub account_id: String,
    pub credit_id: String,
    pub state: String,
    pub expires_at: String,
    pub next_attempt_at: Option<String>,
    pub attempt: i64,
    pub diagnostic_code: Option<String>,
}

/// Daemon-authoritative expiry state. Enabling this policy is independent of
/// the zero-weekly-quota policy; the Marathon master switch gates execution.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetExpiryStatusResponse {
    pub enabled: bool,
    pub effective_enabled: bool,
    pub executor_available: bool,
    pub jobs: Vec<MarathonAutoResetExpiryJob>,
    #[serde(default)]
    pub jobs_truncated: bool,
    pub next_attempt_at: Option<String>,
}

/// Shared expiry state embedded in the overall Marathon status.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonAutoResetExpiryStatus {
    pub enabled: bool,
    pub effective_enabled: bool,
    pub executor_available: bool,
    pub jobs: Vec<MarathonAutoResetExpiryJob>,
    #[serde(default)]
    pub jobs_truncated: bool,
    pub next_attempt_at: Option<String>,
}

impl From<MarathonAutoResetExpiryStatusResponse> for MarathonAutoResetExpiryStatus {
    fn from(response: MarathonAutoResetExpiryStatusResponse) -> Self {
        Self {
            enabled: response.enabled,
            effective_enabled: response.effective_enabled,
            executor_available: response.executor_available,
            jobs: response.jobs,
            jobs_truncated: response.jobs_truncated,
            next_attempt_at: response.next_attempt_at,
        }
    }
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

/// Checkpoint an already-saved current account through native AuthManager.
/// Optimistic identity/generation binding prevents saving a different account
/// after a status/selection race. No snapshot or passphrase crosses this API.
#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonCheckpointParams {
    pub expected_account_id: String,
    pub expected_auth_generation: u64,
}

#[derive(Serialize, Deserialize, Debug, Clone, PartialEq, Eq, JsonSchema, TS)]
#[serde(rename_all = "camelCase")]
#[ts(rename_all = "camelCase", export_to = "v2/")]
pub struct MarathonCheckpointResponse {
    pub account_id: String,
    pub auth_generation: u64,
}

#[cfg(test)]
mod expiry_tests {
    use super::*;

    #[test]
    fn older_status_defaults_expiry_to_unavailable() {
        let status: MarathonStatusResponse = serde_json::from_value(serde_json::json!({
            "enabled": true, "activeAccountId": null, "currentAccountId": null,
            "authGeneration": 1, "activeTurnCount": 0, "accounts": []
        }))
        .expect("older status");
        assert!(status.auto_reset_expiry.is_none());
        assert!(!status.auto_reset_enabled);
    }

    #[test]
    fn expiry_set_and_status_use_camel_case_wire_fields() {
        let params = MarathonAutoResetExpirySetParams { enabled: true };
        assert_eq!(
            serde_json::to_value(params).unwrap(),
            serde_json::json!({ "enabled": true })
        );
        let response = MarathonAutoResetExpiryStatusResponse {
            enabled: true,
            effective_enabled: false,
            executor_available: false,
            jobs: Vec::new(),
            jobs_truncated: false,
            next_attempt_at: None,
        };
        let value = serde_json::to_value(&response).unwrap();
        assert_eq!(value["effectiveEnabled"], false);
        assert_eq!(value["executorAvailable"], false);
        assert_eq!(
            serde_json::from_value::<MarathonAutoResetExpiryStatusResponse>(value).unwrap(),
            response
        );
    }

    #[test]
    fn embedded_expiry_status_preserves_rpc_wire_shape() {
        let response = MarathonAutoResetExpiryStatusResponse {
            enabled: true,
            effective_enabled: false,
            executor_available: true,
            jobs: vec![MarathonAutoResetExpiryJob {
                job_id: "job-1".into(),
                account_id: "account-1".into(),
                credit_id: "credit-1".into(),
                state: "scheduled".into(),
                expires_at: "2026-10-05T00:00:00Z".into(),
                next_attempt_at: Some("2026-10-04T23:45:00Z".into()),
                attempt: 2,
                diagnostic_code: Some("retry".into()),
            }],
            jobs_truncated: true,
            next_attempt_at: Some("2026-10-04T23:45:00Z".into()),
        };
        let expected = serde_json::to_value(&response).unwrap();
        let shared = MarathonAutoResetExpiryStatus::from(response);
        assert_eq!(serde_json::to_value(shared).unwrap(), expected);
    }
}
