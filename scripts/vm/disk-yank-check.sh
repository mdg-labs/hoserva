#!/usr/bin/env bash
# `make vm-suite` step 5 (issue #386, doc 02 §4 "Replacing a failed disk",
# doc 06 §4): now that `planDiskReplace`/`replaceDisk` (#288) and
# `hoserva disk replace` exist, this step stops being a permanent
# NOT-YET-IMPLEMENTED placeholder and actually yanks a real data disk from
# the running array setup (step 3, #258) built, replaces it through the
# real API, and proves SnapRAID reconstructs it byte-for-byte.
#
# The "yank" itself follows array-sequence-check.sh's and #372's own
# storage-target-boot-check.sh convention, not a live hot-unplug: shut the
# guest down, `virsh detach-disk --config`, boot it back up with the disk
# genuinely gone. The "fresh blank virtio disk" that replaces it is a
# brand-new qcow2 image attached at the *exact same* `<target dev>` label
# and PCI `<address slot>` the old one held — never a different slot, and
# never one of create-vm.sh's other spare array disks (disk3/4/5/cache).
# Reusing the same slot is what keeps the guest kernel's own virtio-blk
# probe-order device naming unchanged once the replacement is back in
# place (array-sequence-check.sh's own header comment records that
# removing a disk shifts every later device's letter down by one — the
# same reason this script never leaves the domain with a slot missing for
# longer than its own detach/replace window): run-l3-suite.sh's own
# ARRAY_DATA1_DEV/ARRAY_DATA2_DEV (captured once in array setup, step 3,
# and reused verbatim by step 6's own I/O throttle) stay valid for every
# step that runs after this one.
#
# Every write this script makes to prove reconstruction lands directly on
# the yanked disk's own mountpoint (discovered from a running getPool call
# below, never assumed to be `/mnt/disk1`) — not through the pool — so the
# files are guaranteed to be on the physical disk this script actually
# yanks, per doc 02 §1's mount layout (`/mnt/diskN/<share>` is what
# `/mnt/user/<share>` mergerfs-overlays as one of its RW branches).
# Verification afterward reads both the disk's own mountpoint (proving
# SnapRAID rebuilt it) and the pool path (proving mergerfs serves it), and
# a final `POST /parity/diff` (never a raw `snapraid diff` on the guest,
# per this round's own review note) must come back clean.
#
# The replacement's own <serial> is deliberately never the original
# disk1's own — confirmed necessary the hard way (this issue's own
# second round, nightly run 36236553908): reusing it left the
# replacement's *identity* matching what disk.StorageGate already
# expected for this slot while its filesystem did not, so the gate read
# the array whole and hoservad's own storage-target boot ordering
# (cmd/hoservad/storagetarget.go) tried to mount /mnt/disk1 by its old,
# now-nonexistent filesystem UUID — a mount that can never succeed,
# blocking hoservad's own startup long enough for systemd's Type=notify
# TimeoutStartSec (90s) to kill it first, in a permanent kill-and-restart
# loop. A distinct serial (matching a real disk swap, which is never the
# same physical unit) keeps the gate correctly reporting the original
# disk still missing — the same fast path the missing-disk boot already
# takes. That gap is real product behaviour outside this issue's own
# scope (cmd/hoservad/, not scripts/vm/) and is reported, not fixed,
# here — see this commit's own message.
#
# Failing loudly here must never cascade into later steps (the #372
# lesson run-l3-suite.sh's own header already names): trap
# restore_on_exit, below, dumps hoservad's own service status, boot
# journal and any stuck systemd jobs, then unconditionally falls back to
# reattaching the ORIGINAL disk1 image and confirms hoservad is active
# again — on every exit path, whatever state the replacement was left in
# and whether or not the replace itself ever completed.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

ADMIN_USERNAME="${ARRAY_ADMIN_USERNAME:?disk-yank-check.sh needs ARRAY_ADMIN_USERNAME (set by run-l3-suite.sh from its own array setup step)}"
ADMIN_PASSWORD="${ARRAY_ADMIN_PASSWORD:?disk-yank-check.sh needs ARRAY_ADMIN_PASSWORD}"

COOKIE_JAR="/tmp/hoserva-disk-yank-check-cookies.txt"
DISKYANK_SHARE="hoserval3diskyank"
FILE_COUNT=12
FILE_SIZE_BYTES=131072

array_login() {
  local result
  result="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}'" 2>/dev/null)"
  [[ "$result" == *"\"username\":\"$ADMIN_USERNAME\""* ]]
}

# wait_hoserva_active polls until the guest's own hoservad reports active,
# bounded by timeout_s — matching storage-target-boot-check.sh's own
# helper of the same name.
wait_hoserva_active() {
  local timeout_s=$1
  local deadline=$((SECONDS + timeout_s))
  while (( SECONDS < deadline )); do
    vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# shutdown_guest / boot_guest match storage-target-boot-check.sh's own
# pair exactly (same shutdown-then-reconfigure-then-boot shape this whole
# harness uses for a persistent disk-topology change).
shutdown_guest() {
  echo "disk-yank-check[$HOSERVA_LAB_ID]: shutting the guest down cleanly"
  virsh -c "$VM_CONNECT" shutdown "$VM_DOMAIN" >/dev/null
  local shutdown_deadline=$((SECONDS + 120))
  while vm_domain_running "$VM_DOMAIN"; do
    if (( SECONDS >= shutdown_deadline )); then
      echo "disk-yank-check[$HOSERVA_LAB_ID]: guest did not shut down within 120s, forcing it off" >&2
      virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
      break
    fi
    sleep 2
  done
}

boot_guest() {
  local label=$1 on_failure=${2:-die}
  echo "disk-yank-check[$HOSERVA_LAB_ID]: starting the guest back up ($label)"
  virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
  if ! vm_wait_tcp "$VM_SSH_PORT" 180; then
    [[ "$on_failure" == "warn" ]] && { echo "disk-yank-check[$HOSERVA_LAB_ID]: WARNING: guest did not open its forwarded SSH port within 180s of $label" >&2; return 1; }
    die "guest did not open its forwarded SSH port within 180s of $label"
  fi
  if ! vm_ssh_wait_ready 180; then
    [[ "$on_failure" == "warn" ]] && { echo "disk-yank-check[$HOSERVA_LAB_ID]: WARNING: could not SSH into the guest within 180s of $label" >&2; return 1; }
    die "could not SSH into the guest within 180s of $label"
  fi
}

# wait_job_terminal is run-l3-suite.sh's own helper, duplicated here since
# every *-check.sh step runs as its own process (never sourced by
# run-l3-suite.sh), the same reason every other check script that submits
# a job (none did, until this one) would have had to duplicate it too.
wait_job_terminal() {
  local job_id=$1 timeout_s=$2
  local deadline=$((SECONDS + timeout_s))
  job_status=""
  job_result=""
  while (( SECONDS < deadline )); do
    job_result="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/jobs/$job_id" 2>/dev/null)"
    if [[ "$job_result" =~ \"status\":\"([^\"]+)\" ]]; then
      job_status="${BASH_REMATCH[1]}"
    fi
    case "$job_status" in
      succeeded | failed | cancelled | interrupted) return 0 ;;
    esac
    sleep 2
  done
  return 0
}

STATUS=0

# PERSISTENT_DETACHED/RESTORED and the trap below are storage-target-boot-
# check.sh's own promise, generalized for "swap in a fresh disk" instead
# of "restore the same one": whatever fails, and whenever it fails, this
# script never leaves the domain's own persistent config with the disk1
# slot genuinely empty, or with a disk hoservad cannot bring up, because
# that would cascade into every step run-l3-suite.sh runs after this one
# (the #372 lesson its own header names). Unconditionally falling back to
# the ORIGINAL disk1 — never trying to keep whatever the replacement left
# behind — is deliberate, confirmed by this issue's own second round: a
# replacement already formatted (or attempted) by a failed or interrupted
# replaceDisk run is not a state any later step can rely on, but the
# original disk1, byte for byte from before this script ever touched it,
# always is.
PERSISTENT_DETACHED=0
RESTORED=0
DISK1_TARGET=""
DISK1_SLOT=""
DISK1_ORIGINAL_XML=""
DISK1_ORIGINAL_XML_FILE=""
DISK1_REPLACEMENT_XML_FILE=""
NEW_IMG=""

# dump_guest_diagnostics captures hoservad's own service state, its boot
# journal and any stuck systemd jobs before this script's own trap starts
# reverting anything — best-effort (a guest this is not reachable
# against has bigger problems this step already reports) so a nightly
# failure here is diagnosable from the log alone, never just "hoservad
# did not report active".
dump_guest_diagnostics() {
  if ! vm_domain_running "$VM_DOMAIN"; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: domain is not running — skipping guest diagnostics"
    return
  fi
  echo "disk-yank-check[$HOSERVA_LAB_ID]: === guest diagnostics ahead of restoring disk1 ==="
  echo "--- systemctl status hoserva --no-pager ---"
  vm_ssh 'sudo systemctl status hoserva --no-pager' \
    || echo "disk-yank-check[$HOSERVA_LAB_ID]: could not read systemctl status hoserva (guest unreachable?)" >&2
  echo "--- journalctl -u hoserva -b --no-pager (last 200 lines) ---"
  vm_ssh 'sudo journalctl -u hoserva -b --no-pager | tail -200' \
    || echo "disk-yank-check[$HOSERVA_LAB_ID]: could not read journalctl -u hoserva" >&2
  echo "--- systemctl list-jobs ---"
  vm_ssh 'sudo systemctl list-jobs' \
    || echo "disk-yank-check[$HOSERVA_LAB_ID]: could not read systemctl list-jobs" >&2
  echo "disk-yank-check[$HOSERVA_LAB_ID]: === end guest diagnostics ==="
}

# reattach_original_disk1 is this script's own last-resort recovery: with
# the guest shut down, detaches whatever currently sits at disk1's own
# target-dev label (the replacement, formatted or not — a harmless no-op
# if nothing is there) and reattaches the ORIGINAL disk1 image captured
# before this script ever touched it, then boots and confirms hoservad
# comes back active. Returns 1 (never dies — this already runs from
# inside the EXIT trap) if any of that does not hold.
reattach_original_disk1() {
  if vm_domain_running "$VM_DOMAIN"; then
    shutdown_guest
  fi
  virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$DISK1_TARGET" --config >/dev/null 2>&1 || true
  if ! virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$DISK1_ORIGINAL_XML_FILE" --config >/dev/null 2>&1; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: WARNING: reattaching the original disk1 failed — later suite steps may see a broken array until this is fixed by hand" >&2
    return 1
  fi
  if ! boot_guest "reattaching the original disk1 after a failure" warn; then
    return 1
  fi
  if ! wait_hoserva_active 120; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: WARNING: hoservad did not report active within 120s even after reattaching the original disk1 — later suite steps may see a broken array until this is fixed by hand" >&2
    return 1
  fi
  return 0
}

restore_on_exit() {
  local exit_status=$?
  if [[ "$PERSISTENT_DETACHED" -eq 1 && "$RESTORED" -eq 0 ]]; then
    dump_guest_diagnostics
    echo "disk-yank-check[$HOSERVA_LAB_ID]: restoring the domain's own disk1 slot before exiting (exit status $exit_status) — this script never leaves the domain with disk1 genuinely missing or an array hoservad cannot bring back up"
    if reattach_original_disk1; then
      echo "disk-yank-check[$HOSERVA_LAB_ID]: reattached the original disk1 — hoservad is active again"
    fi
    RESTORED=1
  fi
  rm -f -- "$DISK1_ORIGINAL_XML_FILE" "$DISK1_REPLACEMENT_XML_FILE" 2>/dev/null || true
  exit "$exit_status"
}
trap restore_on_exit EXIT

echo "disk-yank-check[$HOSERVA_LAB_ID]: discovering the 'disk1' array disk from the live domain XML"
DOMXML="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")" || die "could not read domain XML for '$VM_DOMAIN'"

dev="" serial="" slot="" buf=""
while IFS= read -r line; do
  case "$line" in
    *'<disk '*) dev=""; serial=""; slot=""; buf="" ;;
  esac
  buf+="$line"$'\n'
  if [[ "$line" == *'<target '* && "$line" =~ dev=\'([^\']*)\' ]]; then
    dev="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" =~ \<serial\>([^\<]*)\</serial\> ]]; then
    serial="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'<address '* && "$line" =~ slot=\'0x([0-9a-fA-F]+)\' ]]; then
    slot="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'</disk>'* ]]; then
    if [[ "$serial" == "disk1-hoserva-$HOSERVA_LAB_ID" ]]; then
      DISK1_TARGET="$dev"
      DISK1_SLOT="$slot"
      DISK1_ORIGINAL_XML="$buf"
    fi
    dev=""; serial=""; slot=""; buf=""
  fi
done <<<"$DOMXML"

[[ -n "$DISK1_TARGET" && -n "$DISK1_SLOT" && -n "$DISK1_ORIGINAL_XML" ]] || die "no 'disk1-hoserva-$HOSERVA_LAB_ID' array disk found in the domain XML — is this domain fresh from 'make vm-up'?"
echo "disk-yank-check[$HOSERVA_LAB_ID]: disk1 is $DISK1_TARGET at PCI slot 0x$DISK1_SLOT"

DISK1_ORIGINAL_XML_FILE="$VM_STATE_DIR/disk-yank-check-disk1-original.xml"
printf '%s' "$DISK1_ORIGINAL_XML" >"$DISK1_ORIGINAL_XML_FILE"

echo "disk-yank-check[$HOSERVA_LAB_ID]: logging in and confirming disk1's own array mountpoint from a live getPool call"
array_login || die "login as $ADMIN_USERNAME failed ahead of this check's own disk1 discovery"
POOL_BEFORE="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
[[ "$POOL_BEFORE" == *'"mounted":true'* ]] || die "getPool does not report mounted:true ahead of this check — array setup (step 3) or an earlier step should already have the pool mounted"

MOUNTPOINT=""
if [[ "$POOL_BEFORE" =~ \"device\":\"/dev/$DISK1_TARGET\",\"mountPoint\":\"([^\"]+)\",\"role\":\"([^\"]+)\",\"state\":\"([^\"]+)\" ]]; then
  MOUNTPOINT="${BASH_REMATCH[1]}"
  [[ "${BASH_REMATCH[2]}" == "data" ]] || die "getPool reports /dev/$DISK1_TARGET as role '${BASH_REMATCH[2]}', want 'data' — is this the right domain, fresh from array setup (step 3)?"
  [[ "${BASH_REMATCH[3]}" == "active" ]] || die "getPool reports /dev/$DISK1_TARGET as state '${BASH_REMATCH[3]}', want 'active' before this check yanks it"
else
  die "getPool has no data disk entry for /dev/$DISK1_TARGET — the domain's own libvirt target-dev label and the guest's own kernel device name have diverged, or array setup (step 3) did not include this disk: $POOL_BEFORE"
fi
echo "disk-yank-check[$HOSERVA_LAB_ID]: disk1 (/dev/$DISK1_TARGET) is mounted at $MOUNTPOINT"

echo "disk-yank-check[$HOSERVA_LAB_ID]: creating this check's own dedicated share '$DISKYANK_SHARE' (array-only — this array has no cache disk)"
SHARE_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/shares -H 'Content-Type: application/json' -d '{\"name\":\"$DISKYANK_SHARE\",\"cacheMode\":\"array-only\"}'" 2>/dev/null)"
[[ "$SHARE_RESULT" == *"\"name\":\"$DISKYANK_SHARE\""* ]] || die "createShare($DISKYANK_SHARE) did not return the expected share: $SHARE_RESULT"

echo "disk-yank-check[$HOSERVA_LAB_ID]: seeding $FILE_COUNT files directly onto disk1's own mountpoint ($MOUNTPOINT/$DISKYANK_SHARE) so they are guaranteed to be on the disk this check yanks"
SEED_SCRIPT_LOCAL="$(mktemp)"
cat >"$SEED_SCRIPT_LOCAL" <<'SEED'
#!/usr/bin/env bash
set -euo pipefail
share_dir=$1
file_count=$2
file_size=$3
mkdir -p "$share_dir"
cd "$share_dir"
for ((i = 1; i <= file_count; i++)); do
  head -c "$file_size" /dev/urandom >"file-$i.bin"
done
SEED
SEED_SCRIPT_REMOTE="/tmp/hoserva-disk-yank-seed.sh"
vm_scp "$SEED_SCRIPT_LOCAL" "hoserva@127.0.0.1:$SEED_SCRIPT_REMOTE"
rm -f -- "$SEED_SCRIPT_LOCAL"
vm_ssh "chmod +x $SEED_SCRIPT_REMOTE"
vm_ssh "sudo $SEED_SCRIPT_REMOTE '$MOUNTPOINT/$DISKYANK_SHARE' $FILE_COUNT $FILE_SIZE_BYTES" || die "seeding $FILE_COUNT files into $MOUNTPOINT/$DISKYANK_SHARE failed"

if ! ORIGINAL_MANIFEST="$(vm_ssh "sudo sh -c 'cd \"$MOUNTPOINT/$DISKYANK_SHARE\" && sha256sum file-*.bin | sort'" 2>/dev/null)"; then
  die "sha256sum of the seeded files at $MOUNTPOINT/$DISKYANK_SHARE failed"
fi
[[ -n "$ORIGINAL_MANIFEST" ]] || die "sha256sum of the seeded files at $MOUNTPOINT/$DISKYANK_SHARE returned nothing"

echo "disk-yank-check[$HOSERVA_LAB_ID]: running a baseline sync so parity covers the seeded files"
SYNC_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/sync -H 'Content-Type: application/json' -d '{\"confirm\":false,\"dryRun\":false}'" 2>/dev/null)"
[[ "$SYNC_RESULT" =~ \"id\":\"([^\"]+)\" ]] || die "startSync did not return a job id: $SYNC_RESULT"
SYNC_JOB_ID="${BASH_REMATCH[1]}"
wait_job_terminal "$SYNC_JOB_ID" 180
[[ "$job_status" == "succeeded" ]] || die "baseline sync (job $SYNC_JOB_ID) did not succeed (status=${job_status:-unknown}): $job_result"

echo "disk-yank-check[$HOSERVA_LAB_ID]: yanking disk1 — shutting the guest down and detaching $DISK1_TARGET from the domain's own persistent config"
shutdown_guest
virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$DISK1_TARGET" --config >/dev/null
PERSISTENT_DETACHED=1
boot_guest "the missing-disk boot"
wait_hoserva_active 120 || echo "disk-yank-check[$HOSERVA_LAB_ID]: hoservad did not report active within 120s of the missing-disk boot — continuing, since a missing data disk may keep it from reaching full readiness (doc 02 §1)" >&2

echo "disk-yank-check[$HOSERVA_LAB_ID]: confirming the array reports disk1 missing"
array_login || die "login as $ADMIN_USERNAME failed after the missing-disk boot"
POOL_MISSING="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
if [[ "$POOL_MISSING" =~ \"mountPoint\":\"$MOUNTPOINT\",\"role\":\"([^\"]+)\",\"state\":\"([^\"]+)\" ]]; then
  [[ "${BASH_REMATCH[2]}" == "missing" ]] || die "getPool reports $MOUNTPOINT as state '${BASH_REMATCH[2]}' after yanking its disk, want 'missing': $POOL_MISSING"
else
  die "getPool has no entry for $MOUNTPOINT after yanking its disk: $POOL_MISSING"
fi
echo "disk-yank-check[$HOSERVA_LAB_ID]: confirmed — $MOUNTPOINT reports state 'missing'"

echo "disk-yank-check[$HOSERVA_LAB_ID]: creating a fresh blank replacement image and attaching it at disk1's own slot (0x$DISK1_SLOT, target $DISK1_TARGET)"
OLD_IMG="$(printf '%s' "$DISK1_ORIGINAL_XML" | sed -n "s/.*<source file='\\([^']*\\)'.*/\\1/p")"
[[ -n "$OLD_IMG" ]] || die "could not find the original disk1 image path in its own captured XML"
SIZE_BYTES="$(qemu-img info "$OLD_IMG" | sed -n -E 's/.*\(([0-9]+) bytes\).*/\1/p' | head -n1)"
[[ -n "$SIZE_BYTES" ]] || die "could not read the original disk1 image's virtual size from 'qemu-img info $OLD_IMG'"
NEW_IMG="$VM_IMG_DIR/disk1-replacement.qcow2"
rm -f -- "$NEW_IMG"
qemu-img create -q -f qcow2 "$NEW_IMG" "$SIZE_BYTES"

DISK1_REPLACEMENT_XML_FILE="$VM_STATE_DIR/disk-yank-check-disk1-replacement.xml"
{
  echo "<disk type='file' device='disk'>"
  echo "  <driver name='qemu' type='qcow2'/>"
  echo "  <source file='$(vm_xml_attr_escape "$NEW_IMG")'/>"
  echo "  <target dev='$DISK1_TARGET' bus='virtio'/>"
  # A genuinely distinct serial, never the original disk1's own
  # ("disk1repl-", still matching every later step's own "-hoserva-
  # $HOSERVA_LAB_ID" and "disk*-hoserva-..." filters, e.g. array-sequence-
  # check.sh's) — matching a real disk swap, which is never the same
  # physical unit. Confirmed empirically (this issue's own second round,
  # nightly run 36236553908): reusing the original's own serial here
  # left the replacement's *identity* matching what disk.StorageGate
  # already expects for this slot while its filesystem does not,  so the
  # gate read the array whole and let hoservad's own storage-target boot
  # ordering (cmd/hoservad/storagetarget.go) attempt to mount /mnt/disk1
  # by its old, now-nonexistent filesystem UUID — a mount that can never
  # succeed, blocking hoservad's own startup ahead of its Type=notify
  # READY=1 long enough for systemd's own TimeoutStartSec (90s) to kill
  # it first, in a permanent kill-and-restart loop the daemon can never
  # get out of on its own. A distinct serial keeps the gate correctly
  # reporting the original disk still missing, exactly the fast,
  # already-proven path this script's own missing-disk boot (above)
  # takes — confirmed empirically to bring hoservad back within seconds,
  # never the 90s+ hang a shared identity causes. This is a real
  # cmd/hoservad/storagetarget.go gap outside this issue's own scope,
  # reported separately (see this commit's own message) rather than
  # fixed here.
  echo "  <serial>disk1repl-hoserva-$HOSERVA_LAB_ID</serial>"
  echo "  <address type='pci' domain='0x0000' bus='0x00' slot='0x$DISK1_SLOT' function='0x0'/>"
  echo "</disk>"
} >"$DISK1_REPLACEMENT_XML_FILE"

shutdown_guest
virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$DISK1_REPLACEMENT_XML_FILE" --config >/dev/null
boot_guest "the replacement-disk boot"
wait_hoserva_active 120 || die "hoservad did not report active within 120s of the replacement-disk boot"

echo "disk-yank-check[$HOSERVA_LAB_ID]: driving the real replace flow — planDiskReplace, then replaceDisk"
array_login || die "login as $ADMIN_USERNAME failed after the replacement-disk boot"

PLAN_BODY="{\"mountpoint\":\"$MOUNTPOINT\",\"device\":\"/dev/$DISK1_TARGET\",\"filesystem\":\"ext4\"}"
PLAN_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/disks/array/replace/plan -H 'Content-Type: application/json' -d '$PLAN_BODY'" 2>/dev/null)"
[[ "$PLAN_RESULT" == *"\"mountpoint\":\"$MOUNTPOINT\""* ]] || die "planDiskReplace did not return the expected plan for $MOUNTPOINT: $PLAN_RESULT"
[[ "$PLAN_RESULT" =~ \"confirmation\":\"([^\"]*)\" ]] || die "planDiskReplace's response has no confirmation phrase: $PLAN_RESULT"
CONFIRMATION="${BASH_REMATCH[1]}"
[[ -n "$CONFIRMATION" ]] || die "planDiskReplace returned an empty confirmation phrase: $PLAN_RESULT"
echo "disk-yank-check[$HOSERVA_LAB_ID]: plan confirmation: $CONFIRMATION"

REPLACE_BODY="{\"mountpoint\":\"$MOUNTPOINT\",\"device\":\"/dev/$DISK1_TARGET\",\"filesystem\":\"ext4\",\"confirmation\":\"$CONFIRMATION\"}"
REPLACE_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/disks/array/replace -H 'Content-Type: application/json' -d '$REPLACE_BODY'" 2>/dev/null)"
[[ "$REPLACE_RESULT" =~ \"id\":\"([^\"]+)\" ]] || die "replaceDisk did not return a job id: $REPLACE_RESULT"
REPLACE_JOB_ID="${BASH_REMATCH[1]}"

echo "disk-yank-check[$HOSERVA_LAB_ID]: waiting for the disk_replace job (formats the replacement, then runs snapraid fix)"
wait_job_terminal "$REPLACE_JOB_ID" 300
if [[ "$job_status" != "succeeded" ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: replaceDisk job $REPLACE_JOB_ID did not succeed (status=${job_status:-unknown}): $job_result" >&2
  STATUS=1
fi

if [[ "$STATUS" -eq 0 ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: confirming the array reports $MOUNTPOINT active again"
  POOL_AFTER="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
  if [[ "$POOL_AFTER" =~ \"mountPoint\":\"$MOUNTPOINT\",\"role\":\"([^\"]+)\",\"state\":\"([^\"]+)\" ]]; then
    if [[ "${BASH_REMATCH[2]}" != "active" ]]; then
      echo "disk-yank-check[$HOSERVA_LAB_ID]: getPool reports $MOUNTPOINT as state '${BASH_REMATCH[2]}' after replaceDisk succeeded, want 'active': $POOL_AFTER" >&2
      STATUS=1
    fi
  else
    echo "disk-yank-check[$HOSERVA_LAB_ID]: getPool has no entry for $MOUNTPOINT after replaceDisk succeeded: $POOL_AFTER" >&2
    STATUS=1
  fi
fi

if [[ "$STATUS" -eq 0 ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: verifying every seeded file is back on disk1's own mountpoint with its original checksum"
  if ! DISK_MANIFEST_AFTER="$(vm_ssh "sudo sh -c 'cd \"$MOUNTPOINT/$DISKYANK_SHARE\" && sha256sum file-*.bin | sort'" 2>/dev/null)"; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: sha256sum at $MOUNTPOINT/$DISKYANK_SHARE failed after replaceDisk — a seeded file is missing" >&2
    STATUS=1
  elif [[ "$DISK_MANIFEST_AFTER" != "$ORIGINAL_MANIFEST" ]]; then
    printf 'disk-yank-check[%s]: reconstructed checksums at %s do not match the originals\nwant:\n%s\ngot:\n%s\n' \
      "$HOSERVA_LAB_ID" "$MOUNTPOINT/$DISKYANK_SHARE" "$ORIGINAL_MANIFEST" "$DISK_MANIFEST_AFTER" >&2
    STATUS=1
  else
    echo "disk-yank-check[$HOSERVA_LAB_ID]: all $FILE_COUNT files reconstructed byte-for-byte on disk1's own mountpoint — confirmed"
  fi
fi

if [[ "$STATUS" -eq 0 ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: verifying the pool (/mnt/user/$DISKYANK_SHARE) serves the same reconstructed files"
  if ! POOL_MANIFEST_AFTER="$(vm_ssh "sudo sh -c 'cd \"/mnt/user/$DISKYANK_SHARE\" && sha256sum file-*.bin | sort'" 2>/dev/null)"; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: sha256sum at /mnt/user/$DISKYANK_SHARE failed after replaceDisk — the pool is not serving a reconstructed file" >&2
    STATUS=1
  elif [[ "$POOL_MANIFEST_AFTER" != "$ORIGINAL_MANIFEST" ]]; then
    printf 'disk-yank-check[%s]: checksums served through /mnt/user/%s do not match the originals\nwant:\n%s\ngot:\n%s\n' \
      "$HOSERVA_LAB_ID" "$DISKYANK_SHARE" "$ORIGINAL_MANIFEST" "$POOL_MANIFEST_AFTER" >&2
    STATUS=1
  else
    echo "disk-yank-check[$HOSERVA_LAB_ID]: the pool serves all $FILE_COUNT reconstructed files byte-for-byte — confirmed"
  fi
fi

if [[ "$STATUS" -eq 0 ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: running a following snapraid diff through the real API and confirming it is clean"
  DIFF_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/diff" 2>/dev/null)"
  # diffToAPIGroups (internal/api/parity_handler.go) always returns one
  # entry per diff category (removed, updated, added, moved, copied,
  # moved_by_hoserva) — "groups" is never itself empty, clean or not, so a
  # clean diff is every group's own "count" reading 0, never a bare
  # "groups":[] check. The category check guards against a malformed or
  # error response silently reading as clean because it happens to carry
  # no "count" field at all.
  NONZERO_COUNTS="$(printf '%s' "$DIFF_RESULT" | grep -oE '"count":-?[0-9]+' | grep -v -E ':0$' || true)"
  if [[ "$DIFF_RESULT" != *'"category":"removed"'* ]]; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: runParityDiff's response is missing the expected diff groups: $DIFF_RESULT" >&2
    STATUS=1
  elif [[ -n "$NONZERO_COUNTS" ]]; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: runParityDiff reports non-zero diff groups after reconstruction ($NONZERO_COUNTS), want every count at 0: $DIFF_RESULT" >&2
    STATUS=1
  elif [[ "$DIFF_RESULT" != *'"wouldBlock":false'* ]]; then
    echo "disk-yank-check[$HOSERVA_LAB_ID]: runParityDiff's guard reports wouldBlock:true after a clean reconstruction: $DIFF_RESULT" >&2
    STATUS=1
  else
    echo "disk-yank-check[$HOSERVA_LAB_ID]: snapraid diff is clean after reconstruction — confirmed"
  fi
fi

if [[ "$STATUS" -eq 0 ]]; then
  echo "disk-yank-check[$HOSERVA_LAB_ID]: disk1 was yanked, replaced through the real API, and every file reconstructed byte-for-byte with a clean diff — confirmed"
  # Only now, with the whole replace-and-verify sequence genuinely
  # successful, does the trap's own fallback stop applying: any earlier
  # exit — replaceDisk itself failing or timing out, or any verification
  # above failing — must still route through reattach_original_disk1,
  # exactly as this script's own header comment promises. Setting this
  # any earlier (previously done right after the replacement-disk boot,
  # before planDiskReplace/replaceDisk were even called) left the domain
  # with a blank or partially-formatted replacement disk whenever the
  # replace itself failed after that point — a real gap a verifier caught
  # (issue #386, this round).
  RESTORED=1
fi
exit "$STATUS"
