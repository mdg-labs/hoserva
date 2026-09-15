#!/usr/bin/env bash
# Builds a loop-device array inside the lab: a parity disk, N data disks and
# a cache disk, each XFS, plus a mergerfs pool over the data disks (doc 06
# §3). Runs only inside the hoserva-lab container — see docker-compose.dev.yml
# and `make lab-up`.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

DATA_DISKS=${LAB_DATA_DISKS:-3}
DATA_SIZE=${LAB_DATA_SIZE:-1G}
PARITY_SIZE=${LAB_PARITY_SIZE:-1G}
CACHE_SIZE=${LAB_CACHE_SIZE:-512M}
MINFREESPACE=${LAB_MINFREESPACE:-50M}

[[ "$DATA_DISKS" =~ ^[0-9]+$ && "$DATA_DISKS" -ge 1 ]] || die "LAB_DATA_DISKS must be a positive integer, got '$DATA_DISKS'"

# name, size -> creates $LAB/img/<name>.img, attaches it to a loop device
# this lab owns, formats it XFS and mounts it at $LAB/mnt/<name>. Every path
# is built here, from our own $LAB, never taken from an argument.
create_disk() {
  local name=$1 size=$2
  local img="$LAB/img/$name.img"
  [[ -e "$img" ]] && die "refusing to recreate existing image: $img (run destroy-array.sh first)"
  truncate -s "$size" "$img"
  local dev
  dev=$(losetup --find --show "$img")
  lab_assert_own_loop "$dev" "$img"
  mkfs.xfs -q -L "$name" "$dev"
  mkdir -p "$LAB/mnt/$name"
  mount "$dev" "$LAB/mnt/$name"
}

mkdir -p "$LAB/mnt/user"

create_disk parity1 "$PARITY_SIZE"

data_mounts=()
for i in $(seq 1 "$DATA_DISKS"); do
  create_disk "disk$i" "$DATA_SIZE"
  data_mounts+=("$LAB/mnt/disk$i")
done

create_disk cache "$CACHE_SIZE"

branches=$(IFS=:; echo "${data_mounts[*]}")
mergerfs -o category.create=mspmfs,moveonenospc=true,minfreespace="$MINFREESPACE" \
  "$branches" "$LAB/mnt/user"

echo "lab $HOSERVA_LAB_ID ready:"
echo "  parity: $LAB/mnt/parity1"
echo "  data:   ${data_mounts[*]}"
echo "  cache:  $LAB/mnt/cache"
echo "  pool:   $LAB/mnt/user"
