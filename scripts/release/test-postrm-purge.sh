#!/usr/bin/env bash
# Safety-critical data-loss reproduction test for
# packaging/debian/postrm's `purge` case (issue #42, doc 04 §2): purging
# hoserva must remove its own state under /etc/hoserva (Q28's machine
# key) and /var/lib/hoserva, but must never remove
# /var/lib/hoserva/stacks/ — a user's own Docker Compose stacks — and
# must never touch anything under /mnt, on any path, ever. postrm reads
# HOSERVA_TEST_ROOT (unset in production) so this test can run the real
# maintainer script against a throwaway tree instead of a real root
# filesystem.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
postrm="$repo_root/packaging/debian/postrm"

fail=0
note() { printf 'test-postrm-purge: %s\n' "$*" >&2; }
assert_exists() {
  if [ ! -e "$1" ]; then
    note "FAIL: expected $1 to still exist, but it is gone"
    fail=1
  fi
}
assert_gone() {
  if [ -e "$1" ]; then
    note "FAIL: expected $1 to be gone, but it still exists"
    fail=1
  fi
}
assert_file_content() {
  if [ ! -f "$1" ] || [ "$(cat "$1")" != "$2" ]; then
    note "FAIL: expected $1 to contain '$2'"
    fail=1
  fi
}

root="$(mktemp -d)"
cleanup() { rm -rf "$root"; }
trap cleanup EXIT

# A populated installation: real state to purge, and a real stack — the
# thing that must survive — with real, checkable content in it.
mkdir -p "$root/etc/hoserva"
echo secret > "$root/etc/hoserva/secret.key"
mkdir -p "$root/var/lib/hoserva/stacks/plex"
echo 'services: {}' > "$root/var/lib/hoserva/stacks/plex/docker-compose.yml"
echo 'PLEX_CLAIM=abc123' > "$root/var/lib/hoserva/stacks/plex/.env"
mkdir -p "$root/var/lib/hoserva/jobs"
echo joblog > "$root/var/lib/hoserva/jobs/sync-1.log"
mkdir -p "$root/var/lib/hoserva/catalog"
echo catalog > "$root/var/lib/hoserva/catalog/index.json"
echo dbfile > "$root/var/lib/hoserva/hoserva.db"

HOSERVA_TEST_ROOT="$root" "$postrm" purge

# The stack survives purge, byte for byte — the actual acceptance
# criterion ("purge never removes /var/lib/hoserva/stacks/").
assert_exists "$root/var/lib/hoserva/stacks/plex/docker-compose.yml"
assert_exists "$root/var/lib/hoserva/stacks/plex/.env"
assert_file_content "$root/var/lib/hoserva/stacks/plex/docker-compose.yml" 'services: {}'
assert_file_content "$root/var/lib/hoserva/stacks/plex/.env" 'PLEX_CLAIM=abc123'

# Everything else hoserva's own purge is actually responsible for
# cleaning up is gone.
assert_gone "$root/etc/hoserva"
assert_gone "$root/var/lib/hoserva/jobs/sync-1.log"
assert_gone "$root/var/lib/hoserva/catalog/index.json"
assert_gone "$root/var/lib/hoserva/hoserva.db"

# purge must never reference a data-disk path in its actual code — a
# static check on top of the behavioural one above, since /mnt has no
# reason to ever appear there (comments explaining that absence are
# fine, so comment lines are excluded).
if grep -vE '^\s*#' "$postrm" | grep -q '/mnt'; then
  note "FAIL: postrm's code references /mnt — purge must never touch a data disk"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
