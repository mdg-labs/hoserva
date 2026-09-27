#!/usr/bin/env bash
# Builds nightly-l3.yml's own `l3` job matrix (issue #392). Two shapes:
#
# - L3_STEPS_INPUT set (a workflow_dispatch run with an explicit
#   selection, issue #391): one matrix entry, "selected", running exactly
#   that selection — the acceptance criterion that a manual selection
#   still runs as a single job, unsplit.
# - L3_STEPS_INPUT empty (the schedule trigger, or a workflow_dispatch run
#   left at its default): two entries, one per run-l3-suite.sh's own
#   L3_GROUP_SPINDOWN/L3_GROUP_REST (its L3_LIST_GROUPS mode) — the
#   spindown step's own ~34.5-minute observation window in one job,
#   everything else in a second, parallel job, so wall time tracks
#   whichever group takes longer rather than their sum.
#
# Prints `matrix=<json>` on stdout, meant for `>> "$GITHUB_OUTPUT"`. JSON
# is built with jq (present on every GitHub-hosted runner image), never by
# splicing L3_STEPS_INPUT — an arbitrary workflow_dispatch input — into a
# hand-built string.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

l3_steps_input="${L3_STEPS_INPUT:-}"

if [[ -n "$l3_steps_input" ]]; then
  matrix_json=$(jq -nc --arg steps "$l3_steps_input" \
    '{include: [{group: "selected", l3_steps: $steps, timeout: 105}]}')
else
  mapfile -t rest_ids < <(L3_LIST_GROUPS=1 "$script_dir/run-l3-suite.sh" | awk '$1 == "rest" {print $2}')
  rest_steps=$(
    IFS=,
    echo "${rest_ids[*]}"
  )
  # spindown: setup (~5min) + the observation window itself (~34.5min,
  # Q13 — never shortened), generously padded the same ~2x margin the
  # single-job suite's own 105-minute timeout keeps over its ~56-minute
  # measured total (run 36243538178). rest: every other step, measured at
  # ~16min combined in that same run, plus its own setup and the runner-
  # level libvirt/qemu/.deb-toolchain install this group now pays for on
  # its own separate runner (issue #392 splits that overhead too, not
  # only the guest-side steps) — padded further for that reason.
  matrix_json=$(jq -nc --arg rest "$rest_steps" \
    '{include: [{group: "spindown", l3_steps: "spindown", timeout: 75}, {group: "rest", l3_steps: $rest, timeout: 60}]}')
fi

printf 'matrix=%s\n' "$matrix_json"
