# Runtime-owned recovery

Codext remains the owner of `UsageLimitExceeded` handling and conversation
continuity. It parks a recovery continuation, reloads authentication at the
native safe boundary, invalidates account-bound transports, and dispatches the
parked continuation once. The controller only observes:

```text
recovery_parked -> recovery_started -> recovery_completed
```

In the embedded build, these observations come from the real Codex TUI
`ChatWidget` lifecycle. The TUI emits `recovery_parked` when its existing
configured synthetic turn is created, `recovery_started` only after the normal
input flow dispatches that turn, and `recovery_completed` when that same turn
finishes. The app-server bridge carries metadata only and the runtime drains
it before every controller request, so a release cannot race an unregistered
parked recovery. No prompt is copied, and no second prompt or queue is made.

Lifecycle notifications are stage/idempotent across reconnects: the TUI
retains failed sends until the app-server boundary is available again, while
the bridge and runtime adapter suppress repeated `(stage, recovery_id)`
observations. Started and completed events inherit the released transition ID
and target auth generation, allowing the controller to correlate the native
turn with the exact account transition.

The controller may enter `POOL_EXHAUSTED` while a recovery is parked. It waits
for fresh usable telemetry and a verified identity transition; it does not
create a prompt, copy the recovery queue, or submit a duplicate continuation.

Recovery event IDs are deduplicated by the runtime adapter per stage. A
controller event loop can therefore safely reconnect or receive repeated
native notifications without treating one recovery as multiple turns.

If a transition is uncertain during recovery, preserve the transition ID and
reconcile it after reconnecting to the same runtime identity. Do not infer
success from an `auth.json` timestamp or from the reset schedule.
