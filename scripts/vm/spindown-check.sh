#!/usr/bin/env bash
# Spindown acceptance test, L3 leg (issue #33, doc 02 §1, doc 06 §6, Q31,
# Q32): with the real daemon installed and running on the guest, asserts
# that repeatedly issuing the exact `smartctl -j -n standby -a <dev>`
# invocation `internal/disk`'s LinuxProvider.SMART makes (Q32's poller)
# against every array disk over a sustained window causes zero movement
# in that disk's `/sys/block/<dev>/stat` counters — the same "counters
# stay flat" bar doc 08 Spike 1 already confirmed in the lab for idle,
# appdata and SMB-client scenarios, applied here to the SMART-polling
# scenario Spike 1's own residual risk explicitly left open ("No SMART
# polling", doc 08 §1).
#
# What this does NOT cover, and reports honestly rather than faking:
# hoservad has no scheduled SMART-poll or change-journal job wired yet
# (no api/openapi.yaml array/pool operation exists either, the same gap
# run-l3-suite.sh's own "array setup" step names) — internal/disk's SMART
# poller and internal/parity's change journal exist as Go packages
# (issue #24) but nothing in cmd/hoservad/main.go calls either on a
# timer. So this script exercises the poller's own command directly,
# looped for the window itself, standing in for "SMART polling running"
# rather than proving hoservad's own (not-yet-built) scheduler does the
# same. See docs/internal/08-spike-findings.md for the full finding.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

# Q31's own bar is "30+ minutes"; HOSERVA_SPINDOWN_WINDOW_S exists so a
# manual run against a fresh VM can shorten it, never so the nightly run
# silently claims 30 minutes while actually measuring less.
WINDOW_S="${HOSERVA_SPINDOWN_WINDOW_S:-1800}"
POLL_INTERVAL_S="${HOSERVA_SPINDOWN_POLL_INTERVAL_S:-60}"
# The settle gate is deliberately lighter than doc 08 Spike 1's own
# lab gate (an explicit sync, then unchanged for a full 3 minutes): these
# are freshly created, empty qcow2 array disks with no write workload of
# any kind behind them (unlike Spike 1's post-probe-write lab scenario),
# so two consecutive identical samples is enough evidence nothing is
# still settling, not a shortcut taken for its own sake.
SETTLE_INTERVAL_S=15
SETTLE_STABLE_SAMPLES=2
SETTLE_TIMEOUT_S=180

vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

# A virtio-blk device's own exported serial is truncated by the guest
# kernel to VIRTIO_BLK_ID_BYTES (20 bytes, include/uapi/linux/virtio_blk.h)
# before it ever reaches the guest's /sys/block/<dev>/serial — confirmed
# directly against a real VM, where create-vm.sh's own 21-byte
# "hoserva-$HOSERVA_LAB_ID-parity1" serial (for lab id 33-a1) reaches the
# guest as the 20-byte "hoserva-33-a1-parity". That truncation point
# depends on $HOSERVA_LAB_ID's own length, so no fixed-length guest-side
# pattern is safe for every lab id this harness can be given — a longer
# id (this project's own nightly shape, doc 08 Spike 1's L3 addendum)
# truncates the disk-role suffix away entirely, and a shorter one changes
# which characters survive. Disks are discovered from the live domain
# definition instead: libvirt's own dumpxml holds the exact, untruncated
# <serial> create-vm.sh wrote, so this never touches the guest's
# (potentially-truncated) copy or depends on $HOSERVA_LAB_ID's length at
# all. Matched against each disk's own <target dev='vdX'/> rather than
# any assumption about device enumeration order beyond what create-vm.sh
# itself already relies on (ascending PCI slot per array disk).
echo "spindown-check[$HOSERVA_LAB_ID]: discovering array disks from the live domain XML (never the guest's own, virtio-blk-truncated serial)"
DOMXML="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")" || die "could not read domain XML for '$VM_DOMAIN'"

ARRAY_DEVS=()
CACHE_DEVS=()
dev=""
serial=""
while IFS= read -r line; do
  case "$line" in
    *'<disk '*) dev=""; serial="" ;;
  esac
  if [[ "$line" == *'<target '* && "$line" =~ dev=\'([^\']*)\' ]]; then
    dev="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" =~ \<serial\>([^\<]*)\</serial\> ]]; then
    serial="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'</disk>'* ]]; then
    if [[ -n "$dev" && -n "$serial" ]]; then
      case "$serial" in
        "hoserva-$HOSERVA_LAB_ID-cache")
          CACHE_DEVS+=("$dev")
          ;;
        "hoserva-$HOSERVA_LAB_ID-parity"*|"hoserva-$HOSERVA_LAB_ID-disk"*)
          ARRAY_DEVS+=("$dev")
          ;;
      esac
    fi
    dev=""
    serial=""
  fi
done <<<"$DOMXML"

[[ "${#ARRAY_DEVS[@]}" -gt 0 ]] || die "no array disks found in the domain XML for '$VM_DOMAIN' with a hoserva-$HOSERVA_LAB_ID-{parity,disk}* serial — is this the right domain?"
echo "spindown-check[$HOSERVA_LAB_ID]: array disks: ${ARRAY_DEVS[*]}; cache disk(s): ${CACHE_DEVS[*]:-none}"

echo "spindown-check[$HOSERVA_LAB_ID]: ensuring smartctl is present on the guest"
vm_ssh 'command -v smartctl >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq smartmontools)' >/dev/null

# smartd's own default periodic scan (enabled by the smartmontools
# package itself) is one of doc 08's own named culprits ("the SMART
# polling loop itself") — disabled here so it cannot contaminate this
# measurement of hoservad's own poller command. Hoserva's packaging not
# depending on or disabling this on install is a separate, real gap
# (packaging/ is outside this issue's scope) — recorded, not fixed, here.
vm_ssh 'sudo systemctl disable --now smartmontools >/dev/null 2>&1 || true'

snapshot_stats() {
  local devs="$1"
  [[ -n "$devs" ]] || return 0
  vm_ssh "for d in $devs; do printf '%s:' \"\$d\"; cat /sys/block/\$d/stat; done"
}

ALL_DEVS="${ARRAY_DEVS[*]} ${CACHE_DEVS[*]:-}"

echo "spindown-check[$HOSERVA_LAB_ID]: settle gate (sync, then wait for $SETTLE_STABLE_SAMPLES consecutive stable samples, ${SETTLE_INTERVAL_S}s apart, ${SETTLE_TIMEOUT_S}s bound)"
vm_ssh sync
prev=""
stable=0
settle_start=$SECONDS
while true; do
  cur="$(snapshot_stats "$ALL_DEVS")"
  if [[ "$cur" == "$prev" ]]; then
    stable=$((stable + 1))
  else
    stable=1
  fi
  prev="$cur"
  if (( stable >= SETTLE_STABLE_SAMPLES )); then
    break
  fi
  if (( SECONDS - settle_start >= SETTLE_TIMEOUT_S )); then
    die "settle gate did not stabilize within ${SETTLE_TIMEOUT_S}s — array disks are still changing on their own before the measurement window even starts"
  fi
  sleep "$SETTLE_INTERVAL_S"
done
echo "spindown-check[$HOSERVA_LAB_ID]: settled after $((SECONDS - settle_start))s"

echo "spindown-check[$HOSERVA_LAB_ID]: BEFORE snapshot"
vm_ssh sync
BEFORE="$(snapshot_stats "$ALL_DEVS")"

echo "spindown-check[$HOSERVA_LAB_ID]: polling every ${POLL_INTERVAL_S}s for ${WINDOW_S}s with 'smartctl -j -n standby -a <dev>' (the exact argv internal/disk's LinuxProvider.SMART issues in its default, standby-respecting mode)"
window_start=$SECONDS
poll_count=0
declare -A LAST_MESSAGE=()
while (( SECONDS - window_start < WINDOW_S )); do
  for dev in "${ARRAY_DEVS[@]}"; do
    out="$(vm_ssh "sudo smartctl -j -n standby -a /dev/$dev" 2>&1 || true)"
    msg="$(printf '%s' "$out" | grep -o '"string": "[^"]*"' | head -n1)"
    LAST_MESSAGE["$dev"]="$msg"
  done
  poll_count=$((poll_count + 1))
  remaining=$((WINDOW_S - (SECONDS - window_start)))
  if (( remaining > POLL_INTERVAL_S )); then
    sleep "$POLL_INTERVAL_S"
  elif (( remaining > 0 )); then
    sleep "$remaining"
  fi
done
echo "spindown-check[$HOSERVA_LAB_ID]: ran $poll_count poll round(s) across ${#ARRAY_DEVS[@]} array disk(s) over $((SECONDS - window_start))s"

echo "spindown-check[$HOSERVA_LAB_ID]: AFTER snapshot"
vm_ssh sync
AFTER="$(snapshot_stats "$ALL_DEVS")"

echo "spindown-check[$HOSERVA_LAB_ID]: per-disk smartctl verdict during the window (informational — see doc 08 for why virtio-blk cannot return real SMART telemetry in this harness):"
for dev in "${ARRAY_DEVS[@]}"; do
  echo "  $dev: ${LAST_MESSAGE[$dev]:-<no message captured>}"
done

status=0
declare -A BEFORE_MAP=()
declare -A AFTER_MAP=()
while IFS= read -r line; do
  [[ -n "$line" ]] || continue
  BEFORE_MAP["${line%%:*}"]="${line#*:}"
done <<<"$BEFORE"
while IFS= read -r line; do
  [[ -n "$line" ]] || continue
  AFTER_MAP["${line%%:*}"]="${line#*:}"
done <<<"$AFTER"

for dev in "${ARRAY_DEVS[@]}"; do
  if [[ "${BEFORE_MAP[$dev]:-}" != "${AFTER_MAP[$dev]:-}" ]]; then
    echo "spindown-check[$HOSERVA_LAB_ID]: FAIL — $dev moved during the window" >&2
    echo "  before:${BEFORE_MAP[$dev]:-}" >&2
    echo "  after: ${AFTER_MAP[$dev]:-}" >&2
    status=1
  fi
done

for dev in "${CACHE_DEVS[@]}"; do
  if [[ "${BEFORE_MAP[$dev]:-}" != "${AFTER_MAP[$dev]:-}" ]]; then
    echo "spindown-check[$HOSERVA_LAB_ID]: note — cache disk $dev moved during the window (expected to be excluded from the array's flat-counter bar, doc 02 §1)"
  fi
done

if (( status == 0 )); then
  echo "spindown-check[$HOSERVA_LAB_ID]: every array disk's /sys/block/<dev>/stat matched byte-for-byte across the whole window"
fi

exit "$status"
