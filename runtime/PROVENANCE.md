# Embedded runtime provenance

CodexMarathon embeds the Rust Codex runtime so the installed product does not
depend on a separately installed Codext executable or on the local `donor/`
checkouts. The imported source is a pinned snapshot of the public
`Loongphy/codext` repository:

- Repository: <https://github.com/Loongphy/codext>
- Source commit: `10c0989f282050b8617103d904788d994de8c971`
- Imported tree: `runtime/codex-rs/`
- Import date: 2026-09-04
- Refresh rule: update the pinned commit, copy the complete `codex-rs/`
  workspace, rerun the focused runtime build/tests, and record the new commit
  and any local patch in this file and `PATCH_LEDGER.md`.

## License and notices

The embedded Codext source is distributed under Apache-2.0. The original
license and notice files are retained at:

- [`runtime/codex-rs/LICENSE`](codex-rs/LICENSE)
- [`runtime/codex-rs/NOTICE`](codex-rs/NOTICE)

The imported source also retains its vendored third-party license files. The
CodexMarathon adapter and bridge are Apache-2.0 and are separate local
additions; they do not replace or remove the upstream attribution.

## Local integration patch

The runtime-baseline integration additions are:

1. `runtime/codex-rs/codexmarathon-runtime/`, a typed bridge that adapts the
   native Codex authentication, identity, turn, telemetry, transport, and
   recovery authorities to the existing Marathon adapter.
2. The adapter and bridge as members of the embedded Cargo workspace.
3. `codex-cli::codexmarathon`, a public re-export of the in-process bridge for
   the bundled CLI.
4. `codexmarathon-runtime::CodexNativeRuntime`, which directly composes the
   imported `AuthManager`, shared running-turn watch, shared auth-transition
   lock, and `ThreadManager::invalidate_model_transport_caches`. Its native
   `account/login` and `account/refresh` handlers use Codex's login server and
   refresh authority in isolated temporary auth homes. Its
   `account/authSnapshot/read` handler returns the same shared AuthManager
   snapshot so the controller can synchronize Account A before deployment.

The bridge does not independently parse or print credentials, create a second
turn counter, or start a second Codex process. Opaque snapshot reads are made
only through the shared native `AuthManager` and are not logged or journalled.
The donor checkout remains read-only source material and is not referenced by
any product Cargo manifest.
