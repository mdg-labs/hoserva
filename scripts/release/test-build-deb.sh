#!/usr/bin/env bash
# Safety-critical test for scripts/release/build-deb.sh leaving the working
# tree exactly as it found it (issue #521, doc 06 §4, D20): a local build
# (make vm-deploy, make vm-suite) must restore the tracked
# packaging/debian/changelog and remove every file the build wrote — on
# success and on failure — so an orchestrate scratch clone never commits
# build debris. dpkg-buildpackage is stubbed with one that writes the
# outputs the real one leaves behind; the stub also refuses to run unless
# build-deb.sh already rewrote the changelog, so the test cannot pass by
# the script skipping the rewrite.
#
# Issue #525 covers the other half: a build must write nothing into the
# clone's parent directory, which orchestrate's scratch clones (and any
# two builds on one host) share. The stub puts the .changes, .buildinfo
# and .deb only where the real tools would be told to put them
# (--changes-file, --buildinfo-file, HOSERVA_DEB_DESTDIR for the .deb that
# packaging/debian/rules passes to dh_builddeb --destdir), and defaults to
# the parent directory, as the real ones do, when build-deb.sh does not
# redirect them.
set -euo pipefail

# Throwaway repos must not pick up the developer's signing config (tag.gpgsign, commit.gpgsign).
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"

fail=0
note() { printf 'test-build-deb: %s\n' "$*" >&2; }
assert_eq() {
  if [ "$1" != "$2" ]; then
    note "FAIL: $3: expected '$2', got '$1'"
    fail=1
  fi
}

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

parent="$work/parent"
clone="$parent/repo"
mkdir -p "$clone/scripts/release" "$clone/packaging/debian" "$clone/cmd/hoserva"
cp "$script_dir/build-deb.sh" "$script_dir/lib.sh" "$clone/scripts/release/"
cp "$repo_root/packaging/debian/changelog" "$clone/packaging/debian/"
cp "$repo_root/.gitignore" "$clone/"
echo 'package main' >"$clone/cmd/hoserva/main.go"
git -C "$clone" init -q
git -C "$clone" add -A
git -C "$clone" -c user.name=test -c user.email=test@example.com commit -q -m base
changelog_before="$(cat "$clone/packaging/debian/changelog")"

stubs="$work/stubs"
mkdir -p "$stubs"
cat >"$stubs/dpkg-buildpackage" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
head -n1 debian/changelog | grep -q '^hoserva (0\.0\.0~beta\.1) ' || exit 99
changes=../hoserva_0.0.0~beta.1_amd64.changes
buildinfo=../hoserva_0.0.0~beta.1_amd64.buildinfo
changes_dir=..
buildinfo_dir=..
for a in "$@"; do
  case "$a" in
    --changes-file=*) changes="${a#--changes-file=}" ;;
    --buildinfo-file=*) buildinfo="${a#--buildinfo-file=}" ;;
    --changes-option=-u*) changes_dir="${a#--changes-option=-u}" ;;
    --buildinfo-option=-u*) buildinfo_dir="${a#--buildinfo-option=-u}" ;;
  esac
done
deb_dir="${HOSERVA_DEB_DESTDIR:-..}"
: >hoservad
: >hoserva
mkdir -p debian/.debhelper debian/hoserva/usr/bin
: >debian/.debhelper/generated
: >debian/hoserva/usr/bin/hoservad
: >debian/debhelper-build-stamp
: >debian/files
: >debian/hoserva.debhelper.log
: >debian/hoserva.postrm.debhelper
: >debian/hoserva.substvars
[ "${STUB_FAIL:-}" != early ] || exit 2
printf '%s\n' "$PWD" >"$deb_dir/hoserva_0.0.0~beta.1_amd64.deb"
# dpkg-genchanges and dpkg-genbuildinfo look for the .deb in the
# directory their -u option names, ".." by default.
[ "$changes_dir" = "$deb_dir" ] || exit 97
[ "$buildinfo_dir" = "$deb_dir" ] || exit 97
: >"$changes"
: >"$buildinfo"
if [ -n "${STUB_HOLD:-}" ]; then
  : >"$STUB_HOLD.started"
  for _ in $(seq 300); do
    [ -e "$STUB_HOLD.go" ] && break
    sleep 0.1
  done
  [ -e "$STUB_HOLD.go" ] || exit 3
fi
[ "${STUB_FAIL:-}" != late ] || exit 2
STUB
chmod +x "$stubs/dpkg-buildpackage"

# A PATH holding the tools build-deb.sh itself needs, but no
# dpkg-buildpackage — the host may well have a real one on its own PATH.
nodpkg="$work/nodpkg"
mkdir -p "$nodpkg"
for tool in bash env dirname date cat cp mv rm mkdir ln mktemp; do
  ln -s "$(command -v "$tool")" "$nodpkg/$tool"
done

# --ignored reports ignored leftovers too, so cleanup is asserted
# independently of what .gitignore hides.
tree_state() { git -C "$clone" status --porcelain --ignored; }

# Every hoserva_* file in the clones' shared parent directory, with its
# checksum, so a file written, replaced or deleted there shows up.
parent_state() { find "$parent" -maxdepth 1 -type f -name 'hoserva_*' -exec sha256sum {} + | sort; }

# The build's private directory comes from mktemp; pointing TMPDIR at an
# empty directory of its own shows whether the build removed it.
export TMPDIR="$work/tmp"
mkdir -p "$TMPDIR"
tmp_state() { ls -A "$TMPDIR"; }

# Files a sibling build, or an earlier one, left in the parent directory —
# including ones named exactly like this build's outputs. The build must
# neither write nor delete there.
for f in hoserva_0.0.0~beta.1_amd64.deb hoserva_0.0.0~beta.1_amd64.changes \
  hoserva_0.0.0~beta.1_amd64.buildinfo hoserva_9.9.9_arm64.changes; do
  echo "pre-existing $f" >"$parent/$f"
done
parent_before="$(parent_state)"

out="$work/out"

PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null
assert_eq "$(tree_state)" "" "tree after a successful build"
assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog after a successful build"
assert_eq "$(cat "$out/hoserva_0.0.0~beta.1_amd64.deb" 2>/dev/null)" "$clone" "the .deb in the output directory after a successful build"
assert_eq "$(ls -A "$out")" "hoserva_0.0.0~beta.1_amd64.deb" "the output directory holds exactly the .deb"
assert_eq "$(parent_state)" "$parent_before" "the parent directory after a successful build"
assert_eq "$(tmp_state)" "" "the build's temporary directory after a successful build"

for mode in early late; do
  rc=0
  STUB_FAIL=$mode PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$work/out-$mode" >/dev/null 2>&1 || rc=$?
  assert_eq "$rc" "2" "exit status when dpkg-buildpackage fails ($mode)"
  assert_eq "$(tree_state)" "" "tree after a failed dpkg-buildpackage ($mode)"
  assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog after a failed dpkg-buildpackage ($mode)"
  assert_eq "$(parent_state)" "$parent_before" "the parent directory after a failed dpkg-buildpackage ($mode)"
  assert_eq "$(tmp_state)" "" "the build's temporary directory after a failed dpkg-buildpackage ($mode)"
  [ ! -e "$work/out-$mode" ] || { note "FAIL: a failed build ($mode) created its output directory"; fail=1; }
done

rc=0
PATH="$nodpkg" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null 2>&1 || rc=$?
[ "$rc" -ne 0 ] || { note "FAIL: build-deb.sh exited 0 without dpkg-buildpackage"; fail=1; }
assert_eq "$(tree_state)" "" "tree when dpkg-buildpackage is missing"
assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog when dpkg-buildpackage is missing"
assert_eq "$(parent_state)" "$parent_before" "the parent directory when dpkg-buildpackage is missing"
assert_eq "$(tmp_state)" "" "the build's temporary directory when dpkg-buildpackage is missing"

# The cleanup removes only the build's own named paths: tracked files in
# the same directories, and anything else untracked, survive.
echo keep >"$clone/packaging/debian/hoserva.service"
echo keep >"$clone/notes.txt"
git -C "$clone" add packaging/debian/hoserva.service
git -C "$clone" -c user.name=test -c user.email=test@example.com commit -q -m service
STUB_FAIL=late PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null 2>&1 || true
assert_eq "$(cat "$clone/packaging/debian/hoserva.service")" "keep" "a tracked file beside the build outputs"
assert_eq "$(cat "$clone/notes.txt")" "keep" "an unrelated untracked file"

# Two builds at once from sibling clones, which share a parent directory:
# A has produced its .deb and is held at the end of dpkg-buildpackage while
# B builds and finishes. Each must end with its own .deb in its own
# output directory.
clone2="$parent/repo2"
git clone -q "$clone" "$clone2"
rm -f "$work/hold.started" "$work/hold.go"
rcA=0
STUB_HOLD="$work/hold" PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$work/out-a" >/dev/null 2>&1 &
pidA=$!
for _ in $(seq 300); do
  [ -e "$work/hold.started" ] && break
  sleep 0.1
done
[ -e "$work/hold.started" ] || { note "FAIL: the first concurrent build never reached its hold"; fail=1; }
rcB=0
PATH="$stubs:$PATH" "$clone2/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$work/out-b" >/dev/null 2>&1 || rcB=$?
: >"$work/hold.go"
wait "$pidA" || rcA=$?
assert_eq "$rcA" "0" "exit status of the first concurrent build"
assert_eq "$rcB" "0" "exit status of the second concurrent build"
assert_eq "$(cat "$work/out-a/hoserva_0.0.0~beta.1_amd64.deb" 2>/dev/null)" "$clone" "the first concurrent build's .deb"
assert_eq "$(cat "$work/out-b/hoserva_0.0.0~beta.1_amd64.deb" 2>/dev/null)" "$clone2" "the second concurrent build's .deb"
assert_eq "$(parent_state)" "$parent_before" "the parent directory after two concurrent builds"
assert_eq "$(tmp_state)" "" "the temporary directories after two concurrent builds"

# packaging/debian/rules points dh_builddeb at the directory build-deb.sh
# exports, and at the parent directory when none is set — the default of a
# maintainer's own dpkg-buildpackage.
if command -v make >/dev/null 2>&1 && [ -f /usr/share/dpkg/architecture.mk ]; then
  rules="$repo_root/packaging/debian/rules"
  # make's own flags and level leak in when this runs under `make packaging-test`.
  rules_destdir() {
    env -u MAKEFLAGS -u MAKELEVEL -u MFLAGS -u HOSERVA_DEB_DESTDIR "$@" \
      make --no-print-directory -n -f "$rules" override_dh_builddeb 2>&1
  }
  assert_eq "$(rules_destdir HOSERVA_DEB_DESTDIR=/some/dir)" "dh_builddeb --destdir=/some/dir" "dh_builddeb destination when HOSERVA_DEB_DESTDIR is set"
  assert_eq "$(rules_destdir)" "dh_builddeb --destdir=.." "dh_builddeb destination when HOSERVA_DEB_DESTDIR is unset"
  assert_eq "$(rules_destdir HOSERVA_DEB_DESTDIR=)" "dh_builddeb --destdir=.." "dh_builddeb destination when HOSERVA_DEB_DESTDIR is empty"
else
  note "FAIL: make or /usr/share/dpkg/architecture.mk (dpkg-dev) is missing, so packaging/debian/rules cannot be checked"
  fail=1
fi

# .gitignore, as defence in depth beside the cleanup. cmd/hoserva/ is a
# source directory and must stay visible to git.
for path in hoserva hoservad packaging/debian/.debhelper/x packaging/debian/debhelper-build-stamp \
  packaging/debian/files packaging/debian/hoserva.debhelper.log packaging/debian/hoserva.postrm.debhelper \
  packaging/debian/hoserva.substvars packaging/debian/hoserva/usr/bin/hoservad; do
  if ! git -C "$repo_root" check-ignore -q -- "$path"; then
    note "FAIL: .gitignore does not cover $path"
    fail=1
  fi
done
for path in cmd/hoserva/main.go cmd/hoservad/main.go; do
  if git -C "$repo_root" check-ignore --no-index -q -- "$path"; then
    note "FAIL: .gitignore hides $path"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
note "ok"
