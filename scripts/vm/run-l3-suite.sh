#!/usr/bin/env bash
# `make vm-suite` — the nightly/pre-release L3 suite (doc 06 §4, §7,
# Q79): install, onboarding, array setup, disk yank and reconstruction,
# `virsh destroy` mid-sync recovery, reboot persistence, config
# backup/restore, spindown, network confirm-or-revert (issue #114),
# the array stop/start sequence (issue #146), and the UPS
# on-battery/power-restored/low-battery flow against NUT's own dummy-ups
# driver (issue #250).
#
# Every step below runs against whatever hoservad actually exposes today
# and reports PASS/FAIL for it. A step the product does not implement yet
# is reported NOT-YET-IMPLEMENTED, with the reason, and does not fail the
# suite — pretending array setup, disk-yank reconstruction or config
# backup ran would be a false pass; silently dropping the step would hide
# that the acceptance criterion isn't met yet. `run_step`'s summary table
# at the end is the honest record either way. Journey coverage that needs
# a running hoservad UI/API lives in web/'s own Playwright suite
# (scripts/vm/run-playwright.sh), invoked from here once a VM is up.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

declare -a STEP_NAMES=()
declare -a STEP_RESULTS=()

record() { STEP_NAMES+=("$1"); STEP_RESULTS+=("$2"); }

not_yet() {
  echo "vm-suite[$HOSERVA_LAB_ID]: $1 — NOT-YET-IMPLEMENTED: $2"
  record "$1" "NOT-YET-IMPLEMENTED: $2"
}

pass() {
  echo "vm-suite[$HOSERVA_LAB_ID]: $1 — PASS"
  record "$1" "PASS"
}

fail() {
  echo "vm-suite[$HOSERVA_LAB_ID]: $1 — FAIL: $2" >&2
  record "$1" "FAIL: $2"
}

# Array setup (step 3) and journey 5's own fixture (seeded ahead of step 8,
# doc 06 §4) share this L3 admin session and cookie jar — the same account
# step 2's onboarding creates.
ARRAY_ADMIN_USERNAME="hoserva-l3"
ARRAY_ADMIN_PASSWORD="hoserva-l3-suite-password"
ARRAY_COOKIE_JAR="/tmp/hoserva-l3-suite-cookies.txt"

# Journey 5's own share (doc 06 §4): array-only cache mode (no cache disk
# is assigned below), seeded with far more files than the threshold
# guard's default removed-count/percent thresholds (Q16) tolerate, so
# deleting most of them trips the guard the same way a runaway `rm -rf`
# would. Path is D10's own `/mnt/user/<name>` convention.
JOURNEY5_SHARE="massdel"
JOURNEY5_SHARE_PATH="/mnt/user/$JOURNEY5_SHARE"
JOURNEY5_FILE_COUNT=300
JOURNEY5_DELETE_COUNT=250

# array_login signs the L3 admin into the guest's own hoservad over its
# loopback HTTPS listener, writing ARRAY_COOKIE_JAR for every later admin
# call array_setup and seed_journey5_fixture make.
array_login() {
  local result
  result="$(vm_ssh "curl -sk -c $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$ARRAY_ADMIN_USERNAME\",\"password\":\"$ARRAY_ADMIN_PASSWORD\"}'" 2>/dev/null)"
  [[ "$result" == *"\"username\":\"$ARRAY_ADMIN_USERNAME\""* ]]
}

# ensure_pool_mounted calls startArray and checks getPool until the array
# reports mounted. A freshly created array's pool can 501 on startArray
# until hoservad restarts (issue #262) — this restarts the guest's own
# hoservad once and retries before giving up, rather than fixing that gap
# itself (out of scope for this issue).
ensure_pool_mounted() {
  local pool_result
  pool_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
  [[ "$pool_result" == *'"mounted":true'* ]] && return 0

  vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" >/dev/null 2>&1 || true
  pool_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
  [[ "$pool_result" == *'"mounted":true'* ]] && return 0

  echo "vm-suite[$HOSERVA_LAB_ID]: array/start did not mount the pool — restarting hoservad and retrying (issue #262's array-not-mounted-until-restart gap)"
  vm_ssh 'sudo systemctl restart hoserva' >/dev/null 2>&1 || true
  local deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )) && ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; do
    sleep 2
  done
  array_login || return 1
  vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" >/dev/null 2>&1 || true
  pool_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
  [[ "$pool_result" == *'"mounted":true'* ]]
}

# wait_job_terminal polls getJob until it reaches a terminal JobStatus (or
# the deadline passes) and prints the status it settled on (or "unknown").
# Callers read the job body it also leaves for them via job_result_out.
wait_job_terminal() {
  local job_id=$1 timeout_s=$2
  local deadline=$((SECONDS + timeout_s))
  job_status=""
  job_result=""
  while (( SECONDS < deadline )); do
    job_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/jobs/$job_id" 2>/dev/null)"
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

# array_setup drives createArray (one parity disk, two data disks — the
# smallest layout Q18's content-file placement accepts without a cache
# disk, doc 02 §2) and createShare through the real API, discovering the
# array's own virtio devices from the domain XML create-vm.sh wrote
# (same pattern array-sequence-check.sh's own device discovery uses). On
# failure it sets ARRAY_SETUP_REASON and returns 1.
array_setup() {
  echo "vm-suite[$HOSERVA_LAB_ID]: ensuring mergerfs, snapraid, xfsprogs and e2fsprogs are present on the guest"
  vm_ssh 'command -v mergerfs >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq mergerfs)'
  vm_ssh 'command -v snapraid >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq snapraid)'
  vm_ssh 'command -v mkfs.xfs >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq xfsprogs)'
  vm_ssh 'command -v mkfs.ext4 >/dev/null 2>&1 || (sudo apt-get update -qq && sudo apt-get install -y -qq e2fsprogs)'

  echo "vm-suite[$HOSERVA_LAB_ID]: discovering array disks from the live domain XML"
  local domxml
  domxml="$(virsh -c "$VM_CONNECT" dumpxml "$VM_DOMAIN")"
  if [[ -z "$domxml" ]]; then
    ARRAY_SETUP_REASON="could not read domain XML for '$VM_DOMAIN'"
    return 1
  fi

  local dev="" serial="" parity_dev="" data1_dev="" data2_dev=""
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
      case "$serial" in
        "parity1-hoserva-$HOSERVA_LAB_ID") parity_dev="$dev" ;;
        "disk1-hoserva-$HOSERVA_LAB_ID") data1_dev="$dev" ;;
        "disk2-hoserva-$HOSERVA_LAB_ID") data2_dev="$dev" ;;
      esac
      dev=""; serial=""
    fi
  done <<<"$domxml"

  if [[ -z "$parity_dev" || -z "$data1_dev" || -z "$data2_dev" ]]; then
    ARRAY_SETUP_REASON="could not find parity1/disk1/disk2 array devices in the domain XML — is this domain fresh from 'make vm-up'?"
    return 1
  fi
  echo "vm-suite[$HOSERVA_LAB_ID]: array devices: parity=/dev/$parity_dev data1=/dev/$data1_dev data2=/dev/$data2_dev"

  if ! array_login; then
    ARRAY_SETUP_REASON="login as the L3 admin failed ahead of createArray"
    return 1
  fi

  local sorted_devices confirmation
  mapfile -t sorted_devices < <(printf '%s\n' "/dev/$parity_dev" "/dev/$data1_dev" "/dev/$data2_dev" | sort)
  confirmation="ERASE $(printf '%s, ' "${sorted_devices[@]}")"
  confirmation="${confirmation%, }"

  local create_array_body create_array_result job_id
  create_array_body="{\"disks\":[{\"device\":\"/dev/${parity_dev}\",\"role\":\"parity\",\"filesystem\":\"xfs\"},{\"device\":\"/dev/${data1_dev}\",\"role\":\"data\",\"filesystem\":\"ext4\"},{\"device\":\"/dev/${data2_dev}\",\"role\":\"data\",\"filesystem\":\"ext4\"}],\"confirmation\":\"${confirmation}\"}"
  create_array_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/disks/array -H 'Content-Type: application/json' -d '$create_array_body'" 2>/dev/null)"
  if [[ "$create_array_result" =~ \"id\":\"([^\"]+)\" ]]; then
    job_id="${BASH_REMATCH[1]}"
  else
    ARRAY_SETUP_REASON="createArray did not return a job id: $create_array_result"
    return 1
  fi

  wait_job_terminal "$job_id" 120
  if [[ "$job_status" != "succeeded" ]]; then
    ARRAY_SETUP_REASON="createArray job did not succeed (status=${job_status:-unknown}): $job_result"
    return 1
  fi

  if ! ensure_pool_mounted; then
    ARRAY_SETUP_REASON="pool did not report mounted:true after array/start, including after restarting hoservad for issue #262's array-not-mounted-until-restart gap"
    return 1
  fi

  # createShare regenerates both smb.conf and /etc/exports for every
  # share, not only ones with SMB or NFS enabled (D4: the whole file is
  # generated from state) — step 2's own onboarding left both unmanaged
  # (--leave-all, Q76), which refuses that write. Importing here takes
  # over management, but only flips management mode: it does not read
  # the pre-existing file into the database first, so the very next
  # regeneration silently drops whatever seed-existing-host.sh put there
  # (issue #264, a real product bug, filed and deferred rather than
  # fixed here). The suite's own "existing host config" check therefore
  # runs right after onboarding, above, before this import ever touches
  # the files — not here.
  if ! vm_ssh 'sudo hoserva doctor apply-host-config --samba import --nfs import' >/dev/null 2>&1; then
    ARRAY_SETUP_REASON="apply-host-config --samba import --nfs import failed — createShare needs smb.conf and /etc/exports importable"
    return 1
  fi

  local share_result
  share_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/shares -H 'Content-Type: application/json' -d '{\"name\":\"$JOURNEY5_SHARE\",\"cacheMode\":\"array-only\"}'" 2>/dev/null)"
  if [[ "$share_result" != *"\"name\":\"$JOURNEY5_SHARE\""* ]]; then
    ARRAY_SETUP_REASON="createShare did not return the expected share: $share_result"
    return 1
  fi

  return 0
}

# seed_journey5_fixture writes JOURNEY5_FILE_COUNT files into journey 5's
# share through the pool mount, runs a baseline sync through the API
# (guard is clear on this, its first-ever diff — every file is newly
# added, nothing removed), then deletes JOURNEY5_DELETE_COUNT of them —
# well past the threshold guard's default thresholds (Q16:
# parity.DefaultRemovedFilesMax/DefaultRemovedUpdatedPercent) — so
# journey 5's own "Run diff" click has real removals, a tripped guard and
# an unsynced state to assert against. Never runs the diff itself: that
# is journey 5's own first UI step.
seed_journey5_fixture() {
  if ! array_login; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: could not log in as the L3 admin" >&2
    return 1
  fi
  if ! ensure_pool_mounted; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: pool is not mounted, cannot seed $JOURNEY5_SHARE" >&2
    return 1
  fi

  local local_script remote_script="/tmp/hoserva-journey5-fixture.sh"
  local_script="$(mktemp)"
  trap 'rm -f -- "$local_script"' RETURN
  cat >"$local_script" <<'FIXTURE'
#!/usr/bin/env bash
set -euo pipefail
mode=$1
share_path=$2
case "$mode" in
  seed)
    file_count=$3
    mkdir -p "$share_path"
    for ((i = 1; i <= file_count; i++)); do
      echo "journey-5-file-$i" >"$share_path/file-$i.txt"
    done
    ;;
  delete)
    delete_count=$3
    mapfile -t victims < <(ls "$share_path"/file-*.txt | head -n "$delete_count")
    rm -f -- "${victims[@]}"
    ;;
  *)
    echo "hoserva-journey5-fixture: unknown mode '$mode'" >&2
    exit 1
    ;;
esac
FIXTURE
  vm_scp "$local_script" "hoserva@127.0.0.1:$remote_script"
  vm_ssh "chmod +x $remote_script"

  echo "vm-suite[$HOSERVA_LAB_ID]: seeding $JOURNEY5_FILE_COUNT files into $JOURNEY5_SHARE_PATH"
  if ! vm_ssh "sudo $remote_script seed '$JOURNEY5_SHARE_PATH' $JOURNEY5_FILE_COUNT"; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: seeding files failed" >&2
    return 1
  fi

  local sync_result job_id
  sync_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/sync -H 'Content-Type: application/json' -d '{\"confirm\":false,\"dryRun\":false}'" 2>/dev/null)"
  if [[ "$sync_result" =~ \"id\":\"([^\"]+)\" ]]; then
    job_id="${BASH_REMATCH[1]}"
  else
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: baseline startSync did not return a job id: $sync_result" >&2
    return 1
  fi

  wait_job_terminal "$job_id" 180
  if [[ "$job_status" != "succeeded" ]]; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: baseline sync did not succeed (status=${job_status:-unknown}): $job_result" >&2
    return 1
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: deleting $JOURNEY5_DELETE_COUNT of $JOURNEY5_FILE_COUNT seeded files — mass deletion past the guard's default threshold (Q16)"
  if ! vm_ssh "sudo $remote_script delete '$JOURNEY5_SHARE_PATH' $JOURNEY5_DELETE_COUNT"; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture: mass deletion failed" >&2
    return 1
  fi

  return 0
}

echo "vm-suite[$HOSERVA_LAB_ID]: === 1/12 install ==="
if vm_domain_exists "$VM_DOMAIN"; then
  "$script_dir/destroy-vm.sh"
fi
"$script_dir/create-vm.sh"
seed_ok=false
if "$script_dir/seed-existing-host.sh"; then
  seed_ok=true
fi
if DEB="${DEB:-}" TAG="${TAG:-}" "$script_dir/deploy.sh"; then
  if $seed_ok; then
    pass "install"
  else
    fail "install" "deploy succeeded but seed-existing-host.sh failed — guest did not get a Samba share, NFS export and fstab mount before the .deb"
  fi
else
  fail "install" "deploy.sh failed — see its own output above (on the dev host this is expected: dpkg-buildpackage/debhelper/fakeroot are deliberately not installed here, per scripts/release/build-deb.sh's own header comment; a hosted CI runner has them)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 2/12 onboarding ==="
if vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  SETUP_STATUS="$(vm_ssh "curl -sk https://127.0.0.1:8008/api/v1/setup/status" 2>/dev/null || true)"
  if [[ "$SETUP_STATUS" == *'"adminExists":false'* ]]; then
    CREATE_RESULT="$(vm_ssh "curl -sk -X POST https://127.0.0.1:8008/api/v1/setup/admin -H 'Content-Type: application/json' -d '{\"username\":\"hoserva-l3\",\"password\":\"hoserva-l3-suite-password\"}'" 2>/dev/null || true)"
    if [[ "$CREATE_RESULT" == *'"username":"hoserva-l3"'* ]]; then
      APPLY_STATUS=0
      APPLY_RESULT="$(vm_ssh "sudo hoserva doctor apply-host-config --leave-all" 2>&1)" || APPLY_STATUS=$?
      if [[ "$APPLY_STATUS" -eq 0 && "$APPLY_RESULT" == *"docker data-root:"* ]]; then
        pass "onboarding"
      else
        fail "onboarding" "apply-host-config --leave-all exited ${APPLY_STATUS}: ${APPLY_RESULT}"
      fi
    else
      fail "onboarding" "createFirstAdmin did not return the expected admin: $CREATE_RESULT"
    fi
  else
    fail "onboarding" "getSetupStatus did not report adminExists=false: $SETUP_STATUS"
  fi
else
  not_yet "onboarding" "hoservad is not active on the guest (install step above did not complete — see step 1)"
fi

# Runs here — right after onboarding and before array setup — because
# array_setup's own createShare needs smb.conf/exports import (see the
# comment above array_setup), which regenerates both files from hoservad's
# database and drops whatever seed-existing-host.sh put there (issue
# #264: apply-host-config --samba/--nfs import only flips management
# mode, it never reads the pre-existing file into the database first).
# Checking survival now, immediately after the one point in the suite
# where "install and onboarding" (this check's own doc 06 §4/#113
# contract) has happened and nothing has touched those files yet, is
# correct regardless of #264; checking after array setup would fail on
# a real, but separate and already-tracked, product gap.
echo "vm-suite[$HOSERVA_LAB_ID]: === existing host config (Q76) ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/existing-host-config-check.sh"; then
    pass "existing host config"
  else
    fail "existing host config" "Samba share, NFS export or fstab mount did not survive install and onboarding — see existing-host-config-check.sh output above"
  fi
else
  not_yet "existing host config" "no running domain (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 3/12 array setup ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if array_setup; then
    pass "array setup"
  else
    fail "array setup" "$ARRAY_SETUP_REASON"
  fi
else
  not_yet "array setup" "no active hoservad on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 4/12 disk yank and reconstruction ==="
not_yet "disk yank and reconstruction" "depends on array setup (step 3) existing first — nothing to reconstruct without a configured array"

echo "vm-suite[$HOSERVA_LAB_ID]: === 5/12 virsh destroy mid-sync recovery ==="
not_yet "virsh destroy mid-sync recovery" "no sync/parity operation is reachable via the API yet (Q79/doc 02 §2's threshold-guard-protected sync) — 'mid-sync' has nothing running to interrupt"

echo "vm-suite[$HOSERVA_LAB_ID]: === 6/12 reboot persistence ==="
if vm_domain_running "$VM_DOMAIN"; then
  # sshd answering (the existing service, still up from before any
  # reboot happened) can satisfy a plain "wait for SSH" check without the
  # guest ever having rebooted — 'virsh reboot' returns as soon as it has
  # sent the request, not once the guest acts on it, and can itself fail
  # silently. So this records the guest's boot id first, requires 'virsh
  # reboot' to actually succeed, and only trusts a reboot happened once
  # SSH comes back reporting a *different* boot id — never on SSH/port
  # readiness alone.
  before_boot_id="$(vm_ssh 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)"
  if [[ -z "$before_boot_id" ]]; then
    fail "reboot persistence" "could not read the guest's boot id before rebooting"
  elif ! virsh -c "$VM_CONNECT" reboot "$VM_DOMAIN" >/dev/null; then
    fail "reboot persistence" "'virsh reboot $VM_DOMAIN' failed"
  else
    reboot_seen=false
    timeout_s=180
    start_s=$SECONDS
    while (( SECONDS - start_s < timeout_s )); do
      after_boot_id="$(vm_ssh 'cat /proc/sys/kernel/random/boot_id' 2>/dev/null || true)"
      if [[ -n "$after_boot_id" && "$after_boot_id" != "$before_boot_id" ]]; then
        reboot_seen=true
        break
      fi
      sleep 1
    done
    if ! $reboot_seen; then
      fail "reboot persistence" "guest boot id did not change within ${timeout_s}s of 'virsh reboot' — the guest may never have actually rebooted"
    elif vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
      pass "reboot persistence"
    else
      fail "reboot persistence" "hoservad was not active again after a confirmed guest reboot"
    fi
  fi
else
  not_yet "reboot persistence" "no running domain (install step above did not complete)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 7/12 config backup and restore ==="
not_yet "config backup and restore" "no backup/export or import operation is in api/openapi.yaml yet (internal/backup does not exist) — doc 10's config backup feature has not landed"

echo "vm-suite[$HOSERVA_LAB_ID]: === 8/12 Playwright journeys ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if seed_journey5_fixture; then
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture ready (share seeded, baseline synced, mass deletion applied)"
  else
    echo "vm-suite[$HOSERVA_LAB_ID]: journey 5 fixture preparation failed above — journey 5 must now fail for real, not skip" >&2
  fi
fi
if [[ -x "$script_dir/run-playwright.sh" ]]; then
  if HOSERVA_E2E_BASE_URL="https://127.0.0.1:$VM_HTTPS_PORT" "$script_dir/run-playwright.sh"; then
    pass "Playwright journeys"
  else
    fail "Playwright journeys" "see web/'s own Playwright report above"
  fi
else
  not_yet "Playwright journeys" "scripts/vm/run-playwright.sh is missing or not executable"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 9/12 spindown: SMART-poll IO-neutrality ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if "$script_dir/spindown-check.sh"; then
    pass "spindown: SMART-poll IO-neutrality"
  else
    fail "spindown: SMART-poll IO-neutrality" "see spindown-check.sh output above"
  fi
else
  not_yet "spindown: SMART-poll IO-neutrality" "no active hoservad on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 10/12 spindown: 30-min flat counters with a running pool ==="
not_yet "spindown: 30-min flat counters with a running pool" "needs a mergerfs/SnapRAID pool configured through hoservad (no array/pool operation is in api/openapi.yaml yet, the same gap step 3 names) plus a scheduled SMART-poll and change-journal job wired into hoservad (internal/disk's SMART poller and internal/parity's change journal exist as Go packages, issue #24, but cmd/hoservad/main.go calls neither on a timer yet) — the lab's own zero-organic-IO property under realistic idle/appdata/SMB-client load is already confirmed (doc 08 Spike 1, 2026-09-15), but without SMART polling or the change journal actually running; re-check once the pool API and the scheduler land"

echo "vm-suite[$HOSERVA_LAB_ID]: === NFS export mount (issue #47) ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/nfs-export-check.sh"; then
    pass "NFS export mount"
  else
    fail "NFS export mount" "guest loopback NFS mount did not write a file — see nfs-export-check.sh output above"
  fi
else
  not_yet "NFS export mount" "no running domain (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 11/12 network confirm-or-revert (Q75) ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/network-revert-check.sh"; then
    pass "network confirm-or-revert"
  else
    fail "network confirm-or-revert" "see network-revert-check.sh output above (issue #114) — switches the guest NIC to ifupdown for the test; later steps run on that backend"
  fi
else
  not_yet "network confirm-or-revert" "no running domain (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 12/12 array stop/start sequence: missing disk at boot, service stops before unmount ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/array-sequence-check.sh"; then
    pass "array stop/start sequence"
  else
    fail "array stop/start sequence" "see array-sequence-check.sh output above (issue #146) — this step permanently detaches one array disk from this domain's own persistent config, so it runs after every other step but the UPS one"
  fi
else
  not_yet "array stop/start sequence" "no running domain (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === UPS: on battery, power restored, low battery (NUT's own dummy-ups driver, issue #250) ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/ups-check.sh"; then
    pass "UPS on-battery/power-restored/low-battery"
  else
    fail "UPS on-battery/power-restored/low-battery" "see ups-check.sh output above (issue #250) — this step ends with a real low-battery shutdown, leaving the domain shut off, so it runs last of all"
  fi
else
  not_yet "UPS on-battery/power-restored/low-battery" "no running domain (install step above did not complete — see step 1)"
fi

echo ""
echo "vm-suite[$HOSERVA_LAB_ID]: ===== summary ====="
overall_fail=0
for i in "${!STEP_NAMES[@]}"; do
  printf 'vm-suite[%s]: %-32s %s\n' "$HOSERVA_LAB_ID" "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]}"
  [[ "${STEP_RESULTS[$i]}" == FAIL:* ]] && overall_fail=1
done

exit "$overall_fail"
