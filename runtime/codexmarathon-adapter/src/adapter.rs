//! The narrow runtime-side adapter state machine.
//!
//! `CodextBackend` is the integration seam.  A real Codext patch implements
//! it by delegating to the existing `AuthManager`, turn-watch manager, model
//! transport invalidation, account reader, and recovery event stream.  The
//! adapter itself never reads auth files, refreshes tokens, tears down model
//! transports, or constructs recovery prompts.

use crate::error::{AdapterError, BackendError};
use crate::framing::JsonLineCodec;
use crate::protocol::*;
use crate::telemetry::TelemetryForwarder;
use serde::de::DeserializeOwned;
use serde_json::Value;
use std::collections::{BTreeMap, HashSet, VecDeque};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

/// Identity metadata observed from Codext.  Auth material is intentionally
/// not representable here.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct BackendIdentity {
    pub account_id: Option<String>,
    pub auth_mode: Option<String>,
}

impl BackendIdentity {
    pub fn new(account_id: Option<String>) -> Self {
        Self {
            account_id,
            auth_mode: None,
        }
    }
}

/// Result of Codext's existing auth reload path.  `changed` must be the
/// native AuthManager outcome; the adapter does not infer it from file data.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BackendReload {
    pub identity: BackendIdentity,
    pub changed: bool,
}

/// A complete rate-limit read from Codext's account protocol.
#[derive(Clone, Debug, PartialEq)]
pub struct BackendRateLimits {
    pub rate_limits: RateLimitSnapshot,
    pub rate_limits_by_limit_id: Option<std::collections::BTreeMap<String, RateLimitSnapshot>>,
}

/// Implement this trait in the Codext integration patch.
///
/// The defaults make the standalone adapter useful for identity/transition
/// tests with a backend that only supplies those seams.  Production wiring
/// should implement `reload_auth_from_storage` and `read_rate_limits`.
pub trait CodextBackend {
    /// Return the identity currently observed by AuthManager.
    fn identity(&self) -> BackendIdentity;

    /// Return Codext's authoritative running-turn count.  The adapter does
    /// not maintain another counter.
    fn active_turn_count(&self) -> u32;

    /// Delegate to Codext's existing idle-safe auth reload and its existing
    /// model transport invalidation/config refresh path.  The method must
    /// return only after those native side effects have completed.
    fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "auth reload is not wired to Codext",
        ))
    }

    /// Complete the reload boundary, including account-bound transport
    /// invalidation.  Native backends may override this to hold their shared
    /// turn/auth lock across both operations; the default preserves the
    /// existing single-call backend contract.
    fn reload_auth_and_invalidate(&mut self) -> Result<BackendReload, BackendError> {
        self.reload_auth_from_storage()
    }

    /// Delegate to Codext's existing account/rateLimits/read implementation.
    fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "rate-limit read is not wired to Codext",
        ))
    }

    /// Delegate account login to Codex's native login/AuthManager path.
    ///
    /// The result is an opaque auth snapshot. Implementations must not log or
    /// persist it outside the controller's protected credential vault.
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
    /// `AuthManager`. This is used to synchronize refreshed Account A
    /// credentials before the controller deploys Account B.
    fn read_auth_snapshot(&mut self) -> Result<NativeAuthSnapshotResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "auth snapshot read is not wired to Codex",
        ))
    }

    /// Delegate account refresh to Codex's native AuthManager path.
    fn refresh_account(
        &mut self,
        _params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "account refresh is not wired to Codex",
        ))
    }

    /// Drain lifecycle observations from the native recovery owner.
    ///
    /// Codext owns the pending synthetic turn and emits these observations at
    /// the point where its own state changes.  The default keeps standalone
    /// adapter users source-compatible; production backends should return the
    /// native event stream without interpreting or replaying prompt content.
    fn drain_recovery_events(&mut self) -> Vec<RecoveryLifecycleEvent> {
        Vec::new()
    }

    /// Release one recovery turn already parked by Codext after a
    /// `UsageLimitExceeded` failure. The native runtime owns the queue,
    /// configured prompt, and thread; the adapter never constructs or sends
    /// a continuation itself. Implementations must make this operation
    /// idempotent by `recovery_id` so a lost response can be reconciled.
    fn release_recovery(
        &mut self,
        _params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, BackendError> {
        Err(BackendError::new(
            "backend_not_configured",
            "recovery release is not wired to Codex",
        ))
    }
}

#[derive(Clone)]
pub struct AdapterConfig {
    pub protocol_versions: Vec<u32>,
    pub initial_auth_generation: u64,
    pub capabilities: Vec<String>,
    pub now: Arc<dyn Fn() -> u64 + Send + Sync>,
    pub max_frame_bytes: usize,
}

impl Default for AdapterConfig {
    fn default() -> Self {
        Self {
            protocol_versions: vec![PROTOCOL_VERSION],
            initial_auth_generation: 1,
            capabilities: vec![
                "runtime_state".to_string(),
                "identity".to_string(),
                "auth_transition".to_string(),
                "safe_boundary".to_string(),
                "rate_limits".to_string(),
                "account_login".to_string(),
                "account_refresh".to_string(),
                "auth_snapshot_read".to_string(),
                "recovery_events".to_string(),
                "recovery_release".to_string(),
            ],
            now: Arc::new(unix_now_seconds),
            max_frame_bytes: crate::framing::DEFAULT_MAX_FRAME_BYTES,
        }
    }
}

impl AdapterConfig {
    pub fn with_clock<F>(mut self, clock: F) -> Self
    where
        F: Fn() -> u64 + Send + Sync + 'static,
    {
        self.now = Arc::new(clock);
        self
    }
}

#[derive(Clone, Debug)]
struct PendingState {
    params: AuthTransitionParams,
    commit_requested: bool,
    safe_boundary_emitted: bool,
}

/// A per-connection CodexMarathon adapter.
pub struct RuntimeAdapter<B> {
    backend: B,
    identity: RuntimeIdentity,
    pending: Option<PendingState>,
    protocol_versions: Vec<u32>,
    negotiated_version: Option<u32>,
    capabilities: Vec<String>,
    now: Arc<dyn Fn() -> u64 + Send + Sync>,
    codec: JsonLineCodec,
    last_active_turn_count: u32,
    events: VecDeque<RuntimeEvent>,
    seen_recovery_events: HashSet<(String, String)>,
    recoveries: BTreeMap<String, RecoveryState>,
    release_results: BTreeMap<String, RecoveryReleaseResult>,
    telemetry: TelemetryForwarder,
}

impl<B: CodextBackend> RuntimeAdapter<B> {
    pub fn new(backend: B, runtime_id: impl Into<String>) -> Result<Self, AdapterError> {
        Self::with_config(backend, runtime_id, AdapterConfig::default())
    }

    pub fn with_config(
        backend: B,
        runtime_id: impl Into<String>,
        config: AdapterConfig,
    ) -> Result<Self, AdapterError> {
        let runtime_id = runtime_id.into();
        if runtime_id.trim().is_empty() {
            return Err(AdapterError::Protocol(
                "runtime_id must not be empty".to_string(),
            ));
        }
        validate_versions(&config.protocol_versions)?;
        let max_frame_bytes = config.max_frame_bytes;
        let observed_identity = backend.identity();
        let active_turn_count = backend.active_turn_count();
        let identity = RuntimeIdentity {
            runtime_id,
            account_id: observed_identity.account_id,
            auth_generation: config.initial_auth_generation,
        };
        Ok(Self {
            backend,
            identity,
            pending: None,
            protocol_versions: config.protocol_versions,
            negotiated_version: None,
            capabilities: config.capabilities,
            now: config.now,
            codec: JsonLineCodec::new(max_frame_bytes)
                .map_err(|error| AdapterError::Framing(error.to_string()))?,
            last_active_turn_count: active_turn_count,
            events: VecDeque::new(),
            seen_recovery_events: HashSet::new(),
            recoveries: BTreeMap::new(),
            release_results: BTreeMap::new(),
            telemetry: TelemetryForwarder,
        })
    }

    pub fn backend(&self) -> &B {
        &self.backend
    }

    pub fn backend_mut(&mut self) -> &mut B {
        &mut self.backend
    }

    pub fn identity(&self) -> RuntimeIdentity {
        self.identity.clone()
    }

    pub fn negotiated_version(&self) -> Option<u32> {
        self.negotiated_version
    }

    pub fn protocol_ready(&self) -> bool {
        self.negotiated_version.is_some()
    }

    /// Reset only connection-scoped protocol state after an IPC disconnect.
    /// Runtime identity, auth generation, pending transition intent, and
    /// queued lifecycle events remain intact so a reconnect can negotiate
    /// again and reconcile the same in-flight operation.
    pub fn reset_connection(&mut self) {
        self.negotiated_version = None;
    }

    pub fn codec(&self) -> JsonLineCodec {
        self.codec
    }

    pub fn runtime_state(&mut self) -> Result<RuntimeState, AdapterError> {
        let active_turn_count = self.backend.active_turn_count();
        self.observe_turn_count_value(active_turn_count)?;
        Ok(RuntimeState {
            identity: self.identity.clone(),
            active_turn_count,
            pending_transition: self.pending.as_ref().map(|state| PendingTransition {
                transition_id: state.params.transition_id.clone(),
                target_account_id: state.params.target_account_id.clone(),
                expected_generation: state.params.expected_generation,
            }),
            recoveries: self.recoveries.values().cloned().collect(),
        })
    }

    pub fn pending_transition(&self) -> Option<PendingTransition> {
        self.pending.as_ref().map(|state| PendingTransition {
            transition_id: state.params.transition_id.clone(),
            target_account_id: state.params.target_account_id.clone(),
            expected_generation: state.params.expected_generation,
        })
    }

    /// Return and clear events emitted since the last drain.
    pub fn drain_events(&mut self) -> Vec<RuntimeEvent> {
        self.events.drain(..).collect()
    }

    /// Encode and return all currently queued event notifications.
    pub fn drain_event_lines(&mut self) -> Result<Vec<Vec<u8>>, AdapterError> {
        self.drain_events()
            .into_iter()
            .map(|event| {
                let notification = RpcNotification::event(&event)?;
                self.codec
                    .encode(&notification)
                    .map_err(|error| AdapterError::Framing(error.to_string()))
            })
            .collect()
    }

    /// Drain lifecycle observations produced by Codext and forward them into
    /// the controller-facing event queue.  This must run before a controller
    /// request is dispatched so a just-parked recovery is present in the
    /// adapter map before `recovery/release` or `runtime/state/read`.
    pub fn drain_native_recovery_events(&mut self) -> Result<usize, AdapterError> {
        let native_events = self.backend.drain_recovery_events();
        let mut forwarded = 0;
        for event in native_events {
            let accepted = match event {
                RecoveryLifecycleEvent::Parked(payload) => self
                    .forward_recovery_parked_with_context(
                        payload.recovery_id,
                        payload.reason,
                        payload.thread_id,
                        payload.turn_id,
                        payload.source_account_id,
                    )?,
                RecoveryLifecycleEvent::Started(payload) => self
                    .forward_recovery_started_with_context(
                        payload.recovery_id,
                        payload.thread_id,
                        payload.turn_id,
                    )?,
                RecoveryLifecycleEvent::Completed(payload) => self
                    .forward_recovery_completed_with_context(
                        payload.recovery_id,
                        payload.outcome,
                        payload.error_code,
                        payload.thread_id,
                        payload.turn_id,
                    )?,
            };
            forwarded += usize::from(accepted);
        }
        Ok(forwarded)
    }

    /// Handle one decoded controller request and return its JSON-RPC response.
    /// Domain rejections are successful RPC responses carrying a typed
    /// `TransitionResult`; malformed requests use a JSON-RPC error response.
    pub fn handle_request(&mut self, request: RpcRequest) -> RpcResponse {
        let id = request.id.clone();
        if let Err(message) = request.validate() {
            return RpcResponse::error(id, RpcError::new(-32600, message));
        }
        if self.negotiated_version.is_none() && request.method != METHOD_NEGOTIATE {
            return RpcResponse::error(
                id,
                RpcError::new(-32000, "protocol/negotiate must be the first request"),
            );
        }
        if self.negotiated_version.is_some() && request.method == METHOD_NEGOTIATE {
            return RpcResponse::error(
                id,
                RpcError::new(-32600, "protocol has already been negotiated"),
            );
        }
        match self.dispatch(request.method.as_str(), request.params) {
            Ok(result) => RpcResponse {
                jsonrpc: JSONRPC_VERSION.to_string(),
                id,
                result: Some(result),
                error: None,
            },
            Err(error) => RpcResponse::error(id, rpc_error(error)),
        }
    }

    /// Decode one newline-delimited request frame and encode its response.
    /// Invalid JSON is returned as an adapter error because JSON-RPC cannot
    /// correlate a parse failure with a request identifier.
    pub fn handle_frame(&mut self, frame: &[u8]) -> Result<Vec<u8>, AdapterError> {
        let request: RpcRequest = self
            .codec
            .decode(frame)
            .map_err(|error| AdapterError::Framing(error.to_string()))?;
        let response = self.handle_request(request);
        self.codec
            .encode(&response)
            .map_err(|error| AdapterError::Framing(error.to_string()))
    }

    fn dispatch(&mut self, method: &str, params: Value) -> Result<Value, AdapterError> {
        match method {
            METHOD_NEGOTIATE => self.negotiate(params),
            METHOD_GET_RUNTIME_STATE => {
                require_empty_params(&params)?;
                Ok(serde_json::to_value(self.runtime_state()?)?)
            }
            METHOD_GET_IDENTITY => {
                require_empty_params(&params)?;
                Ok(serde_json::to_value(self.identity())?)
            }
            METHOD_GET_AUTH_GENERATION => {
                require_empty_params(&params)?;
                Ok(serde_json::to_value(AuthGenerationResult {
                    auth_generation: self.identity.auth_generation,
                })?)
            }
            METHOD_LOGIN_ACCOUNT => {
                let params: NativeLoginParams = decode_params(params)?;
                Ok(serde_json::to_value(self.login_account(params)?)?)
            }
            METHOD_READ_AUTH_SNAPSHOT => {
                require_empty_params(&params)?;
                Ok(serde_json::to_value(self.read_auth_snapshot()?)?)
            }
            METHOD_REFRESH_ACCOUNT => {
                let params: NativeRefreshParams = decode_params(params)?;
                Ok(serde_json::to_value(self.refresh_account(params)?)?)
            }
            METHOD_RELEASE_RECOVERY => {
                let params: RecoveryReleaseParams = decode_params(params)?;
                Ok(serde_json::to_value(self.release_recovery(params)?)?)
            }
            METHOD_PREPARE_AUTH_TRANSITION => {
                let params: AuthTransitionParams = decode_params(params)?;
                Ok(serde_json::to_value(self.prepare(params)?)?)
            }
            METHOD_COMMIT_AUTH_TRANSITION => {
                let params: AuthTransitionParams = decode_params(params)?;
                Ok(serde_json::to_value(self.commit(params)?)?)
            }
            METHOD_CANCEL_AUTH_TRANSITION => {
                let params: CancelAuthTransitionParams = decode_params(params)?;
                Ok(serde_json::to_value(self.cancel(params)?)?)
            }
            _ => Err(AdapterError::Protocol(format!(
                "method not found: {method}"
            ))),
        }
    }

    fn login_account(
        &mut self,
        params: NativeLoginParams,
    ) -> Result<NativeLoginResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        let result = self.backend.login_account(params)?;
        result.validate().map_err(AdapterError::Protocol)?;
        Ok(result)
    }

    fn read_auth_snapshot(&mut self) -> Result<NativeAuthSnapshotResult, AdapterError> {
        let result = self.backend.read_auth_snapshot()?;
        result.validate().map_err(AdapterError::Protocol)?;
        Ok(result)
    }

    fn refresh_account(
        &mut self,
        params: NativeRefreshParams,
    ) -> Result<NativeRefreshResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        let requested_account_id = params.account_id.clone();
        let result = self.backend.refresh_account(params)?;
        result
            .validate_for(&requested_account_id)
            .map_err(AdapterError::Protocol)?;
        Ok(result)
    }

    fn negotiate(&mut self, params: Value) -> Result<Value, AdapterError> {
        let params: VersionNegotiationParams = decode_params(params)?;
        params.validate().map_err(AdapterError::InvalidParams)?;
        let Some(version) = self
            .protocol_versions
            .iter()
            .copied()
            .filter(|version| params.supported_versions.contains(version))
            .max()
        else {
            return Err(AdapterError::Protocol(
                "no common protocol version".to_string(),
            ));
        };
        self.negotiated_version = Some(version);
        let result = VersionNegotiationResult {
            protocol_version: version,
            server_versions: self.protocol_versions.clone(),
        };
        let payload = RuntimeReadyPayload {
            protocol_version: version,
            account_id: self.identity.account_id.clone(),
            capabilities: self.capabilities.clone(),
        };
        self.emit_event(
            EventBase::new(
                EVENT_RUNTIME_READY,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            &payload,
        )?;
        Ok(serde_json::to_value(result)?)
    }

    pub fn prepare(
        &mut self,
        params: AuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        // The controller sends the generation that the transition is meant
        // to install (current + 1), not the generation currently loaded.
        // Keeping this check here makes stale prepare commands harmless and
        // keeps the Rust seam aligned with the Go coordinator/fake runtime.
        let expected_generation = self
            .identity
            .auth_generation
            .checked_add(1)
            .ok_or_else(|| AdapterError::Protocol("auth_generation overflow".to_string()))?;
        if params.expected_generation != expected_generation {
            return Ok(self.rejected_result(
                &params,
                "stale_generation",
                format!(
                    "expected target generation {}, current generation {}",
                    params.expected_generation, expected_generation
                ),
            ));
        }
        if let Some(existing) = &self.pending {
            if existing.params == params {
                return Ok(self.accepted_result(&params));
            }
            return Ok(self.rejected_result(
                &params,
                "transition_in_progress",
                "another transition is already pending".to_string(),
            ));
        }
        self.pending = Some(PendingState {
            params: params.clone(),
            commit_requested: false,
            safe_boundary_emitted: false,
        });
        let active_turn_count = self.backend.active_turn_count();
        self.observe_turn_count_value(active_turn_count)?;
        Ok(self.accepted_result(&params))
    }

    pub fn commit(
        &mut self,
        params: AuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        let Some(existing) = self.pending.as_ref() else {
            return Ok(self.rejected_result(
                &params,
                "no_pending_transition",
                "no matching prepared transition exists".to_string(),
            ));
        };
        if existing.params.transition_id != params.transition_id {
            return Ok(self.rejected_result(
                &params,
                "transition_id_mismatch",
                "transition_id does not match the pending transition".to_string(),
            ));
        }
        if existing.params.target_account_id != params.target_account_id {
            return Ok(self.rejected_result(
                &params,
                "transition_target_mismatch",
                "target_account_id does not match the pending transition".to_string(),
            ));
        }
        let expected_generation = self
            .identity
            .auth_generation
            .checked_add(1)
            .ok_or_else(|| AdapterError::Protocol("auth_generation overflow".to_string()))?;
        if params.expected_generation != expected_generation
            || params.expected_generation != existing.params.expected_generation
        {
            return Ok(self.rejected_result(
                &params,
                "stale_generation",
                "auth generation no longer matches the pending transition".to_string(),
            ));
        }
        if let Some(pending) = self.pending.as_mut() {
            pending.commit_requested = true;
        }
        let active_turn_count = self.backend.active_turn_count();
        self.observe_turn_count_value(active_turn_count)?;
        if active_turn_count != 0 {
            return Ok(self.rejected_result(
                &params,
                "safe_boundary_pending",
                "Codext still has a running turn; commit remains pending".to_string(),
            ));
        }
        self.commit_at_safe_boundary(params)
    }

    fn commit_at_safe_boundary(
        &mut self,
        params: AuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        let previous_account_id = self.identity.account_id.clone();
        let old_generation = self.identity.auth_generation;
        self.emit_event(
            EventBase::new(
                EVENT_AUTH_RELOAD_STARTED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                old_generation,
                Some(params.transition_id.clone()),
            ),
            &AuthReloadStartedPayload {
                target_account_id: params.target_account_id.clone(),
            },
        )?;

        let reload = match self.backend.reload_auth_and_invalidate() {
            Ok(reload) => reload,
            Err(error) => {
                let code = error.code.clone();
                let message = error.message.clone();
                self.emit_event(
                    EventBase::new(
                        EVENT_AUTH_RELOAD_FAILED,
                        (self.now)(),
                        self.identity.runtime_id.clone(),
                        old_generation,
                        Some(params.transition_id.clone()),
                    ),
                    &AuthReloadFailedPayload {
                        error_code: code.clone(),
                        error_message: message.clone(),
                    },
                )?;
                self.pending = None;
                return Ok(self.rejected_result(&params, code.as_str(), message));
            }
        };

        let new_generation = if reload.changed {
            old_generation
                .checked_add(1)
                .ok_or_else(|| AdapterError::Protocol("auth_generation overflow".to_string()))?
        } else {
            old_generation
        };

        // A reload acknowledgement is not a successful transition unless the
        // native AuthManager now observes the exact account that was prepared.
        // Do this check before emitting the success event or clearing the
        // controller's intent.  A changed-but-wrong native identity is left
        // observable (including its generation) so the controller's
        // three-way reconciliation can classify the split brain rather than
        // treating an arbitrary reload as Account B.
        if reload.identity.account_id.as_deref() != Some(params.target_account_id.as_str()) {
            let observed_account_id = reload.identity.account_id.clone();
            self.identity.account_id = observed_account_id.clone();
            self.identity.auth_generation = new_generation;
            let message = format!(
                "native AuthManager observed account {:?}, expected {}",
                observed_account_id, params.target_account_id
            );
            self.emit_event(
                EventBase::new(
                    EVENT_AUTH_RELOAD_FAILED,
                    (self.now)(),
                    self.identity.runtime_id.clone(),
                    new_generation,
                    Some(params.transition_id.clone()),
                ),
                &AuthReloadFailedPayload {
                    error_code: "identity_mismatch".to_string(),
                    error_message: message.clone(),
                },
            )?;
            self.pending = None;
            return Ok(self.rejected_result(&params, "identity_mismatch", message));
        }
        self.identity.account_id = reload.identity.account_id.clone();
        self.identity.auth_generation = new_generation;
        self.emit_event(
            EventBase::new(
                EVENT_AUTH_RELOAD_SUCCEEDED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                new_generation,
                Some(params.transition_id.clone()),
            ),
            &AuthReloadSucceededPayload {
                account_id: self.identity.account_id.clone(),
                previous_account_id: previous_account_id.clone(),
                changed: reload.changed,
            },
        )?;
        if reload.changed {
            // The backend contract says the existing Codext invalidation and
            // account/config refresh have already returned at this point.
            self.emit_event(
                EventBase::new(
                    EVENT_IDENTITY_CHANGED,
                    (self.now)(),
                    self.identity.runtime_id.clone(),
                    new_generation,
                    Some(params.transition_id.clone()),
                ),
                &IdentityChangedPayload {
                    previous_account_id,
                    account_id: self.identity.account_id.clone(),
                },
            )?;
        }
        self.pending = None;
        Ok(TransitionResult {
            transition_id: params.transition_id,
            runtime_id: self.identity.runtime_id.clone(),
            // `expected_generation` is the target generation (`old + 1`) on
            // both prepare and commit. Keeping this field correlated with
            // the controller command lets the Go coordinator validate a
            // native commit acknowledgement without a second convention.
            expected_generation: Some(params.expected_generation),
            auth_generation: new_generation,
            outcome: TransitionOutcome::Committed,
            account_id: self.identity.account_id.clone(),
            error_code: None,
            error_message: None,
        })
    }

    pub fn cancel(
        &mut self,
        params: CancelAuthTransitionParams,
    ) -> Result<TransitionResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        let Some(existing) = self.pending.as_ref() else {
            return Ok(self.rejected_cancel_result(
                &params,
                "no_pending_transition",
                "no matching prepared transition exists".to_string(),
            ));
        };
        if existing.params.transition_id != params.transition_id {
            return Ok(self.rejected_cancel_result(
                &params,
                "transition_id_mismatch",
                "transition_id does not match the pending transition".to_string(),
            ));
        }
        let expected_generation = self
            .identity
            .auth_generation
            .checked_add(1)
            .ok_or_else(|| AdapterError::Protocol("auth_generation overflow".to_string()))?;
        if existing.params.expected_generation != params.expected_generation
            || params.expected_generation != expected_generation
        {
            return Ok(self.rejected_cancel_result(
                &params,
                "stale_generation",
                "auth generation no longer matches the pending transition".to_string(),
            ));
        }
        self.pending = None;
        Ok(TransitionResult {
            transition_id: params.transition_id,
            runtime_id: self.identity.runtime_id.clone(),
            expected_generation: Some(params.expected_generation),
            auth_generation: self.identity.auth_generation,
            outcome: TransitionOutcome::Committed,
            account_id: self.identity.account_id.clone(),
            error_code: None,
            error_message: None,
        })
    }

    /// Observe the authoritative Codext turn count and emit a boundary event
    /// exactly once for a prepared transition when it reaches zero.
    pub fn observe_turn_count(&mut self) -> Result<bool, AdapterError> {
        let active_turn_count = self.backend.active_turn_count();
        self.observe_turn_count_value(active_turn_count)
    }

    pub fn observe_turn_count_value(
        &mut self,
        active_turn_count: u32,
    ) -> Result<bool, AdapterError> {
        self.last_active_turn_count = active_turn_count;
        let Some(pending) = self.pending.as_ref() else {
            return Ok(false);
        };
        if active_turn_count != 0 || pending.safe_boundary_emitted {
            return Ok(false);
        }
        let transition_id = pending.params.transition_id.clone();
        self.emit_event(
            EventBase::new(
                EVENT_SAFE_BOUNDARY_REACHED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                Some(transition_id.clone()),
            ),
            &SafeBoundaryReachedPayload {
                reason: "running_turn_count_zero".to_string(),
                turn_id: None,
            },
        )?;
        if let Some(pending) = self.pending.as_mut() {
            // A second poll at the same boundary must not emit a duplicate.
            if pending.params.transition_id == transition_id {
                pending.safe_boundary_emitted = true;
            }
        }
        Ok(true)
    }

    pub fn forward_rate_limits_snapshot(
        &mut self,
        snapshot: BackendRateLimits,
    ) -> Result<(), AdapterError> {
        let event = self.telemetry.snapshot(
            EventBase::new(
                EVENT_RATE_LIMITS_SNAPSHOT,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            self.identity.account_id.clone(),
            snapshot.rate_limits,
            snapshot.rate_limits_by_limit_id,
        )?;
        self.push_event(event)
    }

    pub fn read_and_forward_rate_limits(&mut self) -> Result<(), AdapterError> {
        let snapshot = self.backend.read_rate_limits()?;
        self.forward_rate_limits_snapshot(snapshot)
    }

    pub fn forward_rate_limits_update(
        &mut self,
        rate_limits: RateLimitSnapshot,
    ) -> Result<(), AdapterError> {
        let event = self.telemetry.sparse_update(
            EventBase::new(
                EVENT_RATE_LIMITS_UPDATED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            self.identity.account_id.clone(),
            rate_limits,
        )?;
        self.push_event(event)
    }

    pub fn forward_turn_started(
        &mut self,
        turn_id: impl Into<String>,
        thread_id: Option<String>,
    ) -> Result<(), AdapterError> {
        let turn_id = turn_id.into();
        if turn_id.trim().is_empty() {
            return Err(AdapterError::Protocol("turn_id is required".to_string()));
        }
        self.emit_event(
            EventBase::new(
                EVENT_TURN_STARTED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            &TurnStartedPayload { turn_id, thread_id },
        )
    }

    pub fn forward_turn_completed(
        &mut self,
        turn_id: impl Into<String>,
        thread_id: Option<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
    ) -> Result<(), AdapterError> {
        let turn_id = turn_id.into();
        let outcome = outcome.into();
        if turn_id.trim().is_empty() || outcome.trim().is_empty() {
            return Err(AdapterError::Protocol(
                "turn_id and outcome are required".to_string(),
            ));
        }
        self.emit_event(
            EventBase::new(
                EVENT_TURN_COMPLETED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            &TurnCompletedPayload {
                turn_id,
                thread_id,
                outcome,
                error_code,
            },
        )
    }

    /// Forward a runtime-owned recovery observation.  Duplicate observations
    /// for the same event kind and recovery ID are suppressed so the
    /// controller sees one lifecycle event, while the recovery queue remains
    /// wholly owned by Codext.
    pub fn forward_recovery_parked(
        &mut self,
        recovery_id: impl Into<String>,
        reason: impl Into<String>,
    ) -> Result<bool, AdapterError> {
        self.forward_recovery_parked_with_context(recovery_id, reason, None, None, None)
    }

    /// Forward a parked recovery together with its native thread/turn
    /// correlation. Optional context preserves compatibility with older
    /// Codext event producers while allowing same-thread acceptance tests.
    pub fn forward_recovery_parked_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        reason: impl Into<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
        source_account_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        let recovery_id = recovery_id.into();
        let reason = reason.into();
        if recovery_id.trim().is_empty() || reason.trim().is_empty() {
            return Err(AdapterError::Protocol(
                "recovery_id and reason are required".to_string(),
            ));
        }
        if !self
            .seen_recovery_events
            .insert((EVENT_RECOVERY_PARKED.to_string(), recovery_id.clone()))
        {
            return Ok(false);
        }
        self.recoveries
            .entry(recovery_id.clone())
            .or_insert_with(|| RecoveryState {
                recovery_id: recovery_id.clone(),
                thread_id: thread_id.clone(),
                turn_id: turn_id.clone(),
                source_account_id: source_account_id.clone(),
                transition_id: None,
                target_account_id: None,
                expected_generation: None,
                phase: RecoveryPhase::Parked,
            });
        self.emit_event(
            EventBase::new(
                EVENT_RECOVERY_PARKED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                self.identity.auth_generation,
                None,
            ),
            &RecoveryParkedPayload {
                recovery_id,
                reason,
                thread_id,
                turn_id,
                source_account_id,
            },
        )?;
        Ok(true)
    }

    pub fn forward_recovery_started(
        &mut self,
        recovery_id: impl Into<String>,
    ) -> Result<bool, AdapterError> {
        self.forward_recovery_started_with_context(recovery_id, None, None)
    }

    /// Forward a native recovery-started observation with optional context.
    pub fn forward_recovery_started_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        let recovery_id = recovery_id.into();
        if recovery_id.trim().is_empty() {
            return Err(AdapterError::Protocol(
                "recovery_id is required".to_string(),
            ));
        }
        if !self
            .seen_recovery_events
            .insert((EVENT_RECOVERY_STARTED.to_string(), recovery_id.clone()))
        {
            return Ok(false);
        }
        let state = self
            .recoveries
            .entry(recovery_id.clone())
            .or_insert_with(|| RecoveryState::parked(recovery_id.clone()));
        if state.thread_id.is_none() {
            state.thread_id = thread_id.clone();
        }
        if state.turn_id.is_none() {
            state.turn_id = turn_id.clone();
        }
        state.phase = RecoveryPhase::Started;
        let event_thread_id = thread_id.or_else(|| state.thread_id.clone());
        let event_turn_id = turn_id.or_else(|| state.turn_id.clone());
        let event_auth_generation = state
            .expected_generation
            .unwrap_or(self.identity.auth_generation);
        let event_transition_id = state.transition_id.clone();
        self.emit_event(
            EventBase::new(
                EVENT_RECOVERY_STARTED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                event_auth_generation,
                event_transition_id,
            ),
            &RecoveryStartedPayload {
                recovery_id,
                thread_id: event_thread_id,
                turn_id: event_turn_id,
            },
        )?;
        Ok(true)
    }

    pub fn forward_recovery_completed(
        &mut self,
        recovery_id: impl Into<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
    ) -> Result<bool, AdapterError> {
        self.forward_recovery_completed_with_context(recovery_id, outcome, error_code, None, None)
    }

    /// Forward a native recovery-completed observation with optional context.
    pub fn forward_recovery_completed_with_context(
        &mut self,
        recovery_id: impl Into<String>,
        outcome: impl Into<String>,
        error_code: Option<String>,
        thread_id: Option<String>,
        turn_id: Option<String>,
    ) -> Result<bool, AdapterError> {
        let recovery_id = recovery_id.into();
        let outcome = outcome.into();
        if recovery_id.trim().is_empty() || outcome.trim().is_empty() {
            return Err(AdapterError::Protocol(
                "recovery_id and outcome are required".to_string(),
            ));
        }
        if !self
            .seen_recovery_events
            .insert((EVENT_RECOVERY_COMPLETED.to_string(), recovery_id.clone()))
        {
            return Ok(false);
        }
        let state = self
            .recoveries
            .entry(recovery_id.clone())
            .or_insert_with(|| RecoveryState::parked(recovery_id.clone()));
        if state.thread_id.is_none() {
            state.thread_id = thread_id.clone();
        }
        if state.turn_id.is_none() {
            state.turn_id = turn_id.clone();
        }
        state.phase = RecoveryPhase::Completed;
        let event_thread_id = thread_id.or_else(|| state.thread_id.clone());
        let event_turn_id = turn_id.or_else(|| state.turn_id.clone());
        let event_auth_generation = state
            .expected_generation
            .unwrap_or(self.identity.auth_generation);
        let event_transition_id = state.transition_id.clone();
        self.emit_event(
            EventBase::new(
                EVENT_RECOVERY_COMPLETED,
                (self.now)(),
                self.identity.runtime_id.clone(),
                event_auth_generation,
                event_transition_id,
            ),
            &RecoveryCompletedPayload {
                recovery_id,
                outcome,
                error_code,
                thread_id: event_thread_id,
                turn_id: event_turn_id,
            },
        )?;
        Ok(true)
    }

    /// Authorize the native runtime to release a parked recovery. This method
    /// never creates a prompt or submits a user turn. It validates the
    /// transition generation and preserves the first successful result so a
    /// repeated request after a lost acknowledgement cannot dispatch twice.
    pub fn release_recovery(
        &mut self,
        params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, AdapterError> {
        params.validate().map_err(AdapterError::InvalidParams)?;
        if let Some(previous) = self.release_results.get(&params.recovery_id) {
            return Ok(previous.clone());
        }
        if params.expected_generation != self.identity.auth_generation {
            return Ok(self.recovery_release_rejected(
                &params,
                "stale_generation",
                "runtime identity generation does not match the verified transition".to_string(),
            ));
        }
        let Some(existing) = self.recoveries.get(&params.recovery_id).cloned() else {
            return Ok(self.recovery_release_rejected(
                &params,
                "recovery_not_found",
                "no native parked recovery exists for this ID".to_string(),
            ));
        };
        if let Some(thread_id) = params.thread_id.as_deref()
            && existing
                .thread_id
                .as_deref()
                .is_some_and(|value| value != thread_id)
        {
            return Ok(self.recovery_release_rejected(
                &params,
                "thread_id_mismatch",
                "recovery belongs to a different Codex thread".to_string(),
            ));
        }
        if let Some(transition_id) = existing.transition_id.as_deref()
            && transition_id != params.transition_id
        {
            return Ok(self.recovery_release_rejected(
                &params,
                "transition_id_mismatch",
                "recovery is bound to a different transition".to_string(),
            ));
        }
        match existing.phase {
            RecoveryPhase::Started | RecoveryPhase::Completed | RecoveryPhase::Released => {
                let result = RecoveryReleaseResult {
                    recovery_id: params.recovery_id.clone(),
                    thread_id: existing.thread_id.clone(),
                    outcome: RecoveryReleaseOutcome::AlreadyReleased,
                    error_code: None,
                    error_message: None,
                };
                self.release_results
                    .insert(params.recovery_id.clone(), result.clone());
                return Ok(result);
            }
            RecoveryPhase::Parked => {}
        }
        let mut result = self.backend.release_recovery(params.clone())?;
        if result.recovery_id.is_empty() {
            result.recovery_id = params.recovery_id.clone();
        }
        if result.recovery_id != params.recovery_id {
            return Err(AdapterError::Protocol(
                "backend recovery release returned a different recovery_id".to_string(),
            ));
        }
        if result.thread_id.is_none() {
            result.thread_id = existing.thread_id.clone();
        }
        if matches!(
            result.outcome,
            RecoveryReleaseOutcome::Released | RecoveryReleaseOutcome::AlreadyReleased
        ) {
            if let Some(state) = self.recoveries.get_mut(&params.recovery_id) {
                state.phase = RecoveryPhase::Released;
                state.transition_id = Some(params.transition_id.clone());
                state.expected_generation = Some(params.expected_generation);
            }
            self.release_results
                .insert(params.recovery_id.clone(), result.clone());
        }
        Ok(result)
    }

    fn recovery_release_rejected(
        &self,
        params: &RecoveryReleaseParams,
        code: &str,
        message: String,
    ) -> RecoveryReleaseResult {
        RecoveryReleaseResult {
            recovery_id: params.recovery_id.clone(),
            thread_id: params.thread_id.clone(),
            outcome: RecoveryReleaseOutcome::Rejected,
            error_code: Some(code.to_string()),
            error_message: Some(message),
        }
    }

    fn emit_event<T: serde::Serialize>(
        &mut self,
        base: EventBase,
        payload: &T,
    ) -> Result<(), AdapterError> {
        let event = RuntimeEvent::new(base, payload)?;
        self.push_event(event)
    }

    fn push_event(&mut self, event: RuntimeEvent) -> Result<(), AdapterError> {
        event.validate().map_err(AdapterError::Protocol)?;
        self.events.push_back(event);
        Ok(())
    }

    fn accepted_result(&self, params: &AuthTransitionParams) -> TransitionResult {
        TransitionResult {
            transition_id: params.transition_id.clone(),
            runtime_id: self.identity.runtime_id.clone(),
            expected_generation: Some(params.expected_generation),
            auth_generation: self.identity.auth_generation,
            outcome: TransitionOutcome::Committed,
            account_id: self.identity.account_id.clone(),
            error_code: None,
            error_message: None,
        }
    }

    fn rejected_result(
        &self,
        params: &AuthTransitionParams,
        code: &str,
        message: String,
    ) -> TransitionResult {
        TransitionResult {
            transition_id: params.transition_id.clone(),
            runtime_id: self.identity.runtime_id.clone(),
            expected_generation: Some(params.expected_generation),
            auth_generation: self.identity.auth_generation,
            outcome: TransitionOutcome::Rejected,
            account_id: self.identity.account_id.clone(),
            error_code: Some(code.to_string()),
            error_message: Some(message),
        }
    }

    fn rejected_cancel_result(
        &self,
        params: &CancelAuthTransitionParams,
        code: &str,
        message: String,
    ) -> TransitionResult {
        TransitionResult {
            transition_id: params.transition_id.clone(),
            runtime_id: self.identity.runtime_id.clone(),
            expected_generation: Some(params.expected_generation),
            auth_generation: self.identity.auth_generation,
            outcome: TransitionOutcome::Rejected,
            account_id: self.identity.account_id.clone(),
            error_code: Some(code.to_string()),
            error_message: Some(message),
        }
    }
}

fn validate_versions(versions: &[u32]) -> Result<(), AdapterError> {
    if versions.is_empty() {
        return Err(AdapterError::Protocol(
            "protocol_versions must not be empty".to_string(),
        ));
    }
    for (index, version) in versions.iter().enumerate() {
        if *version == 0 {
            return Err(AdapterError::Protocol(
                "protocol versions must be positive".to_string(),
            ));
        }
        if versions[..index].contains(version) {
            return Err(AdapterError::Protocol(format!(
                "protocol version {version} is duplicated"
            )));
        }
    }
    Ok(())
}

fn decode_params<T: DeserializeOwned>(params: Value) -> Result<T, AdapterError> {
    serde_json::from_value(params).map_err(|error| AdapterError::InvalidParams(error.to_string()))
}

fn require_empty_params(params: &Value) -> Result<(), AdapterError> {
    if params
        .as_object()
        .map(|object| object.is_empty())
        .unwrap_or(false)
    {
        Ok(())
    } else {
        Err(AdapterError::InvalidParams(
            "params must be an empty object".to_string(),
        ))
    }
}

fn rpc_error(error: AdapterError) -> RpcError {
    match error {
        AdapterError::InvalidParams(message) => RpcError::new(-32602, message),
        AdapterError::Protocol(message) if message.starts_with("method not found:") => {
            RpcError::new(-32601, message)
        }
        AdapterError::Protocol(message) => RpcError::new(-32000, message),
        AdapterError::Backend(error) => RpcError::new(-32603, error.message),
        AdapterError::Serialization(error) => RpcError::new(-32603, error.to_string()),
        AdapterError::Framing(message) => RpcError::new(-32603, message),
        AdapterError::Io(error) => RpcError::new(-32603, error.to_string()),
    }
}

fn unix_now_seconds() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |duration| duration.as_secs())
}
