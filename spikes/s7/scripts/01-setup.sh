#!/usr/bin/env bash
# Installs this spike's own extra packages (not baked into the standing
# lab image — CLAUDE.md, "Spikes"), writes the snapraid.conf against the
# standing disk1..3 array, seeds a baseline tree (direct writes and writes
# through the mergerfs pool) and runs the first sync — the "last sync"
# baseline every later diff in this spike is measured against.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# snapraid pinned to the same version spike S5 already validated in this
# lab (spikes/s5/README.md) — not a fresh, unvetted pin.
apt-get install -y -qq --no-install-recommends snapraid=12.4-1 e2fsprogs btrfs-progs

{
  echo "snapraid: $(dpkg-query -W -f '${Version}' snapraid)"
  echo "e2fsprogs: $(dpkg-query -W -f '${Version}' e2fsprogs)"
  echo "btrfs-progs: $(dpkg-query -W -f '${Version}' btrfs-progs)"
  echo "xfsprogs: $(dpkg-query -W -f '${Version}' xfsprogs)"
  echo "kernel: $(uname -srvm)"
} | tee "$S7/versions.log"

s7_write_conf

mountpoint -q -- "$LAB/mnt/user" || die "expected the standing mergerfs pool at $LAB/mnt/user (make lab-up)"

s7_log "== seeding baseline files, direct and through the pool =="
mkdir -p -- "$LAB/mnt/disk1/media" "$LAB/mnt/disk2/media" "$LAB/mnt/disk3/media" \
           "$LAB/mnt/disk1/docs" "$LAB/mnt/disk2/docs" "$LAB/mnt/disk1/archive"
for f in "$LAB/mnt/disk1/media/a.bin" "$LAB/mnt/disk2/media/b.bin" "$LAB/mnt/disk3/media/c.bin" \
         "$LAB/mnt/disk1/docs/report.txt" "$LAB/mnt/disk2/docs/notes.txt"; do
  head -c 4096 /dev/urandom > "$f"
done

mkdir -p -- "$LAB/mnt/user/pool-baseline"
head -c 4096 /dev/urandom > "$LAB/mnt/user/pool-baseline/seed.bin"
s7_log "pool-baseline/seed.bin landed on:"
s7_locate_disk pool-baseline/seed.bin

# Filler files that no later step in this spike ever touches — this
# spike's fixture disks otherwise carry only a handful of tracked files
# each, and this run found empirically that touching *all* of a disk's
# few tracked files in one workload trips SnapRAID's own built-in "all
# files previously present ... are now missing or have been rewritten"
# guard (a real, separate safety check from Hoserva's own threshold guard,
# doc 02 §2 — meant for a restored/misconfigured disk, not a legitimate
# multi-file workload). A handful of files is realistic for a home NAS
# workload; a two- or five-file array is not, and this filler exists so
# the fixture's scale doesn't trip a real disk's own genuine safety net.
for d in disk1 disk2 disk3; do
  mkdir -p -- "$LAB/mnt/$d/filler"
  for i in 1 2 3 4 5 6; do
    head -c 512 /dev/urandom > "$LAB/mnt/$d/filler/f$i.bin"
  done
done

s7_snapraid sync

s7_log "01-setup.sh: baseline seeded and synced"
