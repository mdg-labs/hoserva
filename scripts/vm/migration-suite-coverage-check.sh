#!/usr/bin/env bash
# Fails if the migration suite's three records of what it runs have drifted apart
# (doc 06 §5, §7, issue #80): the runs run-migration-suite.sh lists
# (MIGRATION_RUNS), the fixture variants committed under testdata/unraid-fixtures/
# and the matrix of .github/workflows/nightly-migration.yml. A variant without a
# run would never be tested; a run without a fixture, or a matrix entry the
# script does not know, would fail at the first step of a nightly. No VM, no
# HOSERVA_LAB_ID: the script's MIGRATION_LIST_RUNS mode answers before it needs one.
#
# Run directly (no make target: it needs no VM, lab or Docker) by
# nightly-migration.yml's own coverage job ahead of any VM, and by an agent after
# touching MIGRATION_RUNS, the fixtures or the workflow's matrix.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
workflow="$repo_root/.github/workflows/nightly-migration.yml"
fixtures="$repo_root/testdata/unraid-fixtures"
fail=0

bad() {
  echo "migration-suite-coverage-check: $*" >&2
  fail=1
}

mapfile -t runs < <(MIGRATION_LIST_RUNS=1 "$script_dir/run-migration-suite.sh")
((${#runs[@]} > 0)) || {
  echo "migration-suite-coverage-check: run-migration-suite.sh lists no run" >&2
  exit 1
}

# every committed fixture but the shared files and unraid-with-vms (#97) has a run
declare -A has_separate=()
declare -A seen_run=()
for run in "${runs[@]}"; do
  read -r variant layout extra <<<"$run"
  if [[ -n "$extra" || -z "$layout" ]]; then
    bad "malformed run '$run' (want '<variant> <layout>')"
    continue
  fi
  case "$layout" in
    separate | shared-nvme) ;;
    *) bad "run '$run' has an unknown layout '$layout'" ;;
  esac
  if [[ -n "${seen_run[$run]:-}" ]]; then bad "run '$run' is listed twice"; fi
  seen_run[$run]=1
  [[ -d "$fixtures/$variant" ]] || bad "run '$run' names a fixture that is not under testdata/unraid-fixtures/"
  [[ "$layout" != separate ]] || has_separate[$variant]=1
done
for dir in "$fixtures"/*/; do
  variant=$(basename "$dir")
  case "$variant" in common | unraid-with-vms) continue ;; esac
  [[ -n "${has_separate[$variant]:-}" ]] || bad "the fixture '$variant' has no run in run-migration-suite.sh's MIGRATION_RUNS"
done

# the workflow's matrix: each entry is `variant: <v>` then `layout: <l>`
[[ -f "$workflow" ]] || {
  echo "migration-suite-coverage-check: $workflow does not exist" >&2
  exit 1
}
mapfile -t matrix < <(awk '
  $1 == "variant:" { v = $2 }
  $1 == "layout:" && v != "" { print v " " $2; v = "" }' "$workflow")
declare -A in_matrix=()
for entry in "${matrix[@]}"; do
  if [[ -n "${in_matrix[$entry]:-}" ]]; then bad "the workflow's matrix lists '$entry' twice"; fi
  in_matrix[$entry]=1
  [[ -n "${seen_run[$entry]:-}" ]] || bad "the workflow's matrix runs '$entry', which MIGRATION_RUNS does not list"
done
for run in "${runs[@]}"; do
  [[ -n "${in_matrix[$run]:-}" ]] || bad "MIGRATION_RUNS lists '$run', which the workflow's matrix does not run"
done

# the one job name the release checklist (#107) requires
grep -qx '    name: Migration suite' "$workflow" || bad "the workflow has no job named exactly 'Migration suite' (the aggregate the release checklist requires)"

if ((fail)); then exit 1; fi
echo "migration-suite-coverage-check: ${#runs[@]} runs: every fixture has one and the workflow's matrix runs exactly these"
