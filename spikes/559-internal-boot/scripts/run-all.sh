#!/usr/bin/env bash
# Host-side driver: writes the images into this lab's own directory, then runs
# the probes and the classifier inside the lab, before and after formatting the
# data partitions. Needs the lab up (`make lab-up HOSERVA_LAB_ID=<id>`).
#
#   HOSERVA_LAB_ID=<id> bash spikes/559-internal-boot/scripts/run-all.sh
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd -- "$HERE/../../.." && pwd)
OUT="$ROOT/.lab/$HOSERVA_LAB_ID/ib"
RESULTS="$HERE/../results"
mkdir -p -- "$OUT" "$RESULTS"

lab() {
  (cd -- "$ROOT" && docker compose -f docker-compose.dev.yml -p "hoserva-lab-$HOSERVA_LAB_ID" \
    exec -T -e "HOSERVA_LAB_ID=$HOSERVA_LAB_ID" lab bash "$@")
}

build() { python3 "$HERE/build-image.py" --out "$OUT" "$@"; }

{
  build --mode shared --devices 2
  build --mode dedicated
  build --mode array-gpt
  build --mode shared --p3-name 'Hoserva Boot Partition'
} >"$RESULTS/build.log"

S=/src/spikes/559-internal-boot/scripts
lab -c 'blkid --version; partx --version; mkfs.btrfs --version; mkfs.xfs -V' >"$RESULTS/versions.log" 2>&1
lab "$S/probe.sh" >"$RESULTS/probe-baseline.log"
lab "$S/classify.sh" >"$RESULTS/classify.log"
lab "$S/probe.sh" --format-data btrfs "/lab/$HOSERVA_LAB_ID/ib/internal-boot-shared-1.img" >"$RESULTS/probe-data-btrfs.log"
lab "$S/probe.sh" --format-data xfs "/lab/$HOSERVA_LAB_ID/ib/internal-boot-shared-2.img" >"$RESULTS/probe-data-xfs.log"
lab "$S/classify.sh" >"$RESULTS/classify-after-format.log"
echo "run-all: PASS"
