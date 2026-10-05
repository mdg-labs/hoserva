#!/usr/bin/env bash
# `make vm-migration-suite VARIANT=<variant>|all [LAYOUT=shared-nvme]` — the
# migration test procedure of doc 06 §5 (issue #80) against the synthetic Unraid
# fixtures, in this lab's own L3 VM. For a supported variant it runs the nine
# steps through the real `hoserva` CLI and API, in the order the product has
# them (doc 05 §4: the containers need the point of no return, which needs a
# passing verify), each record carrying the procedure's own step number:
#
#   1 restore the fixture snapshot (built first when this lab has none)
#   2 the fixture's disks are the VM's array disks
#   3 install Hoserva (the .deb under test), onboard
#   4 scan both Flash Backup zips, and for a USB-boot server the stick as a
#     read-only USB disk; each report matches the fixture's expected result
#   5 import, every file of the data disks present through /mnt/user with the
#     manifest's sha256, verify passes
#   8 the point of no return, the initial sync completes
#   6 ownership, directories and SMB shares survived
#   7 appdata relocated to the cache and intact, templates convert, the
#     containers start one at a time and see their data
#   8 a scrub covers the array and reports no errors
#   9 delete a file, fix restores it, the checksum matches
#
# A refusal variant (a fixture whose expect file refuses a disk) runs steps 1 to
# 3, scans both zips, asserts the exact refusal message of each refused disk,
# asserts the import is refused and that the whole-device sha256 of every source
# disk is the one the builder recorded.
#
# Every step records PASS or FAIL. A step the product does not do (a missing
# command, a refusal where the fixture says go) is a FAIL with the reason; a
# step that would pass without the product doing the work is a FAIL; a step that
# follows one that failed is a FAIL "not reached", never a skip. A failed import
# is undone, so it is tried again (up to three attempts, each its own record).
# The exit status is non-zero when any step failed. The VM is destroyed on exit.
#
# VARIANT=all runs every supported and refusal variant, and the shared-NVMe
# layout of unraid-7x-xfs-single-parity (LAYOUT=shared-nvme: the fixture's cache
# disk is replaced by the spare partition of the OS disk, doc 01 §6), one after
# the other on this lab id.
#
# Accommodations for the lab, each logged on the run's output where it is made:
#  - the fixtures' cache disks are 256 MiB, below the 300 MB mkfs.xfs needs and
#    the 50G mergerfs minfreespace Hoserva mounts with, so the guest's cache
#    disk is grown to 64 GiB (a sparse image) before the scan; the shared
#    layout's spare partition is created at 64G for the same reason;
#  - the nightly maintenance chain is switched off after onboarding, so that only
#    the suite's own actions write parity (see onboard).
#
# HOSERVA_MIGRATION_KEEP_VM=1 leaves the VM for inspection (make vm-destroy
# removes it); DEB=<path> reuses a built .deb for every run;
# MIGRATION_ARTIFACT_DIR=<dir> keeps each scan's report (hoserva --json migrate
# status) and the step record there, and, for a run that failed, the guest's
# hoservad journal and `hoserva doctor` output (collect-diagnostics.sh) taken
# before the VM is destroyed, for nightly-migration.yml to upload.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"
# shellcheck source=scripts/vm/unraid-lib.sh
source "$script_dir/unraid-lib.sh"

# Every run of the suite: a fixture variant and the layout of the Hoserva host.
# migration-suite-coverage-check.sh holds this list, the committed fixtures and
# nightly-migration.yml's matrix to one set. unraid-with-vms is #97's.
MIGRATION_RUNS=(
  "unraid-6.12-xfs-single-parity separate"
  "unraid-7x-xfs-single-parity separate"
  "unraid-7x-xfs-single-parity shared-nvme"
  "unraid-dual-parity separate"
  "unraid-btrfs-and-ext4-disks separate"
  "unraid-corrupt-xfs separate"
  "unraid-named-pools separate"
  "unraid-no-cache separate"
  "unraid-encrypted separate"
  "unraid-zfs-disk separate"
  "unraid-internal-boot separate"
  "unraid-internal-boot-shared separate"
)

if [[ "${MIGRATION_LIST_RUNS:-}" == "1" ]]; then
  printf '%s\n' "${MIGRATION_RUNS[@]}"
  exit 0
fi

# the cache disk (or spare partition) is made this large: more than mergerfs's
# 50G minfreespace and far more than mkfs.xfs's 300 MB minimum
CACHE_GROWN_GIB=64
CONFIRM_TIMEOUT=1200

declare -a STEP_NAMES=()
declare -a STEP_RESULTS=()
ANY_FAILED=0
RUN_FAILED=0
DIAGNOSTICS_TAKEN=0
RUN_LABEL=""

record() {
  STEP_NAMES+=("$RUN_LABEL: $1")
  STEP_RESULTS+=("$2")
}
pass() {
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: $RUN_LABEL: $1 — PASS${2:+ ($2)}"
  record "$1" "PASS"
}
fail() {
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: $RUN_LABEL: $1 — FAIL: $2" >&2
  record "$1" "FAIL: $2"
  ANY_FAILED=1
  RUN_FAILED=1
}
# a step that follows one that failed is a failure of its own, never a skip
not_reached() {
  fail "$1" "not reached: $2"
}

# ---------------------------------------------------------------- the guest

# hs runs the hoserva CLI in the guest as root over its Unix socket, bounded.
hs() {
  vm_ssh "sudo timeout $CONFIRM_TIMEOUT hoserva $*" </dev/null
}

# hsq runs the hoserva CLI like hs, with every argument quoted for the guest's
# shell: for arguments that come from what a command printed (a stack name, a
# confirmation) and not from this script.
hsq() {
  local quoted
  quoted=$(printf ' %q' "$@")
  vm_ssh "sudo timeout $CONFIRM_TIMEOUT hoserva$quoted" </dev/null
}

# shq WORD prints WORD quoted for the guest's shell. Every guest command that
# carries a name, path or value taken from a fixture, a manifest, a template or
# a command's output is built with it.
shq() {
  printf '%q' "$1"
}

# api METHOD PATH [JSON] calls the daemon's API over its Unix socket, the way the
# CLI does; the body is in API_BODY and the HTTP status in API_CODE. A command
# the CLI has no subcommand for (a share relocation, a parity status) goes
# through here.
API_BODY=""
API_CODE=""
api() {
  local method=$1 path=$2 body=${3:-} out
  if [[ -n "$body" ]]; then
    out=$(vm_ssh "sudo timeout 120 curl -s -w '\\n%{http_code}' -X $method --unix-socket /run/hoserva/hoserva.sock -H 'Authorization: Bearer peer-credentials-trusted' -H 'Content-Type: application/json' -d '$body' http://unix/api/v1$path" </dev/null) || return 1
  else
    out=$(vm_ssh "sudo timeout 120 curl -s -w '\\n%{http_code}' -X $method --unix-socket /run/hoserva/hoserva.sock -H 'Authorization: Bearer peer-credentials-trusted' http://unix/api/v1$path" </dev/null) || return 1
  fi
  API_CODE=${out##*$'\n'}
  API_BODY=${out%$'\n'*}
}

# wait_job ID SECONDS leaves the job's final JSON in JOB_JSON and succeeds when
# it succeeded; a job still running at the deadline, or any other end, fails.
JOB_JSON=""
wait_job() {
  local id=$1 timeout_s=$2 deadline status
  deadline=$((SECONDS + timeout_s))
  while ((SECONDS < deadline)); do
    api GET "/jobs/$id" || return 1
    JOB_JSON=$API_BODY
    status=$(jq -r '.status // empty' <<<"$JOB_JSON" 2>/dev/null || true)
    case "$status" in
      succeeded) return 0 ;;
      failed | cancelled | interrupted) return 1 ;;
    esac
    sleep 2
  done
  return 1
}

# ------------------------------------------------------------ the variant

# load_variant fills the run's own view of its fixture: the disks of its spec,
# whether it is a refusal variant, and where its expected result is.
declare -A D_KIND D_FS D_SIZE D_TARGET D_POOL D_BOOT
D_SLOTS=()
load_variant() {
  local line words w slot
  D_SLOTS=()
  D_KIND=() D_FS=() D_SIZE=() D_TARGET=() D_POOL=() D_BOOT=()
  VDIR="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT"
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" == disk\ * ]] || continue
    read -r -a words <<<"$line"
    slot=${words[1]}
    D_SLOTS+=("$slot")
    for w in "${words[@]:2}"; do
      case "$w" in
        kind=*) D_KIND[$slot]=${w#kind=} ;;
        fs=*) D_FS[$slot]=${w#fs=} ;;
        size=*) D_SIZE[$slot]=${w#size=} ;;
        target=*) D_TARGET[$slot]=${w#target=} ;;
        pool=*) D_POOL[$slot]=${w#pool=} ;;
        boot=*) D_BOOT[$slot]=${w#boot=} ;;
      esac
    done
  done <"$VDIR/spec"
  EXPECT_FILE="$VDIR/expect"
  REFUSAL=0
  if [[ -f "$EXPECT_FILE" ]] && grep -qE '^disk [^ ]+ refuse ' "$EXPECT_FILE"; then REFUSAL=1; fi
  NO_CAPTURE=0
  if grep -qx 'capture=none' "$VDIR/spec"; then NO_CAPTURE=1; fi
  EXPECTED_HOST="$VM_ROOT/unraid/$VARIANT/expected"
  EXPECTED_GUEST="$UNRAID_GUEST_OUT/$VARIANT/expected"
  POOL_SLOTS=""
  CACHE_SLOT=""
  local s
  for s in "${D_SLOTS[@]}"; do
    case "${D_KIND[$s]}" in
      pool | boot) POOL_SLOTS+="${POOL_SLOTS:+,}$s" ;;
    esac
    if [[ "${D_KIND[$s]}" == pool && "${D_POOL[$s]:-}" == cache ]]; then CACHE_SLOT=$s; fi
  done
  HAVE_CACHE=0
  if [[ -n "$CACHE_SLOT" ]]; then HAVE_CACHE=1; fi
}

# serial_of SLOT is the serial the guest reports for the slot's disk, the 20
# bytes the kernel keeps of what create-vm.sh gave it (as the builder's serial_of).
serial_of() {
  local s="${D_TARGET[$1]}-hoserva-$HOSERVA_LAB_ID"
  echo "${s:0:20}"
}

# domain_dev_for_serial SERIAL prints the target dev (vdh) of this lab's disk
# with that serial, from the live domain XML.
domain_dev_for_serial() {
  virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN" | awk -v want="$1" '
    /<disk /       { dev = ""; ser = "" }
    /<target dev=/ { match($0, /dev=.[a-z0-9]+./); dev = substr($0, RSTART + 5, RLENGTH - 6) }
    /<serial>/     { s = $0; gsub(/.*<serial>|<\/serial>.*/, "", s); ser = s }
    /<\/disk>/     { if (ser == want) { print dev; exit } }'
}

# ----------------------------------------------------------------- step 1

STEP_REASON=""

prepare_vm() {
  local topology=$1 spare
  spare="${HOSERVA_VM_SPARE_PARTITION_SIZE:-${CACHE_GROWN_GIB}G}"
  if vm_domain_exists "$VM_DOMAIN"; then
    if [[ "$(cat "$VM_STATE_DIR/topology" 2>/dev/null || true)" != "$topology" ]]; then
      STEP_REASON="the lab's existing VM '$VM_DOMAIN' is not in the $topology topology: run make vm-destroy first"
      return 1
    fi
  else
    echo "vm-migration-suite[$HOSERVA_LAB_ID]: creating the $topology VM, its array disks sized from the $VARIANT spec"
    if ! HOSERVA_VM_TOPOLOGY="$topology" HOSERVA_VM_SPARE_PARTITION_SIZE="$spare" VARIANT="$VARIANT" "$script_dir/create-vm.sh"; then
      STEP_REASON="make vm-up VARIANT=$VARIANT failed (its own message above)"
      return 1
    fi
  fi
  if ! virsh -c "$VM_CONNECT" snapshot-list "$VM_DOMAIN" --name | grep -qxF -- "$VARIANT"; then
    echo "vm-migration-suite[$HOSERVA_LAB_ID]: no fixture snapshot '$VARIANT' on this lab: building it (make vm-unraid-fixture)"
    if ! VARIANT="$VARIANT" "$script_dir/unraid-fixture.sh"; then
      STEP_REASON="make vm-unraid-fixture VARIANT=$VARIANT failed (its own message above)"
      return 1
    fi
  fi
  if ! NAME="$VARIANT" "$script_dir/restore-vm.sh"; then
    STEP_REASON="make vm-restore NAME=$VARIANT failed"
    return 1
  fi
  [[ -d "$EXPECTED_HOST" ]] || {
    STEP_REASON="the fixture's expected result $EXPECTED_HOST does not exist"
    return 1
  }
}

# The fixtures' cache pools are lab-sized (256 MiB), below what mkfs.xfs and
# mergerfs's minfreespace accept; grow the cache disk of the VM, a sparse image,
# before anything reads it. Nothing in the fixture refers to the disk's size.
grow_cache_disk() {
  local slot=$CACHE_SLOT dev bytes want
  [[ -n "$slot" ]] || return 0
  [[ -z "${D_BOOT[$slot]:-}" ]] || return 0
  dev=$(domain_dev_for_serial "$(serial_of "$slot")")
  [[ -n "$dev" ]] || {
    STEP_REASON="the domain has no disk with the serial $(serial_of "$slot")"
    return 1
  }
  bytes=$(numfmt --from=iec "${D_SIZE[$slot]}")
  want=$((CACHE_GROWN_GIB * 1024 * 1024 * 1024))
  if ((bytes >= want)); then return 0; fi
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: growing the cache disk $dev from ${D_SIZE[$slot]} to ${CACHE_GROWN_GIB}G (mkfs.xfs needs 300 MB, mergerfs's minfreespace is 50G)"
  vm_assert_own_domain "$VM_DOMAIN"
  virsh -c "$VM_CONNECT" blockresize "$VM_DOMAIN" "$dev" "$((CACHE_GROWN_GIB * 1024 * 1024))KiB" >/dev/null
}

# shared NVMe: Debian's own disk carries the cache, so the fixture's cache disk
# is not attached any more
detach_fixture_cache() {
  local slot=$CACHE_SLOT dev
  [[ -n "$slot" ]] || return 0
  dev=$(domain_dev_for_serial "$(serial_of "$slot")")
  [[ -n "$dev" ]] || {
    STEP_REASON="the domain has no disk with the serial $(serial_of "$slot")"
    return 1
  }
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: shared NVMe: detaching the fixture's cache disk $dev; the OS disk's spare partition is the cache"
  vm_assert_own_domain "$VM_DOMAIN"
  virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$dev" --live >/dev/null
}

# ----------------------------------------------------------------- step 2

# fs_probe_type FS is what blkid reports for a partition holding fixture
# filesystem FS.
fs_probe_type() {
  case "$1" in
    luks-xfs) echo crypto_LUKS ;;
    zfs) echo zfs_member ;;
    xfs | ext4 | btrfs) echo "$1" ;;
    *) echo "" ;;
  esac
}

check_fixture_disks() {
  local s serial link want got bad=""
  for s in "${D_SLOTS[@]}"; do
    if [[ "$LAYOUT" == shared-nvme && "$s" == "$CACHE_SLOT" ]]; then continue; fi
    serial=$(serial_of "$s")
    link="/dev/disk/by-id/virtio-$serial"
    if ! vm_ssh "test -b $(shq "$link")"; then
      bad+=" $s ($link is not a block device);"
      continue
    fi
    if [[ -n "${D_BOOT[$s]:-}" || "${D_KIND[$s]}" == boot ]]; then
      got=$(vm_ssh "sudo blkid -o value -s TYPE $(shq "$link-part3")" 2>/dev/null || true)
      [[ "$got" == zfs_member ]] || bad+=" $s (partition 3 holds '${got:-nothing}', not Unraid's ZFS boot pool);"
      continue
    fi
    want=$(fs_probe_type "${D_FS[$s]}")
    [[ -n "$want" ]] || continue
    got=$(vm_ssh "sudo blkid -o value -s TYPE $(shq "$link-part1")" 2>/dev/null || true)
    [[ "$got" == "$want" ]] || bad+=" $s (partition 1 holds '${got:-nothing}', the fixture says $want);"
  done
  if [[ -n "$bad" ]]; then
    STEP_REASON="the guest's disks are not the fixture's:$bad"
    return 1
  fi
}

# ----------------------------------------------------------------- step 3

install_hoserva() {
  if ! DEB="${DEB:-}" TAG="${TAG:-}" "$script_dir/deploy.sh"; then
    STEP_REASON="deploy.sh failed (its own message above)"
    return 1
  fi
}

onboard() {
  local status created
  status=$(vm_ssh "curl -sk https://127.0.0.1:8008/api/v1/setup/status" 2>/dev/null || true)
  if [[ "$(jq -r '.adminExists | tostring' <<<"$status" 2>/dev/null || true)" != "false" ]]; then
    STEP_REASON="getSetupStatus did not report adminExists=false: $status"
    return 1
  fi
  created=$(vm_ssh "curl -sk -X POST https://127.0.0.1:8008/api/v1/setup/admin -H 'Content-Type: application/json' -d '{\"username\":\"hoserva-l3\",\"password\":\"hoserva-l3-suite-password\"}'" 2>/dev/null || true)
  if [[ "$(jq -r '.username // empty' <<<"$created" 2>/dev/null || true)" != "hoserva-l3" ]]; then
    STEP_REASON="createFirstAdmin did not return the admin: $created"
    return 1
  fi
  # the import writes the shares into smb.conf and the exports, which Hoserva
  # may only do for files it manages (Q76)
  if ! vm_ssh 'sudo hoserva doctor apply-host-config --samba import --nfs import' >/dev/null 2>&1; then
    STEP_REASON="hoserva doctor apply-host-config --samba import --nfs import failed"
    return 1
  fi
  # On a host set up after the chain's start time the scheduler runs the nightly
  # chain (mover, sync, scrub, config backup) on its first tick, and a sync that
  # lands between the suite's delete and its fix would absorb the delete. The
  # steps are switched off, as a user does in doc 05 step 24, so that only the
  # suite's own actions write parity (#622 tracks the product behaviour this
  # works around).
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: $RUN_LABEL: switching the nightly maintenance chain off after onboarding, so that no scheduled sync lands between the suite's delete and its fix (#622; doc 06 §5)"
  api PUT /settings/schedules/chain '{"steps":[{"id":"mover","enabled":false},{"id":"diff_guard","enabled":false},{"id":"sync","enabled":false},{"id":"scrub","enabled":false},{"id":"config_backup","enabled":false}]}' || {
    STEP_REASON="could not call updateMaintenanceChainSchedule"
    return 1
  }
  if [[ ! "$API_CODE" =~ ^2 ]] || [[ "$(jq -r '[.chain.steps[]? | select(.enabled)] | length' <<<"$API_BODY" 2>/dev/null || echo x)" != 0 ]]; then
    STEP_REASON="updateMaintenanceChainSchedule did not switch the chain off: HTTP $API_CODE $API_BODY"
    return 1
  fi
}

# ----------------------------------------------------------------- step 4

SCAN_JSON=""
SCAN_OUT=""

# scan_zip ZIP scans the Flash Backup at ZIP (a host file) through the CLI and
# leaves the migration's JSON in SCAN_JSON.
scan_zip() {
  local zip=$1 remote
  remote="/tmp/$(basename "$zip")"
  vm_scp "$zip" "hoserva@127.0.0.1:$remote" >/dev/null || {
    STEP_REASON="could not copy $zip to the guest"
    return 1
  }
  if ! SCAN_OUT=$(hsq migrate scan --flash-backup "$remote" 2>&1); then
    STEP_REASON="hoserva migrate scan --flash-backup exited non-zero: $(tail -n 3 <<<"$SCAN_OUT" | tr '\n' ' ')"
    return 1
  fi
  SCAN_JSON=$(hs --json migrate status 2>&1) || {
    STEP_REASON="hoserva --json migrate status failed: $SCAN_JSON"
    return 1
  }
  save_artifact "$(basename "$zip" .zip).scan.json" "$SCAN_JSON"
}

# save_artifact NAME CONTENT keeps what a step saw under MIGRATION_ARTIFACT_DIR,
# for a failed run to be read from its artifacts; nothing when it is not set.
save_artifact() {
  local dir="${MIGRATION_ARTIFACT_DIR:-}/$VARIANT"
  [[ -n "${MIGRATION_ARTIFACT_DIR:-}" ]] || return 0
  [[ "$LAYOUT" == separate ]] || dir+="-$LAYOUT"
  mkdir -p -- "$dir"
  printf '%s\n' "$2" >"$dir/$1"
}

# what migrate.ErrContainerUnconfirmed says (internal/migrate/containers.go); the
# daemon appends ": <stack>" with the stack it is waiting on, and the API answers
# 409 container_unconfirmed.
UNCONFIRMED_REFUSAL="another migrated container was started and is not confirmed yet: confirm that it sees its data, or stop it, before starting the next"

# the refusal message each refusal reason has (datadisks.go), as the report row
# words it: "<slot> is not adopted: <refusal>". An integrity refusal carries the
# check's own output between a fixed lead and a fixed tail.
refusal_code() {
  case "$1" in
    integrity) echo integrity_check ;;
    encrypted) echo encrypted ;;
    zfs) echo zfs ;;
    multi-device-btrfs) echo multi_device_btrfs ;;
    *) echo "" ;;
  esac
}
refusal_message_ok() {  # reason, slot, detail
  local reason=$1 slot=$2 detail=$3 prefix="$2 is not adopted: "
  case "$reason" in
    encrypted)
      [[ "$detail" == "${prefix}it is encrypted (LUKS; the flash says luks:xfs, the device says crypto_LUKS). Hoserva does not adopt encrypted disks: the recovery risk is too high (Q22). Decrypt it in Unraid first, or leave it out of the migration" ]]
      ;;
    zfs)
      [[ "$detail" == "${prefix}it is a ZFS disk (the flash says zfs, the device says zfs_member). Hoserva cannot read ZFS without OpenZFS and does not adopt it (Q23)" ]]
      ;;
    multi-device-btrfs)
      [[ "$detail" == "${prefix}it is one of 2 devices of a btrfs filesystem, which is not a self-contained filesystem per disk (Q23)" ]]
      ;;
    integrity)
      [[ "$detail" == "${prefix}its read-only xfs check failed ("* && "$detail" == *"). Computing parity over a damaged filesystem would keep the damage."* ]]
      ;;
    *) return 1 ;;
  esac
}

# expected_baseline prints, for a data slot or a share, "<files> <symlinks>
# <special>" from the fixture's manifest and entries: every file, symlink and
# FIFO, socket or device node under a share directory of a data disk. Hidden
# top-level directories are not shares and the scan leaves them out. A pool is
# not scanned.
expected_baseline() {  # slot|share name
  local kind=$1 name=$2 files syms specials
  files=$(grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' -v pools=",$POOL_SLOTS," -v kind="$kind" -v name="$name" '
    index(pools, "," $5 ",") == 0 {
      split($6, p, "/"); if (p[1] ~ /^\./) next
      if ((kind == "slot" && $5 == name) || (kind == "share" && p[1] == name)) n++
    } END { print n + 0 }')
  syms=$(grep -v '^#' "$EXPECTED_HOST/entries.tsv" | awk -F'\t' -v pools=",$POOL_SLOTS," -v kind="$kind" -v name="$name" '
    index(pools, "," $2 ",") == 0 && $1 == "symlink" {
      split($3, p, "/"); if (p[1] ~ /^\./) next
      if ((kind == "slot" && $2 == name) || (kind == "share" && p[1] == name)) n++
    } END { print n + 0 }')
  specials=$(grep -v '^#' "$EXPECTED_HOST/entries.tsv" | awk -F'\t' -v pools=",$POOL_SLOTS," -v kind="$kind" -v name="$name" '
    index(pools, "," $2 ",") == 0 && ($1 == "fifo" || $1 == "socket" || $1 == "chardev" || $1 == "blockdev") {
      split($3, p, "/"); if (p[1] ~ /^\./) next
      if ((kind == "slot" && $2 == name) || (kind == "share" && p[1] == name)) n++
    } END { print n + 0 }')
  echo "$files $syms $specials"
}

# assert_scan checks SCAN_JSON against the fixture: the verdict, each disk's
# verdict, and for a variant that is scanned in full the baseline per disk and
# share. It prints each difference on its own line to ASSERT_OUT.
ASSERT_OUT=""
assert_scan() {
  local out="" s slot want want_refused reason expect_line detail code row verdict
  verdict=$(jq -r '.report.verdict // empty' <<<"$SCAN_JSON")
  if [[ "$(jq -r '.phase // empty' <<<"$SCAN_JSON")" != scanned ]]; then
    out+="the migration is in phase '$(jq -r '.phase // "none"' <<<"$SCAN_JSON")', not scanned"$'\n'
  fi
  if ((REFUSAL)); then
    [[ "$verdict" == no_go ]] || out+="the verdict is '$verdict', not no_go"$'\n'
  else
    [[ "$verdict" != no_go && -n "$verdict" ]] || out+="the verdict is '${verdict:-none}': a supported variant must not be refused"$'\n'
    if jq -e '.report.rows[] | select(.status == "refuse")' <<<"$SCAN_JSON" >/dev/null 2>&1; then
      out+="the report has refuse rows: $(jq -r '[.report.rows[] | select(.status == "refuse") | .subject + ": " + .detail] | join("; ")' <<<"$SCAN_JSON")"$'\n'
    fi
  fi

  # each disk of the fixture, by its slot
  for s in "${D_SLOTS[@]}"; do
    case "${D_KIND[$s]}" in
      data | parity) ;;
      *) continue ;;
    esac
    slot=$s
    if ((NO_CAPTURE)); then
      # without the capture's disks.ini the scan knows no slot: each disk of this
      # machine is a row of its own, found by its serial
      row=$(jq -c --arg serial "$(serial_of "$s")" '.report.review.disks[]? | select(.serial == $serial)' <<<"$SCAN_JSON" | head -n 1)
    else
      row=$(jq -c --arg slot "$slot" '.report.review.disks[]? | select(.slot == $slot)' <<<"$SCAN_JSON" | head -n 1)
    fi
    if [[ -z "$row" ]]; then
      out+="the report's disk table has no row for $slot"$'\n'
      continue
    fi
    if [[ "$(jq -r '.serial // empty' <<<"$row")" != "$(serial_of "$s")" ]]; then
      out+="$slot: the report matched serial '$(jq -r '.serial // "none"' <<<"$row")', the fixture's disk has $(serial_of "$s")"$'\n'
    fi
    expect_line=""
    if [[ -f "$EXPECT_FILE" ]]; then expect_line=$(awk -v s="$s" '$1 == "disk" && $2 == s { $1 = ""; $2 = ""; sub(/^ +/, ""); print }' "$EXPECT_FILE"); fi
    want_refused=false
    reason=""
    if [[ "$expect_line" == refuse\ * ]]; then
      want_refused=true
      reason=${expect_line#refuse }
    fi
    if [[ "$(jq -r '.refused' <<<"$row")" != "$want_refused" ]]; then
      out+="$slot: refused is $(jq -r '.refused' <<<"$row"), the fixture says $want_refused"$'\n'
    fi
    if [[ "$want_refused" == true ]]; then
      code=$(refusal_code "$reason")
      if [[ -z "$code" || "$(jq -r '.refusalCode // empty' <<<"$row")" != "$code" ]]; then
        out+="$slot: refusal code is '$(jq -r '.refusalCode // "none"' <<<"$row")', the fixture says '$reason' ($code)"$'\n'
      fi
      detail=$(jq -r --arg slot "$slot" '[.report.rows[] | select(.status == "refuse" and .subject == $slot and (.check == "data_disks" or .check == "disk_integrity")) | .detail] | first // empty' <<<"$SCAN_JSON")
      if ! refusal_message_ok "$reason" "$slot" "$detail"; then
        out+="$slot: the refusal message is not the one for '$reason': '${detail:-none}'"$'\n'
      fi
    else
      if [[ "${D_KIND[$s]}" == data ]]; then
        want=$(fs_probe_type "${D_FS[$s]}")
        [[ "$(jq -r '.filesystem // empty' <<<"$row")" == "$want" ]] || out+="$slot: the report says filesystem '$(jq -r '.filesystem // "none"' <<<"$row")', the fixture says $want"$'\n'
        if ((NO_CAPTURE == 0)) && [[ "$(jq -r '.proposedRole // empty' <<<"$row")" != data ]]; then
          out+="$slot: the proposed role is '$(jq -r '.proposedRole // "none"' <<<"$row")', not data"$'\n'
        fi
      elif ((NO_CAPTURE == 0)) && [[ "$(jq -r '.proposedRole // empty' <<<"$row")" != parity ]]; then
        out+="$slot: the proposed role is '$(jq -r '.proposedRole // "none"' <<<"$row")', not parity"$'\n'
      fi
    fi
  done

  # the capture and the config source, where the expect file says so
  if [[ -f "$EXPECT_FILE" ]] && grep -qx 'warn capture-missing' "$EXPECT_FILE"; then
    [[ "$(jq -r '.report.review.capture.state // empty' <<<"$SCAN_JSON")" == missing ]] || out+="the capture state is '$(jq -r '.report.review.capture.state // "none"' <<<"$SCAN_JSON")', the fixture has none"$'\n'
  fi
  if [[ -f "$EXPECT_FILE" ]] && grep -qx 'config-source zip' "$EXPECT_FILE"; then
    [[ "$(jq -r '.zipOnly' <<<"$SCAN_JSON")" == true ]] || out+="the migration does not say the zip is the only source (zipOnly), the fixture boots Unraid from an internal device"$'\n'
  fi

  # what the scan counted on each data disk and share, against the manifest; a
  # scan without the capture reads no data disk and records no baseline
  if ((NO_CAPTURE)) && jq -e '.report.rows[] | select(.check == "baseline")' <<<"$SCAN_JSON" >/dev/null 2>&1; then
    out+="the report has baseline rows, but the fixture has no capture to name the data disks"$'\n'
  fi
  if ((!REFUSAL && !NO_CAPTURE)); then
    local f sy sp
    for s in "${D_SLOTS[@]}"; do
      [[ "${D_KIND[$s]}" == data ]] || continue
      read -r f sy sp <<<"$(expected_baseline slot "$s")"
      row=$(jq -r --arg slot "$s" '[.report.rows[] | select(.check == "baseline" and .subject == $slot) | .detail] | first // empty' <<<"$SCAN_JSON")
      if [[ ! "$row" =~ ^([0-9]+)\ files?\ \([^\)]*\),\ ([0-9]+)\ symlinks?\ and\ ([0-9]+)\ special\ files?\; ]]; then
        out+="$s: the report has no baseline row: '${row:-none}'"$'\n'
      elif [[ "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]}" != "$f $sy $sp" ]]; then
        out+="$s: the baseline counts ${BASH_REMATCH[1]} files, ${BASH_REMATCH[2]} symlinks, ${BASH_REMATCH[3]} special files; the manifest has $f, $sy, $sp"$'\n'
      fi
    done
    local share
    while IFS= read -r share; do
      [[ -n "$share" ]] || continue
      read -r f sy sp <<<"$(expected_baseline share "$share")"
      row=$(jq -r --arg s "$share" '[.report.rows[] | select(.check == "baseline" and .subject == $s) | .detail] | first // empty' <<<"$SCAN_JSON")
      if [[ ! "$row" =~ ^([0-9]+)\ files?\ \([^\)]*\),\ ([0-9]+)\ symlinks?\ and\ ([0-9]+)\ special\ files?, ]]; then
        out+="share $share: the report has no baseline row: '${row:-none}'"$'\n'
      elif [[ "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]}" != "$f $sy $sp" ]]; then
        out+="share $share: the baseline counts ${BASH_REMATCH[1]} files, ${BASH_REMATCH[2]} symlinks, ${BASH_REMATCH[3]} special files; the manifest has $f, $sy, $sp"$'\n'
      fi
    done < <(share_names_with_data)
  fi
  ASSERT_OUT=$out
  [[ -z "$out" ]]
}

# the shares (first path component, no hidden one) that hold something on a data disk
share_names_with_data() {
  {
    grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' -v pools=",$POOL_SLOTS," 'index(pools, "," $5 ",") == 0 { split($6, p, "/"); if (p[1] !~ /^\./) print p[1] }'
    grep -v '^#' "$EXPECTED_HOST/entries.tsv" | awk -F'\t' -v pools=",$POOL_SLOTS," 'index(pools, "," $2 ",") == 0 && ($1 == "symlink" || $1 == "fifo" || $1 == "socket" || $1 == "chardev" || $1 == "blockdev") { split($3, p, "/"); if (p[1] !~ /^\./) print p[1] }'
  } | sort -u
}

# ------------------------------------------------------- source disk hashes

# source_hashes prints "<sha256> <bytes> <slot>" for every whole source device,
# read in the guest the way the builder reads them (source-disks.sha256).
source_hashes() {
  local s list=""
  for s in "${D_SLOTS[@]}"; do
    if [[ "$LAYOUT" == shared-nvme && "$s" == "$CACHE_SLOT" ]]; then continue; fi
    list+="$s $(serial_of "$s")"$'\n'
  done
  vm_ssh 'sudo bash -s' <<REMOTE
set -euo pipefail
while read -r slot serial; do
  [[ -n "\$slot" ]] || continue
  dev=\$(readlink -f "/dev/disk/by-id/virtio-\$serial")
  [[ -b "\$dev" ]] || exit 1
  blockdev --flushbufs "\$dev"
  printf '%s\t%s\t%s\n' "\$(sha256sum "\$dev" | cut -d' ' -f1)" "\$(blockdev --getsize64 "\$dev")" "\$slot"
done <<'LIST'
$list
LIST
REMOTE
}

# ----------------------------------------------------------------- the runs

run_refusal() {
  local before after recorded zip name bad
  # the builder's record of every source device, taken before any scan
  recorded=$(grep -v "^#" "$EXPECTED_HOST/source-disks.sha256" 2>/dev/null || true)
  if [[ -z "$recorded" ]]; then
    fail "source disks recorded" "the fixture recorded no source-disks.sha256"
    return
  fi
  before=$(source_hashes) || {
    fail "source disks hashed" "could not read a source device in the guest"
    return
  }
  if [[ "$before" == "$recorded" ]]; then
    pass "source disks are the ones the builder hashed" "$(wc -l <<<"$before") devices"
  else
    fail "source disks are the ones the builder hashed" "the sha256 of a source device differs from source-disks.sha256 before any scan"
  fi

  for zip in flash-backup.zip flash-hand-zipped.zip; do
    name="4 scan $zip: every refused disk is refused by name"
    if ! scan_zip "$EXPECTED_HOST/$zip"; then
      fail "$name" "$STEP_REASON"
      continue
    fi
    if assert_scan; then
      pass "$name" "verdict no_go"
    else
      fail "$name" "$(tr '\n' ';' <<<"$ASSERT_OUT")"
    fi
  done

  name="5 import is refused and nothing was written"
  local import_out import_rc=0
  import_out=$(hs migrate import --yes 2>&1) || import_rc=$?
  bad=""
  ((import_rc != 0)) || bad+="hoserva migrate import --yes exited 0; "
  if [[ "$import_out" != *"migration_no_go"* || "$import_out" != *"the scan's verdict is no-go"* ]]; then
    bad+="the refusal is not migration_no_go (the scan's verdict is no-go): $(tail -n 1 <<<"$import_out"); "
  fi
  if [[ "$(hs --json migrate status | jq -r '.phase')" != scanned ]]; then bad+="the migration left the scanned phase; "; fi
  if vm_ssh "mount | grep -E ' on /mnt/(disk|user|parity|cache)'" >/dev/null 2>&1; then bad+="a source disk or the pool is mounted; "; fi
  after=$(source_hashes) || bad+="could not read a source device again; "
  [[ "$after" == "$recorded" ]] || bad+="the whole-device sha256 of a source disk changed (before: $(wc -l <<<"$before"), recorded and after differ); "
  if [[ -z "$bad" ]]; then
    pass "$name" "$(tail -n 1 <<<"$import_out")"
  else
    fail "$name" "$bad"
  fi
}

# ----------------------------------------------------- the supported variants

GUEST_HELPER=/tmp/migration-suite-guest.sh

# guest_check runs one subcommand of migration-suite-guest.sh in the guest as
# root and leaves its output in GUEST_OUT.
GUEST_OUT=""
guest_check() {
  local quoted
  quoted=$(printf ' %q' "$@")
  GUEST_OUT=$(vm_ssh "sudo $GUEST_HELPER$quoted" 2>&1)
}

push_guest_helper() {
  vm_scp "$script_dir/migration-suite-guest.sh" "hoserva@127.0.0.1:$GUEST_HELPER" >/dev/null &&
    vm_ssh "chmod +x $GUEST_HELPER"
}

# usb_boot_variant: Unraid booted from a USB stick, so the stick is a second
# source (an internal boot has none, Q25)
usb_boot_variant() {
  [[ ! -f "$EXPECT_FILE" ]] || ! grep -qx 'config-source zip' "$EXPECT_FILE"
}

# scan_stick builds the fixture's flash as a FAT32 image labelled UNRAID in the
# guest, attaches it read-only as a USB disk, scans it with --flash-device and
# checks that the image's sha256 is the same afterwards.
scan_stick() {
  local img_guest=/home/hoserva/unraid-stick.img img_host before after dev waited=0 qimg qflash
  qimg=$(shq "$img_guest")
  qflash=$(shq "$UNRAID_GUEST_OUT/$VARIANT/flash/.")
  img_host="$VM_STATE_DIR/unraid-stick.img"
  if ! vm_ssh 'sudo bash -s' <<REMOTE
set -euo pipefail
command -v mkfs.vfat >/dev/null 2>&1 || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends dosfstools >/dev/null
rm -f $qimg
truncate -s 128M $qimg
mkfs.vfat -F 32 -n UNRAID $qimg >/dev/null
d=\$(mktemp -d)
mount -t vfat -o loop,rw,umask=0,shortname=mixed $qimg "\$d"
cp -r --no-preserve=mode,ownership,timestamps $qflash "\$d/"
sync
umount "\$d"
rmdir "\$d"
chown hoserva $qimg
REMOTE
  then
    STEP_REASON="could not build the stick image in the guest"
    return 1
  fi
  if ! vm_scp "hoserva@127.0.0.1:$img_guest" "$img_host" >/dev/null; then
    STEP_REASON="could not copy the stick image out of the guest"
    return 1
  fi
  vm_ssh "rm -f $qimg"
  before=$(sha256sum "$img_host" | cut -d' ' -f1)
  if ! "$script_dir/usb-image.sh" attach "$img_host"; then
    STEP_REASON="could not attach the stick image as a USB disk"
    return 1
  fi
  dev=""
  while ((waited < 60)); do
    dev=$(hs --json migrate status 2>/dev/null | jq -r '.flashDevices[0].device // empty' 2>/dev/null || true)
    [[ -z "$dev" ]] || break
    sleep 2
    waited=$((waited + 2))
  done
  if [[ ! "$dev" =~ ^/dev/[a-z0-9]+$ ]]; then
    "$script_dir/usb-image.sh" detach || true
    STEP_REASON="hoserva migrate status lists no usable flash device after the USB disk was attached: '${dev:-none}'"
    return 1
  fi
  if ! SCAN_OUT=$(hsq migrate scan --flash-device "$dev" 2>&1); then
    "$script_dir/usb-image.sh" detach || true
    STEP_REASON="hoserva migrate scan --flash-device $dev exited non-zero: $(tail -n 3 <<<"$SCAN_OUT" | tr '\n' ' ')"
    return 1
  fi
  if ! SCAN_JSON=$(hs --json migrate status 2>&1); then
    "$script_dir/usb-image.sh" detach || true
    STEP_REASON="hoserva --json migrate status failed: $SCAN_JSON"
    return 1
  fi
  "$script_dir/usb-image.sh" detach
  after=$(sha256sum "$img_host" | cut -d' ' -f1)
  if [[ "$before" != "$after" ]]; then
    STEP_REASON="the stick image's sha256 changed (before $before, after $after)"
    return 1
  fi
  if [[ "$(jq -r '.sourceDevice // empty' <<<"$SCAN_JSON")" != "$dev" ]]; then
    STEP_REASON="the migration's source is '$(jq -r '.sourceDevice // "none"' <<<"$SCAN_JSON")', not the stick $dev"
    return 1
  fi
  save_artifact "stick.scan.json" "$SCAN_JSON"
  STEP_DETAIL="stick sha256 $after unchanged"
}

# import_args leaves the --role and --cache-partition arguments the import needs
# in IMPORT_ARGS: none when the scan proposed a role for every parity and data disk of the
# fixture (the user confirming the printed table), and every disk's role from
# the fixture's spec when the capture has no disks.ini to propose them from.
IMPORT_ARGS=()
import_args() {
  local s role retry=${1:-}
  IMPORT_ARGS=()
  if ((NO_CAPTURE)); then
    for s in "${D_SLOTS[@]}"; do
      case "${D_KIND[$s]}" in
        parity) role=parity ;;
        data) role=data ;;
        pool) if [[ "$s" == "$CACHE_SLOT" ]]; then role=cache; else role=ignore; fi ;;
        *) role=ignore ;;
      esac
      if [[ "$LAYOUT" == shared-nvme && "$s" == "$CACHE_SLOT" ]]; then continue; fi
      IMPORT_ARGS+=(--role "$(serial_of "$s")=$role")
    done
  fi
  # Hoserva has one cache, and Unraid's own boot device is never an array disk:
  # every other pool of a server with several, and a boot device that holds no
  # cache, are left out of the migration, as the user does once the import
  # refuses the proposal
  if [[ "$retry" == retry && $NO_CAPTURE == 0 ]]; then
    for s in "${D_SLOTS[@]}"; do
      if [[ ( "${D_KIND[$s]}" == pool && "$s" != "$CACHE_SLOT" ) || "${D_KIND[$s]}" == boot ]]; then IMPORT_ARGS+=(--role "$(serial_of "$s")=ignore"); fi
    done
  fi
  if [[ "$LAYOUT" == shared-nvme ]]; then
    local boot_serial partuuid
    boot_serial="boot-hoserva-$HOSERVA_LAB_ID"
    boot_serial=${boot_serial:0:20}
    if ! partuuid=$(vm_ssh "sudo lsblk -no PARTUUID $(shq "/dev/disk/by-id/virtio-$boot_serial-part2")" 2>/dev/null) || [[ ! "$partuuid" =~ ^[0-9a-f-]{36}$ ]]; then
      return 1
    fi
    IMPORT_ARGS+=(--cache-partition "virtio-$boot_serial-part2:$partuuid")
  fi
}

STEP_DETAIL=""

step_scans() {
  local zip name
  for zip in flash-backup.zip flash-hand-zipped.zip; do
    name="4 scan $zip matches the fixture's expected result"
    if ! scan_zip "$EXPECTED_HOST/$zip"; then
      fail "$name" "$STEP_REASON"
      SCAN_OK=0
      continue
    fi
    if assert_scan; then
      pass "$name" "verdict $(jq -r '.report.verdict' <<<"$SCAN_JSON")"
    else
      fail "$name" "$(tr '\n' ';' <<<"$ASSERT_OUT")"
      SCAN_OK=0
    fi
  done
  if usb_boot_variant; then
    name="4 scan --flash-device (the stick as a read-only USB disk) matches, the image is unchanged"
    STEP_REASON="" STEP_DETAIL=""
    if ! scan_stick; then
      fail "$name" "$STEP_REASON"
      SCAN_OK=0
    elif assert_scan; then
      pass "$name" "$STEP_DETAIL"
    else
      fail "$name" "$(tr '\n' ';' <<<"$ASSERT_OUT")"
      SCAN_OK=0
    fi
  else
    name="4 internal boot: the zip is the only source"
    if [[ "$(hs --json migrate status | jq -r '.zipOnly')" == true && "$(jq -r '.flashDevices | length' <<<"$SCAN_JSON")" == 0 ]]; then
      pass "$name"
    else
      fail "$name" "the migration offers a flash device, or does not say zipOnly"
      SCAN_OK=0
    fi
  fi
  # the import works from the report the last scan stored
  if ((SCAN_OK)); then
    scan_zip "$EXPECTED_HOST/flash-backup.zip" >/dev/null || SCAN_OK=0
  fi
}

step_import() {
  local out name="5 import adopts the data disks read-only" attempt=1
  # The first attempt takes the roles the scan proposed, as the user confirming
  # the printed table does. A failed import is undone, and running the command
  # again is what its own message offers (the user also leaves out the disks the
  # import refuses a role for): each attempt that fails is a FAIL of its own, and
  # the steps after it still run when a later attempt adopts the disks.
  while :; do
    if ! { if ((attempt == 1)); then import_args; else import_args retry; fi; }; then
      fail "$name" "could not read the cache partition's PARTUUID"
      return 1
    fi
    if out=$(hsq migrate import "${IMPORT_ARGS[@]}" --yes 2>&1); then break; fi
    fail "$name (attempt $attempt)" "$(tail -n 1 <<<"$out")"
    attempt=$((attempt + 1))
    if ((attempt > 3)); then return 1; fi
  done
  ((attempt == 1)) || name="$name (attempt $attempt)"
  local bad="" s
  [[ "$(hs --json migrate status | jq -r '.phase')" == imported ]] || bad+="the phase is not imported; "
  if ! vm_ssh "findmnt -no OPTIONS /mnt/user | grep -qw ro"; then bad+="/mnt/user is not mounted read-only; "; fi
  for s in "${D_SLOTS[@]}"; do
    [[ "${D_KIND[$s]}" == data ]] || continue
    vm_ssh "findmnt -no OPTIONS $(shq "/mnt/$s") | grep -qw ro" || bad+="/mnt/$s is not mounted read-only; "
  done
  if [[ -n "$bad" ]]; then
    fail "$name" "$bad"
    return 1
  fi
  pass "$name" "${args:-roles as the scan proposed them}"
}

step_files() {
  local name="5 every file of the data disks is present through /mnt/user with the manifest's sha256"
  if guest_check files "$EXPECTED_GUEST" /mnt/user "${POOL_SLOTS:--}"; then
    pass "$name" "$GUEST_OUT"
  else
    fail "$name" "$(tr '\n' ';' <<<"$GUEST_OUT")"
  fi
}

step_verify() {
  local out name="5 hoserva migrate verify passes against the scan's baseline"
  if out=$(hs migrate verify 2>&1); then
    pass "$name" "$(tail -n 1 <<<"$out")"
  else
    fail "$name" "$(tail -n 4 <<<"$out" | tr '\n' ' ')"
    return 1
  fi
}

# the number of parity and data disks the fixture has
count_kind() {
  local s n=0
  for s in "${D_SLOTS[@]}"; do
    [[ "${D_KIND[$s]}" == "$1" ]] && n=$((n + 1))
  done
  echo "$n"
}

step_point_of_no_return() {
  local conf out name="8 the point of no return formats the former parity and cache disks"
  if ! conf=$(hs --json migrate status | jq -r '.parityInit.confirmation // empty') || [[ -z "$conf" ]]; then
    fail "$name" "the migration offers no point of no return (parityInit.confirmation is empty)"
    return 1
  fi
  if ! out=$(hsq migrate initialize-parity --confirm "$conf" 2>&1); then
    fail "$name" "$(tail -n 3 <<<"$out" | tr '\n' ' ')"
    return 1
  fi
  pass "$name" "$conf"
  name="8 the initial sync completes and parity is current"
  local deadline=$((SECONDS + 120)) job=""
  while ((SECONDS < deadline)); do
    job=$(hs --json jobs | jq -r '[.jobs[]? | select(.type == "sync")] | first | .id // empty' 2>/dev/null || true)
    [[ -z "$job" ]] || break
    sleep 2
  done
  if [[ -z "$job" ]]; then
    fail "$name" "no sync job was queued after the point of no return"
    return 1
  fi
  if ! wait_job "$job" 600; then
    fail "$name" "the initial sync $job did not succeed: $(jq -c '{status, error}' <<<"$JOB_JSON" 2>/dev/null || echo "$JOB_JSON")"
    return 1
  fi
  api GET /parity
  local want_data want_parity
  want_data=$(count_kind data)
  want_parity=$(count_kind parity)
  if [[ "$(jq -r '.freshness' <<<"$API_BODY")" == green && "$(jq -r '.changedSinceSync' <<<"$API_BODY")" == 0 &&
    "$(jq -r '.dataDisks' <<<"$API_BODY")" == "$want_data" && "$(jq -r '.parityDisks' <<<"$API_BODY")" == "$want_parity" ]]; then
    pass "$name" "$(jq -c . <<<"$API_BODY")"
  else
    fail "$name" "parity is not current: $API_BODY (want $want_data data and $want_parity parity disks)"
    return 1
  fi
}

# expected_shares prints "<name> <exported>" for every share the flash configures
# whose directory exists on a disk of the fixture and whose name Hoserva accepts,
# the shares the import must have created (doc 05 §4); exported is true for
# Unraid's shareExport e, eh, et and eth.
expected_shares() {
  local cfg name code dirs
  dirs=$({
    grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' '{ split($6, p, "/"); print p[1] }'
    grep -v '^#' "$EXPECTED_HOST/entries.tsv" | awk -F'\t' '{ split($3, p, "/"); print p[1] }'
  } | sort -u)
  vm_ssh "sudo grep -H '^shareExport=' $(shq "$UNRAID_GUEST_OUT/$VARIANT/flash/config/shares")/*.cfg" 2>/dev/null | while IFS= read -r cfg; do
    name=${cfg%%.cfg:*}
    name=${name##*/}
    code=${cfg#*shareExport=}
    code=${code//\"/}
    [[ "$name" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || continue
    grep -qxF -- "$name" <<<"$dirs" || continue
    case "$code" in e | eh | et | eth) echo "$name true $code" ;; *) echo "$name false $code" ;; esac
  done
}

step_ownership_and_shares() {
  local name="6 ownership (UID 99, GID 100), directories and file modes survived"
  if guest_check owners "$EXPECTED_GUEST" /mnt "${POOL_SLOTS:--}"; then
    pass "$name" "$GUEST_OUT"
  else
    fail "$name" "$(tr '\n' ';' <<<"$GUEST_OUT")"
  fi

  name="6 the shares and their SMB exports survived"
  local bad="" share exported code shares smbconf count browse
  api GET /shares
  shares=$API_BODY
  smbconf=$(vm_ssh 'sudo cat /etc/samba/smb.conf' 2>/dev/null || true)
  if ! vm_ssh 'sudo testparm -s >/dev/null 2>&1'; then bad+="testparm rejects smb.conf; "; fi
  local n=0
  while read -r share exported code; do
    [[ -n "$share" ]] || continue
    n=$((n + 1))
    if ! jq -e --arg n "$share" '.shares[]? | select(.name == $n)' <<<"$shares" >/dev/null 2>&1; then
      bad+="share $share does not exist; "
      continue
    fi
    if [[ "$(jq -r --arg n "$share" '.shares[] | select(.name == $n) | .smb.enabled' <<<"$shares")" != "$exported" ]]; then
      bad+="share $share: smb.enabled is not $exported (shareExport $code); "
    fi
    count=$(grep -c "^\[$share\]" <<<"$smbconf" || true)
    if [[ "$exported" == true && "$count" != 1 ]] || [[ "$exported" == false && "$count" != 0 ]]; then
      bad+="smb.conf has $count [$share] sections for shareExport $code; "
    fi
    if [[ "$exported" == true ]]; then
      case "$code" in e | et) browse=true ;; *) browse=false ;; esac
      if [[ "$(jq -r --arg n "$share" '.shares[] | select(.name == $n) | .smb.browseable' <<<"$shares")" != "$browse" ]]; then
        bad+="share $share: browseable is not $browse (shareExport $code); "
      fi
    fi
  done < <(expected_shares)
  ((n > 0)) || bad+="the flash configures no share whose directory exists; "
  if [[ -z "$bad" ]]; then
    pass "$name" "$n shares"
  else
    fail "$name" "$bad"
  fi
}

step_appdata() {
  local name="7 appdata is relocated to the cache, its links, special files and sparse files intact" job
  if ((!HAVE_CACHE)); then
    # there is no cache to move appdata to: the relocation may be refused or end
    # as a job, and either way appdata stays on the data disks, intact
    local answer bad="" status
    name="7 appdata relocation without a cache moves nothing and loses nothing"
    api POST /shares/appdata/relocate '{"to":"cache"}'
    answer="HTTP $API_CODE"
    if [[ "$API_CODE" =~ ^2 ]]; then
      job=$(jq -r '.id // empty' <<<"$API_BODY" 2>/dev/null || true)
      if [[ -n "$job" ]]; then
        wait_job "$job" 300 || true
        status=$(jq -r '.status // empty' <<<"$JOB_JSON" 2>/dev/null || true)
        answer+=", job $status"
        case "$status" in succeeded | failed | cancelled) ;; *) bad+="the relocation job is still $status; " ;; esac
      else
        bad+="the relocation answered HTTP $API_CODE without a job: $API_BODY; "
      fi
    fi
    if vm_ssh 'mountpoint -q /mnt/cache'; then bad+="a cache is mounted at /mnt/cache; "; fi
    guest_check files "$EXPECTED_GUEST" /mnt/user "${POOL_SLOTS:--}" || bad+="files differ: $(tr '\n' ';' <<<"$GUEST_OUT")"
    if [[ -z "$bad" ]]; then
      pass "$name" "$answer"
    else
      fail "$name" "$bad($answer)"
    fi
    return
  fi
  api POST /shares/appdata/relocate '{"to":"cache"}'
  job=$(jq -r '.id // empty' <<<"$API_BODY" 2>/dev/null || true)
  if [[ ! "$API_CODE" =~ ^2 || -z "$job" ]]; then
    fail "$name" "startShareRelocation answered HTTP $API_CODE: $API_BODY"
    return
  fi
  if ! wait_job "$job" 600; then
    fail "$name" "the relocation job $job did not succeed: $(jq -c '{status, error}' <<<"$JOB_JSON" 2>/dev/null || echo "$JOB_JSON")"
    return
  fi
  if guest_check relocated "$EXPECTED_GUEST" /mnt/cache appdata "${POOL_SLOTS:--}"; then
    pass "$name" "$GUEST_OUT"
  else
    fail "$name" "$(tr '\n' ';' <<<"$GUEST_OUT")"
  fi
}

# install_docker installs Docker Engine and Compose in the guest (Hoserva's
# prerequisite, D8: never a dependency of the .deb) and imports a busybox image
# under every image name the fixture's templates and Compose Manager projects
# use, so the containers start without a registry.
install_docker() {
  local flash images
  flash=$(shq "$UNRAID_GUEST_OUT/$VARIANT/flash/config/plugins")
  vm_ssh 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends docker.io docker-cli docker-compose busybox-static >/dev/null && sudo systemctl start docker && sudo docker info >/dev/null' || {
    STEP_REASON="could not install and start Docker in the guest"
    return 1
  }
  images=$(vm_ssh "{ sudo grep -rhoE '<Repository>[^<]+' $flash/dockerMan/templates-user/ 2>/dev/null | sed 's/<Repository>//'; sudo grep -rhE '^[[:space:]]*image:' $flash/compose.manager/projects/ 2>/dev/null | sed -E 's/^[[:space:]]*image:[[:space:]]*//'; } | sort -u") || true
  if [[ -z "$images" ]]; then
    STEP_REASON="the fixture's flash names no container image"
    return 1
  fi
  vm_ssh 'sudo bash -s' <<REMOTE || {
set -euo pipefail
w=\$(mktemp -d)
mkdir -p "\$w/rootfs/bin"
cp /bin/busybox "\$w/rootfs/bin/busybox"
tar -C "\$w/rootfs" -cf "\$w/rootfs.tar" .
while read -r img; do
  [[ -n "\$img" ]] || continue
  docker import --change 'CMD ["/bin/busybox","sleep","31536000"]' "\$w/rootfs.tar" "\$img" >/dev/null
done <<'LIST'
$images
LIST
rm -rf "\$w"
REMOTE
    STEP_REASON="could not import the container images in the guest"
    return 1
  }
}

# sample_file PREFIX prints "<sha256> <path>" of one file of a data disk under
# the share-relative PREFIX/ that has a plain name and exists once in the manifest
sample_file() {
  grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' -v pools=",$POOL_SLOTS," -v pre="$1/" '
    { cnt[$6]++; row[NR] = $1 "\t" $5 "\t" $6; n = NR }
    END {
      for (i = 1; i <= n; i++) {
        split(row[i], f, "\t")
        if (index(pools, "," f[2] ",") == 0 && index(f[3], pre) == 1 && cnt[f[3]] == 1 && f[3] ~ /^[A-Za-z0-9._\/-]+$/) { print f[1] "\t" f[3]; exit }
      }
    }'
}

# data_under PREFIX: whether a data disk of the fixture holds anything under the
# share-relative PREFIX/
data_under() {
  grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' -v pools=",$POOL_SLOTS," -v pre="$1/" 'index(pools, "," $5 ",") == 0 && index($6, pre) == 1 { found = 1 } END { exit !found }'
}

# container_data_check STACK: reads the stack's data check, and for every path
# it reports ok reads a file of the fixture through the container itself. A path
# that is not ok must be one whose data the fixture kept only on the cache, which
# the point of no return erased. It leaves what it found in CONTAINER_MSG and in
# CONTAINER_ACCEPT the confirm flag such a path needs; non-zero on a difference.
CONTAINER_ACCEPT=""
CONTAINER_MSG=""
container_data_check() {
  local stack=$1 chk path dest status ctr rel sample sha sp cpath got bad="" n=0
  CONTAINER_ACCEPT=""
  CONTAINER_MSG=""
  # a path that is not ok makes the command exit non-zero; its JSON says which
  chk=$(hsq --json migrate containers check "$stack" 2>/dev/null) || true
  if ! jq -e '.paths | type == "array"' <<<"$chk" >/dev/null 2>&1; then
    CONTAINER_MSG="the data check of $stack returned no paths: ${chk:-nothing}"
    return 1
  fi
  while IFS=$'\t' read -r ctr path dest status; do
    n=$((n + 1))
    case "$path" in
      /mnt/user/*) rel=${path#/mnt/user/} ;;
      /mnt/cache/*) rel=${path#/mnt/cache/} ;;
      *) rel="" ;;
    esac
    if [[ "$status" == ok ]]; then
      [[ -n "$rel" ]] || continue
      sample=$(sample_file "${rel%/}") || sample=""
      [[ -n "$sample" ]] || continue
      IFS=$'\t' read -r sha sp <<<"$sample"
      cpath="${dest%/}/${sp#"${rel%/}/"}"
      if ! got=$(vm_ssh "sudo docker exec $(shq "$ctr") /bin/busybox sha256sum $(shq "$cpath")" 2>&1 </dev/null) || [[ "${got%% *}" != "$sha" ]]; then
        bad+="$ctr cannot read $cpath with the fixture's sha256 ($path is ok to the check): ${got:-no output}; "
      fi
    else
      if [[ -n "$rel" ]] && data_under "${rel%/}"; then
        bad+="$ctr: $path is $status, but the fixture's data disks hold files under it; "
      else
        CONTAINER_ACCEPT=" --accept-failed-check"
      fi
    fi
  done < <(jq -r '.paths[] | [.container, .path, .destination, .status] | @tsv' <<<"$chk")
  ((n > 0)) || bad+="the stack has no path under /mnt/user or /mnt/cache; "
  if [[ -n "$bad" ]]; then
    CONTAINER_MSG=$bad
    return 1
  fi
  CONTAINER_MSG="$n paths read"
}

step_templates_and_containers() {
  local name tpl list f stack out i=0 second refused
  local -a names=() acks=() confirm_args=()
  STEP_REASON=""
  if ! install_docker; then
    fail "7 templates convert and the containers start" "$STEP_REASON"
    return
  fi

  name="7 the templates convert"
  if ! tpl=$(hs --json migrate templates 2>&1); then
    fail "$name" "hoserva migrate templates failed: $tpl"
  elif [[ "$(jq -r '.counts.failed' <<<"$tpl")" == 0 && "$(jq -r '.templates | length' <<<"$tpl")" -gt 0 ]]; then
    pass "$name" "$(jq -r '.templates | length' <<<"$tpl") templates, $(jq -r '.counts.clean' <<<"$tpl") clean, $(jq -r '.counts.withWarnings' <<<"$tpl") with warnings"
  else
    fail "$name" "templates did not all convert: $(jq -c '.counts' <<<"$tpl")"
  fi

  if ! list=$(hs --json migrate containers 2>&1); then
    fail "7 the containers are listed" "hoserva migrate containers failed: $list"
    return
  fi
  mapfile -t names < <(jq -r '[.templates[] | select(.preselected)] | sort_by(.autostartPosition) | .[].file' <<<"$list")
  if ((NO_CAPTURE)); then
    if ((${#names[@]} != 0)); then
      fail "7 a scan without the capture pre-selects nothing (Q89)" "pre-selected: ${names[*]}"
    else
      pass "7 a scan without the capture pre-selects nothing (Q89)"
    fi
    mapfile -t names < <(jq -r '[.templates[] | select(.creatable and (.created | not))] | .[].file' <<<"$list" | sort)
  elif ((${#names[@]} == 0)); then
    fail "7 the autostart list pre-selects containers" "nothing is pre-selected"
    return
  fi
  for f in "${names[@]}"; do
    if [[ "$(jq -r --arg f "$f" '.templates[] | select(.file == $f) | .status' <<<"$list")" == warnings ]]; then
      acks+=("--acknowledge" "$f")
    fi
  done
  name="7 the stacks are created from the templates, stopped"
  if out=$(hsq migrate containers create --yes "${names[@]}" "${acks[@]}" 2>&1); then
    pass "$name" "${names[*]}"
  else
    fail "$name" "$(tail -n 3 <<<"$out" | tr '\n' ' ')"
    return
  fi

  for f in "${names[@]}"; do
    i=$((i + 1))
    stack=$(jq -r --arg f "$f" '.templates[] | select(.file == $f) | .stack' <<<"$list")
    name="7 container $stack starts, sees its data and is confirmed"
    if ! out=$(hsq migrate containers start "$stack" 2>&1); then
      fail "$name" "start: $(tail -n 3 <<<"$out" | tr '\n' ' ')"
      continue
    fi
    if ((i < ${#names[@]})); then
      second=$(jq -r --arg f "${names[$i]}" '.templates[] | select(.file == $f) | .stack' <<<"$list")
      if ((i == 1)); then
        name="7 a second container cannot start before the first is confirmed"
        if [[ -z "$second" ]]; then
          fail "$name" "the list names no stack for ${names[$i]}"
        elif refused=$(hsq migrate containers start "$second" 2>&1); then
          fail "$name" "starting $second while $stack was unconfirmed was accepted"
        elif [[ "$refused" == *"container_unconfirmed"* && "$refused" == *"$UNCONFIRMED_REFUSAL: $stack"* ]]; then
          pass "$name" "container_unconfirmed"
        else
          fail "$name" "starting $second while $stack was unconfirmed failed, but not with the unconfirmed-container refusal: $(tail -n 3 <<<"$refused" | tr '\n' ' ')"
        fi
        name="7 container $stack starts, sees its data and is confirmed"
      fi
    fi
    if ! container_data_check "$stack"; then
      fail "$name" "$CONTAINER_MSG"
      continue
    fi
    confirm_args=("$stack")
    [[ -z "$CONTAINER_ACCEPT" ]] || confirm_args+=(--accept-failed-check)
    if out=$(hsq migrate containers confirm "${confirm_args[@]}" 2>&1); then
      pass "$name" "$CONTAINER_MSG${CONTAINER_ACCEPT:+; the path whose data lived on the cache was accepted}"
    else
      fail "$name" "confirm: $(tail -n 3 <<<"$out" | tr '\n' ' ')"
    fi
  done
}

step_scrub() {
  local name="8 a scrub covers the array and reports no errors" job st bad="" pct
  # allBlocks: without it hoserva scrub passes snapraid -o 10 and skips every
  # block the initial sync wrote less than ten days ago, which is all of them
  api POST /parity/scrub '{"percent":100,"allBlocks":true}'
  job=$(jq -r '.id // empty' <<<"$API_BODY" 2>/dev/null || true)
  if [[ ! "$API_CODE" =~ ^2 || -z "$job" ]]; then
    fail "$name" "startScrub answered HTTP $API_CODE: $API_BODY"
    return
  fi
  if ! wait_job "$job" 900; then
    fail "$name" "the scrub job $job did not succeed: $(jq -c '{status, error}' <<<"$JOB_JSON" 2>/dev/null || echo "$JOB_JSON")"
    return
  fi
  # a scrub that finds data errors still ends as a succeeded job (snapraid exits
  # 1), so the job's status alone says nothing: read what snapraid recorded
  st=$(vm_ssh 'sudo snapraid -c /etc/snapraid.conf status 2>&1' || true)
  grep -q 'No error detected\.' <<<"$st" || bad+="snapraid status does not say 'No error detected.'; "
  if [[ "$st" =~ ([0-9]+)%\ of\ the\ array\ is\ not\ scrubbed ]]; then
    pct=${BASH_REMATCH[1]}
    if ((pct > 0)); then
      bad+="the scrub job succeeded but ${pct}% of the array is still not scrubbed after a scrub of every block; "
    fi
  fi
  if [[ -z "$bad" ]]; then
    pass "$name" "no error detected, nothing left unscrubbed"
  else
    fail "$name" "$bad"
  fi
}

# pick_fix_file prints "<sha256>\t<disk>\t<path>" of a file on a data disk of an
# array-only share: not hidden, not appdata (which snapraid.conf leaves out of
# parity), a plain name, once in the manifest and with no other file whose name
# differs only in case.
pick_fix_file() {
  local arrayonly=$1
  grep -v '^#' "$EXPECTED_HOST/manifest.sha256" | awk -F'\t' -v pools=",$POOL_SLOTS," -v arr=",$arrayonly," '
    { lp = tolower($6); cnt[lp]++; row[NR] = $0; n = NR }
    END {
      for (i = 1; i <= n; i++) {
        split(row[i], f, "\t"); split(f[6], p, "/")
        if (index(pools, "," f[5] ",") == 0 && cnt[tolower(f[6])] == 1 && f[2] > 0 && p[1] !~ /^\./ && p[1] != "appdata" && index(arr, "," p[1] ",") > 0 && f[6] ~ /^[A-Za-z0-9._\/-]+$/) { print f[1] "\t" f[5] "\t" f[6]; exit }
      }
    }'
}

step_fix() {
  local name="9 a deleted file is restored by hoserva fix, its checksum matches" arrayonly pick sha disk path job got out
  api GET /shares
  arrayonly=$(jq -r '[.shares[] | select(.cacheMode == "array-only") | .name] | join(",")' <<<"$API_BODY" 2>/dev/null || true)
  pick=$(pick_fix_file "$arrayonly") || pick=""
  if [[ -z "$pick" ]]; then
    fail "$name" "the fixture has no file on a data disk of an array-only share to delete"
    return
  fi
  IFS=$'\t' read -r sha disk path <<<"$pick"
  if ! vm_ssh "sudo rm -- $(shq "/mnt/user/$path")"; then
    fail "$name" "could not delete /mnt/user/$path"
    return
  fi
  if vm_ssh "test -e $(shq "/mnt/user/$path") -o -e $(shq "/mnt/$disk/$path")"; then
    fail "$name" "/mnt/user/$path is still there after the delete"
    return
  fi
  if ! out=$(hs --json fix --confirm 2>&1) || ! job=$(jq -r '.id // empty' <<<"$out") || [[ -z "$job" ]]; then
    fail "$name" "hoserva fix --confirm: ${out:-no output}"
    return
  fi
  if ! wait_job "$job" 900; then
    fail "$name" "the fix job $job did not succeed: $(jq -c '{status, error}' <<<"$JOB_JSON" 2>/dev/null || echo "$JOB_JSON")"
    return
  fi
  # the fix writes the file on its disk, under mergerfs, which keeps what it
  # looked up (a missing entry) for a second: the pool shows the file a moment
  # after the disk does
  if ! vm_ssh "test -f $(shq "/mnt/$disk/$path")"; then
    fail "$name" "the fix job succeeded, but $path is not back on its disk /mnt/$disk"
    return
  fi
  local waited=0
  while ((waited < 20)); do
    got=$(vm_ssh "sudo sha256sum $(shq "/mnt/user/$path")" 2>&1) && break
    sleep 1
    waited=$((waited + 1))
  done
  if [[ "${got%% *}" != "$sha" ]]; then
    fail "$name" "/mnt/user/$path after the fix: ${got:-nothing} (want $sha)"
    return
  fi
  pass "$name" "$path on $disk, sha256 $sha"
}

run_supported() {
  local name
  SCAN_OK=1
  push_guest_helper || {
    fail "4 scan" "could not copy the guest helper"
    return
  }
  step_scans
  if ((!SCAN_OK)); then
    not_reached "5 import and every later step" "a scan did not match the fixture's expected result"
    return
  fi
  step_import || {
    not_reached "5 every file present, 8 parity, 6, 7, 9" "the import failed"
    return
  }
  step_files
  step_verify || {
    not_reached "8 point of no return, 6, 7, 9" "the verify did not pass"
    return
  }
  if ! step_point_of_no_return; then
    # owners and shares read the same before the point of no return
    step_ownership_and_shares
    not_reached "7 appdata and containers, 8 scrub, 9 fix" "the point of no return or the initial sync failed"
    return
  fi
  step_ownership_and_shares
  step_appdata
  step_templates_and_containers
  step_scrub
  step_fix
}

run_variant() {
  VARIANT=$1
  LAYOUT=$2
  RUN_FAILED=0
  DIAGNOSTICS_TAKEN=0
  export VARIANT
  RUN_LABEL="$VARIANT"
  [[ "$LAYOUT" == separate ]] || RUN_LABEL="$VARIANT ($LAYOUT)"
  unraid_require_variant
  load_variant
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: === $RUN_LABEL: $((REFUSAL ? 1 : 0)) refusal variant ==="

  STEP_REASON=""
  if prepare_vm "$( [[ "$LAYOUT" == shared-nvme ]] && echo shared-nvme || echo separate )"; then
    pass "1 restore the fixture snapshot $VARIANT"
  else
    fail "1 restore the fixture snapshot $VARIANT" "$STEP_REASON"
    return
  fi
  if [[ "$LAYOUT" == shared-nvme ]]; then
    if ! detach_fixture_cache; then
      fail "1 shared NVMe layout" "$STEP_REASON"
      return
    fi
  elif ((!REFUSAL)); then
    if ! grow_cache_disk; then
      fail "1 grow the cache disk" "$STEP_REASON"
      return
    fi
  fi

  if check_fixture_disks; then
    pass "2 the fixture's disks are the VM's array disks"
  else
    fail "2 the fixture's disks are the VM's array disks" "$STEP_REASON"
    return
  fi

  if install_hoserva && onboard; then
    pass "3 hoserva installed and onboarded"
  else
    fail "3 hoserva installed and onboarded" "$STEP_REASON"
    return
  fi

  if ((REFUSAL)); then
    run_refusal
  else
    run_supported
  fi
}

# collect_run_diagnostics keeps the guest's hoservad journal and doctor output
# under MIGRATION_ARTIFACT_DIR while the VM is still there; the VM is destroyed
# straight after, so this is the only chance. A failure to collect is reported
# and never changes the run's result.
collect_run_diagnostics() {
  local dir="${MIGRATION_ARTIFACT_DIR:-}"
  [[ -n "$dir" && -n "${VARIANT:-}" ]] || return 0
  ((!DIAGNOSTICS_TAKEN)) || return 0
  DIAGNOSTICS_TAKEN=1
  vm_domain_running "$VM_DOMAIN" || return 0
  dir+="/$VARIANT"
  [[ "${LAYOUT:-separate}" == separate ]] || dir+="-$LAYOUT"
  timeout 300 "$script_dir/collect-diagnostics.sh" "$dir/diagnostics" ||
    echo "vm-migration-suite[$HOSERVA_LAB_ID]: collecting the guest's diagnostics failed" >&2
}

print_summary() {
  local i
  echo
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: step record"
  for i in "${!STEP_NAMES[@]}"; do
    printf '  %-90s %s\n' "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]}"
  done
  if [[ -n "${MIGRATION_ARTIFACT_DIR:-}" ]]; then
    mkdir -p -- "$MIGRATION_ARTIFACT_DIR"
    for i in "${!STEP_NAMES[@]}"; do
      printf '%s\t%s\n' "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]}"
    done >>"$MIGRATION_ARTIFACT_DIR/step-record.tsv"
  fi
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### Migration suite"
      echo
      echo "| Step | Result |"
      echo "|---|---|"
      for i in "${!STEP_NAMES[@]}"; do
        printf '| %s | %s |\n' "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]//|/\\|}"
      done
    } >>"$GITHUB_STEP_SUMMARY"
  fi
}

main() {
  local target=${1:-${VARIANT:-}} run variant layout found=0
  [[ -n "$target" ]] || die "set VARIANT=<variant> or VARIANT=all (make vm-migration-suite VARIANT=unraid-7x-xfs-single-parity)"
  command -v jq >/dev/null 2>&1 || die "jq is not installed"
  vm_require_id
  vm_assert_own_domain "$VM_DOMAIN"

  # shellcheck disable=SC2317 # run by the EXIT trap below
  cleanup() {
    local rc=$?
    if ((rc != 0)); then collect_run_diagnostics; fi
    if [[ "${HOSERVA_MIGRATION_KEEP_VM:-}" == 1 ]]; then
      echo "vm-migration-suite[$HOSERVA_LAB_ID]: HOSERVA_MIGRATION_KEEP_VM=1: the VM '$VM_DOMAIN' is left running; make vm-destroy removes it"
    elif ! "$script_dir/destroy-vm.sh"; then
      echo "vm-migration-suite[$HOSERVA_LAB_ID]: destroying the VM failed — run 'make vm-destroy HOSERVA_LAB_ID=$HOSERVA_LAB_ID'" >&2
      rc=1
    fi
    exit "$rc"
  }
  # A signal must reach cleanup as a non-zero status: bash enters the EXIT trap
  # with $?=0 after an INT or TERM, which would skip the diagnostics and let an
  # interrupted run exit 0.
  trap 'exit 130' INT
  trap 'exit 143' TERM
  trap cleanup EXIT

  if [[ -z "${DEB:-}" && -z "${TAG:-}" ]] && [[ -z "$(git -C "$VM_REPO_ROOT" tag --list 'v*' | head -n 1)" ]]; then
    TAG=v0.0.0-beta.1
    export TAG
  fi

  for run in "${MIGRATION_RUNS[@]}"; do
    read -r variant layout <<<"$run"
    if [[ "$target" == all ]]; then
      :
    elif [[ "$variant" != "$target" || "$layout" != "${LAYOUT:-separate}" ]]; then
      continue
    fi
    found=1
    if vm_domain_exists "$VM_DOMAIN" && [[ "$target" == all ]]; then "$script_dir/destroy-vm.sh"; fi
    run_variant "$variant" "$layout"
    if ((RUN_FAILED)); then collect_run_diagnostics; fi
    if [[ "$target" == all ]]; then "$script_dir/destroy-vm.sh"; fi
  done
  ((found)) || die "no run '$target' (layout '${LAYOUT:-separate}'): the runs are $(printf '%s, ' "${MIGRATION_RUNS[@]}")"

  print_summary
  if ((ANY_FAILED)); then
    echo "vm-migration-suite[$HOSERVA_LAB_ID]: FAIL — at least one step failed" >&2
    exit 1
  fi
  echo "vm-migration-suite[$HOSERVA_LAB_ID]: every step passed"
}

# sourced by migration-suite-check.sh, which tests the functions above without a VM
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
