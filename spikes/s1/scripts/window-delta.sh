#!/usr/bin/env bash
# Re-parses a committed run-window.sh log and prints stat-delta.sh's named,
# per-field deltas for disk1-disk3 between its BEFORE and AFTER snapshot
# (spike S1, doc 06 §6). Lets a reader check any window's counter claim
# against the raw committed log without parsing it by eye — the same
# script run-window.sh itself now uses inline for the DELTA section it
# prints during the run.
#
# Host-side text processing of already-captured lab output; touches no
# disk, mount or container. Usage:
#   spikes/s1/scripts/window-delta.sh <run-window.sh log file>
set -euo pipefail

log=${1:?usage: window-delta.sh <run-window.sh log file>}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

extract() { # $1=BEFORE|AFTER $2=disk
  awk -v marker="=== $1 ===" -v disk="$2" '
    $0 == marker { inblock=1; next }
    inblock && /^===/ { inblock=0 }
    inblock && $1 == disk { print; exit }
  ' "$log"
}

for disk in disk1 disk2 disk3; do
  before_line=$(extract BEFORE "$disk")
  after_line=$(extract AFTER "$disk")
  echo "--- $disk ---"
  if [ -z "$before_line" ] || [ -z "$after_line" ]; then
    echo "(missing BEFORE or AFTER line for $disk in $log)"
    continue
  fi
  "$here/stat-delta.sh" "$before_line" "$after_line"
done
