#!/usr/bin/env bash
# L3 UPS check (issue #250, doc 02 §6, doc 03 §8.1/§8.3, D20): NUT's own
# dummy-ups driver drives a real hoservad through Q77's three reactions —
# on battery, power restored, low battery — the same way a real UPS and
# upsmon would, never a real UPS or a hand-rolled substitute for
# HandleNotify. hoservad's own control socket
# (cmd/hoservad/upscontrol.go), NUT's real NOTIFYCMD/SHUTDOWNCMD wrapper
# scripts (packaging/nut-notify, packaging/nut-shutdown) and its real
# systemd unit (packaging/debian/hoserva.service) are every one of them
# used unmodified — this script only supplies the simulated battery and,
# where the guest has no already-installed .deb to test against (this
# repo's own dev host and lab, per scripts/vm/deploy.sh's own header:
# dpkg-buildpackage/debhelper/fakeroot are deliberately not installed
# there), a hoservad/hoserva built straight from source the same
# build-elsewhere/run-inside way array-sequence-check.sh's own L3 test
# binary already is — compiling touches no device.
#
# Needs no configured array: Scheduler.PauseForBattery/Submit's own
# on-battery hold (internal/job/scheduler.go) runs before any job's own
# RunFunc, so a plain `hoserva mover run`/`hoserva sync --dry-run` against
# a freshly onboarded, arrayless daemon already proves the real refusal
# path — this script never formats or mounts the guest's array disks. The
# one exception is TypeSync/TypeScrub/TypeFix, which cmd/hoservad only
# registers once /etc/snapraid.conf exists (cmd/hoservad/parity.go) — this
# script writes a minimal one, only if none is already there from an
# earlier suite step, purely to unlock that registration; the sync itself
# is never expected to (and never asked to) actually complete.
#
# Runs last in run-l3-suite.sh's own step order: the low-battery scenario
# ends with a real `systemctl poweroff` inside the guest — the domain is
# genuinely shut off once this script returns success, so nothing later
# in the suite can assume it is still running.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' first"

UPS_ADMIN_USER="hoserva-ups-l3"
UPS_ADMIN_PASSWORD="hoserva-ups-l3-password"
DUMMY_DEV_FILE="/etc/nut/hoserva-ups.dev"

echo "ups-check[$HOSERVA_LAB_ID]: === setup: hoservad ==="
if vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  echo "ups-check[$HOSERVA_LAB_ID]: hoservad is already active on the guest (an earlier suite step installed it) — using it as-is"
else
  echo "ups-check[$HOSERVA_LAB_ID]: no active hoservad on the guest — building hoservad/hoserva from source and installing the real packaged pieces by hand (deploy.sh's own .deb build needs dpkg-buildpackage/debhelper/fakeroot, deliberately absent from the dev host and lab)"
  BUILD_OUT="$VM_STATE_DIR/ups-build"
  mkdir -p -- "$BUILD_OUT"
  (cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BUILD_OUT/hoservad" ./cmd/hoservad)
  (cd "$VM_REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$BUILD_OUT/hoserva" ./cmd/hoserva)

  vm_ssh 'mkdir -p /tmp/hoserva-ups-deploy'
  vm_scp "$BUILD_OUT/hoservad" "hoserva@127.0.0.1:/tmp/hoserva-ups-deploy/hoservad"
  vm_scp "$BUILD_OUT/hoserva" "hoserva@127.0.0.1:/tmp/hoserva-ups-deploy/hoserva"
  vm_scp "$VM_REPO_ROOT/packaging/nut-notify" "hoserva@127.0.0.1:/tmp/hoserva-ups-deploy/nut-notify"
  vm_scp "$VM_REPO_ROOT/packaging/nut-shutdown" "hoserva@127.0.0.1:/tmp/hoserva-ups-deploy/nut-shutdown"
  vm_scp "$VM_REPO_ROOT/packaging/debian/hoserva.service" "hoserva@127.0.0.1:/tmp/hoserva-ups-deploy/hoserva.service"

  vm_ssh 'sudo install -m0755 /tmp/hoserva-ups-deploy/hoservad /usr/bin/hoservad
sudo install -m0755 /tmp/hoserva-ups-deploy/hoserva /usr/bin/hoserva
sudo install -D -m0755 /tmp/hoserva-ups-deploy/nut-notify /usr/lib/hoserva/nut-notify
sudo install -D -m0755 /tmp/hoserva-ups-deploy/nut-shutdown /usr/lib/hoserva/nut-shutdown
sudo install -m0644 /tmp/hoserva-ups-deploy/hoserva.service /etc/systemd/system/hoserva.service
sudo mkdir -p /etc/hoserva /var/lib/hoserva /run/hoserva
sudo systemctl daemon-reload
sudo systemctl start hoserva'
  vm_ssh 'sudo systemctl is-active hoserva' >/dev/null || die "hoservad did not become active after a from-source install"
fi

echo "ups-check[$HOSERVA_LAB_ID]: === setup: an admin account, so the CLI's own peer-credential (root) or a session can reach admin-only endpoints ==="
# The daemon's own TLS listener can still be opening when this step first
# runs (right after a fresh install or restart above), so curl can return
# an empty body well before setup/status is actually reachable — this
# polls (bounded) for a body that actually names adminExists, rather than
# ever reading "empty/unreadable" as "an admin already exists".
SETUP_STATUS=""
setup_status_seen=false
for _ in $(seq 1 30); do
  SETUP_STATUS="$(vm_ssh "curl -sk https://127.0.0.1:8008/api/v1/setup/status" 2>/dev/null || true)"
  if [[ "$SETUP_STATUS" == *'"adminExists"'* ]]; then
    setup_status_seen=true
    break
  fi
  sleep 1
done
$setup_status_seen || die "setup/status never returned a body containing \"adminExists\" within 30s — last response: '$SETUP_STATUS'"
if [[ "$SETUP_STATUS" == *'"adminExists":false'* ]]; then
  CREATE_RESULT="$(vm_ssh "curl -sk -X POST https://127.0.0.1:8008/api/v1/setup/admin -H 'Content-Type: application/json' -d '{\"username\":\"$UPS_ADMIN_USER\",\"password\":\"$UPS_ADMIN_PASSWORD\"}'" 2>/dev/null || true)"
  [[ "$CREATE_RESULT" == *"\"username\":\"$UPS_ADMIN_USER\""* ]] || die "createFirstAdmin did not return the expected admin: $CREATE_RESULT"
elif [[ "$SETUP_STATUS" == *'"adminExists":true'* ]]; then
  echo "ups-check[$HOSERVA_LAB_ID]: an admin already exists (an earlier suite step's own onboarding) — leaving it alone"
else
  die "setup/status returned an \"adminExists\" body neither true nor false: $SETUP_STATUS"
fi
COOKIE_JAR="/tmp/hoserva-ups-cookiejar"
LOGIN_RESULT="$(vm_ssh "curl -sk -c $COOKIE_JAR -X POST https://127.0.0.1:8008/api/v1/auth/login -H 'Content-Type: application/json' -d '{\"username\":\"$UPS_ADMIN_USER\",\"password\":\"$UPS_ADMIN_PASSWORD\"}'" 2>/dev/null || true)"
if [[ "$LOGIN_RESULT" != *'"role":"admin"'* ]]; then
  echo "ups-check[$HOSERVA_LAB_ID]: logging in as $UPS_ADMIN_USER did not return an admin session ($LOGIN_RESULT) — the notifications assertion below will be skipped, everything else (the CLI, run as root) does not need it"
  HAVE_SESSION=false
else
  HAVE_SESSION=true
fi

echo "ups-check[$HOSERVA_LAB_ID]: === setup: unlock TypeSync/Scrub/Fix's own registration (cmd/hoservad/parity.go) if no array has configured one already ==="
if vm_ssh 'test -f /etc/snapraid.conf'; then
  echo "ups-check[$HOSERVA_LAB_ID]: /etc/snapraid.conf already exists (an earlier suite step's own array) — leaving it alone"
else
  vm_ssh 'sudo mkdir -p /var/lib/hoserva-ups-l3/parity /var/lib/hoserva-ups-l3/content /var/lib/hoserva-ups-l3/data1'
  vm_ssh "sudo tee /etc/snapraid.conf >/dev/null" <<'SNAPRAID_CONF'
parity /var/lib/hoserva-ups-l3/parity/snapraid.parity
content /var/lib/hoserva-ups-l3/content/snapraid.content
data d1 /var/lib/hoserva-ups-l3/data1
SNAPRAID_CONF
  vm_ssh 'sudo systemctl restart hoserva'
  vm_ssh 'sudo systemctl is-active hoserva' >/dev/null || die "hoservad did not come back up after adding a placeholder snapraid.conf"
fi

echo "ups-check[$HOSERVA_LAB_ID]: === setup: NUT with the dummy-ups driver, from hoservad's own real config renderer (never a hand-rolled substitute) ==="
if vm_ssh 'command -v upsc >/dev/null 2>&1'; then
  echo "ups-check[$HOSERVA_LAB_ID]: nut is already installed (an earlier suite step) — leaving it alone"
else
  vm_ssh 'sudo apt-get update -qq && sudo apt-get install -y -qq nut'
  # #340: upsmon.conf no longer overrides RUN_AS_USER, so upsmon's own
  # unprivileged child runs as nut (Debian's default) rather than root,
  # reachable only on the ups control socket's own root:nut 0660
  # (applySocketGroupPermissions, cmd/hoservad/main.go) — never the
  # hoserva group, which stays root-equivalent (Q44). nut is only
  # Recommends:, not Depends:, so this branch always installs it after
  # hoservad already started (above) and found no nut group to chow the
  # socket to; restarting hoservad re-runs that same startup chown now
  # that the group exists (#340) — the production path a settings-ups
  # save takes instead, UPSService.Update's own Socket.Apply, is
  # exercised at L1 (internal/api/ups_handler_test.go), not here.
  vm_ssh 'sudo systemctl restart hoserva'
  vm_ssh 'sudo systemctl is-active hoserva' >/dev/null || die "hoservad did not come back up after nut was installed"
fi

render_nut_file() {
  (cd "$VM_REPO_ROOT" && go run ./scripts/vm/nutconfig "$1")
}
vm_ssh "sudo tee /etc/nut/nut.conf >/dev/null" <<<"$(render_nut_file nut.conf)"
vm_ssh "sudo tee /etc/nut/ups.conf >/dev/null" <<<"$(render_nut_file ups.conf)"
vm_ssh "sudo tee /etc/nut/upsd.users >/dev/null" <<<"$(render_nut_file upsd.users)"
vm_ssh "sudo tee /etc/nut/upsmon.conf >/dev/null" <<<"$(render_nut_file upsmon.conf)"

# dummy-ups's own simulated-hardware state file (dummy-ups(8)): NUT reads
# it fresh whenever its mtime changes, which is what set_battery_state
# below relies on to drive the scenarios — this is the harness's own
# stand-in for a battery, never a Hoserva-managed config file.
set_battery_state() {
  vm_ssh "sudo tee $DUMMY_DEV_FILE >/dev/null" <<<"$1"
  vm_ssh "sudo chown root:nut $DUMMY_DEV_FILE && sudo chmod 640 $DUMMY_DEV_FILE"
}
set_battery_state 'ups.status: OL
battery.charge: 100
battery.runtime: 5000'

vm_ssh 'sudo systemctl reset-failed "nut-driver@hoserva-ups.service" >/dev/null 2>&1 || true'
vm_ssh 'sudo systemctl restart nut-driver-enumerator.service'
vm_ssh 'sudo systemctl restart "nut-driver@hoserva-ups.service"'
vm_ssh 'sudo systemctl restart nut-server.service'
vm_ssh 'sudo systemctl restart nut-monitor.service'

# upsmon always forks into a small privileged parent (root — the only
# piece that ever runs SHUTDOWNCMD, per upsmon.conf(5)) plus the bulk
# worker RUN_AS_USER names; before #340 that worker was itself root
# (RenderUPSMonConf's own RUN_AS_USER override), so the presence of a
# nut-owned upsmon process here — not the absence of any root one, which
# always exists — is what actually distinguishes the fix from a
# regression back to it.
echo "ups-check[$HOSERVA_LAB_ID]: === confirming upsmon's own bulk-work process runs as nut, not root (#340) ==="
nut_child_seen=false
for _ in $(seq 1 15); do
  if vm_ssh 'sudo ps -C upsmon -o user= | grep -qx nut'; then
    nut_child_seen=true
    break
  fi
  sleep 1
done
$nut_child_seen || die "no upsmon process is running as nut within 15s of restarting nut-monitor.service — RenderUPSMonConf's own RUN_AS_USER removal did not take effect, or the nut package's default RUN_AS_USER changed"

# applySocketGroupPermissions (cmd/hoservad/main.go) chowns the ups
# control socket to root:nut 0660 at daemon start (#340) — this is what
# actually lets the nut-owned upsmon process confirmed above reach it,
# never the hoserva group (that stays reserved for root-equivalent admin
# access, Q44).
echo "ups-check[$HOSERVA_LAB_ID]: === confirming the ups control socket is root:nut 0660 (#340) ==="
SOCK_STAT="$(vm_ssh 'stat -c "%U:%G %a" /run/hoserva/ups-control.sock')"
[[ "$SOCK_STAT" == "root:nut 660" ]] || die "ups control socket is '$SOCK_STAT', want 'root:nut 660'"

echo "ups-check[$HOSERVA_LAB_ID]: waiting for upsd to see the dummy-ups driver online"
ready=false
for _ in $(seq 1 30); do
  if vm_ssh 'upsc hoserva-ups ups.status' 2>/dev/null | grep -q '^OL'; then
    ready=true
    break
  fi
  sleep 1
done
$ready || die "dummy-ups driver never reported ups.status: OL through upsd — see the guest's own nut-driver@hoserva-ups.service and nut-server.service logs"

declare -a STEP_NAMES=()
declare -a STEP_RESULTS=()
record() { STEP_NAMES+=("$1"); STEP_RESULTS+=("$2"); }

# submit_refused/submit_accepted report whether `sudo hoserva mover run`/
# `sync --dry-run` is refused or accepted right now — the same real
# Submit() path Q77's on-battery hold (internal/job/scheduler.go) runs
# before any job's own RunFunc, so this never needs a configured array to
# prove. wait_until_refused/wait_until_accepted poll it directly (bounded)
# rather than pre-gating on upsmon's own journal transition line: a stale
# match from an earlier boot's own flapping (this guest's dummy-ups state
# file persists across a reboot) would otherwise let the poll return
# before hoservad's real HandleNotify for *this* transition has actually
# run — confirmed empirically against this exact harness on a
# second, already-configured run.
#
# submit_refused only accepts a refusal that actually carries #356's own
# on_battery error code (mapAPIErr's generic pass-through prints the
# server's Error struct into the CLI's stderr as "code 409:
# {Code:on_battery ...}") — any other nonzero exit, e.g. a generic
# `internal` error or a same-class conflict, is not proof of the
# on-battery hold and must not be read as one.
submit_refused() {
  local cmd="$1" output
  output="$(vm_ssh "sudo hoserva $cmd --json" 2>&1)" && return 1
  [[ "$output" == *'Code:on_battery'* ]]
}
submit_accepted() {
  local cmd="$1"
  vm_ssh "sudo hoserva $cmd --json" >/dev/null 2>&1
}
wait_until_refused() {
  local cmd="$1" deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    submit_refused "$cmd" && return 0
    sleep 1
  done
  return 1
}
wait_until_accepted() {
  local cmd="$1" deadline=$((SECONDS + 30))
  while (( SECONDS < deadline )); do
    submit_accepted "$cmd" && return 0
    sleep 1
  done
  return 1
}

echo "ups-check[$HOSERVA_LAB_ID]: === scenario 1/3: on battery — notify, mover paused, syncs held ==="
SCENARIO1_STATUS=0
set_battery_state 'ups.status: OB
battery.charge: 40
battery.runtime: 900'
if ! wait_until_refused "mover run"; then
  echo "ups-check[$HOSERVA_LAB_ID]: 'hoserva mover run' was never refused with the on_battery error code within 30s of going on battery — Q77's own hold did not take effect" >&2
  SCENARIO1_STATUS=1
elif ! wait_until_refused "sync --dry-run"; then
  echo "ups-check[$HOSERVA_LAB_ID]: 'hoserva sync --dry-run' was never refused with the on_battery error code within 30s of going on battery — Q77's own hold did not take effect" >&2
  SCENARIO1_STATUS=1
fi
if [[ "$SCENARIO1_STATUS" -eq 0 ]]; then
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 1 — PASS"
  record "on battery (notify, mover paused, syncs held)" "PASS"
else
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 1 — FAIL" >&2
  record "on battery (notify, mover paused, syncs held)" "FAIL"
fi

echo "ups-check[$HOSERVA_LAB_ID]: === scenario 2/3: power restored — both resume ==="
SCENARIO2_STATUS=0
set_battery_state 'ups.status: OL
battery.charge: 100
battery.runtime: 5000'
if ! wait_until_accepted "mover run"; then
  echo "ups-check[$HOSERVA_LAB_ID]: 'hoserva mover run' is still refused 30s after power was restored" >&2
  SCENARIO2_STATUS=1
elif ! wait_until_accepted "sync --dry-run"; then
  echo "ups-check[$HOSERVA_LAB_ID]: 'hoserva sync --dry-run' is still refused 30s after power was restored" >&2
  SCENARIO2_STATUS=1
fi
if [[ "$SCENARIO2_STATUS" -eq 0 ]]; then
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 2 — PASS"
  record "power restored (mover and syncs resume)" "PASS"
else
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 2 — FAIL" >&2
  record "power restored (mover and syncs resume)" "FAIL"
fi

echo "ups-check[$HOSERVA_LAB_ID]: === doc 03 §8.3 event catalog: ups_on_battery / ups_battery_low reached the notify inbox ==="
if $HAVE_SESSION; then
  NOTIF_RESULT="$(vm_ssh "curl -sk -b $COOKIE_JAR https://127.0.0.1:8008/api/v1/notifications" 2>/dev/null || true)"
  if [[ "$NOTIF_RESULT" == *'"ups_on_battery"'* ]]; then
    echo "ups-check[$HOSERVA_LAB_ID]: the ups_on_battery event reached the notify inbox — PASS"
    record "ups_on_battery event delivered" "PASS"
  else
    echo "ups-check[$HOSERVA_LAB_ID]: the ups_on_battery event never reached the notify inbox: $NOTIF_RESULT" >&2
    record "ups_on_battery event delivered" "FAIL"
  fi
else
  echo "ups-check[$HOSERVA_LAB_ID]: no admin session — see the setup step above"
  record "ups_on_battery event delivered" "NOT-YET-IMPLEMENTED: no admin session"
fi

echo "ups-check[$HOSERVA_LAB_ID]: === scenario 3/3: low battery — checkpoint, then a real clean shutdown ==="
# handleLowBattery's own real ShutdownSequence (cmd/hoservad/upscontrol.go
# newUPSController: job.UPSShutdown{Array, Power}) checkpoints every
# running job through the exact same ArraySequence.Stop/Drain path
# array-sequence-check.sh's own scenarios already exercise directly, then
# calls `systemctl poweroff` — this scenario proves that real chain is
# reachable end-to-end from a real upsmon SHUTDOWNCMD, not that Drain
# itself marks a resumable job interrupted (internal/job/ups_test.go
# already covers that at L1/L2, with a fake ShutdownSequence able to hold
# a job open exactly as long as the assertion needs — a real snapraid
# sync here finishes far too fast, and far too non-deterministically, for
# an L3 shell script to race reliably against it).
set_battery_state 'ups.status: OB LB
battery.charge: 5
battery.runtime: 60'
shutoff_seen=false
shutdown_deadline=$((SECONDS + 90))
while (( SECONDS < shutdown_deadline )); do
  if ! vm_domain_running "$VM_DOMAIN"; then
    shutoff_seen=true
    break
  fi
  sleep 2
done
if $shutoff_seen; then
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 3 — PASS (the guest domain reached 'shut off' — a real ArraySequence.Stop then 'systemctl poweroff', triggered by upsmon's own SHUTDOWNCMD over hoservad's control socket)"
  record "low battery (checkpoint, clean shutdown)" "PASS"
else
  echo "ups-check[$HOSERVA_LAB_ID]: scenario 3 — FAIL: the guest domain never reached 'shut off' within 90s of ups.status: OB LB" >&2
  record "low battery (checkpoint, clean shutdown)" "FAIL"
fi

echo ""
echo "ups-check[$HOSERVA_LAB_ID]: ===== summary ====="
overall_fail=0
for i in "${!STEP_NAMES[@]}"; do
  printf 'ups-check[%s]: %-42s %s\n' "$HOSERVA_LAB_ID" "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]}"
  [[ "${STEP_RESULTS[$i]}" == FAIL* ]] && overall_fail=1
done

exit "$overall_fail"
