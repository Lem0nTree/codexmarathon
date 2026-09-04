# Release validation

CodexMarathon is released as one product: a Go controller plus the embedded
Rust Codex runtime. A user does not install `codex-switch`, `codext`, or a
second adapter process. The controller's supervisor starts the bundled runtime
and uses the user-scoped local IPC boundary selected for the platform.

## Evidence vocabulary

Every release report must label the evidence that actually ran:

| Label | Meaning |
| --- | --- |
| `static` | Files, schemas, manifests, provenance, and forbidden-content checks ran. |
| `focused` | A unit or package test ran against a deterministic fixture. |
| `integration` | Controller and adapter/runtime boundaries ran together in-process or over local IPC. |
| `live` | The compiled embedded runtime was started and exercised; no donor checkout or mock runtime substituted for it. |
| `blocked` | The check could not run because a toolchain, platform, credential, or environment was unavailable. |
| `pending` | The behavior still requires implementation or a controlled acceptance environment. |

`PASS` means only that the named check executed and passed. A static or fake
runtime result is never reported as live Codex success.

## Required release checks

The Gate 7 scenarios in `PLAN.md` are the release contract:

1. proactive threshold A to B;
2. hard-limit A to B with exactly-once recovery;
3. active-turn safe-boundary protection;
4. lost commit acknowledgement reconciliation;
5. stale-generation rejection;
6. exhausted-pool earliest-reset revalidation;
7. missing-reset bounded waiting;
8. controller/runtime restart recovery;
9. refreshed-token write-back;
10. clean-machine packaging without donor repositories or developer tools.

The current focused Go and Rust tests exercise the controller policy,
transition, recovery, telemetry, reset, wire, adapter, and bridge seams. The
`integration` module uses a deterministic fake runtime for those portions; its
results are `focused`/`integration`, not `live`.

The Linux CI job additionally runs `scripts/live_smoke.py` when the compiled
embedded app-server exists. That smoke starts the real binary with an isolated
temporary `CODEX_HOME`, connects to its Unix socket, negotiates protocol v1,
and reads runtime identity. It does not log in or make a provider request, so
it proves the packaged local runtime boundary only. A missing binary is
reported as `blocked` and cannot be promoted to `live`.

Both platform jobs run `scripts/clean_machine_check.py` against the exact
archive they just assembled. The check extracts into a temporary directory and
runs the packaged controller's `init`, `status --json`, and empty account-list
commands with an empty `PATH`, an isolated `CODEX_HOME`, and no repository or
donor checkout. This is a clean-machine installation check, not a provider
acceptance test.

## Repeatable commands

From the repository root:

```text
python scripts/verify_provenance.py --root .
python scripts/verify_package.py --root . --manifest packaging/release-manifest.json
python scripts/verify_protocol.py --root .

cd controller
go test ./...
cd ../integration
go test ./...
cd ../runtime/codex-rs
cargo test --locked -p codexmarathon-runtime-adapter
cargo test --locked -p codexmarathon-runtime
cargo build --locked --release -p codex-app-server
cd ../..
python scripts/live_smoke.py --runtime-binary runtime/codex-rs/target/release/codex-app-server
```

After an archive is built, exercise its extraction boundary with:

```text
python scripts/clean_machine_check.py --artifact <path-to-release-archive>
```

Use `scripts/build-release.sh` on Linux or `scripts/build-release.ps1` on
Windows to build and package the two binaries. The scripts write only below
`dist/`; generated output is ignored by Git and is independently checked
before it is archived.

## Clean-machine check

Extract an artifact into an empty directory that has no repository checkout,
Cargo target, Go toolchain, donor directory, or developer state. Confirm that
the two entrypoints and included notices are present, then run:

```text
codexmarathon init --state-dir <temporary-state>
codexmarathon status --state-dir <temporary-state> --json
codexmarathon run --state-dir <temporary-state>
```

The last command requires the packaged runtime and should be run only in a
controlled test environment; stop it with the platform interrupt signal. A
successful extraction or `status` invocation does not prove OAuth, quota
switching, or continuation. Those require the live acceptance environment and
must be reported separately.

## Provenance and legal clearance

`runtime/PROVENANCE.md`, `runtime/PATCH_LEDGER.md`, and the generated
`release-manifest.json` identify the pinned Codext source and local bridge
patches. The embedded Codex source retains its Apache-2.0 `LICENSE` and
`NOTICE` files. CodexMarathon adapts implementation ideas and code from the
public `codex-switch` checkout, but the pinned checkout contains no formal
license file; its source is therefore not copied into release archives until
the owner’s redistribution grant is recorded. This check is intentional and
does not silently convert public availability into a license.

Before a release is published, review the generated manifest, source commit,
uncommitted changes, archive contents, checksums, and any platform signing
records. Never put auth files, vault snapshots, tokens, provider headers, or
local journals in an artifact or release log.
