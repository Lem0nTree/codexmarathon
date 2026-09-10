# Release validation

CodexMarathon releases are custom Codex CLI builds with native Marathon
controls compiled into the `codex` executable. The release does not include a
separate controller, Go binary, or companion archive.

## Required checks

From the repository root:

```bash
python3 scripts/verify_provenance.py --root .
cd runtime/codex-rs
cargo fmt --all -- --check
cargo test --locked -p codexmarathon-runtime
cargo build --locked --release -p codex-cli
```

The resulting executable is `runtime/codex-rs/target/release/codex`. Verify
that its Marathon surface is present:

```bash
runtime/codex-rs/target/release/codex marathon --help
runtime/codex-rs/target/release/codex marathon status
```

## Live acceptance

Use a disposable `CODEX_HOME` and a real ChatGPT account only in an explicit
acceptance environment. Record the executable version and exercise:

1. `codex marathon login ALIAS --device-code` on a headless host.
2. A second account import or login under another alias.
3. `codex marathon status`, `on`, `off`, and `switch ALIAS`.
4. `/marathon` help and status inside an interactive session.
5. Status-line Marathon state and managed-account count.
6. Recovery at a safe turn boundary with no duplicate continuation.

Do not invoke a real quota reset while testing automatic reset. The product
uses mocked reset capabilities and executors until a provider integration is
explicitly reviewed and enabled.

## Provenance

The embedded Codex source and local Marathon additions are documented in
[`runtime/PROVENANCE.md`](../runtime/PROVENANCE.md) and
[`runtime/PATCH_LEDGER.md`](../runtime/PATCH_LEDGER.md). Do not package local
credentials, Cargo target output, or developer caches in a release.
