#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${CODEXMARATHON_OUTPUT:-${repo_root}/dist/release}"
runtime_dir="${repo_root}/runtime/codex-rs"

mkdir -p "${output_dir}"
(
  cd "${runtime_dir}"
  cargo build --locked --release -p codex-cli
)

install -m 0755 "${runtime_dir}/target/release/codex" "${output_dir}/codex"
echo "Native CodexMarathon CLI created at ${output_dir}/codex."
