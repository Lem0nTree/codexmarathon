use super::TurnAdmission;
use crate::error_code::INVALID_REQUEST_ERROR_CODE;
use codex_extension_api::TurnStartAdmission;
use pretty_assertions::assert_eq;

#[test]
fn drain_rejects_new_work_without_waiting_for_admitted_work() {
    let admission = TurnAdmission::default();
    let active = admission.subscribe_active();
    let in_flight = admission.admit().expect("initial request admitted");
    let automatic = admission
        .admit_turn_start()
        .expect("automatic start admitted");
    admission.begin_drain();
    assert_eq!(*active.borrow(), 2);
    assert!(admission.admit_turn_start().is_none());

    let error = admission.admit().err().expect("new request rejected");
    assert_eq!(error.code, INVALID_REQUEST_ERROR_CODE);
    assert_eq!(
        error.data,
        Some(serde_json::json!({ "reason": "serverShuttingDown" }))
    );
    drop(in_flight);
    assert_eq!(*active.borrow(), 1);
    drop(automatic);
    assert_eq!(*active.borrow(), 0);
}

#[tokio::test]
async fn workspace_pause_waits_for_admitted_work_and_preserves_shutdown_drain() {
    let admission = TurnAdmission::default();
    let in_flight = admission.admit().unwrap();
    let pending_pause = admission.pause();
    tokio::pin!(pending_pause);
    tokio::select! {
        biased;
        _ = &mut pending_pause => panic!("pause completed before the existing request"),
        () = async {} => {}
    }
    assert!(admission.admit().is_err());
    assert!(admission.admit_turn_start().is_none());
    drop(in_flight);
    let pause = pending_pause.await.unwrap();
    drop(pause);
    drop(admission.admit().unwrap());
    let pause = admission.pause().await.unwrap();
    admission.begin_drain();
    drop(pause);
    assert!(admission.admit().is_err());
}

#[tokio::test]
async fn transition_drains_admitted_start_before_acquiring_auth_lock() {
    use std::sync::Arc;
    use std::time::Duration;
    use tokio::sync::{Mutex, oneshot};

    let admission = TurnAdmission::default();
    let lock = Arc::new(Mutex::new(()));
    let permit = admission.admit().unwrap();
    let (paused_tx, paused_rx) = oneshot::channel();
    let transition_admission = admission.clone();
    let transition_lock = Arc::clone(&lock);
    let transition = tokio::spawn(async move {
        let pause = transition_admission.pause().await.unwrap();
        let _guard = transition_lock.lock().await;
        paused_tx.send(()).unwrap();
        drop(pause);
    });
    tokio::task::yield_now().await;
    assert!(admission.admit().is_err());
    // The admitted start must still be able to take the lock while the
    // transition waits for its admission permit to drain.
    let guard = tokio::time::timeout(Duration::from_secs(1), lock.lock())
        .await
        .expect("transition acquired the lock before draining admitted work");
    drop(guard);
    drop(permit);
    tokio::time::timeout(Duration::from_secs(1), paused_rx)
        .await
        .unwrap()
        .unwrap();
    transition.await.unwrap();
    assert!(admission.admit().is_ok());
}
