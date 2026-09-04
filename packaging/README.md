# CodexMarathon release packaging

The release package is deliberately assembled from an allow-list. It contains
the Go controller, the embedded Rust `codex-app-server` renamed to
`codexmarathon-runtime`, protocol metadata, operator documentation, and the
Apache-2.0 notices required by the embedded Codex source.

The package does not contain `donor/`, a Cargo target directory, controller
state, `auth.json`, credential snapshots, journals, diagnostics, environment
files, or developer caches. `scripts/package_release.py` refuses to archive a
path outside the allow-list and runs the same forbidden-path and secret scan
used by CI.

Build the binaries first, then run:

```text
python scripts/package_release.py --root . --output dist/release \
  --controller-binary dist/build/codexmarathon \
  --runtime-binary runtime/codex-rs/target/release/codex-app-server \
  --platform linux-x86_64 --version 0.1.0 --format tar.gz
```

Use `--format zip` for a Windows artifact. The generated archive contains a
`release-manifest.json` with SHA-256 hashes and source provenance. Package
verification is independent and can be rerun with:

```text
python scripts/verify_package.py --artifact dist/release/<artifact> \
  --manifest packaging/release-manifest.json
```

This is archive packaging, not a signed installer. Signing, notarization, and
platform-specific installers remain release follow-up work; an unsigned
archive must not be described as a trusted installation.
