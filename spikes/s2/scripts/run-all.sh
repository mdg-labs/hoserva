#!/usr/bin/env bash
# Runs every step of spike S2 in order: build the fixture, run the
# measurement-integrity controls, verify adoption end to end, then
# characterize the dirty-log case on its own disposable disk. Each step
# fails loudly (set -euo pipefail throughout, every exit status checked) —
# this script does not continue past a failed step.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

# sfdisk ships in Debian's *fdisk* package, not util-linux — it is not in
# the base lab image (scripts/devenv/Dockerfile) and must be installed in
# this lab container for this spike only (CLAUDE.md's Spikes rule: never
# baked into the base image). Fail loudly with the exact fix rather than
# the opaque "sfdisk: command not found" every script here would otherwise
# hit partway through build-fixture.sh.
if ! command -v sfdisk >/dev/null 2>&1; then
  echo "run-all.sh: sfdisk not found. It ships in Debian's *fdisk* package," >&2
  echo "not util-linux. Install it in this lab container before re-running:" >&2
  echo "  apt-get update && apt-get install -y --no-install-recommends fdisk" >&2
  exit 1
fi

run_step() {
  local script=$1
  echo "##### s2/$script #####"
  bash "$HERE/$script"
}

run_step build-fixture.sh
run_step negative-controls.sh
run_step check-fixture.sh
run_step dirty-log.sh
echo "run-all.sh: PASS"
