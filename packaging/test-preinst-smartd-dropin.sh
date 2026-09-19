#!/usr/bin/env bash
# Safety-critical reproduction test for the review finding on #171:
# packaging/debian/postinst masks and stops smartmontools.service, but
# smartmontools is a Depends: (packaging/debian/control) configured
# *before* hoserva's own postinst runs, and its 7.4-3 postinst
# unconditionally starts smartmontools.service (smartd) as part of that
# configuration — before postinst ever gets a chance to react. This test
# proves preinst — which runs as hoserva's own package is unpacked,
# reliably before any package in the same install run is configured —
# already has the durable `.service.d/` drop-in in place by itself, with
# no dependency on postinst running at all.
#
# HOSERVA_TEST_ROOT points preinst's own file writes at a throwaway tree
# instead of real /etc, so the real preinst script runs end to end
# without ever touching this host's real systemd state — same reasoning
# as packaging/test-postinst-smartd-mask.sh running postinst for real.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
preinst="$script_dir/debian/preinst"

fail=0
note() { printf 'test-preinst-smartd-dropin: %s\n' "$*" >&2; }

bash -n "$preinst" 2>/dev/null || { note "FAIL: preinst is not even valid shell (bash -n)"; fail=1; }

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

test_root="$work/root"
dropin="$test_root/etc/systemd/system/smartmontools.service.d/hoserva-disable.conf"

HOSERVA_TEST_ROOT="$test_root" sh "$preinst" install

if [ ! -f "$dropin" ]; then
  note "FAIL: preinst never writes the smartmontools.service.d/ drop-in — smartmontools' own postinst (configured before hoserva's, since hoserva Depends: smartmontools) can start smartd with nothing in place yet to stop it, reintroducing the spindown-wake race postinst alone runs too late to close"
  fail=1
elif ! grep -qE '^ConditionPathExists=/nonexistent-hoserva-smartd-disabled$' "$dropin"; then
  note "FAIL: the smartmontools.service.d/ drop-in does not contain the expected ConditionPathExists override"
  fail=1
fi

# abort-upgrade is a rollback, not an install in progress — preinst must
# not react to it the same way (matching postinst's own configure-only
# case guard).
test_root2="$work/root-abort"
HOSERVA_TEST_ROOT="$test_root2" sh "$preinst" abort-upgrade "1.0.0"
if [ -e "$test_root2/etc/systemd/system/smartmontools.service.d/hoserva-disable.conf" ]; then
  note "FAIL: preinst wrote the drop-in on abort-upgrade, which is a rollback, not an install"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
