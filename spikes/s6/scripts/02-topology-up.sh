#!/usr/bin/env bash
# Step 1 / step 2 (correct order) — brings up doc 02 §1's full topology: the
# catch-all (already mounted by 01-ordering-shadow-mount.sh, tracked as
# "catchall") plus twelve per-share mounts — four of each cache mode — and
# the array-only mover write target for one of them (lib.sh's
# s6_bring_up_shares, shared with 06-cost.sh and 08-topology-down.sh). Then
# proves the *correct* dependency direction is enforced too: attempting to
# unmount the catch-all while any per-share mount is still nested inside it
# must fail.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

OUT=${S6_OUT:-$LAB/s6-results}
mkdir -p -- "$OUT"

mountpoint -q -- "$LAB/mnt/user" || die "catch-all is not mounted — run 01-ordering-shadow-mount.sh first"

s6_log "== mounting twelve per-share pools (4 cache-then-move, 4 cache-only, 4 array-only) plus the mover target =="
s6_bring_up_shares

s6_log "== all 13 mounts (catch-all + 12 shares) + the mover target are up — confirmed via findmnt =="
{
  echo "$LAB/mnt/user and everything mounted under it:"
  findmnt -R "$LAB/mnt/user"
  echo
  echo "mover target:"
  findmnt "$LAB/mnt/mover/ctm1"
} | tee "$OUT/topology-findmnt.log"

n_mounted=$(findmnt -R -n "$LAB/mnt/user" | wc -l)
[[ "$n_mounted" -eq 13 ]] || die "expected 13 mounts under $LAB/mnt/user (1 catch-all + 12 shares), found $n_mounted"
s6_log "confirmed: exactly 13 mounts (1 catch-all + 12 per-share) under $LAB/mnt/user"

s6_log "== ordering, the other direction: the catch-all must refuse to unmount while a share is still nested inside it =="
s6_expect_unmount_fails "$LAB/mnt/user" | tee "$OUT/catchall-busy-unmount.log"
mountpoint -q -- "$LAB/mnt/user" || die "catch-all should still be mounted after the refused unmount attempt"
for share in "${S6_SHARES_CTM[@]}" "${S6_SHARES_CO[@]}" "${S6_SHARES_AO[@]}"; do
  mountpoint -q -- "$LAB/mnt/user/$share" || die "$share should still be mounted after the refused catch-all unmount"
done
s6_log "confirmed: the catch-all's own unmount attempt failed while shares were nested inside it, and nothing was torn down by the attempt"
