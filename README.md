# CodexMarathon

CodexMarathon is a companion CLI for an existing Codex CLI installation. It
stores several Codex authentication profiles, selects the account with usable
quota, and coordinates a safe handoff when the active account is exhausted.
The normal CodexMarathon release contains only the Go companion executable;
it does not embed or replace Codex.

The companion uses the Codex executable already installed on the user's
machine. It discovers `codex` on `PATH`, or accepts an explicit path in the
configuration/command line. The account manager owns profile metadata and
protected credential snapshots. Codex remains authoritative for its own
authentication state, running turn state, transport cache, conversation files,
and recovery queue.

## Handoff behavior

CodexMarathon chooses one of two integration paths for an account change:

1. If the installed Codex version exposes the supported local control/runtime
   interface, the companion asks the running process to wait for its safe
   boundary, reload the selected credential, invalidate account-bound
   transports, and report the new identity and generation. The active
   conversation stays in the same process.
2. If that interface is unavailable, the companion performs a controlled
   restart. It asks Codex to exit cleanly, atomically deploys the selected
   credential snapshot, launches the same installed `codex` executable again,
   and passes the conversation/thread resume identity. A restart is committed
   only after the resumed process reports the target account.

Both paths preserve the interrupted task in Codex's existing conversation and
recovery mechanism. CodexMarathon does not edit credentials behind an active
turn, create a second turn counter, or submit a duplicate resume prompt.
The process boundary details are in [`docs/companion-cli.md`](docs/companion-cli.md).

## Project status

The repository contains:

- a Go controller with account metadata, protected opaque credential storage,
  telemetry, quota policy, reset scheduling, journaling, and transition
  reconciliation;
- the versioned JSON-RPC model used by the supported local Codex integration;
- deterministic controller/integration fixtures;
- an imported Codex-derived runtime and adapter retained for protocol
  development and the explicitly optional self-contained package; and
- release checks that keep that runtime out of the default companion archive.

Provider-backed OAuth switching, installed-Codex discovery, live reload, and
controlled restart/resume require the acceptance environment described in
[`PLAN.md`](PLAN.md). Static and deterministic tests must not be reported as
live provider evidence.

## Installation

Install Codex first using its normal distribution, then install the matching
CodexMarathon companion archive. The archive is self-contained with respect to
the Go companion, but intentionally expects the user's existing Codex CLI.

On Linux:

```bash
tar -xzf codexmarathon-<version>-linux-aarch64.tar.gz
cd codexmarathon-<version>-linux-aarch64
install -m 0755 codexmarathon "$HOME/.local/bin/codexmarathon"
codexmarathon doctor
```

Use `linux-x86_64` for an amd64 host. `doctor` reports whether `codex` is
discoverable and which integration mode the installed version supports; it
does not print credential values.

The package does not modify an existing Codex installation during extraction.
The first account operation creates only CodexMarathon-owned state under the
configured state directory.

## Build and test

The normal release build compiles only the Go companion:

```bash
bash scripts/build-release.sh
```

That script selects the host platform (`linux-x86_64` or `linux-aarch64`) and
writes the archive below `dist/release`. To assemble an archive directly:

```bash
mkdir -p dist/build
(cd controller && go build -trimpath -o ../dist/build/codexmarathon ./cmd/codexmarathon)
python scripts/package_release.py --root . --output dist/release \
  --controller-binary dist/build/codexmarathon \
  --platform linux-aarch64 --version 0.1.0 --format tar.gz
```

The Go tests and deterministic integration tests are run with:

```bash
(cd controller && go test ./...)
(cd controller && go vet ./...)
(cd integration && go test ./...)
python scripts/test_package_release.py
python scripts/verify_provenance.py --root .
python scripts/verify_package.py --root .
python scripts/verify_protocol.py --root .
```

The imported Rust runtime and adapter remain testable for the optional
integration path:

```bash
(cd runtime/codexmarathon-adapter && cargo test)
(cd runtime/codex-rs && cargo test --locked -p codexmarathon-runtime)
```

They are not required to build or install the normal companion archive.

## Configuration and commands

State and credential paths are controller-owned. Defaults are computed by the
controller; pass explicit paths in automation:

| Option | Purpose |
| --- | --- |
| `--state-dir` | Account registry, journal, and diagnostics root. |
| `--registry` | Non-secret account metadata file. |
| `--vault` | Protected opaque credential snapshot directory. |
| `--auth` | Codex's active authentication file. |
| `--codex` | Installed Codex executable path; otherwise discover `codex`. |

The command surface is:

```text
codexmarathon init
codexmarathon doctor
codexmarathon status [--json]
codexmarathon accounts list
codexmarathon accounts login
codexmarathon accounts activate <account-id>
codexmarathon accounts refresh <account-id>
codexmarathon accounts rename <account-id> --name <alias>
codexmarathon accounts remove <account-id>
codexmarathon run [codex arguments]
codexmarathon switch <account-id>
codexmarathon resume <conversation-or-thread-id>
```

Commands that affect a live Codex process use its local control interface
when available. Otherwise `switch` uses the controlled restart/resume path.
`run` forwards normal Codex arguments to the installed executable and records
the launch identity needed for a later resume. It does not launch the
embedded Rust runtime from the default release.

## Architecture

```text
user
  |
  v
codexmarathon (small Go companion)
  |-- profile registry + protected credential vault
  |-- quota policy, reset scheduling, journal, reconciliation
  |-- installed-Codex discovery and compatibility check
  |
  +--> supported local control interface (preferred)
  |      safe boundary -> reload -> invalidate -> verify identity
  |
  `--> controlled process handoff (fallback)
         graceful stop -> atomic credential deploy -> relaunch codex
         -> resume conversation -> verify identity
```

The companion owns account selection and transition intent. The installed
Codex process owns authentication reload, active-turn safety, model transport
invalidation, conversation state, and exactly-once recovery. The protocol
schemas in [`protocol/`](protocol/) describe the supported local integration;
they are not a requirement for users to install a second server.

## Release contents

The default archive contains `codexmarathon`, metadata, notices, and operator
documentation. It excludes the embedded `codex-app-server`, Cargo target
trees, donor checkouts, auth files, vault snapshots, journals, and developer
caches. The generated manifest marks it as `companion` and records hashes.

An embedded runtime archive can be made only by explicit opt-in:

```bash
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh
```

That package is marked `companion-with-embedded-runtime` and is intended for
diagnostics, protocol development, or offline experiments. It is not the
recommended installation for a machine that already has Codex installed.

## Provenance and licensing

The imported runtime source remains under `runtime/codex-rs/` with its
Apache-2.0 `LICENSE` and `NOTICE`. Its source and adapter are retained for the
optional integration path and provenance review. The read-only donor
checkouts are never runtime dependencies and are never copied into release
archives. See [`runtime/PROVENANCE.md`](runtime/PROVENANCE.md),
[`runtime/PATCH_LEDGER.md`](runtime/PATCH_LEDGER.md), and
[`packaging/README.md`](packaging/README.md).
