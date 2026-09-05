# CodexMarathon companion product plan

## Non-negotiable product outcome

CodexMarathon is a companion CLI for a Codex CLI that the user has already
installed. The normal CodexMarathon release contains the small Go companion
and its metadata; it does not contain, replace, or silently start a second
Codex runtime.

The companion must provide:

1. Multiple account/profile management with protected opaque credential
   snapshots.
2. Installed-Codex discovery, version checks, and an actionable compatibility
   diagnostic.
3. Proactive and `UsageLimitExceeded` quota policy with fresh telemetry,
   deterministic account ranking, reset waiting, and post-reset revalidation.
4. A preferred live handoff through the installed Codex supported local
   control interface: wait for the runtime-owned safe boundary, reload the
   selected authentication, invalidate account-bound transports, and verify
   the new identity/generation.
5. A controlled fallback handoff when live control is unavailable: stop the
   same installed Codex process cleanly, atomically deploy the selected
   credential, relaunch that executable, resume the same conversation/thread,
   and verify the target identity before continuing.
6. Exactly-once continuation using Codex's existing parked recovery turn.
7. Durable transition/recovery intent and reconciliation across process loss.
8. A small Linux and Windows companion archive with no credentials, donor
   checkout, Cargo target, or embedded runtime in the default package.

An embedded Codex-derived runtime may remain in the source tree for protocol
development and an explicitly opt-in diagnostic package. It is never a
default installation dependency.

## Product architecture

```text
codexmarathon (Go companion)
|
+-- account registry + protected credential vault
+-- quota telemetry + policy + reset scheduler
+-- transition journal + reconciliation
+-- installed-Codex discovery + capability/version check
|
+--> preferred: supported local control interface
|      safe boundary -> auth reload -> transport invalidation -> verify
|
`--> fallback: controlled process handoff
       graceful stop -> atomic auth deploy -> relaunch codex
       -> resume conversation -> verify identity
```

Codex remains authoritative for authentication reload, active-turn state,
model transport invalidation, conversation persistence, and recovery ordering.
The companion owns account selection, credential snapshots, transition intent,
telemetry policy, and evidence reconciliation. It must not create a second
turn counter, duplicate a recovery prompt, or mutate `auth.json` during an
active turn.

The imported `runtime/codex-rs` and `runtime/codexmarathon-adapter` trees are
test/provenance inputs for the optional compatible control implementation.
They are not runtime dependencies of the companion archive. No donor path is
read at build or runtime.

## Required donor-code reuse

The controller continues to reuse and adapt the pinned `codex-switch` feature
cores behind the companion command surface:

- profile validation, opaque snapshots, metadata, and atomic replacement;
- OAuth refresh and refreshed-token persistence;
- inactive-account quota requests, response-header parsing, retry, and cache;
- candidate ordering, threshold/cooldown policy, reset scheduling, and event
  persistence; and
- account capture, list, status, activate, rename, refresh, and remove flows.

CodexMarathon must use the installed Codex's supported authentication and
control interface for live operations. It must not shell out to `codex-switch`
or copy an unlicensed donor checkout into a release.

The imported Codex-derived runtime remains the source for the optional local
control adapter and tests around AuthManager reload, safe boundaries,
transport invalidation, telemetry, and native recovery. The companion's
fallback process handoff must use the same installed `codex` executable that
the user selected.

## Licensing and provenance gate

The imported Codex-derived source retains its Apache-2.0 `LICENSE` and
`NOTICE`. `runtime/PROVENANCE.md`, `runtime/PATCH_LEDGER.md`, and those
notices remain tracked and are included as allow-listed metadata. The pinned
`codex-switch` checkout has no formal license file; its source remains outside
release archives until an explicit redistribution grant is recorded.

## Gate 1 — Companion executable and installed-Codex boundary

- Build the normal release from the Go controller alone.
- Resolve Codex from `--codex`, configured path, or `PATH` without searching
  donor or embedded-runtime directories.
- Report version, executable path, health, and supported live-control versus
  restart/resume capability without exposing credentials.
- Forward normal Codex arguments and preserve the selected process/thread
  identity needed for controlled resume.
- Keep the imported runtime build behind an explicit opt-in package flag.

Exit: a clean machine with Codex installed can run `codexmarathon doctor` and
launch Codex through the companion; a machine without Codex receives an
actionable prerequisite error.

## Gate 2 — Native multi-account login and credential lifecycle

- Provide account login/add, list/status, activate/use, rename, refresh, and
  remove using one companion command surface.
- Capture completed login output through the installed Codex supported
  interface and store it only in the protected vault.
- Preserve provider-specific fields and refreshed tokens in the correct
  profile.
- Keep tokens out of metadata, journals, diagnostics, logs, and normal output.
- Leave the previous active profile intact on cancellation or failed writes.

Exit: two accounts can be stored, inspected, refreshed, and selected without
manual `auth.json` copying or another account-switching tool.

## Gate 3 — Quota intelligence and automatic policy

- Feed active-account observations from installed Codex when available.
- Probe inactive profiles through supported Codex-compatible telemetry calls.
- Handle proactive thresholds and `UsageLimitExceeded` deterministically.
- Rank only fresh eligible accounts and prevent switch loops.
- Wait for the earliest trustworthy reset when the pool is exhausted, then
  re-query telemetry before authorizing a switch.

Exit: focused tests cover multi-bucket limits, partial/stale observations,
errors, cooldowns, pool exhaustion, reset ordering, and failed refreshes.

## Gate 4 — Preferred live reload transition

- Negotiate the installed Codex local control protocol and validate its
  version/capabilities before changing state.
- Use Codex's authoritative running-turn and safe-boundary state.
- Persist Account A token refreshes before selecting Account B.
- Atomically deploy Account B's snapshot only at the safe boundary.
- Request native auth reload and account-bound transport invalidation through
  the supported interface.
- Correlate transition ID and expected auth generation, then verify runtime,
  disk, and process identity before committing.

Exit: a live compatible Codex process switches accounts without losing the
conversation, and its next request cannot reuse Account A's transport.

## Gate 5 — Controlled restart and resume fallback

- Detect when the installed Codex lacks the compatible local control path or
  loses it during a transition.
- Ask the same process to stop cleanly and persist its conversation/thread
  resume identity.
- Confirm process exit before replacing the active credential.
- Atomically deploy the selected snapshot and preserve legitimate token
  refreshes from Account A.
- Relaunch the same executable with original arguments and the captured resume
  identity.
- Verify Account B and the expected conversation/thread before committing.

Exit: a restart fallback preserves the same conversation and resumes only
after target identity verification; partial failures remain reconcilable.

## Gate 6 — Exactly-once recovery and restart durability

- Observe Codex's existing parked recovery turn after `UsageLimitExceeded`.
- Keep it parked during quota exhaustion and reset waiting.
- Release it only after either Gate 4 or Gate 5 verifies the target identity.
- Persist transition/recovery IDs and phases so controller/process restarts do
  not lose or duplicate continuation.
- Reconcile lost acknowledgements, stale generations, process crashes, and
  refreshed-token write-back using the same transition identity.

Exit: an interrupted task resumes once in the same conversation on Account B,
including failure/restart at every handoff phase.

## Gate 7 — Companion release evidence

Required acceptance cases:

1. Installed-Codex discovery and incompatible-version diagnostics.
2. Proactive threshold A -> B through the live control interface.
3. `UsageLimitExceeded` A -> B with exactly-once recovery.
4. Active turn prevents live credential mutation.
5. Live control unavailable -> controlled restart and same-thread resume.
6. Lost reload/stop acknowledgement reconciles by the same transition ID.
7. Stale generation, split identity, and failed deployment are rejected.
8. Exhausted pool -> earliest reset -> fresh telemetry -> transition.
9. Refreshed-token write-back survives switch and process restart.
10. Clean-machine companion packaging without donor repositories, developer
    toolchains, or embedded runtime.

The default artifact must be built by Go only and must contain one
`codexmarathon` entrypoint. The optional embedded-runtime artifact must be
explicitly requested, marked in its generated manifest, and tested separately.
Static, deterministic, and optional-runtime results are never substituted for
live installed-Codex evidence.
