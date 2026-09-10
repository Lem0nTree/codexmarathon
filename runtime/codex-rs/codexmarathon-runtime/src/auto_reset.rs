//! Durable, provider-backed automatic quota-reset coordination.
//!
//! The provider is the authority for whether a reset action exists.  This
//! module only coordinates that action: it never constructs a provider URL,
//! edits credentials, or treats a weekly reset timestamp as an action.  The
//! application supplies a [`QuotaResetExecutor`] backed by Codex's existing
//! account API.  The executor is deliberately a trait so the policy can be
//! tested without contacting a provider or consuming a real reset credit.

use crate::errors::{DomainError, DomainResult};
use crate::persistence::{atomic_write, ensure_private_dir, reject_unsafe_file};
use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

/// Current on-disk schema for automatic reset state.
pub const AUTO_RESET_STATE_VERSION: u32 = 1;

/// Provider capability observed for one managed account.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum QuotaResetCapability {
    /// No provider-supported immediate reset action was reported.
    #[default]
    Unavailable,
    /// A provider reset action can be consumed.  The id is opaque and may be
    /// omitted when the provider selects the next available credit.
    Available {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        credit_id: Option<String>,
    },
}

impl QuotaResetCapability {
    pub fn is_available(&self) -> bool {
        matches!(self, Self::Available { .. })
    }

    pub fn credit_id(&self) -> Option<&str> {
        match self {
            Self::Available { credit_id } => credit_id.as_deref(),
            Self::Unavailable => None,
        }
    }
}

/// Secret-free quota observation used by the reset policy.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct AccountQuotaTelemetry {
    pub account_id: String,
    /// Whether this account may participate in an automatic reset decision.
    pub eligible: bool,
    /// Provider-reported remaining percentage for the weekly/long window.
    /// `None` means the provider did not expose a weekly window.
    pub weekly_remaining_percent: Option<f64>,
    #[serde(default)]
    pub reset_capability: QuotaResetCapability,
    pub observed_at: DateTime<Utc>,
}

impl AccountQuotaTelemetry {
    /// Construct a strict zero-quota observation from provider used percent.
    pub fn from_weekly_used_percent(
        account_id: impl Into<String>,
        eligible: bool,
        used_percent: Option<f64>,
        reset_capability: QuotaResetCapability,
        observed_at: DateTime<Utc>,
    ) -> Self {
        Self {
            account_id: account_id.into(),
            eligible,
            weekly_remaining_percent: used_percent.map(|used| (100.0 - used).max(0.0)),
            reset_capability,
            observed_at,
        }
    }

    pub fn weekly_quota_is_zero(&self) -> bool {
        self.weekly_remaining_percent
            .is_some_and(|remaining| remaining <= 0.0)
    }
}

/// One provider reset invocation.  The idempotency key is persisted before
/// the executor is called and must be reused if the process is restarted.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct QuotaResetRequest {
    pub account_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub credit_id: Option<String>,
    pub idempotency_key: String,
}

/// Provider outcome for an immediate reset action.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum QuotaResetOutcome {
    Reset,
    AlreadyRedeemed,
    NothingToReset,
    NoCredit,
}

/// Result returned by the provider adapter after it has refreshed telemetry.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct QuotaResetExecution {
    pub outcome: QuotaResetOutcome,
    /// The executor must return a fresh observation.  A successful provider
    /// response without refreshed quota is not enough to resume Codex.
    pub refreshed_accounts: Vec<AccountQuotaTelemetry>,
}

/// Provider adapter for the real, existing reset-credit API.
pub trait QuotaResetExecutor: Send + Sync {
    fn execute(&self, request: &QuotaResetRequest) -> DomainResult<QuotaResetExecution>;
}

/// Durable lifecycle of automatic reset.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AutoResetPhase {
    #[default]
    Idle,
    Attempting,
    Succeeded,
    Blocked,
}

/// Secret-free persisted state.  The attempt key is safe to persist and is
/// the idempotency identity supplied to the provider.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct AutoResetState {
    pub version: u32,
    #[serde(default)]
    pub enabled: bool,
    #[serde(default)]
    pub phase: AutoResetPhase,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub attempt_key: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_attempt_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub cooldown_until: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub last_error: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub verified_account_id: String,
}

impl Default for AutoResetState {
    fn default() -> Self {
        Self {
            version: AUTO_RESET_STATE_VERSION,
            enabled: false,
            phase: AutoResetPhase::Idle,
            attempt_key: String::new(),
            last_attempt_at: None,
            cooldown_until: None,
            last_error: String::new(),
            verified_account_id: String::new(),
        }
    }
}

impl AutoResetState {
    fn validate(&self) -> DomainResult<()> {
        if self.version != AUTO_RESET_STATE_VERSION
            || self.attempt_key.len() > 512
            || self.last_error.len() > 4096
            || self.verified_account_id.len() > 240
            || self
                .last_error
                .chars()
                .any(|character| character == '\0' || character.is_control())
        {
            return Err(DomainError::InvalidAutoResetState);
        }
        Ok(())
    }
}

/// Private JSON store for the state above.
pub struct AutoResetStore {
    path: PathBuf,
    gate: Mutex<()>,
    clock: Arc<dyn Fn() -> DateTime<Utc> + Send + Sync>,
}

impl AutoResetStore {
    pub fn new(path: impl Into<PathBuf>) -> Self {
        Self::with_clock(path, Arc::new(Utc::now))
    }

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

    pub fn path(&self) -> &Path {
        &self.path
    }

    pub fn state(&self) -> DomainResult<AutoResetState> {
        let _guard = self
            .gate
            .lock()
            .map_err(|_| DomainError::InvalidAutoResetState)?;
        self.load_unlocked()
    }

    pub fn set_enabled(&self, enabled: bool) -> DomainResult<AutoResetState> {
        let _guard = self
            .gate
            .lock()
            .map_err(|_| DomainError::InvalidAutoResetState)?;
        let mut state = self.load_unlocked()?;
        state.enabled = enabled;
        if !enabled {
            state.phase = AutoResetPhase::Idle;
            state.cooldown_until = None;
            state.last_error.clear();
        }
        self.save_unlocked(&state)?;
        Ok(state)
    }

    /// Persist the idempotency key before any provider invocation.
    pub fn begin_attempt(&self, attempt_key: &str) -> DomainResult<AutoResetBegin> {
        if attempt_key.trim().is_empty() {
            return Err(DomainError::InvalidAutoResetState);
        }
        let _guard = self
            .gate
            .lock()
            .map_err(|_| DomainError::InvalidAutoResetState)?;
        let mut state = self.load_unlocked()?;
        let now = (self.clock)();
        if !state.enabled {
            return Ok(AutoResetBegin::Disabled(state));
        }
        if state.cooldown_until.is_some_and(|until| until > now) {
            return Ok(AutoResetBegin::Cooldown(state));
        }
        // A successful attempt for the same exhaustion epoch is terminal
        // until fresh telemetry proves a new zero-quota epoch.
        if state.phase == AutoResetPhase::Succeeded && state.attempt_key == attempt_key {
            return Ok(AutoResetBegin::AlreadySucceeded(state));
        }
        state.phase = AutoResetPhase::Attempting;
        state.attempt_key = attempt_key.to_string();
        state.last_attempt_at = Some(now);
        state.cooldown_until = None;
        state.last_error.clear();
        self.save_unlocked(&state)?;
        Ok(AutoResetBegin::Execute(state))
    }

    pub fn mark_succeeded(
        &self,
        attempt_key: &str,
        account_id: &str,
    ) -> DomainResult<AutoResetState> {
        self.update_attempt(attempt_key, AutoResetPhase::Succeeded, "", account_id, None)
    }

    pub fn mark_blocked(
        &self,
        attempt_key: &str,
        reason: &str,
        cooldown: Duration,
    ) -> DomainResult<AutoResetState> {
        let now = (self.clock)();
        self.update_attempt(
            attempt_key,
            AutoResetPhase::Blocked,
            reason,
            "",
            Some(now + cooldown.max(Duration::seconds(1))),
        )
    }

    fn update_attempt(
        &self,
        attempt_key: &str,
        phase: AutoResetPhase,
        error: &str,
        account_id: &str,
        cooldown_until: Option<DateTime<Utc>>,
    ) -> DomainResult<AutoResetState> {
        let _guard = self
            .gate
            .lock()
            .map_err(|_| DomainError::InvalidAutoResetState)?;
        let mut state = self.load_unlocked()?;
        if state.attempt_key != attempt_key {
            return Err(DomainError::AutoResetAttemptMismatch);
        }
        state.phase = phase;
        state.last_error = error.to_string();
        state.verified_account_id = account_id.to_string();
        state.cooldown_until = cooldown_until;
        self.save_unlocked(&state)?;
        Ok(state)
    }

    fn load_unlocked(&self) -> DomainResult<AutoResetState> {
        reject_unsafe_file(&self.path)?;
        let raw = match fs::read(&self.path) {
            Ok(raw) => raw,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(AutoResetState::default());
            }
            Err(error) => return Err(error.into()),
        };
        if raw.is_empty() {
            return Ok(AutoResetState::default());
        }
        let mut state: AutoResetState =
            serde_json::from_slice(&raw).map_err(|_| DomainError::InvalidAutoResetState)?;
        if state.version == 0 {
            state.version = AUTO_RESET_STATE_VERSION;
        }
        state.validate()?;
        Ok(state)
    }

    fn save_unlocked(&self, state: &AutoResetState) -> DomainResult<()> {
        state.validate()?;
        let mut raw = serde_json::to_vec_pretty(state)?;
        raw.push(b'\n');
        if let Some(parent) = self.path.parent() {
            ensure_private_dir(parent)?;
        }
        atomic_write(&self.path, &raw)
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum AutoResetBegin {
    Disabled(AutoResetState),
    Cooldown(AutoResetState),
    AlreadySucceeded(AutoResetState),
    Execute(AutoResetState),
}

/// Result of evaluating automatic reset against fresh managed-account data.
#[derive(Clone, Debug, PartialEq)]
pub enum AutoResetDecision {
    Disabled,
    NotAllAccountsExhausted,
    TelemetryBlocked(&'static str),
    ActionUnavailable,
    Cooldown(DateTime<Utc>),
    AlreadySucceeded,
    ResetApplied { account_id: String },
    ResetBlocked(String),
}

/// Executes at most one provider action for one all-zero quota epoch.
pub struct AutoResetCoordinator<'a, E: QuotaResetExecutor> {
    pub store: &'a AutoResetStore,
    pub executor: &'a E,
    pub cooldown: Duration,
}

impl<'a, E: QuotaResetExecutor> AutoResetCoordinator<'a, E> {
    pub fn evaluate(
        &self,
        accounts: &[AccountQuotaTelemetry],
        now: DateTime<Utc>,
    ) -> DomainResult<AutoResetDecision> {
        let state = self.store.state()?;
        if !state.enabled {
            return Ok(AutoResetDecision::Disabled);
        }
        let eligible: Vec<_> = accounts.iter().filter(|account| account.eligible).collect();
        if eligible.is_empty() {
            return Ok(AutoResetDecision::TelemetryBlocked(
                "no eligible managed accounts",
            ));
        }
        if eligible.iter().any(|account| {
            account.account_id.is_empty()
                || account.weekly_remaining_percent.is_none()
                || !account.weekly_quota_is_zero()
        }) {
            return Ok(AutoResetDecision::NotAllAccountsExhausted);
        }
        let Some(candidate) = eligible
            .iter()
            .filter(|account| account.reset_capability.is_available())
            .min_by(|left, right| {
                left.account_id.cmp(&right.account_id).then_with(|| {
                    left.reset_capability
                        .credit_id()
                        .cmp(&right.reset_capability.credit_id())
                })
            })
        else {
            return Ok(AutoResetDecision::ActionUnavailable);
        };

        let attempt_key = make_attempt_key(eligible.iter().map(|account| {
            (
                account.account_id.as_str(),
                account.reset_capability.credit_id(),
                account.observed_at.timestamp_millis(),
            )
        }));
        let begin = self.store.begin_attempt(&attempt_key)?;
        match begin {
            AutoResetBegin::Disabled(_) => return Ok(AutoResetDecision::Disabled),
            AutoResetBegin::Cooldown(state) => {
                return Ok(AutoResetDecision::Cooldown(
                    state.cooldown_until.unwrap_or(now),
                ));
            }
            AutoResetBegin::AlreadySucceeded(_) => {
                return Ok(AutoResetDecision::AlreadySucceeded);
            }
            AutoResetBegin::Execute(_) => {}
        }

        let request = QuotaResetRequest {
            account_id: candidate.account_id.clone(),
            credit_id: candidate.reset_capability.credit_id().map(str::to_string),
            idempotency_key: attempt_key.clone(),
        };
        let execution = match self.executor.execute(&request) {
            Ok(execution) => execution,
            Err(error) => {
                self.store
                    .mark_blocked(&attempt_key, error.code(), self.cooldown)?;
                return Ok(AutoResetDecision::ResetBlocked(error.code().to_string()));
            }
        };
        if !matches!(
            execution.outcome,
            QuotaResetOutcome::Reset | QuotaResetOutcome::AlreadyRedeemed
        ) {
            let reason = match execution.outcome {
                QuotaResetOutcome::NothingToReset => "provider reported nothing to reset",
                QuotaResetOutcome::NoCredit => "provider reported no reset action available",
                _ => "provider reset failed",
            };
            self.store
                .mark_blocked(&attempt_key, reason, self.cooldown)?;
            return Ok(AutoResetDecision::ResetBlocked(reason.to_string()));
        }
        let refreshed = execution.refreshed_accounts.iter().any(|account| {
            account.account_id == candidate.account_id
                && account.eligible
                && account
                    .weekly_remaining_percent
                    .is_some_and(|remaining| remaining > 0.0)
        });
        if !refreshed {
            let reason = "reset succeeded but refreshed quota was not verified";
            self.store
                .mark_blocked(&attempt_key, reason, self.cooldown)?;
            return Ok(AutoResetDecision::ResetBlocked(reason.to_string()));
        }
        self.store
            .mark_succeeded(&attempt_key, &candidate.account_id)?;
        Ok(AutoResetDecision::ResetApplied {
            account_id: candidate.account_id.clone(),
        })
    }
}

fn make_attempt_key<'a>(
    accounts: impl IntoIterator<Item = (&'a str, Option<&'a str>, i64)>,
) -> String {
    let mut parts: Vec<String> = accounts
        .into_iter()
        .map(|(account, credit, observed_at)| {
            format!("{account}:{}:{observed_at}", credit.unwrap_or("auto"))
        })
        .collect();
    parts.sort();
    format!("weekly-zero:{}", parts.join(","))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tempfile::tempdir;

    struct FakeExecutor {
        calls: AtomicUsize,
        result: Option<QuotaResetExecution>,
    }

    impl QuotaResetExecutor for FakeExecutor {
        fn execute(&self, _request: &QuotaResetRequest) -> DomainResult<QuotaResetExecution> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.result
                .clone()
                .ok_or(DomainError::InvalidAutoResetState)
        }
    }

    fn now() -> DateTime<Utc> {
        DateTime::parse_from_rfc3339("2026-09-10T00:00:00Z")
            .expect("timestamp")
            .with_timezone(&Utc)
    }

    fn account(id: &str, capability: QuotaResetCapability) -> AccountQuotaTelemetry {
        AccountQuotaTelemetry {
            account_id: id.to_string(),
            eligible: true,
            weekly_remaining_percent: Some(0.0),
            reset_capability: capability,
            observed_at: now(),
        }
    }

    #[test]
    fn default_disabled_never_calls_executor() {
        let directory = tempdir().expect("tempdir");
        let store =
            AutoResetStore::with_clock(directory.path().join("auto-reset.json"), Arc::new(now));
        let executor = FakeExecutor {
            calls: AtomicUsize::new(0),
            result: Some(QuotaResetExecution {
                outcome: QuotaResetOutcome::Reset,
                refreshed_accounts: vec![],
            }),
        };
        let coordinator = AutoResetCoordinator {
            store: &store,
            executor: &executor,
            cooldown: Duration::hours(1),
        };
        assert_eq!(
            coordinator
                .evaluate(
                    &[account(
                        "a",
                        QuotaResetCapability::Available { credit_id: None }
                    )],
                    now()
                )
                .expect("decision"),
            AutoResetDecision::Disabled
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn no_action_is_blocked_without_provider_capability() {
        let directory = tempdir().expect("tempdir");
        let store =
            AutoResetStore::with_clock(directory.path().join("auto-reset.json"), Arc::new(now));
        store.set_enabled(true).expect("enable");
        let executor = FakeExecutor {
            calls: AtomicUsize::new(0),
            result: None,
        };
        let coordinator = AutoResetCoordinator {
            store: &store,
            executor: &executor,
            cooldown: Duration::hours(1),
        };
        assert_eq!(
            coordinator
                .evaluate(&[account("a", QuotaResetCapability::Unavailable)], now())
                .expect("decision"),
            AutoResetDecision::ActionUnavailable
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn reset_requires_all_zero_and_freshly_refreshed_quota() {
        let directory = tempdir().expect("tempdir");
        let store =
            AutoResetStore::with_clock(directory.path().join("auto-reset.json"), Arc::new(now));
        store.set_enabled(true).expect("enable");
        let executor = FakeExecutor {
            calls: AtomicUsize::new(0),
            result: Some(QuotaResetExecution {
                outcome: QuotaResetOutcome::Reset,
                refreshed_accounts: vec![AccountQuotaTelemetry {
                    account_id: "a".to_string(),
                    eligible: true,
                    weekly_remaining_percent: Some(25.0),
                    reset_capability: QuotaResetCapability::Unavailable,
                    observed_at: now(),
                }],
            }),
        };
        let coordinator = AutoResetCoordinator {
            store: &store,
            executor: &executor,
            cooldown: Duration::hours(1),
        };
        let accounts = vec![
            account(
                "a",
                QuotaResetCapability::Available {
                    credit_id: Some("credit-a".to_string()),
                },
            ),
            account("b", QuotaResetCapability::Unavailable),
        ];
        assert_eq!(
            coordinator.evaluate(&accounts, now()).expect("decision"),
            AutoResetDecision::ResetApplied {
                account_id: "a".to_string()
            }
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
        assert_eq!(
            coordinator.evaluate(&accounts, now()).expect("decision"),
            AutoResetDecision::AlreadySucceeded
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn nonzero_account_prevents_reset() {
        let directory = tempdir().expect("tempdir");
        let store =
            AutoResetStore::with_clock(directory.path().join("auto-reset.json"), Arc::new(now));
        store.set_enabled(true).expect("enable");
        let executor = FakeExecutor {
            calls: AtomicUsize::new(0),
            result: None,
        };
        let coordinator = AutoResetCoordinator {
            store: &store,
            executor: &executor,
            cooldown: Duration::hours(1),
        };
        let mut other = account("b", QuotaResetCapability::Unavailable);
        other.weekly_remaining_percent = Some(10.0);
        assert_eq!(
            coordinator
                .evaluate(
                    &[
                        account("a", QuotaResetCapability::Available { credit_id: None }),
                        other
                    ],
                    now()
                )
                .expect("decision"),
            AutoResetDecision::NotAllAccountsExhausted
        );
        assert_eq!(executor.calls.load(Ordering::SeqCst), 0);
    }
}
