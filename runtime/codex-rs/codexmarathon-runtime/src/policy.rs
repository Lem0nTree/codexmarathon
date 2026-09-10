//! Pure, deterministic account-selection policy.
//!
//! Policy consumes normalized telemetry and returns an authorization hint. It
//! never reads credentials, writes the vault, reloads AuthManager, or starts a
//! transition. Those side effects belong to later orchestration stages.

use crate::telemetry::{AccountTelemetry, CandidateRank, Thresholds};
use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};

/// Why a policy evaluation was requested.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PolicyTrigger {
    /// Explicit operator selection or initial account choice.
    #[default]
    Manual,
    /// An account crossed a configured usage threshold.
    ProactiveThreshold,
    /// The active request received a hard usage-limit response.
    UsageLimitExceeded,
    /// A reset boundary was reached and telemetry is being revalidated.
    ResetRevalidation,
}

/// Stable policy result category.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PolicyDecisionType {
    /// Keep using the current account.
    Stay,
    /// Ask the transition layer to select the returned account.
    Transition,
    /// Wait for an authoritative future quota reset.
    WaitForReset,
    /// Refresh or probe telemetry before selecting an account.
    NoTelemetry,
    /// Suppress a duplicate/in-flight request until the cooldown boundary.
    Cooldown,
}

/// Policy settings.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct PolicyConfig {
    /// Proactive primary/secondary usage thresholds.
    pub thresholds: Thresholds,
    /// Maximum age accepted for automatic selection.
    pub freshness_ttl: Duration,
    /// Minimum delay between automatic transitions.
    pub cooldown: Duration,
}

impl Default for PolicyConfig {
    fn default() -> Self {
        Self {
            thresholds: Thresholds::default(),
            freshness_ttl: Duration::minutes(5),
            cooldown: Duration::minutes(1),
        }
    }
}

impl PolicyConfig {
    /// Normalize zero or invalid settings to conservative defaults.
    pub fn normalized(self) -> Self {
        let defaults = Self::default();
        Self {
            thresholds: self.thresholds.normalized(),
            freshness_ttl: if self.freshness_ttl <= Duration::zero() {
                defaults.freshness_ttl
            } else {
                self.freshness_ttl
            },
            cooldown: if self.cooldown <= Duration::zero() {
                defaults.cooldown
            } else {
                self.cooldown
            },
        }
    }
}

/// One pure policy evaluation request.
#[derive(Clone, Debug)]
pub struct PolicyInput {
    /// Complete or partial account observations.
    pub accounts: Vec<AccountTelemetry>,
    /// Runtime's currently authenticated identity, if known.
    pub active_account_id: Option<String>,
    /// Reason for this evaluation.
    pub trigger: PolicyTrigger,
    /// Optional caller override for proactive thresholds.
    pub thresholds: Option<Thresholds>,
    /// Runtime/controller cooldown boundary, if one is already active.
    pub cooldown_until: Option<DateTime<Utc>>,
    /// Existing target intent. A policy tick must not create a duplicate.
    pub pending_target_id: Option<String>,
    /// Evaluation time.
    pub now: DateTime<Utc>,
}

/// Secret-free policy result.
#[derive(Clone, Debug, PartialEq)]
pub struct PolicyDecision {
    /// Category of the decision.
    pub kind: PolicyDecisionType,
    /// Trigger that produced the decision.
    pub trigger: PolicyTrigger,
    /// Account currently in use, if known.
    pub active_account_id: Option<String>,
    /// Selected account when `kind` is `Transition`.
    pub account_id: Option<String>,
    /// Stable operator-safe explanation.
    pub reason: String,
    /// Earliest future reset when waiting for quota.
    pub reset_at: Option<DateTime<Utc>>,
    /// When a cooldown/no-telemetry caller should evaluate again.
    pub retry_at: Option<DateTime<Utc>>,
    /// Selected account's deterministic pressure score.
    pub rank: Option<CandidateRank>,
}

impl PolicyDecision {
    /// Return whether the transition layer should consider an account switch.
    pub fn is_transition(&self) -> bool {
        self.kind == PolicyDecisionType::Transition
    }

    /// Return whether policy asks the caller to wait or refresh data.
    pub fn is_waiting(&self) -> bool {
        matches!(
            self.kind,
            PolicyDecisionType::WaitForReset
                | PolicyDecisionType::NoTelemetry
                | PolicyDecisionType::Cooldown
        )
    }
}

/// Evaluate account telemetry without mutating state or performing I/O.
pub fn evaluate(input: PolicyInput, config: PolicyConfig) -> PolicyDecision {
    let config = config.normalized();
    let now = if input.now.timestamp() == 0 && input.now.timestamp_subsec_nanos() == 0 {
        Utc::now()
    } else {
        input.now
    };
    let thresholds = input.thresholds.unwrap_or(config.thresholds).normalized();
    let active = input.active_account_id.clone();
    let trigger = input.trigger;
    let mut accounts = deduplicate_accounts(input.accounts);
    accounts.sort_by(|left, right| left.account_id.cmp(&right.account_id));
    let max_age = Some(config.freshness_ttl);

    let active_observation = active.as_deref().and_then(|account_id| {
        accounts
            .iter()
            .find(|account| account.account_id == account_id)
    });
    let active_eligible = active_observation
        .is_some_and(|account| account.complete_fresh_capacity(now, max_age).is_ok());
    if trigger == PolicyTrigger::ProactiveThreshold
        && active_observation.is_some()
        && active_eligible
        && !active_observation.is_some_and(|account| account.threshold_reached(thresholds))
    {
        return decision(
            PolicyDecisionType::Stay,
            trigger,
            active,
            None,
            "active account is below configured thresholds",
            None,
            None,
            None,
        );
    }

    if let Some(pending_target) = input
        .pending_target_id
        .as_deref()
        .filter(|target| !target.is_empty())
    {
        return decision(
            PolicyDecisionType::Cooldown,
            trigger,
            active,
            Some(pending_target.to_string()),
            "a transition is already in flight",
            None,
            input.cooldown_until,
            None,
        );
    }

    let eligible: Vec<&AccountTelemetry> = accounts
        .iter()
        .filter(|account| account.complete_fresh_capacity(now, max_age).is_ok())
        .collect();
    let mut candidates: Vec<&AccountTelemetry> = eligible
        .iter()
        .copied()
        .filter(|account| active.as_deref() != Some(account.account_id.as_str()))
        .filter(|account| {
            trigger != PolicyTrigger::ProactiveThreshold || !account.threshold_reached(thresholds)
        })
        .collect();

    if active.is_none() && trigger == PolicyTrigger::Manual {
        candidates = eligible.clone();
    }

    if let Some(account) = choose_candidate(&mut candidates) {
        let cooldown_until = input.cooldown_until.unwrap_or(DateTime::<Utc>::MIN_UTC);
        if trigger != PolicyTrigger::Manual && cooldown_until > now {
            return decision(
                PolicyDecisionType::Cooldown,
                trigger,
                active,
                Some(account.account_id.clone()),
                "switch cooldown is active",
                None,
                Some(cooldown_until),
                Some(account.rank()),
            );
        }
        let reason = match trigger {
            PolicyTrigger::UsageLimitExceeded => {
                "UsageLimitExceeded requires a fresh replacement account"
            }
            PolicyTrigger::ProactiveThreshold => {
                "active account crossed the configured usage threshold"
            }
            _ => "fresh account has lower quota pressure",
        };
        return decision(
            PolicyDecisionType::Transition,
            trigger,
            active,
            Some(account.account_id.clone()),
            reason,
            None,
            None,
            Some(account.rank()),
        );
    }

    if !eligible.is_empty()
        && (trigger == PolicyTrigger::ProactiveThreshold
            || (active_eligible && trigger != PolicyTrigger::UsageLimitExceeded))
    {
        return decision(
            PolicyDecisionType::Stay,
            trigger,
            active,
            None,
            "no lower-pressure replacement account is available",
            None,
            None,
            None,
        );
    }

    let reset_at = accounts
        .iter()
        .filter_map(|account| {
            account.earliest_future_reset_with_age(now, Some(config.freshness_ttl))
        })
        .min();
    if reset_at.is_some() {
        return decision(
            PolicyDecisionType::WaitForReset,
            trigger,
            active,
            None,
            "all accounts are exhausted; wait for the earliest reset",
            reset_at,
            reset_at,
            None,
        );
    }

    decision(
        PolicyDecisionType::NoTelemetry,
        trigger,
        active,
        None,
        "no fresh complete account telemetry is available",
        None,
        None,
        None,
    )
}

/// Compatibility spelling for callers that use an evaluation verb.
pub fn evaluate_policy(input: PolicyInput, config: PolicyConfig) -> PolicyDecision {
    evaluate(input, config)
}

fn decision(
    kind: PolicyDecisionType,
    trigger: PolicyTrigger,
    active_account_id: Option<String>,
    account_id: Option<String>,
    reason: &str,
    reset_at: Option<DateTime<Utc>>,
    retry_at: Option<DateTime<Utc>>,
    rank: Option<CandidateRank>,
) -> PolicyDecision {
    PolicyDecision {
        kind,
        trigger,
        active_account_id,
        account_id,
        reason: reason.to_string(),
        reset_at,
        retry_at,
        rank,
    }
}

fn deduplicate_accounts(accounts: Vec<AccountTelemetry>) -> Vec<AccountTelemetry> {
    let mut unique = Vec::with_capacity(accounts.len());
    for account in accounts {
        if account.account_id.is_empty()
            || unique
                .iter()
                .any(|seen: &AccountTelemetry| seen.account_id == account.account_id)
        {
            continue;
        }
        unique.push(account);
    }
    unique
}

fn choose_candidate<'a>(accounts: &mut Vec<&'a AccountTelemetry>) -> Option<&'a AccountTelemetry> {
    accounts.sort_by(|left, right| {
        let left = left.rank();
        let right = right.rank();
        left.secondary_used_percent
            .total_cmp(&right.secondary_used_percent)
            .then_with(|| {
                left.primary_used_percent
                    .total_cmp(&right.primary_used_percent)
            })
            .then_with(|| left.max_used_percent.total_cmp(&right.max_used_percent))
            .then_with(|| left.total_used_percent.total_cmp(&right.total_used_percent))
            .then_with(|| left.account_id.cmp(&right.account_id))
    });
    accounts.first().copied()
}

/// Stable, whitespace-normalized reason helper for journal/UI callers.
pub fn normalize_reason(reason: &str) -> String {
    reason.trim().to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::telemetry::{Freshness, LimitTelemetry, UsageWindow, WindowKind};

    fn account(account_id: &str, primary: f64, secondary: f64) -> AccountTelemetry {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .expect("time")
            .with_timezone(&Utc);
        AccountTelemetry {
            account_id: account_id.to_string(),
            limits: [(
                "codex".to_string(),
                LimitTelemetry {
                    limit_id: "codex".to_string(),
                    limit_name: String::new(),
                    plan_type: String::new(),
                    windows: vec![
                        UsageWindow {
                            kind: WindowKind::Primary,
                            used_percent: primary,
                            window_duration_mins: Some(300),
                            resets_at: Some(now + Duration::hours(1)),
                            observed_at: now,
                            freshness: Freshness::Fresh,
                        },
                        UsageWindow {
                            kind: WindowKind::Secondary,
                            used_percent: secondary,
                            window_duration_mins: Some(1_000),
                            resets_at: Some(now + Duration::hours(2)),
                            observed_at: now,
                            freshness: Freshness::Fresh,
                        },
                    ],
                },
            )]
            .into_iter()
            .collect(),
            observed_at: now,
            usable: true,
            reset_capability: Default::default(),
            source: "test".to_string(),
        }
    }

    #[test]
    fn policy_selects_lowest_secondary_then_primary_pressure() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .expect("time")
            .with_timezone(&Utc);
        let result = evaluate(
            PolicyInput {
                accounts: vec![
                    account("account-b", 10.0, 20.0),
                    account("account-a", 30.0, 20.0),
                ],
                active_account_id: Some("account-c".to_string()),
                trigger: PolicyTrigger::UsageLimitExceeded,
                thresholds: None,
                cooldown_until: None,
                pending_target_id: None,
                now,
            },
            PolicyConfig::default(),
        );
        assert_eq!(result.kind, PolicyDecisionType::Transition);
        assert_eq!(result.account_id.as_deref(), Some("account-b"));
    }

    #[test]
    fn proactive_trigger_stays_below_threshold() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .expect("time")
            .with_timezone(&Utc);
        let result = evaluate(
            PolicyInput {
                accounts: vec![account("account-a", 10.0, 20.0)],
                active_account_id: Some("account-a".to_string()),
                trigger: PolicyTrigger::ProactiveThreshold,
                thresholds: None,
                cooldown_until: None,
                pending_target_id: None,
                now,
            },
            PolicyConfig::default(),
        );
        assert_eq!(result.kind, PolicyDecisionType::Stay);
    }

    #[test]
    fn stale_accounts_do_not_authorize_a_switch() {
        let now = DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .expect("time")
            .with_timezone(&Utc);
        let mut stale = account("account-a", 10.0, 20.0);
        stale.observed_at = now - Duration::hours(1);
        let result = evaluate(
            PolicyInput {
                accounts: vec![stale],
                active_account_id: Some("account-b".to_string()),
                trigger: PolicyTrigger::UsageLimitExceeded,
                thresholds: None,
                cooldown_until: None,
                pending_target_id: None,
                now,
            },
            PolicyConfig::default(),
        );
        assert_eq!(result.kind, PolicyDecisionType::NoTelemetry);
    }
}
