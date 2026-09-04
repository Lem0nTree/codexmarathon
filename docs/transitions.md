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

