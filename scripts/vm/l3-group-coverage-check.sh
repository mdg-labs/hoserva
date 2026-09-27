#!/usr/bin/env bash
# Fails if run-l3-suite.sh's own L3_STEP_ORDER (issue #391) and
# nightly-l3.yml's own two parallel groups (issue #392, L3_GROUP_SPINDOWN/
# L3_GROUP_REST, defined right alongside it) have drifted apart: an id
# missing from both groups would silently never run in the split nightly,
# and an id present in both would run twice. No VM and no real
# HOSERVA_LAB_ID: run-l3-suite.sh's own L3_LIST_STEPS/L3_LIST_GROUPS modes
# answer both questions before vm_require_id ever runs.
#
# Run directly (no `make` target — this needs no VM, lab or Docker, so
# there is nothing a `make` wrapper would add) by nightly-l3.yml's own
# matrix-building job, ahead of creating either group's VM, and by an
# agent locally after touching L3_STEP_ORDER or either group list.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

mapfile -t all_steps < <(L3_LIST_STEPS=1 "$script_dir/run-l3-suite.sh")
mapfile -t group_lines < <(L3_LIST_GROUPS=1 "$script_dir/run-l3-suite.sh")

declare -A group_of=()
declare -A group_counts=()
fail=0

for line in "${group_lines[@]}"; do
  group=${line%% *}
  id=${line#* }
  group_counts[$group]=$(( ${group_counts[$group]:-0} + 1 ))
  if [[ -n "${group_of[$id]:-}" ]]; then
    echo "l3-group-coverage-check: '$id' is in more than one group (${group_of[$id]}, $group)" >&2
    fail=1
    continue
  fi
  group_of[$id]=$group
done

for id in "${all_steps[@]}"; do
  if [[ -z "${group_of[$id]:-}" ]]; then
    echo "l3-group-coverage-check: '$id' is in run-l3-suite.sh's own L3_STEP_ORDER but in neither nightly-l3.yml group (L3_GROUP_SPINDOWN/L3_GROUP_REST)" >&2
    fail=1
  fi
done

declare -A known_step=()
for id in "${all_steps[@]}"; do
  known_step[$id]=1
done
for id in "${!group_of[@]}"; do
  if [[ -z "${known_step[$id]:-}" ]]; then
    echo "l3-group-coverage-check: '$id' is grouped in nightly-l3.yml but is not a step id in run-l3-suite.sh's own L3_STEP_ORDER" >&2
    fail=1
  fi
done

if [[ "$fail" == "1" ]]; then
  exit 1
fi

echo "l3-group-coverage-check: every one of ${#all_steps[@]} step ids is in exactly one nightly-l3.yml group (spindown: ${group_counts[spindown]:-0}, rest: ${group_counts[rest]:-0})"
