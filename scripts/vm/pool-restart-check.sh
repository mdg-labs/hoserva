#!/usr/bin/env bash
# L3 pool-survives-hoservad-restart check (issue #335, doc 02 §1, §4):
# proves that `systemctl restart hoserva` and a SIGKILL of hoservad's own
# MainPID leave the catch-all and every per-share mergerfs mount up and
# serving — the mergerfs processes live in their generated systemd
# .mount units' cgroups, not hoserva.service's. Before this issue,
# packaging/debian/hoserva.service's default KillMode=control-group
# killed every mergerfs child hoservad had spawned, tearing the pool
# down while Samba/NFS kept serving the bare mountpoints.
#
# Runs after "array setup" (run-l3-suite.sh step 3): reuses that step's
# admin session and its live share (ARRAY_ADMIN_*/ARRAY_RESTART_SHARE).
# Always refreshes /usr/bin/hoservad from this workspace's source before
# the assertion so the binary under test is the one just built — the
# same build-elsewhere/run-inside pattern ups-check.sh uses when the
# host has no dpkg-buildpackage (deploy.sh's own .deb path).
#
# Kill by MainPID only — never pkill/killall (orchestrate dispatch).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

ADMIN_USERNAME="${ARRAY_ADMIN_USERNAME:?pool-restart-check.sh needs ARRAY_ADMIN_USERNAME (set by run-l3-suite.sh from its own array setup step)}"
ADMIN_PASSWORD="${ARRAY_ADMIN_PASSWORD:?pool-restart-check.sh needs ARRAY_ADMIN_PASSWORD}"
SHARE_NAME="${ARRAY_RESTART_SHARE:?pool-restart-check.sh needs ARRAY_RESTART_SHARE}"
SHARE_PATH="/mnt/user/$SHARE_NAME"
MARKER="$SHARE_PATH/hoserva-335-marker.txt"
COOKIE_JAR="/tmp/hoserva-pool-restart-check-cookies.txt"
HOLD_PID_FILE="/tmp/hoserva-335-hold.pid"
MARKER_BODY="hoserva-335-restart-check"

# hoserva.service is Type=simple, so systemctl is-active can succeed before
# the API socket is accepting. Poll the login itself inside the deadline.
wait_for_admin_login() {
  local label=$1
  local deadline=$((SECONDS + 60))
  local result=""
  while (( SECONDS < deadline )); do
    if vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
      result="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}'" 2>/dev/null || true)"
      if [[ "$result" == *'"role":"admin"'* ]]; then
        LOGIN_RESULT="$result"
        return 0
      fi
    fi
    sleep 2
  done
  die "$label: API did not accept an admin login within 60s (last response: ${result:-service inactive})"
}

echo "pool-restart-check[$HOSERVA_LAB_ID]: refreshing hoservad from source so the binary under test is this workspace's build (#335)"
BUILD_OUT="$VM_STATE_DIR/pool-restart-build"
mkdir -p -- "$BUILD_OUT"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BUILD_OUT/hoservad" ./cmd/hoservad)
vm_scp "$BUILD_OUT/hoservad" "hoserva@127.0.0.1:/tmp/hoservad-335"
vm_scp "$VM_REPO_ROOT/packaging/debian/hoserva.service" "hoserva@127.0.0.1:/tmp/hoserva.service-335"
vm_ssh 'sudo install -m0755 /tmp/hoservad-335 /usr/bin/hoservad
sudo install -m0644 /tmp/hoserva.service-335 /etc/systemd/system/hoserva.service
sudo systemctl daemon-reload
sudo systemctl restart hoserva'

echo "pool-restart-check[$HOSERVA_LAB_ID]: logging in as $ADMIN_USERNAME"
wait_for_admin_login "after the from-source refresh"

echo "pool-restart-check[$HOSERVA_LAB_ID]: confirming the pool and share '$SHARE_NAME' are mounted"
POOL_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
if [[ "$POOL_RESULT" != *'"mounted":true'* ]]; then
  vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" >/dev/null 2>&1 || true
  POOL_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
fi
[[ "$POOL_RESULT" == *'"mounted":true'* ]] || die "pool is not mounted ahead of the restart check: $POOL_RESULT"

CATCHALL_COUNT="$(vm_ssh "mount | grep -c ' on /mnt/user type fuse.mergerfs'" 2>/dev/null || true)"
[[ "${CATCHALL_COUNT:-0}" == "1" ]] || die "expected exactly one catch-all fuse.mergerfs at /mnt/user before restart, found ${CATCHALL_COUNT:-unknown}"
SHARE_COUNT="$(vm_ssh "mount | grep -c ' on $SHARE_PATH type fuse.mergerfs'" 2>/dev/null || true)"
[[ "${SHARE_COUNT:-0}" == "1" ]] || die "expected exactly one share mount at $SHARE_PATH before restart, found ${SHARE_COUNT:-unknown}"

echo "pool-restart-check[$HOSERVA_LAB_ID]: writing a marker and holding it open across the restart"
# Seed the marker on a data-disk branch directly: the L3 guest's array
# disks are a few GiB, while pool.DefaultOptions's minfreespace is 50G,
# so a create through mergerfs itself returns ENOSPC even though the
# share mount is live and healthy. Reading and holding the FD goes
# through the pool path — that is what must survive the restart.
vm_ssh "sudo mkdir -p /mnt/disk1/$SHARE_NAME && echo '$MARKER_BODY' | sudo tee /mnt/disk1/$SHARE_NAME/hoserva-335-marker.txt >/dev/null" \
  || die "could not seed marker on /mnt/disk1/$SHARE_NAME"
got="$(vm_ssh "sudo cat $MARKER" 2>/dev/null || true)"
[[ "$got" == "$MARKER_BODY" ]] || die "marker not visible through the pool at $MARKER (got '$got')"
# Hold an open FD on the marker through a background bash — proves the
# FUSE mount stays live under a real open file, not only a fresh open
# after the restart. Kill by the recorded PID only.
vm_ssh "sudo bash -c 'exec 3<$MARKER; echo \$\$ > $HOLD_PID_FILE; while kill -0 \$\$ 2>/dev/null; do sleep 1; done' >/dev/null 2>&1 &"
sleep 1
HOLD_PID="$(vm_ssh "sudo cat $HOLD_PID_FILE" 2>/dev/null || true)"
[[ -n "$HOLD_PID" && "$HOLD_PID" =~ ^[0-9]+$ ]] || die "could not start the open-file holder (pid file empty)"

assert_mounts_and_marker() {
  local label=$1
  local catchall share got pool
  catchall="$(vm_ssh "mount | grep -c ' on /mnt/user type fuse.mergerfs'" 2>/dev/null || true)"
  [[ "${catchall:-0}" == "1" ]] || die "$label: expected exactly one catch-all at /mnt/user, found ${catchall:-unknown}"
  share="$(vm_ssh "mount | grep -c ' on $SHARE_PATH type fuse.mergerfs'" 2>/dev/null || true)"
  [[ "${share:-0}" == "1" ]] || die "$label: expected exactly one share mount at $SHARE_PATH, found ${share:-unknown}"
  got="$(vm_ssh "sudo cat $MARKER" 2>/dev/null || true)"
  [[ "$got" == "$MARKER_BODY" ]] || die "$label: marker at $MARKER read back as '$got', want '$MARKER_BODY'"
  pool="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null || true)"
  [[ "$pool" == *'"mounted":true'* ]] || die "$label: GET /pool did not report mounted:true after hoservad came back: $pool"
}

cleanup_hold() {
  if [[ -n "${HOLD_PID:-}" ]]; then
    vm_ssh "sudo kill $HOLD_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup_hold EXIT

echo "pool-restart-check[$HOSERVA_LAB_ID]: === scenario 1/2: systemctl restart hoserva ==="
vm_ssh 'sudo systemctl restart hoserva'
# Re-login: the daemon's sessions died with the process.
wait_for_admin_login "after systemctl restart"
assert_mounts_and_marker "after systemctl restart"
echo "pool-restart-check[$HOSERVA_LAB_ID]: scenario 1 — PASS"

echo "pool-restart-check[$HOSERVA_LAB_ID]: === scenario 2/2: SIGKILL hoservad MainPID (Restart=on-failure) ==="
MAIN_PID="$(vm_ssh 'sudo systemctl show -p MainPID --value hoserva' 2>/dev/null || true)"
[[ "$MAIN_PID" =~ ^[1-9][0-9]*$ ]] || die "could not read hoserva MainPID (got '$MAIN_PID')"
vm_ssh "sudo kill -KILL $MAIN_PID"
wait_for_admin_login "after SIGKILL"
assert_mounts_and_marker "after SIGKILL"
echo "pool-restart-check[$HOSERVA_LAB_ID]: scenario 2 — PASS"

cleanup_hold
trap - EXIT

echo "pool-restart-check[$HOSERVA_LAB_ID]: PASS — catch-all and $SHARE_PATH survived systemctl restart and SIGKILL with an open file"
