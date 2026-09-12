#!/bin/sh
set -eu

repo='Lem0nTree/codexmarathon'
target_dir="${CODEXMARATHON_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s):$(uname -m)" in
  Linux:aarch64|Linux:arm64) suffix='linux-aarch64.tar.gz' ;;
  *) echo 'CodexMarathon currently publishes Linux ARM64 binaries only.' >&2; exit 1 ;;
esac

command -v curl >/dev/null || { echo 'curl is required.' >&2; exit 1; }
command -v python3 >/dev/null || { echo 'python3 is required.' >&2; exit 1; }
command -v tar >/dev/null || { echo 'tar is required.' >&2; exit 1; }

asset_url="$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | python3 -c "
import json, sys
suffix = '$suffix'
for asset in json.load(sys.stdin).get('assets', []):
    if asset['name'].endswith(suffix):
        print(asset['browser_download_url'])
        break
")"

[ -n "$asset_url" ] || { echo "No latest release asset matches $suffix." >&2; exit 1; }
mkdir -p "$target_dir"
curl -fsSL "$asset_url" | tar -xz -C "$target_dir"
printf 'Installed latest CodexMarathon to %s/codex\n' "$target_dir"
