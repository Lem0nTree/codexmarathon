#!/bin/sh
set -eu

# Linux ARM64 cannot build Codex's V8-backed code-mode host locally because
# rusty_v8 does not publish the required archive for this target. Keep this
# fallback pinned to the exact upstream release that this source tree embeds.
asset_url='https://github.com/openai/codex/releases/download/rust-v0.160.0/codex-code-mode-host-aarch64-unknown-linux-musl.tar.gz'
asset_sha256='94066fdf13ffecd2f58776ec5cb8fc3040283a642e3d6ff25c5d82048c41038e'
asset_member='codex-code-mode-host-aarch64-unknown-linux-musl'

die() {
    printf 'CodexMarathon code-mode host: %s\n' "$1" >&2
    exit 1
}

[ "$#" -eq 1 ] || die 'usage: fetch-code-mode-host.sh DESTINATION'
destination=$1
destination_dir=${destination%/*}
[ "$destination_dir" != "$destination" ] || destination_dir=.
[ -d "$destination_dir" ] || die "destination directory does not exist: $destination_dir"

command -v curl >/dev/null 2>&1 || die 'curl is required to download the ARM64 code-mode host.'
command -v sha256sum >/dev/null 2>&1 || die 'sha256sum is required to verify the ARM64 code-mode host.'
command -v tar >/dev/null 2>&1 || die 'tar is required to unpack the ARM64 code-mode host.'
command -v mktemp >/dev/null 2>&1 || die 'mktemp is required to unpack the ARM64 code-mode host.'

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/codexmarathon-code-mode-host.XXXXXX") ||
    die 'could not create a temporary directory.'
cleanup() {
    rm -rf -- "$work_dir"
}
trap cleanup EXIT HUP INT TERM

archive="$work_dir/codex-code-mode-host-aarch64-unknown-linux-musl.tar.gz"
extract_dir="$work_dir/extract"
mkdir "$extract_dir"

curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
    --output "$archive" "$asset_url" || die 'could not download the pinned ARM64 code-mode host.'
printf '%s  %s\n' "$asset_sha256" "$archive" | sha256sum --check --status ||
    die 'the downloaded ARM64 code-mode host failed SHA-256 verification.'

archive_members=$(tar -tzf "$archive") ||
    die 'the downloaded ARM64 code-mode host is not a valid tar.gz archive.'
[ "$archive_members" = "$asset_member" ] ||
    die "the ARM64 code-mode host archive must contain exactly $asset_member."

tar -xzf "$archive" -C "$extract_dir" -- "$asset_member" ||
    die 'could not extract the verified ARM64 code-mode host.'
source_binary="$extract_dir/$asset_member"
[ -f "$source_binary" ] && [ ! -L "$source_binary" ] && [ -x "$source_binary" ] ||
    die 'the verified ARM64 code-mode host archive did not contain an executable file.'

temporary_destination=$(mktemp "$destination_dir/.$(basename "$destination").XXXXXX") ||
    die 'could not create a temporary destination for the ARM64 code-mode host.'
if ! install -m 0755 "$source_binary" "$temporary_destination"; then
    rm -f -- "$temporary_destination"
    die 'could not stage the ARM64 code-mode host.'
fi
if ! mv -fT -- "$temporary_destination" "$destination"; then
    rm -f -- "$temporary_destination"
    die 'could not install the ARM64 code-mode host.'
fi
