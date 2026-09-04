# Runtime-owned recovery

Codext remains the owner of `UsageLimitExceeded` handling and conversation
continuity. It parks a recovery continuation, reloads authentication at the
native safe boundary, invalidates account-bound transports, and dispatches the
parked continuation once. The controller only observes:

```text
recovery_parked -> recovery_started -> recovery_completed
```

The controller may enter `POOL_EXHAUSTED` while a recovery is parked. It waits
for fresh usable telemetry and a verified identity transition; it does not
create a prompt, copy the recovery queue, or submit a duplicate continuation.

Recovery event IDs are deduplicated by the runtime adapter per stage. A
controller event loop can therefore safely reconnect or receive repeated
native notifications without treating one recovery as multiple turns.

If a transition is uncertain during recovery, preserve the transition ID and
reconcile it after reconnecting to the same runtime identity. Do not infer
success from an `auth.json` timestamp or from the reset schedule.

