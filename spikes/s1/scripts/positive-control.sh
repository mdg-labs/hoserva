#!/usr/bin/env bash
# Positive control for the S1 per-disk fatrace attribution mechanism
# (spike S1, doc 06 §6, doc 08 §1). Runs once, before the acceptance
# matrix, and confirms two separate things:
#
#   1. Direct-disk probe: a write+read directly under one array disk's real
#      mountpoint is captured by that disk's own `fatrace -c` ("current
#      mount") instance, attributed by PID and process name, and does NOT
#      appear in either of the other two disks' instances (no
#      cross-contamination between per-disk FAN_MARK_FILESYSTEM scopes).
#   2. Pool-routed probe: a write+read through the mergerfs pool mount
#      ($LAB/mnt/user, not a disk's own mountpoint) is captured by the
#      tracer of whichever disk mergerfs's create policy actually placed it
#      on, attributed to the `mergerfs` process, and does NOT appear in
#      either of the other two disks' tracers. Attempt 2's positive control
#      only covered case 1 — a verifier reviewing attempt 2 independently
#      confirmed case 2 holds in their own lab, but the recorded evidence
#      didn't demonstrate it; this closes that gap.
#
# This lab's capability set lacks CAP_DAC_READ_SEARCH (docker-compose.dev.yml
# grants only SYS_ADMIN), so fatrace cannot resolve a fanotify file handle to
# a path via open_by_handle_at(2) — every event's path prints as "(deleted)"
# regardless of what happened. Path-based attribution is therefore
# structurally unable to ever match in this lab; this control instead proves
# PID/process-name attribution scoped per-disk, which needs no path
# resolution. A tracer that fails this control cannot be trusted for the
# acceptance matrix (CLAUDE.md "Spikes": a spike that cannot reach a verdict
# says so, it does not guess).
#
# Usage (inside the lab container only, pool mount already up from
# `make lab-up`):
#   bash /src/spikes/s1/scripts/positive-control.sh
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
DIR="$LAB/fatrace/positive-control"

rm -rf "$DIR"
mkdir -p "$DIR"

echo "=== POSITIVE CONTROL started $(date -u +%FT%TZ) ==="

fatrace_pids=()
for disk in disk1 disk2 disk3; do
  ( cd "$LAB/mnt/$disk" && fatrace -c -t -u -s 8 -f CROW -o "$DIR/$disk.log" ) \
    2>"$DIR/$disk.err" &
  fatrace_pids+=("$disk:$!")
done
sleep 1

echo "probe" > "$LAB/mnt/disk2/positive-control-probe.txt"
cat "$LAB/mnt/disk2/positive-control-probe.txt" >/dev/null
rm -f "$LAB/mnt/disk2/positive-control-probe.txt"

fatal=0
for entry in "${fatrace_pids[@]}"; do
  disk=${entry%%:*}
  pid=${entry##*:}
  wait "$pid"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FATAL: fatrace on $disk exited $rc during positive control" >&2
    cat "$DIR/$disk.err" >&2
    fatal=1
  fi
done
if [ "$fatal" -ne 0 ]; then
  echo "=== POSITIVE CONTROL FAILED — tracer(s) did not run, matrix cannot proceed ===" >&2
  exit 1
fi

for disk in disk1 disk2 disk3; do
  echo "=== $disk.log ==="
  cat "$DIR/$disk.log" 2>/dev/null || echo "(missing)"
  echo "=== $disk.err ==="
  cat "$DIR/$disk.err" 2>/dev/null || echo "(missing)"
done

d1_lines=$(wc -l < "$DIR/disk1.log")
d2_lines=$(wc -l < "$DIR/disk2.log")
d3_lines=$(wc -l < "$DIR/disk3.log")

echo "=== VERDICT ==="
echo "disk1 lines: $d1_lines (expect 0 — probe was written to disk2)"
echo "disk2 lines: $d2_lines (expect > 0 — the probe write+read)"
echo "disk3 lines: $d3_lines (expect 0 — probe was written to disk2)"

if [ "$d2_lines" -eq 0 ]; then
  echo "CONTROL FAILED: disk2's tracer recorded no events for the probe" >&2
  exit 1
fi
if [ "$d1_lines" -ne 0 ] || [ "$d3_lines" -ne 0 ]; then
  echo "CONTROL FAILED: cross-contamination — the disk2 probe leaked into disk1 or disk3" >&2
  exit 1
fi

echo "CONTROL PASSED: the probe on disk2 was attributed to disk2's tracer only"

# --- Part 2: pool-routed probe, through mergerfs, not a direct disk write ---
echo "=== POOL-ROUTED PROBE started $(date -u +%FT%TZ) ==="

POOL_DIR="$LAB/fatrace/positive-control-pool"
rm -rf "$POOL_DIR"
mkdir -p "$POOL_DIR"

pool_fatrace_pids=()
for disk in disk1 disk2 disk3; do
  ( cd "$LAB/mnt/$disk" && fatrace -c -t -u -s 8 -f CROW -o "$POOL_DIR/$disk.log" ) \
    2>"$POOL_DIR/$disk.err" &
  pool_fatrace_pids+=("$disk:$!")
done
sleep 1

echo "pool probe" > "$LAB/mnt/user/positive-control-pool-probe.txt"
cat "$LAB/mnt/user/positive-control-pool-probe.txt" >/dev/null

landed=""
for disk in disk1 disk2 disk3; do
  if [ -f "$LAB/mnt/$disk/positive-control-pool-probe.txt" ]; then
    landed="$disk"
  fi
done

rm -f "$LAB/mnt/user/positive-control-pool-probe.txt"

pool_fatal=0
for entry in "${pool_fatrace_pids[@]}"; do
  disk=${entry%%:*}
  pid=${entry##*:}
  wait "$pid"
  rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "FATAL: fatrace on $disk exited $rc during pool-routed probe" >&2
    cat "$POOL_DIR/$disk.err" >&2
    pool_fatal=1
  fi
done
if [ "$pool_fatal" -ne 0 ]; then
  echo "=== POOL-ROUTED PROBE FAILED — tracer(s) did not run, matrix cannot proceed ===" >&2
  exit 1
fi

for disk in disk1 disk2 disk3; do
  echo "=== pool probe: $disk.log ==="
  cat "$POOL_DIR/$disk.log" 2>/dev/null || echo "(missing)"
  echo "=== pool probe: $disk.err ==="
  cat "$POOL_DIR/$disk.err" 2>/dev/null || echo "(missing)"
done

echo "=== POOL-ROUTED PROBE VERDICT ==="
echo "landed on: ${landed:-UNKNOWN, probe file not found on any array disks real mountpoint}"

if [ -z "$landed" ]; then
  echo "CONTROL FAILED: pool-routed probe was not found on any array disk" >&2
  exit 1
fi

landed_lines=$(wc -l < "$POOL_DIR/$landed.log")
echo "$landed lines: $landed_lines (expect > 0 — mergerfs placed the probe here)"
if [ "$landed_lines" -eq 0 ]; then
  echo "CONTROL FAILED: $landed's tracer recorded no events for the pool-routed probe" >&2
  exit 1
fi

cross_contamination=0
for disk in disk1 disk2 disk3; do
  [ "$disk" = "$landed" ] && continue
  other_lines=$(wc -l < "$POOL_DIR/$disk.log")
  echo "$disk lines: $other_lines (expect 0 — mergerfs placed the probe on $landed, not $disk)"
  if [ "$other_lines" -ne 0 ]; then
    cross_contamination=1
  fi
done
if [ "$cross_contamination" -ne 0 ]; then
  echo "CONTROL FAILED: cross-contamination — the pool-routed probe on $landed leaked into another disk's tracer" >&2
  exit 1
fi

if ! grep -q "mergerfs" "$POOL_DIR/$landed.log"; then
  echo "CONTROL FAILED: the pool-routed probe on $landed was not attributed to the mergerfs process" >&2
  exit 1
fi

echo "CONTROL PASSED: the pool-routed probe landed on $landed via mergerfs's create policy and was attributed to $landed's tracer only, as the mergerfs process"
echo "=== POSITIVE CONTROL finished $(date -u +%FT%TZ) ==="
