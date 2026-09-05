# Release validation

CodexMarathon's default release is a companion archive containing the Go
controller and allow-listed metadata. A user installs Codex separately. The
release process must not silently bundle `codex-app-server`, replace an
existing `codex` executable, or write user credentials into the archive.

## Required companion checks

From the repository root:

```bash
python scripts/verify_provenance.py --root .
python scripts/verify_package.py --root .
python scripts/verify_protocol.py --root .
python scripts/test_package_release.py
(cd controller && go test ./...)
(cd integration && go test ./...)
(cd controller && go vet ./...)
bash scripts/build-release.sh
artifact=$(find dist/release -maxdepth 1 -type f -name '*.tar.gz' -print -quit)
python scripts/verify_package.py --artifact "$artifact"
python scripts/clean_machine_check.py --artifact "$artifact"
```

The package regression test covers both the normal companion archive and the
explicit opt-in embedded-runtime archive. The clean-machine check extracts
the exact release, sets an empty `PATH`, uses an isolated `CODEX_HOME`, and
runs `init`, `status --json`, and an empty account listing. It proves local
state setup without a repository, toolchain, donor checkout, or bundled
Codex. It does not prove OAuth or live account switching.

## Installed-Codex acceptance

The live acceptance environment must provide a real `codex` executable and a
safe disposable Codex home. Record the exact executable path and version, then
exercise both supported handoff paths:

1. Start a conversation through CodexMarathon. Exhaust or simulate the active
   account at a safe turn boundary. Confirm that the companion requests the
   installed Codex local control interface to reload the target credential,
   invalidates account-bound transports, and reports the target identity and
   generation before continuation is released.
2. Repeat with a Codex version that does not expose the local control
   capability. Confirm that the companion asks the process to exit cleanly,
   atomically deploys the target credential, relaunches the same installed
   executable, resumes the same thread/conversation, and verifies the target
   identity before releasing recovery.

Both runs must show one continuation in the original conversation. Capture
process logs only after redaction; never record access or refresh tokens.

The full Gate 7 contract in [`PLAN.md`](../PLAN.md) also covers active-turn
protection, stale generations, lost acknowledgements, pool exhaustion and
reset revalidation, refreshed-token write-back, and restart recovery.

## Optional embedded-runtime package

The imported Rust runtime is retained for protocol development and controlled
offline diagnostics. It is excluded from the default package. Build it only
with explicit opt-in:

```bash
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh
python scripts/live_smoke.py \
  --runtime-binary runtime/codex-rs/target/release/codex-app-server
```

The generated manifest marks this package
`companion-with-embedded-runtime`. Its smoke result is evidence about the
optional binary and cannot be substituted for installed-Codex acceptance.

## Archive and provenance policy

`scripts/verify_package.py` rejects unsafe paths, duplicate members, missing
hash records, and obvious credential material. It requires one controller
entrypoint and rejects an embedded runtime in an archive marked `companion`.
`runtime/PROVENANCE.md`, `runtime/PATCH_LEDGER.md`, and the Apache-2.0 notices
remain allow-listed metadata for the optional runtime source. Donor checkouts,
Cargo targets, auth files, vault snapshots, journals, and developer caches
are never release inputs.
