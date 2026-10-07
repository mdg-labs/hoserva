#!/usr/bin/env bash
# Lists the earlier commits touching the given paths whose `Fixes #n` issue
# carries the `security` label, so an executor or verifier sees the guards
# earlier security fixes added before it edits the same files.
#
# Usage: scripts/security-history.sh <path>...
# Run from inside the checkout whose history is read (a path is relative to
# it). Prints one line per commit, newest first:
#   <sha> <subject> (fixes #n: <issue title>)
# at most SECURITY_HISTORY_MAX lines (default 10), then a line saying how many
# more there are. Prints nothing and exits 0 when no commit on the paths
# fixes a security issue. Exit 2 on a usage error or when git or GitHub
# cannot be read: an unreadable history is an error, never "no history".
# Issues are read once, through scripts/gh-rest.sh (repository-scoped REST).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
MAX=${SECURITY_HISTORY_MAX:-10}
MAX_LINE=200

die() { printf 'security-history: %s\n' "$*" >&2; exit 2; }

[[ $# -ge 1 ]] || die "usage: $0 <path>..."
[[ $MAX =~ ^[1-9][0-9]*$ ]] || die "SECURITY_HISTORY_MAX must be a positive integer"

issues=$("$HERE/gh-rest.sh" issue-list --state all --label security \
  --jq '.[] | [.number, .title] | @tsv') \
  || die "could not list the issues labelled security"

declare -A title=()
while IFS=$'\t' read -r number issue_title; do
  [[ -n $number ]] && title[$number]=$issue_title
done <<<"$issues"

(( ${#title[@]} > 0 )) || exit 0

history=$(git log --format='%x1e%h%x1f%s%x1f%b' -- "$@") \
  || die "could not read the git history of: $*"

lines=()
while IFS= read -r -d $'\x1e' record; do
  [[ -n $record ]] || continue
  sha=${record%%$'\x1f'*}
  rest=${record#*$'\x1f'}
  subject=${rest%%$'\x1f'*}
  body=${rest#*$'\x1f'}
  while IFS= read -r body_line; do
    [[ $body_line =~ ^[Ff]ixes[[:space:]]+(mdg-labs/hoserva)?#([0-9]+)[[:space:]]*$ ]] || continue
    number=${BASH_REMATCH[2]}
    [[ -n ${title[$number]:-} ]] || continue
    line="$sha $subject (fixes #$number: ${title[$number]})"
    lines+=("${line:0:MAX_LINE}")
    break
  done <<<"$body"
done <<<"$history"$'\x1e'

(( ${#lines[@]} > 0 )) || exit 0

printf '%s\n' "${lines[@]:0:MAX}"
if (( ${#lines[@]} > MAX )); then
  printf '... and %d more\n' $(( ${#lines[@]} - MAX ))
fi
