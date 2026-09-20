#!/usr/bin/env bash
# `make vm-soak` — Phase 1's L3 soak (doc 06 §6, doc 07 §1, Q16, Q30, D20):
# 30 nightly maintenance chains back to back over seeded churn, with a
# yanked disk, a full disk, and power loss mid-sync. Every blocked sync is
# reviewed in the run report. The recorded diff history is what Q16's
# thresholds are confirmed against.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

NIGHTS="${HOSERVA_SOAK_NIGHTS:-30}"
if ! [[ "$NIGHTS" =~ ^[0-9]+$ ]] || (( NIGHTS < 30 )); then
  die "HOSERVA_SOAK_NIGHTS must be an integer >= 30 (got ${NIGHTS})"
fi

HISTORY_JSONL="$VM_STATE_DIR/soak-history.jsonl"
REPORT_MD="$VM_STATE_DIR/soak-report.md"
NIGHT_DIR="$VM_STATE_DIR/nights"
GUEST_HELPER="/usr/local/lib/hoserva-soak/soak-guest.py"
YANK_XML="$VM_STATE_DIR/yank-disk.xml"
CHECKSUM_DIR="$VM_STATE_DIR/checksums"
FINDINGS_FILE="$VM_STATE_DIR/findings.txt"
mkdir -p -- "$CHECKSUM_DIR" "$NIGHT_DIR"

# Soak disks are small enough that filling one does not allocate terabytes
# on the host. vm-suite keeps the default 8T/4T/1T sizes.
export HOSERVA_VM_PARITY_SIZE="${HOSERVA_VM_PARITY_SIZE:-2G}"
export HOSERVA_VM_DATA_SIZE="${HOSERVA_VM_DATA_SIZE:-2G}"
export HOSERVA_VM_CACHE_SIZE="${HOSERVA_VM_CACHE_SIZE:-512M}"

# Calendar Monday 2026-09-21 so weekly scrub (Sunday, Q30 default) lands
# on nights 7, 14, 21, 28.
SOAK_DAY0="2026-09-21"

YANK_NIGHT=12
FULL_NIGHT=18
POWER_NIGHT=24

destroyed=false
copy_artifacts() {
  if [[ -f "$HISTORY_JSONL" ]]; then
    cp -f -- "$HISTORY_JSONL" "$script_dir/soak-history.jsonl"
  fi
  if [[ -f "$REPORT_MD" ]]; then
    cp -f -- "$REPORT_MD" "$script_dir/soak-report.md"
  fi
}
cleanup() {
  local st=$?
  copy_artifacts || true
  if ! $destroyed; then
    destroyed=true
    echo "vm-soak[$HOSERVA_LAB_ID]: tearing down this lab's VM"
    "$script_dir/destroy-vm.sh" || true
  fi
  exit "$st"
}
trap cleanup EXIT

wait_socket() {
  local i=0
  while (( i < 30 )); do
    if vm_ssh 'sudo test -S /run/hoserva/hoserva.sock'; then
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  echo "vm-soak[$HOSERVA_LAB_ID]: hoservad socket missing; service status:" >&2
  vm_ssh 'sudo systemctl status hoserva --no-pager || true' >&2 || true
  vm_ssh 'sudo journalctl -u hoserva -n 80 --no-pager || true' >&2 || true
  die "hoservad did not create /run/hoserva/hoserva.sock"
}

# Drive soak-guest.py as root over this lab's own SSH (Unix socket is
# peer-credential-trusted for uid 0, Q44). printf %q keeps argv intact.
# Keep the last stdout line so a cloud-image motd cannot poison JSON.
guest() {
  local args out st
  printf -v args '%q ' "$@"
  set +e
  # shellcheck disable=SC2029
  out="$(vm_ssh "sudo python3 $GUEST_HELPER $args")"
  st=$?
  set -e
  printf '%s\n' "${out##*$'\n'}"
  return "$st"
}

echo "vm-soak[$HOSERVA_LAB_ID]: === install ==="
if vm_domain_exists "$VM_DOMAIN"; then
  "$script_dir/destroy-vm.sh"
  destroyed=false
fi
"$script_dir/create-vm.sh"

echo "vm-soak[$HOSERVA_LAB_ID]: installing runtime packages on the guest"
vm_ssh 'sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq mergerfs snapraid xfsprogs smartmontools hdparm python3 adduser fuse3'

echo "vm-soak[$HOSERVA_LAB_ID]: building hoservad and hoserva (compiling touches no device)"
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$VM_STATE_DIR/hoservad" ./cmd/hoservad)
(cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$VM_STATE_DIR/hoserva" ./cmd/hoserva)

echo "vm-soak[$HOSERVA_LAB_ID]: deploying binaries and systemd unit into the guest"
vm_ssh 'sudo install -d -m0755 /usr/local/lib/hoserva-soak /var/lib/hoserva /run/hoserva /etc/hoserva'
vm_scp "$VM_STATE_DIR/hoservad" "hoserva@127.0.0.1:/tmp/hoservad"
vm_scp "$VM_STATE_DIR/hoserva" "hoserva@127.0.0.1:/tmp/hoserva"
vm_scp "$script_dir/soak-guest.py" "hoserva@127.0.0.1:/tmp/soak-guest.py"
vm_scp "$VM_REPO_ROOT/packaging/debian/hoserva.service" "hoserva@127.0.0.1:/tmp/hoserva.service"
vm_ssh "sudo install -m0755 /tmp/hoservad /usr/bin/hoservad && sudo install -m0755 /tmp/hoserva /usr/bin/hoserva && sudo install -m0755 /tmp/soak-guest.py $GUEST_HELPER"
vm_ssh 'getent group hoserva >/dev/null || sudo addgroup --system hoserva'
vm_ssh 'sudo install -m0644 /tmp/hoserva.service /etc/systemd/system/hoserva.service'
# smartd is a spindown-wake culprit (doc 08 §1); the .deb preinst/postinst
# disable it. Binary deploy does the same drop-in so the soak is not woken
# by an unmanaged smartd.
vm_ssh 'sudo mkdir -p /etc/systemd/system/smartmontools.service.d && printf "%s\n" "[Unit]" "ConditionPathExists=/nonexistent-hoserva-smartd-disabled" | sudo tee /etc/systemd/system/smartmontools.service.d/hoserva-disable.conf >/dev/null'
vm_ssh 'sudo systemctl daemon-reload && sudo systemctl stop smartmontools.service 2>/dev/null || true && sudo systemctl mask smartmontools.service && sudo systemctl stop --now xfs_scrub_all.timer 2>/dev/null || true && sudo systemctl mask xfs_scrub_all.timer 2>/dev/null || true'
vm_ssh 'sudo systemctl daemon-reload && sudo systemctl enable --now hoserva && sudo systemctl is-active hoserva'
wait_socket
guest setup-admin --username hoserva-soak --password hoserva-soak-password >/dev/null

echo "vm-soak[$HOSERVA_LAB_ID]: === array setup ==="
create_out="$(guest create-array --min-free-space 32M)"
echo "$create_out"
job_id="$(printf '%s\n' "$create_out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
[[ -n "$job_id" ]] || die "create-array did not return a job id: $create_out"
guest wait-job "$job_id" --timeout 1800 >/dev/null

echo "vm-soak[$HOSERVA_LAB_ID]: restarting hoservad so it loads snapraid.conf and the array sequence"
vm_ssh 'sudo systemctl restart hoserva && sudo systemctl is-active hoserva'
wait_socket
# mergerfs refuses a missing Where=; systemd .mount units mkdir it, the
# array-start sequence's direct exec does not (recorded as a finding).
vm_ssh 'sudo mkdir -p /mnt/user'
# Array start mounts the catch-all; create-array only mounted physical disks.
guest array-start >/dev/null
vm_ssh 'findmnt /mnt/user >/dev/null'
vm_ssh 'sudo timedatectl set-ntp false'

echo "vm-soak[$HOSERVA_LAB_ID]: seeding keep+churn files and taking the baseline sync"
guest seed >/dev/null
sync_out="$(guest sync)"
echo "$sync_out"
sync_id="$(printf '%s\n' "$sync_out" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
guest wait-job "$sync_id" --timeout 1800 >/dev/null
guest checksum -o /tmp/soak-checksum-baseline.txt >/dev/null
vm_scp "hoserva@127.0.0.1:/tmp/soak-checksum-baseline.txt" "$CHECKSUM_DIR/baseline.txt"

# Persist the yank target's domain-XML fragment now, while every disk is
# still attached. array-sequence-check.sh's own parse: serial carries
# HOSERVA_LAB_ID and disk1 is the first data-role disk (parity is first
# in declaration order).
export VM_DOMAIN VM_CONNECT HOSERVA_LAB_ID
python3 - "$YANK_XML" <<'PY'
import os, subprocess, sys, xml.etree.ElementTree as ET
out = sys.argv[1]
xml = subprocess.check_output(["virsh", "-c", os.environ["VM_CONNECT"], "dumpxml", os.environ["VM_DOMAIN"]], text=True)
root = ET.fromstring(xml)
want = None
for disk in root.findall("./devices/disk"):
    serial = disk.findtext("serial") or ""
    if serial.startswith("disk1-hoserva-"):
        want = disk
        break
if want is None:
    raise SystemExit("no disk1 array disk in domain XML to yank")
ET.ElementTree(want).write(out)
print("yank target serial", want.findtext("serial"), "dev", want.find("./target").get("dev"))
PY

: > "$HISTORY_JSONL"
: > "$FINDINGS_FILE"

note_finding() {
  printf '%s\n' "$1" | tee -a "$FINDINGS_FILE"
}

checksum_ok() {
  local tag=$1
  guest checksum -o "/tmp/soak-checksum-${tag}.txt" >/dev/null
  vm_scp "hoserva@127.0.0.1:/tmp/soak-checksum-${tag}.txt" "$CHECKSUM_DIR/${tag}.txt"
  if ! cmp -s "$CHECKSUM_DIR/pre-${tag}.txt" "$CHECKSUM_DIR/${tag}.txt"; then
    note_finding "$tag: checksum mismatch after recovery"
    echo "vm-soak[$HOSERVA_LAB_ID]: CHECKSUM MISMATCH for $tag" >&2
    diff -u "$CHECKSUM_DIR/pre-${tag}.txt" "$CHECKSUM_DIR/${tag}.txt" >&2 || true
    return 1
  fi
  echo "vm-soak[$HOSERVA_LAB_ID]: checksums verified for $tag"
}

yank_disk() {
  local target
  target="$(python3 -c 'import xml.etree.ElementTree as ET,sys; print(ET.parse(sys.argv[1]).getroot().find("target").get("dev"))' "$YANK_XML")"
  vm_assert_own_domain "$VM_DOMAIN"
  echo "vm-soak[$HOSERVA_LAB_ID]: yanking $target (live+config)"
  if ! virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$target" --live --config >/dev/null; then
    echo "vm-soak[$HOSERVA_LAB_ID]: live detach failed, shutting down to detach from persistent config"
    virsh -c "$VM_CONNECT" shutdown "$VM_DOMAIN" >/dev/null
    deadline=$((SECONDS + 120))
    while vm_domain_running "$VM_DOMAIN"; do
      if (( SECONDS >= deadline )); then
        virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
        break
      fi
      sleep 2
    done
    virsh -c "$VM_CONNECT" detach-disk "$VM_DOMAIN" "$target" --config >/dev/null
    virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
    vm_wait_tcp "$VM_SSH_PORT" 180 || die "guest SSH port did not return after yank reboot"
    vm_ssh_wait_ready 180 || die "could not SSH after yank reboot"
    wait_socket
  fi
}

reattach_disk() {
  vm_assert_own_domain "$VM_DOMAIN"
  echo "vm-soak[$HOSERVA_LAB_ID]: reattaching yanked disk"
  virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$YANK_XML" --live --config >/dev/null
  vm_ssh 'sudo mkdir -p /mnt/user'
  guest array-start >/dev/null || true
  sleep 2
}

power_loss() {
  vm_assert_own_domain "$VM_DOMAIN"
  echo "vm-soak[$HOSERVA_LAB_ID]: power loss — virsh destroy mid-sync"
  virsh -c "$VM_CONNECT" destroy "$VM_DOMAIN" >/dev/null
  virsh -c "$VM_CONNECT" start "$VM_DOMAIN" >/dev/null
  vm_wait_tcp "$VM_SSH_PORT" 180 || die "guest SSH port did not return after power loss"
  vm_ssh_wait_ready 180 || die "could not SSH after power loss"
  vm_ssh 'sudo systemctl is-active hoserva'
  wait_socket
  vm_ssh 'sudo mkdir -p /mnt/user'
  guest array-start >/dev/null || true
}

echo "vm-soak[$HOSERVA_LAB_ID]: === 30 nightly chains ==="
for night in $(seq 1 "$NIGHTS"); do
  echo "vm-soak[$HOSERVA_LAB_ID]: --- night $night / $NIGHTS ---"
  day="$(python3 -c 'from datetime import date,timedelta; d=date.fromisoformat("'"$SOAK_DAY0"'")+timedelta(days='"$night"'-1); print(d.isoformat())')"
  weekday="$(python3 -c 'from datetime import date,timedelta; d=date.fromisoformat("'"$SOAK_DAY0"'")+timedelta(days='"$night"'-1); print(d.strftime("%A"))')"

  churn_args=("$night")
  inject=""
  case "$night" in
    8|22) churn_args+=(--mass-delete); inject="mass-delete" ;;
  esac
  if (( night == POWER_NIGHT )); then
    churn_args+=(--heavy)
    inject="power-loss"
  fi
  if (( night == YANK_NIGHT )); then
    inject="yank"
  fi
  if (( night == FULL_NIGHT )); then
    inject="full-disk"
  fi

  guest churn "${churn_args[@]}" >"$NIGHT_DIR/$night.churn.json"

  if [[ "$inject" == "yank" || "$inject" == "full-disk" || "$inject" == "power-loss" ]]; then
    guest checksum -o /tmp/soak-checksum-pre.txt >/dev/null
    vm_scp "hoserva@127.0.0.1:/tmp/soak-checksum-pre.txt" "$CHECKSUM_DIR/pre-${inject}.txt"
  fi

  if [[ "$inject" == "yank" ]]; then
    yank_disk
  fi
  if [[ "$inject" == "full-disk" ]]; then
    guest fill
  fi

  if ! guest diff >"$NIGHT_DIR/$night.diff.json"; then
    printf '%s\n' '{"guard":{"wouldBlock":true,"summary":"POST /parity/diff failed"},"error":true}' >"$NIGHT_DIR/$night.diff.json"
  fi

  before_run="$(guest last-run || true)"
  vm_ssh "sudo timedatectl set-time '${day} 02:01:00'"
  guest wait-chain --before "$before_run" --timeout 180 >/dev/null
  if [[ "$inject" == "power-loss" ]]; then
    powered=false
    i=0
    while (( i < 45 )); do
      guest running-jobs >"$NIGHT_DIR/$night.running.json" || true
      if python3 -c 'import json,sys; jobs=(json.load(open(sys.argv[1])).get("jobs") or []); sys.exit(0 if any(j.get("type")=="sync" for j in jobs) else 1)' "$NIGHT_DIR/$night.running.json"; then
        power_loss
        powered=true
        break
      fi
      i=$((i + 1))
      sleep 1
    done
    if ! $powered; then
      echo "vm-soak[$HOSERVA_LAB_ID]: chain sync finished too quickly; starting a sync to inject power-loss"
      guest sync --confirm >"$NIGHT_DIR/$night.force-sync.json" || true
      i=0
      while (( i < 45 )); do
        guest running-jobs >"$NIGHT_DIR/$night.running.json" || true
        if python3 -c 'import json,sys; jobs=(json.load(open(sys.argv[1])).get("jobs") or []); sys.exit(0 if any(j.get("type")=="sync" for j in jobs) else 1)' "$NIGHT_DIR/$night.running.json"; then
          power_loss
          powered=true
          break
        fi
        i=$((i + 1))
        sleep 1
      done
    fi
    if ! $powered; then
      note_finding "night $night: power-loss injection missed a running sync job"
    fi
  fi
  guest wait-idle --timeout 1800 >/dev/null || true

  if [[ "$inject" == "yank" ]]; then
    reattach_disk
    checksum_ok yank || true
  fi
  if [[ "$inject" == "full-disk" ]]; then
    guest unfill
    checksum_ok full-disk || true
  fi
  if [[ "$inject" == "power-loss" ]]; then
    checksum_ok power-loss || true
    recover="$(guest sync --confirm || true)"
    printf '%s\n' "$recover" >"$NIGHT_DIR/$night.recover.json" || true
    rid="$(python3 -c 'import json,sys
try:
  print(json.load(open(sys.argv[1])).get("id") or "")
except Exception:
  print("")' "$NIGHT_DIR/$night.recover.json" || true)"
    if [[ -n "$rid" ]]; then
      guest wait-job "$rid" --timeout 1800 --allow-failure >/dev/null || true
    fi
  fi

  guest jobs >"$NIGHT_DIR/$night.jobs.json" || true
  rec_st=0
  python3 "$script_dir/soak-record-night.py" \
    "$night" "$day" "$weekday" "$inject" \
    "$NIGHT_DIR/$night.churn.json" "$NIGHT_DIR/$night.diff.json" "$NIGHT_DIR/$night.jobs.json" \
    "$HISTORY_JSONL" "$FINDINGS_FILE" || rec_st=$?
  if [[ $rec_st -eq 10 ]]; then
    if [[ "$inject" != "yank" ]]; then
      force="$(guest sync --confirm)"
      fid="$(printf '%s\n' "$force" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"
      guest wait-job "$fid" --timeout 1800 --allow-failure >/dev/null || true
    fi
  elif [[ $rec_st -ne 0 ]]; then
    die "failed recording night $night"
  fi

  echo "vm-soak[$HOSERVA_LAB_ID]: night $night done (inject=${inject:-none})"
done

echo "vm-soak[$HOSERVA_LAB_ID]: writing run report"
python3 "$script_dir/soak-write-report.py" \
  "$HISTORY_JSONL" "$REPORT_MD" "$FINDINGS_FILE" \
  "$HOSERVA_LAB_ID" "$HOSERVA_VM_PARITY_SIZE" "$HOSERVA_VM_DATA_SIZE" "$HOSERVA_VM_CACHE_SIZE" \
  "$SOAK_DAY0" "$YANK_NIGHT" "$FULL_NIGHT" "$POWER_NIGHT"

copy_artifacts
echo "vm-soak[$HOSERVA_LAB_ID]: done — report at scripts/vm/soak-report.md"
