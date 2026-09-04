//! Native Codex authority implementation for the Marathon adapter.
//!
//! This module is deliberately a thin composition wrapper.  Authentication,
//! refresh, turn observation, and model transport invalidation all stay in
//! the imported Codex crates.  The wrapper only supplies the synchronous
//! `NativeCodexRuntime` calls required by the newline-delimited Marathon
//! adapter and keeps the shared Codex auth-transition lock across a reload and
//! its transport invalidation.

use crate::{NativeCodexRuntime, NativeRecoveryBridge};
use codex_backend_client::Client as BackendClient;
use codex_login::{
    AuthCredentialsStoreMode, AuthDotJson, AuthKeyringBackendKind, AuthManager,
    AuthReloadStatus, AuthRouteConfig, CLIENT_ID, ServerOptions, load_auth_dot_json, run_login_server,
    save_auth,
};
use codexmarathon_runtime_adapter::protocol::{
    NativeAuthSnapshotResult, NativeLoginParams, NativeLoginResult, NativeRefreshParams,
    NativeRefreshResult, RecoveryLifecycleEvent, RecoveryReleaseParams, RecoveryReleaseResult,
};
use codexmarathon_runtime_adapter::{
    BackendError, BackendIdentity, BackendRateLimits, BackendReload,
};
use codex_core::ThreadManager;
use codex_http_client::{HttpClientFactory, OutboundProxyPolicy};
use codex_protocol::protocol::{RateLimitSnapshot as NativeRateLimitSnapshot, RateLimitWindow as NativeRateLimitWindow};
use std::collections::BTreeMap;
use std::future::Future;
use std::path::PathBuf;
use std::sync::Arc;
use tokio::runtime::Handle;
use tokio::sync::{Mutex, watch};

/// Configuration used for isolated native login and refresh operations.
///
/// Login and refresh operate in a temporary `CODEX_HOME`, then return an
/// opaque snapshot to the controller.  This prevents a secondary account
/// operation from overwriting the active account's `auth.json`.
#[derive(Clone, Debug)]
pub struct NativeAuthConfig {
    /// The active Codex home.  It is used only as the source of resolved
    /// routing/workspace policy; account login and refresh use a temporary
    /// home below it.
    pub codex_home: PathBuf,
    pub forced_chatgpt_workspace_id: Option<Vec<String>>,
    pub chatgpt_base_url: Option<String>,
    pub auth_route_config: AuthRouteConfig,
    pub login_client_id: String,
    /// HTTP routing used by the native Codex backend client for rate-limit
    /// reads.  Keeping this factory from the composition root preserves
    /// proxy/cookie policy and avoids a second unauthenticated client.
    pub http_client_factory: HttpClientFactory,
}

impl NativeAuthConfig {
    pub fn new(codex_home: PathBuf, auth_route_config: AuthRouteConfig) -> Self {
        Self {
            codex_home,
            forced_chatgpt_workspace_id: None,
            chatgpt_base_url: None,
            auth_route_config,
            login_client_id: CLIENT_ID.to_string(),
            http_client_factory: HttpClientFactory::new(OutboundProxyPolicy::ReqwestDefault),
        }
    }

    /// Carry the effective Codex HTTP factory into the Marathon native rate
    /// limit reader.  The factory is cloneable and contains no credentials.
    pub fn with_http_client_factory(mut self, factory: HttpClientFactory) -> Self {
        self.http_client_factory = factory;
        self
    }
}

/// A production Marathon backend backed by the actual Codex authorities.
///
/// The `AuthManager` and `ThreadManager` are shared with the running Codex
/// process.  No replacement auth cache, turn counter, recovery queue, or
/// model client is created here.
pub struct CodexNativeRuntime {
    auth_manager: Arc<AuthManager>,
    thread_manager: Arc<ThreadManager>,
    running_turns: watch::Receiver<usize>,
    auth_transition_lock: Arc<Mutex<()>>,
    auth_config: NativeAuthConfig,
    runtime_handle: Handle,
    recovery_bridge: Option<Arc<dyn NativeRecoveryBridge>>,
}

impl CodexNativeRuntime {
    /// Construct the native authority bridge from components already owned by
    /// Codex's app-server/runtime composition root.
    pub fn from_parts(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        auth_transition_lock: Arc<Mutex<()>>,
        auth_config: NativeAuthConfig,
        runtime_handle: Handle,
    ) -> Self {
        Self::from_parts_with_recovery_bridge(
            auth_manager,
            thread_manager,
            running_turns,
            auth_transition_lock,
            auth_config,
            runtime_handle,
            None,
        )
    }

    /// Construct the bridge and attach the native TUI recovery command seam.
    ///
    /// Keeping this as a separate constructor preserves the small standalone
    /// test/runtime composition while making the production app-server wiring
    /// explicit: only the composition root can provide the command bridge.
    pub fn from_parts_with_recovery_bridge(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        auth_transition_lock: Arc<Mutex<()>>,
        auth_config: NativeAuthConfig,
        runtime_handle: Handle,
        recovery_bridge: Option<Arc<dyn NativeRecoveryBridge>>,
    ) -> Self {
        Self {
            auth_manager,
            thread_manager,
            running_turns,
            auth_transition_lock,
            auth_config,
            runtime_handle,
            recovery_bridge,
        }
    }

    /// Construct the bridge using the current Tokio runtime handle.
    pub fn from_current_runtime(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        auth_transition_lock: Arc<Mutex<()>>,
        auth_config: NativeAuthConfig,
    ) -> Result<Self, BackendError> {
        let runtime_handle = Handle::try_current().map_err(|_| {
            BackendError::new(
                "runtime_not_running",
                "native Codex runtime is not attached to a Tokio runtime",
            )
        })?;
        Ok(Self::from_parts(
            auth_manager,
            thread_manager,
            running_turns,
            auth_transition_lock,
            auth_config,
            runtime_handle,
        ))
    }

    /// Construct the native bridge on the current Tokio runtime and attach a
    /// TUI recovery command bridge.
    pub fn from_current_runtime_with_recovery_bridge(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        auth_transition_lock: Arc<Mutex<()>>,
        auth_config: NativeAuthConfig,
        recovery_bridge: Option<Arc<dyn NativeRecoveryBridge>>,
    ) -> Result<Self, BackendError> {
        let runtime_handle = Handle::try_current().map_err(|_| {
            BackendError::new(
                "runtime_not_running",
                "native Codex runtime is not attached to a Tokio runtime",
            )
        })?;
        Ok(Self::from_parts_with_recovery_bridge(
            auth_manager,
            thread_manager,
            running_turns,
            auth_transition_lock,
            auth_config,
            runtime_handle,
            recovery_bridge,
        ))
    }

    fn run_async<T, F>(&self, future: F) -> Result<T, BackendError>
    where
        T: Send + 'static,
        F: Future<Output = Result<T, BackendError>> + Send + 'static,
    {
        let handle = self.runtime_handle.clone();
        let join = std::thread::Builder::new()
            .name("codexmarathon-native-authority".to_string())
            .spawn(move || handle.block_on(future))
            .map_err(|_| {
                BackendError::new(
                    "native_authority_unavailable",
                    "failed to start native Codex authority task",
                )
            })?;
        join.join().map_err(|_| {
            BackendError::new(
                "native_authority_panic",
                "native Codex authority task terminated unexpectedly",
            )
        })?
    }

    fn current_turn_count(&self) -> u32 {
        (*self.running_turns.borrow()).min(u32::MAX as usize) as u32
    }

    fn identity_from_manager(auth_manager: &AuthManager) -> BackendIdentity {
        auth_manager
            .auth_cached()
            .map(|auth| BackendIdentity {
                account_id: auth.get_account_id(),
                auth_mode: Some(auth.api_auth_mode().to_string()),
            })
            .unwrap_or_default()
    }

    fn identity_from_snapshot(auth: &AuthDotJson) -> Option<String> {
        auth.tokens
            .as_ref()
            .and_then(|tokens| tokens.account_id.clone().or_else(|| {
                tokens.id_token.chatgpt_account_id.clone()
            }))
            .or_else(|| {
                auth.agent_identity.as_ref().and_then(|identity| match identity {
                    codex_login::AgentIdentityStorage::Jwt(_) => None,
                    codex_login::AgentIdentityStorage::Record(record) => {
                        Some(record.account_id.clone())
                    }
                })
            })
    }

    fn snapshot_metadata(auth: &AuthDotJson) -> BTreeMap<String, String> {
        let mut metadata = BTreeMap::new();
        if let Some(mode) = auth.auth_mode {
            metadata.insert("auth_mode".to_string(), mode.to_string());
        }
        if let Some(tokens) = auth.tokens.as_ref() {
            if let Some(email) = tokens.id_token.email.clone() {
                metadata.insert("email".to_string(), email);
            }
            if let Some(plan) = tokens.id_token.get_chatgpt_plan_type_raw() {
                metadata.insert("plan_type".to_string(), plan);
            }
        }
        metadata
    }

    async fn login_native(
        auth_config: NativeAuthConfig,
        params: NativeLoginParams,
    ) -> Result<NativeLoginResult, BackendError> {
        let temp_home = tempfile::tempdir().map_err(|_| {
            BackendError::new(
                "native_login_failed",
                "failed to allocate isolated Codex auth home",
            )
        })?;
        let mut options = ServerOptions::new(
            temp_home.path().to_path_buf(),
            auth_config.login_client_id,
            auth_config.forced_chatgpt_workspace_id,
            AuthCredentialsStoreMode::File,
            AuthKeyringBackendKind::default(),
            auth_config.auth_route_config,
        );
        // Native login opens the same browser callback flow as Codex CLI.  It
        // writes only to the isolated temporary home until the callback has
        // completed, so Account A cannot be replaced during Account B login.
        options.open_browser = true;
        let server = run_login_server(options).map_err(|_| {
            BackendError::new("native_login_failed", "failed to start Codex login server")
        })?;
        server
            .block_until_done_with_callback_result()
            .await
            .map_err(|_| BackendError::new("native_login_failed", "Codex login did not complete"))?;
        let auth = load_auth_dot_json(
            temp_home.path(),
            AuthCredentialsStoreMode::File,
            AuthKeyringBackendKind::default(),
        )
        .map_err(|_| BackendError::new("native_login_failed", "Codex login data was not saved"))?
        .ok_or_else(|| {
            BackendError::new("native_login_failed", "Codex login returned no auth snapshot")
        })?;
        let snapshot_account_id = Self::identity_from_snapshot(&auth);
        if let (Some(requested), Some(observed)) =
            (params.account_id.as_deref(), snapshot_account_id.as_deref())
            && requested != observed
        {
            return Err(BackendError::new(
                "account_identity_mismatch",
                "Codex login returned a different account identity",
            ));
        }
        let account_id = snapshot_account_id.or(params.account_id).ok_or_else(|| {
                BackendError::new(
                    "native_login_failed",
                    "Codex login returned no account identity",
                )
            })?;
        let auth_json = serde_json::to_value(&auth).map_err(|_| {
            BackendError::new("native_login_failed", "Codex auth snapshot could not be encoded")
        })?;
        Ok(NativeLoginResult {
            account_id,
            alias: params.alias,
            auth_json,
            metadata: Self::snapshot_metadata(&auth),
        })
    }

    async fn refresh_native(
        auth_config: NativeAuthConfig,
        params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, BackendError> {
        let requested_account_id = params.account_id;
        let auth: AuthDotJson = serde_json::from_value(params.auth_json).map_err(|_| {
            BackendError::new("native_refresh_failed", "auth snapshot is not valid Codex JSON")
        })?;
        if let Some(snapshot_account_id) = Self::identity_from_snapshot(&auth)
            && snapshot_account_id != requested_account_id
        {
            return Err(BackendError::new(
                "account_identity_mismatch",
                "auth snapshot belongs to a different account",
            ));
        }

        let temp_home = tempfile::tempdir().map_err(|_| {
            BackendError::new(
                "native_refresh_failed",
                "failed to allocate isolated Codex auth home",
            )
        })?;
        save_auth(
            temp_home.path(),
            &auth,
            AuthCredentialsStoreMode::File,
            AuthKeyringBackendKind::default(),
        )
        .map_err(|_| BackendError::new("native_refresh_failed", "auth snapshot could not be staged"))?;

        let manager = AuthManager::shared(
            temp_home.path().to_path_buf(),
            false,
            AuthCredentialsStoreMode::File,
            auth_config.forced_chatgpt_workspace_id,
            auth_config.chatgpt_base_url,
            AuthKeyringBackendKind::default(),
            auth_config.auth_route_config,
        )
        .await;
        manager.refresh_token_from_authority().await.map_err(|_| {
            BackendError::new("native_refresh_failed", "Codex token refresh failed")
        })?;
        let updated = load_auth_dot_json(
            temp_home.path(),
            AuthCredentialsStoreMode::File,
            AuthKeyringBackendKind::default(),
        )
        .map_err(|_| BackendError::new("native_refresh_failed", "refreshed auth was not saved"))?
        .ok_or_else(|| BackendError::new("native_refresh_failed", "refreshed auth is missing"))?;
        let Some(tokens) = updated.tokens else {
            return Err(BackendError::new(
                "native_refresh_failed",
                "Codex auth does not expose refreshable token data",
            ));
        };
        let account_id = tokens
            .account_id
            .clone()
            .or_else(|| tokens.id_token.chatgpt_account_id.clone());
        if account_id
            .as_deref()
            .is_some_and(|account_id| account_id != requested_account_id)
        {
            return Err(BackendError::new(
                "account_identity_mismatch",
                "refreshed auth belongs to a different account",
            ));
        }
        let result = NativeRefreshResult {
            access_token: (!tokens.access_token.is_empty()).then_some(tokens.access_token),
            id_token: (!tokens.id_token.raw_jwt.is_empty()).then_some(tokens.id_token.raw_jwt),
            refresh_token: (!tokens.refresh_token.is_empty()).then_some(tokens.refresh_token),
            account_id,
        };
        result.validate_for(&requested_account_id).map_err(|_| {
            BackendError::new(
                "native_refresh_failed",
                "Codex refresh returned no usable token fields",
            )
        })?;
        Ok(result)
    }

    async fn read_auth_snapshot_native(
        auth_manager: Arc<AuthManager>,
    ) -> Result<NativeAuthSnapshotResult, BackendError> {
        // Prefer AuthManager's cached native snapshot: this is the same
        // refresh-updated value used by Codex requests. Fall back to its
        // configured storage reader for hosts that have not materialized a
        // cached token-backed auth yet.
        let auth = auth_manager
            .auth_dot_json_cached()
            .or_else(|| auth_manager.auth_dot_json_from_storage().ok().flatten())
            .ok_or_else(|| {
                BackendError::new(
                    "auth_snapshot_unavailable",
                    "Codex AuthManager has no serializable auth snapshot",
                )
            })?;
        let account_id = Self::identity_from_snapshot(&auth)
            .or_else(|| auth_manager.auth_cached().and_then(|auth| auth.get_account_id()))
            .ok_or_else(|| {
                BackendError::new(
                    "auth_snapshot_unavailable",
                    "Codex auth snapshot has no account identity",
                )
            })?;
        let auth_json = serde_json::to_value(&auth).map_err(|_| {
            BackendError::new(
                "auth_snapshot_unavailable",
                "Codex auth snapshot could not be encoded",
            )
        })?;
        Ok(NativeAuthSnapshotResult {
            account_id,
            auth_json,
        })
    }

    #[allow(clippy::await_holding_lock)]
    async fn reload_and_invalidate_native(
        auth_manager: Arc<AuthManager>,
        thread_manager: Arc<ThreadManager>,
        running_turns: watch::Receiver<usize>,
        auth_transition_lock: Arc<Mutex<()>>,
    ) -> Result<BackendReload, BackendError> {
        let _guard = auth_transition_lock.lock().await;
        if *running_turns.borrow() != 0 {
            return Err(BackendError::new(
                "active_turns",
                "Codex has a running turn; auth reload remains pending",
            ));
        }
        let status = auth_manager.reload_with_status().await;
        let changed = match status {
            AuthReloadStatus::Reloaded { changed } => changed,
            AuthReloadStatus::Failed => {
                return Err(BackendError::new(
                    "auth_reload_failed",
                    "Codex AuthManager could not reload auth storage",
                ));
            }
        };
        if changed {
            // This is Codex's real per-thread model client cache invalidation;
            // it tears down account-bound transport state before the lock is
            // released and before a new turn can start.
            thread_manager.invalidate_model_transport_caches().await;
        }
        Ok(BackendReload {
            identity: Self::identity_from_manager(&auth_manager),
            changed,
        })
    }

    async fn read_rate_limits_native(
        auth_manager: Arc<AuthManager>,
        auth_config: NativeAuthConfig,
    ) -> Result<BackendRateLimits, BackendError> {
        let auth = auth_manager.auth().await.ok_or_else(|| {
            BackendError::new(
                "authentication_required",
                "Codex authentication is required to read rate limits",
            )
        })?;
        if !auth.uses_codex_backend() {
            return Err(BackendError::new(
                "rate_limits_unavailable",
                "Codex backend authentication is required to read rate limits",
            ));
        }
        let base_url = auth_config
            .chatgpt_base_url
            .unwrap_or_else(|| "https://chatgpt.com/backend-api".to_string());
        let client = BackendClient::from_auth(base_url, &auth, auth_config.http_client_factory);
        let response = client.get_rate_limits_with_reset_credits().await.map_err(|error| {
            BackendError::new("rate_limits_failed", format!("failed to fetch Codex rate limits: {error}"))
        })?;
        let mut snapshots = response.rate_limits.into_iter();
        let first = snapshots.next().ok_or_else(|| {
            BackendError::new(
                "rate_limits_empty",
                "Codex returned no rate-limit snapshots",
            )
        })?;
        let mut by_limit_id = std::collections::BTreeMap::new();
        let first_adapter = map_rate_limit_snapshot(first.clone());
        let first_id = first
            .limit_id
            .clone()
            .unwrap_or_else(|| "codex".to_string());
        by_limit_id.insert(first_id, first_adapter.clone());
        for snapshot in snapshots {
            let id = snapshot
                .limit_id
                .clone()
                .unwrap_or_else(|| "codex".to_string());
            by_limit_id.insert(id, map_rate_limit_snapshot(snapshot));
        }
        Ok(BackendRateLimits {
            rate_limits: first_adapter,
            rate_limits_by_limit_id: Some(by_limit_id),
        })
    }
}

impl NativeCodexRuntime for CodexNativeRuntime {
    fn auth_identity(&self) -> BackendIdentity {
        Self::identity_from_manager(&self.auth_manager)
    }

    fn active_turn_count(&self) -> u32 {
        self.current_turn_count()
    }

    fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError> {
        self.reload_auth_and_invalidate()
    }

    fn reload_auth_and_invalidate(&mut self) -> Result<BackendReload, BackendError> {
        let auth_manager = Arc::clone(&self.auth_manager);
        let thread_manager = Arc::clone(&self.thread_manager);
        let running_turns = self.running_turns.clone();
        let auth_transition_lock = Arc::clone(&self.auth_transition_lock);
        self.run_async(Self::reload_and_invalidate_native(
            auth_manager,
            thread_manager,
            running_turns,
            auth_transition_lock,
        ))
    }

    #[allow(clippy::await_holding_lock)]
    fn invalidate_model_transports(&mut self) -> Result<(), BackendError> {
        let thread_manager = Arc::clone(&self.thread_manager);
        let running_turns = self.running_turns.clone();
        let auth_transition_lock = Arc::clone(&self.auth_transition_lock);
        self.run_async(async move {
            let _guard = auth_transition_lock.lock().await;
            if *running_turns.borrow() != 0 {
                return Err(BackendError::new(
                    "active_turns",
                    "Codex has a running turn; transport invalidation is deferred",
                ));
            }
            thread_manager.invalidate_model_transport_caches().await;
            Ok(())
        })
    }

    fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError> {
		let auth_manager = Arc::clone(&self.auth_manager);
		let config = self.auth_config.clone();
		self.run_async(Self::read_rate_limits_native(auth_manager, config))
    }

    fn read_auth_snapshot(&mut self) -> Result<NativeAuthSnapshotResult, BackendError> {
        let auth_manager = Arc::clone(&self.auth_manager);
        self.run_async(Self::read_auth_snapshot_native(auth_manager))
    }

    fn login_account(
        &mut self,
        params: NativeLoginParams,
    ) -> Result<NativeLoginResult, BackendError> {
        let config = self.auth_config.clone();
        self.run_async(Self::login_native(config, params))
    }

    fn refresh_account(
        &mut self,
        params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, BackendError> {
        let config = self.auth_config.clone();
        self.run_async(Self::refresh_native(config, params))
    }

    fn release_recovery(
        &mut self,
        params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, BackendError> {
        let Some(bridge) = self.recovery_bridge.clone() else {
            return Err(BackendError::new(
                "recovery_bridge_unavailable",
                "Codex TUI recovery command bridge is not attached",
            ));
        };
        self.run_async(async move { bridge.release(params).await })
    }

    fn drain_recovery_events(&mut self) -> Vec<RecoveryLifecycleEvent> {
        self.recovery_bridge
            .as_ref()
            .map_or_else(Vec::new, |bridge| bridge.drain_events())
    }
}

fn map_rate_limit_snapshot(snapshot: NativeRateLimitSnapshot) -> codexmarathon_runtime_adapter::protocol::RateLimitSnapshot {
	codexmarathon_runtime_adapter::protocol::RateLimitSnapshot {
		limit_id: snapshot.limit_id,
		limit_name: None,
		plan_type: snapshot.plan_type.and_then(|value| enum_label(&value)),
		rate_limit_reached_type: snapshot
			.rate_limit_reached_type
			.and_then(|value| enum_label(&value)),
		primary: snapshot.primary.map(map_rate_limit_window),
		secondary: snapshot.secondary.map(map_rate_limit_window),
	}
}

fn map_rate_limit_window(window: NativeRateLimitWindow) -> codexmarathon_runtime_adapter::protocol::RateLimitWindow {
	codexmarathon_runtime_adapter::protocol::RateLimitWindow {
		used_percent: window.used_percent,
		window_duration_mins: window
			.window_minutes
			.and_then(|value| u64::try_from(value).ok()),
		resets_at: window.resets_at.and_then(|value| u64::try_from(value).ok()),
	}
}

fn enum_label<T: serde::Serialize>(value: &T) -> Option<String> {
	serde_json::to_value(value)
		.ok()
		.and_then(|value| value.as_str().map(str::to_string))
}
