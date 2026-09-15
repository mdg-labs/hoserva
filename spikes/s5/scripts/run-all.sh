#!/usr/bin/env bash
# Runs every step of spike S5 (SnapRAID fidelity on loop devices) in order,
# against the *standing* array `make lab-up` built with this spike's
# geometry (LAB_DATA_SIZE=2G, LAB_PARITY_SIZE=3G — see spikes/s5/README.md).
# Each step fails loudly (set -euo pipefail throughout, every exit status
# checked) — this script does not continue past a failed step.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

if ! command -v snapraid >/dev/null 2>&1; then
  echo "run-all.sh: snapraid not found. It is not part of the base lab image" >&2
  echo "(scripts/devenv/Dockerfile) — install it in this lab container before" >&2
  echo "re-running (per CLAUDE.md's Spikes rule: never baked into the base image):" >&2
  echo "  apt-get update && apt-get install -y --no-install-recommends snapraid" >&2
  exit 1
fi

run_step() {
  local script=$1
  echo "##### s5/$script #####"
  bash "$HERE/$script"
}

run_step 01-seed-and-sync.sh
run_step 02-diff-and-sync.sh
run_step 03-touch-and-copy.sh
run_step 04-scrub-and-fix.sh
run_step 05-reconstruct-single-parity.sh
run_step 06-dual-parity-reconstruct.sh
echo "run-all.sh: PASS"
