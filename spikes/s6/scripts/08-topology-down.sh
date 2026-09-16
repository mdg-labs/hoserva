#!/usr/bin/env bash
# Step 1 (unmount half) / teardown — unmounts the full topology in strict
# dependency order (twelve shares + the mover target, then the catch-all),
# confirms zero mergerfs processes and zero mounts remain, removes the
# mountpoint directories this spike itself created (never a disk's own
# top-level directory — those belong to destroy-array.sh), and archives
# every mount command this spike ever ran into results/mount-commands.log.
# Must run before `make lab-destroy`: destroy-array.sh's own teardown only
# unmounts what is directly under $LAB/mnt/*/ (one level), so it would
# never reach a per-share mount nested inside the catch-all or the mover
# target nested inside $LAB/mnt/mover/ — that is this spike's own
# responsibility (dispatch teardown instructions). A prior attempt
# unmounted everything here but never rmdir'd the now-empty mountpoint
# directories it had `mkdir -p`'d, so a subsequent `make lab-destroy`
# needed them removed by hand inside the container first; the rmdir pass
# below closes that gap.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

s6_log "== unmounting the twelve shares + mover target =="
s6_tear_down_shares

s6_log "== unmounting the catch-all =="
s6_unmount catchall "$LAB/mnt/user"

n_procs=$(s6_process_count)
n_zombies=$(s6_zombie_count)
[[ "$n_procs" -eq 0 ]] || die "expected 0 live mergerfs processes after full teardown, found $n_procs"
mountpoint -q -- "$LAB/mnt/user" && die "$LAB/mnt/user is still mounted after teardown"
s6_log "confirmed: 0 live mergerfs processes running, $LAB/mnt/user unmounted — the full topology (catch-all + twelve shares + mover target) unmounted cleanly, in dependency order. $n_zombies zombie mergerfs entries remain in the process table (lab-environment artifact: this container's PID 1 never reaps children once a mount's own launching script has exited — see README's 'no init system' finding; they hold no memory or file descriptors and are not a leak this spike's own teardown can fix)"

s6_log "== removing this spike's own now-empty mountpoint directories (never a disk's own top-level directory) =="
RMDIR_TARGETS=()
for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_CO[@]}" "${S6_SHARES_AO[@]}" shadow-test; do
  RMDIR_TARGETS+=("$LAB/mnt/user/$share")
done
for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_CO[@]}" shadow-test; do
  RMDIR_TARGETS+=("$LAB/mnt/cache/$share")
done
for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_AO[@]}" shadow-test; do
  RMDIR_TARGETS+=("$LAB/mnt/disk1/$share" "$LAB/mnt/disk2/$share" "$LAB/mnt/disk3/$share")
done
RMDIR_TARGETS+=("$LAB/mnt/mover/ctm1" "$LAB/mnt/mover")
for d in "${RMDIR_TARGETS[@]}"; do
  [[ -d "$d" ]] || continue
  rmdir --ignore-fail-on-non-empty -- "$d"
  if [[ -d "$d" ]]; then
    s6_log "left in place (not empty — a step wrote a real file into it): $d"
  else
    s6_log "removed: $d"
  fi
done

cat "$S6_LOGS"/*.cmd > "$OUT/mount-commands.log" 2>/dev/null || true
s6_log "archived every mount command this spike ran to results/mount-commands.log"
