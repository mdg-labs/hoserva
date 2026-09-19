#!/usr/bin/env bash
# Q68: the .deb ships unattended-upgrades config for Debian security
# updates only, and Automatic-Reboot must be off — Hoserva never reboots
# on its own.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
conf="$script_dir/debian/51hoserva-unattended-upgrades"
control="$script_dir/debian/control"

fail=0
note() { printf 'test-unattended-upgrades: %s\n' "$*" >&2; }

if [ ! -f "$conf" ]; then
  note "FAIL: missing $conf"
  exit 1
fi

if ! grep -q 'unattended-upgrades' "$control"; then
  note "FAIL: packaging/debian/control does not Recommend unattended-upgrades (Q68)"
  fail=1
fi

if ! grep -qE 'Unattended-Upgrade::Automatic-Reboot[[:space:]]+"false"' "$conf"; then
  note "FAIL: Automatic-Reboot is not false — a kernel update would reboot mid-sync"
  fail=1
fi

if grep -qE 'Unattended-Upgrade::Automatic-Reboot[[:space:]]+"true"' "$conf"; then
  note "FAIL: Automatic-Reboot is true"
  fail=1
fi

if grep -qE 'label=Debian"' "$conf" && ! grep -qE 'label=Debian-Security' "$conf"; then
  note "FAIL: Origins-Pattern allows non-security Debian updates"
  fail=1
fi

if ! grep -q 'Debian-Security' "$conf"; then
  note "FAIL: config does not restrict unattended-upgrades to Debian security"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
