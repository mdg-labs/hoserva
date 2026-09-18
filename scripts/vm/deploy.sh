#!/usr/bin/env bash
# `make vm-deploy` (doc 06 §4): builds (or reuses) the .deb under test and
# installs it into this lab's own running VM over SSH — the same package
# a user would install, never a hand-copied binary.
#
# DEB=<path> skips the build and deploys an already-built .deb (the
# release pipeline's own artifact, or one built by a previous vm-deploy
# run) — useful when iterating on the harness itself without rebuilding.
# Without it, this calls scripts/release/build-deb.sh directly rather
# than a Makefile `deb` target: no such target exists yet (Makefile's own
# header comment — "vm-*, deb, iso, ... arrive with the issues that build
# what they need" — and this issue's scope is scripts/vm/, not
# packaging/), and build-deb.sh already needs a released version string,
# never a hand-maintained one, from the same source every other build
# step reads (doc 12 §6): the latest 'v*' tag, matching api-check's own
# "no release yet" fallback in the Makefile. Building needs
# dpkg-buildpackage, debhelper and fakeroot, which — same as
# scripts/release/build-deb.sh's own header comment records — CI
# installs and the dev host and lab deliberately do not; this script
# fails loudly there rather than reaching for an install of its own.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"

vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' or 'make vm-restore' first"

DEB="${DEB:-}"
if [[ -z "$DEB" ]]; then
  TAG="${TAG:-}"
  if [[ -z "$TAG" ]]; then
    TAG="$(git -C "$VM_REPO_ROOT" tag --list 'v*' --sort=-v:refname | head -n1)"
    [[ -n "$TAG" ]] || die "no released tag to build against (no 'v*' git tag) and no TAG= given — pass TAG=v0.0.0-beta.1 for a dev build, or DEB=<path> to deploy an already-built .deb"
  fi
  echo "vm-deploy[$HOSERVA_LAB_ID]: building the .deb for $TAG (amd64)"
  BUILD_OUT="$VM_STATE_DIR/deb-build"
  mkdir -p -- "$BUILD_OUT"
  "$VM_REPO_ROOT/scripts/release/build-deb.sh" "$TAG" amd64 "$BUILD_OUT"
  DEB="$(find "$BUILD_OUT" -maxdepth 1 -name '*.deb' -print -quit)"
  [[ -n "$DEB" ]] || die "build-deb.sh reported success but no .deb was found in $BUILD_OUT"
else
  [[ -f "$DEB" ]] || die "DEB='$DEB' does not exist"
fi

echo "vm-deploy[$HOSERVA_LAB_ID]: copying $(basename "$DEB") to the guest"
vm_ssh 'mkdir -p /tmp/hoserva-deploy'
vm_scp "$DEB" "hoserva@127.0.0.1:/tmp/hoserva-deploy/hoserva.deb"

echo "vm-deploy[$HOSERVA_LAB_ID]: installing (apt resolves mergerfs/snapraid from Debian's own repos)"
vm_ssh 'sudo apt-get update -qq && sudo apt-get install -y -qq /tmp/hoserva-deploy/hoserva.deb'

echo "vm-deploy[$HOSERVA_LAB_ID]: verifying the hoservad service is active"
vm_ssh 'sudo systemctl is-active hoservad'

echo "vm-deploy[$HOSERVA_LAB_ID]: done — hoservad TLS: https://127.0.0.1:$VM_HTTPS_PORT"
