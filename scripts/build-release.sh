#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${CODEXMARATHON_VERSION:-0.1.0}"
platform="${CODEXMARATHON_PLATFORM:-linux-x86_64}"
output_dir="${CODEXMARATHON_OUTPUT:-${repo_root}/dist/release}"
build_dir="${repo_root}/dist/build"

mkdir -p "${build_dir}" "${output_dir}"
(
  cd "${repo_root}/controller"
  go build -trimpath -o "${build_dir}/codexmarathon" ./cmd/codexmarathon
)
(
  cd "${repo_root}/runtime/codex-rs"
  cargo build --locked --release -p codex-app-server
)

python3 "${repo_root}/scripts/package_release.py" \
  --root "${repo_root}" \
  --output "${output_dir}" \
  --controller-binary "${build_dir}/codexmarathon" \
  --runtime-binary "${repo_root}/runtime/codex-rs/target/release/codex-app-server" \
  --platform "${platform}" \
  --version "${version}" \
  --format tar.gz

echo "Release archive created under ${output_dir}. Run scripts/live_smoke.py separately to obtain live runtime evidence."
