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

# Everything this script prints goes to stdout, die() and every failure
# diagnostic included: the step's evidence has to reach the suite log
# whatever the caller has done with its own stderr. run-l3-suite.sh's
# fd 2 is /dev/null by the time this step runs (lib.sh's vm_wait_tcp,
# called by the mid-sync recovery step, runs `exec 3>&- 2>/dev/null` in
# the caller's shell), so anything written to stderr here would vanish.
exec 2>&1

# BEFORE/AFTER are filled in below. On any non-zero exit — the verdict
# loop's own mismatch, die(), or `set -e` aborting — on_exit prints the
# command that failed (recorded by the ERR trap, which -E carries into
# functions and subshells) and both snapshots, so a red run always
# leaves the delta in the log.
set -E
BEFORE=""
AFTER=""
FAILED_CMD=""
# shellcheck disable=SC2329 # invoked by the ERR trap below
on_err() {
  local i
  FAILED_CMD="${BASH_SOURCE[1]##*/}:$1: $2 (exit $3)"
  for ((i = 1; i < ${#FUNCNAME[@]} - 1; i++)); do
    FAILED_CMD+=" <- ${FUNCNAME[i]} called at ${BASH_SOURCE[i + 1]##*/}:${BASH_LINENO[i]}"
  done
}
# shellcheck disable=SC2329 # invoked by the EXIT trap below
on_exit() {
  local rc=$?
  (( rc != 0 )) || return 0
  echo "spindown-check[$HOSERVA_LAB_ID]: exiting with status $rc"
  if [[ -n "$FAILED_CMD" ]]; then
    echo "spindown-check[$HOSERVA_LAB_ID]: last failed command: $FAILED_CMD"
  fi
  echo "spindown-check[$HOSERVA_LAB_ID]: BEFORE snapshot (/sys/block/<dev>/stat):"
  echo "${BEFORE:-  <not captured>}"
  echo "spindown-check[$HOSERVA_LAB_ID]: AFTER snapshot (/sys/block/<dev>/stat):"
  echo "${AFTER:-  <not captured>}"
}
trap 'on_err "$LINENO" "$BASH_COMMAND" "$?"' ERR
trap on_exit EXIT

# Q31's own bar is "30+ minutes"; HOSERVA_SPINDOWN_WINDOW_S exists so a
# manual run against a fresh VM can shorten it, never so the nightly run
# silently claims 30 minutes while actually measuring less.
WINDOW_S="${HOSERVA_SPINDOWN_WINDOW_S:-1800}"
POLL_INTERVAL_S="${HOSERVA_SPINDOWN_POLL_INTERVAL_S:-60}"
# The settle gate is doc 08 Spike 1's own (Q31): an explicit sync, then
# every disk's counters unchanged for 180s, sampled every 5s, failing
# after 900s. By this step the suite has built an array and run journey
# 5's sync and mass deletion on it, and XFS covers its log in 30s
# (xfssyncd_centisecs) steps for a minute or more after the last change,
# so a short gate can pass between two of those steps and let the next
# one land inside the window.
SETTLE_INTERVAL_S=5
SETTLE_QUIET_S=180
SETTLE_TIMEOUT_S=900

vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

# A virtio-blk device's own exported serial is truncated by the guest
# kernel to VIRTIO_BLK_ID_BYTES (20 bytes, include/uapi/linux/virtio_blk.h)
# before it ever reaches the guest's /sys/block/<dev>/serial — confirmed
# directly against a real VM. create-vm.sh leads each array disk's
# <serial> with its own disk_name and trails it with $HOSERVA_LAB_ID (not
# the reverse) for exactly this reason: $HOSERVA_LAB_ID's length is
# unbounded (this project's own nightly shape is far longer than a
# short, hand-picked lab id), so a leading lab id can push the
# disk-identifying suffix past the truncation point entirely, colliding
# every array disk onto the same guest-side serial. Disks are discovered
# from the live domain definition instead: libvirt's own dumpxml holds
# the exact, untruncated <serial> create-vm.sh wrote, so this never
# touches the guest's (potentially-truncated) copy or depends on
# $HOSERVA_LAB_ID's length at all. Matched against each disk's own
# <target dev='vdX'/> rather than any assumption about device enumeration
# order beyond what create-vm.sh itself already relies on (ascending PCI
# slot per array disk).
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
        "cache-hoserva-$HOSERVA_LAB_ID")
          CACHE_DEVS+=("$dev")
          ;;
        "parity"*"-hoserva-$HOSERVA_LAB_ID"|"disk"*"-hoserva-$HOSERVA_LAB_ID")
          ARRAY_DEVS+=("$dev")
          ;;
      esac
    fi
    dev=""
    serial=""
  fi
done <<<"$DOMXML"

[[ "${#ARRAY_DEVS[@]}" -gt 0 ]] || die "no array disks found in the domain XML for '$VM_DOMAIN' with a {parity,disk}*-hoserva-$HOSERVA_LAB_ID serial — is this the right domain?"
echo "spindown-check[$HOSERVA_LAB_ID]: array disks: ${ARRAY_DEVS[*]}; cache disk(s): ${CACHE_DEVS[*]:-none}"

echo "spindown-check[$HOSERVA_LAB_ID]: ensuring smartctl is present on the guest"
vm_ssh 'command -v smartctl >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq smartmontools)' >/dev/null

# smartd's own default periodic scan (enabled by the smartmontools
# package itself) is one of doc 08's own named culprits ("the SMART
# polling loop itself") — disabled here so it cannot contaminate this
# measurement of hoservad's own poller command. Hoserva's packaging
# (packaging/debian/postinst) now masks and stops this service on install
# (#161); it is disabled here too because this script measures
# hoservad's own poller command in isolation, independent of what
# packaging does on a real install.
vm_ssh 'sudo systemctl disable --now smartmontools >/dev/null 2>&1 || true'

snapshot_stats() {
  local devs="$1"
  [[ -n "$devs" ]] || return 0
  vm_ssh "for d in $devs; do printf '%s:' \"\$d\"; cat /sys/block/\$d/stat; done"
}

ALL_DEVS="${ARRAY_DEVS[*]} ${CACHE_DEVS[*]:-}"

echo "spindown-check[$HOSERVA_LAB_ID]: settle gate (sync, then every disk unchanged for ${SETTLE_QUIET_S}s, sampled every ${SETTLE_INTERVAL_S}s, ${SETTLE_TIMEOUT_S}s bound)"
vm_ssh sync
settle_start=$SECONDS
prev="$(snapshot_stats "$ALL_DEVS")"
last_change=$SECONDS
while (( SECONDS - last_change < SETTLE_QUIET_S )); do
  if (( SECONDS - settle_start >= SETTLE_TIMEOUT_S )); then
    die "settle gate did not stabilize within ${SETTLE_TIMEOUT_S}s — disks were still changing on their own before the measurement window even started"
  fi
  sleep "$SETTLE_INTERVAL_S"
  cur="$(snapshot_stats "$ALL_DEVS")"
  if [[ "$cur" != "$prev" ]]; then
    last_change=$SECONDS
    prev="$cur"
  fi
done
echo "spindown-check[$HOSERVA_LAB_ID]: settled after $((SECONDS - settle_start))s"

# No sync between here and the AFTER snapshot: on a mounted ext4 disk,
# sync(2) sends an empty cache flush even when nothing is dirty, which
# moves write_ios and flush_ios by one with no sectors written. A block
# trace of ext4 data disks formatted the way createArray formats them
# showed exactly that, issued by `sync` itself (the XFS parity disk did
# not move). The check would be measuring its own harness.
echo "spindown-check[$HOSERVA_LAB_ID]: BEFORE snapshot"
BEFORE="$(snapshot_stats "$ALL_DEVS")"

echo "spindown-check[$HOSERVA_LAB_ID]: polling every ${POLL_INTERVAL_S}s for ${WINDOW_S}s with 'smartctl -j -n standby -a <dev>' (the exact argv internal/disk's LinuxProvider.SMART issues in its default, standby-respecting mode)"
window_start=$SECONDS
poll_count=0
declare -A LAST_MESSAGE=()
while (( SECONDS - window_start < WINDOW_S )); do
  for dev in "${ARRAY_DEVS[@]}"; do
    out="$(vm_ssh "sudo smartctl -j -n standby -a /dev/$dev" 2>&1 || true)"
    # smartctl can exit nonzero on a successful, parseable poll (doc 02
    # §4 — LinuxProvider.SMART tolerates that too), so the exit status
    # alone can't gate this. But a missing smartctl JSON object means
    # the poll never actually happened (ssh failure, sudo failure,
    # missing smartctl, malformed output) — that must not be able to
    # masquerade as flat before/after counters below.
    printf '%s' "$out" | grep -Eq '"smartctl"[[:space:]]*:[[:space:]]*\{' \
      || die "SMART poll failed for /dev/$dev: $out"
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

# Instead of a sync, wait out the kernel's own writeback before the AFTER
# snapshot, so a write dirtied in the window's last seconds still reaches
# the disk and moves its counters: dirty data is written back once it is
# dirty_expire_centisecs old, on the next dirty_writeback_centisecs pass,
# and XFS pushes logged metadata every xfssyncd_centisecs (ext4 commits
# every 5s, well inside that). With no XFS module loaded there is no XFS
# filesystem to wait for, and its 30s default is used anyway.
drain_cs="$(vm_ssh 'cat /proc/sys/vm/dirty_expire_centisecs /proc/sys/vm/dirty_writeback_centisecs; cat /proc/sys/fs/xfs/xfssyncd_centisecs 2>/dev/null || echo 3000')"
drain_s=0
while IFS= read -r cs; do
  [[ "$cs" =~ ^[0-9]+$ ]] || die "unexpected writeback sysctl value from the guest: '$cs'"
  drain_s=$((drain_s + (cs + 99) / 100))
done <<<"$drain_cs"
echo "spindown-check[$HOSERVA_LAB_ID]: waiting ${drain_s}s for writeback of anything dirtied during the window (dirty_expire + dirty_writeback + xfssyncd)"
sleep "$drain_s"

echo "spindown-check[$HOSERVA_LAB_ID]: AFTER snapshot"
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

# Field names of /sys/block/<dev>/stat, in order (Documentation/block/stat.rst).
STAT_FIELDS=(read_ios read_merges read_sectors read_ticks write_ios write_merges write_sectors write_ticks in_flight io_ticks time_in_queue discard_ios discard_merges discard_sectors discard_ticks flush_ios flush_ticks)

stat_delta() {
  local -a b a
  local i out=""
  read -r -a b <<<"$1"
  read -r -a a <<<"$2"
  for i in "${!STAT_FIELDS[@]}"; do
    if [[ "${b[i]:-}" != "${a[i]:-}" ]]; then
      out+=" ${STAT_FIELDS[i]}:${b[i]:-?}->${a[i]:-?}"
    fi
  done
  echo "${out# }"
}

for dev in "${ARRAY_DEVS[@]}"; do
  if [[ "${BEFORE_MAP[$dev]:-}" != "${AFTER_MAP[$dev]:-}" ]]; then
    echo "spindown-check[$HOSERVA_LAB_ID]: FAIL — $dev moved during the window"
    echo "  before:${BEFORE_MAP[$dev]:-}"
    echo "  after: ${AFTER_MAP[$dev]:-}"
    echo "  moved: $(stat_delta "${BEFORE_MAP[$dev]:-}" "${AFTER_MAP[$dev]:-}")"
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
