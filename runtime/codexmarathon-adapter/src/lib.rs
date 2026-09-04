//! CodexMarathon's Rust runtime adapter seam.
//!
//! The crate is an internal workspace component of the embedded Codex runtime.
//! `codexmarathon-runtime` implements the production bridge while tests and
//! other integrations can implement [`CodextBackend`] directly. The adapter
//! never owns credentials, a second turn counter, or a recovery queue.

mod adapter;
mod error;
pub mod framing;
pub mod protocol;
pub mod server;
pub mod telemetry;

pub use adapter::{
    AdapterConfig, BackendIdentity, BackendRateLimits, BackendReload, CodextBackend,
    RuntimeAdapter,
};
pub use error::{AdapterError, BackendError};
pub use server::{RuntimeServer, ServerConfig};

#[cfg(test)]
mod tests {
    use super::*;
    use crate::framing::JsonLineCodec;
    use crate::protocol::*;
    use serde_json::{json, Value};
    use std::collections::{BTreeMap, VecDeque};
    use std::io::Cursor;
    use std::sync::{Arc, Mutex};

    #[derive(Clone)]
    struct FakeBackend {
        account_id: Option<String>,
        active_turn_count: u32,
        reload: Result<BackendReload, BackendError>,
        rate_limits: Option<BackendRateLimits>,
        login: Option<Result<NativeLoginResult, BackendError>>,
        auth_snapshot: Option<Result<NativeAuthSnapshotResult, BackendError>>,
        refresh: Option<Result<NativeRefreshResult, BackendError>>,
        recovery_release: Option<Result<RecoveryReleaseResult, BackendError>>,
        recovery_events: Arc<Mutex<VecDeque<RecoveryLifecycleEvent>>>,
        reload_calls: Arc<Mutex<u32>>,
        recovery_release_calls: Arc<Mutex<u32>>,
    }

    impl FakeBackend {
        fn new(account_id: &str, active_turn_count: u32) -> Self {
            Self {
                account_id: Some(account_id.to_string()),
                active_turn_count,
                reload: Ok(BackendReload {
                    identity: BackendIdentity::new(Some(account_id.to_string())),
                    changed: false,
                }),
                rate_limits: None,
                login: None,
                auth_snapshot: None,
                refresh: None,
                recovery_release: None,
                recovery_events: Arc::new(Mutex::new(VecDeque::new())),
                reload_calls: Arc::new(Mutex::new(0)),
                recovery_release_calls: Arc::new(Mutex::new(0)),
            }
        }
    }

    impl CodextBackend for FakeBackend {
        fn identity(&self) -> BackendIdentity {
            BackendIdentity::new(self.account_id.clone())
        }

        fn active_turn_count(&self) -> u32 {
            self.active_turn_count
        }

        fn reload_auth_from_storage(&mut self) -> Result<BackendReload, BackendError> {
            *self.reload_calls.lock().expect("test mutex poisoned") += 1;
            self.reload.clone()
        }

        fn read_rate_limits(&mut self) -> Result<BackendRateLimits, BackendError> {
            self.rate_limits.clone().ok_or_else(|| {
                BackendError::new("missing_rate_limits", "test rate limits are not configured")
            })
        }

        fn login_account(
            &mut self,
            _params: NativeLoginParams,
        ) -> Result<NativeLoginResult, BackendError> {
            self.login.clone().ok_or_else(|| {
                BackendError::new("missing_login", "test login is not configured")
            })?
        }

        fn refresh_account(
            &mut self,
            _params: NativeRefreshParams,
        ) -> Result<NativeRefreshResult, BackendError> {
            self.refresh.clone().ok_or_else(|| {
                BackendError::new("missing_refresh", "test refresh is not configured")
            })?
        }

        fn read_auth_snapshot(
            &mut self,
        ) -> Result<NativeAuthSnapshotResult, BackendError> {
            self.auth_snapshot.clone().ok_or_else(|| {
                BackendError::new("missing_snapshot", "test snapshot is not configured")
            })?
        }

        fn release_recovery(
            &mut self,
            params: RecoveryReleaseParams,
        ) -> Result<RecoveryReleaseResult, BackendError> {
            *self
                .recovery_release_calls
                .lock()
                .expect("test mutex poisoned") += 1;
            self.recovery_release
                .clone()
                .ok_or_else(|| BackendError::new("missing_recovery_release", "test recovery release is not configured"))
                .map(|mut result| {
                    if result.recovery_id.is_empty() {
                        result.recovery_id = params.recovery_id;
                    }
                    result
                })
        }

        fn drain_recovery_events(&mut self) -> Vec<RecoveryLifecycleEvent> {
            self.recovery_events
                .lock()
                .expect("test mutex poisoned")
                .drain(..)
                .collect()
        }
    }

    fn adapter(backend: FakeBackend) -> RuntimeAdapter<FakeBackend> {
        RuntimeAdapter::with_config(
            backend,
            "runtime-test",
            AdapterConfig::default().with_clock(|| 1_725_451_200),
        )
        .expect("adapter should construct")
    }

    fn transition(id: &str, target: &str, generation: u64) -> AuthTransitionParams {
        AuthTransitionParams {
            transition_id: id.to_string(),
            target_account_id: target.to_string(),
            expected_generation: generation,
        }
    }

    fn snapshot(limit_id: &str) -> RateLimitSnapshot {
        RateLimitSnapshot {
            limit_id: Some(limit_id.to_string()),
            limit_name: Some("Codex".to_string()),
            plan_type: Some("pro".to_string()),
            rate_limit_reached_type: None,
            primary: Some(RateLimitWindow {
                used_percent: 91.5,
                window_duration_mins: Some(300),
                resets_at: Some(1_725_454_800),
            }),
            secondary: None,
        }
    }

    #[test]
    fn framing_skips_blank_lines_and_round_trips_json() {
        let codec = JsonLineCodec::new(1024).expect("codec");
        let mut reader = Cursor::new(b"\n\r\n{\"value\":1}\n".to_vec());
        let frame = codec
            .read_frame(&mut reader)
            .expect("read frame")
            .expect("one frame");
        let value: Value = codec.decode(&frame).expect("decode JSON");
        assert_eq!(value, json!({"value": 1}));
        assert!(codec.read_frame(&mut reader).expect("read EOF").is_none());
        assert_eq!(codec.encode(&value).expect("encode"), b"{\"value\":1}\n");
    }

    #[test]
    fn negotiation_is_required_and_emits_runtime_ready() {
        let mut adapter = adapter(FakeBackend::new("account-a", 0));
        let before = adapter.handle_request(RpcRequest::new(
            "one",
            METHOD_GET_IDENTITY,
            json!({}),
        ));
        assert_eq!(before.error.expect("error").code, -32000);

        let response = adapter.handle_request(RpcRequest::new(
            "two",
            METHOD_NEGOTIATE,
            json!({"supported_versions": [1]}),
        ));
        assert_eq!(response.error, None);
        assert_eq!(response.result, Some(json!({
            "protocol_version": 1,
            "server_versions": [1]
        })));
        let events = adapter.drain_events();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].base.event_type, EVENT_RUNTIME_READY);
        let ready: RuntimeReadyPayload = events[0].payload_as().expect("ready payload");
        assert_eq!(ready.account_id.as_deref(), Some("account-a"));
    }

    #[test]
    fn native_account_handlers_delegate_opaque_login_and_refresh() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.login = Some(Ok(NativeLoginResult {
            account_id: "account-b".to_string(),
            alias: Some("work".to_string()),
            auth_json: json!({
                "auth_mode": "chatgpt",
                "tokens": {"access_token": "opaque-test-value"}
            }),
            metadata: BTreeMap::from([(String::from("source"), String::from("native"))]),
        }));
        backend.refresh = Some(Ok(NativeRefreshResult {
            access_token: Some("new-access".to_string()),
            id_token: None,
            refresh_token: Some("new-refresh".to_string()),
            account_id: Some("account-b".to_string()),
        }));
        let mut adapter = adapter(backend);
        let ready = adapter.handle_request(RpcRequest::new(
            "ready",
            METHOD_NEGOTIATE,
            json!({"supported_versions": [1]}),
        ));
        assert!(ready.error.is_none());

        let login = adapter.handle_request(RpcRequest::new(
            "login",
            METHOD_LOGIN_ACCOUNT,
            json!({"alias": "work"}),
        ));
        assert_eq!(login.error, None);
        let login_result = login.result.expect("login result");
        assert_eq!(login_result["account_id"], "account-b");
        assert_eq!(login_result["auth_json"]["tokens"]["access_token"], "opaque-test-value");

        let refresh = adapter.handle_request(RpcRequest::new(
            "refresh",
            METHOD_REFRESH_ACCOUNT,
            json!({
                "account_id": "account-b",
                "auth_json": {"auth_mode": "chatgpt"}
            }),
        ));
        assert_eq!(refresh.error, None);
        assert_eq!(refresh.result.expect("refresh result")["account_id"], "account-b");
    }

    #[test]
    fn native_account_handlers_reject_invalid_opaque_payloads() {
        let mut adapter = adapter(FakeBackend::new("account-a", 0));
        let _ = adapter.handle_request(RpcRequest::new(
            "ready",
            METHOD_NEGOTIATE,
            json!({"supported_versions": [1]}),
        ));
        let invalid = adapter.handle_request(RpcRequest::new(
            "refresh",
            METHOD_REFRESH_ACCOUNT,
            json!({"account_id": "account-b", "auth_json": "not-an-object"}),
        ));
        assert_eq!(invalid.error.expect("invalid params").code, -32602);
    }

    #[test]
    fn native_auth_snapshot_handler_delegates_opaque_value() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.auth_snapshot = Some(Ok(NativeAuthSnapshotResult {
            account_id: "account-a".to_string(),
            auth_json: json!({
                "auth_mode": "chatgpt",
                "tokens": {"account_id": "account-a", "access_token": "opaque"}
            }),
        }));
        let mut adapter = adapter(backend);
        let ready = adapter.handle_request(RpcRequest::new(
            "ready",
            METHOD_NEGOTIATE,
            json!({"supported_versions": [1]}),
        ));
        assert!(ready.error.is_none());

        let response = adapter.handle_request(RpcRequest::new(
            "snapshot",
            METHOD_READ_AUTH_SNAPSHOT,
            json!({}),
        ));
        assert_eq!(response.error, None);
        assert_eq!(
            response.result.expect("snapshot result")["account_id"],
            "account-a"
        );
    }

    #[test]
    fn running_turn_keeps_commit_pending_until_boundary() {
        let mut backend = FakeBackend::new("account-a", 1);
        backend.reload = Ok(BackendReload {
            identity: BackendIdentity::new(Some("account-b".to_string())),
            changed: true,
        });
        let calls = Arc::clone(&backend.reload_calls);
        let mut adapter = adapter(backend);
        let prepared = adapter
            .prepare(transition("tx-1", "account-b", 2))
            .expect("prepare");
        assert_eq!(prepared.outcome, TransitionOutcome::Committed);
        assert!(adapter.drain_events().is_empty());

        let pending_commit = adapter
            .commit(transition("tx-1", "account-b", 2))
            .expect("commit request");
        assert_eq!(pending_commit.outcome, TransitionOutcome::Rejected);
        assert_eq!(
            pending_commit.error_code.as_deref(),
            Some("safe_boundary_pending")
        );
        assert!(adapter.pending_transition().is_some());
        assert_eq!(*calls.lock().expect("test mutex poisoned"), 0);

        adapter.backend_mut().active_turn_count = 0;
        assert!(adapter.observe_turn_count().expect("boundary poll"));
        assert_eq!(adapter.observe_turn_count().expect("duplicate poll"), false);
        let events = adapter.drain_events();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].base.event_type, EVENT_SAFE_BOUNDARY_REACHED);
        assert_eq!(events[0].base.transition_id.as_deref(), Some("tx-1"));

        let committed = adapter
            .commit(transition("tx-1", "account-b", 2))
            .expect("commit after boundary");
        assert_eq!(committed.outcome, TransitionOutcome::Committed);
        assert_eq!(committed.expected_generation, Some(2));
        assert_eq!(committed.auth_generation, 2);
        assert_eq!(committed.account_id.as_deref(), Some("account-b"));
        assert!(adapter.pending_transition().is_none());
        assert_eq!(*calls.lock().expect("test mutex poisoned"), 1);
        let event_types: Vec<_> = adapter
            .drain_events()
            .into_iter()
            .map(|event| event.base.event_type)
            .collect();
        assert_eq!(
            event_types,
            vec![
                EVENT_AUTH_RELOAD_STARTED.to_string(),
                EVENT_AUTH_RELOAD_SUCCEEDED.to_string(),
                EVENT_IDENTITY_CHANGED.to_string()
            ]
        );
    }

    #[test]
    fn stale_generation_and_id_are_rejected_without_reload() {
        let backend = FakeBackend::new("account-a", 0);
        let calls = Arc::clone(&backend.reload_calls);
        let mut adapter = adapter(backend);
        adapter
            .prepare(transition("tx-1", "account-b", 2))
            .expect("prepare");
        let stale = adapter
            .commit(transition("tx-1", "account-b", 3))
            .expect("stale commit");
        assert_eq!(stale.outcome, TransitionOutcome::Rejected);
        assert_eq!(stale.error_code.as_deref(), Some("stale_generation"));
        let wrong_id = adapter
            .commit(transition("tx-2", "account-b", 2))
            .expect("wrong id commit");
        assert_eq!(wrong_id.outcome, TransitionOutcome::Rejected);
        assert_eq!(wrong_id.error_code.as_deref(), Some("transition_id_mismatch"));
        assert_eq!(*calls.lock().expect("test mutex poisoned"), 0);
        assert!(adapter.pending_transition().is_some());
    }

    #[test]
    fn reload_failure_preserves_identity_and_emits_failure() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.reload = Err(BackendError::new("reload_failed", "auth file unavailable"));
        let mut adapter = adapter(backend);
        adapter
            .prepare(transition("tx-1", "account-b", 2))
            .expect("prepare");
        adapter.drain_events();
        let result = adapter
            .commit(transition("tx-1", "account-b", 2))
            .expect("commit");
        assert_eq!(result.outcome, TransitionOutcome::Rejected);
        assert_eq!(result.error_code.as_deref(), Some("reload_failed"));
        assert_eq!(adapter.identity().account_id.as_deref(), Some("account-a"));
        assert_eq!(adapter.identity().auth_generation, 1);
        let events = adapter.drain_events();
        assert_eq!(events.len(), 2);
        assert_eq!(events[0].base.event_type, EVENT_AUTH_RELOAD_STARTED);
        assert_eq!(events[1].base.event_type, EVENT_AUTH_RELOAD_FAILED);
        assert!(adapter.pending_transition().is_none());
    }

    #[test]
    fn reload_with_wrong_native_identity_is_not_committed() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.reload = Ok(BackendReload {
            identity: BackendIdentity::new(Some("account-c".to_string())),
            changed: true,
        });
        let mut adapter = adapter(backend);
        adapter
            .prepare(transition("tx-wrong", "account-b", 2))
            .expect("prepare");
        adapter.drain_events();
        let result = adapter
            .commit(transition("tx-wrong", "account-b", 2))
            .expect("commit");
        assert_eq!(result.outcome, TransitionOutcome::Rejected);
        assert_eq!(result.error_code.as_deref(), Some("identity_mismatch"));
        assert_eq!(result.account_id.as_deref(), Some("account-c"));
        assert_eq!(adapter.identity().auth_generation, 2);
        assert!(adapter.pending_transition().is_none());
        let events = adapter.drain_events();
        assert_eq!(events.len(), 2);
        assert_eq!(events[1].base.event_type, EVENT_AUTH_RELOAD_FAILED);
    }

    #[test]
    fn telemetry_attaches_connection_identity_and_preserves_sparse_shape() {
        let mut backend = FakeBackend::new("account-a", 0);
        let mut by_id = BTreeMap::new();
        by_id.insert("codex".to_string(), snapshot("codex"));
        backend.rate_limits = Some(BackendRateLimits {
            rate_limits: snapshot("codex"),
            rate_limits_by_limit_id: Some(by_id),
        });
        let mut adapter = adapter(backend);
        adapter
            .read_and_forward_rate_limits()
            .expect("full rate limits");
        adapter
            .forward_rate_limits_update(snapshot("codex"))
            .expect("sparse rate limits");
        let events = adapter.drain_events();
        assert_eq!(events.len(), 2);
        let full: RateLimitsSnapshotPayload = events[0].payload_as().expect("full payload");
        let sparse: RateLimitsUpdatedPayload = events[1].payload_as().expect("sparse payload");
        assert_eq!(full.account_id.as_deref(), Some("account-a"));
        assert_eq!(sparse.account_id.as_deref(), Some("account-a"));
        assert_eq!(sparse.rate_limits.limit_id.as_deref(), Some("codex"));
    }

    #[test]
    fn recovery_observations_are_forwarded_once_per_stage() {
        let mut adapter = adapter(FakeBackend::new("account-a", 0));
        assert!(adapter
            .forward_recovery_parked("recovery-1", "unauthorized")
            .expect("parked"));
        assert!(!adapter
            .forward_recovery_parked("recovery-1", "duplicate")
            .expect("duplicate parked"));
        assert!(adapter
            .forward_recovery_started("recovery-1")
            .expect("started"));
        assert!(adapter
            .forward_recovery_completed("recovery-1", "completed", None)
            .expect("completed"));
        assert_eq!(adapter.drain_events().len(), 3);
    }

    #[test]
    fn recovery_release_is_idempotent_and_does_not_create_prompt() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.recovery_release = Some(Ok(RecoveryReleaseResult {
            recovery_id: String::new(),
            thread_id: Some("thread-1".to_string()),
            outcome: RecoveryReleaseOutcome::Released,
            error_code: None,
            error_message: None,
        }));
        let calls = Arc::clone(&backend.recovery_release_calls);
        let mut adapter = adapter(backend);
        assert!(adapter
            .forward_recovery_parked_with_context(
                "recovery-1",
                "usage_limit_exceeded",
                Some("thread-1".to_string()),
                Some("turn-1".to_string()),
                Some("account-a".to_string()),
            )
            .expect("parked"));
        let params = RecoveryReleaseParams {
            recovery_id: "recovery-1".to_string(),
            thread_id: Some("thread-1".to_string()),
            transition_id: "tx-1".to_string(),
            expected_generation: 1,
        };
        let first = adapter.release_recovery(params.clone()).expect("first release");
        assert_eq!(first.outcome, RecoveryReleaseOutcome::Released);
        let second = adapter.release_recovery(params).expect("idempotent release");
        assert_eq!(second.outcome, RecoveryReleaseOutcome::Released);
        assert_eq!(*calls.lock().expect("test mutex poisoned"), 1);
        assert!(adapter.drain_events().len() == 1, "release creates no synthetic event");
    }

    #[test]
    fn native_recovery_drain_registers_before_release_and_correlates_later_stages() {
        let mut backend = FakeBackend::new("account-a", 0);
        backend.recovery_release = Some(Ok(RecoveryReleaseResult {
            recovery_id: String::new(),
            thread_id: Some("thread-1".to_string()),
            outcome: RecoveryReleaseOutcome::Released,
            error_code: None,
            error_message: None,
        }));
        let recovery_events = Arc::clone(&backend.recovery_events);
        recovery_events
            .lock()
            .expect("test mutex poisoned")
            .push_back(RecoveryLifecycleEvent::Parked(RecoveryParkedPayload {
                recovery_id: "recovery-1".to_string(),
                reason: "usage_limit_exceeded".to_string(),
                thread_id: Some("thread-1".to_string()),
                turn_id: Some("failed-turn".to_string()),
                source_account_id: Some("account-a".to_string()),
            }));
        let mut adapter = adapter(backend);

        assert_eq!(adapter.drain_native_recovery_events().expect("parked drain"), 1);
        assert_eq!(
            adapter
                .runtime_state()
                .expect("runtime state")
                .recoveries[0]
                .phase,
            RecoveryPhase::Parked
        );
        let parked = adapter.drain_events();
        assert_eq!(parked.len(), 1);

        let release = adapter
            .release_recovery(RecoveryReleaseParams {
                recovery_id: "recovery-1".to_string(),
                thread_id: Some("thread-1".to_string()),
                transition_id: "tx-1".to_string(),
                expected_generation: 1,
            })
            .expect("release parked recovery");
        assert_eq!(release.outcome, RecoveryReleaseOutcome::Released);

        recovery_events
            .lock()
            .expect("test mutex poisoned")
            .extend([
                RecoveryLifecycleEvent::Started(RecoveryStartedPayload {
                    recovery_id: "recovery-1".to_string(),
                    thread_id: None,
                    turn_id: None,
                }),
                RecoveryLifecycleEvent::Completed(RecoveryCompletedPayload {
                    recovery_id: "recovery-1".to_string(),
                    outcome: "completed".to_string(),
                    error_code: None,
                    thread_id: None,
                    turn_id: None,
                }),
                // Simulate a replay after reconnect. The adapter must not
                // duplicate any stage already observed.
                RecoveryLifecycleEvent::Started(RecoveryStartedPayload {
                    recovery_id: "recovery-1".to_string(),
                    thread_id: None,
                    turn_id: None,
                }),
            ]);
        assert_eq!(adapter.drain_native_recovery_events().expect("later drain"), 2);
        let events = adapter.drain_events();
        assert_eq!(events.len(), 2);
        assert_eq!(events[0].base.event_type, EVENT_RECOVERY_STARTED);
        assert_eq!(events[0].base.auth_generation, 1);
        assert_eq!(events[0].base.transition_id.as_deref(), Some("tx-1"));
        let started: RecoveryStartedPayload = events[0].payload_as().expect("started payload");
        assert_eq!(started.thread_id.as_deref(), Some("thread-1"));
        assert_eq!(started.turn_id.as_deref(), Some("failed-turn"));
        assert_eq!(events[1].base.event_type, EVENT_RECOVERY_COMPLETED);
        assert_eq!(events[1].base.auth_generation, 1);
        assert_eq!(events[1].base.transition_id.as_deref(), Some("tx-1"));
        let state = adapter
            .runtime_state()
            .expect("runtime state after completion")
            .recoveries
            .into_iter()
            .find(|state| state.recovery_id == "recovery-1")
            .expect("recovery state");
        assert_eq!(state.phase, RecoveryPhase::Completed);
        assert_eq!(state.transition_id.as_deref(), Some("tx-1"));
        assert_eq!(state.expected_generation, Some(1));
    }

    #[test]
    fn event_round_trip_keeps_unknown_fields() {
        let raw = json!({
            "event_type": "rate_limits_updated",
            "occurred_at": 1_725_451_200u64,
            "runtime_id": "runtime-test",
            "auth_generation": 3,
            "account_id": "account-a",
            "rateLimits": {
                "limitId": "codex",
                "limitName": null,
                "planType": "pro",
                "rateLimitReachedType": null,
                "primary": null,
                "secondary": null,
                "futureField": true
            },
            "futureEventField": {"kept": true}
        });
        let event: RuntimeEvent = serde_json::from_value(raw).expect("decode event");
        assert!(event.payload.get("futureEventField").is_some());
        let encoded = serde_json::to_value(event).expect("encode event");
        assert_eq!(encoded["futureEventField"]["kept"], true);
        assert_eq!(encoded["rateLimits"]["futureField"], true);
    }
}
