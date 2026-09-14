#!/bin/sh
set -eu

# Lightweight installer acceptance harness. It uses command stubs instead of
# requiring a running user systemd manager, root, or a built release binary.
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/codexmarathon-install-test.XXXXXX")
trap 'rm -rf -- "$tmp_root"' EXIT HUP INT TERM

fail() {
    printf 'installer acceptance test: %s\n' "$1" >&2
    exit 1
}

make_executable() {
    file=$1
    body=$2
    printf '#!/bin/sh\n%s\n' "$body" > "$file"
    chmod 0755 "$file"
}

stub_bin=$tmp_root/bin
package_dir=$tmp_root/package
mkdir -p "$stub_bin" "$package_dir/systemd/user"
make_executable "$stub_bin/systemctl" 'exit 0'
make_executable "$stub_bin/loginctl" 'exit 0'
make_executable "$package_dir/codex" 'exit 0'
make_executable "$package_dir/codexmarathon-accountd" 'exit 0'
printf '%s\n' '[Unit]' 'Description=test accountd' '[Service]' 'Type=exec' \
    'ExecStart=/bin/true' > \
    "$package_dir/systemd/user/codexmarathon-accountd.service"
printf '%s\n' '[Unit]' 'Description=test accountd socket' > \
    "$package_dir/systemd/user/codexmarathon-accountd.socket"

export HOME=$tmp_root/home
export PATH=$stub_bin:$PATH
export CODEXMARATHON_INSTALL_DIR=$HOME/.local/bin
export XDG_CONFIG_HOME="$tmp_root/xdg config"
custom_home="$tmp_root/custom Codex \"Home\" \\state"
fallback_home="$tmp_root/fallback Codex Home"
override=$HOME/.config/systemd/user/codexmarathon-accountd.service.d/10-codex-home.conf

CODEX_HOME="$tmp_root//custom Codex \"Home\" \\state/" \
CODEXMARATHON_CODEX_HOME="$custom_home" \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null
[ -f "$override" ] || fail 'custom CODEX_HOME did not create the drop-in'
[ -d "$custom_home/marathon" ] || fail 'custom Marathon state directory was not created'
config_file=$XDG_CONFIG_HOME/codexmarathon/config.json
[ -f "$config_file" ] || fail 'custom home was not persisted'
grep -F '"version":1' "$config_file" >/dev/null || fail 'persisted config version is missing'
grep -F 'custom Codex' "$config_file" >/dev/null || fail 'persisted custom home is missing'
grep -F '\"Home\"' "$config_file" >/dev/null || fail 'persisted quote was not JSON-escaped'
grep -F '\\state' "$config_file" >/dev/null || fail 'persisted backslash was not JSON-escaped'
grep -F 'Environment="CODEX_HOME=' "$override" >/dev/null || \
    fail 'custom CODEX_HOME was not written to the drop-in'
grep -F 'ReadWritePaths=' "$override" >/dev/null || \
    fail 'drop-in did not reset the base write allowlist'
if command -v systemd-analyze >/dev/null 2>&1; then
    systemd-analyze verify "$HOME/.config/systemd/user/codexmarathon-accountd.service" \
        >/dev/null 2>&1 || fail 'generated drop-in is not valid systemd syntax'
fi
if command -v stat >/dev/null 2>&1; then
    [ "$(stat -c '%a' "$XDG_CONFIG_HOME/codexmarathon")" = 700 ] || \
        fail 'persisted config directory is not owner-private'
    [ "$(stat -c '%a' "$config_file")" = 600 ] || \
        fail 'persisted config file is not owner-private'
fi

env -u CODEXMARATHON_CODEX_HOME CODEX_HOME="$fallback_home" \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null
[ -d "$fallback_home/marathon" ] || fail 'CODEX_HOME fallback was not used'
grep -F 'fallback Codex Home' "$config_file" >/dev/null || \
    fail 'CODEX_HOME fallback was not persisted'

env -u CODEXMARATHON_CODEX_HOME -u CODEX_HOME \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null
[ ! -e "$override" ] || fail 'default upgrade retained the custom drop-in'
[ -d "$HOME/.codex/marathon" ] || fail 'default Marathon state directory was not created'
grep -F "$HOME/.codex" "$config_file" >/dev/null || \
    fail 'default home was not persisted'

env -u XDG_CONFIG_HOME -u CODEXMARATHON_CODEX_HOME -u CODEX_HOME \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null
[ -f "$HOME/.config/codexmarathon/config.json" ] || \
    fail 'persistence did not fall back to ~/.config'
grep -F "$HOME/.codex" "$HOME/.config/codexmarathon/config.json" >/dev/null || \
    fail 'fallback config did not persist the default home'

if CODEXMARATHON_CODEX_HOME="$custom_home" CODEX_HOME="$fallback_home" \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null 2>&1; then
    fail 'conflicting CODEX_HOME values were accepted'
fi

if CODEXMARATHON_CODEX_HOME=relative \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null 2>&1; then
    fail 'relative CODEX_HOME was accepted'
fi

if XDG_CONFIG_HOME=relative CODEXMARATHON_CODEX_HOME="$custom_home" \
    sh "$repo_root/scripts/install-release.sh" "$package_dir" >/dev/null 2>&1; then
    fail 'relative XDG_CONFIG_HOME was accepted'
fi

printf 'installer acceptance tests passed\n'
