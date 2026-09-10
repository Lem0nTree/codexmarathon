# Build and verification

The supported product is the custom Codex Rust CLI with Marathon compiled into
the same binary. A Rust toolchain and the native build dependencies are
required; no Go toolchain or external Marathon process is part of the build.

From the repository root, run the focused checks:

```bash
cd runtime/codex-rs
cargo fmt --all -- --check
cargo check -p codexmarathon-runtime
cargo check -p codex-login
cargo check -p codex-app-server
cargo check -p codex-tui
cargo check -p codex-cli
cargo test --locked -p codexmarathon-runtime
cargo build --locked --release -p codex-cli
```

The release executable is written to:

```text
runtime/codex-rs/target/release/codex
```

Run the command surface smoke check against that binary:

```bash
runtime/codex-rs/target/release/codex --version
runtime/codex-rs/target/release/codex marathon --help
```

For a server without a local browser, use `codex marathon login ALIAS
--device-code`; the command prints a verification URL and one-time code. Use a
disposable `CODEX_HOME` for account and transition tests.

## Evidence labels

- `static`: formatting, source review, and whitespace checks.
- `focused`: deterministic unit tests for the native Marathon runtime.
- `build`: a release `codex-cli` executable was produced by Cargo.
- `live`: the resulting Codex executable was exercised with a real account and
  safe disposable Codex home.
- `blocked`: a required Rust toolchain, linker dependency, platform, or test
  credential was unavailable.

A successful runtime unit test or compilation proves the native code builds;
it does not by itself prove a live account switch or provider login.
