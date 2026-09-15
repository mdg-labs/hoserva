#!/usr/bin/env bash
# Builds three synthetic Unraid array data disks: single-partition MBR,
# 4K-aligned (Unraid's default for disks <= 2TB, doc 08 S2), XFS, seeded with
# Unraid-style share directories that intentionally span more than one disk
# (media, documents) and some that sit on a single disk (backup, isos) —
# matching doc 05 §1.2's "shares are a union of identically-named
# directories" claim. Every file's sha256 is recorded, keyed by the path it
# will have *in the mergerfs union*, in $S2/manifest.sha256 — the expected
# result check-fixture.sh verifies against.
#
# Each disk is cleanly unmounted at the end, so its XFS log is clean — the
# "disk pulled from a cleanly-stopped Unraid array" case (docs.unraid.net:
# stopping the array unmounts every storage device). scripts/dirty-log.sh
# covers the unclean-shutdown case separately, on its own disk.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"

MANIFEST="$S2/manifest.sha256"
: > "$MANIFEST"

seed_file() {
  local mnt=$1 relpath=$2 size=$3
  local dest="$mnt/$relpath"
  mkdir -p -- "$(dirname -- "$dest")"
  head -c "$size" /dev/urandom > "$dest"
  local hash
  hash=$(sha256sum "$dest" | awk '{print $1}')
  printf '%s  %s\n' "$hash" "$relpath" >> "$MANIFEST"
}

build_disk() {
  local name=$1; shift
  local img="$S2_IMG/$name.img"
  local mnt="$S2_MNT/$name"
  [[ -e "$img" ]] && die "refusing to recreate existing image: $img"
  # 320 MiB image (xfsprogs 6.13 refuses to format below ~300 MB); well
  # under Unraid's 2TB MBR/GPT boundary, so the documented "MBR: 4K-aligned"
  # default (helptext.txt, disk_default_partition_format_help) is the layout
  # that actually applies.
  truncate -s 320M "$img"
  local dev
  dev=$(s2_partition_disk "$img")
  mkfs.xfs -q -f "$dev"
  mkdir -p -- "$mnt"
  mount "$dev" "$mnt"
  echo "built $name: image=$img partition-dev=$dev mount=$mnt"

  # remaining args are "relpath:size" pairs
  local pair relpath size
  for pair in "$@"; do
    relpath=${pair%%:*}
    size=${pair##*:}
    seed_file "$mnt" "$relpath" "$size"
  done

  sync
  umount "$mnt"
  rmdir "$mnt"
  losetup -d "$dev"
  echo "  seeded, cleanly unmounted, loop device detached (disk now 'unplugged' until adoption)"
}

build_disk disk1 \
  "media/movies/apollo-11.bin:300000" \
  "media/tv/season01/e01.bin:150000" \
  "backup/weekly-2026-09-01.bin:500000"

build_disk disk2 \
  "media/movies/gattaca.bin:280000" \
  "media/tv/season01/e02.bin:150000" \
  "media/tv/season01/e03.bin:150000" \
  "documents/tax-2025.bin:90000"

build_disk disk3 \
  "media/movies/moon.bin:260000" \
  "documents/tax-2026.bin:90000" \
  "documents/manual.bin:40000" \
  "isos/debian-13-netinst.bin:700000"

sort -k2 -o "$MANIFEST" "$MANIFEST"
echo "manifest: $MANIFEST ($(wc -l < "$MANIFEST") files)"
