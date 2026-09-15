#!/usr/bin/env bash
# Settle gate (spike S1, issue #2 attempt 3, orchestrator requirement 3;
# Q31's acceptance procedure — the idle window must start only after disks
# have settled). Runs after the positive control and before the six-window
# acceptance matrix: an explicit `sync`, then samples disk1-disk3's
# /sys/block/<dev>/stat every 5s until every array disk has been unchanged
# for >= SETTLE_SECONDS, or fails loudly after MAX_WAIT_SECONDS without
# settling. The settle period is not assumed — every sample and every
# detected change is printed via stat-delta.sh, so the settle duration this
# run actually needed is in the committed log, not asserted.
#
# Usage (inside the lab container only):
#   bash /src/spikes/s1/scripts/settle-gate.sh <settle_seconds> <max_wait_seconds>
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
SETTLE=${1:?settle period in seconds required}
MAXWAIT=${2:?max wait in seconds required}
INTERVAL=5

snapshot() {
  echo "# snapshot $(date -u +%FT%TZ)"
  for img in disk1 disk2 disk3; do
    local dev base
    dev=$(losetup -j "$LAB/img/$img.img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
    if [ -z "$dev" ]; then
      echo "$img MISSING"
      continue
    fi
    base=$(basename "$dev")
    echo "$img $dev $(cat "/sys/block/$base/stat")"
  done
}

line_for() { # $1=snapshot text $2=disk
  echo "$1" | awk -v d="$2" '$1==d'
}

echo "=== SETTLE GATE started $(date -u +%FT%TZ) settle=${SETTLE}s max_wait=${MAXWAIT}s interval=${INTERVAL}s ==="
sync
prev=$(snapshot)
echo "=== POST-SYNC BASELINE ==="
echo "$prev"

stable=0
elapsed=0
while :; do
  sleep "$INTERVAL"
  elapsed=$((elapsed + INTERVAL))
  cur=$(snapshot)
  echo "=== T+${elapsed}s $(date -u +%FT%TZ) ==="
  echo "$cur"

  changed=0
  for disk in disk1 disk2 disk3; do
    b=$(line_for "$prev" "$disk")
    a=$(line_for "$cur" "$disk")
    if [ "$b" != "$a" ]; then
      changed=1
      echo "-- $disk changed --"
      bash /src/spikes/s1/scripts/stat-delta.sh "$b" "$a"
    fi
  done

  if [ "$changed" -ne 0 ]; then
    stable=0
  else
    stable=$((stable + INTERVAL))
  fi
  echo "stable_for=${stable}s elapsed=${elapsed}s"
  prev="$cur"

  if [ "$stable" -ge "$SETTLE" ]; then
    echo "=== SETTLED: array disks unchanged for ${stable}s (required ${SETTLE}s), total wait ${elapsed}s, $(date -u +%FT%TZ) ==="
    exit 0
  fi
  if [ "$elapsed" -ge "$MAXWAIT" ]; then
    echo "=== SETTLE GATE FAILED: not settled within ${MAXWAIT}s (last stable streak ${stable}s) ===" >&2
    exit 1
  fi
done
