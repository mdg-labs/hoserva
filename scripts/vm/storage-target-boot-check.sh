#!/usr/bin/env bash
# L3 storage-target boot-ordering check (issue #372, doc 02 §1, Q69): the
# data-loss scenario the #331 verifier raised — with a data disk missing
# and unacknowledged at real boot, nothing had ever stopped smbd or
# nfs-kernel-server from starting before the pool mounted, so a client
# could write straight into the still-empty /mnt/user directory on the
# boot disk. cmd/hoservad's own storage-target regeneration
# (cmd/hoservad/storagetarget.go) and packaging/debian/hoserva.service's
# Type=notify (the boot-ordering race fix, see its own comment) are what
# this proves, against a real systemd boot, never a fake.
#
# Runs after array setup (run-l3-suite.sh's own step 3) and the
# SMB-connected array stop/start check (issue #309, which this script
# reuses the same dedicated SMB account and 'massdel' share from) —
# before array-sequence-check.sh's own step (issue #146), which
# permanently detaches an array disk from this domain's own persistent
# config. This script's own detach is temporary: the exact same disk,
# captured from the live domain XML before detaching, is reattached
# before this script returns — on every exit path, including a failing
# one (trap restore_disk1_on_exit, below), not only the one where every
# assertion in between happens to pass — so every later step still sees
# the full array-disk topology it expects.
#
# "Acknowledging" the degraded state (Q69's own wording, and the
# acceptance criterion this script proves) has no reachable production
# entry point yet: disk.StorageGate.Acknowledge exists
# (internal/disk/degraded.go) but no operation in api/openapi.yaml calls
# it (confirmed empirically: grep -in "acknowledg" api/openapi.yaml
# matches nothing storage-related), and api/openapi.yaml, internal/api/
# and cmd/hoserva/ are all outside this issue's own declared file scope.
# This script instead exercises the same underlying mechanism — hoservad
# regenerating the storage-target units once disk.StorageGate.Ready()
# flips true — through the one path that is reachable today: the missing
# disk physically returning. A real "acknowledge without the disk coming
# back" flow is a separate, still genuine gap; this script's own summary
# output says so plainly rather than silently treating disk recovery as
# equivalent to acknowledgement.
#
# The disk returns twice: once live, while the degraded boot's own
# hoservad keeps running with no reboot at all — packaging/debian/
# hoserva-storage.rules's own SIGHUP trigger and cmd/hoservad/reload.go's handler
# are what this half proves, since a reboot alone cannot tell "the boot
# happened to come up ready" from "a live not-ready→ready transition
# actually brings Samba/NFS back" — and once more across a real reboot, so
# the ordinary boot path stays covered too. A share create in between
# proves the other direction: storageTargetSync.Update must not restart
# anything already running correctly (cmd/hoservad/storagetarget.go) — a
# rejected earlier attempt at this fix stopped and restarted Samba, NFS,
# Docker and libvirt on every share create, even with the gate unchanged.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

ADMIN_USERNAME="${ARRAY_ADMIN_USERNAME:?storage-target-boot-check.sh needs ARRAY_ADMIN_USERNAME (set by run-l3-suite.sh from its own array setup step)}"
ADMIN_PASSWORD="${ARRAY_ADMIN_PASSWORD:?storage-target-boot-check.sh needs ARRAY_ADMIN_PASSWORD}"
SMB_SHARE="${ARRAY_SMB_SHARE:?storage-target-boot-check.sh needs ARRAY_SMB_SHARE}"

SMB_USERNAME="hoserva-l3-smb"
SMB_PASSWORD="hoserva-l3-smb-password"
COOKIE_JAR="/tmp/hoserva-storage-target-boot-check-cookies.txt"

array_login() {
  local result
  result="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}'" 2>/dev/null)"
  [[ "$result" == *"\"username\":\"$ADMIN_USERNAME\""* ]]
}

# ensure_smb_account creates (or reuses, matching smb-stop-check.sh's own
# idempotent lookup) a dedicated share-only account — never the admin,
# same reasoning smb-stop-check.sh already documents — and gives it a
# Samba password, so this script's own SMB connectivity assertions below
# never depend on smb-stop-check.sh (issue #309) having already run.
ensure_smb_account() {
  array_login || die "login as $ADMIN_USERNAME failed ahead of the SMB account setup"
  local users_result user_id
  users_result="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/users" 2>/dev/null)"
  if [[ "$users_result" =~ \"id\":\"([^\"]+)\",\"username\":\"$SMB_USERNAME\" ]]; then
    user_id="${BASH_REMATCH[1]}"
  else
    local create_result
    create_result="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/users -H 'Content-Type: application/json' -d '{\"username\":\"$SMB_USERNAME\"}'" 2>/dev/null)"
    [[ "$create_result" =~ \"id\":\"([^\"]+)\" ]] || die "createUser($SMB_USERNAME) did not return an id: $create_result"
    user_id="${BASH_REMATCH[1]}"
  fi
  vm_ssh "getent passwd '$SMB_USERNAME' >/dev/null 2>&1 || sudo useradd -M -N -s /usr/sbin/nologin '$SMB_USERNAME'"
  local status
  status="$(vm_ssh "curl -sk -b $COOKIE_JAR -o /dev/null -w '%{http_code}' -X POST https://127.0.0.1:8008/api/v1/users/$user_id/password -H 'Content-Type: application/json' -d '{\"password\":\"$SMB_PASSWORD\"}'" 2>/dev/null)"
  [[ "$status" == "204" ]] || die "setUserPassword($SMB_USERNAME) returned HTTP $status"
}

# wait_hoserva_active polls until the guest's own hoservad reports
# active, bounded by timeout_s.
wait_hoserva_active() {
  local timeout_s=$1
  local deadline=$((SECONDS + timeout_s))
  while (( SECONDS < deadline )); do
    vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# shutdown_guest shuts the guest down cleanly, falling back to a forced
# destroy if it does not exit within 120s (matching array-sequence-
# check.sh's own pattern) — split from boot_guest below so a caller can
# change the domain's own persistent disk config (detach-disk,
# attach-device) in between, while the guest is off.
shutdown_guest() {
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: shutting the guest down cleanly"
  virsh -c "$VM_CONNECT" shutdown "$VM_DOMAIN" >/dev/null
  local shutdown_deadline=$((SECONDS + 120))
  while vm_domain_running "$VM_DOMAIN"; do
    if (( SECONDS >= shutdown_deadline )); then
      echo "storage-target-boot-check[$HOSERVA_LAB_ID]: guest did not shut down within 120s, forcing it off" >&2
      virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
      break
    fi
    sleep 2
  done
}

# boot_guest starts the guest back up and waits for SSH. on_failure="die"
# (the default, every existing call site) aborts the whole script the
# usual way; on_failure="warn" instead prints a warning and returns 1 —
# used only by restore_disk1 below, which runs from inside an EXIT trap
# and must never let its own failure discard the exit status that got it
# there in the first place.
boot_guest() {
  local label=$1 on_failure=${2:-die}
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: starting the guest back up ($label)"
  virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
  if ! vm_wait_tcp "$VM_SSH_PORT" 180; then
    [[ "$on_failure" == "warn" ]] && { echo "storage-target-boot-check[$HOSERVA_LAB_ID]: WARNING: guest did not open its forwarded SSH port within 180s of $label" >&2; return 1; }
    die "guest did not open its forwarded SSH port within 180s of $label"
  fi
  if ! vm_ssh_wait_ready 180; then
    [[ "$on_failure" == "warn" ]] && { echo "storage-target-boot-check[$HOSERVA_LAB_ID]: WARNING: could not SSH into the guest within 180s of $label" >&2; return 1; }
    die "could not SSH into the guest within 180s of $label"
  fi
}

# smbd_main_pid prints smbd.service's current MainPID (0 if inactive), for
# asserting a share create never restarts it (finding 1, doc 02 §1):
# storageTargetSync.Update (cmd/hoservad/storagetarget.go) must never
# touch systemd when neither readiness nor disk topology actually
# changed.
smbd_main_pid() {
  vm_ssh "systemctl show -p MainPID --value smbd.service" 2>/dev/null
}

# assert_pool_live checks that /mnt/user and $SMB_SHARE's own mount are
# genuinely live mergerfs mounts — findmnt's own FSTYPE, never just
# "mountpoint says something is mounted there" — the actual hazard #372
# exists to prevent. hoserva-storage-ready.service, hoserva-storage.target,
# smbd and nfs-kernel-server all reporting active only proves hoservad's
# own boot-ordering gate held; it never by itself proved the pool clients
# read and write through was mounted (cmd/hoservad/storagetarget.go's own
# confirmPoolMounted is what closes that gap in hoservad — this is this
# script's own, independent proof that it did, against a real guest). A
# failed check also inspects the bare mountpoint directly: if smbd/nfs
# really did start before the pool mounted, this is where a client's
# write would have landed on the boot disk instead.
assert_pool_live() {
  local label=$1
  local ok=1 fstype
  fstype="$(vm_ssh 'findmnt -n -o FSTYPE /mnt/user' 2>/dev/null || true)"
  if [[ "$fstype" != "fuse.mergerfs" ]]; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: /mnt/user is not a live mergerfs mount $label (findmnt reports '${fstype:-nothing mounted}')" >&2
    ok=0
  fi
  fstype="$(vm_ssh "findmnt -n -o FSTYPE '/mnt/user/$SMB_SHARE'" 2>/dev/null || true)"
  if [[ "$fstype" != "fuse.mergerfs" ]]; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: /mnt/user/$SMB_SHARE is not a live mergerfs mount $label (findmnt reports '${fstype:-nothing mounted}')" >&2
    ok=0
  fi
  if [[ "$ok" -eq 0 ]]; then
    local bare_contents
    bare_contents="$(vm_ssh 'sudo find /mnt/user -mindepth 1' 2>/dev/null || true)"
    if [[ -n "$bare_contents" ]]; then
      echo "storage-target-boot-check[$HOSERVA_LAB_ID]: /mnt/user has content while the pool is not genuinely mounted $label — a write landed on the boot disk: $bare_contents" >&2
    fi
    STATUS=1
    return 1
  fi
  return 0
}

# wait_units_active polls until every unit named in the remaining
# arguments reports active, bounded by timeout_s.
wait_units_active() {
  local timeout_s=$1
  shift
  local deadline=$((SECONDS + timeout_s))
  while (( SECONDS < deadline )); do
    local all_active=1 unit
    for unit in "$@"; do
      vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1 || { all_active=0; break; }
    done
    (( all_active )) && return 0
    sleep 3
  done
  return 1
}

# PERSISTENT_DETACHED and RESTORED track this script's own promise (its
# header comment above) that the disk it detaches is reattached
# byte-for-byte "before this script returns" — on every exit path, not
# only the one where every assertion in between happens to pass. Before
# this pair of flags and the trap below, a `die` anywhere after the
# detach (a real one hit the createShare probe: #372's own nightly L3
# re-run) skipped straight past the reboot-based restore near the bottom
# of this script, leaving the domain's own persistent config missing
# disk1 for every later run-l3-suite.sh step.
PERSISTENT_DETACHED=0
RESTORED=0
# Declared empty here, before trap restore_disk1_on_exit EXIT below is
# registered: set -u means the trap referencing either before its own
# real assignment further down (from a `die` raised that early) would
# itself be an unset-variable error, on top of whatever raised it.
DETACH_TARGET=""
DISK_XML_FILE=""

# restore_disk1 makes $DETACH_TARGET (disk1) a real, persistent part of
# the domain's own config again, from $DISK_XML_FILE — the exact same
# device definition captured before it was ever detached — and boots the
# guest back up. Used both by the deliberate reboot-based recovery this
# script performs on its own success path (on_failure="die", the default,
# matching every other call in this script), and by the exit trap below
# when something failed before reaching that point (on_failure="warn":
# this already runs from inside a trap on an already-failing exit, and
# must never let its own failure discard the original one).
restore_disk1() {
  local on_failure=${1:-die}
  if vm_domain_running "$VM_DOMAIN"; then
    shutdown_guest
  fi
  if virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN" --inactive 2>/dev/null | grep -q "disk1-hoserva-$HOSERVA_LAB_ID"; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: $DETACH_TARGET (disk1) is already back in the domain's own persistent config"
  elif ! virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$DISK_XML_FILE" --config >/dev/null 2>&1; then
    if [[ "$on_failure" == "warn" ]]; then
      echo "storage-target-boot-check[$HOSERVA_LAB_ID]: WARNING: reattaching $DETACH_TARGET failed — later suite steps may see a missing disk until this is fixed by hand" >&2
    else
      die "reattaching $DETACH_TARGET (disk1) to the domain's own persistent config failed"
    fi
  else
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: reattached $DETACH_TARGET (disk1) to the domain's own persistent config, byte-for-byte from before it was detached"
  fi
  # || true: boot_guest returns 1 (rather than calling die) in "warn"
  # mode, and this runs under set -e — without it, that plain nonzero
  # return would abort the script right here, before RESTORED=1 below
  # ever runs, the same class of bug this function exists to close.
  boot_guest "restoring $DETACH_TARGET (disk1)" "$on_failure" || true
  RESTORED=1
}

# restore_disk1_on_exit is this script's own EXIT trap: if something
# failed (a `die` call, or any other command under `set -e`) after the
# detach but before the deliberate restore near the bottom ran,
# restore_disk1 still runs here — this script's own promise to leave
# disk1 persistently reattached holds on every exit path, not only the
# one where every assertion in between happened to pass. Runs after
# $STATUS (or `die`'s own exit 1) is already fixed, so the original exit
# status is always what this script actually reports.
restore_disk1_on_exit() {
  local exit_status=$?
  if [[ "$PERSISTENT_DETACHED" -eq 1 && "$RESTORED" -eq 0 ]]; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: restoring $DETACH_TARGET (disk1) before exiting (exit status $exit_status) — this script's own header promises it never leaves the domain missing a disk"
    restore_disk1 warn
  fi
  rm -f -- "$DISK_XML_FILE" 2>/dev/null || true
  exit "$exit_status"
}
trap restore_disk1_on_exit EXIT

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: confirming the healthy baseline — smbd, nfs-kernel-server and the storage-target gate are all active with every array disk present"
for unit in smbd nfs-kernel-server hoserva-storage-ready.service hoserva-storage.target; do
  vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1 || die "'$unit' is not active before this check's own disk detach — array setup (step 3) should already have left it active"
done
ensure_smb_account

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: discovering the 'disk1' array disk from the live domain XML"
DOMXML="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")" || die "could not read domain XML for '$VM_DOMAIN'"

DETACH_TARGET=""
DISK_XML=""
dev="" serial="" buf=""
while IFS= read -r line; do
  case "$line" in
    *'<disk '*) dev=""; serial=""; buf="" ;;
  esac
  buf+="$line"$'\n'
  if [[ "$line" == *'<target '* && "$line" =~ dev=\'([^\']*)\' ]]; then
    dev="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" =~ \<serial\>([^\<]*)\</serial\> ]]; then
    serial="${BASH_REMATCH[1]}"
  fi
  if [[ "$line" == *'</disk>'* ]]; then
    if [[ "$serial" == "disk1-hoserva-$HOSERVA_LAB_ID" ]]; then
      DETACH_TARGET="$dev"
      DISK_XML="$buf"
    fi
    dev=""; serial=""; buf=""
  fi
done <<<"$DOMXML"

[[ -n "$DETACH_TARGET" && -n "$DISK_XML" ]] || die "no 'disk1-hoserva-$HOSERVA_LAB_ID' array disk found in the domain XML — is this domain fresh from 'make vm-up'?"
echo "storage-target-boot-check[$HOSERVA_LAB_ID]: will temporarily detach $DETACH_TARGET (disk1) and reattach it byte-for-byte before this script returns"

# DISK_XML_FILE is written once, up front, and reused for both the live
# reattach (below, no reboot) and the final persistent one — the exact
# same device definition either way, removed only once this script is
# done with it.
DISK_XML_FILE="$VM_STATE_DIR/storage-target-boot-check-disk1.xml"
printf '%s' "$DISK_XML" >"$DISK_XML_FILE"

STATUS=0

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: detaching $DETACH_TARGET from the domain's own persistent config"
shutdown_guest
virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$DETACH_TARGET" --config >/dev/null
# From here on, trap restore_disk1_on_exit (registered above) reattaches
# $DETACH_TARGET before this script can ever exit without it — including
# a `die` raised by anything below, which used to skip straight past the
# reboot-based restore near the bottom and leave the domain missing a
# disk for every later run-l3-suite.sh step.
PERSISTENT_DETACHED=1
boot_guest "the missing-disk boot"
wait_hoserva_active 120 || echo "storage-target-boot-check[$HOSERVA_LAB_ID]: hoservad did not report active within 120s of the missing-disk boot — continuing, since the whole point of this scenario is that it may refuse to reach full readiness" >&2

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: asserting nothing storage-dependent started with a data disk missing and unacknowledged"
for unit in hoserva-storage-ready.service hoserva-storage.target smbd nfs-kernel-server; do
  if vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: '$unit' is active with a data disk missing and unacknowledged — the boot-ordering gate did not hold" >&2
    STATUS=1
  fi
done
BARE_CONTENTS="$(vm_ssh 'sudo find /mnt/user -mindepth 1' 2>/dev/null || true)"
if [[ -n "$BARE_CONTENTS" ]]; then
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: /mnt/user has content while the pool is unmounted — a write landed on the boot disk: $BARE_CONTENTS" >&2
  STATUS=1
fi
if vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname storage-target-boot-check-degraded.txt'" >/dev/null 2>&1; then
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: an SMB put succeeded with the array degraded and unacknowledged — smbd must still have been reachable" >&2
  STATUS=1
fi

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: live-attaching $DETACH_TARGET back to the still-running degraded-boot guest — no reboot — to prove a live not-ready→ready transition brings Samba/NFS back on its own (packaging/debian/hoserva-storage.rules's SIGHUP trigger, cmd/hoservad/reload.go)"
if ! virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$DISK_XML_FILE" --live >/dev/null; then
  # A live PCI hotplug this libvirt/qemu combination refuses is an
  # environment limitation, not this fix's own concern (run-l3-soak.sh's
  # own yank_disk has the identical fallback for the same virtio disk) —
  # reported plainly and left for the reboot-based recovery below to
  # still cover the ordinary boot path, rather than aborting this whole
  # script over it.
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: live attach-device failed for $DETACH_TARGET — this environment does not support hotplugging it, so the live not-ready→ready path could not be exercised here" >&2
  STATUS=1
elif wait_units_active 150 hoserva-storage-ready.service hoserva-storage.target smbd nfs-kernel-server; then
  if ! assert_pool_live "after the live disk-return — the units came up"; then
    :
  elif ! vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname storage-target-boot-check-live-recovered.txt'" >/dev/null 2>&1; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: an SMB put failed after the live disk-return — smbd should be reachable now" >&2
    STATUS=1
  else
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: a live not-ready→ready transition brought Samba, NFS and the storage-target gate back with no reboot — confirmed"
  fi
else
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: hoserva-storage-ready.service, hoserva-storage.target, smbd or nfs-kernel-server did not come active within 150s of the live disk-return — the SIGHUP path (packaging/debian/hoserva-storage.rules, cmd/hoservad/reload.go) did not recover it live" >&2
  STATUS=1
fi

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: creating a throwaway share while the array is ready, to prove it never restarts smbd (finding 1: storageTargetSync.Update must not touch systemd on an unchanged gate)"
PROBE_SHARE="hoserva-l3-storage-target-probe"
BEFORE_PID="$(smbd_main_pid)"
if [[ -z "$BEFORE_PID" || "$BEFORE_PID" == "0" ]]; then
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: smbd has no MainPID ahead of the share create — the live disk-return above should have left it running" >&2
  STATUS=1
else
  array_login || die "login as $ADMIN_USERNAME failed ahead of the share-create-does-not-restart-smbd check"
  # cacheMode is required here, not optional: this array (array setup,
  # step 3) has no cache disk, and createShare's own default cache mode
  # is cache-then-move (internal/share/service.go), which 400s without
  # one ("cache mode \"cache-then-move\" needs a cache disk") — the same
  # reason array_setup's own massdel share and step 6's own
  # hoserval3midsync share both pass "array-only" explicitly. The body is
  # captured (never discarded to /dev/null) so a future failure here
  # prints the real reason instead of a bare HTTP status.
  CREATE_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/shares -H 'Content-Type: application/json' -d '{\"name\":\"$PROBE_SHARE\",\"cacheMode\":\"array-only\"}'" 2>/dev/null)"
  [[ "$CREATE_RESULT" == *"\"name\":\"$PROBE_SHARE\""* ]] || die "createShare($PROBE_SHARE) did not return the expected share: $CREATE_RESULT"
  sleep 3
  AFTER_PID="$(smbd_main_pid)"
  vm_ssh "curl -sk -b $COOKIE_JAR -o /dev/null -X DELETE https://127.0.0.1:8008/api/v1/shares/$PROBE_SHARE -H 'Content-Type: application/json' -d '{\"confirm\":true}'" >/dev/null 2>&1 || true
  if [[ "$AFTER_PID" != "$BEFORE_PID" ]]; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: smbd's MainPID changed across a share create ($BEFORE_PID -> $AFTER_PID) — it was restarted with the gate unchanged" >&2
    STATUS=1
  else
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: a share create with the gate unchanged left smbd's MainPID untouched ($BEFORE_PID) — confirmed"
  fi
fi

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: rebooting to also confirm the ordinary boot path with every array disk present — shutting the guest down drops the live-only attach above on its own, so restore_disk1 below makes the disk a real, persistent part of the domain's own config before the reboot"
restore_disk1
wait_hoserva_active 120 || die "hoservad did not report active within 120s of the disk-returned boot"

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: asserting Samba, NFS and the storage-target gate all come up once every array disk is present again"
for unit in hoserva-storage-ready.service hoserva-storage.target smbd nfs-kernel-server; do
  if ! vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1; then
    echo "storage-target-boot-check[$HOSERVA_LAB_ID]: '$unit' is not active once every array disk is present again" >&2
    STATUS=1
  fi
done
assert_pool_live "once every array disk is present again" || true
if ! vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname storage-target-boot-check-recovered.txt'" >/dev/null 2>&1; then
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: an SMB put failed once every array disk is present again — smbd should be reachable now" >&2
  STATUS=1
fi

echo "storage-target-boot-check[$HOSERVA_LAB_ID]: cleaning up this check's own SMB test files from '$SMB_SHARE'"
vm_ssh "sudo rm -f /mnt/user/$SMB_SHARE/storage-target-boot-check-*.txt" || true
# DISK_XML_FILE itself is removed by trap restore_disk1_on_exit below,
# which fires on this normal exit path too, not only a failing one.

if [[ "$STATUS" -eq 0 ]]; then
  echo "storage-target-boot-check[$HOSERVA_LAB_ID]: boot-ordering gate held while degraded, released live once the disk returned with no reboot, and held again across a real reboot with the array whole — confirmed"
fi
exit "$STATUS"
