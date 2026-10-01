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
set -euo pipefail

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

clone="$work/repo"
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
[ -z "${STUB_FAIL:-}" ] || exit 2
: >../hoserva_0.0.0~beta.1_amd64.deb
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

out="$work/out"

PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null
assert_eq "$(tree_state)" "" "tree after a successful build"
assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog after a successful build"
[ -f "$out/hoserva_0.0.0~beta.1_amd64.deb" ] || { note "FAIL: the .deb was not moved to the output directory"; fail=1; }

rc=0
STUB_FAIL=1 PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null 2>&1 || rc=$?
assert_eq "$rc" "2" "exit status when dpkg-buildpackage fails"
assert_eq "$(tree_state)" "" "tree after a failed dpkg-buildpackage"
assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog after a failed dpkg-buildpackage"

rc=0
PATH="$nodpkg" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null 2>&1 || rc=$?
[ "$rc" -ne 0 ] || { note "FAIL: build-deb.sh exited 0 without dpkg-buildpackage"; fail=1; }
assert_eq "$(tree_state)" "" "tree when dpkg-buildpackage is missing"
assert_eq "$(cat "$clone/packaging/debian/changelog")" "$changelog_before" "changelog when dpkg-buildpackage is missing"

# The cleanup removes only the build's own named paths: tracked files in
# the same directories, and anything else untracked, survive.
echo keep >"$clone/packaging/debian/hoserva.service"
echo keep >"$clone/notes.txt"
git -C "$clone" add packaging/debian/hoserva.service
git -C "$clone" -c user.name=test -c user.email=test@example.com commit -q -m service
STUB_FAIL=1 PATH="$stubs:$PATH" "$clone/scripts/release/build-deb.sh" v0.0.0-beta.1 amd64 "$out" >/dev/null 2>&1 || true
assert_eq "$(cat "$clone/packaging/debian/hoserva.service")" "keep" "a tracked file beside the build outputs"
assert_eq "$(cat "$clone/notes.txt")" "keep" "an unrelated untracked file"

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
  if git -C "$repo_root" check-ignore -q -- "$path"; then
    note "FAIL: .gitignore hides $path"
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  exit 1
fi
note "ok"
