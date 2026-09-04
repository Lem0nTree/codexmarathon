# CodexMarathon product completion plan

## Non-negotiable product outcome

CodexMarathon is a self-contained Codex distribution. Users install and run
`codexmarathon`; they do not install, configure, or launch codex-switch or
Codext separately. Those repositories are not operational dependencies; their
open-source implementation is pinned, imported, and adapted in the tracked
CodexMarathon product tree with provenance and required notices preserved.

The shipped product must provide:

1. Native OAuth login for multiple Codex accounts.
2. Secure account/profile storage and quota observation.
3. Automatic switching when a configured threshold is reached or a request
   returns `UsageLimitExceeded`.
4. Pool-exhaustion handling using authoritative reset timestamps, followed by
   mandatory telemetry revalidation.
5. Safe-boundary credential deployment, native AuthManager reload, and
   invalidation of account-bound model transports.
6. Verified runtime identity change before continuation.
7. Exactly-once continuation of the interrupted task in the same conversation.

The only unavoidable external services are the official authentication and
model endpoints used by Codex itself.

## Product architecture

```text
codexmarathon (single user-facing command and installer)
|
+-- integrated Codex-derived runtime
|   +-- normal Codex CLI/TUI and conversation state
|   +-- native Codex OAuth/login implementation
|   +-- authoritative running-turn state
|   +-- AuthManager reload and transport invalidation
|   `-- parked, exactly-once recovery turn
|
+-- account manager
|   +-- add/login/list/remove/rename accounts
|   +-- encrypted or OS-protected credential vault
|   `-- token-refresh write-back
|
`-- marathon coordinator
    +-- active/inactive account telemetry
    +-- threshold and exhaustion policy
    +-- reset scheduler and revalidation
    +-- TransitionID and AuthGeneration
    +-- durable transaction journal and reconciliation
    `-- automatic recovery orchestration
```

An internal protocol may remain as a testable module boundary, but it must not
be a user-managed deployment dependency. If implementation uses two bundled
processes, the launcher owns their startup, authenticated local IPC, version
compatibility, shutdown, and recovery. A single-process integration is
preferred when it reduces failure states without duplicating Codex internals.

## Required donor-code reuse

CodexMarathon will reuse and adapt implementation code from both donor
repositories rather than merely reproduce their ideas. Every imported unit
must record its source repository, pinned commit, original path, local path,
and subsequent Marathon changes in a provenance manifest.

### Reuse from `humeo/codex-switch`

Import the feature cores, their tests, and necessary supporting types from the
pinned donor checkout:

- `internal/profile`: profile validation, storage, opaque auth snapshots, and
  token-preserving updates;
- `internal/auth`: OAuth refresh and refreshed-token persistence;
- `internal/quota`: inactive-account quota requests, header parsing,
  rate-limit handling, and retry behavior;
- `internal/switcher`: atomic `auth.json` replacement and profile activation;
- `internal/watcher`: threshold evaluation, candidate ordering, cooldown,
  event/state persistence, and pool-depletion signals;
- selected `internal/cli` flows for account capture, list, use, remove, and
  status, rewritten behind the CodexMarathon command surface.

The session-file watcher becomes a fallback only. Native events from the
integrated runtime are the primary trigger. The donor's shell-out login flow
must be replaced with direct calls to the integrated Codex login crate.

### Reuse from `Loongphy/codext`

Import the required Codext `codex-rs` runtime code as the product runtime and
retain its existing implementations for:

- Codex CLI/TUI, thread/session state, and model request execution;
- `login` AuthManager, browser/device-code OAuth, auth reload, and refresh;
- authoritative running-turn guard and deferred authentication changes;
- account/workspace identity refresh;
- invalidation of cached account-bound model transports;
- rate-limit snapshots and update notifications;
- `UsageLimitExceeded` detection, parked synthetic recovery, and exactly-once
  continuation in the same conversation.

Marathon-specific code must be integrated at those existing seams. It must not
replace them with parallel AuthManager, turn counter, transport teardown, or
recovery implementations.

### Licensing and provenance gate

Codext carries Apache-2.0 and its license/notices must be preserved. The
checked-out codex-switch tree contains no `LICENSE` file. Before distributing
copied codex-switch code, obtain or locate an explicit license grant from its
owner and record it in the repository. Public source and an invitation to
adapt it establish project intent but are not, by themselves, a standard
redistribution license. This is a release/legal gate, not a reason to redesign
away from the required technical reuse.

## Gate 1 — Import a maintainable runtime baseline

- Create a tracked `runtime/codex-rs` product tree from a pinned Codext/OpenAI
  Codex revision, preserving Apache-2.0 notices and attribution.
- Exclude unrelated Codext UX changes unless required for continuity.
- Maintain a small upstream-reapply ledger for every Marathon-specific patch.
- Rename/package the resulting executable as part of CodexMarathon.

Exit: the in-repository runtime builds and behaves as a normal Codex CLI before
Marathon behavior is enabled.

## Gate 2 — Native multi-account login and vault

- Reuse the in-tree Codex login crate for browser and device-code OAuth.
- Add `account login`/`add`, `account list`/`status`, `account use`/`activate`,
  `account rename`, `account refresh`, and `account remove` commands.
- Capture each completed login directly into the Marathon vault without asking
  users to copy `auth.json` or run another tool.
- Protect credentials with OS facilities where available and restrictive file
  permissions as a fallback.
- Preserve provider-specific fields and write refreshed tokens back to the
  correct stored profile.

Exit: a user can add two accounts, inspect health, refresh, rename, and switch
manually using only `codexmarathon`, with no token printed or stored in
metadata/logs.

## Gate 3 — Integrated telemetry and policy loop

- Feed active-account rate limits directly from the runtime.
- Implement an in-product provider for inactive stored accounts using supported
  Codex authentication/telemetry behavior; do not shell out to codex-switch.
- Evaluate both proactive thresholds and `UsageLimitExceeded`.
- Rank only fresh, eligible accounts and prevent switch loops.
- When all accounts are unavailable, select the earliest trustworthy reset;
  if none exists, wait for fresh data with bounded backoff.
- At a reset boundary, invalidate the old observation and re-query before
  declaring capacity available.

Exit: deterministic tests cover threshold switching, hard-limit switching,
multi-bucket limits, stale data, pool exhaustion, reset ordering, and failed
refreshes.

## Gate 4 — Native safe transition

- Use the runtime's authoritative running-turn state; do not create a second
  SafeBoundaryManager.
- Before leaving Account A, persist any legitimate token refresh to A's vault
  snapshot.
- Atomically deploy Account B's credential snapshot.
- Reload the existing runtime AuthManager and invalidate all account-bound
  model transports and cached account state.
- Emit and verify `IdentityChanged(B, generation N+1, transition ID)`.
- Reconcile controller intent, deployed credentials, and runtime identity after
  any timeout, crash, or lost acknowledgement.

Exit: the next model request is proven to use Account B without restarting or
replacing the active conversation.

## Gate 5 — Exactly-once task continuation

- On `UsageLimitExceeded`, preserve Codext's existing parked recovery turn.
- Do not let the account manager or coordinator create a second resume prompt.
- Release recovery only after the verified identity-changing reload.
- If the pool is exhausted, keep recovery parked across reset waiting and
  controller/runtime restarts.
- Persist enough transition and recovery metadata to avoid duplicate dispatch.

Exit: the interrupted task resumes once, in the same conversation, after a
verified switch; crash/restart cases do not lose or duplicate continuation.

## Gate 6 — One-install operational product

- Add a single launcher and configuration surface.
- Bundle or statically link all product-owned components; no separate Codext or
  codex-switch installation is permitted.
- If local IPC remains, use authenticated user-scoped Windows named pipes and
  Unix domain sockets created and managed automatically.
- Add migration, diagnostics, safe rollback, and upstream-version reporting.
- Produce signed/reproducible platform artifacts and an installer/uninstaller.

Exit: a clean machine can install CodexMarathon, log into multiple accounts,
start a long task, switch automatically, and resume without manual file edits
or auxiliary processes.

## Gate 7 — Release evidence

Required end-to-end cases:

1. Proactive threshold A -> B during a long task.
2. `UsageLimitExceeded` A -> B with exactly-once recovery.
3. Active turn prevents mid-turn identity mutation.
4. Lost commit acknowledgement resolves by reconciliation.
5. Stale generation is rejected.
6. Entire pool exhausted -> earliest reset -> refresh -> transition.
7. No trustworthy reset -> wait for data with bounded backoff.
8. Controller/runtime restart during each transition phase.
9. Refreshed token write-back prevents stale credential resurrection.
10. Packaging test on a machine without donor repositories or developer tools.

Release requires compiled Go/Rust tests, live Codex integration tests, CI on
supported platforms, and a documented upstream sync/reapply procedure.
