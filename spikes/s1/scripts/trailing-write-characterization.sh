#!/usr/bin/env bash
# Characterizes deferred (trailing) writeback after a single file op on an
# otherwise-quiescent array disk, with NO explicit sync forcing it (spike
# S1, issue #2 attempt 3, orchestrator requirement 3). A previous attempt
# found a small write-side delta following a probe operation and explained
# it as deferred writeback without measuring the timing or magnitude of
# deferred writeback in isolation — this script measures it directly.
#
# Establishes a clean baseline with one `sync`, does a create+write+unlink
# probe directly on the disk's own real mountpoint (bypassing mergerfs —
# this measures the kernel/XFS layer, not the pool), then samples that
# disk's /sys/block/<dev>/stat every 5s for >= 6 minutes with NO further
# sync, printing a stat-delta.sh comparison against the baseline at every
# sample so the reader can see exactly when and by how much any field
# moves.
#
# Usage (inside the lab container only, /src is this workspace read-only):
#   bash /src/spikes/s1/scripts/trailing-write-characterization.sh <disk>
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
disk=${1:?disk required, e.g. disk1}

SAMPLE_INTERVAL=5
SAMPLE_COUNT=72 # 72 * 5s = 360s = 6 minutes, over the >= 5 minute floor

snapshot_one() {
  local dev base
  dev=$(losetup -j "$LAB/img/$disk.img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  if [ -z "$dev" ]; then
    echo "$disk MISSING"
    return
  fi
  base=$(basename "$dev")
  echo "$disk $dev $(cat "/sys/block/$base/stat")"
}

echo "=== TRAILING WRITE CHARACTERIZATION disk=$disk started=$(date -u +%FT%TZ) ==="
echo "--- kernel/XFS writeback timers (read-only, doc 08 S1) ---"
echo "fs.xfs.xfssyncd_centisecs: $(cat /proc/sys/fs/xfs/xfssyncd_centisecs 2>/dev/null || echo unavailable)"
echo "vm.dirty_expire_centisecs: $(cat /proc/sys/vm/dirty_expire_centisecs 2>/dev/null || echo unavailable)"
echo "vm.dirty_writeback_centisecs: $(cat /proc/sys/vm/dirty_writeback_centisecs 2>/dev/null || echo unavailable)"

sync
baseline=$(snapshot_one)
echo "=== BASELINE (post-sync, pre-probe) $(date -u +%FT%TZ) ==="
echo "$baseline"

echo "=== PROBE $(date -u +%FT%TZ): create+write+unlink on $LAB/mnt/$disk, no sync after ==="
echo "trailing-write-probe" > "$LAB/mnt/$disk/trailing-write-probe.txt"
cat "$LAB/mnt/$disk/trailing-write-probe.txt" >/dev/null
rm -f "$LAB/mnt/$disk/trailing-write-probe.txt"

for i in $(seq 1 "$SAMPLE_COUNT"); do
  sleep "$SAMPLE_INTERVAL"
  elapsed=$((i * SAMPLE_INTERVAL))
  cur=$(snapshot_one)
  echo "=== T+${elapsed}s $(date -u +%FT%TZ) ==="
  echo "$cur"
  bash /src/spikes/s1/scripts/stat-delta.sh "$baseline" "$cur"
done

echo "=== TRAILING WRITE CHARACTERIZATION finished $(date -u +%FT%TZ) ==="
