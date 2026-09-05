# Ubuntu development-machine setup

The normal CodexMarathon release is a Go companion for a separately installed
Codex CLI. Go, Python, Git, and the existing Codex executable are enough for
companion development and live handoff tests. Cargo is needed only when
working on the optional local-control adapter or building the explicitly
opt-in embedded-runtime package.

## 1. Install operating-system packages

```bash
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \
  build-essential ca-certificates curl file git jq pkg-config unzip xz-utils
```

The imported optional runtime may additionally need `clang`, `cmake`,
`libclang-dev`, `libssl-dev`, `libsqlite3-dev`, `protobuf-compiler`,
`libcap-dev`, `libseccomp-dev`, and `zlib1g-dev`.

## 2. Install Go

Use Go 1.22 or newer. The following installs the version used by the release
checks and maps the host architecture correctly:

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

## 3. Install and verify Codex

Install Codex through its normal distribution and ensure the executable is
available:

```bash
command -v codex
codex --version
```

If it is installed elsewhere, retain the absolute path and pass it to the
companion's `--codex` option for acceptance tests. Use a disposable
`CODEX_HOME` and test account when exercising authentication or process
handoff.

## 4. Run the companion checks

From the repository root:

```bash
(cd controller && go mod download && go test ./... && go vet ./...)
(cd integration && go mod download && go test ./...)
python3 scripts/test_package_release.py
python3 scripts/verify_provenance.py --root .
python3 scripts/verify_package.py --root .
python3 scripts/verify_protocol.py --root .
```

Build the normal Linux companion package:

```bash
bash scripts/build-release.sh
```

On an ARM64 machine this produces `linux-aarch64`; on amd64 it produces
`linux-x86_64`. The build compiles only the Go companion and does not invoke
Cargo.

## 5. Optional Rust adapter/runtime work

Install the pinned Rust toolchain only if you need the local-control adapter
or optional self-contained package:

```bash
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
  | sh -s -- -y --profile minimal --default-toolchain 1.95.0
source "$HOME/.cargo/env"
rustup component add --toolchain 1.95.0 rustfmt clippy rust-src

(cd runtime/codexmarathon-adapter && cargo test)
(cd runtime/codex-rs && cargo test --locked -p codexmarathon-runtime)
CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME=1 bash scripts/build-release.sh
```

The optional build can consume substantial temporary disk space because Cargo
retains intermediate objects. The resulting companion archive remains small;
the large Rust target tree is a build cache and is not included by default.

## 6. Formatting and repository verifier

```bash
find controller -name '*.go' -type f -print0 | xargs -0 gofmt -w
find integration -name '*.go' -type f -print0 | xargs -0 gofmt -w
```

On a machine with PowerShell 7, run:

```bash
pwsh -NoProfile -File ./verify.ps1
```

The verifier reports missing optional toolchains as `BLOCKED`; it does not
turn an unavailable Rust or live Codex check into a pass.

## Recommended capacity

- 2 CPU cores and 2 GB RAM are sufficient for the normal Go companion build.
- 4+ cores and 8 GB RAM are recommended for optional Rust compilation.
- Keep at least 5 GB free for normal tests and packaging.
- Keep at least 30 GB free only when compiling the full optional Codex runtime.
