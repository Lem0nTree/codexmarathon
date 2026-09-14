#!/bin/sh
set -eu

# Install one unpacked CodexMarathon release. This script is shipped inside the
# release archive and is also used by scripts/install.sh.
package_dir=${1:-.}
cli_dir=${CODEXMARATHON_INSTALL_DIR:-$HOME/.local/bin}
daemon_dir=$HOME/.local/bin
unit_dir=$HOME/.config/systemd/user
unit_dropin_dir=$unit_dir/codexmarathon-accountd.service.d
unit_dropin=$unit_dropin_dir/10-codex-home.conf
umask 077

die() {
    printf 'CodexMarathon install: %s\n' "$1" >&2
    exit 1
}

normalize_path() {
    normalized_path=$1
    while :; do
        case "$normalized_path" in
            *//* ) normalized_path=${normalized_path%%//*}/${normalized_path#*//} ;;
            *) break ;;
        esac
    done
    while [ "$normalized_path" != / ] \
        && [ "${normalized_path%/}" != "$normalized_path" ]; do
        normalized_path=${normalized_path%/}
    done
}

validate_codex_home() {
    path=$1
    [ -n "$path" ] || die 'CODEX_HOME must not be empty.'
    case "$path" in
        /*) ;;
        *) die "CODEX_HOME must be an absolute path: $path" ;;
    esac
    case "$path" in
        *[![:print:]]*) die 'CODEX_HOME must contain printable characters only.' ;;
        *%*) die 'CODEX_HOME must not contain % (reserved by systemd specifiers).' ;;
        */../*|*/..|*/./*|*/.) die 'CODEX_HOME must not contain . or .. path components.' ;;
        /) die 'CODEX_HOME must not be the filesystem root.' ;;
    esac
    [ "${#path}" -le 4096 ] || die 'CODEX_HOME is too long.'
}

validate_config_home() {
    path=$1
    [ -n "$path" ] || die 'XDG_CONFIG_HOME must not be empty.'
    case "$path" in
        /*) ;;
        *) die "XDG_CONFIG_HOME must be an absolute path: $path" ;;
    esac
    case "$path" in
        *[![:print:]]*) die 'XDG_CONFIG_HOME must contain printable characters only.' ;;
        */../*|*/..|*/./*|*/.) die 'XDG_CONFIG_HOME must not contain . or .. path components.' ;;
        /) die 'XDG_CONFIG_HOME must not be the filesystem root.' ;;
    esac
    [ "${#path}" -le 4096 ] || die 'XDG_CONFIG_HOME is too long.'
}

systemd_quote() {
    value=$1
    escaped=
    while [ -n "$value" ]; do
        character=${value%"${value#?}"}
        value=${value#?}
        case "$character" in
            \\) escaped="${escaped}\\\\" ;;
            \") escaped="${escaped}\\\"" ;;
            *) escaped="${escaped}${character}" ;;
        esac
    done
    printf '"%s"' "$escaped"
}

json_quote() {
    value=$1
    escaped=
    while [ -n "$value" ]; do
        character=${value%"${value#?}"}
        value=${value#?}
        case "$character" in
            \\) escaped="${escaped}\\\\" ;;
            \") escaped="${escaped}\\\"" ;;
            *) escaped="${escaped}${character}" ;;
        esac
    done
    printf '"%s"' "$escaped"
}

persist_home_config() {
    if [ -L "$config_dir" ]; then
        die 'persisted CodexMarathon config directory must not be a symbolic link.'
    fi
    mkdir -p "$config_dir" || die 'could not create persisted CodexMarathon config directory.'
    [ -d "$config_dir" ] || die 'persisted CodexMarathon config path is not a directory.'
    chmod 0700 "$config_dir" || die 'could not make persisted CodexMarathon config directory private.'

    config_tmp=$(mktemp "$config_dir/.config.json.XXXXXX") || \
        die 'could not create persisted CodexMarathon config file.'
    if ! printf '{"version":1,"codex_home":%s}\n' "$(json_quote "$codex_home")" > "$config_tmp"; then
        rm -f -- "$config_tmp"
        die 'could not write persisted CodexMarathon config file.'
    fi
    chmod 0600 "$config_tmp" || {
        rm -f -- "$config_tmp"
        die 'could not make persisted CodexMarathon config file private.'
    }
    if ! mv -f -- "$config_tmp" "$config_file"; then
        rm -f -- "$config_tmp"
        die 'could not atomically install persisted CodexMarathon config file.'
    fi
}

select_codex_home() {
    if [ "${CODEXMARATHON_CODEX_HOME+x}" = x ]; then
        normalize_path "$CODEXMARATHON_CODEX_HOME"
        selected=$normalized_path
        validate_codex_home "$selected"
        if [ "${CODEX_HOME+x}" = x ]; then
            normalize_path "$CODEX_HOME"
            codex_environment_home=$normalized_path
            validate_codex_home "$codex_environment_home"
            [ "$selected" = "$codex_environment_home" ] || \
                die 'CODEXMARATHON_CODEX_HOME and CODEX_HOME must resolve to the same path.'
        fi
    elif [ "${CODEX_HOME+x}" = x ]; then
        normalize_path "$CODEX_HOME"
        selected=$normalized_path
        validate_codex_home "$selected"
    else
        selected=$HOME/.codex
        normalize_path "$selected"
        selected=$normalized_path
        validate_codex_home "$selected"
    fi
    printf '%s' "$selected"
}

codex_home=$(select_codex_home)
normalize_path "$HOME/.codex"
default_codex_home=$normalized_path
validate_codex_home "$default_codex_home"
[ "$default_codex_home" != / ] || die 'HOME must not be the filesystem root.'
custom_codex_home=false
[ "$codex_home" = "$default_codex_home" ] || custom_codex_home=true

if [ "${XDG_CONFIG_HOME+x}" = x ]; then
    config_home=$XDG_CONFIG_HOME
else
    config_home=$HOME/.config
fi
normalize_path "$config_home"
config_home=$normalized_path
validate_config_home "$config_home"
config_dir=$config_home/codexmarathon
config_file=$config_dir/config.json

require_file() {
    [ -f "$package_dir/$1" ] || {
        printf 'CodexMarathon release is missing %s.\n' "$1" >&2
        exit 1
    }
}

require_file codex
require_file codexmarathon-accountd
require_file systemd/user/codexmarathon-accountd.service
require_file systemd/user/codexmarathon-accountd.socket
command -v systemctl >/dev/null 2>&1 || {
    echo 'systemctl is required to install codexmarathon-accountd.' >&2
    exit 1
}
systemctl --user show-environment >/dev/null
# Enable persistence for this user's manager so scheduling continues on a
# headless machine after the installing SSH session exits. Some distributions
# permit the user directly, while others require passwordless sudo. Do not
# report a successful unattended install unless persistence is guaranteed.
linger_enabled=false
if command -v loginctl >/dev/null 2>&1; then
    install_user=$(id -un)
    if [ "$(loginctl show-user "$install_user" -p Linger --value 2>/dev/null || true)" = yes ]; then
        linger_enabled=true
    elif loginctl enable-linger "$install_user" >/dev/null 2>&1; then
        linger_enabled=true
    elif command -v sudo >/dev/null 2>&1 \
        && sudo -n loginctl enable-linger "$install_user" >/dev/null 2>&1; then
        linger_enabled=true
    fi
fi
[ "$linger_enabled" = true ] || {
    printf '%s\n' \
        'Cannot enable user lingering automatically; accountd was not installed.' >&2
    exit 1
}

mkdir -p "$cli_dir" "$daemon_dir" "$unit_dir"
mkdir -p "$codex_home/marathon"
chmod 0700 "$codex_home" "$codex_home/marathon"
systemctl --user stop codexmarathon-accountd.socket codexmarathon-accountd.service \
    >/dev/null 2>&1 || true

for binary in codex codex-code-mode-host codex-responses-api-proxy bwrap; do
    if [ -f "$package_dir/$binary" ]; then
        install -m 0755 "$package_dir/$binary" "$cli_dir/$binary"
    fi
done
install -m 0755 "$package_dir/codexmarathon-accountd" \
    "$daemon_dir/codexmarathon-accountd"
install -m 0644 "$package_dir/systemd/user/codexmarathon-accountd.service" \
    "$unit_dir/codexmarathon-accountd.service"
install -m 0644 "$package_dir/systemd/user/codexmarathon-accountd.socket" \
    "$unit_dir/codexmarathon-accountd.socket"

if [ "$custom_codex_home" = true ]; then
    mkdir -p "$unit_dropin_dir"
    override_tmp=$(mktemp "$unit_dropin_dir/.10-codex-home.conf.XXXXXX")
    umask 077
    {
        printf '[Service]\n'
        printf 'Environment=%s\n' "$(systemd_quote "CODEX_HOME=$codex_home")"
        # Reset the base unit's allowlists before adding the custom state.
        printf 'ReadWritePaths=\n'
        printf 'ReadWritePaths=%s\n' "$(systemd_quote "$codex_home/marathon")"
        printf 'InaccessiblePaths=\n'
        printf 'InaccessiblePaths=%s %s\n' \
            "$(systemd_quote "$codex_home/auth.json")" \
            "$(systemd_quote "$codex_home/marathon/vault")"
    } > "$override_tmp"
    chmod 0600 "$override_tmp"
    mv -f "$override_tmp" "$unit_dropin"
else
    # A default-path upgrade must not retain an earlier custom override.
    rm -f -- "$unit_dropin"
    rmdir "$unit_dropin_dir" 2>/dev/null || true
fi

persist_home_config

systemctl --user daemon-reload
systemctl --user enable --now \
    codexmarathon-accountd.socket codexmarathon-accountd.service
systemctl --user is-active --quiet codexmarathon-accountd.socket
systemctl --user is-active --quiet codexmarathon-accountd.service
CODEX_HOME="$codex_home" "$cli_dir/codex" marathon accounts --daemon --format json >/dev/null

printf 'Installed CodexMarathon CLI to %s/codex\n' "$cli_dir"
printf 'Installed and started codexmarathon-accountd for user %s\n' "$(id -un)"
if [ "$custom_codex_home" = true ]; then
    printf 'Configured accountd CODEX_HOME as %s\n' "$codex_home"
fi
