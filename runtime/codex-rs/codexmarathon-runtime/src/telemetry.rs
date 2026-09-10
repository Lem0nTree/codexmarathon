//! Provider-neutral quota telemetry used by the pure policy layer.

use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

use crate::auto_reset::QuotaResetCapability;

/// Conventional quota window kind.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum WindowKind {
    /// Short or primary provider window.
    Primary,
    /// Long or secondary provider window.
    Secondary,
}

/// Explicit freshness marker for an observation.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Freshness {
    /// Observation may be used by policy if its age/reset checks also pass.
    #[default]
    Fresh,
    /// Observation must be refreshed before it authorizes a transition.
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

impl UsageWindow {
    /// Return whether this observation can be used at `now`.
    pub fn is_fresh_at(&self, now: DateTime<Utc>, max_age: Option<Duration>) -> bool {
        if self.freshness == Freshness::Stale
            || self.resets_at.is_some_and(|reset| reset <= now)
            || self.observed_at > now
        {
            return false;
        }
        max_age.is_none_or(|bound| bound <= Duration::zero() || now - self.observed_at < bound)
    }

    /// Return whether this window carries exhausted quota.
    pub fn is_exhausted(&self) -> bool {
        self.used_percent >= 100.0
    }

    /// Return the future reset boundary, if present.
    pub fn future_reset_at(&self, now: DateTime<Utc>) -> Option<DateTime<Utc>> {
        self.resets_at.filter(|reset| *reset > now)
    }
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

impl LimitTelemetry {
    /// Return a cloned window list for deterministic policy evaluation.
    pub fn window_list(&self) -> Vec<UsageWindow> {
        self.windows.clone()
    }
}

/// Complete telemetry for one configured account.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct AccountTelemetry {
    pub account_id: String,
    #[serde(default)]
    pub limits: BTreeMap<String, LimitTelemetry>,
    pub observed_at: DateTime<Utc>,
    #[serde(default)]
    pub usable: bool,
    /// Provider-reported capability for an immediate quota reset action.
    /// This is never inferred from a future reset timestamp.
    #[serde(default)]
    pub reset_capability: QuotaResetCapability,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
}

impl AccountTelemetry {
    /// Return all windows in deterministic bucket order.
    pub fn windows(&self) -> Vec<&UsageWindow> {
        self.limits
            .values()
            .flat_map(|limit| limit.windows.iter())
            .collect()
    }

    /// Refresh reset-boundary freshness without changing usage values.
    pub fn refresh_staleness(&mut self, now: DateTime<Utc>) {
        for window in self
            .limits
            .values_mut()
            .flat_map(|limit| limit.windows.iter_mut())
        {
            if window.resets_at.is_some_and(|reset| reset <= now) {
                window.freshness = Freshness::Stale;
            }
        }
    }

    /// Return a strict eligibility result for automatic account selection.
    pub fn complete_fresh_capacity(
        &self,
        now: DateTime<Utc>,
        max_age: Option<Duration>,
    ) -> Result<(), &'static str> {
        if !self.usable {
            return Err("account telemetry is unusable");
        }
        if self.account_id.is_empty() {
            return Err("account telemetry has no account id");
        }
        if self.limits.is_empty() {
            return Err("account telemetry has no rate-limit buckets");
        }
        if max_age.is_some_and(|bound| {
            bound > Duration::zero() && (self.observed_at > now || now - self.observed_at >= bound)
        }) {
            return Err("account telemetry is outside the freshness bound");
        }
        for limit in self.limits.values() {
            if limit.windows.is_empty() {
                return Err("rate-limit bucket has no windows");
            }
            let mut kinds = [false, false];
            for window in &limit.windows {
                let slot = match window.kind {
                    WindowKind::Primary => &mut kinds[0],
                    WindowKind::Secondary => &mut kinds[1],
                };
                if *slot {
                    return Err("rate-limit bucket has duplicate window kinds");
                }
                *slot = true;
                if !window.used_percent.is_finite() {
                    return Err("rate-limit usage is not finite");
                }
                if !window.is_fresh_at(now, max_age) {
                    return Err("rate-limit window is stale");
                }
                if window.is_exhausted() {
                    return Err("rate-limit window is exhausted");
                }
            }
        }
        Ok(())
    }

    /// Return whether any window crosses its configured threshold.
    pub fn threshold_reached(&self, thresholds: Thresholds) -> bool {
        self.windows().into_iter().any(|window| match window.kind {
            WindowKind::Primary => window.used_percent >= thresholds.primary_percent,
            WindowKind::Secondary => window.used_percent >= thresholds.secondary_percent,
        })
    }

    /// Find the earliest future reset boundary among exhausted windows.
    pub fn earliest_future_reset(&self, now: DateTime<Utc>) -> Option<DateTime<Utc>> {
        self.windows()
            .into_iter()
            .filter(|window| window.is_exhausted())
            .filter_map(|window| window.future_reset_at(now))
            .min()
    }

    /// Find the earliest reset whose window observation is still fresh under
    /// the supplied age bound. Stale reset hints must never authorize a wait
    /// or a subsequent transition.
    pub fn earliest_future_reset_with_age(
        &self,
        now: DateTime<Utc>,
        max_age: Option<Duration>,
    ) -> Option<DateTime<Utc>> {
        if !self.usable
            || self.observed_at > now
            || max_age
                .is_some_and(|bound| bound > Duration::zero() && now - self.observed_at >= bound)
        {
            return None;
        }
        self.windows()
            .into_iter()
            .filter(|window| window.is_exhausted() && window.is_fresh_at(now, max_age))
            .filter_map(|window| window.future_reset_at(now))
            .min()
    }

    /// Compute the deterministic pressure score used by account selection.
    pub fn rank(&self) -> CandidateRank {
        let mut rank = CandidateRank {
            account_id: self.account_id.clone(),
            ..CandidateRank::default()
        };
        for window in self.windows() {
            rank.max_used_percent = rank.max_used_percent.max(window.used_percent);
            rank.total_used_percent += window.used_percent;
            match window.kind {
                WindowKind::Primary => {
                    rank.primary_used_percent = rank.primary_used_percent.max(window.used_percent)
                }
                WindowKind::Secondary => {
                    rank.secondary_used_percent =
                        rank.secondary_used_percent.max(window.used_percent)
                }
            }
        }
        rank
    }
}

/// Deterministic account pressure score.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct CandidateRank {
    pub account_id: String,
    pub secondary_used_percent: f64,
    pub primary_used_percent: f64,
    pub max_used_percent: f64,
    pub total_used_percent: f64,
}

/// Thresholds for proactive switching.
#[derive(Clone, Copy, Debug, PartialEq, Serialize, Deserialize)]
pub struct Thresholds {
    pub primary_percent: f64,
    pub secondary_percent: f64,
}

impl Default for Thresholds {
    fn default() -> Self {
        Self {
            primary_percent: 90.0,
            secondary_percent: 95.0,
        }
    }
}

impl Thresholds {
    /// Normalize zero/out-of-range values to safe policy defaults.
    pub fn normalized(self) -> Self {
        let defaults = Self::default();
        Self {
            primary_percent: if self.primary_percent <= 0.0 {
                defaults.primary_percent
            } else {
                self.primary_percent.min(100.0)
            },
            secondary_percent: if self.secondary_percent <= 0.0 {
                defaults.secondary_percent
            } else {
                self.secondary_percent.min(100.0)
            },
        }
    }
}
