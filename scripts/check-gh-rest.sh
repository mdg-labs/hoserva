#!/usr/bin/env bash
# Fails on a GraphQL-backed `gh` subcommand, `--paginate`, or a
# non-repository-scoped search path anywhere under .claude/ or scripts/
# (issue #410) — the patterns a Claude Code cloud session's egress proxy
# refuses (GraphQL entirely, `search/issues`, `--paginate`'s
# `repositories/{id}/...` follow-on links). A skill or script that
# reintroduces one of these works on the maintainer's own machine and then
# breaks silently the moment it runs in a cloud session.
#
# Run by `make lint-gh` (CI's lint-and-unit job, .github/workflows/ci.yml).
set -euo pipefail

root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
allowlist="$root/scripts/gh-rest-allowlist.txt"

# Each pattern is an extended regex checked one line at a time.
patterns=(
  'gh +issue +(view|list|edit|create|comment|close|reopen)\>'
  'gh +pr +(view|list|create|edit|comment|merge|checks|status)\>'
  'gh +repo +view\>'
  'gh +label +list\>'
  'gh +release +list\>'
  'gh +api +graphql\>'
  'gh +api +search/'
  'gh +search\>'
  '\-\-search\>'
  '\-\-paginate\>'
)

# scripts/gh-rest-allowlist.txt lines are "<file>::<substring>" — a file
# whose flagged line also contains that exact substring is prose that
# *forbids* the command (CLAUDE.md's "Never `gh issue close`" and its
# siblings), not a live call. This is deliberately narrow: a new
# reintroduction of a forbidden pattern anywhere else in that same file
# still fails, because the substring has to actually be present on the
# flagged line.
declare -a allow_file=() allow_substr=()
if [[ -f $allowlist ]]; then
  while IFS= read -r entry; do
    [[ -z $entry || $entry == \#* ]] && continue
    [[ $entry == *"::"* ]] || continue
    allow_file+=("${entry%%::*}")
    allow_substr+=("${entry#*::}")
  done <"$allowlist"
fi

is_allowed() {
  local file=$1 line=$2 i
  for i in "${!allow_file[@]}"; do
    if [[ $file == "${allow_file[$i]}" && $line == *"${allow_substr[$i]}"* ]]; then
      return 0
    fi
  done
  return 1
}

fail=0
while IFS= read -r -d '' file; do
  rel=${file#"$root"/}
  line_no=0
  while IFS= read -r line || [[ -n $line ]]; do
    line_no=$((line_no + 1))
    for pat in "${patterns[@]}"; do
      if [[ $line =~ $pat ]]; then
        if is_allowed "$rel" "$line"; then
          continue
        fi
        printf 'check-gh-rest: %s:%d: forbidden pattern (%s): %s\n' \
          "$rel" "$line_no" "$pat" "$line" >&2
        fail=1
      fi
    done
  done <"$file"
done < <(find "$root/.claude" "$root/scripts" -type f \
  \( -name '*.md' -o -name '*.sh' -o -name '*.yml' -o -name '*.yaml' \) \
  -not -path "$root/scripts/testdata/*" \
  -not -name 'check-gh-rest.sh' \
  -not -name 'test-check-gh-rest.sh' \
  -not -name 'gh-rest-allowlist.txt' \
  -print0)

exit "$fail"
