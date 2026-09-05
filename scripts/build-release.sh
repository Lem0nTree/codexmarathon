#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${CODEXMARATHON_VERSION:-0.1.0}"
if [[ -z "${CODEXMARATHON_PLATFORM:-}" ]]; then
  case "$(uname -m)" in
    x86_64) platform="linux-x86_64" ;;
    aarch64|arm64) platform="linux-aarch64" ;;
    *) echo "unsupported Linux architecture: $(uname -m)" >&2; exit 1 ;;
  esac
else
  platform="${CODEXMARATHON_PLATFORM}"
fi
output_dir="${CODEXMARATHON_OUTPUT:-${repo_root}/dist/release}"
build_dir="${repo_root}/dist/build"
include_runtime="${CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME:-0}"

mkdir -p "${build_dir}" "${output_dir}"
(
  cd "${repo_root}/controller"
  go build -trimpath -o "${build_dir}/codexmarathon" ./cmd/codexmarathon
)

package_args=(
  --root "${repo_root}"
  --output "${output_dir}"
  --controller-binary "${build_dir}/codexmarathon"
  --platform "${platform}"
  --version "${version}"
  --format tar.gz
)
if [[ "${include_runtime}" == "1" ]]; then
  (
    cd "${repo_root}/runtime/codex-rs"
    cargo build --locked --release -p codex-app-server
  )
  package_args+=(
    --include-embedded-runtime
    --runtime-binary "${repo_root}/runtime/codex-rs/target/release/codex-app-server"
  )
elif [[ "${include_runtime}" != "0" ]]; then
  echo "CODEXMARATHON_INCLUDE_EMBEDDED_RUNTIME must be 0 or 1" >&2
  exit 1
fi

python3 "${repo_root}/scripts/package_release.py" \
  "${package_args[@]}"

echo "Companion release archive created under ${output_dir}."
if [[ "${include_runtime}" == "1" ]]; then
  echo "Embedded runtime was included by explicit opt-in; run scripts/live_smoke.py separately for runtime evidence."
fi
