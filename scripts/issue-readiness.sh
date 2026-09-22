#!/usr/bin/env bash
# Mechanical readiness check for an issue, before orchestrate dispatches it.
#
# The orchestrator must not investigate code itself (its context stays small),
# but it has to know when an issue is too thin or too old to hand to an
# executor: issues synced from the roadmap without triage failed verification
# nearly three times as often and let five times as many defects through to
# CodeRabbit. This script only reads the issue text. An issue it rejects goes
# to an issue-refiner subagent, which does the investigation.
#
# Usage: scripts/issue-readiness.sh <issue-number>
# Prints READY or NOT-READY plus one reason per line; paths the body names
# that do not exist in this checkout are listed as warnings (new files are
# legitimate, so they never make an issue NOT-READY on their own).
# Exit: 0 ready, 1 not ready, 2 usage or gh failure.
set -euo pipefail

REPO=${GH_REPO:-mdg-labs/hoserva}

die() { printf '%s\n' "$*" >&2; exit 2; }

[[ $# -eq 1 && $1 =~ ^[0-9]+$ ]] || die "usage: $0 <issue-number>"
n=$1

json=$(gh issue view "$n" --repo "$REPO" --json body,labels,state) || die "issue-readiness: gh issue view $n failed"
body=$(jq -r '.body // ""' <<<"$json")
labels=$(jq -r '[.labels[].name] | join(" ")' <<<"$json")

has_label() { [[ " $labels " == *" $1 "* ]]; }

if has_label epic; then
  printf '#%s READY\n- epic: not dispatched itself\n' "$n"
  exit 0
fi

reasons=()
section() { grep -qE "^##[[:space:]]+$1" <<<"$body"; }

section 'Original report' || reasons+=("no '## Original report' — never triaged (roadmap-synced or hand-written)")
section 'Acceptance criteria' || reasons+=("no '## Acceptance criteria'")
if has_label feat || has_label bug || has_label chore; then
  section 'Out of scope' || reasons+=("no '## Out of scope'")
fi
if has_label feat || has_label bug; then
  grep -qiE 'Reachable via' <<<"$body" || reasons+=("no 'Reachable via:' criterion — nothing says where the capability must be wired")
fi

stale_patterns=(
  'beta branch|origin/beta|beta-part|push(es|ed)? to .?beta|beta → main|beta -> main'
  '\bL4\b|hardware tier|needs-hardware|real hardware test'
  'Community Applications|AppFeed|Squidly271'
)
stale_labels=('names the retired beta branch (Q46: dev/main)' 'names a hardware test tier (D20: none exists)' 'references the Unraid CA feed (D19: own catalog)')
# A line that cites the decision or negates the term ("D20: no hardware
# tier", "there is no beta branch") states the current design, not a stale
# assumption.
for i in "${!stale_patterns[@]}"; do
  if grep -iE "${stale_patterns[$i]}" <<<"$body" | grep -qviE 'D19|D20|Q46|\bno\b|\bnot\b|never|retired|removed'; then
    reasons+=("stale: ${stale_labels[$i]}")
  fi
done

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
# shellcheck disable=SC2016 # the backticks are literal Markdown, not a substitution
path_re='`(cmd|internal|web|api|scripts|packaging|docs|testdata|templates|spikes|site|\.github)/[^` ]+`'
missing=()
while IFS= read -r p; do
  p=${p%%[*{<…:#]*}
  p=${p%/}
  [[ -z $p || -e "$root/$p" ]] || missing+=("$p")
done < <(grep -oE "$path_re" <<<"$body" | tr -d '`' | sort -u)

if ((${#reasons[@]})); then
  printf '#%s NOT-READY\n' "$n"
  printf -- '- %s\n' "${reasons[@]}"
  status=1
else
  printf '#%s READY\n' "$n"
  status=0
fi
if ((${#missing[@]})); then
  printf -- '- warning: path not in this checkout: %s\n' "${missing[@]}"
fi
exit "$status"
