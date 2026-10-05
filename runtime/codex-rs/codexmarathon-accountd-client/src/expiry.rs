//! Credential-free expiry scheduler protocol.
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryCredit {
    pub credit_id: String,
    pub status: String,
    pub expires_at: Option<DateTime<Utc>>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryObservation {
    pub account_id: String,
    pub observed_at: DateTime<Utc>,
    /// None means incomplete/unknown, never an empty inventory.
    pub credits: Option<Vec<ExpiryCredit>>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryJob {
    pub job_id: String,
    pub account_id: String,
    pub credit_id: String,
    pub idempotency_key: String,
    pub lease_token: String,
    pub expires_at: DateTime<Utc>,
    pub attempt: i64,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExpiryOutcome {
    Reset,
    AlreadyRedeemed,
    Retry,
    NoCredit,
    NothingToReset,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryCompletion {
    pub job_id: String,
    pub lease_token: String,
    pub outcome: ExpiryOutcome,
    pub diagnostic_code: Option<String>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryJobStatus {
    pub job_id: String,
    pub account_id: String,
    pub credit_id: String,
    pub state: String,
    pub diagnostic_code: Option<String>,
    pub expires_at: DateTime<Utc>,
    pub next_attempt_at: Option<DateTime<Utc>>,
    pub attempt: i64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpiryStatus {
    pub codex_home: Option<String>,
    pub enabled: bool,
    pub effective_enabled: bool,
    pub executor_available: bool,
    pub jobs: Vec<ExpiryJobStatus>,
    #[serde(default)]
    pub jobs_truncated: bool,
    pub next_attempt_at: Option<DateTime<Utc>>,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct ExpirySet {
    pub enabled: bool,
    pub expected_codex_home: String,
}
