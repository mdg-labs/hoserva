#!/usr/bin/env bash
# Step 1: write the single-parity snapraid.conf, seed an initial dataset
# directly onto disk1..disk3's own mount points (not through the mergerfs
# pool — SnapRAID protects the data disks directly, doc 02 §2's "data
# d1..dN — data disks by mount point"; writing through the pool would hand
# placement to mergerfs's create policy and make which disk gets which file
# nondeterministic, which this spike does not want), and run the first
# "sync" to build initial parity — snapraid.txt §4 ("Getting started").
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

cat > "$CONF" <<EOF
parity $LAB/mnt/parity1/snapraid.parity
content $LAB/mnt/parity1/snapraid.content
content $LAB/mnt/cache/snapraid.content
data d1 $LAB/mnt/disk1/
data d2 $LAB/mnt/disk2/
data d3 $LAB/mnt/disk3/
EOF
echo "wrote $CONF:"
cat "$CONF"

# One copy for the single parity disk used plus one (snapraid.txt §7.4) —
# parity1 and cache are two distinct physical devices, neither a data disk.
seed_file() {  # disk name, relpath, size-for-head-c
  local disk=$1 relpath=$2 size=$3
  local dest="$LAB/mnt/$disk/$relpath"
  mkdir -p -- "$(dirname -- "$dest")"
  head -c "$size" /dev/urandom > "$dest"
}

seed_file disk1 "movies/apollo-11.bin" 2000000
seed_file disk1 "movies/gattaca.bin" 1500000
seed_file disk1 "photos/img0001.bin" 400000
seed_file disk2 "movies/moon.bin" 1800000
seed_file disk2 "documents/tax-2025.bin" 300000
seed_file disk2 "documents/manual.bin" 250000
seed_file disk3 "backup/weekly.bin" 2200000
seed_file disk3 "isos/debian-netinst.bin" 3000000
sync

echo "seeded $(find "$LAB/mnt/disk1" "$LAB/mnt/disk2" "$LAB/mnt/disk3" -type f | wc -l) files"
find "$LAB/mnt/disk1" "$LAB/mnt/disk2" "$LAB/mnt/disk3" -type f -printf '%p %s\n' | sort

echo "### diff before first sync (expect: everything 'added', nothing else) ###"
s5_snapraid diff || true

echo "### first sync ###"
s5_snapraid sync

echo "### post-sync diff (expect: everything 'equal', return code 0) ###"
s5_snapraid diff

s5_array_manifest disk1 disk2 disk3 > "$S5/manifest-after-initial-sync.sha256"
echo "baseline manifest: $S5/manifest-after-initial-sync.sha256 ($(wc -l < "$S5/manifest-after-initial-sync.sha256") files)"
