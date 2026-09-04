//! The in-process CodexMarathon runtime bridge.
//!
//! CodexMarathon vendors the pinned Codext workspace under `codex-rs/` and
//! keeps the controller protocol adapter in the parent `runtime/` tree.  This
//! crate is the small, typed seam between those two pieces.  A Codex runtime
//! integration implements [`NativeCodexRuntime`] with its existing
//! authentication, turn, telemetry, transport, and recovery authorities, then
//! wraps it in [`EmbeddedRuntime`].
//!
//! The bridge deliberately does not own credentials or duplicate Codex state.
//! It never opens `auth.json` independently of the native `AuthManager`,
//! invents a turn counter, or creates a second recovery queue. Those
//! operations remain in the imported Codex runtime and are supplied through
//! the hook trait.

use codexmarathon_runtime_adapter::{
    AdapterError, BackendError, BackendIdentity, BackendRateLimits, BackendReload, CodextBackend,
};
use codexmarathon_runtime_adapter::protocol::{
    AuthTransitionParams, NativeAuthSnapshotResult, NativeLoginParams, NativeLoginResult,
    NativeRefreshParams, NativeRefreshResult, RateLimitSnapshot, RecoveryReleaseParams,
    RecoveryLifecycleEvent, RecoveryReleaseResult, RuntimeEvent, TransitionResult,
};
use codexmarathon_runtime_adapter::RuntimeAdapter;
use std::future::Future;
use std::pin::Pin;

mod native;

pub use native::{CodexNativeRuntime, NativeAuthConfig};
pub use codexmarathon_runtime_adapter::server::{RuntimeServer, ServerConfig};

/// Future returned by the native Codex/TUI recovery command seam.
pub type NativeRecoveryReleaseFuture = Pin<
    Box<dyn Future<Output = Result<RecoveryReleaseResult, BackendError>> + Send>,
>;

/// Native composition hook for releasing Codext's session-owned parked
/// `UsageLimitExceeded` continuation.
///
/// The hook is intentionally command-shaped rather than prompt-shaped. A
/// production implementation must deliver the command to the existing TUI
/// input flow and return the TUI's acknowledgement; it must not enqueue a
/// second recovery turn or reconstruct the prompt.
pub trait NativeRecoveryBridge: Send + Sync {
    fn release(&self, params: RecoveryReleaseParams) -> NativeRecoveryReleaseFuture;

    /// Drain lifecycle observations emitted by the TUI's native recovery
    /// owner. This synchronous, non-command seam lets the runtime register a
    /// parked recovery before a controller release arrives; it never creates
    /// or replays a prompt.
    fn drain_events(&self) -> Vec<RecoveryLifecycleEvent> {
        Vec::new()
    }
}

/// The native authorities that the embedded Codex runtime already owns.
///
/// Implementations should forward each method to the corresponding Codex
/// component. `reload_auth_from_storage` performs the native AuthManager
/// storage read, while [`Self::invalidate_model_transports`] owns the
/// account-bound transport cache. The bridge invokes the latter only after a
/// changed reload and returns only after both native operations complete. This
/// ordering is what lets the adapter prove that the first request after a
/// transition does not reuse Account A's transport.
pub trait NativeCodexRuntime {
    /// Return the account identity currently held by the native AuthManager.
    fn auth_identity(&self) -> BackendIdentity;

    /// Return the authoritative number of running turns from Codex.
    fn active_turn_count(&self) -> u32;

    /// Reload native authentication from the active storage source.
    fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError>;

    /// Atomically perform the native reload and account-bound transport
    /// invalidation while the runtime's auth-transition lock is held.
    ///
    /// The default preserves the small test seam for runtimes whose reload
    /// implementation already owns the same critical section. A production
    /// Codex authority should override this method when a turn-start lock must
    /// span both operations.
    fn reload_auth_and_invalidate(&mut self) -> Result<BackendReload, BackendError> {
        let reload = self.reload_auth_from_storage()?;
        if reload.changed {
            self.invalidate_model_transports()?;
        }
        Ok(reload)
    }

    /// Invalidate cached model transports after a changed auth reload.
    ///
    /// The default is suitable for a native runtime that already performs
    /// invalidation as part of `reload_auth_from_storage`; runtimes that keep
    /// the cache in a separate component must override this method.
    fn invalidate_model_transports(&mut self) -> Result<(), BackendError> {
        Ok(())
    }

    /// Read the native account rate-limit response without exposing auth
    /// material to the controller protocol.
    fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError>;

    /// Run one native Codex login flow and return its opaque auth snapshot.
    /// The caller is responsible for storing the snapshot in protected
    /// account storage; this trait must never log or journal it.
    fn login_account(
        &mut self,
        _params: NativeLoginParams,
    ) -> Result<NativeLoginResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "account login is not wired to Codex",
        ))
    }

    /// Read the active opaque auth snapshot from Codex's existing
    /// AuthManager. The controller uses this only to synchronize Account A's
    /// refreshed credentials before deploying another account.
    fn read_auth_snapshot(&mut self) -> Result<NativeAuthSnapshotResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "auth snapshot read is not wired to Codex",
        ))
    }

    /// Refresh one opaque account snapshot using Codex's existing native
    /// AuthManager/token authority path.
    fn refresh_account(
        &mut self,
        _params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "account refresh is not wired to Codex",
        ))
    }

    /// Release Codex's existing parked `UsageLimitExceeded` recovery turn.
    /// The Codex UI/core owns the prompt, thread queue, and submission; this
    /// hook is only an authorization seam for the controller. A concrete
    /// app composition must connect it to that native queue before exposing
    /// automatic recovery as operational.
    fn release_recovery(
        &mut self,
        _params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "recovery release is not wired to Codex",
        ))
    }

    /// Drain native lifecycle observations without creating a second queue.
    fn drain_recovery_events(&mut self) -> Vec<RecoveryLifecycleEvent> {
        Vec::new()
    }
}

/// Adapter backend that delegates every authority to a native Codex runtime.
pub struct NativeBackend<R> {
    runtime: R,
}

impl<R> NativeBackend<R> {
    /// Wrap a native runtime implementation without taking ownership of any
    /// credential or session data outside that implementation.
    pub fn new(runtime: R) -> Self {
        Self { runtime }
    }

    /// Borrow the native runtime for inspection.
    pub fn runtime(&self) -> &R {
        &self.runtime
    }

    /// Borrow the native runtime mutably for event forwarding or lifecycle
    /// integration.
    pub fn runtime_mut(&mut self) -> &mut R {
        &mut self.runtime
    }
}

impl<R: NativeCodexRuntime> CodextBackend for NativeBackend<R> {
    fn identity(&self) -> BackendIdentity {
        self.runtime.auth_identity()
    }

    fn active_turn_count(&self) -> u32 {
        self.runtime.active_turn_count()
    }

    fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError> {
        self.runtime.reload_auth_from_storage()
    }

    fn reload_auth_and_invalidate(&mut self) -> Result<BackendReload, BackendError> {
        self.runtime.reload_auth_and_invalidate()
    }

    fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError> {
        self.runtime.read_rate_limits()
    }

    fn login_account(
        &mut self,
        params: NativeLoginParams,
    ) -> Result<NativeLoginResult, BackendError> {
        self.runtime.login_account(params)
    }

    fn read_auth_snapshot(&mut self) -> Result<NativeAuthSnapshotResult, BackendError> {
        self.runtime.read_auth_snapshot()
    }

    fn refresh_account(
        &mut self,
        params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, BackendError> {
        self.runtime.refresh_account(params)
    }

    fn release_recovery(
        &mut self,
        params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, BackendError> {
        self.runtime.release_recovery(params)
    }

    fn drain_recovery_events(&mut self) -> Vec<RecoveryLifecycleEvent> {
        self.runtime.drain_recovery_events()
    }
}

/// A single-process CodexMarathon runtime.
///
/// The controller can drive this value over an in-process call boundary today;
/// a future named-pipe or Unix-socket listener can use the same
/// [`RuntimeAdapter::handle_frame`] and event-drain methods without introducing
/// another runtime process.
pub struct EmbeddedRuntime<R> {
    adapter: RuntimeAdapter<NativeBackend<R>>,
}

impl<R: NativeCodexRuntime> EmbeddedRuntime<R> {
    /// Construct an embedded runtime with the supplied runtime identifier.
    pub fn new(runtime: R, runtime_id: impl Into<String>) -> Result<Self, AdapterError> {
        Ok(Self {
            adapter: RuntimeAdapter::new(NativeBackend::new(runtime), runtime_id)?,
        })
    }

    /// Construct an embedded runtime with an explicit adapter configuration.
    pub fn with_config(
        runtime: R,
        runtime_id: impl Into<String>,
        config: codexmarathon_runtime_adapter::AdapterConfig,
    ) -> Result<Self, AdapterError> {
        Ok(Self {
            adapter: RuntimeAdapter::with_config(NativeBackend::new(runtime), runtime_id, config)?,
        })
    }

    /// Borrow the protocol adapter.
    pub fn adapter(&self) -> &RuntimeAdapter<NativeBackend<R>> {
        &self.adapter
    }

    /// Borrow the protocol adapter mutably.
    pub fn adapter_mut(&mut self) -> &mut RuntimeAdapter<NativeBackend<R>> {
        &mut self.adapter
    }

    /// Move the embedded adapter into the authenticated-local IPC server.
    /// The server retains transition/identity state across reconnects while
    /// resetting only per-connection protocol negotiation.
    pub fn into_server(self, config: ServerConfig) -> RuntimeServer<NativeBackend<R>> {
        RuntimeServer::new(self.adapter, config)
    }

    /// Decode and handle one JSON-RPC frame.
    pub fn handle_frame(&mut self, frame: &[u8]) -> Result<Vec<u8>, AdapterError> {
        self.adapter.handle_frame(frame)
    }

    /// Observe the native turn authority and emit a safe-boundary event when
    /// a prepared transition becomes commit-safe.
    pub fn observe_turn_count(&mut self) -> Result<bool, AdapterError> {
        self.adapter.observe_turn_count()
    }

    /// Forward a complete native rate-limit response to the controller.
    pub fn forward_rate_limits(&mut self, snapshot: BackendRateLimits) -> Result<(), AdapterError> {
        self.adapter.forward_rate_limits_snapshot(snapshot)
    }

    /// Read the native account rate limits and enqueue a complete snapshot.
    pub fn read_and_forward_rate_limits(&mut self) -> Result<(), AdapterError> {
        self.adapter.read_and_forward_rate_limits()
    }

    /// Forward a sparse native rate-limit update to the controller.
    pub fn forward_rate_limit_update(
        &mut self,
        snapshot: RateLimitSnapshot,
    ) -> Result<(), AdapterError> {
        self.adapter.forward_rate_limits_update(snapshot)
    }

    /// Forward a native turn-start lifecycle observation.
    pub fn forward_turn_started(
        &mut self,
        turn_id: impl Into<String>,
        thread_id: Option<String>,
    ) -> Result<(), AdapterError> {
        self.adapter.forward_turn_started(turn_id, thread_id)
    }

    /// Forward a native turn-completed lifecycle observation.
    pub fn forward_turn_completed(
        &mut self,
        turn_id: impl Into<String>,
        thread_id: Option<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
    ) -> Result<(), AdapterError> {
        self.adapter
            .forward_turn_completed(turn_id, thread_id, outcome, error_code)
    }

    /// Forward a native recovery-parked observation.  The recovery queue
    /// remains owned by the embedded Codex runtime; this only emits the
    /// controller-facing lifecycle notification.
    pub fn forward_recovery_parked(
        &mut self,
        recovery_id: impl Into<String>,
        reason: impl Into<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter.forward_recovery_parked(recovery_id, reason)
    }

    /// Forward a parked recovery with the native thread/turn correlation used
    /// by exactly-once release and queued-input ordering checks.
    pub fn forward_recovery_parked_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        reason: impl Into<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
        source_account_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter.forward_recovery_parked_with_context(
            recovery_id,
            reason,
            thread_id,
            turn_id,
            source_account_id,
        )
    }

    /// Forward a native recovery-started observation.
    pub fn forward_recovery_started(
        &mut self,
        recovery_id: impl Into<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter.forward_recovery_started(recovery_id)
    }

    /// Forward a recovery-started observation with native correlation.
    pub fn forward_recovery_started_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter
            .forward_recovery_started_with_context(recovery_id, thread_id, turn_id)
    }

    /// Forward a native recovery-completed observation.
    pub fn forward_recovery_completed(
        &mut self,
        recovery_id: impl Into<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter
            .forward_recovery_completed(recovery_id, outcome, error_code)
    }

    /// Forward a recovery-completed observation with native correlation.
    pub fn forward_recovery_completed_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        self.adapter.forward_recovery_completed_with_context(
            recovery_id,
            outcome,
            error_code,
            thread_id,
            turn_id,
        )
    }

    /// Return and clear queued runtime events.
    pub fn drain_events(&mut self) -> Vec<RuntimeEvent> {
        self.adapter.drain_events()
    }

    /// Return and clear queued event notifications as JSON lines.
    pub fn drain_event_lines(&mut self) -> Result<Vec<Vec<u8>>, AdapterError> {
        self.adapter.drain_event_lines()
    }

    /// Drain lifecycle notifications emitted by the native Codex recovery
    /// owner. The listener invokes this before each incoming command.
    pub fn drain_native_recovery_events(&mut self) -> Result<usize, AdapterError> {
        self.adapter.drain_native_recovery_events()
    }

    /// Request a transition through the embedded adapter.
    pub fn prepare_transition(
        &mut self,
        params: AuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        self.adapter.prepare(params)
    }

    /// Commit a transition at the native safe boundary.
    pub fn commit_transition(
        &mut self,
        params: AuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        self.adapter.commit(params)
    }

    /// Authorize release of a native Codex recovery turn. This never creates
    /// a prompt or queues a duplicate continuation.
    pub fn release_recovery(
        &mut self,
        params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, AdapterError> {
        self.adapter.release_recovery(params)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use codexmarathon_runtime_adapter::protocol::{
        AuthTransitionParams, RateLimitSnapshot, RateLimitWindow,
    };

    #[derive(Default)]
    struct FakeRuntime {
        account_id: Option<String>,
        turns: u32,
        reloads: u32,
        invalidations: u32,
    }

    impl NativeCodexRuntime for FakeRuntime {
        fn auth_identity(&self) -> BackendIdentity {
            BackendIdentity::new(self.account_id.clone())
        }

        fn active_turn_count(&self) -> u32 {
            self.turns
        }

        fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError> {
            self.reloads += 1;
            self.account_id = Some("account-b".to_string());
            Ok(BackendReload {
                identity: self.auth_identity(),
                changed: true,
            })
        }

        fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError> {
            Ok(BackendRateLimits {
                rate_limits: RateLimitSnapshot {
                    limit_id: Some("codex".to_string()),
                    limit_name: None,
                    plan_type: None,
                    rate_limit_reached_type: None,
                    primary: Some(RateLimitWindow {
                        used_percent: 1.0,
                        window_duration_mins: Some(300),
                        resets_at: Some(1_725_454_800),
                    }),
                    secondary: None,
                },
                rate_limits_by_limit_id: None,
            })
        }

        fn invalidate_model_transports(&mut self) -> Result<(), BackendError> {
            self.invalidations += 1;
            Ok(())
        }
    }

    #[test]
    fn embedded_runtime_delegates_native_identity_and_transition() {
        let mut runtime = EmbeddedRuntime::new(
            FakeRuntime {
                account_id: Some("account-a".to_string()),
                ..Default::default()
            },
            "test-runtime",
        )
        .expect("runtime should construct");
        assert_eq!(
            runtime.adapter().identity().account_id.as_deref(),
            Some("account-a")
        );
        let prepared = runtime
            .prepare_transition(AuthTransitionParams {
                transition_id: "tx-1".to_string(),
                target_account_id: "account-b".to_string(),
                expected_generation: 2,
            })
            .expect("prepare");
        assert_eq!(prepared.outcome.to_string(), "committed");
        let committed = runtime
            .commit_transition(AuthTransitionParams {
                transition_id: "tx-1".to_string(),
                target_account_id: "account-b".to_string(),
                expected_generation: 2,
            })
            .expect("commit");
        assert_eq!(committed.account_id.as_deref(), Some("account-b"));
        assert_eq!(runtime.adapter().identity().auth_generation, 2);
        assert_eq!(runtime.adapter().backend().runtime().reloads, 1);
        assert_eq!(runtime.adapter().backend().runtime().invalidations, 1);
    }

    #[test]
    fn rate_limits_are_read_through_the_native_hook() {
        let mut runtime = EmbeddedRuntime::new(
            FakeRuntime {
                account_id: Some("account-a".to_string()),
                ..Default::default()
            },
            "test-runtime",
        )
        .expect("runtime should construct");
        runtime
            .read_and_forward_rate_limits()
            .expect("rate limits");
        let events = runtime.drain_events();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].base.event_type, "rate_limits_snapshot");
    }
}
