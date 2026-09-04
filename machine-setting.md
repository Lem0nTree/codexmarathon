# Ubuntu development-machine setup

This prepares a clean Ubuntu 22.04 or 24.04 instance to build and test the Go
controller, Rust adapter/integrated Codex runtime, integration tests, protocol
files, and repository formatting tools.

The pinned donor requirements currently imply:

- Go 1.25.4 for reused codex-switch code (`donor/codex-switch/go.mod`).
- Rust 1.95.0 with rustfmt, clippy, and rust-src (`donor/codext/rust-toolchain.toml`).
- Node.js 22+ and pnpm 10.34.5 for Codext repository maintenance.
- PowerShell 7 for `verify.ps1` (optional on Ubuntu; native commands are also
  listed below).

## 1. Install operating-system packages

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  build-essential ca-certificates curl file git jq pkg-config unzip xz-utils \
  clang cmake libclang-dev libssl-dev libsqlite3-dev protobuf-compiler \
  libcap-dev libseccomp-dev zlib1g-dev
```

## 2. Install Go 1.25.4

This installs Go under `/opt` and leaves an existing `/usr/local/go` untouched.

```bash
GO_VERSION=1.25.4
case "$(uname -m)" in
  x86_64) GO_ARCH=amd64 ;;
  aarch64|arm64) GO_ARCH=arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl -fLO "https://go.dev/dl/go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
sudo mkdir -p "/opt/go${GO_VERSION}"
sudo tar -C "/opt/go${GO_VERSION}" --strip-components=1 \
  -xzf "go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"
sudo ln -sfn "/opt/go${GO_VERSION}/bin/go" /usr/local/bin/go
sudo ln -sfn "/opt/go${GO_VERSION}/bin/gofmt" /usr/local/bin/gofmt
rm "go${GO_VERSION}.linux-${GO_ARCH}.tar.gz"

go version
```

## 3. Install the pinned Rust toolchain

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
  | sh -s -- -y --profile minimal --default-toolchain 1.95.0
source "$HOME/.cargo/env"

rustup component add --toolchain 1.95.0 rustfmt clippy rust-src
rustc --version
cargo --version
```

Install the Codext workspace helpers:

```bash
cargo install --locked just
cargo install --locked dotslash
cargo install --locked cargo-nextest
just --version
cargo nextest --version
```

## 4. Install Node.js 22 and pnpm 10.34.5

Node is used for Codext repository-wide schema/formatting maintenance, not for
the CodexMarathon runtime itself.

```bash
curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nodejs
sudo corepack enable
sudo corepack prepare pnpm@10.34.5 --activate

node --version
pnpm --version
```

## 5. Optional: install PowerShell 7

Ubuntu 22.04 and 24.04 can install PowerShell from Microsoft's package feed.
The repository can otherwise be tested with the native commands in the next
section.

```bash
source /etc/os-release
curl -fLO "https://packages.microsoft.com/config/ubuntu/${VERSION_ID}/packages-microsoft-prod.deb"
sudo dpkg -i packages-microsoft-prod.deb
rm packages-microsoft-prod.deb
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y powershell
pwsh --version
```

## 6. Fetch dependencies and run current tests

From the CodexMarathon repository root:

```bash
git submodule status || true

(cd controller && go mod download && go test ./...)
(cd integration && go mod download && go test ./...)
(cd runtime/codexmarathon-adapter && cargo test)

jq empty protocol/protocol.json
jq empty protocol/commands.json
jq empty protocol/events.json
test "$(tr -d '\r\n ' < protocol/VERSION)" = "1"
```

If PowerShell was installed, also run the repository verifier:

```bash
pwsh -NoProfile -File ./verify.ps1
```

## 7. Build the current components

```bash
mkdir -p build
(cd controller && go build -o ../build/codexmarathon-controller ./cmd/codexmarathon)
(cd runtime/codexmarathon-adapter && cargo build --release)
```

Build and test the tracked embedded runtime baseline with:

```bash
cd runtime/codex-rs
cargo build -p codex-cli -p codexmarathon-runtime
cargo test -p codex-app-server-protocol
# Run the full suite only after focused packages pass:
cargo nextest run
```

## 8. Formatting and quality checks

```bash
find controller -name '*.go' -type f -print0 | xargs -0 gofmt -w
find integration -name '*.go' -type f -print0 | xargs -0 gofmt -w
(cd controller && go vet ./...)
(cd integration && go vet ./...)
(cd runtime/codexmarathon-adapter && cargo fmt --check && cargo clippy --all-targets -- -D warnings)
```

For the imported Codext runtime, follow its in-tree `AGENTS.md` and upstream
reapply guardrails. Some donor branches intentionally restrict formatting or
test changes during an upstream reapply; repository instructions take priority.

## 9. Recommended machine capacity

- 4 CPU cores minimum; 8+ recommended.
- 8 GB RAM minimum; 16 GB recommended for full Rust workspace tests.
- At least 30 GB free disk for Rust targets, caches, and parallel builds.
- Ubuntu 22.04/24.04 x86_64 or arm64.
