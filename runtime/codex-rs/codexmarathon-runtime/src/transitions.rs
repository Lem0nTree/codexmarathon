//! Secret-free transition intent and lifecycle state.
//!
//! These types are durable coordination records. They do not deploy an auth
//! snapshot or call Codex. The later transition coordinator will use them to
//! correlate safe-boundary, reload, and identity acknowledgements across
//! process loss.

use crate::errors::{DomainError, DomainResult};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

/// Outcome of a transition command or reconciliation attempt.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum TransitionOutcome {
    /// Target identity and generation were verified.
    Committed,
    /// Runtime rejected the request before applying it.
    Rejected,
    /// The caller cannot prove whether the state-changing operation applied.
    Uncertain,
}

/// Controller-side transition lifecycle phase.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TransitionPhase {
    /// No operation has been sent.
    #[default]
    Idle,
    /// Runtime accepted preparation.
    Prepared,
    /// Waiting for the native runtime's idle/safe boundary.
    WaitingForBoundary,
    /// Target snapshot deployment is in progress.
    Deploying,
    /// Commit request was sent and may need reconciliation.
    CommitSent,
    /// State-changing acknowledgement was lost or contradictory.
    Uncertain,
    /// Target identity was verified.
    Committed,
    /// Runtime rejected the transition.
    Rejected,
}

impl TransitionPhase {
    /// Return whether this phase is terminal.
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Committed | Self::Rejected)
    }
}

/// Durable secret-free state for one account transition.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct TransitionState {
    pub transition_id: String,
    pub runtime_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub current_account_id: String,
    pub target_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub disk_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub adopted_account_id: String,
    pub expected_generation: u64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub final_generation: u64,
    pub phase: TransitionPhase,
    pub prepare_sent: bool,
    pub commit_sent: bool,
    pub commit_attempts: u32,
    pub active_turn_count: u32,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completed_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

impl TransitionState {
    /// Create a new transition intent without performing any side effect.
    pub fn new(
        transition_id: impl Into<String>,
        runtime_id: impl Into<String>,
        current_account_id: impl Into<String>,
        target_account_id: impl Into<String>,
        expected_generation: u64,
        now: DateTime<Utc>,
    ) -> DomainResult<Self> {
        let state = Self {
            transition_id: transition_id.into(),
            runtime_id: runtime_id.into(),
            current_account_id: current_account_id.into(),
            target_account_id: target_account_id.into(),
            disk_account_id: String::new(),
            adopted_account_id: String::new(),
            expected_generation,
            final_generation: 0,
            phase: TransitionPhase::Idle,
            prepare_sent: false,
            commit_sent: false,
            commit_attempts: 0,
            active_turn_count: 0,
            created_at: now,
            updated_at: now,
            completed_at: None,
            reason: String::new(),
        };
        state.validate()?;
        Ok(state)
    }

    /// Validate cross-field invariants before persistence or replay.
    pub fn validate(&self) -> DomainResult<()> {
        if self.transition_id.trim().is_empty()
            || self.runtime_id.trim().is_empty()
            || self.target_account_id.trim().is_empty()
        {
            return Err(DomainError::InvalidTransitionState(
                "missing transition identity",
            ));
        }
        if self.expected_generation == 0 {
            return Err(DomainError::InvalidTransitionState(
                "expected_generation must be greater than zero",
            ));
        }
        if self.commit_sent && !self.prepare_sent {
            return Err(DomainError::InvalidTransitionState(
                "commit cannot be sent before prepare",
            ));
        }
        if self.phase == TransitionPhase::Committed {
            if !self.commit_sent {
                return Err(DomainError::InvalidTransitionState(
                    "committed transition lacks commit acknowledgement",
                ));
            }
            if self.final_generation == 0 || self.adopted_account_id.is_empty() {
                return Err(DomainError::InvalidTransitionState(
                    "committed transition lacks verified identity",
                ));
            }
            if self.completed_at.is_none() {
                return Err(DomainError::InvalidTransitionState(
                    "committed transition lacks completion time",
                ));
            }
        }
        if self.phase == TransitionPhase::Rejected && self.completed_at.is_none() {
            return Err(DomainError::InvalidTransitionState(
                "rejected transition lacks completion time",
            ));
        }
        if self.updated_at < self.created_at {
            return Err(DomainError::InvalidTransitionState(
                "updated_at precedes created_at",
            ));
        }
        if self.reason.len() > 4096
            || self
                .reason
                .chars()
                .any(|character| character == '\0' || character.is_control())
        {
            return Err(DomainError::InvalidTransitionState("invalid reason"));
        }
        Ok(())
    }

    /// Mark runtime preparation as accepted.
    pub fn mark_prepared(&mut self, now: DateTime<Utc>) -> DomainResult<()> {
        self.prepare_sent = true;
        self.phase = TransitionPhase::Prepared;
        self.updated_at = now;
        self.validate()
    }

    /// Mark that the runtime is waiting for an idle/safe boundary.
    pub fn mark_waiting_for_boundary(&mut self, now: DateTime<Utc>) -> DomainResult<()> {
        if !self.prepare_sent {
            return Err(DomainError::InvalidTransitionState(
                "boundary wait requires prepare",
            ));
        }
        self.phase = TransitionPhase::WaitingForBoundary;
        self.updated_at = now;
        self.validate()
    }

    /// Mark target snapshot deployment as started.
    pub fn mark_deploying(&mut self, now: DateTime<Utc>) -> DomainResult<()> {
        if !self.prepare_sent {
            return Err(DomainError::InvalidTransitionState(
                "deployment requires prepare",
            ));
        }
        self.phase = TransitionPhase::Deploying;
        self.updated_at = now;
        self.validate()
    }

    /// Record one commit attempt.
    pub fn mark_commit_sent(&mut self, now: DateTime<Utc>) -> DomainResult<()> {
        if !self.prepare_sent {
            return Err(DomainError::InvalidTransitionState(
                "commit requires prepare",
            ));
        }
        self.commit_sent = true;
        self.commit_attempts = self.commit_attempts.saturating_add(1);
        self.phase = TransitionPhase::CommitSent;
        self.updated_at = now;
        self.validate()
    }

    /// Record an uncertain state-changing acknowledgement.
    pub fn mark_uncertain(
        &mut self,
        reason: impl Into<String>,
        now: DateTime<Utc>,
    ) -> DomainResult<()> {
        self.reason = reason.into();
        self.phase = TransitionPhase::Uncertain;
        self.updated_at = now;
        self.validate()
    }

    /// Record a verified committed identity and generation.
    pub fn mark_committed(
        &mut self,
        adopted_account_id: impl Into<String>,
        final_generation: u64,
        now: DateTime<Utc>,
    ) -> DomainResult<()> {
        self.adopted_account_id = adopted_account_id.into();
        self.final_generation = final_generation;
        self.phase = TransitionPhase::Committed;
        self.completed_at = Some(now);
        self.updated_at = now;
        self.validate()
    }

    /// Record a terminal rejection without exposing provider details.
    pub fn mark_rejected(
        &mut self,
        reason: impl Into<String>,
        now: DateTime<Utc>,
    ) -> DomainResult<()> {
        self.reason = reason.into();
        self.phase = TransitionPhase::Rejected;
        self.completed_at = Some(now);
        self.updated_at = now;
        self.validate()
    }
}

/// Secret-free result returned by transition orchestration.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct TransitionResult {
    pub transition_id: String,
    pub target_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub adopted_account_id: String,
    pub expected_generation: u64,
    pub final_generation: u64,
    pub outcome: TransitionOutcome,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completed_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub runtime_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub disk_account_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub reason: String,
}

/// Read-only intent passed from policy into a future transition coordinator.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct TransitionIntent {
    pub transition_id: String,
    pub target_account_id: String,
    pub expected_generation: u64,
}

impl TransitionIntent {
    /// Validate the minimum correlation fields.
    pub fn validate(&self) -> DomainResult<()> {
        if self.transition_id.trim().is_empty() || self.target_account_id.trim().is_empty() {
            return Err(DomainError::InvalidTransitionState(
                "missing transition intent",
            ));
        }
        if self.expected_generation == 0 {
            return Err(DomainError::InvalidTransitionState(
                "expected_generation must be greater than zero",
            ));
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Duration;

    fn time() -> DateTime<Utc> {
        DateTime::parse_from_rfc3339("2026-01-01T00:00:00Z")
            .expect("time")
            .with_timezone(&Utc)
    }

    #[test]
    fn lifecycle_preserves_secret_free_transition_state() {
        let start = time();
        let mut state =
            TransitionState::new("tx-1", "runtime-1", "a", "b", 2, start).expect("state");
        state
            .mark_prepared(start + Duration::seconds(1))
            .expect("prepare");
        state
            .mark_waiting_for_boundary(start + Duration::seconds(2))
            .expect("boundary");
        state
            .mark_deploying(start + Duration::seconds(3))
            .expect("deploy");
        state
            .mark_commit_sent(start + Duration::seconds(4))
            .expect("commit");
        state
            .mark_committed("b", 3, start + Duration::seconds(5))
            .expect("complete");
        assert_eq!(state.phase, TransitionPhase::Committed);
        assert!(state.validate().is_ok());
    }

    #[test]
    fn commit_before_prepare_is_rejected() {
        let mut state =
            TransitionState::new("tx-1", "runtime-1", "a", "b", 2, time()).expect("state");
        assert!(matches!(
            state.mark_commit_sent(time()),
            Err(DomainError::InvalidTransitionState(_))
        ));
    }
}
