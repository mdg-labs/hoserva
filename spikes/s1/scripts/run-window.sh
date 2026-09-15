#!/usr/bin/env bash
# Runs one S1 measurement window (spike S1, doc 06 §6, doc 08 §1).
#
# Remounts the lab pool with Hoserva's intended mergerfs options at the
# given cache-timeout setting, snapshots per-disk IO counters, runs the
# scenario's workload for the window, snapshots again, and attributes any
# IO that reached an array disk to a process via fatrace (fanotify).
#
# Usage (inside the lab container only — /src is the read-only bind mount
# of the workspace, doc 06 §3):
#   bash /src/spikes/s1/scripts/run-window.sh <idle|appdata|smb> <default|raised> <seconds>
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"

# Duplicated rather than sourced from snapshot.sh, which was written and
# tested as the standalone, directly runnable form before this script
# switched from stdin-piping to running from /src directly. Keep the two in
# sync by hand.
snapshot() {
  echo "# snapshot $(date -u +%FT%TZ)"
  for img in parity1 disk1 disk2 disk3 cache; do
    local dev base
    dev=$(losetup -j "$LAB/img/$img.img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
    if [ -z "$dev" ]; then
      echo "$img MISSING"
      continue
    fi
    base=$(basename "$dev")
    # 17 fields on this kernel (kernel Documentation/admin-guide/iostats.rst;
    # snapshot.sh has the full field-by-field list). Never parsed by eye
    # here — the DELTA section below uses stat-delta.sh on this same output.
    echo "$img $dev $(cat "/sys/block/$base/stat")"
  done
}

scenario=${1:?scenario required: idle|appdata|smb}
mode=${2:?mode required: default|raised}
duration=${3:?duration in seconds required}

case "$scenario" in idle|appdata|smb) ;; *) echo "unknown scenario: $scenario" >&2; exit 1 ;; esac
case "$mode" in default|raised) ;; *) echo "unknown mode: $mode" >&2; exit 1 ;; esac
[[ "$duration" =~ ^[0-9]+$ ]] || { echo "duration must be an integer" >&2; exit 1; }

echo "=== WINDOW scenario=$scenario mode=$mode duration=${duration}s started=$(date -u +%FT%TZ) ==="

# --- mount the pool with Hoserva's intended options (doc 02 §1 table) ---
COMMON_OPTS="category.create=mspmfs,moveonenospc=true,minfreespace=50M,dropcacheonclose=true,cache.files=partial,cache.statfs=0"
if [ "$mode" = "default" ]; then
  CACHE_OPTS="cache.entry=1,cache.attr=1,cache.negative_entry=1"
else
  CACHE_OPTS="cache.entry=600,cache.attr=600,cache.negative_entry=600"
fi
MOUNT_OPTS="$COMMON_OPTS,$CACHE_OPTS"

# Stop a smbd left running from a previous window, by the PID it recorded
# itself (never by name/pattern — see CLAUDE.md "kill by PID only").
prev_smbd_pid=$(cat "$LAB/smb/run/smbd.pid" 2>/dev/null || true)
if [ -n "$prev_smbd_pid" ] && kill -0 "$prev_smbd_pid" 2>/dev/null; then
  kill "$prev_smbd_pid"
  sleep 1
fi

if mountpoint -q "$LAB/mnt/user"; then
  umount "$LAB/mnt/user" || fusermount3 -u "$LAB/mnt/user"
fi
mergerfs -o "$MOUNT_OPTS" "$LAB/mnt/disk1:$LAB/mnt/disk2:$LAB/mnt/disk3" "$LAB/mnt/user"
echo "=== MOUNT ==="
grep " $LAB/mnt/user " /proc/mounts

# `sync` before BEFORE, not just once before the whole matrix (settle-gate.sh):
# attempt 2's idle-default window captured its BEFORE snapshot with a still-
# pending deferred write from the positive control's own probe, which then
# landed inside the window instead of before it (spike S1 attempt 3,
# orchestrator requirement 3; Q31's acceptance procedure — the window must
# start only after disks have settled). A sync here is a generic flush, not
# a storage-engine command, and forces this window's own baseline to be
# clean regardless of what came before it.
sync

echo "=== BEFORE ==="
before_snapshot=$(snapshot)
echo "$before_snapshot"

# --- attribution: one fatrace -c ("current mount") instance per array
# disk, each launched with its cwd on that disk's own mountpoint, rather
# than one whole-pool instance filtered by path afterward. This lab's
# capability set lacks CAP_DAC_READ_SEARCH (docker-compose.dev.yml grants
# only SYS_ADMIN; confirmed via /proc/1/status CapEff=0xa82425fb), so
# open_by_handle_at(2) cannot resolve a fanotify file handle to a path and
# every logged event's path prints as "(deleted)" regardless of whether real
# IO reached an array disk — path-based filtering is structurally unable to
# ever match here (docs/internal/08-spike-findings.md). Scoping each tracer
# to one disk's filesystem via FAN_MARK_FILESYSTEM (-c, with cwd set to that
# disk before fatrace starts) sidesteps path resolution entirely: any event
# a disk's own tracer records is IO on that disk by construction, attributed
# by PID and process name (fatrace shows both without resolving a path) —
# verified by scripts/positive-control.sh, which a probe write under one
# disk shows up only in that disk's own log and never the other two's.
mkdir -p "$LAB/fatrace"
fatrace_pids=()
for disk in disk1 disk2 disk3; do
  rm -f "$LAB/fatrace/$scenario-$mode-$disk.log" "$LAB/fatrace/$scenario-$mode-$disk.err"
  ( cd "$LAB/mnt/$disk" && fatrace -c -t -u -s "$duration" -f CROW -o "$LAB/fatrace/$scenario-$mode-$disk.log" ) \
    2>"$LAB/fatrace/$scenario-$mode-$disk.err" &
  fatrace_pids+=("$disk:$!")
done

# --- workload ---
workload_pid=""
case "$scenario" in
  idle)
    : # no workload; the array and the pool are untouched for the window
    ;;
  appdata)
    ( end=$((SECONDS + duration));
      mkdir -p "$LAB/mnt/cache/appdata";
      i=0;
      while [ "$SECONDS" -lt "$end" ]; do
        i=$((i + 1));
        head -c 4096 /dev/urandom > "$LAB/mnt/cache/appdata/proxy-$i.db";
        sleep 5;
      done ) &
    workload_pid=$!
    ;;
  smb)
    ( end=$((SECONDS + duration));
      mkdir -p "$LAB/mnt/cache/appdata";
      i=0;
      while [ "$SECONDS" -lt "$end" ]; do
        i=$((i + 1));
        head -c 4096 /dev/urandom > "$LAB/mnt/cache/appdata/proxy-$i.db";
        sleep 5;
      done ) &
    workload_pid=$!

    mkdir -p "$LAB/smb"
    # `load printers`/CUPS enumeration and reverse-DNS hostname lookups on
    # connect are classic smbd start-of-session stalls in a minimal
    # container with no CUPS and no usable resolver — disabled explicitly
    # so a connect doesn't block for a DNS or lpstat timeout.
    cat > "$LAB/smb/smb.conf" <<EOF
[global]
  workgroup = WORKGROUP
  security = user
  map to guest = Bad User
  guest account = nobody
  hostname lookups = no
  name resolve order = bcast
  disable netbios = yes
  load printers = no
  printing = bsd
  printcap name = /dev/null
  disable spoolss = yes
  log level = 1
  log file = $LAB/smb/smbd.log
  pid directory = $LAB/smb/run
  lock directory = $LAB/smb/run
  private dir = $LAB/smb/run
  state directory = $LAB/smb/run
  cache directory = $LAB/smb/run
  ncalrpc dir = $LAB/smb/run/ncalrpc
  smb ports = 445
[pool]
  path = $LAB/mnt/user
  guest ok = yes
  read only = no
  browseable = yes
EOF
    mkdir -p "$LAB/smb/run"
    # Two independent smbd gotchas, both found the hard way in this spike:
    # (1) smbd --daemon forks helper processes (notifyd, cleanupd) that
    # would otherwise inherit this script's stdout/stderr, and `docker
    # exec` then blocks waiting for that pipe to close even after smbd is
    # killed below — fixed by /dev/null and its own log. (2) `smbd` on its
    # own SIGTERM shutdown broadcasts to its own process group; with
    # `--no-process-group` that group is *this script's*, so killing smbd
    # below took the whole script down with it (exit 143). Omitting that
    # flag lets smbd start its own process group, which is what isolates
    # its shutdown broadcast from us.
    smbd --daemon --configfile="$LAB/smb/smb.conf" \
      < /dev/null > "$LAB/smb/smbd-stdout.log" 2>&1
    sleep 1

    smb_fifo="$LAB/smb/session.fifo"
    rm -f "$smb_fifo"
    mkfifo "$smb_fifo"
    smbclient //127.0.0.1/pool -N < "$smb_fifo" > "$LAB/smb/smbclient-$scenario-$mode.log" 2>&1 &
    smbclient_pid=$!
    exec 9> "$smb_fifo"
    sleep "$duration"
    echo quit >&9
    exec 9>&-
    wait "$smbclient_pid" || true
    smbd_pid=$(cat "$LAB/smb/run/smbd.pid" 2>/dev/null || true)
    [ -n "$smbd_pid" ] && kill "$smbd_pid" 2>/dev/null || true
    ;;
esac

if [ "$scenario" != "smb" ]; then
  sleep "$duration"
fi

[ -n "$workload_pid" ] && wait "$workload_pid" 2>/dev/null || true

fatrace_fatal=0
for entry in "${fatrace_pids[@]}"; do
  disk=${entry%%:*}
  pid=${entry##*:}
  wait "$pid"
  rc=$?
  # A per-event "open_by_handle_at: Operation not permitted" stderr line
  # (see the comment above) is expected on every logged event in this lab
  # and is not a failure — this mechanism doesn't depend on path
  # resolution. A genuine tracer failure (e.g. "Failed to open output file:
  # File exists" from a stale log this script didn't clear) makes fatrace
  # exit non-zero instead; that non-zero exit is the only failure signal
  # checked, so a window whose tracer never actually ran cannot be reported
  # as a clean "(missing)"/"(none)" result.
  if [ "$rc" -ne 0 ]; then
    echo "FATAL: fatrace on $disk exited $rc — window cannot count" >&2
    cat "$LAB/fatrace/$scenario-$mode-$disk.err" >&2
    fatrace_fatal=1
  fi
done
if [ "$fatrace_fatal" -ne 0 ]; then
  echo "=== WINDOW FAILED scenario=$scenario mode=$mode — fatrace tracer failure, discard this window ===" >&2
  exit 1
fi

echo "=== AFTER ==="
after_snapshot=$(snapshot)
echo "$after_snapshot"

echo "=== DELTA (named per-field deltas, computed by stat-delta.sh — never by eye) ==="
for disk in disk1 disk2 disk3; do
  before_line=$(echo "$before_snapshot" | awk -v d="$disk" '$1==d')
  after_line=$(echo "$after_snapshot" | awk -v d="$disk" '$1==d')
  bash /src/spikes/s1/scripts/stat-delta.sh "$before_line" "$after_line"
done

for disk in disk1 disk2 disk3; do
  echo "=== FATRACE $disk log (scoped to $LAB/mnt/$disk only — doc 06 §6) ==="
  cat "$LAB/fatrace/$scenario-$mode-$disk.log" 2>/dev/null || echo "(missing)"
  echo "=== FATRACE $disk stderr (per-event open_by_handle_at noise expected, see header comment) ==="
  cat "$LAB/fatrace/$scenario-$mode-$disk.err" 2>/dev/null || echo "(missing)"
done

if [ "$scenario" = "smb" ]; then
  echo "=== SMBCLIENT LOG ==="
  cat "$LAB/smb/smbclient-$scenario-$mode.log" 2>/dev/null || echo "(missing)"
  echo "=== SMBD LOG (tail) ==="
  tail -n 40 "$LAB/smb/smbd.log" 2>/dev/null || echo "(missing)"
fi

echo "=== WINDOW DONE scenario=$scenario mode=$mode finished=$(date -u +%FT%TZ) ==="
