# Release validation

CodexMarathon releases are custom Codex CLI builds with native Marathon
controls compiled into the `codex` executable. There is no separate Marathon
controller or Go binary. Release archives do include every sibling executable
the Codex CLI package expects:

- Linux: `codex`, `codex-code-mode-host`, `codex-responses-api-proxy`, and
  `bwrap`.
- Windows: `codex.exe`, `codex-code-mode-host.exe`,
  `codex-responses-api-proxy.exe`, `codex-command-runner.exe`, and
  `codex-windows-sandbox-setup.exe`.

Do not publish a CLI-only archive. Code mode and platform sandboxing depend on
those resources.

## Automated upstream releases

`.github/workflows/upstream-release.yml` polls the latest published stable
release from `openai/codex` every six hours. It maps a tag such as
`rust-v0.154.0` to `codexmarathon-v0.154.0` and exits successfully when that
repository release already exists. A concurrency group and a second lookup
immediately before publication protect against duplicate releases.

For a new release, the workflow reconstructs the maintained customization
delta from the exact baseline in `.github/upstream-base.txt`, applies it to the
upstream tag, and stops before building if Git reports a conflict. A clean port
is archived once and used for native Linux x64 and Windows x64 builds.
Publication requires both packages and the Marathon command smoke test. Only
the final publish job has `contents: write`.

Each release carries the platform packages, the exact prepared source archive,
the generated full-index customization patch, a JSON provenance manifest, and
SHA-256 checksums for every asset. The manifest records the upstream and
customization commits plus the patch, source-tree, and source-archive digests.

Run the workflow manually with `port_only` enabled to check a particular
published stable tag without building or publishing. A failed port uploads a
14-day diagnostic artifact and never guesses at conflict resolution.

## Required checks

From the repository root:

```bash
python3 scripts/verify_provenance.py --root .
cd runtime/codex-rs
cargo fmt --all -- --check
cargo test --locked -p codexmarathon-runtime
cd ../..
scripts/build-release.sh
```

The build script creates a complete archive under `dist/release` and verifies
the Marathon command surface. For an additional local check:

```bash
dist/release/codex marathon --help
dist/release/codex marathon status
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
credentials, unrelated Cargo target output, or developer caches in a release.
