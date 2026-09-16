# Shared helpers for spike S7 (fanotify change journal). Sourced, not
# executed.
#
# Runs against the *standing* array `make lab-up` builds (parity1,
# disk1..3, cache, default 1G/1G/512M geometry — plenty for the small,
# many-small-files workloads this spike needs; no custom LAB_DATA_SIZE
# required, so the standard `make lab-up` target is used unmodified). This
# spike's own files (snapraid.conf, the journal binary's TSV logs, the
# extra ext4/btrfs images and their mounts) live under a dedicated $LAB/s7
# subtree for anything that is not an image or a standing mount — not
# cleaned by destroy-array.sh (it only removes $LAB/img and $LAB/mnt) and
# so removed by this spike's own run-all.sh before `make lab-destroy`
# (dispatch teardown instructions) — plus $LAB/img/*.img and $LAB/mnt/*
# for the ext4/btrfs disks, which destroy-array.sh *does* clean up
# generically (same img/mnt discipline as scripts/devenv/create-array.sh).

# shellcheck source=/dev/null
source /src/scripts/devenv/lib.sh
lab_require_id

S7="$LAB/s7"
S7_RUN="$S7/run"
mkdir -p -- "$S7_RUN"

JOURNAL=/src/spikes/s7/bin/journal
[[ -x "$JOURNAL" ]] || die "journal binary not found at $JOURNAL — build it on the host first: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o spikes/s7/bin/journal ./spikes/s7/cmd/journal"

s7_log() { printf '%s\n' "$*"; }

# relpath -> which of disk1/disk2/disk3's own mountpoints actually holds a
# file written through the pool — read off each disk's own mount, never
# inferred from the pool's view (same rule S6's s6_locate follows).
s7_locate_disk() {
  local relpath=$1 d
  for d in disk1 disk2 disk3; do
    if [[ -e "$LAB/mnt/$d/$relpath" ]]; then
      echo "$d: present"
    else
      echo "$d: absent"
    fi
  done
}

# name, size, fstype -> creates $LAB/img/<name>.img, attaches it to a loop
# device this lab owns, formats it, and mounts it at $LAB/mnt/<name>.
# Mirrors scripts/devenv/create-array.sh's create_disk() exactly, except
# for the filesystem — needed here because that script is XFS-only and
# this spike also needs ext4 and btrfs data-disk fixtures (Q23).
s7_create_disk() {
  local name=$1 size=$2 fstype=$3
  local img="$LAB/img/$name.img"
  [[ -e "$img" ]] && die "refusing to recreate existing image: $img"
  truncate -s "$size" "$img"
  local dev
  dev=$(losetup --find --show "$img")
  lab_assert_own_loop "$dev" "$img"
  case "$fstype" in
    xfs) mkfs.xfs -q -L "$name" "$dev" ;;
    ext4) mkfs.ext4 -q -L "$name" "$dev" ;;
    btrfs) mkfs.btrfs -q -L "$name" "$dev" ;;
    *) die "s7_create_disk: unknown fstype '$fstype'" ;;
  esac
  mkdir -p -- "$LAB/mnt/$name"
  mount "$dev" "$LAB/mnt/$name"
  s7_log "created $name: image=$img dev=$dev fstype=$fstype mount=$LAB/mnt/$name"
}

# name, mark-path, out-log, extra journal args... -> starts the journal
# binary as a background job (`cmd & pid=$!`, per the dispatch's
# kill-by-PID rule), waits for its own ready-file before returning so the
# caller never races the mark being installed, and records the PID to
# $S7_RUN/<name>.pid for s7_stop_journal to kill later — exactly that PID,
# never a name or pattern match.
s7_start_journal() {
  local name=$1 mark=$2 out=$3; shift 3
  local ready="$S7_RUN/$name.ready"
  rm -f -- "$ready" "$out"
  s7_log "+ $JOURNAL -mark $mark -out $out -ready-file $ready $*"
  "$JOURNAL" -mark "$mark" -out "$out" -ready-file "$ready" "$@" &
  local pid=$!
  echo "$pid" > "$S7_RUN/$name.pid"
  local _
  for _ in $(seq 1 100); do
    [[ -s "$ready" ]] && { s7_log "$name: ready, pid=$pid"; return 0; }
    s7_pid_alive "$pid" || die "$name: journal (pid $pid) exited before signalling ready — see $out"
    sleep 0.1
  done
  die "$name: journal (pid $pid) did not signal ready within 10s"
}

# pid -> true if the process exists and is not a zombie (this container's
# PID 1 never reaps orphans — same lesson S6 recorded empirically).
s7_pid_alive() {
  local pid=$1
  [[ -r "/proc/$pid/stat" ]] || return 1
  local state; state=$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null) || return 1
  [[ "$state" != "Z" ]]
}

# name -> SIGTERMs exactly the PID s7_start_journal captured for it, and
# waits (up to 5s) for that PID to actually exit before returning.
s7_stop_journal() {
  local name=$1
  local pidfile="$S7_RUN/$name.pid"
  [[ -f "$pidfile" ]] || { s7_log "$name: no pidfile, nothing to stop"; return 0; }
  local pid; pid=$(cat "$pidfile")
  s7_pid_alive "$pid" || { s7_log "$name: pid $pid already gone"; return 0; }
  # No `ps`/`pgrep` in the base lab image (procps is absent, per the
  # machine notes) — confirm this is really our own journal process by
  # reading /proc/<pid>/cmdline directly before ever sending it a signal.
  grep -qa 'journal' "/proc/$pid/cmdline" 2>/dev/null || die "$name: pid $pid's /proc/$pid/cmdline does not mention 'journal' — refusing to kill it"
  kill -TERM "$pid"
  local _
  for _ in $(seq 1 50); do
    s7_pid_alive "$pid" || { s7_log "$name: stopped, pid $pid exited"; return 0; }
    sleep 0.1
  done
  die "$name: pid $pid still alive 5s after SIGTERM"
}

# logfile -> number of distinct (fid, name) pairs across every non-control
# line (excludes OVERFLOW/STOPPED, which carry no fid/name). This is S7's
# proxy for "distinct changed paths per disk" (the issue's own default,
# doc 13 Q13): the dir-FID bytes plus the reported name, since this lab
# cannot resolve a fanotify file handle to a real path (no
# CAP_DAC_READ_SEARCH — open_by_handle_at fails EPERM, confirmed by S1/S6).
s7_distinct_count() {
  local logfile=$1
  [[ -f "$logfile" ]] || { echo 0; return; }
  awk -F'\t' '$2 != "OVERFLOW" && $2 != "STOPPED" {print $3 "\t" $4}' "$logfile" | sort -u | wc -l | tr -d ' '
}

# logfile -> 1 if any OVERFLOW line is present, else 0 — the "mark the
# journal incomplete" signal the issue's acceptance criterion requires.
s7_had_overflow() {
  local logfile=$1
  [[ -f "$logfile" ]] || { echo 0; return; }
  awk -F'\t' '$2=="OVERFLOW"{f=1} END{print f+0}' "$logfile"
}

# Writes this spike's snapraid.conf against the standing disk1..3 array,
# content on parity1 (boot-device stand-in, Q18) and cache — same shape
# as spike S5's own config (spikes/s5/scripts/01-seed-and-sync.sh),
# reused rather than reinvented.
CONF="$S7/snapraid.conf"
s7_write_conf() {
  cat > "$CONF" <<EOF
parity $LAB/mnt/parity1/snapraid.parity
content $LAB/mnt/parity1/snapraid.content
content $LAB/mnt/cache/snapraid.content
data d1 $LAB/mnt/disk1/
data d2 $LAB/mnt/disk2/
data d3 $LAB/mnt/disk3/
EOF
  s7_log "wrote $CONF:"
  cat "$CONF"
}

# Runs snapraid against this spike's config, printing the exact command
# and exit status — every claim about a diff result must be checkable
# against a committed log line, not prose (S5's own convention, reused).
s7_snapraid() {
  s7_log "+ snapraid -c $CONF $*"
  local rc=0
  snapraid -c "$CONF" "$@" || rc=$?
  s7_log "[exit $rc]"
  return "$rc"
}

# Parses `snapraid diff`'s own summary block (the "N added / N removed /
# ... " lines it always prints, confirmed verbatim in spikes/s5/results)
# from a captured diff log, and prints one number: every non-equal,
# non-moved category summed. "moved" is excluded deliberately — S5 already
# found SnapRAID can never classify a move as `moved` in this lab (no
# disk UUIDs available, ever — spikes/s5), so it is always 0 here, and
# leaving it out of the sum keeps this parser honest about what it can
# actually see rather than adding a category this lab cannot produce.
s7_diff_changed_count() {
  local difflog=$1
  awk '
    /^ *[0-9]+ added$/   {added=$1}
    /^ *[0-9]+ removed$/ {removed=$1}
    /^ *[0-9]+ updated$/ {updated=$1}
    /^ *[0-9]+ copied$/  {copied=$1}
    /^ *[0-9]+ restored$/{restored=$1}
    END {print added+removed+updated+copied+restored}
  ' "$difflog"
}
