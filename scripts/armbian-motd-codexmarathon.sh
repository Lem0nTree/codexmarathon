#!/bin/bash

# Show the credential-safe CodexMarathon account summary in Armbian's MOTD.
# Keep SSH login responsive if Codex is unavailable or its state is busy.

codex_bin=/home/pi/.local/bin/codex
[[ -x "$codex_bin" ]] || exit 0

if (( EUID == 0 )); then
    pi_runtime=/run/user/$(id -u pi)
    status=$(runuser -u pi -- env XDG_RUNTIME_DIR="$pi_runtime" timeout 3s "$codex_bin" marathon accounts --daemon --format motd 2>/dev/null) || exit 0
else
    status=$(timeout 3s "$codex_bin" marathon accounts --daemon --format motd 2>/dev/null) || exit 0
fi

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
done <<< "$status"

exit 0
