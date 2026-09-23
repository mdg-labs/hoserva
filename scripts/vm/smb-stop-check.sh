#!/usr/bin/env bash
# L3 SMB-connected array stop/start check (issue #309, doc 02 §4): proves
# `hoserva array stop` — reached here through the real POST /array/stop
# served by a real hoservad — actually stops Samba and NFS *before* it
# ever unmounts the pool, with a real SMB client actively writing the
# whole time. Before this issue, newArraySequence
# (cmd/hoservad/array.go) left job.ArraySequence.Services empty, so a
# stop unmounted /mnt/user (pool.CatchAllPath) while smbd was still bound
# to a share underneath it — a client still writing landed directly on
# the bare mountpoint's own root-filesystem directory, hidden again the
# instant the pool remounted. disk.ServiceUnitController (internal/disk/
# unitcontrol.go) is what this script exercises for real, through
# systemd, never a fake.
#
# Runs after "array setup" (run-l3-suite.sh's own step 3): reuses that
# step's admin session (to drive createUser/setUserPassword/array-stop/
# array-start, every one of them x-hoserva-role: admin) and its "massdel"
# share (SMB-enabled by share.defaultSMB, doc 02 §4) rather than creating
# a second one, taking their names from the environment
# (ARRAY_ADMIN_USERNAME/ARRAY_ADMIN_PASSWORD/ARRAY_SMB_SHARE) so this
# script never hardcodes a second, possibly-diverging copy of run-l3-
# suite.sh's own constants. Runs before array-sequence-check.sh (step
# 13/13, issue #146), which permanently detaches an array disk — this
# script needs every array disk still present and the array still
# mounted.
#
# The SMB client itself connects as a separate, dedicated account this
# script creates (SMB_USERNAME below), never the admin: setUserPassword
# refuses to touch the sole admin account (`cannot_modify_admin`,
# confirmed empirically against this exact harness), and smb.conf's own
# `security = user` with no `valid users` line (RenderSambaConf,
# internal/config/samba.go) gives any authenticated account the same
# share access regardless of role, so a share-only account exercises the
# real client path exactly as well as the admin would.
#
# setUserPassword's own Samba write (internal/share.SmbpasswdAccounts,
# `smbpasswd -a -s`) requires the Unix account it names to already
# exist — hoservad never provisions one itself, the same precondition
# scripts/devenv/smb-check.sh already works around for the lab — so this
# script creates a nologin system account for SMB_USERNAME first.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

ADMIN_USERNAME="${ARRAY_ADMIN_USERNAME:?smb-stop-check.sh needs ARRAY_ADMIN_USERNAME (set by run-l3-suite.sh from its own array setup step)}"
ADMIN_PASSWORD="${ARRAY_ADMIN_PASSWORD:?smb-stop-check.sh needs ARRAY_ADMIN_PASSWORD}"
SMB_SHARE="${ARRAY_SMB_SHARE:?smb-stop-check.sh needs ARRAY_SMB_SHARE}"

SMB_USERNAME="hoserva-l3-smb"
SMB_PASSWORD="hoserva-l3-smb-password"

COOKIE_JAR="/tmp/hoserva-smb-stop-check-cookies.txt"
LOOP_SCRIPT_REMOTE="/tmp/hoserva-smb-stop-check-loop.sh"
LOOP_LOG_REMOTE="/tmp/hoserva-smb-stop-check-loop.log"
LOOP_PID_FILE_REMOTE="/tmp/hoserva-smb-stop-check-loop.pid"

echo "smb-stop-check[$HOSERVA_LAB_ID]: ensuring smbclient is present on the guest"
vm_ssh 'command -v smbclient >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq smbclient)'

echo "smb-stop-check[$HOSERVA_LAB_ID]: logging in as $ADMIN_USERNAME"
LOGIN_RESULT="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ADMIN_USERNAME\",\"password\":\"$ADMIN_PASSWORD\"}'" 2>/dev/null)"
[[ "$LOGIN_RESULT" == *'"role":"admin"'* ]] || die "login as $ADMIN_USERNAME did not return an admin session: $LOGIN_RESULT"

echo "smb-stop-check[$HOSERVA_LAB_ID]: creating a dedicated share-only account ($SMB_USERNAME) for the SMB client — setUserPassword refuses to touch the sole admin account"
USERS_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/users" 2>/dev/null)"
if [[ "$USERS_RESULT" =~ \"id\":\"([^\"]+)\",\"username\":\"$SMB_USERNAME\" ]]; then
  SMB_USER_ID="${BASH_REMATCH[1]}"
else
  CREATE_USER_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/users -H 'Content-Type: application/json' -d '{\"username\":\"$SMB_USERNAME\"}'" 2>/dev/null)"
  if [[ "$CREATE_USER_RESULT" =~ \"id\":\"([^\"]+)\" ]]; then
    SMB_USER_ID="${BASH_REMATCH[1]}"
  else
    die "createUser($SMB_USERNAME) did not return an id: $CREATE_USER_RESULT"
  fi
fi

echo "smb-stop-check[$HOSERVA_LAB_ID]: provisioning a Unix account for $SMB_USERNAME so smbpasswd -a (setUserPassword) has an account to attach to"
vm_ssh "getent passwd '$SMB_USERNAME' >/dev/null 2>&1 || sudo useradd -M -N -s /usr/sbin/nologin '$SMB_USERNAME'"

echo "smb-stop-check[$HOSERVA_LAB_ID]: setting a Samba password for $SMB_USERNAME through setUserPassword"
SET_PW_STATUS="$(vm_ssh "curl -sk -b $COOKIE_JAR -o /dev/null -w '%{http_code}' -X POST https://127.0.0.1:8008/api/v1/users/$SMB_USER_ID/password -H 'Content-Type: application/json' -d '{\"password\":\"$SMB_PASSWORD\"}'" 2>/dev/null)"
[[ "$SET_PW_STATUS" == "204" ]] || die "setUserPassword($SMB_USERNAME) returned HTTP $SET_PW_STATUS"

echo "smb-stop-check[$HOSERVA_LAB_ID]: confirming the pool is mounted and '$SMB_SHARE' is listed before connecting"
POOL_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
[[ "$POOL_RESULT" == *'"mounted":true'* ]] || die "pool is not mounted ahead of the SMB connection — array setup (step 3) did not leave it mounted: $POOL_RESULT"
SHARES_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/shares" 2>/dev/null)"
[[ "$SHARES_RESULT" == *"\"name\":\"$SMB_SHARE\""* ]] || die "share '$SMB_SHARE' is not listed ahead of the SMB connection: $SHARES_RESULT"

echo "smb-stop-check[$HOSERVA_LAB_ID]: writing once over SMB to confirm access works before any stop"
vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname smb-stop-check-baseline.txt'" \
  || die "baseline smbclient put failed before array stop — SMB access is not working yet"

echo "smb-stop-check[$HOSERVA_LAB_ID]: starting a background SMB client that keeps writing to '$SMB_SHARE' while the stop runs"
LOCAL_LOOP_SCRIPT="$(mktemp)"
cat >"$LOCAL_LOOP_SCRIPT" <<LOOP
#!/usr/bin/env bash
i=0
while true; do
  i=\$((i + 1))
  smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c "put /etc/hostname smb-stop-check-loop-\$i.txt" >>'$LOOP_LOG_REMOTE' 2>&1
  sleep 1
done
LOOP
vm_scp "$LOCAL_LOOP_SCRIPT" "hoserva@127.0.0.1:$LOOP_SCRIPT_REMOTE"
rm -f -- "$LOCAL_LOOP_SCRIPT"

# The proven pattern for a detached, PID-capturable background process
# over a non-interactive ssh command (network-revert-check.sh's own
# hoservad launcher uses the same shape): nohup backgrounds it, and $! —
# expanded by the *remote* shell, escaped here so this local script never
# expands it itself — captures its PID into a file this script reads
# back right after.
vm_ssh "chmod +x '$LOOP_SCRIPT_REMOTE'; rm -f '$LOOP_LOG_REMOTE' '$LOOP_PID_FILE_REMOTE'; nohup '$LOOP_SCRIPT_REMOTE' >'$LOOP_LOG_REMOTE' 2>&1 & echo \$! >'$LOOP_PID_FILE_REMOTE'"
sleep 1
LOOP_PID="$(vm_ssh "cat '$LOOP_PID_FILE_REMOTE'" 2>/dev/null)"
[[ "$LOOP_PID" =~ ^[0-9]+$ ]] || die "could not capture the background SMB loop's PID"
echo "smb-stop-check[$HOSERVA_LAB_ID]: background SMB loop running as guest PID $LOOP_PID"

# Killed by this exact captured PID only, never by name or pattern — the
# loop belongs to the guest's own "hoserva" SSH user, the same one that
# started it, so no sudo is needed to stop it.
cleanup() { vm_ssh "kill '$LOOP_PID' >/dev/null 2>&1 || true"; }
trap cleanup EXIT

sleep 2

STATUS=0

echo "smb-stop-check[$HOSERVA_LAB_ID]: calling POST /array/stop with a client still writing"
STOP_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/stop -H 'Content-Type: application/json' -d '{\"confirm\":true}'" 2>/dev/null)"
if [[ "$STOP_RESULT" != *'"maintenanceMode":true'* ]]; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: array/stop did not report maintenanceMode:true: $STOP_RESULT" >&2
  STATUS=1
fi

vm_ssh "kill '$LOOP_PID' >/dev/null 2>&1 || true"
trap - EXIT

if [[ "$STATUS" -eq 0 ]]; then
  if vm_ssh 'sudo systemctl is-active smbd' >/dev/null 2>&1; then
    echo "smb-stop-check[$HOSERVA_LAB_ID]: smbd is still active after array/stop" >&2
    STATUS=1
  fi
  # nfs-kernel-server is the real unit config.NFSServiceUnit (internal/
  # config/nfs.go) and disk.ServiceUnitController act on — asserted by
  # that name, not the "nfs-server" alias, so this checks exactly what
  # this issue's own code stopped.
  if vm_ssh 'sudo systemctl is-active nfs-kernel-server' >/dev/null 2>&1; then
    echo "smb-stop-check[$HOSERVA_LAB_ID]: nfs-kernel-server is still active after array/stop" >&2
    STATUS=1
  fi
  if vm_ssh "smbclient '//127.0.0.1/$SMB_SHARE' -U '$SMB_USERNAME%$SMB_PASSWORD' -c 'put /etc/hostname smb-stop-check-after-stop.txt'" >/dev/null 2>&1; then
    echo "smb-stop-check[$HOSERVA_LAB_ID]: an SMB put succeeded after array/stop — smbd must still have been reachable" >&2
    STATUS=1
  fi
  if vm_ssh 'mountpoint -q /mnt/user' >/dev/null 2>&1; then
    echo "smb-stop-check[$HOSERVA_LAB_ID]: /mnt/user is still mounted after array/stop" >&2
    STATUS=1
  fi
  BARE_CONTENTS="$(vm_ssh 'sudo find /mnt/user -mindepth 1' 2>/dev/null || true)"
  if [[ -n "$BARE_CONTENTS" ]]; then
    echo "smb-stop-check[$HOSERVA_LAB_ID]: the bare /mnt/user mountpoint has content while the array is stopped — a client write landed on the root filesystem: $BARE_CONTENTS" >&2
    STATUS=1
  fi
fi

echo "smb-stop-check[$HOSERVA_LAB_ID]: calling POST /array/start"
START_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" 2>/dev/null)"
if [[ "$START_RESULT" != *'"maintenanceMode":false'* ]]; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: array/start did not report maintenanceMode:false: $START_RESULT" >&2
  STATUS=1
fi
POOL_AFTER="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
if [[ "$POOL_AFTER" != *'"mounted":true'* ]]; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: pool is not mounted after array/start: $POOL_AFTER" >&2
  STATUS=1
fi
if ! vm_ssh 'sudo systemctl is-active smbd' >/dev/null 2>&1; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: smbd is not active after array/start" >&2
  STATUS=1
fi
if ! vm_ssh 'sudo systemctl is-active nfs-kernel-server' >/dev/null 2>&1; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: nfs-kernel-server is not active after array/start" >&2
  STATUS=1
fi

echo "smb-stop-check[$HOSERVA_LAB_ID]: cleaning up this check's own SMB test files from '$SMB_SHARE'"
vm_ssh "sudo rm -f /mnt/user/$SMB_SHARE/smb-stop-check-*.txt" || true

if [[ "$STATUS" -eq 0 ]]; then
  echo "smb-stop-check[$HOSERVA_LAB_ID]: array stop/start with an SMB client connected — confirmed"
fi
exit "$STATUS"
