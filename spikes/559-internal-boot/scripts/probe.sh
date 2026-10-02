#!/usr/bin/env bash
# Probes the images build-image.py wrote, inside the loop-device lab.
#
# For each image: the whole-image partition table (blkid -p, partx), then each
# partition through its own offset loop device (major 7, the only block major
# the lab container may open) with blkid -p -o udev, the same ID_FS_* names
# disk.Lister reads from the udev database. With --format-data FS the data
# partition (4) of every image that has one is formatted first, to show what an
# ordinary pool filesystem there looks like to the same probes.
#
#   docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T \
#     -e HOSERVA_LAB_ID=<id> lab bash /src/spikes/559-internal-boot/scripts/probe.sh [--format-data btrfs|xfs] [IMAGE...]
set -euo pipefail

die() { printf 'probe: %s\n' "$*" >&2; exit 1; }

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
IMG_DIR="$LAB/ib"

FORMAT=""
if [[ "${1:-}" == "--format-data" ]]; then
  FORMAT=${2:?--format-data needs btrfs or xfs}
  case "$FORMAT" in btrfs|xfs) ;; *) die "unsupported --format-data $FORMAT" ;; esac
  shift 2
fi

images=("$@")
if ((${#images[@]} == 0)); then
  shopt -s nullglob
  images=("$IMG_DIR"/*.img)
fi
((${#images[@]} > 0)) || die "no images under $IMG_DIR"

declare -a attached=()
cleanup() {
  local d
  for d in "${attached[@]}"; do
    losetup -d "$d" 2>/dev/null || true
  done
}
trap cleanup EXIT

attach() {
  local img=$1 start=$2 sectors=$3 owner
  DEV=$(losetup --find --show --offset $((start * 512)) --sizelimit $((sectors * 512)) "$img")
  attached+=("$DEV")
  owner=$(losetup -j "$img" --output NAME --noheadings | tr -d ' ' | grep -Fx "$DEV" || true)
  [[ -n "$owner" ]] || die "refusing $DEV: not backed by $img"
  rm -f /run/blkid/blkid.tab* 2>/dev/null || true
}

detach() {
  losetup -d "$1"
  local keep=() d
  for d in "${attached[@]}"; do [[ "$d" == "$1" ]] || keep+=("$d"); done
  attached=("${keep[@]}")
}

for img in "${images[@]}"; do
  case "$(realpath -- "$img")" in "$IMG_DIR"/*) ;; *) die "refusing $img: not under $IMG_DIR" ;; esac
  [[ -f "$img" && ! -L "$img" ]] || die "refusing $img: not a regular file"
  echo "##### $(basename "$img")"
  echo "--- blkid -p -o export (whole image)"
  blkid -p -o export "$img"
  echo "--- partx --show"
  partx --show -o NR,START,END,SECTORS,TYPE,NAME "$img"

  while read -r nr start _end sectors ptype pname; do
    [[ "$nr" =~ ^[0-9]+$ ]] || continue
    attach "$img" "$start" "$sectors"
    dev=$DEV
    if [[ -n "$FORMAT" && "$nr" == 4 ]]; then
      case "$FORMAT" in
        btrfs) mkfs.btrfs -q -f "$dev" ;;
        xfs) mkfs.xfs -q -f "$dev" ;;
      esac
      rm -f /run/blkid/blkid.tab* 2>/dev/null || true
      echo "--- formatted partition 4 as $FORMAT"
    fi
    echo "--- partition $nr (type $ptype, name '${pname}') blkid -p -o udev"
    rc=0
    blkid -p -o udev "$dev" || rc=$?
    ((rc == 0)) || echo "(blkid exit $rc: no filesystem or other signature found)"
    detach "$dev"
  done < <(partx --show --noheadings -o NR,START,END,SECTORS,TYPE,NAME "$img")
done
echo "probe: done"
