# CodexMarathon development machine

The project is a Rust workspace containing a custom Codex CLI. Install the
native build dependencies, Rust, Python for provenance checks, and Git. No Go
toolchain or separate Marathon executable is required.

## System packages

On Ubuntu:

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  build-essential ca-certificates curl file git jq pkg-config unzip xz-utils \
  libssl-dev libsqlite3-dev
```

The full Codex workspace may additionally need `clang`, `cmake`,
`libclang-dev`, `protobuf-compiler`, `libcap-dev`, `libseccomp-dev`, and
`zlib1g-dev` for platform-specific crates.

## Rust toolchain

Use the toolchain pinned or selected by `runtime/codex-rs` and install the
formatter:

```bash
rustup toolchain install stable
rustup component add rustfmt clippy
```

## Build and focused checks

From the repository root:

```bash
python3 scripts/verify_provenance.py --root .
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

The executable is `runtime/codex-rs/target/release/codex`. Use an isolated
`CODEX_HOME` for login and transition checks. On a headless server, use
`codex marathon login ALIAS --device-code` and complete the displayed code on
another machine.

## Formatting

```bash
cd runtime/codex-rs
cargo fmt --all
```

Keep Cargo target output, local credentials, and `.cache`/`.toolchains`
outside commits.
