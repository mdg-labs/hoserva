#!/usr/bin/env bash
# Safety-critical reproduction test for issue #161: a fresh install must
# not leave smartmontools' own smartd service running on Debian's default
# unmanaged periodic-scan schedule (doc 08 §1's own named spindown-wake
# culprit) once `smartmontools` becomes a Depends: of the hoserva package
# (packaging/debian/control) alongside the smartctl/hdparm binaries
# internal/disk/linux_provider.go execs directly.
#
# Every command postinst can call (getent, addgroup, deb-systemd-helper,
# deb-systemd-invoke) is stubbed out in a throwaway bin/ directory
# prepended to PATH, and HOSERVA_TEST_ROOT points postinst's own file
# writes (the durable systemd drop-in) at a throwaway tree instead of
# real /etc, so the real postinst script runs end to end without ever
# touching this host's real system group table or systemd state — same
# reasoning as scripts/release/test-postrm-purge.sh running its subject
# for real against a throwaway root, and the same reason
# scripts/release/test-postinst.sh gives for *not* doing so itself
# (getent/addgroup would otherwise touch a real system group).
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
postinst="$script_dir/debian/postinst"

fail=0
note() { printf 'test-postinst-smartd-mask: %s\n' "$*" >&2; }

bash -n "$postinst" 2>/dev/null || { note "FAIL: postinst is not even valid shell (bash -n)"; fail=1; }

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

stub_bin="$work/bin"
log="$work/calls.log"
mkdir -p "$stub_bin"
: >"$log"

# getent reports the group as absent (exit 2, like the real getent when
# no such group exists) so postinst's own addgroup branch is exercised
# too, exactly as it would be on a fresh install.
cat >"$stub_bin/getent" <<'EOF'
#!/bin/sh
echo "getent $*" >>"$HOSERVA_TEST_LOG"
exit 2
EOF
cat >"$stub_bin/addgroup" <<'EOF'
#!/bin/sh
echo "addgroup $*" >>"$HOSERVA_TEST_LOG"
exit 0
EOF
cat >"$stub_bin/deb-systemd-helper" <<'EOF'
#!/bin/sh
echo "deb-systemd-helper $*" >>"$HOSERVA_TEST_LOG"
exit 0
EOF
cat >"$stub_bin/deb-systemd-invoke" <<'EOF'
#!/bin/sh
echo "deb-systemd-invoke $*" >>"$HOSERVA_TEST_LOG"
exit 0
EOF
chmod +x "$stub_bin"/*

test_root="$work/root"
dropin="$test_root/etc/systemd/system/smartmontools.service.d/hoserva-disable.conf"

HOSERVA_TEST_LOG="$log" HOSERVA_TEST_ROOT="$test_root" PATH="$stub_bin:$PATH" sh "$postinst" configure ""

if ! grep -qE '^deb-systemd-helper mask smartmontools\.service$' "$log"; then
  note "FAIL: postinst never masks smartmontools.service — a fresh install leaves smartd on its unmanaged default schedule, reintroducing the spindown-wake culprit doc 08 §1 already names"
  fail=1
fi

if ! grep -qE '^deb-systemd-invoke stop smartmontools\.service$' "$log"; then
  note "FAIL: postinst never stops smartmontools.service — smartmontools' own postinst (configured before this one, since hoserva Depends: smartmontools) already started it, and it would keep scanning through and after this install"
  fail=1
fi

# The plain mask above does not survive a future smartmontools upgrade
# (its own postinst unmasks before re-enabling and restarting the
# unit) — this drop-in is what does, since `deb-systemd-helper unmask`
# only ever removes the mask symlink, never a `.service.d/` override
# directory.
if [ ! -f "$dropin" ]; then
  note "FAIL: postinst never writes a smartmontools.service.d/ drop-in — the mask alone does not survive a future smartmontools upgrade re-enabling and restarting the unit, so smartd would return to its unmanaged default schedule with nothing re-suppressing it"
  fail=1
elif ! grep -qE '^ConditionPathExists=/nonexistent-hoserva-smartd-disabled$' "$dropin"; then
  note "FAIL: the smartmontools.service.d/ drop-in does not contain the expected ConditionPathExists override"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
