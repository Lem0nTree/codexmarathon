#!/bin/bash

# Show the credential-safe CodexMarathon account summary in Armbian's MOTD.
# Keep SSH login responsive if Codex is unavailable or its state is busy.

codex_bin=/home/pi/.local/bin/codex
[[ -x "$codex_bin" ]] || exit 0

if (( EUID == 0 )); then
    pi_runtime=/run/user/$(id -u pi)
    status=$(runuser -u pi -- env XDG_RUNTIME_DIR="$pi_runtime" timeout 3s "$codex_bin" marathon accounts --daemon --format json 2>/dev/null) || exit 0
else
    status=$(timeout 3s "$codex_bin" marathon accounts --daemon --format json 2>/dev/null) || exit 0
fi

# Classify windows by their provider-reported duration instead of assuming that
# every primary window is five hours. Pro accounts can expose only a weekly
# primary window (10,080 minutes).
rows=$(python3 - 3<<<"$status" <<'PY'
import datetime
import json
import os


def quota_left(window):
    if window is None:
        return "—"
    if window.get("freshness") == "stale":
        return "stale"
    used = window.get("used_percent")
    if used is None:
        return "—"
    return f"{max(0.0, 100.0 - float(used)):.1f}%"


def reset_in(window):
    if window is None or not window.get("resets_at"):
        return "—"
    reset = datetime.datetime.fromisoformat(window["resets_at"].replace("Z", "+00:00"))
    seconds = int((reset - datetime.datetime.now(datetime.timezone.utc)).total_seconds())
    if seconds <= 0:
        return "ready"
    hours, remainder = divmod(seconds, 3600)
    minutes = remainder // 60
    return f"{hours}h {minutes}m"


data = json.load(os.fdopen(3))
for account in data.get("accounts", []):
    windows = [
        window
        for limit in (account.get("quota") or {}).get("limits", {}).values()
        for window in limit.get("windows", [])
    ]

    short = next(
        (window for window in windows if window.get("window_duration_mins") == 300),
        None,
    )
    weekly = next(
        (window for window in windows if window.get("window_duration_mins") == 10080),
        None,
    )

    # Preserve compatibility with observations that predate duration metadata.
    if short is None:
        short = next(
            (
                window
                for window in windows
                if window.get("kind") == "primary"
                and window.get("window_duration_mins") in (None, 300)
            ),
            None,
        )
    if weekly is None:
        weekly = next(
            (
                window
                for window in windows
                if window.get("kind") == "secondary"
                and window.get("window_duration_mins") in (None, 10080)
            ),
            None,
        )

    fields = (
        str(account.get("alias", "—")),
        "yes" if account.get("active") else "no",
        str(account.get("credential_health", "unknown")),
        quota_left(short),
        quota_left(weekly),
        reset_in(short),
    )
    print("\t".join(field.replace("\t", " ").replace("\n", " ") for field in fields))
PY
) || exit 0

grey='\033[90m'
green='\033[92m'
reset='\033[0m'

printf '\n %bCodexMarathon accounts%b %b(accountd)%b\n' "$grey" "$reset" "$green" "$reset"
printf ' %b%-18s %-8s %-10s %-10s %-12s %-12s%b\n' "$grey" 'ACCOUNT' 'ACTIVE' 'HEALTH' '5H LEFT' 'WEEKLY LEFT' '5H RESET' "$reset"
printf ' %b%-18s %-8s %-10s %-10s %-12s %-12s%b\n' "$grey" '-------' '------' '------' '-------' '-----------' '--------' "$reset"

while IFS=$'\t' read -r alias active health short weekly short_reset; do
    [[ $alias == ACCOUNT ]] && continue
    printf ' %-18.18s ' "$alias"
    if [[ $active == yes ]]; then
        printf '%byes%b      ' "$green" "$reset"
    else
        printf '%-8s ' 'no'
    fi
    printf '%-10.10s %-10s %-12s %-12s\n' "${health,,}" "$short" "$weekly" "$short_reset"
done <<< "$rows"

exit 0
