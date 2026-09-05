# CodexMarathon release packaging

The default release is a small companion package. It contains the Go
`codexmarathon` executable and the protocol, documentation, provenance, and
license metadata needed by the companion. It does not contain Codex itself.
Users install Codex separately, and the companion discovers the existing
`codex` executable on `PATH` (or accepts an explicit executable path) when it
starts or resumes a session.

Build the normal Linux package with:

```text
bash scripts/build-release.sh
```

The script builds only the Go controller and selects `linux-x86_64` or
`linux-aarch64` from the host architecture. Override the label with
`CODEXMARATHON_PLATFORM` when cross-building. The direct packaging command
is:

```text
python scripts/package_release.py --root . --output dist/release \
  --controller-binary dist/build/codexmarathon \
  --platform linux-aarch64 --version 0.1.0 --format tar.gz
```

The archive contains one required executable, `codexmarathon`, and never
contains `auth.json`, account state, credential snapshots, journals, donor
checkouts, Cargo targets, or developer caches. The generated
`release-manifest.json` records the distribution as `companion` and hashes
every archive member.

An embedded Codex app-server is retained only as an explicit diagnostic or
offline package variant. It is intentionally excluded from the default
release and requires both a runtime binary and the opt-in flag:

```text
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh

python scripts/package_release.py --root . --output dist/release \
  --controller-binary dist/build/codexmarathon \
  --include-embedded-runtime \
  --runtime-binary runtime/codex-rs/target/release/codex-app-server \
  --platform linux-aarch64 --version 0.1.0 --format tar.gz
```

That variant is marked `companion-with-embedded-runtime` in its generated
manifest so it cannot be mistaken for the normal installed-Codex package.
Use `scripts/live_smoke.py` only for this opt-in variant; it tests the
optional runtime boundary and is not part of companion installation.

Verify either archive independently:

```text
python scripts/verify_package.py --artifact dist/release/<artifact> \
  --manifest packaging/release-manifest.json
python scripts/clean_machine_check.py --artifact dist/release/<artifact>
```

Packaging is archive assembly, not a signed installer. Signing, notarization,
and platform-specific installer work remain separate release tasks.
