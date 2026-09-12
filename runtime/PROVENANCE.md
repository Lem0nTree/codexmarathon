# Optional runtime provenance

CodexMarathon retains this Rust Codex runtime source for the compatible local
control implementation, protocol development, and an explicitly opt-in
self-contained diagnostic package. The normal companion release uses the
user's separately installed Codex executable. The imported source is a pinned
snapshot of the public
`openai/codex` repository:

- Repository: <https://github.com/openai/codex>
- Source tag: `rust-v0.154.0`
- Source commit: `6b9826e3aa83b1a5947db50f4332cb9c65f1b340`
- Imported tree: `runtime/codex-rs/`
- Import date: 2026-09-12
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
   the optional runtime compatibility build.
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
