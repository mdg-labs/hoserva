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
# Runs after array setup (run-l3-suite.sh's own step 3) and the
# storage-target boot-ordering check (issue #372, which leaves the array
# whole and running) — reuses that step's own admin account and
# 'massdel' share from the environment (ARRAY_ADMIN_USERNAME/
# ARRAY_ADMIN_PASSWORD/ARRAY_SMB_SHARE), never a second, possibly-
# diverging copy of run-l3-suite.sh's own constants. Leaves the array
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
# lookup smb-stop-check.sh and storage-target-boot-check.sh already use.
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

# assert_nothing_serves_pool checks that neither /mnt/user nor the
# SMB_SHARE mount under it is a live mergerfs mount — findmnt's own
# FSTYPE, the same real hazard storage-target-boot-check.sh's own
# assert_pool_live checks for — and that a client cannot actually reach
# the share over SMB. A failed check also inspects the bare mountpoint
# directly: if smbd genuinely did start and serve /mnt/user unmounted, a
# put here is where a client's write would land on the boot disk instead.
assert_nothing_serves_pool() {
  local label=$1
  local ok=1 fstype
  fstype="$(vm_ssh 'findmnt -n -o FSTYPE /mnt/user' 2>/dev/null || true)"
  if [[ "$fstype" == "fuse.mergerfs" ]]; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: /mnt/user is still a live mergerfs mount $label" >&2
    ok=0
  fi
  if vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname maintenance-gate-check-probe.txt'" >/dev/null 2>&1; then
    echo "maintenance-gate-check[$HOSERVA_LAB_ID]: an SMB put succeeded $label — smbd must still have been reachable and serving the share" >&2
    ok=0
  fi
  if [[ "$ok" -eq 0 ]]; then
    local bare_contents
    bare_contents="$(vm_ssh 'sudo find /mnt/user -mindepth 1' 2>/dev/null || true)"
    if [[ -n "$bare_contents" ]]; then
      echo "maintenance-gate-check[$HOSERVA_LAB_ID]: /mnt/user has content while the pool is unmounted $label — a write landed on the boot disk: $bare_contents" >&2
    fi
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
