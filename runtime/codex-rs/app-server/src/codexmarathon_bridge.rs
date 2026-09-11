//! Native CodexMarathon command bridge for the embedded TUI.
//!
//! The Marathon adapter is hosted by app-server, while the parked
//! `UsageLimitExceeded` continuation is owned by the TUI `ChatWidget`.  This
//! module is the deliberately narrow hand-off between those authorities: it
//! emits one typed app-server notification and waits for the TUI's typed
//! acknowledgement.  It never carries prompt text and never owns a queue.

use codex_app_server_protocol::CodexMarathonRecoveryLifecycleNotification;
use codex_app_server_protocol::CodexMarathonRecoveryReleaseNotification;
use codex_app_server_protocol::CodexMarathonRecoveryReleaseResultNotification;
use codex_app_server_protocol::ServerNotification;
use codexmarathon_runtime::NativeRecoveryBridge;
use codexmarathon_runtime::NativeRecoveryReleaseFuture;
use codexmarathon_runtime_adapter::protocol::{
    RecoveryCompletedPayload, RecoveryLifecycleEvent, RecoveryParkedPayload,
    RecoveryReleaseOutcome, RecoveryReleaseParams, RecoveryReleaseResult, RecoveryStartedPayload,
};
use crate::outgoing_message::OutgoingMessageSender;
use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex as StdMutex};
use tokio::sync::Mutex;
use tokio::sync::oneshot;
use tokio::time::Duration;

/// A bounded wait prevents a disconnected TUI from pinning the native
/// authority thread forever. The adapter reports `uncertain` and its
/// controller-side journal can reconcile the same release after reconnect.
const RELEASE_ACK_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Default)]
struct BridgeState {
    pending: HashMap<String, PendingRelease>,
    completed: HashMap<String, RecoveryReleaseResult>,
    next_waiter_id: u64,
}

#[derive(Default)]
struct LifecycleEventState {
    events: VecDeque<RecoveryLifecycleEvent>,
    /// Replayed app-server notifications are harmless. Keep one key per
    /// lifecycle stage so a reconnect cannot enqueue a second observation.
    seen: HashSet<(String, String)>,
}

struct PendingRelease {
    thread_id: Option<String>,
    transition_id: String,
    expected_generation: u64,
    waiters: Vec<(u64, oneshot::Sender<RecoveryReleaseResult>)>,
}

/// App-server-owned half of the native recovery command seam.
pub(crate) struct CodexMarathonRecoveryBridge {
    outgoing: Arc<OutgoingMessageSender>,
    state: Arc<Mutex<BridgeState>>,
    lifecycle: Arc<StdMutex<LifecycleEventState>>,
}

impl CodexMarathonRecoveryBridge {
    pub(crate) fn new(outgoing: Arc<OutgoingMessageSender>) -> Self {
        Self {
            outgoing,
            state: Arc::new(Mutex::new(BridgeState::default())),
            lifecycle: Arc::new(StdMutex::new(LifecycleEventState::default())),
        }
    }

    /// Record one metadata-only lifecycle observation from the TUI. The
    /// native runtime drains this queue synchronously before serving a
    /// controller command, which closes the park/release race.
    pub(crate) fn observe_lifecycle_event(
        &self,
        notification: CodexMarathonRecoveryLifecycleNotification,
    ) {
        let event_type = notification.event_type.trim().to_string();
        let recovery_id = notification.recovery_id.trim().to_string();
        if recovery_id.is_empty() {
            tracing::warn!("ignoring CodexMarathon recovery lifecycle event without recovery_id");
            return;
        }
        let event = match event_type.as_str() {
            "recovery_parked" => {
                let Some(reason) = notification.reason.filter(|value| !value.trim().is_empty())
                else {
                    tracing::warn!(recovery_id, "ignoring parked recovery event without reason");
                    return;
                };
                RecoveryLifecycleEvent::Parked(RecoveryParkedPayload {
                    recovery_id: recovery_id.clone(),
                    reason,
                    thread_id: notification.thread_id,
                    turn_id: notification.turn_id,
                    source_account_id: notification.source_account_id,
                })
            }
            "recovery_started" => RecoveryLifecycleEvent::Started(RecoveryStartedPayload {
                recovery_id: recovery_id.clone(),
                thread_id: notification.thread_id,
                turn_id: notification.turn_id,
            }),
            "recovery_completed" => {
                let Some(outcome) = notification
                    .outcome
                    .filter(|value| !value.trim().is_empty())
                else {
                    tracing::warn!(recovery_id, "ignoring completed recovery event without outcome");
                    return;
                };
                RecoveryLifecycleEvent::Completed(RecoveryCompletedPayload {
                    recovery_id: recovery_id.clone(),
                    outcome,
                    error_code: notification.error_code,
                    thread_id: notification.thread_id,
                    turn_id: notification.turn_id,
                })
            }
            _ => {
                tracing::warn!(event_type, recovery_id, "ignoring unknown CodexMarathon recovery lifecycle event");
                return;
            }
        };
        let mut lifecycle = self
            .lifecycle
            .lock()
            .expect("CodexMarathon lifecycle mutex poisoned");
        if !lifecycle
            .seen
            .insert((event_type, recovery_id))
        {
            return;
        }
        // Lifecycle events are finite and only metadata. Bound the queue so
        // a disconnected controller cannot grow app-server memory forever.
        const MAX_LIFECYCLE_EVENTS: usize = 1024;
        if lifecycle.events.len() >= MAX_LIFECYCLE_EVENTS {
            lifecycle.events.pop_front();
        }
        lifecycle.events.push_back(event);
    }

    pub(crate) fn drain_lifecycle_events(&self) -> Vec<RecoveryLifecycleEvent> {
        let mut lifecycle = self
            .lifecycle
            .lock()
            .expect("CodexMarathon lifecycle mutex poisoned");
        lifecycle.events.drain(..).collect()
    }

    async fn release_async(
        &self,
        params: RecoveryReleaseParams,
    ) -> Result<RecoveryReleaseResult, codexmarathon_runtime_adapter::BackendError> {
        let (waiter_id, response_rx, should_send) = {
            let mut state = self.state.lock().await;
            if let Some(result) = state.completed.get(&params.recovery_id) {
                return Ok(result.clone());
            }
            let (response_tx, response_rx) = oneshot::channel();
            state.next_waiter_id = state.next_waiter_id.wrapping_add(1);
            let waiter_id = state.next_waiter_id;
            let should_send = if let Some(pending) = state.pending.get_mut(&params.recovery_id) {
                // A recovery id is bound to one transition and one native
                // thread. Do not let a stale/replayed controller request join
                // the in-flight command for a different identity transition.
                if pending.thread_id != params.thread_id
                    || pending.transition_id != params.transition_id
                    || pending.expected_generation != params.expected_generation
                {
                    return Ok(RecoveryReleaseResult {
                        recovery_id: params.recovery_id,
                        thread_id: params.thread_id,
                        outcome: RecoveryReleaseOutcome::Rejected,
                        error_code: Some("bridge_request_conflict".to_string()),
                        error_message: Some(
                            "recovery id is already bound to a different transition".to_string(),
                        ),
                    });
                }
                let should_send = pending.waiters.is_empty();
                pending.waiters.push((waiter_id, response_tx));
                should_send
            } else {
                state.pending.insert(
                    params.recovery_id.clone(),
                    PendingRelease {
                        thread_id: params.thread_id.clone(),
                        transition_id: params.transition_id.clone(),
                        expected_generation: params.expected_generation,
                        waiters: vec![(waiter_id, response_tx)],
                    },
                );
                true
            };
            (waiter_id, response_rx, should_send)
        };

        if should_send {
            self.outgoing
                .send_server_notification(ServerNotification::CodexMarathonRecoveryRelease(
                    CodexMarathonRecoveryReleaseNotification {
                        recovery_id: params.recovery_id.clone(),
                        thread_id: params.thread_id.clone(),
                        transition_id: params.transition_id.clone(),
                        expected_generation: params.expected_generation,
                    },
                ))
                .await;
        }

        match tokio::time::timeout(RELEASE_ACK_TIMEOUT, response_rx).await {
            Ok(Ok(result)) => Ok(result),
            Ok(Err(_)) | Err(_) => {
                let mut state = self.state.lock().await;
                if let Some(pending) = state.pending.get_mut(&params.recovery_id) {
                    // Keep the request context after a timeout. A late TUI
                    // acknowledgement can still be correlated and cached;
                    // a controller retry may also safely resend the same
                    // idempotent command when no waiter remains.
                    pending.waiters.retain(|(id, _)| *id != waiter_id);
                }
                Ok(RecoveryReleaseResult {
                    recovery_id: params.recovery_id,
                    thread_id: params.thread_id,
                    outcome: RecoveryReleaseOutcome::Uncertain,
                    error_code: Some("tui_ack_timeout".to_string()),
                    error_message: Some(
                        "Codex TUI did not acknowledge the recovery release".to_string(),
                    ),
                })
            }
        }
    }

    /// Resolve a release command acknowledged by the TUI. The result is
    /// retained only for successful outcomes so a rejected stale command can
    /// be retried after the controller reconciles its identity transition.
    pub(crate) async fn resolve_release(
        &self,
        notification: CodexMarathonRecoveryReleaseResultNotification,
    ) {
        let outcome = match notification.outcome.as_str() {
            "released" => RecoveryReleaseOutcome::Released,
            "already_released" => RecoveryReleaseOutcome::AlreadyReleased,
            "uncertain" => RecoveryReleaseOutcome::Uncertain,
            _ => RecoveryReleaseOutcome::Rejected,
        };
        let result = RecoveryReleaseResult {
            recovery_id: notification.recovery_id.clone(),
            thread_id: notification.thread_id,
            outcome,
            error_code: notification.error_code,
            error_message: notification.error_message,
        };
        let mut state = self.state.lock().await;
        let Some(pending) = state.pending.get(&notification.recovery_id) else {
            tracing::debug!(
                recovery_id = %notification.recovery_id,
                "ignoring unsolicited CodexMarathon recovery acknowledgement"
            );
            return;
        };
        if pending.thread_id.as_ref() != result.thread_id.as_ref()
            || pending.transition_id != notification.transition_id
            || pending.expected_generation != notification.expected_generation
        {
            tracing::warn!(
                recovery_id = %notification.recovery_id,
                "ignoring recovery acknowledgement with mismatched transition context"
            );
            return;
        }
        let pending = state
            .pending
            .remove(&notification.recovery_id)
            .expect("pending recovery entry was checked above");
        let waiters = pending.waiters;
        if matches!(
            result.outcome,
            RecoveryReleaseOutcome::Released | RecoveryReleaseOutcome::AlreadyReleased
        ) {
            state
                .completed
                .insert(notification.recovery_id, result.clone());
        }
        for (_, waiter) in waiters {
            let _ = waiter.send(result.clone());
        }
    }
}

impl NativeRecoveryBridge for CodexMarathonRecoveryBridge {
    fn release(&self, params: RecoveryReleaseParams) -> NativeRecoveryReleaseFuture {
        let bridge = self.clone_for_task();
        Box::pin(async move { bridge.release_async(params).await })
    }

    fn drain_events(&self) -> Vec<RecoveryLifecycleEvent> {
        self.drain_lifecycle_events()
    }
}

impl CodexMarathonRecoveryBridge {
    fn clone_for_task(&self) -> Self {
        Self {
            outgoing: Arc::clone(&self.outgoing),
            state: Arc::clone(&self.state),
            lifecycle: Arc::clone(&self.lifecycle),
        }
    }
}
