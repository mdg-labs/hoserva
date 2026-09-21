#!/usr/bin/env bash
# Reproduction test for issue #48 (Q26 ownership model): postinst must
# create the `hoserva-apps` system user pinned to UID 99 when that UID
# is free, and must report — never override — when it isn't.
#
# Every command postinst can call (getent, addgroup, adduser,
# deb-systemd-helper, deb-systemd-invoke) is stubbed out in a throwaway
# bin/ directory prepended to PATH, and HOSERVA_TEST_ROOT points
# postinst's own file writes at a throwaway tree instead of real /etc,
# so the real postinst script runs end to end without ever touching
# this host's real system passwd/group table — same reasoning as
# packaging/test-postinst-smartd-mask.sh running its subject for real.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
postinst="$script_dir/debian/postinst"

fail=0
note() { printf 'test-postinst-hoserva-apps: %s\n' "$*" >&2; }

bash -n "$postinst" 2>/dev/null || { note "FAIL: postinst is not even valid shell (bash -n)"; fail=1; }

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

stub_bin="$work/bin"
mkdir -p "$stub_bin"

cat >"$stub_bin/addgroup" <<'EOF'
#!/bin/sh
echo "addgroup $*" >>"$HOSERVA_TEST_LOG"
exit 0
EOF
cat >"$stub_bin/adduser" <<'EOF'
#!/bin/sh
echo "adduser $*" >>"$HOSERVA_TEST_LOG"
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
chmod +x "$stub_bin"/addgroup "$stub_bin/adduser" "$stub_bin/deb-systemd-helper" "$stub_bin/deb-systemd-invoke"

# write_getent_stub($1=uid_99_status, $2=hoserva_apps_status): each
# status is an exit code — 0 for "found", 2 for "not found" (matching
# the real getent's own convention, and packaging/test-postinst-smartd-mask.sh's
# stub for the group case).
write_getent_stub() {
  local uid99_exit="$1" hoservaapps_exit="$2"
  cat >"$stub_bin/getent" <<EOF
#!/bin/sh
echo "getent \$*" >>"\$HOSERVA_TEST_LOG"
case "\$1 \$2" in
  "group hoserva") exit 2 ;;
  "passwd hoserva-apps") exit $hoservaapps_exit ;;
  "passwd 99") exit $uid99_exit ;;
esac
exit 2
EOF
  chmod +x "$stub_bin/getent"
}

run_postinst() {
  local test_root="$1" log="$2"
  : >"$log"
  HOSERVA_TEST_LOG="$log" HOSERVA_TEST_ROOT="$test_root" PATH="$stub_bin:$PATH" sh "$postinst" configure ""
}

# Case 1: UID 99 is free — hoserva-apps must be created, pinned to that
# UID, in the shared `users` group (GID 100, Q26).
write_getent_stub 2 2
log1="$work/calls-free.log"
run_postinst "$work/root-free" "$log1"

if ! grep -qE '^adduser .*--uid 99 .*--ingroup users .* hoserva-apps$' "$log1"; then
  note "FAIL: UID 99 free but postinst never creates hoserva-apps pinned to UID 99 in group users — migrated Unraid data (nobody:users, 99:100) and PUID=99/PGID=100 templates would need remapping for nothing"
  fail=1
fi

# Case 2: UID 99 is already taken by a different account — postinst
# must report this, and must never create or otherwise touch that UID.
write_getent_stub 0 2
log2="$work/calls-taken.log"
run_postinst "$work/root-taken" "$log2" 2>"$work/stderr-taken.log"

if grep -q '^adduser ' "$log2"; then
  note "FAIL: UID 99 is already taken by another account but postinst called adduser anyway — this must never override an existing account"
  fail=1
fi
if ! grep -qi 'UID 99' "$work/stderr-taken.log"; then
  note "FAIL: UID 99 is already taken but postinst printed no pre-flight report about it"
  fail=1
fi

# Case 3: hoserva-apps already exists (an earlier install or
# reconfigure) — postinst must no-op, not attempt to recreate it.
write_getent_stub 0 0
log3="$work/calls-exists.log"
run_postinst "$work/root-exists" "$log3"

if grep -q '^adduser ' "$log3"; then
  note "FAIL: hoserva-apps already exists but postinst tried to create it again"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  note "PASS"
fi
exit "$fail"
