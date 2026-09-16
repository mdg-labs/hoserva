#!/usr/bin/env bash
# Runs the whole spike, in order, against a freshly built standing lab
# array (`make lab-up`, default geometry). Fails loudly on the first
# non-zero exit, like spikes S1/S5/S6's own run-all.sh.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

for step in \
  01-setup.sh \
  02-workload-compare.sh \
  03-negative-control.sh \
  04-restart-remount.sh \
  05-overflow.sh \
  06-ext4-btrfs.sh
do
  s7_log ""
  s7_log "########## $step ##########"
  bash "$HERE/$step"
done

s7_log ""
s7_log "== cleaning up this spike's own working directory ($S7) — not touched by destroy-array.sh =="
# destroy-array.sh (run by `make lab-destroy`) only ever removes $LAB/img
# and $LAB/mnt — the extra ext4/btrfs images and their mounts this spike
# created are covered by that generic sweep, but $S7 (pidfiles, TSV
# journal logs, snapraid.conf) is this spike's own responsibility, exactly
# as spikes S5 and S6 document for their own equivalent subtrees.
rm -rf -- "$S7"

s7_log "spike S7 run-all.sh: all steps passed"
