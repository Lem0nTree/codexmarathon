# MVP architecture

## Outcome

CodexMarathon coordinates a controller-owned credential choice with a live
Codext runtime. It does not reimplement Codex authentication or continuity.

```text
operator / one-shot CLI
          |
          v
Go controller
  accounts | vault | telemetry | policy | reset
  transition coordinator | reconciliation | journal
          |
    JSON-RPC/IPC v1
          |
embedded Rust Codex runtime + Marathon bridge
  AuthManager | running-turn guard | transports | recovery
```

The two public donor checkouts under `donor/` remain read-only source
provenance. `codex-switch` supplies the profile, refresh, switching, and CLI
behavior adapted by the controller; Codext supplies the native login/
AuthManager authority. The pinned Codext `codex-rs` source is imported into the tracked
`runtime/codex-rs/` product tree, with source commit and attribution recorded
in [`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md). Product code never
depends on a donor path at build or runtime.

## Ownership

| Concern | Controller | Runtime |
| --- | ---: | ---: |
| Account aliases and profile metadata | owns | observes target |
| Opaque credential snapshots and atomic deployment | owns | reads deployed file |
| Active runtime identity | verifies | authoritative |
| Running-turn count and safe boundary | consumes observation | owns |
| Auth reload and transport invalidation | requests/correlates | owns |
| Rate-limit provider and native recovery | consumes events | owns |
| Multi-bucket merge, freshness, and reset scheduling | owns | emits observations |
| Transition ID, journal, and uncertain reconciliation | owns | returns evidence |

## Composition root

`controller/app` constructs the durable domains and accepts an already-running
`io.ReadWriteCloser`. It negotiates protocol v1, performs one runtime state
read, then runs one event fan-out loop. The loop forwards safe-boundary events
to the coordinator and rate-limit events to `StateStore`/`SnapshotCache`.

The coordinator deliberately receives a request-only runtime facade. This
prevents two goroutines from consuming the same event channel: the app loop
handles events, while the coordinator can still poll runtime state as a
fallback when a notification is delayed.

The CLI is a thin one-shot surface:

* `init` creates controller-owned directories;
* `status` displays paths, configured account count, telemetry count, policy,
  and optionally a live runtime read;
* `accounts list`, `accounts status`, `accounts login/add`, `accounts rename`,
  `accounts activate/use`, `accounts refresh`, and `accounts remove` provide
  the secret-free profile lifecycle. Login and refresh call the integrated
  runtime's native auth service; the controller never shells out to another
  Codex executable;
* `transition` and `reconcile` operate on one live runtime endpoint.

No command modifies a donor checkout. The runtime endpoint used by auth
commands is an internal CodexMarathon boundary; the final launcher owns its
process lifecycle and local transport protection.

## Runtime integration boundary

The internal `codexmarathon-runtime` crate supplies a typed
`NativeCodexRuntime` bridge around the existing AuthManager, turn-watch,
account rate-limit read, transport invalidation, and recovery machinery. The
adapter must not read the controller vault, maintain another turn counter, or
generate a second recovery prompt. The bridge is in-process; the later
lifecycle gate owns any authenticated local transport needed by the Go
controller.
