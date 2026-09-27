#!/usr/bin/env bash
# Fails if run-l3-suite.sh's own L3_STEPS parser (issue #391) accepts a
# malformed selection instead of refusing it: a newline-separated list
# (only the first line would be read, silently dropping every later id) or
# an empty comma-separated entry. Also confirms a well-formed selection
# still resolves. No VM and no real HOSERVA_LAB_ID: run-l3-suite.sh's own
# L3_PLAN mode stops right after resolving the selection, before
# create-vm.sh ever runs.
#
# Run directly, like l3-group-coverage-check.sh, by nightly-l3.yml's own
# matrix-building job and by an agent locally after touching the parser.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail=0

plan() {
  HOSERVA_LAB_ID=l3-step-selection-check L3_PLAN=1 L3_STEPS="$1" \
    timeout 30 "$script_dir/run-l3-suite.sh" >/dev/null 2>&1
}

mapfile -t all_steps < <(L3_LIST_STEPS=1 "$script_dir/run-l3-suite.sh")
first_id=${all_steps[0]}
second_id=${all_steps[1]}

if ! plan "$first_id,$second_id"; then
  echo "l3-step-selection-check: well-formed selection '$first_id,$second_id' was refused" >&2
  fail=1
fi

for bad in \
  "$first_id"$'\n'"$second_id" \
  "$first_id"$'\r' \
  "$first_id,,$second_id" \
  ",$first_id" \
  "$first_id," \
  ","; do
  if plan "$bad"; then
    printf 'l3-step-selection-check: malformed selection %q was accepted\n' "$bad" >&2
    fail=1
  fi
done

exit "$fail"
