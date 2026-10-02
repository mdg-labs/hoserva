#!/usr/bin/env bash
# Applies the internal-boot fingerprint to every image under $LAB/ib and prints
# one verdict per image. The rule is the one Unraid's own API applies
# (unraid/api DisksService.matchesInternalBootLayout, doc 08 section 2): at least four
# partitions, numbered 1 to 4 in order, whose GPT names and type GUIDs are
#   1 'BIOS Boot Partition'   21686148-6449-6e6f-744e-656564454649
#   2 'EFI System Partition'  c12a7328-f81f-11d2-ba4b-00a0c93ec93b
#   3 'Unraid Boot Partition' 0fc63daf-8483-4772-8e79-3d69d8477de4
#   4 (any name)              0fc63daf-8483-4772-8e79-3d69d8477de4
# It reads the partition table only: no filesystem is probed, so no data
# partition is opened. Exits non-zero when a verdict differs from what the
# image's file name promises (internal-boot-* must match, anything else must not).
#
#   docker compose -f docker-compose.dev.yml -p hoserva-lab-<id> exec -T \
#     -e HOSERVA_LAB_ID=<id> lab bash /src/spikes/559-internal-boot/scripts/classify.sh
set -euo pipefail

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
IMG_DIR="/lab/$HOSERVA_LAB_ID/ib"

BIOS=21686148-6449-6e6f-744e-656564454649
ESP=c12a7328-f81f-11d2-ba4b-00a0c93ec93b
LINUX=0fc63daf-8483-4772-8e79-3d69d8477de4

classify() {
  local img=$1 nr type name line table
  local -A ptype=() pname=()
  table=$(partx --pairs -o NR,TYPE,NAME "$img")
  while IFS= read -r line; do
    [[ "$line" =~ NR=\"([0-9]+)\"\ TYPE=\"([^\"]*)\"\ NAME=\"([^\"]*)\" ]] || continue
    nr=${BASH_REMATCH[1]} type=${BASH_REMATCH[2]} name=${BASH_REMATCH[3]}
    ptype[$nr]=${type,,}
    pname[$nr]=$name
  done <<<"$table"
  if [[ "${pname[1]:-}" == "BIOS Boot Partition" && "${ptype[1]:-}" == "$BIOS" \
     && "${pname[2]:-}" == "EFI System Partition" && "${ptype[2]:-}" == "$ESP" \
     && "${pname[3]:-}" == "Unraid Boot Partition" && "${ptype[3]:-}" == "$LINUX" \
     && "${ptype[4]:-}" == "$LINUX" ]]; then
    echo internal-boot
  else
    echo other
  fi
}

fail=0
shopt -s nullglob
for img in "$IMG_DIR"/*.img; do
  got=$(classify "$img")
  case "$(basename "$img")" in
    internal-boot-*-renamed-*) want=other ;;
    internal-boot-*) want=internal-boot ;;
    *) want=other ;;
  esac
  status=ok
  [[ "$got" == "$want" ]] || { status=MISMATCH; fail=1; }
  printf '%-40s %-14s (expected %-14s) %s\n' "$(basename "$img")" "$got" "$want" "$status"
done
exit "$fail"
