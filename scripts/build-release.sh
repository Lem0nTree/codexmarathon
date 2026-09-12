#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${CODEXMARATHON_OUTPUT:-${repo_root}/dist/release}"
runtime_dir="${CODEXMARATHON_RUNTIME_DIR:-${repo_root}/runtime/codex-rs}"
target="${CODEXMARATHON_TARGET:-$(rustc -vV | awk '/^host:/ {print $2}')}"
version="${CODEXMARATHON_VERSION:-codexmarathon-dev}"

case "$target" in
  aarch64-unknown-linux-gnu)
    # rusty_v8 does not publish the sandboxed code-mode-host archive for Linux ARM64.
    # The primary Codex CLI and its sandbox helper remain fully native on this target.
    binaries=(codex bwrap)
    ;;
  *linux*)
    binaries=(codex codex-code-mode-host codex-responses-api-proxy bwrap)
    ;;
  *)
    echo "scripts/build-release.sh supports Linux targets; use build-release.ps1 on Windows." >&2
    exit 64
    ;;
esac

mkdir -p "${output_dir}"
(
  cd "${runtime_dir}"
  cargo build --locked --release --target "$target" --bin bwrap
  strip --strip-debug --strip-unneeded "target/${target}/release/bwrap"
  export CODEX_BWRAP_SHA256
  CODEX_BWRAP_SHA256="$(sha256sum "target/${target}/release/bwrap" | awk '{print $1}')"
  export STABLE_GIT_COMMIT="${CODEXMARATHON_UPSTREAM_COMMIT:-unknown}"
  cargo_build_args=(cargo build --locked --release --target "$target" --bin codex)
  if [[ "$target" != "aarch64-unknown-linux-gnu" ]]; then
    cargo_build_args+=(--bin codex-code-mode-host --bin codex-responses-api-proxy)
  fi
  "${cargo_build_args[@]}"
)

for binary in "${binaries[@]}"; do
  install -m 0755 "${runtime_dir}/target/${target}/release/${binary}" "${output_dir}/${binary}"
done

archive="${output_dir}/${version}-${target}.tar.gz"
tar -C "$output_dir" -czf "$archive" "${binaries[@]}"
"${output_dir}/codex" marathon --help >/dev/null
echo "CodexMarathon package created at $archive (${binaries[*]})."
