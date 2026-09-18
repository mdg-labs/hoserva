#!/usr/bin/env bash
# postinst never touches a data disk (issue #42 acceptance criterion) and
# never generates the machine key itself (Q28's "Implementation note
# (#22)": hoservad generates it, once, at its own first start — see
# packaging/debian/postinst's own comment for why). Static checks only:
# postinst's one piece of real behaviour (creating the `hoserva` group)
# needs `addgroup`/`getent`, which this sandbox may not be allowed to run
# for real, so this test never executes it — see
# scripts/release/test-postrm-purge.sh for the one maintainer-script test
# that does run its subject, against a throwaway tree.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
postinst="$repo_root/packaging/debian/postinst"

fail=0
note() { printf 'test-postinst: %s\n' "$*" >&2; }

bash -n "$postinst" 2>/dev/null || { note "FAIL: postinst is not even valid shell (bash -n)"; fail=1; }

code="$(grep -vE '^\s*#' "$postinst")"

if grep -q '/mnt' <<<"$code"; then
  note "FAIL: postinst's code references /mnt — it must never touch a data disk"
  fail=1
fi

# Q76: installing onto a Debian system already in use must never
# overwrite existing Samba, NFS, fstab or Docker configuration.
if grep -qE 'smb\.conf|exports|fstab|/var/lib/docker|docker-compose' <<<"$code"; then
  note "FAIL: postinst references existing system configuration it must never touch (Q76)"
  fail=1
fi

if grep -qE 'secret\.key|urandom|/dev/random' <<<"$code"; then
  note "FAIL: postinst generates the machine key itself — that duplicates internal/auth.LoadOrGenerateMachineKey (Q28)"
  fail=1
fi

if ! grep -q 'addgroup' <<<"$code"; then
  note "FAIL: postinst does not create the hoserva group"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
