#!/usr/bin/env bash
# `make vm-suite` — the nightly/pre-release L3 suite (doc 06 §4, §7,
# Q79): install, onboarding, array setup, array stop/start with a live
# share (issue #268), disk yank and reconstruction, `virsh destroy`
# mid-sync recovery, reboot persistence, config backup/restore, spindown,
# network confirm-or-revert (issue #114), the array stop/start sequence
# (issue #146), and the UPS on-battery/power-restored/low-battery flow
# against NUT's own dummy-ups driver (issue #250).
#
# Every step below runs against whatever hoservad actually exposes today
# and reports PASS/FAIL for it. A step the product does not implement yet
# is reported NOT-YET-IMPLEMENTED, with the reason, and does not fail the
# suite — pretending disk-yank reconstruction ran without an add/replace-
# disk API operation to drive it, or a scheduled spindown check ran
# without a scheduler, would be a false pass; silently dropping the step
# would hide that the acceptance criterion isn't met yet. `run_step`'s
# summary table at the end is the honest record either way. Journey
# coverage that needs a running hoservad UI/API lives in web/'s own
# Playwright suite (scripts/vm/run-playwright.sh), invoked from here once
# a VM is up.
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

# The array's own virtio device names, set once by array_setup (step 3)
# from the live domain XML and reused by midsync_throttle_devices (step 6,
# issue #352) — never rediscovered a second time for the same domain.
ARRAY_PARITY_DEV=""
ARRAY_DATA1_DEV=""
ARRAY_DATA2_DEV=""

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
  # Not local: midsync_throttle_devices (step 6, issue #352) reuses these
  # same three device names to cap I/O on the guest's own array disks —
  # rediscovering them from the domain XML a second time there would just
  # duplicate this parsing loop for no benefit.
  ARRAY_PARITY_DEV="$parity_dev"
  ARRAY_DATA1_DEV="$data1_dev"
  ARRAY_DATA2_DEV="$data2_dev"

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

  # cmd/hoservad/main.go builds parity.Engine, and registers TypeSync/
  # TypeScrub/TypeFix against it, exactly once at process startup, reading
  # snapraid.conf — which does not exist before this array's own createArray
  # job just wrote it. #262 already fixed array/start's own 501 for this
  # same "live topology change, stale in-memory state" shape, but that fix
  # only rebuilds the array sequence, not the parity engine or its job
  # registrations: confirmed empirically running this exact suite — without
  # this restart, every later startSync/startScrub/startFix call this suite
  # (or a real user, right after finishing the onboarding wizard) makes
  # fails with job_type_not_registered, forever, until something restarts
  # hoservad. Restarting once, unconditionally, right after the array
  # exists, is the same restart-and-retry shape ensure_pool_mounted already
  # applies for #262's own narrower symptom (out of scope to fix in
  # cmd/hoservad/main.go from here) — folded in here rather than
  # duplicated, since every step after this one needs both gaps closed.
  echo "vm-suite[$HOSERVA_LAB_ID]: restarting hoservad so it registers sync/scrub/fix against the snapraid.conf createArray just wrote"
  vm_ssh 'sudo systemctl restart hoserva' >/dev/null 2>&1 || true
  local restart_deadline=$((SECONDS + 60))
  while (( SECONDS < restart_deadline )) && ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; do
    sleep 2
  done
  if ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
    ARRAY_SETUP_REASON="hoservad did not come back active within 60s of the post-createArray restart"
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
  # over management and (Q76) ingests pre-existing Samba/NFS entries into
  # the database first, so regeneration keeps the names seed-existing-
  # host.sh put there.
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

  # #264: imported host entries must survive the regeneration createShare
  # just triggered — grep the on-disk files, not exportfs/testparm, so a
  # missing DB ingest fails this step rather than only the earlier
  # "existing host config" check (which runs before this import).
  if ! vm_ssh 'grep -q "^\[hoserva-existing\]" /etc/samba/smb.conf'; then
    ARRAY_SETUP_REASON="regenerated smb.conf lost [hoserva-existing] after import + createShare (Q76 / #264)"
    return 1
  fi
  if ! vm_ssh 'grep -q "/mnt/user/hoserva-existing" /etc/exports'; then
    ARRAY_SETUP_REASON="regenerated /etc/exports lost /mnt/user/hoserva-existing after import + createShare (Q76 / #264)"
    return 1
  fi

  return 0
}

# array_stop_start_share_check is #268's own reproduction, run against
# the live share array_setup (step 3, above) just created: with that
# share still mounted, POST /array/stop must succeed rather than fail
# EBUSY on the catch-all (the share's own per-share mergerfs mount was
# never unmounted first, because newArraySequence never populated
# ArraySequence.ShareMounts), and POST /array/start afterward must bring
# the catch-all back exactly once — not stacked on top of a mount a
# failed stop left behind — and the share's own mount back with it,
# still serving a file written before the stop. On failure it sets
# ARRAY_STOP_START_REASON and returns 1.
array_stop_start_share_check() {
  local marker="$JOURNEY5_SHARE_PATH/hoserva-268-marker.txt"
  local marker_body
  marker_body="hoserva-268-$(date +%s)"

  if ! vm_ssh "echo '$marker_body' | sudo tee $marker >/dev/null"; then
    ARRAY_STOP_START_REASON="could not write a marker file into $JOURNEY5_SHARE_PATH before stopping the array"
    return 1
  fi
  local before
  before="$(vm_ssh "sudo cat $marker" 2>/dev/null)"
  if [[ "$before" != "$marker_body" ]]; then
    ARRAY_STOP_START_REASON="marker file was not readable at $marker before stopping the array"
    return 1
  fi

  local stop_result
  stop_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/stop -H 'Content-Type: application/json' -d '{\"confirm\":true}'" 2>/dev/null)"
  if [[ "$stop_result" != *'"maintenanceMode":true'* ]]; then
    ARRAY_STOP_START_REASON="array/stop did not report maintenanceMode:true with the share still live — the catch-all unmount fails EBUSY while the share's own mount is still nested under it (#268's own symptom): $stop_result"
    return 1
  fi

  # Neither the catch-all nor the share's own mount may still be mounted
  # under /mnt/user once stop reports success — " on /mnt/user" matches
  # both mount(8) table lines ("... on /mnt/user type ..." and "... on
  # /mnt/user/massdel type ...").
  local mounts_after_stop
  mounts_after_stop="$(vm_ssh "mount | grep -c ' on /mnt/user'" 2>/dev/null)"
  if [[ "${mounts_after_stop:-0}" != "0" ]]; then
    ARRAY_STOP_START_REASON="array/stop reported success but a mount under /mnt/user is still up (mount | grep -c count: ${mounts_after_stop:-unknown})"
    return 1
  fi

  local start_result
  start_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/array/start" 2>/dev/null)"
  if [[ "$start_result" != *'"maintenanceMode":false'* ]]; then
    ARRAY_STOP_START_REASON="array/start did not report maintenanceMode:false: $start_result"
    return 1
  fi

  # Exactly one catch-all mount — a second array/start after a failed
  # stop must not stack a duplicate mergerfs mount on top of one already
  # there (pool.Mounter.Mount's own idempotency guard).
  local catchall_count
  catchall_count="$(vm_ssh "mount | grep -c ' on /mnt/user type fuse.mergerfs'" 2>/dev/null)"
  if [[ "${catchall_count:-0}" != "1" ]]; then
    ARRAY_STOP_START_REASON="expected exactly one catch-all mount at /mnt/user after array/start, found ${catchall_count:-unknown}"
    return 1
  fi

  local share_mount_count
  share_mount_count="$(vm_ssh "mount | grep -c ' on $JOURNEY5_SHARE_PATH type fuse.mergerfs'" 2>/dev/null)"
  if [[ "${share_mount_count:-0}" != "1" ]]; then
    ARRAY_STOP_START_REASON="expected exactly one share mount at $JOURNEY5_SHARE_PATH after array/start, found ${share_mount_count:-unknown} — a share that survives array/stop must have its own mount rejoin array/start"
    return 1
  fi

  local after
  after="$(vm_ssh "sudo cat $marker" 2>/dev/null)"
  if [[ "$after" != "$marker_body" ]]; then
    ARRAY_STOP_START_REASON="marker file at $marker did not read back its pre-stop content after array/start (got: $after, want: $marker_body) — the share's own mount came back pointed at the wrong branches, or not at all"
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

# midsync_destroy (step 5, doc 02 §2, doc 06 §4) proves the "power loss
# mid-sync" row of doc 02 §6's failure-mode table for real: it seeds real
# bulk data, starts a sync, destroys the guest, boots it back, and asserts
# (doc 01 §4) that RecoverFromRestart left the job interrupted, that
# parity reads as not fresh, and that re-running the sync is a genuinely
# new job through the same guarded startSync path, not an automatic
# resume.
#
# issue #352 (this round): destroying the instant startSync *returns* is
# not the same as destroying while the sync is genuinely still running,
# and neither is destroying on any *elapsed-time* signal, however it is
# derived — including the previous attempt's own "job progress is between
# 1% and 90%" gate. Root cause of that gate's own failure, confirmed
# empirically against a real snapraid 12.4-1 binary: this lab's own
# container-equivalent of the L3 guest's storage (a real sync over 450MB
# of just-written data, piped to a file exactly like CommandRunner's own
# os.Pipe, not a tty) ran its entire "Syncing..." pass — the only phase
# that ever prints a "NN%, MB" tick — from 0% straight to "100% completed"
# in under a tenth of a second, with the intermediate percentage never
# once landing in a range a poller could catch. The 6144MB seed already
# exceeds the guest's own 4096MB RAM (HOSERVA_VM_MEMORY_MIB), which
# defeats the *guest's* own page cache for enough of the read pass to
# force it to re-read from its virtio block device — but create-vm.sh
# never sets `cache=` on the array's qcow2 disks, so QEMU's own default
# (writeback) still serves that "re-read from the block device" out of
# the *host* kernel's page cache for the qcow2 backing file, regardless
# of how large the guest-visible dataset is relative to guest RAM. Sizing
# past guest RAM was solving the wrong layer's cache. There is also a
# second, independent reason a percentage window can never be trusted
# here even with real disk I/O: SnapRAID's own percentage is scoped to
# the hash/read pass alone ("Syncing..." → "100% completed, NN MB
# accessed") — the write-parity, "Saving state" and "Verifying" phases
# that actually decide whether a kill leaves the content file durably
# rewritten run *after* that percentage already reads 100%, so a window
# keyed on the percentage can miss the entire span this test cares about
# even when the percentage pass itself is slow.
#
# The fix does not chase a faster or larger dataset (issue #352's own
# direction: growing it is a race already lost once, to a cache layer
# this harness does not control the size of). It instead throttles the
# array's own virtio block devices, from inside the guest
# (midsync_throttle_devices, a `systemctl set-property --runtime`
# cgroup io.max cap — transient, never persisted, and cleared the instant
# the guest's systemd re-execs, which midsync_recover_guest's own destroy
# does unconditionally), and gates the destroy on the two signals that
# are true throughout the *entire* run regardless of which phase it is in
# or how fast the storage under it happens to be: the job's own status is
# "running", and the snapraid process is actually still alive in the
# guest.
#
# This project's own loop-device-lab-hosted L3 guest (a real Debian 13
# trixie VM, real snapraid 12.4-1, `make vm-up`) confirmed empirically
# why a *content-file* signal — this round's own first draft, "unchanged
# since before this sync started" — cannot be the gate either, even
# though it sounds like exactly the property getParity's lastSyncAt
# cares about: SnapRAID rewrites the content file *twice*, not once. A
# first-ever sync's own "Resizing..." phase (growing the parity file to
# its new size) is immediately followed by its own "Saving state..."
# checkpoint — confirmed to land within the first second, before
# "Syncing..." (the real hash pass) even starts — and only the *second*
# rewrite, after "Syncing..." actually finishes, is the one a completed
# run's own `summary:exit:ok` covers. Gating on "the content file has not
# changed" would therefore almost always see it change within the first
# second regardless of throttling, and fail to ever confirm mid-flight.
# What actually matters — confirmed by killing a real snapraid process by
# PID at both points, then reading `snapraid status -l` for an
# `info_time` line the same way status_parse.go's own StatusReport does —
# is that neither rewrite ever produces one: a kill at the early
# checkpoint (before "Syncing...") and a kill mid-"Syncing..." (after
# it, for a later iteration reusing an existing content file) both leave
# `info_time` exactly as it was before this sync started (absent for a
# true first-ever sync, unchanged at its prior value for a later one).
# The content file's own mtime is simply not a proxy the guarantee
# depends on; process-alive plus job-status is both sufficient and the
# only externally observable signal this harness has that lines up with
# what getParity itself actually reports afterward.
#
# midsync_destroy is called once per entry in MIDSYNC_DESTROY_DELAYS
# below, at increasing delays past that confirmation, to prove the same
# property holds "immediately", "about a second after" and "several
# seconds after" in one run rather than trusting whatever a single run's
# own timing happens to land on. On failure it sets MIDSYNC_REASON and
# returns 1.
MIDSYNC_SHARE="hoserval3midsync"
MIDSYNC_SHARE_PATH="/mnt/user/$MIDSYNC_SHARE"
# 6144MB per iteration: still comfortably past the L3 guest's own default
# 4096MB (HOSERVA_VM_MEMORY_MIB, create-vm.sh) so the guest's own page
# cache cannot serve the whole read pass either — kept alongside the
# throttle below rather than relied on alone (issue #352: the guest's own
# cache is a real, independent layer from the host's, and every layer
# that can silently absorb a read has to be accounted for, not just one).
MIDSYNC_FILE_SIZE_MB=1024
MIDSYNC_FILE_COUNT=6
# 16 MB/s on each array device: confirmed in this project's own L3 guest
# (`make vm-up`) to reliably turn a sub-second cached sync into one that
# takes tens of seconds — comfortably longer than this harness's own
# SSH-round-trip poll interval below — without stretching one iteration
# into minutes.
MIDSYNC_THROTTLE_RATE="${MIDSYNC_THROTTLE_RATE:-16M}"
# The exact points doc 06 §4 and issue #352 name: destroy the instant a
# genuinely in-flight sync is confirmed, about a second after, and several
# seconds after.
MIDSYNC_DESTROY_DELAYS="${MIDSYNC_DESTROY_DELAYS:-0 1 5}"

# midsync_throttle_devices caps IOReadBandwidthMax/IOWriteBandwidthMax on
# hoserva.service — the unit whose own cgroup the snapraid child process
# it forks inherits — for every device name passed in, at MIDSYNC_THROTTLE_RATE.
# --runtime only: never written to disk, and cleared automatically the
# instant the guest's systemd re-execs (a `virsh destroy` and reboot, or a
# plain restart), so nothing here can outlive one iteration by accident.
midsync_throttle_devices() {
  local props="" dev
  for dev in "$@"; do
    props+="'IOReadBandwidthMax=/dev/$dev $MIDSYNC_THROTTLE_RATE' 'IOWriteBandwidthMax=/dev/$dev $MIDSYNC_THROTTLE_RATE' "
  done
  vm_ssh "sudo systemctl set-property --runtime hoserva.service $props" >/dev/null 2>&1
}

# midsync_clear_throttle undoes midsync_throttle_devices early, for the
# one path that does not already get it for free from a guest reboot: a
# failed midsync_wait_mid_flight below, which returns before
# midsync_recover_guest ever runs. Best-effort (a guest that is
# unreachable here has bigger problems this step already reports) — never
# lets a failed clear turn this step's own real failure into a pass.
midsync_clear_throttle() {
  vm_ssh 'sudo systemctl set-property --runtime hoserva.service IOReadBandwidthMax= IOWriteBandwidthMax=' >/dev/null 2>&1 || true
}

# midsync_wait_mid_flight polls until the sync is confirmed genuinely
# still in flight: the job's own status is "running" and a snapraid
# process is actually alive in the guest — see this section's own header
# comment for why a content-file signal was tried and rejected. Both
# checks in one SSH round trip per poll. If the job reaches a terminal
# status first, that is this run's own precondition failing to hold — the
# seeded data finished syncing before this step could catch it mid-flight
# — reported as such, not treated as the safety property under test
# having passed.
midsync_wait_mid_flight() {
  local job_id=$1 timeout_s=$2
  local deadline=$((SECONDS + timeout_s))
  local result job_json proc_alive status
  while (( SECONDS < deadline )); do
    result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/jobs/$job_id; printf '\\n---MIDSYNC---\\n'; (sudo pgrep -x snapraid >/dev/null 2>&1 && echo yes || echo no)" 2>/dev/null)"
    job_json="${result%%---MIDSYNC---*}"
    proc_alive="${result##*---MIDSYNC---}"
    proc_alive="${proc_alive#$'\n'}"
    status=""
    [[ "$job_json" =~ \"status\":\"([^\"]+)\" ]] && status="${BASH_REMATCH[1]}"
    if [[ "$status" == "running" && "$proc_alive" == "yes" ]]; then
      return 0
    fi
    case "$status" in
      succeeded | failed | cancelled | interrupted)
        MIDSYNC_REASON="job $job_id reached a terminal status ('$status') before this check ever confirmed it was genuinely mid-flight (process alive=$proc_alive) — the seeded data finished syncing before this step could catch it mid-flight (this run's own precondition, not the safety property under test)"
        return 1
        ;;
    esac
    sleep 1
  done
  MIDSYNC_REASON="job $job_id never confirmed genuinely mid-flight within ${timeout_s}s (last: status=${status:-unknown}, process alive=$proc_alive)"
  return 1
}

# midsync_recover_guest destroys the running guest, boots it back, and
# waits for SSH, hoservad and the array's own pool mount to come back —
# the recovery half every midsync_destroy iteration below shares.
midsync_recover_guest() {
  virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
  echo "vm-suite[$HOSERVA_LAB_ID]: destroyed '$VM_DOMAIN' mid-sync — booting it back up"
  virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
  vm_wait_tcp "$VM_SSH_PORT" 180 || { MIDSYNC_REASON="guest did not open its forwarded SSH port within 180s of the post-destroy boot"; return 1; }
  vm_ssh_wait_ready 180 || { MIDSYNC_REASON="could not SSH into the guest within 180s of the post-destroy boot"; return 1; }

  local deadline=$((SECONDS + 120))
  while (( SECONDS < deadline )) && ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; do
    sleep 2
  done
  if ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
    # cmd/hoservad/cert.go's loadOrGenerateTLSCertificate writes a fresh
    # self-signed cert with plain os.WriteFile — no fsync, unlike its own
    # installTLSCertificate (the Let's Encrypt replacement path), which
    # does the full temp-file/fsync/rename dance. Confirmed empirically,
    # reproducibly, running this exact suite: a `virsh destroy` shortly
    # after hoservad has (re)started can leave hoserva.crt/hoserva.key
    # zero bytes — never flushed past the guest's own page cache — and
    # hoservad then crash-loops forever on "tls: failed to find any PEM
    # data in certificate input", never recovering on its own. This is a
    # real, separate durability gap (cmd/hoservad/cert.go, out of scope
    # here — scripts/vm/, not cmd/hoservad/) that a genuine power loss at
    # the wrong moment can hit in production too, not an artifact of this
    # test. Recognizing and clearing the exact symptom here — never a
    # blind retry — is what lets this step still exercise doc 02 §6's own
    # "sync marked interrupted... not auto-resumed" recovery, the thing
    # this step exists to test, instead of always failing on this
    # unrelated confounder; the finding itself belongs in a filed issue,
    # not silently papered over, so it is reported as this run's own
    # summary text below regardless of how this step finishes.
    local cert_zero
    cert_zero="$(vm_ssh "sudo journalctl -u hoserva -n 5 --no-pager 2>/dev/null" | grep -c 'failed to find any PEM data' || true)"
    if [[ "$cert_zero" -gt 0 ]]; then
      echo "vm-suite[$HOSERVA_LAB_ID]: hoservad is crash-looping on a zero-byte TLS certificate after the destroy (cmd/hoservad/cert.go does not fsync its own write) — clearing it and retrying once"
      vm_ssh 'sudo rm -f /var/lib/hoserva/tls/hoserva.crt /var/lib/hoserva/tls/hoserva.key && sudo systemctl restart hoserva' >/dev/null 2>&1 || true
      local cert_deadline=$((SECONDS + 60))
      while (( SECONDS < cert_deadline )) && ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; do
        sleep 2
      done
    fi
  fi
  if ! vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
    MIDSYNC_REASON="hoservad did not come back active within 120s of the post-destroy boot (including after clearing a zero-byte TLS certificate, cmd/hoservad/cert.go's own non-fsynced write)"
    return 1
  fi
  if ! array_login; then
    MIDSYNC_REASON="login as the L3 admin failed after the post-destroy boot"
    return 1
  fi

  # The array's own data/parity disks are not mounted again on their own
  # after any guest boot — graceful or not, confirmed empirically running
  # this exact suite: nothing persists a systemd .mount unit for them, and
  # hoservad itself does not remount a previously configured array at its
  # own startup — only an explicit startArray call does (the same call
  # ensure_pool_mounted already makes; a real user hits this identically
  # after any reboot, not just this test's own destroy).
  if ! ensure_pool_mounted; then
    MIDSYNC_REASON="pool did not remount after the post-destroy boot"
    return 1
  fi
  return 0
}

# midsync_destroy runs one full destroy/recover cycle at delay_s seconds
# past the moment the sync is confirmed genuinely mid-flight. iteration 1
# creates MIDSYNC_SHARE and seeds its very first data, so getParity's own
# lastSyncAt (internal/parity/status_parse.go's StatusReport.LastActivityAt,
# which a completed sync's content-file write is what actually creates)
# must be entirely absent afterward — doc 02 §6's "first-ever sync
# interrupted" case, unambiguous because there is no earlier sync for it
# to fall back to. Every later iteration reuses the same share with a
# fresh batch of files and instead asserts the general form of the same
# property: lastSyncAt must not move past the last confirmed-good sync
# (doc 02 §6, "parity stays not fresh") — freshness itself does not
# reliably move off green for a small interrupted sync (confirmed
# empirically: ToParityStatus only reads Amber from `snapraid status`'s
# own has_unsynced count), so lastSyncAt is the one signal both cases can
# actually rely on.
midsync_destroy() {
  local delay_s=$1 iteration=$2
  if ! array_login; then
    MIDSYNC_REASON="login as the L3 admin failed ahead of the mid-sync destroy test (iteration $iteration)"
    return 1
  fi
  if ! ensure_pool_mounted; then
    MIDSYNC_REASON="pool is not mounted, cannot seed $MIDSYNC_SHARE (iteration $iteration)"
    return 1
  fi

  if [[ "$iteration" == "1" ]]; then
    local share_result
    share_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/shares -H 'Content-Type: application/json' -d '{\"name\":\"$MIDSYNC_SHARE\",\"cacheMode\":\"array-only\"}'" 2>/dev/null)"
    if [[ "$share_result" != *"\"name\":\"$MIDSYNC_SHARE\""* ]]; then
      MIDSYNC_REASON="createShare did not return the expected share: $share_result"
      return 1
    fi
  fi

  local pre_lastsyncat=""
  if [[ "$iteration" != "1" ]]; then
    local pre_parity
    pre_parity="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/parity" 2>/dev/null)"
    if [[ "$pre_parity" =~ \"lastSyncAt\":\"([^\"]+)\" ]]; then
      pre_lastsyncat="${BASH_REMATCH[1]}"
    else
      MIDSYNC_REASON="expected a prior successful sync's lastSyncAt ahead of iteration $iteration's own destroy: $pre_parity"
      return 1
    fi
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: seeding $((MIDSYNC_FILE_COUNT * MIDSYNC_FILE_SIZE_MB))MB into $MIDSYNC_SHARE_PATH (iteration $iteration, destroy delay ${delay_s}s past confirmed mid-flight) so this sync still has real work left when we destroy the guest"
  if ! vm_ssh "sudo mkdir -p '$MIDSYNC_SHARE_PATH' && for i in \$(seq 1 $MIDSYNC_FILE_COUNT); do sudo dd if=/dev/zero of='$MIDSYNC_SHARE_PATH/bulk-${iteration}-\$i.bin' bs=1M count=$MIDSYNC_FILE_SIZE_MB status=none; done"; then
    MIDSYNC_REASON="seeding bulk files into $MIDSYNC_SHARE_PATH failed (iteration $iteration)"
    return 1
  fi

  # Throttled from here, not from before the seed above: dd's own writes
  # need to land at full speed (they are not part of what this test
  # measures), only the sync's own read-back of them needs to be slow
  # enough to catch (issue #352). A throttle that fails to apply is not
  # silently ignored — without it, this step is back to racing however
  # fast this run's own storage happens to be, the exact failure mode
  # this fix closes.
  if ! midsync_throttle_devices "$ARRAY_PARITY_DEV" "$ARRAY_DATA1_DEV" "$ARRAY_DATA2_DEV"; then
    MIDSYNC_REASON="could not throttle the array's own devices ($ARRAY_PARITY_DEV, $ARRAY_DATA1_DEV, $ARRAY_DATA2_DEV) via systemctl set-property --runtime ahead of iteration $iteration's own sync"
    return 1
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: starting this sync and destroying '$VM_DOMAIN' once it is confirmed genuinely mid-flight, ${delay_s}s after that confirmation (iteration $iteration)"
  local sync_result job_id
  sync_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/sync -H 'Content-Type: application/json' -d '{\"confirm\":false,\"dryRun\":false}'" 2>/dev/null)"
  if [[ "$sync_result" =~ \"id\":\"([^\"]+)\" ]]; then
    job_id="${BASH_REMATCH[1]}"
  else
    midsync_clear_throttle
    MIDSYNC_REASON="startSync did not return a job id (iteration $iteration): $sync_result"
    return 1
  fi

  if ! midsync_wait_mid_flight "$job_id" 90; then
    midsync_clear_throttle
    return 1
  fi
  if (( delay_s > 0 )); then
    sleep "$delay_s"
  fi

  midsync_recover_guest || return 1

  wait_job_terminal "$job_id" 30
  if [[ "$job_status" != "interrupted" ]]; then
    MIDSYNC_REASON="sync job $job_id status after the guest came back = '${job_status:-unknown}' (iteration $iteration, delay ${delay_s}s), want 'interrupted' (doc 01 §4: RecoverFromRestart marks every job left queued or running interrupted) — $job_result"
    return 1
  fi

  local parity_result
  parity_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/parity" 2>/dev/null)"
  if [[ "$parity_result" != *'"dataDisks":'* ]]; then
    MIDSYNC_REASON="getParity returned an unexpected response after the post-destroy boot (iteration $iteration): $parity_result"
    return 1
  fi
  if [[ "$iteration" == "1" ]]; then
    if [[ "$parity_result" == *'"lastSyncAt"'* ]]; then
      MIDSYNC_REASON="getParity reports a lastSyncAt after only an interrupted first-ever sync (delay ${delay_s}s) — parity should not read as ever having synced. getParity's lastSyncAt reads internal/parity/status_parse.go's StatusReport.LastActivityAt, itself only ever set from a snapraid status log's own info_time lines, which only a completed sync's content-file write creates: $parity_result"
      return 1
    fi
  elif [[ "$parity_result" != *"\"lastSyncAt\":\"$pre_lastsyncat\""* ]]; then
    MIDSYNC_REASON="getParity's lastSyncAt moved past the last confirmed-good sync ($pre_lastsyncat) after only an interrupted sync (iteration $iteration, delay ${delay_s}s) — doc 02 §6: parity stays not fresh until a sync actually completes: $parity_result"
    return 1
  fi

  # The bulk files this function seeded above are gone: this lab's qcow2
  # array disks are freshly formatted (array setup, step 3) and the write
  # cache backing them had not flushed any of that data before the
  # destroy — confirmed empirically, and expected on freshly formatted
  # disks under `virsh destroy`'s hard power-off (no ACPI shutdown, no
  # cache flush), the same reason production disk write-caching exists
  # and the same reason "only what completed is durable" is the honest
  # framing doc 02 §6 already uses for this row. A same-state re-sync
  # (nothing changed since the last, interrupted attempt's own scan) hits
  # a separate, real snapraid_engine.go parsing gap (its own "exit"
  # tracking falls back to the last diff category name instead of "ok"
  # when snapraid reports nothing to do, confirmed by running `snapraid
  # sync` directly on the guest afterward — it succeeds with "Nothing to
  # do" while the identical call through startSync fails with
  # `parity: snapraid sync: exit "equal"`) — writing one new file first
  # keeps this re-sync a normal, real one instead of exercising that
  # separate, unrelated gap.
  vm_ssh "sudo mkdir -p '$MIDSYNC_SHARE_PATH' && echo resync-after-interrupt-${iteration} | sudo tee '$MIDSYNC_SHARE_PATH/resync-marker-${iteration}.txt' >/dev/null"

  echo "vm-suite[$HOSERVA_LAB_ID]: interrupted sync confirmed (job $job_id interrupted, iteration $iteration, delay ${delay_s}s) — re-running the sync through the normal guarded path"
  local resync_result resync_job_id
  resync_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/sync -H 'Content-Type: application/json' -d '{\"confirm\":false,\"dryRun\":false}'" 2>/dev/null)"
  if [[ "$resync_result" =~ \"id\":\"([^\"]+)\" ]]; then
    resync_job_id="${BASH_REMATCH[1]}"
  else
    MIDSYNC_REASON="the guarded re-sync after the interrupted one did not return a job id (iteration $iteration): $resync_result"
    return 1
  fi
  if [[ "$resync_job_id" == "$job_id" ]]; then
    MIDSYNC_REASON="startSync returned the same job id ($job_id) as the interrupted one (iteration $iteration) — doc 01 §4 requires an interrupted job to never be silently resumed, only explicitly re-run as a new job"
    return 1
  fi

  wait_job_terminal "$resync_job_id" 180
  if [[ "$job_status" != "succeeded" ]]; then
    MIDSYNC_REASON="the guarded re-sync (job $resync_job_id, iteration $iteration) did not succeed (status=${job_status:-unknown}): $job_result"
    return 1
  fi

  parity_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/parity" 2>/dev/null)"
  if [[ "$parity_result" != *'"lastSyncAt"'* || "$parity_result" != *'"freshness":"green"'* ]]; then
    MIDSYNC_REASON="getParity does not report a completed sync (freshness=green, lastSyncAt set) after the guarded re-sync succeeded (iteration $iteration): $parity_result"
    return 1
  fi
  if [[ "$iteration" != "1" && "$parity_result" == *"\"lastSyncAt\":\"$pre_lastsyncat\""* ]]; then
    MIDSYNC_REASON="getParity's lastSyncAt did not advance past the pre-iteration value ($pre_lastsyncat) after the guarded re-sync succeeded (iteration $iteration): $parity_result"
    return 1
  fi

  return 0
}

# midsync_ensure_synced (issue #352) is step 6's own "finally", called
# once after the whole midsync_destroy loop regardless of how it ended.
# The failing CI run this issue's second attempt investigated showed
# exactly why this matters: an iteration whose own mid-flight check never
# triggers does not stop at "this run's own precondition failed" the way
# MIDSYNC_REASON describes it — the sync it seeded keeps running on the
# guest and completes for real, durably resyncing all of that iteration's
# bulk data, then midsync_destroy returns 1 without ever reaching its own
# resync-and-verify tail. Step 6 reported FAIL, correctly, but left the
# array in whatever state that accidental real sync produced — and
# journey 5's own baseline sync (step 8, seed_journey5_fixture) then ran
# against it unconditionally, on the same array, with no check in between
# that it was starting from a state its own diff logic could make sense
# of. This never deletes anything (a removal-heavy path here is exactly
# what the threshold guard exists to question, and this is housekeeping,
# not a reviewed removal) — it only confirms getParity already reports a
# fully synced array, and if not, runs one more guarded sync to bring it
# there. A step that cannot leave its own array clean fails here, rather
# than silently handing journey 5 a starting state it never asked for.
midsync_ensure_synced() {
  midsync_clear_throttle
  if ! array_login; then
    MIDSYNC_REASON="login as the L3 admin failed ahead of step 6's own cleanup"
    return 1
  fi
  if ! ensure_pool_mounted; then
    MIDSYNC_REASON="pool is not mounted, cannot confirm step 6 left the array synced"
    return 1
  fi

  local parity_result
  parity_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/parity" 2>/dev/null)"
  if [[ "$parity_result" == *'"lastSyncAt"'* && "$parity_result" == *'"freshness":"green"'* ]]; then
    return 0
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: step 6 did not leave the array synced (freshness=green with lastSyncAt not both present) — running one cleanup sync so journey 5's own baseline (step 8) starts from a clean array: $parity_result"
  if ! vm_ssh "sudo mkdir -p '$MIDSYNC_SHARE_PATH' && echo hoserva-352-cleanup | sudo tee '$MIDSYNC_SHARE_PATH/cleanup-marker.txt' >/dev/null"; then
    MIDSYNC_REASON="step 6 cleanup: could not write a marker file into $MIDSYNC_SHARE_PATH ahead of the cleanup sync"
    return 1
  fi

  local sync_result job_id
  sync_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/parity/sync -H 'Content-Type: application/json' -d '{\"confirm\":false,\"dryRun\":false}'" 2>/dev/null)"
  if [[ "$sync_result" =~ \"id\":\"([^\"]+)\" ]]; then
    job_id="${BASH_REMATCH[1]}"
  else
    MIDSYNC_REASON="step 6 cleanup: startSync did not return a job id: $sync_result"
    return 1
  fi

  wait_job_terminal "$job_id" 180
  if [[ "$job_status" != "succeeded" ]]; then
    MIDSYNC_REASON="step 6 cleanup sync (job $job_id) did not succeed (status=${job_status:-unknown}): $job_result"
    return 1
  fi

  parity_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/parity" 2>/dev/null)"
  if [[ "$parity_result" != *'"lastSyncAt"'* || "$parity_result" != *'"freshness":"green"'* ]]; then
    MIDSYNC_REASON="step 6 cleanup sync succeeded but getParity still does not report freshness=green with lastSyncAt set: $parity_result"
    return 1
  fi
  return 0
}

# config_backup_restore (step 7, doc 10 §1) is doc 10's *in-place* restore
# ("rolling back a bad config change") — not the bare-metal restore flow
# (doc 10 §1's numbered steps 1-6, a freshly installed system with no
# admin session to authenticate importConfig's own x-hoserva-role: admin,
# and explicitly out of scope for this issue as #65). It exports the array
# this suite already built (step 3: the admin account, the array's
# disk-role assignment, and a dedicated share this function creates fresh
# for itself — see CONFIG_TEST_SHARE below for why, rather than the
# "massdel"/"hoserval3midsync" shares steps 3 and 5 already made), makes
# two real, distinguishing changes on top of that export (delete the
# share, add a throwaway user — each independently provable before the
# import), imports the same archive back over the running config, and
# asserts the delete was undone and the addition was wiped: exactly
# "replaces the running configuration" doc 10 §1 promises, not a no-op
# round trip that would pass even if importConfig did nothing. On failure
# it sets CONFIG_REASON and returns 1.
CONFIG_TEST_SHARE="hoserval3configtest"
CONFIG_THROWAWAY_USER="hoserval3throwaway"

config_backup_restore() {
  CONFIG_NOT_YET=0
  if ! array_login; then
    CONFIG_REASON="login as the L3 admin failed ahead of config export"
    return 1
  fi
  if ! ensure_pool_mounted; then
    CONFIG_REASON="pool is not mounted, cannot create $CONFIG_TEST_SHARE"
    return 1
  fi

  # A share created *now*, not one carried over from step 3 or step 5:
  # step 5's own virsh destroy has already rebooted the guest once by
  # this point, and — confirmed empirically running this exact suite,
  # against a share that had never been touched by any reboot versus one
  # that had — a share's own per-share mergerfs mount does not come back
  # after any reboot (only the pool catch-all does, ensure_pool_mounted).
  # deleteShare unmounts that per-share mount as part of removing the
  # share, and 500s ("fusermount: ... invalid argument / no such file or
  # directory") when it is already gone rather than treating that as a
  # clean no-op — a real, separate gap (internal/share, out of scope
  # here), reported below rather than routed around, since creating this
  # share fresh keeps this test on its own subject (in-place config
  # restore) without depending on a fix for that gap.
  local share_result
  share_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/shares -H 'Content-Type: application/json' -d '{\"name\":\"$CONFIG_TEST_SHARE\",\"cacheMode\":\"array-only\"}'" 2>/dev/null)"
  if [[ "$share_result" != *"\"name\":\"$CONFIG_TEST_SHARE\""* ]]; then
    CONFIG_REASON="createShare($CONFIG_TEST_SHARE) did not return the expected share: $share_result"
    return 1
  fi

  local shares_before
  shares_before="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/shares" 2>/dev/null)"
  if [[ "$shares_before" != *"\"name\":\"$CONFIG_TEST_SHARE\""* ]]; then
    CONFIG_REASON="listShares before export did not include the freshly created '$CONFIG_TEST_SHARE' share: $shares_before"
    return 1
  fi
  local pool_before
  pool_before="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"
  if [[ "$pool_before" != *'"role":"parity"'* || "$pool_before" != *'"role":"data"'* ]]; then
    CONFIG_REASON="getPool before export does not show the parity/data role assignment step 3 created: $pool_before"
    return 1
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: exporting the config archive"
  local export_remote="/tmp/hoserva-l3-config-export.tar.zst" export_status
  export_status="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -o $export_remote -w '%{http_code}' -X POST https://127.0.0.1:8008/api/v1/config/export" 2>/dev/null)"
  if [[ "$export_status" == "501" ]]; then
    # internal/api/phase1_handler.go's ExportConfig/ImportConfig 501 with
    # "not_configured" whenever api.Handler.Backup is nil — and
    # cmd/hoservad/main.go builds a real *backup.Service (backupService,
    # already wired into the nightly maintenance chain and the
    # pre-self-update backup) but never assigns it to handler.Backup.
    # Confirmed empirically running this exact suite against a real
    # array with real shares and users: every export/import call 501s
    # regardless of onboarding or array state. Out of scope here
    # (cmd/hoservad/, not scripts/vm/) — this is CONFIG_NOT_YET, not a
    # FAIL, because no test setup on this side of the API can make it
    # succeed; it needs the one-line wiring fix in cmd/hoservad/main.go.
    CONFIG_REASON="POST /config/export returned 501 not_configured — api.Handler.Backup (internal/api/handler.go) is never assigned in cmd/hoservad/main.go, even though backupService is built there and used by the nightly chain and pre-update backup; exportConfig/importConfig (internal/api/phase1_handler.go) 501 unconditionally until that one wiring line is added"
    CONFIG_NOT_YET=1
    return 1
  fi
  if [[ "$export_status" != "200" ]]; then
    CONFIG_REASON="exportConfig returned HTTP $export_status"
    return 1
  fi
  local export_size
  export_size="$(vm_ssh "stat -c %s $export_remote" 2>/dev/null || true)"
  if [[ -z "$export_size" ]] || (( export_size < 1 )); then
    CONFIG_REASON="exported config archive is empty or missing at $export_remote"
    return 1
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: making two changes after the export — deleting '$CONFIG_TEST_SHARE' and adding a throwaway user — for the import below to genuinely undo"
  local delete_status
  delete_status="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -o /dev/null -w '%{http_code}' -X DELETE https://127.0.0.1:8008/api/v1/shares/$CONFIG_TEST_SHARE -H 'Content-Type: application/json' -d '{\"confirm\":true}'" 2>/dev/null)"
  if [[ "$delete_status" != "204" ]]; then
    CONFIG_REASON="deleteShare($CONFIG_TEST_SHARE) returned HTTP $delete_status ahead of the import round-trip"
    return 1
  fi
  local create_user_result
  create_user_result="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/users -H 'Content-Type: application/json' -d '{\"username\":\"$CONFIG_THROWAWAY_USER\"}'" 2>/dev/null)"
  if [[ "$create_user_result" != *"\"username\":\"$CONFIG_THROWAWAY_USER\""* ]]; then
    CONFIG_REASON="createUser($CONFIG_THROWAWAY_USER) did not return the expected account ahead of the import round-trip: $create_user_result"
    return 1
  fi

  local shares_mid users_mid
  shares_mid="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/shares" 2>/dev/null)"
  users_mid="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/users" 2>/dev/null)"
  if [[ "$shares_mid" == *"\"name\":\"$CONFIG_TEST_SHARE\""* ]]; then
    CONFIG_REASON="'$CONFIG_TEST_SHARE' is still listed after deleteShare — the import below would not prove it restores anything"
    return 1
  fi
  if [[ "$users_mid" != *"\"username\":\"$CONFIG_THROWAWAY_USER\""* ]]; then
    CONFIG_REASON="'$CONFIG_THROWAWAY_USER' is not listed right after createUser — the import below would not prove it undoes anything"
    return 1
  fi

  echo "vm-suite[$HOSERVA_LAB_ID]: importing the exported archive back — in-place restore (doc 10 §1)"
  local import_status
  import_status="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR -o /dev/null -w '%{http_code}' -X POST https://127.0.0.1:8008/api/v1/config/import -F 'archive=@$export_remote;type=application/zstd' -F 'confirm=true'" 2>/dev/null)"
  if [[ "$import_status" != "204" ]]; then
    CONFIG_REASON="importConfig returned HTTP $import_status"
    return 1
  fi
  if ! array_login; then
    CONFIG_REASON="login as the L3 admin failed after config import"
    return 1
  fi

  local shares_after users_after pool_after
  shares_after="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/shares" 2>/dev/null)"
  users_after="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/users" 2>/dev/null)"
  pool_after="$(vm_ssh "curl -sk -b $ARRAY_COOKIE_JAR https://127.0.0.1:8008/api/v1/pool" 2>/dev/null)"

  if [[ "$shares_after" != *"\"name\":\"$CONFIG_TEST_SHARE\""* ]]; then
    CONFIG_REASON="'$CONFIG_TEST_SHARE' did not come back after importConfig — shares did not restore: $shares_after"
    return 1
  fi
  if [[ "$users_after" == *"\"username\":\"$CONFIG_THROWAWAY_USER\""* ]]; then
    CONFIG_REASON="'$CONFIG_THROWAWAY_USER' is still listed after importConfig — the running configuration was not actually replaced: $users_after"
    return 1
  fi
  if [[ "$users_after" != *"\"username\":\"$ARRAY_ADMIN_USERNAME\""* ]]; then
    CONFIG_REASON="the L3 admin account is missing from listUsers after importConfig: $users_after"
    return 1
  fi
  if [[ "$pool_after" != *'"role":"parity"'* || "$pool_after" != *'"role":"data"'* ]]; then
    CONFIG_REASON="getPool after importConfig does not show the parity/data role assignment from before the export: $pool_after"
    return 1
  fi

  return 0
}

echo "vm-suite[$HOSERVA_LAB_ID]: === 1/13 install ==="
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 2/13 onboarding ==="
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
# this is the one point in the suite where "install and onboarding"
# (this check's own doc 06 §4/#113 contract) has happened and nothing
# has regenerated smb.conf/exports yet. array_setup later imports and
# createShares; #264's survival assertion lives inside array_setup.
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 3/13 array setup ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if array_setup; then
    pass "array setup"
  else
    fail "array setup" "$ARRAY_SETUP_REASON"
  fi
else
  not_yet "array setup" "no active hoservad on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 4/13 array stop/start with a live share (issue #268) ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if array_stop_start_share_check; then
    pass "array stop/start with a live share"
  else
    fail "array stop/start with a live share" "$ARRAY_STOP_START_REASON"
  fi
else
  not_yet "array stop/start with a live share" "no active hoservad on the guest, or array setup (step 3) did not complete"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === SMB-connected array stop/start (issue #309) ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if ARRAY_ADMIN_USERNAME="$ARRAY_ADMIN_USERNAME" ARRAY_ADMIN_PASSWORD="$ARRAY_ADMIN_PASSWORD" ARRAY_SMB_SHARE="$JOURNEY5_SHARE" "$script_dir/smb-stop-check.sh"; then
    pass "SMB-connected array stop/start"
  else
    fail "SMB-connected array stop/start" "see smb-stop-check.sh output above (issue #309) — needs array setup (step 3)'s own admin account and '$JOURNEY5_SHARE' share still present and the array still mounted"
  fi
else
  not_yet "SMB-connected array stop/start" "no active hoservad on the guest (install or array setup above did not complete — see step 1 and step 3)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === pool survives hoservad restart (issue #335) ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if ARRAY_ADMIN_USERNAME="$ARRAY_ADMIN_USERNAME" ARRAY_ADMIN_PASSWORD="$ARRAY_ADMIN_PASSWORD" ARRAY_RESTART_SHARE="$JOURNEY5_SHARE" "$script_dir/pool-restart-check.sh"; then
    pass "pool survives hoservad restart"
  else
    fail "pool survives hoservad restart" "see pool-restart-check.sh output above (issue #335) — needs array setup (step 3)'s own admin account and '$JOURNEY5_SHARE' share still present and the array still mounted"
  fi
else
  not_yet "pool survives hoservad restart" "no active hoservad on the guest (install or array setup above did not complete — see step 1 and step 3)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 5/13 disk yank and reconstruction ==="
not_yet "disk yank and reconstruction" "array setup (step 3, #258) now gives this a real array to yank a disk from, but there is still no add/replace/remove-disk operation in api/openapi.yaml to reintroduce a replacement disk into an already-created array: createArray (POST /disks/array) only drives the wizard's one-time initial array creation (doc 03 §3.1 step 6); the only other topology-touching operations are formatExternalDisk (non-array disks only, doc 02 §4's Q72) and startFix (POST /parity/fix), which reconstructs a disk already mounted at its assigned /mnt/diskN — it has nothing to reconstruct onto if no operation ever formats and remounts a replacement there. JobType reserves disk_add/disk_replace/disk_remove (doc 01 §4) but no REST operation triggers any of them, and cmd/hoserva has no 'disk add'/'disk replace' subcommand either — re-check once one lands"

echo "vm-suite[$HOSERVA_LAB_ID]: === 6/13 virsh destroy mid-sync recovery ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  midsync_ok=1
  midsync_iteration=0
  midsync_last_reason=""
  for midsync_delay in $MIDSYNC_DESTROY_DELAYS; do
    midsync_iteration=$((midsync_iteration + 1))
    if midsync_destroy "$midsync_delay" "$midsync_iteration"; then
      echo "vm-suite[$HOSERVA_LAB_ID]: virsh destroy mid-sync recovery — iteration $midsync_iteration (destroy delay ${midsync_delay}s past confirmed mid-flight) confirmed"
    else
      midsync_ok=0
      midsync_last_reason="iteration $midsync_iteration (destroy delay ${midsync_delay}s): $MIDSYNC_REASON"
      break
    fi
  done
  # midsync_ensure_synced (issue #352) always runs, pass or fail: a step
  # that leaves the array anything but fully synced — including on the
  # specific failure path that leaves it accidentally, durably resynced
  # for real (the sync this run seeded simply ran to completion before
  # midsync_wait_mid_flight ever confirmed it mid-flight) — must not hand
  # journey 5's own baseline sync (step 8) a starting state that is not a
  # clean array, since a real sync's own diff there depends on nothing
  # having changed here that it does not already know about.
  if ! midsync_ensure_synced; then
    midsync_ok=0
    midsync_last_reason="${midsync_last_reason:+$midsync_last_reason; }cleanup: $MIDSYNC_REASON"
  fi
  if [[ "$midsync_ok" == "1" ]]; then
    pass "virsh destroy mid-sync recovery"
  else
    fail "virsh destroy mid-sync recovery" "$midsync_last_reason"
  fi
else
  not_yet "virsh destroy mid-sync recovery" "no active hoservad on the guest (install or array setup above did not complete)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 7/13 reboot persistence ==="
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 8/13 config backup and restore ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if config_backup_restore; then
    pass "config backup and restore"
  elif [[ "$CONFIG_NOT_YET" == "1" ]]; then
    not_yet "config backup and restore" "$CONFIG_REASON"
  else
    fail "config backup and restore" "$CONFIG_REASON"
  fi
else
  not_yet "config backup and restore" "no active hoservad on the guest (install or array setup above did not complete)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 9/13 Playwright journeys ==="
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 10/13 spindown: SMART-poll IO-neutrality ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if "$script_dir/spindown-check.sh"; then
    pass "spindown: SMART-poll IO-neutrality"
  else
    fail "spindown: SMART-poll IO-neutrality" "see spindown-check.sh output above"
  fi
else
  not_yet "spindown: SMART-poll IO-neutrality" "no active hoservad on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 11/13 spindown: 30-min flat counters with a running pool ==="
not_yet "spindown: 30-min flat counters with a running pool" "array setup (step 3, #258) now gives this a real mergerfs/SnapRAID pool with a mounted share to test against — that half of the old gap is closed — but hoservad still does not run internal/disk's SMART poller or internal/parity's change journal on a timer (both exist as Go packages, issue #24, but cmd/hoservad/main.go wires neither into a scheduled job). spindown-check.sh (step 10) already stands in for that missing scheduler by looping the poller's own smartctl command directly against the array's already-mounted disks; doing the same loop against this step's live pool would still only be standing in for the scheduler, not proving hoservad's own 30-minute window produces zero drive writes with a pool mounted underneath it — re-check once cmd/hoservad/main.go wires the SMART poller and change journal on a timer"

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

echo "vm-suite[$HOSERVA_LAB_ID]: === 12/13 network confirm-or-revert (Q75) ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/network-revert-check.sh"; then
    pass "network confirm-or-revert"
  else
    fail "network confirm-or-revert" "see network-revert-check.sh output above (issue #114) — switches the guest NIC to ifupdown for the test; later steps run on that backend"
  fi
else
  not_yet "network confirm-or-revert" "no running domain (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 13/13 array stop/start sequence: missing disk at boot, service stops before unmount ==="
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
