#!/usr/bin/env bash
# Seeds the lab's mergerfs pool with synthetic data of the file-size
# distributions doc 06 §3 names — mover and create-policy behaviour depend
# on file size, so realistic corpora matter more than realistic content.
# Runs only inside the hoserva-lab container, only under $LAB/mnt/user.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=./lib.sh
source "$HERE/lib.sh"
lab_require_id

profile=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --profile)
      [[ $# -ge 2 ]] || die "--profile needs a value"
      profile=$2
      shift 2
      ;;
    --profile=*)
      profile=${1#*=}
      shift
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done
[[ -n "$profile" ]] || die "usage: seed-data.sh --profile <media|photos|appdata|mixed>"

target="$LAB/mnt/user"
mountpoint -q "$target" || die "no pool mounted at $target — run create-array.sh first"

fill_random() {  # path, bytes
  head -c "$2" /dev/urandom > "$1"
}

seed_media() {  # few, huge files
  mkdir -p "$target/media"
  local i
  for i in 1 2 3; do
    fallocate -l 256M "$target/media/movie$i.mkv"
  done
}

seed_photos() {  # many, medium files
  mkdir -p "$target/photos"
  local i name
  for i in $(seq 1 60); do
    name=$(printf 'img_%04d.jpg' "$i")
    fill_random "$target/photos/$name" 3M
  done
}

seed_appdata() {  # many, tiny, hot files
  mkdir -p "$target/appdata"
  local i name
  for i in $(seq 1 300); do
    name=$(printf 'cfg_%04d.db' "$i")
    fill_random "$target/appdata/$name" 4K
  done
}

case "$profile" in
  media) seed_media ;;
  photos) seed_photos ;;
  appdata) seed_appdata ;;
  mixed) seed_media; seed_photos; seed_appdata ;;
  *) die "unknown profile: $profile (expected media|photos|appdata|mixed)" ;;
esac

echo "seeded profile '$profile' under $target"
