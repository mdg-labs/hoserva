#!/usr/bin/env bash
# L3 maintenance-gate check (#387, doc 02 §1/§4, Q70): the acceptance
# criterion storage-target-boot-check.sh (issue #372) cannot prove —
# TestStorageTargetSync_Close_* (cmd/hoservad/storagetarget_test.go) only
# assert the argv a FakeRunner recorded, never that systemd itself refuses
# a dependent start once the gate is closed. `array stop` must remove
# hoserva-storage-ready.service's own runtime flag and stop the unit, so
# its fixed `test -e` ExecStart reruns — and fails — the next time
# anything needs hoserva-storage.target; before this issue, the unit
# stayed "active (exited)" (RemainAfterExit=yes) once it had ever
# succeeded, so an unattended-upgrades restart of smbd or
# nfs-kernel-server during maintenance still passed the target and served
# the unmounted pool straight off the boot disk.
#
# Runs after array setup (run-l3-suite.sh's own step 3) — its own admin
# account and 'massdel' share, taken from the environment
# (ARRAY_ADMIN_USERNAME/ARRAY_ADMIN_PASSWORD/ARRAY_SMB_SHARE) rather than
# a second, possibly-diverging copy of run-l3-suite.sh's own constants,
# are the only real prerequisite: this script creates its own SMB
# account below (ensure_smb_account) exactly like storage-target-boot-
# check.sh does, so it never actually depends on that check (issue #372)
# or smb-stop-check.sh (issue #309) having run first, whichever of them
# also happen to be selected (issue #391's own selectable-step audit
# confirmed this by reading both scripts, not assumed). Leaves the array
# started again before returning — on every exit path, including a
# failing one — so every later run-l3-suite.sh step still finds it
# running.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: ensuring smbclient is present on the guest"
vm_ssh 'command -v smbclient >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq smbclient)'

ADMIN_USERNAME="${ARRAY_ADMIN_USERNAME:?maintenance-gate-check.sh needs ARRAY_ADMIN_USERNAME (set by run-l3-suite.sh from its own array setup step)}"
ADMIN_PASSWORD="${ARRAY_ADMIN_PASSWORD:?maintenance-gate-check.sh needs ARRAY_ADMIN_PASSWORD}"
SMB_SHARE="${ARRAY_SMB_SHARE:?maintenance-gate-check.sh needs ARRAY_SMB_SHARE}"

# SMB_USERNAME/SMB_PASSWORD match smb-stop-check.sh's own dedicated
# share-only account (issue #309): setUserPassword refuses to touch the
# sole admin account (cannot_modify_admin), so an SMB client needs its
# own account regardless of which admin ran this check. This script's
# own run-l3-suite.sh step runs after smb-stop-check.sh and storage-
# target-boot-check.sh, both of which already create it — ensure_smb_account
# below reuses it exactly like they do — but this script also works
# standalone, against a guest neither of those has touched yet.
SMB_USERNAME="hoserva-l3-smb"
SMB_PASSWORD="hoserva-l3-smb-password"
COOKIE_JAR="/tmp/hoserva-maintenance-gate-check-cookies.txt"

array_login() {
  local result
  result="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}'" 2>/dev/null)"
  [[ "$result" == *"\"username\":\"$ADMIN_USERNAME\""* ]]
}

# ensure_smb_account creates (or reuses) SMB_USERNAME, the same idempotent
# lookup smb-stop-check.sh and storage-target-boot-check.sh already use,
# sets its Samba password, and grants it read-write access to SMB_SHARE
# through updateUserSharePermissions (a PUT replacing only this one
# account's own grants, so repeating it is harmless). smb.conf enforces
# per-user grants (internal/config/samba.go): without the grant the share
# renders `available = no` and every smbclient connection below fails
# with NT_STATUS_BAD_NETWORK_NAME.
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
  local grant_status
  grant_status="$(vm_ssh "curl -sk -b $COOKIE_JAR -o /dev/null -w '%{http_code}' -X PUT https://127.0.0.1:8008/api/v1/users/$user_id/permissions -H 'Content-Type: application/json' -d '{\"permissions\":[{\"shareName\":\"$SMB_SHARE\",\"access\":\"read-write\"}]}'" 2>/dev/null)"
  [[ "$grant_status" == "200" ]] || die "updateUserSharePermissions($SMB_USERNAME on $SMB_SHARE) returned HTTP $grant_status"
}

# assert_nothing_serves_pool checks that /mnt/user is not a live
# mergerfs mount — findmnt's own FSTYPE, the same real hazard storage-
# target-boot-check.sh's own assert_pool_live checks for — and that a
# client cannot actually reach the share over SMB. The two checks are
# reported and evaluated independently, never conflated into a single
# contradictory message: a mounted /mnt/user is reported as exactly
# that, and the bare-boot-disk inspection below only ever runs once an
# SMB write is known to have actually gone through while /mnt/user was
# not mounted — the one combination that means a client's write truly
# landed on the boot disk underneath the bare mountpoint, never on the
# mountpoint directory's own pre-existing empty scaffold (mkdir'd ahead
# of every mount attempt, present whether or not anything is ever
# mounted there, and never itself evidence of a write).
assert_nothing_serves_pool() {
  local label=$1
  local ok=1 fstype put_ok=1

  fstype="$(vm_ssh 'findmnt -n -o FSTYPE /mnt/user' 2>/dev/null || true)"
  if [[ "$fstype" == "fuse.mergerfs" ]]; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: /mnt/user is a live mergerfs mount $label — the pool is mounted while the array should be stopped" >&2
    ok=0
  fi

  if vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname maintenance-gate-check-probe.txt'" >/dev/null 2>&1; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: an SMB put succeeded $label — smbd must still have been reachable and serving the share" >&2
    ok=0
    put_ok=0
  fi

  if [[ "$put_ok" -eq 0 && "$fstype" != "fuse.mergerfs" ]]; then
    local written
    written="$(vm_ssh 'sudo find /mnt/user -mindepth 1 -type f' 2>/dev/null || true)"
    if [[ -n "$written" ]]; then
      echo "maintenance-gate-check[$HOSERVA_LAB_ID]: a write through smbd landed on the boot disk underneath the unmounted pool $label: $written" >&2
      # Remove it now: array/start later mounts mergerfs over this
      # directory, and the final cleanup can no longer reach it.
      vm_ssh "sudo find /mnt/user -mindepth 1 -type f -name 'maintenance-gate-check-*.txt' -delete" || true
    fi
  fi

  [[ "$ok" -eq 1 ]]
}

# assert_no_pool_mount_active checks that no Hoserva-generated mount
# unit — the catch-all, any per-share mount, or a physical data/parity/
# cache disk — is active. #387 (L3 nightly run 36250289714, then
# 36258823325): a physical disk mount unit and the catch-all were both
# found remounted after a refused dependent-service start during
# maintenance, confirmed via journalctl and systemctl show — nfs-utils'
# own systemd integration derives a RequiresMountsFor= directly on
# nfs-server.service for every NFS-exported path, an edge entirely
# outside anything Hoserva itself writes, and that reached the exported
# share's own mount unit and, through its own RequiresMountsFor=, the
# catch-all and the disks under it.
#
# A plain `mnt-*` glob over active mount units is too broad: a real
# /etc/fstab entry the
# suite's own "existing host config" fixture (Q76) creates at
# /mnt/hoserva-existing is a genuine, unrelated system mount whose own
# systemd-generated unit name also happens to start with `mnt-`. Every
# unit this check must care about instead is derived from the generated
# unit *files* themselves — each one carries the same "Generated by
# Hoserva" header every managed config file does (D4, doc 01 §2) — never
# a name pattern, so a real system mount that merely happens to sit under
# /mnt can never be mistaken for one of Hoserva's own.
assert_no_pool_mount_active() {
  local label=$1
  local active
  active="$(vm_ssh "for u in \$(systemctl list-units --type=mount --state=active --no-legend --plain | awk '{print \$1}'); do sudo grep -qF 'Generated by Hoserva' \"/etc/systemd/system/\$u\" 2>/dev/null && echo \"\$u\"; done")"
  if [[ -n "$active" ]]; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: Hoserva-generated mount unit(s) active $label, while the array should be stopped: $active" >&2
    return 1
  fi
  return 0
}

# STARTED_AGAIN tracks whether this script's own restore (array/start)
# already ran, for the EXIT trap below: a `die` anywhere after the stop
# but before the deliberate restore near the bottom must still bring the
# array back, on every exit path, not only the one where every assertion
# in between happens to pass — every later run-l3-suite.sh step needs it
# running.
STARTED_AGAIN=0

restore_array_on_exit() {
  local exit_status=$?
  if [[ "$STARTED_AGAIN" -eq 0 ]]; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: restarting the array before exiting (exit status $exit_status) — this script's own header promises every later step still finds it running"
    array_login || true
    vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" >/dev/null 2>&1 || true
  fi
  exit "$exit_status"
}
trap restore_array_on_exit EXIT

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: confirming the healthy baseline — smbd, nfs-kernel-server and the storage-target gate are all active before this check's own array stop"
for unit in smbd nfs-kernel-server hoserva-storage-ready.service hoserva-storage.target; do
  vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1 || die "'$unit' is not active before this check's own array stop — array setup (step 3) should already have left it active"
done

ensure_smb_account

array_login || die "login as $ADMIN_USERNAME failed ahead of array/stop"

STATUS=0

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: running \`hoserva array stop\`"
STOP_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/stop -H 'Content-Type: application/json' -d '{\"confirm\":true}'" 2>/dev/null)"
if [[ "$STOP_RESULT" != *'"maintenanceMode":true'* ]]; then
  die "array/stop did not report maintenanceMode:true: $STOP_RESULT"
fi

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: asserting the storage-target gate closed — the readiness flag is gone and hoserva-storage-ready.service is no longer active"
if vm_ssh "sudo systemctl is-active hoserva-storage-ready.service" >/dev/null 2>&1; then
  echo "maintenance-gate-check[$HOSERVA_LAB_ID]: hoserva-storage-ready.service is still active after array/stop — RemainAfterExit=yes left it 'active (exited)' instead of being stopped" >&2
  STATUS=1
fi
if vm_ssh "test -e /run/hoserva/storage-ready" >/dev/null 2>&1; then
  echo "maintenance-gate-check[$HOSERVA_LAB_ID]: the storage-ready runtime flag still exists after array/stop" >&2
  STATUS=1
fi
assert_nothing_serves_pool "immediately after array/stop" || STATUS=1
assert_no_pool_mount_active "immediately after array/stop" || STATUS=1

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: attempting to start smbd.service and nfs-kernel-server directly while the array is stopped — this is the acceptance criterion itself: a dependent start during maintenance must fail on hoserva-storage.target, never actually start"
for unit in smbd.service nfs-kernel-server; do
  if vm_ssh "sudo systemctl start '$unit'" >/dev/null 2>&1; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: 'systemctl start $unit' succeeded while the array is in maintenance mode — the storage-target gate did not hold" >&2
    STATUS=1
  else
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: 'systemctl start $unit' refused, as expected"
  fi
  if vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: '$unit' reports active after its own start was refused" >&2
    STATUS=1
  fi
done
assert_nothing_serves_pool "after the refused dependent starts" || STATUS=1
assert_no_pool_mount_active "after the refused dependent starts" || STATUS=1

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: running \`hoserva array start\` to restore the array for every later step"
array_login || die "login as $ADMIN_USERNAME failed ahead of array/start"
START_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" 2>/dev/null)"
if [[ "$START_RESULT" != *'"maintenanceMode":false'* ]]; then
  die "array/start did not report maintenanceMode:false: $START_RESULT"
fi
STARTED_AGAIN=1

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: asserting Samba, NFS and the storage-target gate all come back after array/start"
for unit in smbd nfs-kernel-server hoserva-storage-ready.service hoserva-storage.target; do
  if ! vm_ssh "sudo systemctl is-active '$unit'" >/dev/null 2>&1; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: '$unit' is not active after array/start" >&2
    STATUS=1
  fi
done
if ! vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname maintenance-gate-check-recovered.txt'" >/dev/null 2>&1; then
  echo "maintenance-gate-check[$HOSERVA_LAB_ID]: an SMB put failed after array/start — smbd should be reachable again" >&2
  STATUS=1
fi

echo "maintenance-gate-check[$HOSERVA_LAB_ID]: cleaning up this check's own SMB test files from '$SMB_SHARE'"
vm_ssh "sudo rm -f /mnt/user/$SMB_SHARE/maintenance-gate-check-*.txt" || true

if [[ "$STATUS" -eq 0 ]]; then
  echo "maintenance-gate-check[$HOSERVA_LAB_ID]: the storage-target gate closed on array/stop, refused a dependent Samba/NFS start throughout maintenance, and reopened on array/start — confirmed"
fi
exit "$STATUS"
