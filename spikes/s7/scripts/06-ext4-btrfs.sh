#!/usr/bin/env bash
# ext4 and single-device btrfs data disks (Q23). Not part of the standing
# XFS array or the snapraid.conf — these are their own throwaway images,
# checked the same way spikes/s5 and spikes/s2 build extra fixtures: a
# fixed, known set of file operations and a distinct-path count the
# script can assert against directly, without needing `snapraid diff` at
# all (SnapRAID's own parity disk in this spike is XFS-formatted, per doc
# 02 §2 — these disks are never part of that array).
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

# name, mountpoint -> baseline (base1/base2/base3), then a fixed 4-operation
# workload while a journal watches, then asserts exactly 5 distinct paths:
# {created1.bin} + {base1.bin} + {base2.bin} + {base3.bin, renamed3.bin}.
s7_fixed_workload() {
  local name=$1 mnt=$2
  head -c 1024 /dev/urandom > "$mnt/base1.bin"
  head -c 1024 /dev/urandom > "$mnt/base2.bin"
  head -c 1024 /dev/urandom > "$mnt/base3.bin"

  s7_start_journal "$name" "$mnt" "$S7_RUN/$name.log"

  head -c 1024 /dev/urandom > "$mnt/created1.bin"     # create
  head -c 1024 /dev/urandom > "$mnt/base1.bin"         # overwrite
  rm -f -- "$mnt/base2.bin"                            # delete
  mv -- "$mnt/base3.bin" "$mnt/renamed3.bin"           # rename

  sleep 2
  s7_stop_journal "$name"

  s7_log "-- $name raw log --"
  cat "$S7_RUN/$name.log"

  local count; count=$(s7_distinct_count "$S7_RUN/$name.log")
  s7_log "$name: $count distinct paths (expected 5: created1.bin, base1.bin, base2.bin, base3.bin, renamed3.bin)"
  [[ "$count" -eq 5 ]] || die "$name: expected 5 distinct paths, got $count"
  s7_log "PASS: $name (mountpoint $mnt) — FAN_MARK_FILESYSTEM behaves the same as on XFS"
}

s7_log "== ext4 data disk =="
s7_create_disk ext4disk 512M ext4
s7_fixed_workload ext4-fixed "$LAB/mnt/ext4disk"

s7_log ""
s7_log "== single-device btrfs data disk (whole-disk root subvolume, Q23) =="
s7_create_disk btrfsdisk 512M btrfs
s7_fixed_workload btrfs-fixed "$LAB/mnt/btrfsdisk"

s7_log ""
s7_log "== btrfs subvolume mounted separately from its root (the EXDEV question) =="
btrfs subvolume create "$LAB/mnt/btrfsdisk/sub1" | tee "$S7_RUN/btrfs-subvol-create.log"
mkdir -p -- "$LAB/mnt/btrfssub"
dev=$(findmnt -n -o SOURCE -- "$LAB/mnt/btrfsdisk")
mount -o "subvol=sub1" -- "$dev" "$LAB/mnt/btrfssub"
mountpoint -q -- "$LAB/mnt/btrfssub" || die "failed to mount the sub1 subvolume separately"

set +e
"$JOURNAL" -mark "$LAB/mnt/btrfssub" -out "$S7_RUN/btrfs-subvol.log" -duration 2s > "$S7_RUN/btrfs-subvol.stderr" 2>&1
subvol_rc=$?
set -e
s7_log "-- fanotify_mark(FAN_MARK_FILESYSTEM) against the separately-mounted subvolume --"
s7_log "journal exit code: $subvol_rc"
cat "$S7_RUN/btrfs-subvol.stderr" 2>/dev/null || true
if [[ "$subvol_rc" -eq 0 ]]; then
  s7_log "RESULT: fanotify_mark succeeded on the separately-mounted subvolume — this lab's kernel/btrfs-progs combination reports the same fsid for a subvolume as its root, so FAN_MARK_FILESYSTEM covers it without a distinct mark. No EXDEV observed."
else
  s7_log "RESULT: fanotify_mark FAILED on the separately-mounted subvolume (see stderr above) — if this is EXDEV, it confirms the issue's upstream research; whatever the exact errno, a data disk laid out as a btrfs subvolume distinct from its root needs its own FAN_MARK_FILESYSTEM call per subvolume mount, not one mark on the root covering all of them."
fi

umount -- "$LAB/mnt/btrfssub"
rmdir -- "$LAB/mnt/btrfssub"
