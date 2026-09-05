# CodexMarathon companion-agent assignments

These assignments describe the installed-Codex companion product. Agents own
the implementation, focused tests, documentation, and evidence for their
area. They must preserve unrelated work and report compiler/live checks as
passed, blocked, or failed from observed output.

## Agent 1 — Installed-Codex discovery and command wrapper

Implement executable discovery through an explicit `--codex` path,
configuration, or `PATH`; version/capability probing; actionable `doctor`
output; argument forwarding; and launch identity capture. The wrapper must
never search donor trees or start the optional embedded app-server.

Acceptance:

- A clean machine with `codex` installed is detected without reading secrets.
- Missing/incompatible Codex versions produce actionable diagnostics.
- Normal Codex arguments and conversation/thread identity survive wrapper
  launch.

## Agent 2 — Multi-account login and credential lifecycle

Implement login/add, list/status, activate/use, rename, refresh, and remove
through the supported installed-Codex interface. Store opaque snapshots in the
protected vault, preserve refreshed fields, and keep tokens out of metadata,
logs, journals, diagnostics, and output.

Acceptance:

- Two profiles can be created and switched without manual `auth.json` copying.
- Failed or canceled activation leaves the previous profile intact.
- Refreshed credentials return to the correct profile.

## Agent 3 — Quota and account policy

Implement active/inactive quota observation, fresh telemetry, threshold and
`UsageLimitExceeded` decisions, candidate ranking, cooldowns, exhausted-pool
waiting, reset scheduling, and mandatory post-reset revalidation.

Acceptance:

- Focused tests cover multi-bucket, stale/partial, error, reset, and pool
  exhaustion cases.
- No stale or ambiguous observation authorizes an account transition.

## Agent 4 — Preferred live control transition

Integrate the installed Codex supported local control interface. Negotiate
capabilities, wait for Codex's safe boundary, preserve Account A refreshes,
atomically deploy Account B, request native reload and transport invalidation,
and verify account/generation identity before commit.

Acceptance:

- A compatible running Codex process switches without losing its conversation.
- Active turns prevent credential mutation.
- Lost acknowledgements and stale generations reconcile by the same transition
  ID.

## Agent 5 — Controlled restart and resume fallback

Implement the fallback for Codex versions without live control. Ask the same
Codex process to exit cleanly, confirm exit, deploy credentials atomically,
relaunch the same executable, resume the same conversation/thread, and verify
the target identity.

Acceptance:

- Deployment never occurs before process exit.
- Resume uses an exact thread ID or Codex's `--last` selection and preserves
  original arguments.
- Partial restart failures remain durable and reconcilable.

## Agent 6 — Exactly-once recovery and restart durability

Connect policy decisions to Codex's existing parked recovery turn. Keep it
parked through exhaustion/reset waiting, release it only after a verified live
reload or controlled resume, and persist transition/recovery intent across
companion or Codex process restarts.

Acceptance:

- The interrupted task resumes once in the same conversation.
- Crash, reconnect, duplicate-event, and lost-response cases produce zero
  duplicate continuations.

## Agent 7 — Companion packaging and release evidence

Build the Go-only default archive and clean-machine checks. Keep the imported
Rust runtime behind explicit opt-in, validate the archive distribution marker,
and document installed-Codex acceptance for both live reload and controlled
restart/resume. Do not replace missing live evidence with embedded-runtime or
fake-runtime smoke results.

Acceptance:

- Linux companion artifact contains one `codexmarathon` entrypoint and no
  `codex-app-server`.
- Optional embedded-runtime artifacts require an explicit flag and are marked
  separately.
- Static, focused, integration, optional-runtime, and live installed-Codex
  evidence are clearly distinguished.

## Dependency order and orchestration

1. Agent 1 establishes installed-Codex discovery and wrapper behavior.
2. Agents 2 and 3 can proceed in parallel against that boundary.
3. Agent 4 consumes Agents 1–3 for live reload.
4. Agent 5 provides the fallback process boundary and resume identity.
5. Agent 6 connects both transition paths to recovery and restart replay.
6. Agent 7 packages and validates the resulting companion release.

The orchestrator should assign independent tasks to Luna Max agents where
slots permit, review shared-worktree diffs after each handoff, and run the
full companion verification before calling the product complete.
