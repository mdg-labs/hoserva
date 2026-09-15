#!/usr/bin/env bash
# Runs the full S1 acceptance matrix (spike S1, doc 06 §6, doc 08 §1):
# a trailing-write characterization, the positive control, a settle gate,
# then three scenarios (idle, appdata, smb) x two cache-timeout settings
# (default, raised), each a >= 30 minute window, sequentially so a fixed
# set of array disks never serves two scenarios at once (doc 06 §6 notes
# this is required when windows aren't run with separate disks). One run
# of this script produces every result file the acceptance matrix needs
# (spike S1 attempt 3, orchestrator requirement 5).
#
# Run only inside the lab container:
#   bash /src/spikes/s1/scripts/run-all.sh <duration_seconds> [settle_seconds] [max_wait_seconds]
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
DURATION=${1:?duration in seconds required}
SETTLE=${2:-180}
MAXWAIT=${3:-900}

mkdir -p "$LAB/results"

# Characterizes deferred writeback timing/magnitude on a quiescent disk
# before anything else touches the array, so the settle period used below
# is justified by a measurement rather than assumed (attempt 3,
# orchestrator requirement 3).
echo "=== TRAILING WRITE CHARACTERIZATION starting $(date -u +%FT%TZ) ==="
bash /src/spikes/s1/scripts/trailing-write-characterization.sh disk1 \
  > "$LAB/results/trailing-write-characterization.log" 2>"$LAB/results/trailing-write-characterization.err"
echo "=== TRAILING WRITE CHARACTERIZATION finished $(date -u +%FT%TZ) ==="

# The per-disk fatrace attribution mechanism (run-window.sh) must pass its
# own positive control before any window counts — a tracer that can't prove
# it sees IO on its own disk, and only its own disk, can't be trusted to
# report zero IO honestly either (rejected-attempt finding 1/2). Covers
# both a direct-disk probe and a pool-routed probe (attempt 3, orchestrator
# requirement 2).
echo "=== POSITIVE CONTROL starting $(date -u +%FT%TZ) ==="
bash /src/spikes/s1/scripts/positive-control.sh \
  > "$LAB/results/positive-control.log" 2>"$LAB/results/positive-control.err"
echo "=== POSITIVE CONTROL finished $(date -u +%FT%TZ) ==="

# Positive control's own probe writes leave the array in a state that
# needs settling before the acceptance matrix starts (attempt 3,
# orchestrator requirement 3; Q31's acceptance procedure).
echo "=== SETTLE GATE starting $(date -u +%FT%TZ) ==="
bash /src/spikes/s1/scripts/settle-gate.sh "$SETTLE" "$MAXWAIT" \
  > "$LAB/results/settle-gate.log" 2>"$LAB/results/settle-gate.err"
echo "=== SETTLE GATE finished $(date -u +%FT%TZ) ==="

# The acceptance bar (issue #2) is the full matrix, all 6 windows: three
# scenarios (idle, appdata, smb) x two cache-timeout settings (default,
# raised). An earlier draft of this script dropped idle:raised and
# appdata:raised as "architecturally redundant" on the theory that a
# mergerfs-FUSE-level cache-TTL setting cannot matter when nothing walks
# the pool tree. The maintainer overruled that: narrowing the acceptance
# bar is a maintainer call, not an executor one, and the mount is live in
# both cases regardless of workload, so it is exactly what the spike exists
# to measure rather than assume. All 6 run here.
for mode in default raised; do
  for scenario in idle appdata smb; do
    name="$scenario-$mode"
    echo "=== RUN $name starting $(date -u +%FT%TZ) ==="
    bash /src/spikes/s1/scripts/run-window.sh "$scenario" "$mode" "$DURATION" \
      > "$LAB/results/$name.log" 2>"$LAB/results/$name.err"
    echo "=== RUN $name finished $(date -u +%FT%TZ) ==="
  done
done

echo "=== ALL RUNS DONE $(date -u +%FT%TZ) ==="
