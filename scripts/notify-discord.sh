#!/usr/bin/env bash
# Post a markdown message to the maintainer's Discord webhook.
#
# The webhook URL lives in ~/.claude/.env as DISCORD_WEBHOOK. This script is
# the only thing that ever reads it — a caller supplies a message file and
# never touches the secret directly, and this script never echoes it either,
# even on an error path.
set -euo pipefail

die() { printf '%s\n' "$*" >&2; exit 1; }

[[ $# -eq 1 ]] || die "usage: notify-discord.sh <path-to-markdown-message-file>"
[[ -f $1 ]] || die "no such file: $1"

env_file="$HOME/.claude/.env"
[[ -f $env_file ]] || die "missing $env_file — no DISCORD_WEBHOOK to read"

set -a
# shellcheck disable=SC1090
source "$env_file"
set +a

[[ -n ${DISCORD_WEBHOOK:-} ]] || die "DISCORD_WEBHOOK not set in $env_file"

message=$(cat "$1")

# Discord caps message content at 2000 chars; truncate rather than fail.
limit=1900
if (( ${#message} > limit )); then
  message="${message:0:limit}"$'\n\n…(truncated — see the full report in the session)'
fi

payload=$(jq -n --arg content "$message" '{content: $content}')

body_file=$(mktemp)
trap 'rm -f "$body_file"' EXIT

status=$(curl -sS -o "$body_file" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d "$payload" "$DISCORD_WEBHOOK")

if (( status < 200 || status >= 300 )); then
  die "Discord webhook returned HTTP $status: $(cat "$body_file")"
fi

printf 'Discord notified (HTTP %s)\n' "$status"
