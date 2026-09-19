#!/usr/bin/env bash
# Safety-critical reproduction test for issue #161's own blocking review
# finding: packaging/test-postinst-smartd-mask.sh proves postinst masks
# and stops smartmontools.service on a fresh install, but that plain
# mask does not survive a *future* smartmontools upgrade — smartmontools
# 7.4-3's own postinst opens with `deb-systemd-helper unmask
# smartmontools.service`, which deletes exactly the mask symlink
# postinst creates, before re-enabling and restarting the unit. This
# test reproduces that exact regression against the real smartmontools
# package and the real deb-systemd-helper (init-system-helpers), inside
# a throwaway `debian:trixie-slim` container — never against this host's
# real systemd or package state (CLAUDE.md) — and confirms postinst's
# `smartmontools.service.d/hoserva-disable.conf` drop-in survives the
# reinstall that removes the mask, because `deb-systemd-helper unmask`
# only ever touches the unit's own mask symlink, never a `.service.d/`
# override directory.
#
# deb-systemd-helper refuses to act unless DPKG_MAINTSCRIPT_PACKAGE is
# set (it detects being run outside a real dpkg maintainer-script
# context and no-ops otherwise) — real dpkg sets this when it invokes
# postinst, so the container script sets it the same way.
#
# Requires Docker; skipped (not failed) when it isn't available, same
# posture as scripts/release/test-lib.sh skipping its dpkg-only check.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
postinst_dir="$script_dir/debian"

fail=0
note() { printf 'test-postinst-smartd-survives-upgrade: %s\n' "$*" >&2; }

if ! command -v docker >/dev/null 2>&1; then
  note "SKIP: docker not available in this environment"
  exit 0
fi

container_script='
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends smartmontools adduser >/dev/null

addgroup --quiet --system hoserva
export DPKG_MAINTSCRIPT_PACKAGE=hoserva
export DPKG_MAINTSCRIPT_NAME=postinst
sh /pkg/postinst configure ""

# Sanity: the fresh install actually masked the unit, exactly as
# packaging/test-postinst-smartd-mask.sh already proves in isolation —
# if this is not true, the reinstall step below proves nothing.
if [ "$(readlink -f /etc/systemd/system/smartmontools.service)" != "/dev/null" ]; then
  echo "SETUP-FAIL: postinst did not mask smartmontools.service before the reinstall" >&2
  exit 2
fi
if [ ! -f /etc/systemd/system/smartmontools.service.d/hoserva-disable.conf ]; then
  echo "SETUP-FAIL: postinst did not write the durable drop-in before the reinstall" >&2
  exit 2
fi

# The exact regression scenario: an unrelated later smartmontools
# update, not a hoserva install/upgrade — hoserva postinst does not run
# again here.
apt-get install -y -qq --reinstall smartmontools >/dev/null

# Reproduces the reported bug: the plain mask is gone and the unit is
# enabled again — smartmontools own postinst removed the mask symlink
# and re-enabled the unit, exactly as the finding describes.
if [ -e /etc/systemd/system/smartmontools.service ]; then
  echo "UNEXPECTED: the plain mask symlink survived the reinstall — the reproduction itself is stale, not the fix" >&2
  exit 3
fi
if [ ! -e /etc/systemd/system/multi-user.target.wants/smartmontools.service ]; then
  echo "UNEXPECTED: smartmontools reinstall did not re-enable the unit — the reproduction itself is stale, not the fix" >&2
  exit 3
fi

# The fix: the drop-in with its condition override is untouched by the
# reinstall, so the unit still cannot actually start.
if [ ! -f /etc/systemd/system/smartmontools.service.d/hoserva-disable.conf ]; then
  echo "FIX-FAIL: the hoserva drop-in did not survive the smartmontools reinstall" >&2
  exit 1
fi
if ! grep -qE "^ConditionPathExists=/nonexistent-hoserva-smartd-disabled$" /etc/systemd/system/smartmontools.service.d/hoserva-disable.conf; then
  echo "FIX-FAIL: the surviving drop-in no longer contains the expected ConditionPathExists override" >&2
  exit 1
fi

echo "OK: drop-in survived the smartmontools reinstall with its condition intact"
'

if ! timeout 180 docker run --rm \
  -v "$postinst_dir:/pkg:ro" \
  debian:trixie-slim bash -c "$container_script"; then
  note "FAIL: the durable suppression did not survive a real smartmontools reinstall in a throwaway debian:trixie-slim container — see output above"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
