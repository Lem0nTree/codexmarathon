# CodexMarathon implementation plan

## Ground truth

- Architecture sources: `CodexMarathon_v_0.0.2.md` and `CodexMarathon_v_0.0.2-parat2.md`.
- Donors are reference-only checkouts under `donor/`:
  - `humeo/codex-switch` for controller-side account, credential, and atomic deployment patterns.
  - `Loongphy/codext` for runtime-side turn safety, auth reload, transport invalidation, telemetry, and recovery.
- Product code lives outside `donor/`. No donor mechanism is copied blindly.
- MVP language split: Go controller, Rust Codext runtime adapter, versioned JSON-RPC/IPC contract.

## Delivery gates

### Gate 1 — Contract and foundations

1. Define protocol v1 commands, events, shared identity fields, version negotiation, and JSON schemas.
2. Bootstrap the Go controller module and runtime client types generated or validated against the protocol.
3. Specify the narrow Rust adapter seam against current Codext code, with a patch ledger.

Exit: schemas validate, Go protocol tests pass, and the adapter design maps every operation to observed Codext code.

### Gate 2 — Controller state domains

4. Implement normalized multi-bucket telemetry, per-window freshness, sparse-update reconciliation, and immutable cache.
5. Implement policy decisions plus pool exhaustion/reset scheduling; elapsed reset times require re-observation.
6. Implement account registry, credential vault abstraction, atomic deployment, token write-back contract, and journal.

Exit: deterministic unit tests cover sparse ambiguous updates, cache immutability, stale windows, earliest reset, and no inferred availability.

### Gate 3 — Transition correctness

7. Implement TransitionID/AuthGeneration state, coordinator, runtime/disk identity verification, and uncertain-state reconciliation.
8. Add a fake runtime and failure injection for lost acknowledgements, stale commands, reload failures, and reconnects.

Exit: controller integration tests prove safe A-to-B transition, lost-ACK reconciliation, and stale-generation rejection.

### Gate 4 — Runtime integration

9. Implement the minimal Codext adapter in a dedicated runtime patch area: status/identity API, safe-boundary events, correlated transition commands/events, generation tracking, and telemetry forwarding.
10. Preserve Codext ownership of AuthManager reload, transport invalidation, running-turn guard, and exactly-once parked recovery.

Exit: targeted Rust build/tests pass and patch ledger shows a small, reviewable delta from the donor commit.

### Gate 5 — End-to-end MVP

11. Connect controller to the runtime adapter over local IPC and add supervised lifecycle behavior.
12. Run the seven architecture acceptance cases plus full-pool exhaustion, reset revalidation, and recovery-survival cases.
13. Add operator CLI/status output and concise architecture, telemetry, transitions, recovery, and upstream-sync docs.

Exit: one repeatable local command builds and tests the MVP; observed limitations are documented.

## Agent queue

- Task A: protocol v1 and Go controller bootstrap.
- Task B: telemetry, cache, policy, and reset scheduler.
- Task C: accounts, credentials, atomic deployment, and journal.
- Task D: transition coordinator, reconciliation, fake runtime, and integration tests.
- Task E: Codext runtime adapter implementation.
- Task F: end-to-end wiring, CLI, documentation, and acceptance verification.

Tasks B and C may proceed after the Go module skeleton exists or within isolated packages. Task D consumes Task A's contract. Task E consumes the reviewed protocol and adapter mapping. Task F begins only after controller and runtime gates pass.
