# CodexMarathon

CodexMarathon is an experimental, local control plane for coordinating
multiple Codex identities with a live Codext runtime. It combines a Go
controller, a narrow Rust runtime adapter, and a versioned JSON-RPC protocol.
The controller evaluates fresh rate-limit observations, selects an account,
atomically deploys its opaque credential snapshot, and verifies the runtime's
identity before declaring a transition complete.

CodexMarathon is an MVP foundation, not a replacement for Codex or Codext
authentication. The runtime remains authoritative for authentication reload,
running-turn safety, model transport invalidation, and conversation recovery.

## Project status

The current checkout contains:

- protocol v1 JSON-RPC envelopes, commands, events, and schemas;
- a Go controller with account metadata, an opaque credential vault,
  normalized telemetry, policy/reset scheduling, an append-only journal, and
  transition reconciliation;
- a standalone Rust library adapter with framing, negotiation, transition
  correlation, generation checks, and runtime-event forwarding;
- a deterministic fake runtime and Go integration tests; and
- architecture, telemetry, transition, recovery, and upstream-sync notes.

The live Codext integration is not complete. The Rust crate is a library seam;
it does not open a listener or start Codext. A Codext patch or test harness
must implement CodextBackend and own the local IPC/process lifecycle. There is
no dashboard, daemon, process supervisor, or account-import CLI in this MVP.

Verification in this checkout observed 14 static/protocol checks passing and
0 failures. Go and Rust test execution was blocked because go.exe and
cargo.exe were not installed on the host. See Verification for the
repeatable command and its pass/block semantics.

## Architecture

~~~text
operator / one-shot CLI
          |
          v
Go controller
  accounts | opaque vault | telemetry | policy | reset
  transition coordinator | reconciliation | metadata journal
          |
    newline-delimited JSON-RPC / IPC v1
          |
Rust runtime adapter + Codext
  AuthManager | running-turn guard | transports | recovery
~~~

The ownership boundary is deliberate:

| Concern | Controller | Runtime |
| --- | --- | --- |
| Account aliases and profile metadata | owns | observes target |
| Opaque credential snapshots and atomic deployment | owns | reads deployed file |
| Active runtime identity | verifies | authoritative |
| Running-turn count and safe boundary | consumes | owns |
| Auth reload and transport invalidation | requests/correlates | owns |
| Rate-limit provider and native recovery | consumes events | owns |
| Multi-bucket merge, freshness, and reset scheduling | owns | emits observations |
| Transition ID, journal, and uncertain reconciliation | owns | returns evidence |

The transition sequence is:

1. Read the runtime identity at generation N.
2. Prepare one transition for the target account and expected generation N + 1.
3. Wait for the runtime's authoritative safe boundary.
4. Atomically replace the Codex auth file with the target snapshot.
5. Commit the correlated transition to the runtime.
6. Read runtime and disk identity; commit only when both agree at the target
   generation.

The outcomes are committed, rejected, and uncertain. An uncertain operation
must be reconciled with the same transition ID before another transition is
attempted. The journal is durable metadata evidence, while the MVP
coordinator's active transition map is process-local; durable replay and
resume across a controller restart are roadmap work.

## Donors and attribution

Two public repositories were used as read-only design references:

- [humeo/codex-switch](https://github.com/humeo/codex-switch) informed the
  controller-side separation of account/profile snapshots and atomic file
  replacement.
- [Loongphy/codext](https://github.com/Loongphy/codext) informed the
  runtime-side seam for turn safety, authentication reload, transport
  invalidation, telemetry, and recovery.

The corresponding checkouts are under donor/codex-switch and donor/codext.
They are not build dependencies and must remain unmodified. Product code is
outside donor/; the Rust adapter's narrow mapping and unapplied changes are
recorded in docs/runtime-adapter.md and runtime/PATCH_LEDGER.md.

## Prerequisites

- Git.
- Go 1.22 or newer for the controller and Go tests.
- A current Rust toolchain with Cargo for the adapter (edition 2021).
- PowerShell 7 (pwsh) to run the Windows-friendly verifier.
- A Codex installation with an auth file. The default is
  ~/.codex/auth.json; pass --auth when using another path.
- For live commands, an already-running Codext-compatible peer that speaks
  protocol v1. The generic CLI connector accepts tcp://host:port and
  host:port; the library also accepts unix:// where supported. Named pipes
  require a host-specific connector.

The Go modules have no external application dependency. Cargo downloads the
crate's serde and serde_json dependencies when building the adapter.

Check the local prerequisites with:

~~~powershell
go version
cargo --version
pwsh --version
~~~

## Installation and builds

Clone the repository and enter its root:

~~~powershell
git clone <repository-url> codexmarathon
cd codexmarathon
~~~

Build the controller executable from the Go module:

~~~powershell
Set-Location ./controller
go build -o ../codexmarathon.exe ./cmd/codexmarathon
go test ./...
~~~

Run the integration module separately:

~~~powershell
Set-Location ../integration
go test ./...
~~~

Build and test the Rust adapter library:

~~~powershell
Set-Location ../runtime/codexmarathon-adapter
cargo build
cargo test
~~~

The adapter package intentionally produces a library, not a listener binary.
The controller can be used with go run without first producing an executable.

## Configuration

All path flags are command-local and must follow the subcommand. Defaults are
computed by controller/app:

| Flag | Default | Purpose |
| --- | --- | --- |
| --state-dir | os.UserConfigDir()/CodexMarathon (or .codexmarathon) | Controller-owned state root |
| --registry | <state-dir>/accounts.json | Non-secret account registry |
| --vault | <state-dir>/credentials | Opaque per-account JSON snapshots |
| --auth | os.UserHomeDir()/.codex/auth.json | Codex runtime auth file to replace |
| --journal | <state-dir>/events.jsonl | Append-only transition/runtime metadata |

Initialize only the controller-owned directories:

~~~powershell
.\codexmarathon.exe init --state-dir ./.codexmarathon-dev --auth "$env:USERPROFILE/.codex/auth.json"
~~~

init does not create, modify, or read the Codex auth file. The registry is
version 1 JSON and contains metadata only. A minimal non-secret example is:

~~~json
{
  "version": 1,
  "active_account_id": "account-a",
  "accounts": {
    "account-a": {
      "id": "account-a",
      "alias": "work",
      "credential_ref": "account-a",
      "credential_health": "unknown"
    },
    "account-b": {
      "id": "account-b",
      "alias": "personal",
      "credential_ref": "account-b",
      "credential_health": "unknown"
    }
  }
}
~~~

Each vault snapshot is stored as <vault>/<account-id>.json. It must be a valid
JSON object; the controller preserves provider-specific fields without
interpreting them. Use the credentials.FileVault API or an audited local
setup tool to save snapshots. The MVP CLI has no credential import command,
so never put real tokens in source control, examples, shell history, or the
journal.

## CLI usage

Show the command surface:

~~~powershell
.\codexmarathon.exe --help
~~~

Create state and inspect it without a runtime connection:

~~~powershell
.\codexmarathon.exe init --state-dir ./.state
.\codexmarathon.exe status --state-dir ./.state
.\codexmarathon.exe status --state-dir ./.state --json
.\codexmarathon.exe accounts list --state-dir ./.state
~~~

With a trusted, already-running protocol peer, transition to one configured
account:

~~~powershell
.\codexmarathon.exe transition account-b --runtime tcp://127.0.0.1:43123 --state-dir ./.state --auth "$env:USERPROFILE/.codex/auth.json" --timeout 2m
~~~

If transport or persistence fails after a state-changing operation, preserve
the printed transition ID and reconcile that same ID after reconnecting to the
same runtime identity:

~~~powershell
.\codexmarathon.exe reconcile tx-7 --runtime tcp://127.0.0.1:43123 --state-dir ./.state --auth "$env:USERPROFILE/.codex/auth.json" --json
~~~

reconcile accepts an optional transition ID; when omitted, a long-lived
controller instance uses its active unresolved transition. Because the CLI is
one-shot and the coordinator state is currently in memory, use a library
integration or a future supervisor/replay implementation when reconciliation
must survive process exit.

The command surface is intentionally small:

| Command | Behavior |
| --- | --- |
| init | Creates the state and vault directories. |
| status | Shows paths, account/telemetry counts, policy, and optionally one live runtime read. |
| accounts list | Lists non-secret registry metadata. |
| transition <account-id> | Performs one guarded transition through a live runtime endpoint. |
| reconcile [transition-id] | Compares fresh runtime/disk evidence for an uncertain transition. |

Use --json on status, transition, and reconcile for machine-readable output.
No CLI command starts a daemon or edits a donor checkout.

## Protocol and runtime integration

Protocol v1 is newline-delimited JSON-RPC 2.0. The first request on a
connection is protocol/negotiate; normal requests follow only after a common
version is selected. Runtime notifications use the codexmarathon/event method.
The contract is defined in:

- [protocol/protocol.json](protocol/protocol.json) — JSON-RPC envelope and
  shared types;
- [protocol/commands.json](protocol/commands.json) — controller-to-runtime
  requests;
- [protocol/events.json](protocol/events.json) — runtime-to-controller events;
- [protocol/README.md](protocol/README.md) — wire-level rules; and
- [protocol/VERSION](protocol/VERSION) — compatibility integer (1).

The request methods are runtime/state/read, auth/transition/prepare,
auth/transition/commit, auth/transition/cancel, runtime/identity/read, and
runtime/authGeneration/read. Events cover runtime readiness, full and sparse
rate limits, turn lifecycle, safe boundaries, auth reload, identity changes,
and runtime-owned recovery lifecycle.

An integration supplies a CodextBackend implementation that delegates to
Codext's existing AuthManager, running-turn watch, rate-limit read, transport
invalidation, and recovery machinery. The adapter must not read the
controller vault, create another turn counter, invalidate transports itself,
or create a second recovery continuation. See
[docs/runtime-adapter.md](docs/runtime-adapter.md).

## Verification

Run the single repository verifier from the root:

~~~powershell
pwsh -NoProfile -File ./verify.ps1
~~~

It checks required files, parses all three protocol JSON documents, validates
protocol version 1, then runs (when available):

- go test ./... in controller/;
- go test ./... in integration/; and
- cargo test in runtime/codexmarathon-adapter/.

Missing executables are reported as BLOCKED, not as passes. A static or
executed failure exits 1; missing optional toolchains exit 2 after the other
checks complete. -SkipGo and -SkipRust are available when a caller
intentionally wants to omit those toolchain-gated checks.

The in-process fake runtime tests transition behavior including waiting for a
safe boundary, lost-ack reconciliation, stale-generation rejection, and
identity comparison. They do not prove a live Codext IPC integration.

## Safety and security

- Credential snapshots are opaque to the controller and are kept separate
  from registry metadata.
- Controller directories are created with owner-only permissions where the
  host supports them; vault and auth files use owner read/write permissions.
- Auth replacement writes and syncs a temporary file in the destination
  directory before an atomic replace.
- Journal records contain transition/runtime metadata only. Tokens, auth JSON,
  authorization headers, and recovery prompt text are not journal fields.
- Fresh telemetry is required for policy decisions. An elapsed reset
  timestamp never proves that capacity has returned.
- A transition is not considered complete from a disk write or an uncorrelated
  runtime response alone.
- The generic TCP connector has no TLS or peer authentication. Keep the
  listener on a trusted local interface or use a protected IPC transport; do
  not expose it to an untrusted network.
- Cross-process writers for the registry/vault are outside the MVP. Atomic
  replacement protects readers from partial files but does not provide a
  distributed lock.

For operational details, read docs/recovery.md, docs/telemetry.md, and
docs/transitions.md.

## Repository layout

| Path | Responsibility |
| --- | --- |
| controller/ | Go module, controller domains, runtime client, and CLI |
| integration/ | Separate Go module with fake runtime and acceptance tests |
| runtime/codexmarathon-adapter/ | Standalone Rust library adapter |
| protocol/ | Versioned JSON-RPC schemas and wire contract |
| docs/ | Architecture, build, telemetry, transition, recovery, and sync notes |
| donor/ | Read-only public reference checkouts |
| verify.ps1 | Windows-friendly static and toolchain-gated verification |
| PLAN.md | Implementation gates and remaining work |

## Roadmap

The next gates are intentionally narrow:

1. Apply the adapter seam to a fresh Codext checkout and implement the local
   listener/process boundary.
2. Add a supervised runtime lifecycle and protected Windows IPC option.
3. Add safe account/profile import and management commands without exposing
   tokens in logs or registry metadata.
4. Persist/replay transition intent so uncertain reconciliation survives a
   controller restart.
5. Add end-to-end acceptance cases, CI/toolchain coverage, and packaging only
   after the runtime authority boundary is validated.

The full gate list is in [PLAN.md](PLAN.md).

## Contributing and upstream sync

Keep changes small, source-grounded, and outside donor/. Before updating a
runtime integration:

1. Fetch the relevant public upstream ref in its donor checkout.
2. Re-audit AuthManager reload, running-turn safety, transport invalidation,
   rate-limit notifications, and recovery ordering.
3. Re-check protocol payloads against the current runtime schema.
4. Update the adapter patch ledger and focused tests.
5. Run verify.ps1, Go tests, Rust tests, and the integration module when the
   corresponding toolchains are available.
6. Review the diff to confirm donor files were not changed and no credential
   value entered source, logs, or journal records.

Reapply a small adapter patch onto a fresh upstream runtime instead of
merging unrelated donor history. For a code contribution, include the
smallest relevant test or documentation update and report checks as passed,
blocked, or failed based on observed output.
