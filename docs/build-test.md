# Build and verification

The normal CodexMarathon companion release requires Go and Python. It does
not require Cargo because the user's Codex CLI is installed separately.

Run the focused checks from the repository root:

```bash
(cd controller && go test ./...)
(cd controller && go vet ./...)
(cd integration && go test ./...)
python scripts/test_package_release.py
python scripts/verify_provenance.py --root .
python scripts/verify_package.py --root .
python scripts/verify_protocol.py --root .
```

Build the normal Linux companion artifact with:

```bash
bash scripts/build-release.sh
```

The script compiles the Go controller, chooses `linux-x86_64` or
`linux-aarch64` from the host, and packages only the companion executable and
allow-listed metadata. Verify the exact archive and its clean-machine
boundary:

```bash
artifact=$(find dist/release -maxdepth 1 -type f -name '*.tar.gz' -print -quit)
python scripts/verify_package.py --artifact "$artifact"
python scripts/clean_machine_check.py --artifact "$artifact"
```

`clean_machine_check.py` runs the packaged controller's initialization,
status, and empty account-list commands with an empty `PATH` and isolated
Codex home. This proves that the companion does not need a repository,
toolchain, donor checkout, or bundled runtime for local state management. A
separate installed `codex` executable is required for live login and process
handoff tests.

The imported Rust adapter/runtime remains available for optional protocol
development and the explicit embedded-runtime variant:

```bash
(cd runtime/codexmarathon-adapter && cargo test)
(cd runtime/codex-rs && cargo test --locked -p codexmarathon-runtime)
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh
python scripts/live_smoke.py \
  --runtime-binary runtime/codex-rs/target/release/codex-app-server
```

The optional smoke is evidence about that explicitly bundled runtime only. It
does not replace testing against the installed Codex executable, OAuth, quota,
reload, controlled restart, or conversation resume.

On Windows, use `scripts/build-release.ps1` for the Go companion archive. The
same packaging contract applies; the named-pipe or process-restart acceptance
tests must run on a Windows host when Windows support is enabled.

## Evidence labels

- `static`: manifests, schemas, provenance, and forbidden-content checks.
- `focused`: deterministic unit or package tests.
- `integration`: controller and protocol fixtures together.
- `live`: an installed Codex process was exercised in the selected mode.
- `blocked`: a required toolchain, binary, platform, or credential was absent.
- `pending`: the behavior still needs implementation or an acceptance run.

Static, focused, or optional embedded-runtime evidence must not be described
as proof of live installed-Codex switching.
