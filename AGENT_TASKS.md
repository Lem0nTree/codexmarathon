# CodexMarathon feature-agent assignments

These are feature-sized assignments. Each agent owns an end-to-end capability,
including donor-code import, integration, tests, documentation, and a reviewable
handoff. Agents must preserve unrelated work and may not declare success from
static inspection alone when a compiler or live runtime is required.

## Agent 1 — Runtime product baseline

Create the in-repository CodexMarathon runtime from the pinned Codext source.
Preserve Apache-2.0 attribution, retain the normal Codex CLI/TUI behavior, add
the existing Marathon adapter as an internal crate/module, and expose native
AuthManager, identity, turn-state, telemetry, transport-invalidation, and
recovery seams. Establish the upstream patch/provenance ledger and a focused
build that does not require the donor checkout.

Acceptance:

- CodexMarathon's tracked runtime builds and launches as a Codex-compatible CLI.
- No separate Codext installation or process started by the user is required.
- Existing Codext continuity behavior remains covered by imported/adapted tests.
- All copied paths and commits appear in the provenance manifest.

## Agent 2 — Native multi-account login and credential lifecycle

Deliver the complete account-manager experience. Reuse codex-switch profile,
auth-refresh, switching, and relevant CLI code; replace its shell-out login
with direct use of the integrated Codex login crate. Implement account login,
list, status, rename, activate, refresh, and safe removal. Integrate secure
credential storage, atomic active-auth deployment, and refreshed-token
write-back to the correct account.

Acceptance:

- A user can log into at least two accounts using only `codexmarathon`.
- No token appears in registry metadata, logs, journal, or normal CLI output.
- Manual A -> B -> A switching preserves refreshed credentials.
- Failure/cancellation leaves the previous active account intact.

## Agent 3 — Quota intelligence and automatic account policy

Deliver active and inactive account quota observation and the automatic policy
engine. Reuse codex-switch quota, cache, retry, watcher/candidate, threshold,
and cooldown code, then adapt it to CodexMarathon's multi-bucket telemetry,
freshness, and runtime event model. Implement proactive thresholds,
`UsageLimitExceeded`, loop prevention, deterministic account ranking, complete
pool exhaustion, earliest-reset selection, bounded waiting, and mandatory
post-reset revalidation.

Acceptance:

- Both proactive threshold and hard-limit events produce a deterministic next
  action from fresh telemetry.
- Stale/ambiguous data never authorizes a switch.
- All-exhausted pools wait without busy polling and never infer restored quota.
- Tests cover multiple buckets, partial updates, errors, cooldowns, and resets.

## Agent 4 — Transactional live account switching

Integrate the Go transition model with the embedded runtime's native mechanics.
Implement safe-boundary waiting, Account A token synchronization, atomic
Account B deployment, correlated reload, transport invalidation, identity
refresh, TransitionID/AuthGeneration validation, and three-way reconciliation
of controller intent, disk identity, and runtime identity.

Acceptance:

- Authentication never changes during an active turn.
- A committed transition proves that disk and runtime both use Account B at the
  expected generation.
- Lost acknowledgements, disconnects, reload failures, and stale commands have
  deterministic recoverable outcomes.
- The first post-transition model request cannot reuse Account A's transport.

## Agent 5 — Exactly-once interrupted-task recovery

Connect automatic policy decisions to Codext's existing parked recovery turn.
Reuse Codext's `UsageLimitExceeded` recovery queue and configured resume prompt;
do not create a controller-owned continuation. Keep recovery parked while the
pool is exhausted, release it only after a verified identity-changing reload,
and persist sufficient metadata to survive controller/runtime restarts without
losing or duplicating the continuation.

Acceptance:

- A failed Account A request resumes once in the same thread on Account B.
- User-queued input and synthetic recovery retain the intended ordering.
- Pool-exhaustion waiting does not discard or submit recovery early.
- Crash/restart tests at every transition phase produce zero duplicate turns.

## Agent 6 — Unified launcher, IPC, and operational lifecycle

Make CodexMarathon a one-command product. Choose and implement either a direct
single-process integration or automatically managed bundled processes. If IPC
remains, provide authenticated user-scoped Unix sockets and Windows named
pipes, protocol compatibility checks, startup readiness, health monitoring,
reconnect, graceful shutdown, and diagnostics. The user must never configure
or launch an adapter or donor executable.

Acceptance:

- `codexmarathon` starts every required internal component.
- Component version mismatch fails safely with an actionable error.
- Runtime crashes and reconnects preserve or reconcile in-flight state.
- No unauthenticated TCP listener is part of the default product.

## Agent 7 — Release validation and packaging

Build the release evidence and distribution pipeline. Add live end-to-end
fixtures, Linux and Windows CI, clean-machine installation tests, upgrade and
rollback checks, license/provenance validation, release artifacts, and concise
operator documentation. Exercise real integrated runtime paths, not only the
fake runtime.

Acceptance:

- All ten scenarios in `PLAN.md` Gate 7 pass in CI.
- A clean supported machine installs and runs without donor repositories or
  developer toolchains.
- Release artifacts contain required licenses/notices and no credentials.
- Published checks clearly separate simulated, integration, and live evidence.

## Dependency order and orchestration

1. Agent 1 establishes the runtime baseline.
2. Agents 2 and 3 can proceed in parallel against that pinned baseline.
3. Agent 4 consumes the reviewed outputs of Agents 1–3.
4. Agent 5 consumes the live transition path from Agent 4.
5. Agent 6 owns final process integration after the runtime boundary stabilizes.
6. Agent 7 validates the whole product and is not permitted to replace missing
   live evidence with mocks.

After each assignment, the orchestrator reviews provenance, scope, security,
tests, and behavior before unlocking dependent work. Rejected work returns as
one coherent feature correction, not a collection of microtasks.
