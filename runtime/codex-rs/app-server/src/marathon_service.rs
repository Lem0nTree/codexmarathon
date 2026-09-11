//! Native Marathon account service owned by app-server.
//!
//! This service composes the existing AuthManager, ThreadManager and turn
//! watcher with Marathon's secret-free registry and opaque snapshot vault. It
//! reports status, enables/disables Marathon, performs explicit account
//! switches, and coordinates quota-triggered switches at an idle boundary.

use codex_app_server_protocol::{
    MarathonAccount, MarathonAutoResetSetResponse, MarathonEnabledSetResponse,
    MarathonImportResponse, MarathonStatusResponse, MarathonSwitchOutcome, MarathonSwitchResponse,
};
use codex_core::ThreadManager;
use codex_login::{AuthDotJson, AuthManager, AuthReloadStatus, AuthTransitionSnapshot};
use codex_protocol::protocol::RateLimitSnapshot;
use codexmarathon_runtime::{
    AccountStore, AutoResetStore, CredentialHealth, DomainError, FileAccountRegistry,
    FileSnapshotVault, MarathonConfig, SnapshotVault,
};
use std::sync::Arc;
use std::sync::Mutex as StdMutex;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use thiserror::Error;
use tokio::sync::{Mutex, watch};

const AUTO_FAILOVER_USED_PERCENT: f64 = 90.0;
const AUTO_FAILOVER_TELEMETRY_TTL: chrono::Duration = chrono::Duration::minutes(5);

#[derive(Clone, Copy, Debug)]
struct CachedQuota {
    used_percent: f64,
    observed_at: chrono::DateTime<chrono::Utc>,
    is_weekly: bool,
}

#[derive(Debug, Error)]
pub(crate) enum MarathonServiceError {
    #[error("Marathon state is unavailable")]
    StateUnavailable,
    #[error("Marathon account was not found")]
    AccountNotFound,
    #[error("Marathon account alias is ambiguous")]
    AmbiguousAlias,
    #[error("Marathon account alias is already used by another account")]
    AliasConflict,
    #[error("Marathon account alias is invalid")]
    InvalidAlias,
    #[error("Codex authentication is required")]
    AuthenticationRequired,
    #[error("Marathon account has no stored credentials")]
    CredentialsUnavailable,
    #[error("Marathon native auth transition failed")]
    NativeTransition,
    #[error("Marathon persistence failed")]
    Persistence,
}

/// App-server-owned native Marathon composition.
///
/// `transition_mutex` is shared with the normal app-server account and turn
/// processors. Holding it across the active-turn check, guarded AuthManager
/// operation, and model transport invalidation closes the turn-start race.
pub(crate) struct MarathonService {
    auth_manager: Arc<AuthManager>,
    thread_manager: Arc<ThreadManager>,
    running_turns: watch::Receiver<usize>,
    transition_mutex: Arc<Mutex<()>>,
    registry: Arc<FileAccountRegistry>,
    vault: Arc<FileSnapshotVault>,
    auto_reset: Arc<AutoResetStore>,
    auth_generation: AtomicU64,
    quota_by_account: StdMutex<std::collections::BTreeMap<String, CachedQuota>>,
    auto_failover_in_flight: AtomicBool,
}

impl MarathonService {
    pub(crate) fn from_config(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        transition_mutex: Arc<Mutex<()>>,
        config: &MarathonConfig,
    ) -> Result<Self, MarathonServiceError> {
        config
            .validate()
            .map_err(|_| MarathonServiceError::StateUnavailable)?;
        Ok(Self {
            auth_manager,
            thread_manager,
            running_turns,
            transition_mutex,
            registry: Arc::new(FileAccountRegistry::new(config.registry_path())),
            vault: Arc::new(FileSnapshotVault::new(config.vault_dir())),
            auto_reset: Arc::new(AutoResetStore::new(config.auto_reset_state_path())),
            auth_generation: AtomicU64::new(1),
            quota_by_account: StdMutex::new(std::collections::BTreeMap::new()),
            auto_failover_in_flight: AtomicBool::new(false),
        })
    }

    #[cfg(test)]
    pub(crate) fn with_stores_for_test(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        transition_mutex: Arc<Mutex<()>>,
        registry: Arc<FileAccountRegistry>,
        vault: Arc<FileSnapshotVault>,
    ) -> Self {
        Self {
            auth_manager,
            thread_manager,
            running_turns,
            transition_mutex,
            registry: Arc::clone(&registry),
            vault,
            auto_reset: Arc::new(AutoResetStore::new(
                registry
                    .path()
                    .parent()
                    .unwrap_or_else(|| std::path::Path::new("."))
                    .join("auto-reset.json"),
            )),
            auth_generation: AtomicU64::new(1),
            quota_by_account: StdMutex::new(std::collections::BTreeMap::new()),
            auto_failover_in_flight: AtomicBool::new(false),
        }
    }

    /// Cache one provider-authored observation for the current identity and
    /// schedule one idle-boundary switch when less than ten percent remains.
    pub(crate) fn observe_rate_limits(self: &Arc<Self>, snapshot: &RateLimitSnapshot) {
        let Some(account_id) = self
            .auth_manager
            .auth_cached()
            .and_then(|auth| auth.get_account_id())
        else {
            return;
        };
        let Some(used_percent) = quota_used_percent(snapshot) else {
            return;
        };
        let now = chrono::Utc::now();
        let Ok(mut quotas) = self.quota_by_account.lock() else {
            return;
        };
        quotas.insert(
            account_id.clone(),
            CachedQuota {
                used_percent,
                observed_at: now,
                is_weekly: snapshot.secondary.is_some(),
            },
        );
        drop(quotas);

        if !quota_requires_failover(used_percent)
            || !try_begin_auto_failover(&self.auto_failover_in_flight)
        {
            return;
        }
        let service = Arc::clone(self);
        tokio::spawn(async move {
            service.run_auto_failover(account_id).await;
            service.auto_failover_in_flight.store(false, Ordering::Release);
        });
    }

    async fn run_auto_failover(&self, source_account_id: String) {
        let mut running_turns = self.running_turns.clone();
        while *running_turns.borrow() != 0 {
            if running_turns.changed().await.is_err() {
                return;
            }
        }
        let Ok(state) = self.registry.state() else {
            return;
        };
        let now = chrono::Utc::now();
        let target = {
            let Ok(quotas) = self.quota_by_account.lock() else {
                return;
            };
            select_auto_target(&state, &quotas, &self.vault, &source_account_id, now)
        };
        let Some(target) = target else {
            tracing::debug!(
                source_account_id,
                "automatic Marathon failover is parked: no fresh eligible target telemetry"
            );
            return;
        };
        // `switch` re-checks the running-turn count while holding the shared
        // auth-transition mutex, closing the race with a newly starting turn.
        // A parked UsageLimit continuation remains TUI-owned: its existing
        // auth-reload completion path takes and submits that value once, and
        // only after this switch has installed and verified the new identity.
        match self.switch(&target).await {
            Ok(response) if response.outcome == MarathonSwitchOutcome::Committed => {}
            Ok(response) => tracing::debug!(
                target,
                ?response.outcome,
                reason = ?response.reason,
                "automatic Marathon failover did not commit"
            ),
            Err(error) => tracing::warn!(
                target,
                %error,
                "automatic Marathon failover failed"
            ),
        }
    }

    pub(crate) fn status(&self) -> Result<MarathonStatusResponse, MarathonServiceError> {
        let state = self
            .registry
            .state()
            .map_err(|_| MarathonServiceError::StateUnavailable)?;
        let current_account_id = self
            .auth_manager
            .auth_cached()
            .and_then(|auth| auth.get_account_id());
        let active_turn_count = (*self.running_turns.borrow()).min(u32::MAX as usize) as u32;
        let now = chrono::Utc::now();
        let quotas = self.quota_by_account.lock().ok();
        let accounts = state
            .accounts
            .values()
            .map(|account| {
                let credential_ref = if account.credential_ref.is_empty() {
                    &account.id
                } else {
                    &account.credential_ref
                };
                let credential_present = self.vault.contains(credential_ref).unwrap_or(false);
                MarathonAccount {
                    account_id: account.id.clone(),
                    alias: account.alias.clone(),
                    active: state.active_account_id == account.id,
                    credential_present,
                    credential_health: credential_health_label(account.credential_health),
                    weekly_quota_remaining_percent: quotas
                        .as_ref()
                        .and_then(|quotas| quotas.get(&account.id))
                        .filter(|quota| quota.is_weekly)
                        .filter(|quota| {
                            quota.observed_at <= now
                                && now - quota.observed_at < AUTO_FAILOVER_TELEMETRY_TTL
                        })
                        .map(|quota| (100.0 - quota.used_percent).max(0.0)),
                    reset_action_available: false,
                }
            })
            .collect();
        let auto_reset = self
            .auto_reset
            .state()
            .map_err(|_| MarathonServiceError::StateUnavailable)?;
        Ok(MarathonStatusResponse {
            enabled: state.enabled,
            active_account_id: (!state.active_account_id.is_empty())
                .then_some(state.active_account_id),
            current_account_id,
            auth_generation: self.auth_generation.load(Ordering::Acquire),
            active_turn_count,
            accounts,
            auto_reset_enabled: auto_reset.enabled,
            auto_reset_phase: auto_reset_phase_label(auto_reset.phase).to_string(),
            auto_reset_last_error: (!auto_reset.last_error.is_empty())
                .then_some(auto_reset.last_error),
        })
    }

    pub(crate) async fn set_auto_reset_enabled(
        &self,
        enabled: bool,
    ) -> Result<MarathonAutoResetSetResponse, MarathonServiceError> {
        let _transition_guard = self.transition_mutex.lock().await;
        let state = self
            .auto_reset
            .set_enabled(enabled)
            .map_err(|_| MarathonServiceError::Persistence)?;
        Ok(MarathonAutoResetSetResponse {
            enabled: state.enabled,
            phase: auto_reset_phase_label(state.phase).to_string(),
        })
    }

    pub(crate) async fn set_enabled(
        &self,
        enabled: bool,
    ) -> Result<MarathonEnabledSetResponse, MarathonServiceError> {
        let _transition_guard = self.transition_mutex.lock().await;
        self.registry
            .set_enabled(enabled)
            .map_err(|_| MarathonServiceError::Persistence)?;
        Ok(MarathonEnabledSetResponse { enabled })
    }

    /// Capture the identity currently held by Codex's native AuthManager.
    ///
    /// This is the supported way to add an account from the TUI: the user
    /// completes the normal Codex login flow first, then this operation saves
    /// the resulting opaque snapshot under an operator-facing alias. No auth
    /// material crosses the app-server protocol boundary.
    pub(crate) async fn import_current(
        &self,
        alias: &str,
    ) -> Result<MarathonImportResponse, MarathonServiceError> {
        let _transition_guard = self.transition_mutex.lock().await;
        let state = self
            .registry
            .state()
            .map_err(|_| MarathonServiceError::StateUnavailable)?;
        let current_account_id = self
            .auth_manager
            .auth_cached()
            .and_then(|auth| auth.get_account_id())
            .ok_or(MarathonServiceError::AuthenticationRequired)?;
        // AccountRecord::new performs the canonical alias validation while
        // preserving the exact alias supplied by the user.
        let validated = codexmarathon_runtime::AccountRecord::new(&current_account_id, alias)
            .map_err(|_| MarathonServiceError::InvalidAlias)?;
        let mut account = state
            .accounts
            .get(&current_account_id)
            .cloned()
            .unwrap_or_else(|| validated.clone());
        if state
            .accounts
            .values()
            .any(|candidate| candidate.id != current_account_id && candidate.alias == alias)
        {
            return Err(MarathonServiceError::AliasConflict);
        }
        account.alias = validated.alias;
        account.credential_ref = current_account_id.clone();
        account.credential_health = CredentialHealth::Healthy;

        let source_snapshot = self
            .auth_manager
            .snapshot_for_transition(&current_account_id)
            .await
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let source_json = serde_json::to_vec(source_snapshot.as_auth_dot_json())
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let source_snapshot =
            codexmarathon_runtime::AuthSnapshot::for_account(&current_account_id, source_json)
                .map_err(|_| MarathonServiceError::NativeTransition)?;
        let previous_snapshot = self.vault.load(&current_account_id).ok();
        self.vault
            .save(&current_account_id, &source_snapshot)
            .map_err(|_| MarathonServiceError::Persistence)?;

        let previous_record = state.accounts.get(&current_account_id).cloned();
        if let Err(error) = self.registry.upsert(account.clone()) {
            rollback_import(
                &self.registry,
                &self.vault,
                &current_account_id,
                previous_record.as_ref(),
                previous_snapshot.as_ref(),
            );
            let _ = error;
            return Err(MarathonServiceError::Persistence);
        }
        if let Err(error) = self.registry.set_active(&current_account_id) {
            rollback_import(
                &self.registry,
                &self.vault,
                &current_account_id,
                previous_record.as_ref(),
                previous_snapshot.as_ref(),
            );
            let _ = error;
            return Err(MarathonServiceError::Persistence);
        }

        Ok(MarathonImportResponse {
            account_id: current_account_id,
            alias: account.alias,
            active: true,
            replaced: previous_snapshot.is_some(),
        })
    }

    pub(crate) async fn switch(
        &self,
        target: &str,
    ) -> Result<MarathonSwitchResponse, MarathonServiceError> {
        let _transition_guard = self.transition_mutex.lock().await;
        let state = self
            .registry
            .state()
            .map_err(|_| MarathonServiceError::StateUnavailable)?;
        let active_turn_count = (*self.running_turns.borrow()).min(u32::MAX as usize) as u32;
        let auth_generation = self.auth_generation.load(Ordering::Acquire);
        if let Some((outcome, reason)) = switch_admission(state.enabled, active_turn_count) {
            return Ok(MarathonSwitchResponse {
                account_id: None,
                outcome,
                auth_generation,
                active_turn_count,
                reason: Some(reason.to_string()),
            });
        }

        let target_account = resolve_target(&state.accounts, target)?;
        let current_account_id = self
            .auth_manager
            .auth_cached()
            .and_then(|auth| auth.get_account_id())
            .ok_or(MarathonServiceError::AuthenticationRequired)?;
        if current_account_id == target_account.id {
            self.registry
                .set_active(&target_account.id)
                .map_err(|_| MarathonServiceError::Persistence)?;
            return Ok(MarathonSwitchResponse {
                account_id: Some(target_account.id),
                outcome: MarathonSwitchOutcome::Committed,
                auth_generation,
                active_turn_count,
                reason: Some("account is already active".into()),
            });
        }

        // Capture any refreshed source credentials through the guarded native
        // AuthManager API before loading the target snapshot.
        let source_snapshot = self
            .auth_manager
            .snapshot_for_transition(&current_account_id)
            .await
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let source_json = serde_json::to_vec(source_snapshot.as_auth_dot_json())
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let source_snapshot =
            codexmarathon_runtime::AuthSnapshot::for_account(&current_account_id, source_json)
                .map_err(|_| MarathonServiceError::NativeTransition)?;
        let source_credential_ref = state
            .accounts
            .get(&current_account_id)
            .map(|account| {
                if account.credential_ref.is_empty() {
                    account.id.clone()
                } else {
                    account.credential_ref.clone()
                }
            })
            .unwrap_or_else(|| current_account_id.clone());
        self.vault
            .save(&source_credential_ref, &source_snapshot)
            .map_err(|_| MarathonServiceError::Persistence)?;

        let target_credential_ref = if target_account.credential_ref.is_empty() {
            target_account.id.as_str()
        } else {
            target_account.credential_ref.as_str()
        };
        let target_snapshot =
            self.vault
                .load(target_credential_ref)
                .map_err(|error| match error {
                    DomainError::SnapshotNotFound => MarathonServiceError::CredentialsUnavailable,
                    _ => MarathonServiceError::Persistence,
                })?;
        let target_auth: AuthDotJson = serde_json::from_slice(target_snapshot.bytes())
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let target_auth = AuthTransitionSnapshot::from_auth_dot_json(target_auth)
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        if target_auth.account_id() != target_account.id {
            return Err(MarathonServiceError::NativeTransition);
        }

        let status = self
            .auth_manager
            .install_snapshot_for_transition(target_auth, &current_account_id)
            .await
            .map_err(|_| MarathonServiceError::NativeTransition)?;
        let changed = match status {
            AuthReloadStatus::Reloaded { changed } => changed,
            AuthReloadStatus::Failed => return Err(MarathonServiceError::NativeTransition),
        };
        let observed = self
            .auth_manager
            .auth_cached()
            .and_then(|auth| auth.get_account_id());
        if observed.as_deref() != Some(target_account.id.as_str()) {
            return Err(MarathonServiceError::NativeTransition);
        }
        // The service still owns the shared transition mutex while the
        // account-bound model transport caches are invalidated.
        self.thread_manager
            .invalidate_model_transport_caches()
            .await;
        self.registry
            .set_active(&target_account.id)
            .map_err(|_| MarathonServiceError::Persistence)?;
        let auth_generation = if changed {
            self.auth_generation.fetch_add(1, Ordering::AcqRel) + 1
        } else {
            self.auth_generation.load(Ordering::Acquire)
        };
        Ok(MarathonSwitchResponse {
            account_id: Some(target_account.id),
            outcome: MarathonSwitchOutcome::Committed,
            auth_generation,
            active_turn_count,
            reason: None,
        })
    }
}

fn try_begin_auto_failover(in_flight: &AtomicBool) -> bool {
    !in_flight.swap(true, Ordering::AcqRel)
}

fn quota_requires_failover(used_percent: f64) -> bool {
    used_percent > AUTO_FAILOVER_USED_PERCENT
}

fn quota_used_percent(snapshot: &RateLimitSnapshot) -> Option<f64> {
    // Secondary is the longer (normally weekly) provider window. Values are
    // percentage consumed, so <10 remaining is the strict >90 boundary.
    let used = snapshot
        .secondary
        .as_ref()
        .or(snapshot.primary.as_ref())?
        .used_percent;
    used.is_finite().then_some(used.clamp(0.0, 100.0))
}

fn select_auto_target(
    state: &codexmarathon_runtime::RegistryState,
    quotas: &std::collections::BTreeMap<String, CachedQuota>,
    vault: &FileSnapshotVault,
    source_account_id: &str,
    now: chrono::DateTime<chrono::Utc>,
) -> Option<String> {
    if !state.enabled {
        return None;
    }
    state
        .accounts
        .values()
        .filter(|account| account.id != source_account_id)
        .filter(|account| account.credential_health == CredentialHealth::Healthy)
        .filter(|account| {
            let credential_ref = if account.credential_ref.is_empty() {
                account.id.as_str()
            } else {
                account.credential_ref.as_str()
            };
            vault.contains(credential_ref).unwrap_or(false)
        })
        .filter_map(|account| {
            let quota = quotas.get(&account.id)?;
            (quota.observed_at <= now
                && now - quota.observed_at < AUTO_FAILOVER_TELEMETRY_TTL
                && quota.used_percent <= AUTO_FAILOVER_USED_PERCENT)
                .then_some((account.id.clone(), quota.used_percent))
        })
        .min_by(|left, right| {
            left.1
                .total_cmp(&right.1)
                .then_with(|| left.0.cmp(&right.0))
        })
        .map(|(account_id, _)| account_id)
}

fn resolve_target(
    accounts: &std::collections::BTreeMap<String, codexmarathon_runtime::AccountRecord>,
    target: &str,
) -> Result<codexmarathon_runtime::AccountRecord, MarathonServiceError> {
    if target.trim().is_empty() {
        return Err(MarathonServiceError::AccountNotFound);
    }
    if let Some(account) = accounts.get(target) {
        return Ok(account.clone());
    }
    let matches: Vec<_> = accounts
        .values()
        .filter(|account| account.alias == target)
        .collect();
    match matches.as_slice() {
        [account] => Ok((*account).clone()),
        [] => Err(MarathonServiceError::AccountNotFound),
        _ => Err(MarathonServiceError::AmbiguousAlias),
    }
}

fn credential_health_label(health: CredentialHealth) -> String {
    match health {
        CredentialHealth::Unknown => "unknown",
        CredentialHealth::Healthy => "healthy",
        CredentialHealth::Stale => "stale",
        CredentialHealth::Invalid => "invalid",
    }
    .to_string()
}

fn auto_reset_phase_label(phase: codexmarathon_runtime::AutoResetPhase) -> &'static str {
    match phase {
        codexmarathon_runtime::AutoResetPhase::Idle => "idle",
        codexmarathon_runtime::AutoResetPhase::Attempting => "attempting",
        codexmarathon_runtime::AutoResetPhase::Succeeded => "succeeded",
        codexmarathon_runtime::AutoResetPhase::Blocked => "blocked",
    }
}

fn rollback_import(
    registry: &FileAccountRegistry,
    vault: &FileSnapshotVault,
    account_id: &str,
    previous_record: Option<&codexmarathon_runtime::AccountRecord>,
    previous_snapshot: Option<&codexmarathon_runtime::AuthSnapshot>,
) {
    if let Some(previous_record) = previous_record {
        let _ = registry.upsert(previous_record.clone());
    } else {
        let _ = registry.remove(account_id, true);
    }
    if let Some(previous_snapshot) = previous_snapshot {
        let _ = vault.save(account_id, previous_snapshot);
    } else {
        let _ = vault.delete(account_id);
    }
}

fn switch_admission(
    enabled: bool,
    active_turn_count: u32,
) -> Option<(MarathonSwitchOutcome, &'static str)> {
    if !enabled {
        Some((MarathonSwitchOutcome::Rejected, "Marathon is disabled"))
    } else if active_turn_count != 0 {
        Some((
            MarathonSwitchOutcome::Deferred,
            "wait for active turns to finish before switching accounts",
        ))
    } else {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Utc;
    use tempfile::tempdir;

    fn rate_limits(primary: f64, secondary: Option<f64>) -> RateLimitSnapshot {
        let window = |used_percent| codex_protocol::protocol::RateLimitWindow {
            used_percent,
            window_minutes: Some(10_080),
            resets_at: None,
        };
        RateLimitSnapshot {
            limit_id: Some("codex".to_string()),
            limit_name: None,
            primary: Some(window(primary)),
            secondary: secondary.map(window),
            credits: None,
            individual_limit: None,
            spend_control_reached: None,
            plan_type: None,
            rate_limit_reached_type: None,
        }
    }

    #[test]
    fn failover_starts_only_below_ten_percent_remaining() {
        assert_eq!(
            quota_used_percent(&rate_limits(1.0, Some(89.9))),
            Some(89.9)
        );
        assert_eq!(
            quota_used_percent(&rate_limits(1.0, Some(90.0))),
            Some(90.0)
        );
        assert_eq!(
            quota_used_percent(&rate_limits(90.0, None)),
            Some(90.0)
        );
        assert!(!quota_requires_failover(90.0));
        assert!(quota_requires_failover(90.1));
    }

    #[test]
    fn repeated_threshold_observations_start_exactly_one_transition() {
        let in_flight = AtomicBool::new(false);
        assert!(try_begin_auto_failover(&in_flight));
        assert!(!try_begin_auto_failover(&in_flight));
        assert!(!try_begin_auto_failover(&in_flight));
    }

    #[test]
    fn auto_target_requires_fresh_healthy_credentials_and_prefers_capacity() {
        let directory = tempdir().expect("tempdir");
        let vault = FileSnapshotVault::new(directory.path().join("vault"));
        let mut state = codexmarathon_runtime::RegistryState::default();
        for (id, health) in [
            ("account-a", CredentialHealth::Healthy),
            ("account-b", CredentialHealth::Healthy),
            ("account-c", CredentialHealth::Healthy),
            ("account-d", CredentialHealth::Invalid),
        ] {
            let mut account = codexmarathon_runtime::AccountRecord::new(id, "").expect("account");
            account.credential_health = health;
            state.accounts.insert(id.to_string(), account);
            let snapshot = codexmarathon_runtime::AuthSnapshot::for_account(id, b"{}")
                .expect("snapshot");
            vault.save(id, &snapshot).expect("vault save");
        }
        let mut no_credentials =
            codexmarathon_runtime::AccountRecord::new("account-e", "").expect("account");
        no_credentials.credential_health = CredentialHealth::Healthy;
        state
            .accounts
            .insert(no_credentials.id.clone(), no_credentials);
        let now = Utc::now();
        let quotas = std::collections::BTreeMap::from([
            (
                "account-a".to_string(),
                CachedQuota {
                    used_percent: 90.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
            (
                "account-b".to_string(),
                CachedQuota {
                    used_percent: 40.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
            (
                "account-c".to_string(),
                CachedQuota {
                    used_percent: 20.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
            (
                "account-d".to_string(),
                CachedQuota {
                    used_percent: 0.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
            (
                "account-e".to_string(),
                CachedQuota {
                    used_percent: 0.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
        ]);
        assert_eq!(
            select_auto_target(&state, &quotas, &vault, "account-a", now).as_deref(),
            Some("account-c")
        );

        let stale = now - AUTO_FAILOVER_TELEMETRY_TTL;
        let quotas = std::collections::BTreeMap::from([
            (
                "account-a".to_string(),
                CachedQuota {
                    used_percent: 90.0,
                    observed_at: now,
                    is_weekly: true,
                },
            ),
            (
                "account-b".to_string(),
                CachedQuota {
                    used_percent: 20.0,
                    observed_at: stale,
                    is_weekly: true,
                },
            ),
        ]);
        assert_eq!(
            select_auto_target(&state, &quotas, &vault, "account-a", now),
            None
        );
    }

    #[test]
    fn switching_is_deferred_while_a_turn_is_active() {
        assert_eq!(
            switch_admission(true, 1),
            Some((
                MarathonSwitchOutcome::Deferred,
                "wait for active turns to finish before switching accounts"
            ))
        );
        assert_eq!(switch_admission(true, 0), None);
    }

    #[test]
    fn disabled_service_rejects_before_account_resolution() {
        assert_eq!(
            switch_admission(false, 0),
            Some((MarathonSwitchOutcome::Rejected, "Marathon is disabled"))
        );
    }

    #[test]
    fn alias_resolution_requires_one_unambiguous_match() {
        let mut accounts = std::collections::BTreeMap::new();
        for id in ["account-a", "account-b"] {
            let mut account =
                codexmarathon_runtime::AccountRecord::new(id, "same").expect("account");
            account.updated_at = Utc::now();
            accounts.insert(id.to_string(), account);
        }
        assert!(matches!(
            resolve_target(&accounts, "same"),
            Err(MarathonServiceError::AmbiguousAlias)
        ));
        assert_eq!(
            resolve_target(&accounts, "account-a")
                .expect("id target")
                .id,
            "account-a"
        );
    }

    #[test]
    fn enabled_state_is_persisted_without_auth_material() {
        let directory = tempdir().expect("tempdir");
        let registry = FileAccountRegistry::new(directory.path().join("accounts.json"));
        assert!(registry.enabled().expect("default state"));
        registry.set_enabled(false).expect("disable");
        assert!(!registry.enabled().expect("persisted state"));
        let contents = std::fs::read_to_string(registry.path()).expect("registry");
        assert!(!contents.contains("access_token"));
        assert!(!contents.contains("refresh_token"));
    }
}
