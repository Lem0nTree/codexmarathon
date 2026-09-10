//! Native Marathon account service owned by app-server.
//!
//! This service composes the existing AuthManager, ThreadManager and turn
//! watcher with Marathon's secret-free registry and opaque snapshot vault. It
//! is intentionally a small manual-control surface: it reports status,
//! enables/disables Marathon, and performs an explicit account switch at an
//! idle boundary. Automatic recovery is outside this service.

use codex_app_server_protocol::{
    MarathonAccount, MarathonAutoResetSetResponse, MarathonEnabledSetResponse,
    MarathonImportResponse, MarathonStatusResponse, MarathonSwitchOutcome, MarathonSwitchResponse,
};
use codex_core::ThreadManager;
use codex_login::{AuthDotJson, AuthManager, AuthReloadStatus, AuthTransitionSnapshot};
use codexmarathon_runtime::{
    AccountStore, AutoResetStore, CredentialHealth, DomainError, FileAccountRegistry,
    FileSnapshotVault, MarathonConfig, SnapshotVault,
};
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use thiserror::Error;
use tokio::sync::{Mutex, watch};

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
                    weekly_quota_remaining_percent: None,
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
        assert!(!registry.enabled().expect("default state"));
        registry.set_enabled(true).expect("enable");
        assert!(registry.enabled().expect("persisted state"));
        let contents = std::fs::read_to_string(registry.path()).expect("registry");
        assert!(!contents.contains("access_token"));
        assert!(!contents.contains("refresh_token"));
    }
}
