# Transition protocol and reconciliation

A transition has one controller-generated `transition_id` and one expected
target generation. For a runtime currently at generation `N`, the controller
sends `expected_generation = N + 1` in both prepare and commit. The runtime
must reject a stale or mismatched ID/generation before changing AuthManager.

```text
read runtime identity (A, N)
        |
prepare(tx, B, N+1)
        |
wait for Codext safe boundary
        |
read latest native Account A snapshot -> protected vault write-back
        |
atomic deploy of B snapshot
        |
commit(tx, B, N+1)
        |
read runtime identity and disk identity
        |
committed only when both report B at N+1
```

The runtime owns the running-turn guard. The controller does not create a
second turn counter; it consumes `safe_boundary_reached` and falls back to
state reads. `identity_changed` is meaningful only after the existing Codext
reload/invalidation/config-refresh path has completed.

When the runtime exposes the native snapshot reader, the coordinator captures
Account A's latest AuthManager snapshot at that boundary and writes it to the
same protected vault used by deployment. This preserves any token refresh that
occurred during the interrupted turn before Account B replaces `auth.json`.

## Outcomes

* `committed`: disk and runtime agree at the expected generation.
* `rejected`: the runtime gave a correlated domain rejection before adoption.
* `uncertain`: an IPC, journal, deployment, or verification failure means the
  controller cannot prove whether a state-changing operation was applied.

An uncertain record blocks a new transition. Reconcile the same ID by reading
runtime and disk authorities. If both already agree, adopt the committed
state; if only the disk has advanced, retry the same correlated commit; if
both remain at the previous identity, close as rejected. Split-brain or
generation mismatch stays unresolved and requires operator investigation.

## Generation compatibility

The Go controller and fake runtime define `expected_generation` as the target
generation (`current + 1`). The standalone adapter must use the same meaning
when it is connected to the controller. This explicit rule avoids the unsafe
failure mode where one side interprets the same command as current generation
and the other as next generation.

## Interrupted-turn recovery

Codext's native `UsageLimitExceeded` path parks its configured synthetic
resume turn in the same thread and retains ordering with user-queued input.
CodexMarathon observes that parked recovery by `recovery_id`; it does not copy
the prompt or submit a replacement turn. A policy decision may keep the
recovery in `waiting_for_capacity` while every configured account is exhausted
or until the earliest reset.

After the transition coordinator proves runtime identity, deployed identity,
and `auth_generation` all match Account B, the controller durably records a
`RecoveryReleaseRequested` event and sends `recovery/release`. The runtime
returns `released` or `already_released` without creating another prompt. A
lost response remains `release_requested`; on reconnect, the controller reads
runtime identity/recovery state and retries the same ID only when the verified
target generation is still active. Recovery journal records contain only IDs,
account names, generations, phases, and bounded diagnostic reasons.
