#!/usr/bin/env bash
# `make vm-suite` — the nightly/pre-release L3 suite (doc 06 §4, §7,
# Q79): install, onboarding, array setup, disk yank and reconstruction,
# `virsh destroy` mid-sync recovery, reboot persistence, config
# backup/restore, spindown, and the array stop/start sequence (issue
# #146).
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 1/11 install ==="
if vm_domain_exists "$VM_DOMAIN"; then
  "$script_dir/destroy-vm.sh"
fi
"$script_dir/create-vm.sh"
if DEB="${DEB:-}" TAG="${TAG:-}" "$script_dir/deploy.sh"; then
  pass "install"
else
  fail "install" "deploy.sh failed — see its own output above (on the dev host this is expected: dpkg-buildpackage/debhelper/fakeroot are deliberately not installed here, per scripts/release/build-deb.sh's own header comment; a hosted CI runner has them)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 2/11 onboarding ==="
if vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  SETUP_STATUS="$(vm_ssh "curl -sk https://127.0.0.1:8008/api/v1/setup/status" 2>/dev/null || true)"
  if [[ "$SETUP_STATUS" == *'"adminExists":false'* ]]; then
    CREATE_RESULT="$(vm_ssh "curl -sk -X POST https://127.0.0.1:8008/api/v1/setup/admin -H 'Content-Type: application/json' -d '{\"username\":\"hoserva-l3\",\"password\":\"hoserva-l3-suite-password\"}'" 2>/dev/null || true)"
    if [[ "$CREATE_RESULT" == *'"username":"hoserva-l3"'* ]]; then
      pass "onboarding"
    else
      fail "onboarding" "createFirstAdmin did not return the expected admin: $CREATE_RESULT"
    fi
  else
    fail "onboarding" "getSetupStatus did not report adminExists=false: $SETUP_STATUS"
  fi
else
  not_yet "onboarding" "hoservad is not active on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 3/11 array setup ==="
not_yet "array setup" "no array/disk/pool operation is in api/openapi.yaml yet (internal/pool, internal/parity exist as Go packages with L1/L2 tests, but no API surface a VM-level end-to-end test could drive) — re-check once that API lands"

echo "vm-suite[$HOSERVA_LAB_ID]: === 4/11 disk yank and reconstruction ==="
not_yet "disk yank and reconstruction" "depends on array setup (step 3) existing first — nothing to reconstruct without a configured array"

echo "vm-suite[$HOSERVA_LAB_ID]: === 5/11 virsh destroy mid-sync recovery ==="
not_yet "virsh destroy mid-sync recovery" "no sync/parity operation is reachable via the API yet (Q79/doc 02 §2's threshold-guard-protected sync) — 'mid-sync' has nothing running to interrupt"

echo "vm-suite[$HOSERVA_LAB_ID]: === 6/11 reboot persistence ==="
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

echo "vm-suite[$HOSERVA_LAB_ID]: === 7/11 config backup and restore ==="
not_yet "config backup and restore" "no backup/export or import operation is in api/openapi.yaml yet (internal/backup does not exist) — doc 10's config backup feature has not landed"

echo "vm-suite[$HOSERVA_LAB_ID]: === 8/11 Playwright journeys ==="
if [[ -x "$script_dir/run-playwright.sh" ]]; then
  if HOSERVA_E2E_BASE_URL="https://127.0.0.1:$VM_HTTPS_PORT" "$script_dir/run-playwright.sh"; then
    pass "Playwright journeys"
  else
    fail "Playwright journeys" "see web/'s own Playwright report above — journey 5 (mass deletion blocks the sync) is expected to fail honestly until the mover/threshold-guard UI exists (web/src/routes has no share or array page yet), not silently skipped"
  fi
else
  not_yet "Playwright journeys" "scripts/vm/run-playwright.sh is missing or not executable"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 9/11 spindown: SMART-poll IO-neutrality ==="
if vm_domain_running "$VM_DOMAIN" && vm_ssh 'sudo systemctl is-active hoserva' >/dev/null 2>&1; then
  if "$script_dir/spindown-check.sh"; then
    pass "spindown: SMART-poll IO-neutrality"
  else
    fail "spindown: SMART-poll IO-neutrality" "see spindown-check.sh output above"
  fi
else
  not_yet "spindown: SMART-poll IO-neutrality" "no active hoservad on the guest (install step above did not complete — see step 1)"
fi

echo "vm-suite[$HOSERVA_LAB_ID]: === 10/11 spindown: 30-min flat counters with a running pool ==="
not_yet "spindown: 30-min flat counters with a running pool" "needs a mergerfs/SnapRAID pool configured through hoservad (no array/pool operation is in api/openapi.yaml yet, the same gap step 3 names) plus a scheduled SMART-poll and change-journal job wired into hoservad (internal/disk's SMART poller and internal/parity's change journal exist as Go packages, issue #24, but cmd/hoservad/main.go calls neither on a timer yet) — the lab's own zero-organic-IO property under realistic idle/appdata/SMB-client load is already confirmed (doc 08 Spike 1, 2026-09-15), but without SMART polling or the change journal actually running; re-check once the pool API and the scheduler land"

echo "vm-suite[$HOSERVA_LAB_ID]: === 11/11 array stop/start sequence: missing disk at boot, service stops before unmount ==="
if vm_domain_running "$VM_DOMAIN"; then
  if "$script_dir/array-sequence-check.sh"; then
    pass "array stop/start sequence"
  else
    fail "array stop/start sequence" "see array-sequence-check.sh output above (issue #146) — this step permanently detaches one array disk from this domain's own persistent config, so it runs last"
  fi
else
  not_yet "array stop/start sequence" "no running domain (install step above did not complete — see step 1)"
fi

echo ""
echo "vm-suite[$HOSERVA_LAB_ID]: ===== summary ====="
overall_fail=0
for i in "${!STEP_NAMES[@]}"; do
  printf 'vm-suite[%s]: %-32s %s\n' "$HOSERVA_LAB_ID" "${STEP_NAMES[$i]}" "${STEP_RESULTS[$i]}"
  [[ "${STEP_RESULTS[$i]}" == FAIL:* ]] && overall_fail=1
done

exit "$overall_fail"
