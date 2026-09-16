# Shared helpers for spike S6 (per-share mergerfs mount topology). Sourced,
# not executed.
#
# This spike runs against the *standing* array `make lab-up` builds
# (disk1..3, cache, and the catch-all pool already mounted at
# $LAB/mnt/user — scripts/devenv/create-array.sh's own defaults, unchanged
# by this spike). Every per-share mount this spike adds lives at
# $LAB/mnt/user/<share> (nested inside the standing catch-all, exactly doc
# 02 §1's topology) or, for the ordering experiment's disposable share, at
# the same path while the catch-all is deliberately down. This spike's own
# non-image, non-standing-mount files (pidfiles, logs) live under a
# dedicated $LAB/s6 subtree, which `destroy-array.sh` does not clean (it
# only removes $LAB/img and $LAB/mnt) and which this spike removes itself,
# from inside the container, before teardown (dispatch instructions).
#
# Every mergerfs instance this spike starts is launched in *foreground*
# mode (`-f`) as a backgrounded shell job, `cmd & pid=$!` — the same
# pattern the mergerfs wiki's own systemd unit uses (`ExecStart=... -f`,
# `Type=simple`, reference checkout `man/mergerfs.1` "systemd mount"
# section) and the pattern the dispatch's kill-by-PID rule requires: the
# PID this spike ever kills is the exact PID captured at that mount's own
# launch, never found by scanning for a name or pattern.

# shellcheck source=../../../scripts/devenv/lib.sh
source /src/scripts/devenv/lib.sh
lab_require_id

S6="$LAB/s6"
S6_PIDS="$S6/pids"
S6_LOGS="$S6/logs"
mkdir -p -- "$S6_PIDS" "$S6_LOGS"

s6_log() { printf '%s\n' "$*"; }

# Common mount options matching doc 02 §1's table exactly (fsname makes
# each instance identifiable in `mount`/`df`; cache.statfs=0 favours
# accuracy per doc 09 §5). policy, minfreespace and the branch list are the
# only things that vary between calls.
s6_opts() {
  local policy=$1 minfreespace=$2 fsname=$3
  echo "category.create=$policy,moveonenospc=true,minfreespace=$minfreespace,dropcacheonclose=true,cache.files=partial,cache.statfs=0,fsname=$fsname"
}

# pid -> true if the process exists and is not a zombie. `kill -0` alone
# is not enough: this container's PID 1 (the entrypoint's own `sleep
# infinity`) never reaps its children, so a mergerfs process whose *own*
# launching script has already exited (every s6_mount call started in one
# numbered script and unmounted from a later one — the common case here,
# since 02-topology-up.sh's shell exits long before 06-cost.sh or
# 08-topology-down.sh unmount the same processes) is reparented to PID 1
# and, once it actually exits, sits as a zombie forever rather than being
# reaped — `kill -0` still reports it present (a zombie's PID is valid
# until reaped) even though the process has genuinely finished and holds
# no resources. Found empirically: mover-ctm1 (mounted by 02, unmounted by
# 06) reported "still alive after 5s" purely from this, confirmed via
# `/proc/<pid>/stat`'s third field reading `Z`.
s6_pid_alive() {
  local pid=$1
  [[ -r "/proc/$pid/stat" ]] || return 1
  local state; state=$(awk '{print $3}' "/proc/$pid/stat" 2>/dev/null) || return 1
  [[ "$state" != "Z" ]]
}

# name, opts, branches, mountpoint -> starts mergerfs in the foreground as
# a background job, records its PID and full command line to
# $S6_PIDS/<name>.pid / .cmd, waits (up to 10s) for the kernel to register
# the mount, and fails loudly if it never does. Every mount this spike
# creates goes through this one function, so every PID this spike later
# kills or measures is captured at the moment of its own launch.
s6_mount() {
  local name=$1 opts=$2 branches=$3 mountpoint=$4
  local log="$S6_LOGS/$name.log"
  mkdir -p -- "$mountpoint"
  echo "+ mergerfs -f -o $opts $branches $mountpoint" | tee "$S6_LOGS/$name.cmd"
  mergerfs -f -o "$opts" "$branches" "$mountpoint" > "$log" 2>&1 &
  local pid=$!
  echo "$pid" > "$S6_PIDS/$name.pid"
  local _
  for _ in $(seq 1 100); do
    mountpoint -q -- "$mountpoint" && { echo "$name: mounted, pid=$pid"; return 0; }
    s6_pid_alive "$pid" || die "$name: mergerfs (pid $pid) exited before mounting — see $log"
    sleep 0.1
  done
  die "$name: did not register as a mountpoint within 10s (pid $pid) — see $log"
}

# name, mountpoint -> unmounts cleanly and waits for the PID this spike
# itself captured at mount time to exit, so "unmounts cleanly" means both
# "the kernel mount entry is gone" and "the process that served it is
# gone", not just one or the other.
s6_unmount() {
  local name=$1 mountpoint=$2
  local pidfile="$S6_PIDS/$name.pid"
  mountpoint -q -- "$mountpoint" || { s6_log "$name: not mounted, skipping"; return 0; }
  fusermount -u -- "$mountpoint" || die "$name: fusermount -u $mountpoint failed"
  if [[ -f "$pidfile" ]]; then
    local pid; pid=$(cat "$pidfile")
    local _
    for _ in $(seq 1 50); do
      s6_pid_alive "$pid" || { s6_log "$name: unmounted, pid $pid exited"; return 0; }
      sleep 0.1
    done
    die "$name: unmounted but pid $pid is still alive after 5s"
  fi
}

# Attempts an unmount that is expected to fail (the "wrong order" probes) —
# succeeds (returns 0) only when the unmount itself fails, and prints
# exactly what it failed with, so a claim of "the kernel refuses this" is
# checkable against real output rather than asserted.
s6_expect_unmount_fails() {
  local mountpoint=$1
  local out
  if out=$(fusermount -u -- "$mountpoint" 2>&1); then
    die "expected unmount of $mountpoint to fail, but it succeeded: $out"
  fi
  echo "unmount of $mountpoint failed as expected: $out"
}

# pid -> VmRSS in kB, straight from /proc/<pid>/status — no procps in the
# base lab image (machine-check notes), so this reads the kernel's own
# accounting directly rather than shelling out to `ps`.
s6_rss_kb() {
  local pid=$1
  awk '/^VmRSS:/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0
}

# Number of live (non-zombie) processes whose comm is exactly "mergerfs"
# — a direct /proc scan, since neither `ps` nor `pgrep` (procps) is
# installed in the base lab image (machine-check notes) and this spike
# doesn't install anything extra for a count it can get from the kernel
# directly. Zombies are excluded deliberately: this container's PID 1
# (the entrypoint's own `sleep infinity`) never reaps children, so a
# mergerfs daemon unmounted from outside its own controlling process (the
# standing catch-all `make lab-up` itself starts and 01-ordering-
# shadow-mount.sh later unmounts and replaces) is left as a zombie —
# holding no memory, doing no work, and not one of the mounts this spike
# is trying to count or measure.
s6_process_count() {
  local n=0 p comm state
  for p in /proc/[0-9]*; do
    [[ -r "$p/comm" ]] || continue
    comm=$(cat "$p/comm" 2>/dev/null) || continue
    [[ "$comm" == "mergerfs" ]] || continue
    state=$(awk '{print $3}' "$p/stat" 2>/dev/null) || continue
    [[ "$state" == "Z" ]] && continue
    n=$((n+1))
  done
  echo "$n"
}

# The zombie-side counterpart to s6_process_count, for reporting the
# zombie-accumulation finding explicitly rather than only ever excluding
# it silently.
s6_zombie_count() {
  local n=0 p comm state
  for p in /proc/[0-9]*; do
    [[ -r "$p/comm" ]] || continue
    comm=$(cat "$p/comm" 2>/dev/null) || continue
    [[ "$comm" == "mergerfs" ]] || continue
    state=$(awk '{print $3}' "$p/stat" 2>/dev/null) || continue
    [[ "$state" == "Z" ]] && n=$((n+1))
  done
  echo "$n"
}

# name, size -> mirrors scripts/devenv/create-array.sh's own create_disk()
# exactly (reused, not reimplemented) for the dedicated small array the
# mspmfs/epmfs fallback experiment needs, kept separate from the standing
# disk1..3 array so filling one of these to below minfreespace never
# affects the twelve-share topology or the cost measurement.
s6_create_disk() {
  local name=$1 size=$2
  local img="$LAB/img/$name.img"
  [[ -e "$img" ]] && die "refusing to recreate existing image: $img"
  truncate -s "$size" "$img"
  local dev
  dev=$(losetup --find --show "$img")
  lab_assert_own_loop "$dev" "$img"
  mkfs.xfs -q -L "$name" "$dev"
  mkdir -p -- "$LAB/mnt/$name"
  mount "$dev" "$LAB/mnt/$name"
  echo "created $name: image=$img dev=$dev mount=$LAB/mnt/$name"
}

# Which physical branch a file actually landed on — read directly off each
# candidate branch's own mountpoint, never inferred from the pool's view,
# per the dispatch's "show which branch, not just that the write
# succeeded" rule. Prints one "branch: present/absent" line per candidate.
s6_locate() {
  local relpath=$1; shift
  local branch
  for branch in "$@"; do
    if [[ -e "$branch/$relpath" ]]; then
      echo "$branch: present"
    else
      echo "$branch: absent"
    fi
  done
}

# The twelve-share topology doc 02 §1 describes: four of each cache mode.
# Named once here so 02-topology-up.sh (build it), 06-cost.sh (tear it down
# and rebuild it around the "single mount" comparison) and
# 08-topology-down.sh (tear it down for good) share one definition rather
# than three copies that could drift apart.
S6_SHARES_CTM=(ctm1 ctm2 ctm3 ctm4)
S6_SHARES_CO=(co1 co2 co3 co4)
S6_SHARES_AO=(ao1 ao2 ao3 ao4)

# Mounts all twelve shares plus the ctm1 mover write target. Requires the
# catch-all already mounted at $LAB/mnt/user (its mountpoint directories
# are created through it, per doc 02 §1's nesting).
s6_bring_up_shares() {
  local share
  for share in "${S6_SHARES_CTM[@]}"; do
    mkdir -p -- "$LAB/mnt/cache/$share" "$LAB/mnt/disk1/$share" "$LAB/mnt/disk2/$share" "$LAB/mnt/disk3/$share"
    mkdir -p -- "$LAB/mnt/user/$share"
    s6_mount "$share" "$(s6_opts mspmfs 10M "hoserva-$share")" \
      "$LAB/mnt/cache/$share=RW:$LAB/mnt/disk1/$share=NC:$LAB/mnt/disk2/$share=NC:$LAB/mnt/disk3/$share=NC" \
      "$LAB/mnt/user/$share"
  done
  for share in "${S6_SHARES_CO[@]}"; do
    mkdir -p -- "$LAB/mnt/cache/$share"
    mkdir -p -- "$LAB/mnt/user/$share"
    s6_mount "$share" "$(s6_opts mspmfs 10M "hoserva-$share")" \
      "$LAB/mnt/cache/$share=RW" "$LAB/mnt/user/$share"
  done
  for share in "${S6_SHARES_AO[@]}"; do
    mkdir -p -- "$LAB/mnt/disk1/$share" "$LAB/mnt/disk2/$share" "$LAB/mnt/disk3/$share"
    mkdir -p -- "$LAB/mnt/user/$share"
    s6_mount "$share" "$(s6_opts mspmfs 10M "hoserva-$share")" \
      "$LAB/mnt/disk1/$share=RW:$LAB/mnt/disk2/$share=RW:$LAB/mnt/disk3/$share=RW" \
      "$LAB/mnt/user/$share"
  done
  mkdir -p -- "$LAB/mnt/disk1/ctm1" "$LAB/mnt/disk2/ctm1" "$LAB/mnt/disk3/ctm1" "$LAB/mnt/mover/ctm1"
  s6_mount mover-ctm1 "$(s6_opts mspmfs 10M hoserva-mover-ctm1)" \
    "$LAB/mnt/disk1/ctm1=RW:$LAB/mnt/disk2/ctm1=RW:$LAB/mnt/disk3/ctm1=RW" \
    "$LAB/mnt/mover/ctm1"
}

# Unmounts the mover target and all twelve shares — the reverse dependency
# order from s6_bring_up_shares, and always before the catch-all itself is
# ever touched.
s6_tear_down_shares() {
  s6_unmount mover-ctm1 "$LAB/mnt/mover/ctm1"
  local share
  for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_CO[@]}" "${S6_SHARES_AO[@]}"; do
    s6_unmount "$share" "$LAB/mnt/user/$share"
  done
}
