# Shared helpers for spike S2 (Unraid disk adoption). Sourced, not executed.
#
# This spike works entirely under $LAB/s2/ — never $LAB/img or $LAB/mnt,
# which belong to the standing array `make lab-up` built and which
# destroy-array.sh removes on `make lab-destroy`. Everything under $LAB/s2/
# is this spike's own responsibility to unmount, detach and delete before
# the lab is destroyed (per the dispatch's teardown instructions).
#
# Loop-device discipline mirrors scripts/devenv/lib.sh: every device this
# spike touches is resolved back to one of its own image files via
# `losetup -j`, and `losetup -D` (detach-all) is never used.

die() { printf 's2: %s\n' "$*" >&2; exit 1; }

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
S2="$LAB/s2"
S2_IMG="$S2/img"
S2_MNT="$S2/mnt"
S2_ADOPT="$S2/adopt"
S2_POOL="$S2/pool"
mkdir -p -- "$S2_IMG" "$S2_MNT" "$S2_ADOPT" "$S2_POOL"

# Refuses any device that is not a loop device losetup itself just attached
# to one of this spike's own images under $S2_IMG — same discipline as
# scripts/devenv/lib.sh's lab_assert_own_loop, scoped to $S2_IMG instead of
# $LAB/img so this spike's devices are checked against its own images only.
s2_assert_own_loop() {
  local dev=$1 img=$2
  local resolved_img resolved_s2_img
  resolved_img=$(realpath -m -- "$img") || die "refusing $img: cannot resolve path"
  resolved_s2_img=$(realpath -m -- "$S2_IMG")
  case "$resolved_img" in
    "$resolved_s2_img"/*) ;;
    *) die "refusing $img: not under \$S2/img/" ;;
  esac
  case "$dev" in
    /dev/loop[0-9]*) ;;
    *) die "refusing non-loop device: $dev" ;;
  esac
  local resolved
  resolved=$(losetup -j "$resolved_img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  [[ -n "$resolved" && "$resolved" == "$dev" ]] || die "refusing $dev: not backed by $img"
}

# Builds a single-partition MBR ("dos") image the way Unraid's default
# "MBR: 4K-aligned" disk format lays out an array disk 2TB or smaller —
# partition 1 starting at the 64th 512-byte sector (byte offset 32768),
# spanning to the end of the disk (unraid/webgui,
# emhttp/languages/en_US/helptext.txt, disk_default_partition_format_help;
# identical in tags 6.12.15 and 7.3.2 — see doc 08 §2). Prints the loop
# device for the partition (major 7, exposed via --offset/--sizelimit so no
# partition-scanned device node is ever involved).
s2_partition_disk() {
  local img=$1
  local start_sector=64 sector_size=512
  sfdisk --no-reread -q "$img" >/dev/null <<EOF
label: dos
unit: sectors

start=$start_sector, type=83
EOF
  local part_line size_sectors
  part_line=$(sfdisk -d "$img" | grep -E '^\S+1\s*:')
  [[ -n "$part_line" ]] || die "sfdisk -d $img: could not find partition 1 line"
  size_sectors=$(sed -n 's/.*size=\s*\([0-9]\+\).*/\1/p' <<<"$part_line")
  [[ "$size_sectors" =~ ^[0-9]+$ ]] || die "could not parse partition size from: $part_line"
  local offset_bytes=$((start_sector * sector_size))
  local size_bytes=$((size_sectors * sector_size))
  local dev
  dev=$(losetup --find --show --offset "$offset_bytes" --sizelimit "$size_bytes" "$img")
  s2_assert_own_loop "$dev" "$img"
  echo "$dev"
}

# This container has no udevd, so nothing invalidates /run/blkid/blkid.tab
# when a loop device's backing file changes — the cache is keyed by device
# number, and a reused /dev/loopN keeps its old cache entry across a
# detach/reattach to a *different* image (found the hard way: a UUID
# resolved and mounted successfully in one round of check-fixture.sh, then
# failed to resolve in a later round against the same disk, because a loop
# number recycled in between picked up a stale cache entry from a different
# image). Deleting the cache file forces the next `blkid`/`mount UUID=` to
# probe the device directly instead of trusting it — call this right after
# every fresh `losetup` attach, before resolving or mounting by UUID.
s2_fresh_blkid_cache() {
  rm -f /run/blkid/blkid.tab* 2>/dev/null || true
}

# Copies a device's raw bytes to a throwaway file under $S2 (never
# $S2_IMG, so build-fixture.sh's own-image "already exists" guard is never
# confused by it) for a later byte-level `cmp -l` against a second copy.
# Read-only: never opens the device for writing.
s2_snapshot_bytes() {
  local dev=$1 dest=$2
  dd if="$dev" of="$dest" bs=1M status=none
}

# Byte-level diff between two snapshots taken by s2_snapshot_bytes. Writes
# one 0-indexed, half-open [start,end) range per line — one per contiguous
# run of differing bytes — to $ranges_file, and echoes the total differing
# byte count. The raw material any "N bytes changed" claim in this spike's
# output must be derived from, never asserted free-hand.
s2_byte_diff_ranges() {
  local before=$1 after=$2 ranges_file=$3
  local diffs
  diffs=$(cmp -l "$before" "$after" 2>/dev/null || true)
  : > "$ranges_file"
  if [[ -z "$diffs" ]]; then
    echo 0
    return 0
  fi
  printf '%s\n' "$diffs" | awk '{print $1-1}' | sort -n | awk '
    BEGIN { first = -1; prev = -1 }
    {
      if (first == -1) { first = $1; prev = $1; next }
      if ($1 == prev + 1) { prev = $1; next }
      print first"-"(prev+1)
      first = $1; prev = $1
    }
    END { if (first != -1) print first"-"(prev+1) }
  ' > "$ranges_file"
  wc -l < "$ranges_file"
}

# Reads an XFS filesystem's internal-log location straight from its own
# superblock, via the read-only debugger (`xfs_db -r` — never opens the
# device for writing), and converts the log's starting fsblock to a real
# byte offset within the device with xfs_db's own `convert` command rather
# than hand-multiplying logstart*blocksize (which is wrong here: XFS
# encodes an internal sb_logstart as an AG-relative fsblock number when the
# AG size is rounded up to a power of two for addressing, as it is on every
# fixture disk this spike builds — `convert fsb <n> daddr` decodes that
# correctly and is the same conversion xfs_db itself uses for repair work).
# Prints "log_start_byte log_length_bytes sectsize" for the given device.
s2_xfs_log_geometry() {
  local dev=$1
  local logstart logblocks blocksize sectsize
  logstart=$(xfs_db -r -c 'sb 0' -c 'print logstart' "$dev" | awk '{print $NF}')
  logblocks=$(xfs_db -r -c 'sb 0' -c 'print logblocks' "$dev" | awk '{print $NF}')
  blocksize=$(xfs_db -r -c 'sb 0' -c 'print blocksize' "$dev" | awk '{print $NF}')
  sectsize=$(xfs_db -r -c 'sb 0' -c 'print sectsize' "$dev" | awk '{print $NF}')
  [[ "$logstart" =~ ^[0-9]+$ && "$logblocks" =~ ^[0-9]+$ && "$blocksize" =~ ^[0-9]+$ && "$sectsize" =~ ^[0-9]+$ ]] \
    || die "$dev: could not read log geometry from xfs_db -r"
  local daddr
  daddr=$(xfs_db -r -c "convert fsb $logstart daddr" "$dev" | awk '{print $2}' | tr -d '()')
  [[ "$daddr" =~ ^[0-9]+$ ]] || die "$dev: could not convert logstart fsblock $logstart to a disk address"
  local log_start_byte=$((daddr * sectsize))
  local log_length_bytes=$((logblocks * blocksize))
  echo "$log_start_byte $log_length_bytes $sectsize"
}

# Classifies each [start,end) range s2_byte_diff_ranges wrote against the
# primary superblock (always the filesystem's first sector, byte offset 0)
# and the internal log region from s2_xfs_log_geometry — never guessing at
# a range this spike cannot place.
s2_classify_ranges() {
  local sectsize=$1 log_start_byte=$2 log_length_bytes=$3
  local log_end_byte=$((log_start_byte + log_length_bytes))
  local range start end where
  while IFS= read -r range; do
    [[ -n "$range" ]] || continue
    start=${range%-*}
    end=${range#*-}
    if (( start < sectsize )); then
      where="primary superblock (first sector, offset 0-$((sectsize - 1)))"
    elif (( start >= log_start_byte && end <= log_end_byte )); then
      where="internal log region (offset $log_start_byte-$((log_end_byte - 1)))"
    else
      where="outside the primary superblock and the internal log region — not further identified"
    fi
    echo "  $range: $where"
  done
}
