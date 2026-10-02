#!/usr/bin/env bash
# Self-check of scripts/devenv/unraid-fixture.sh (doc 06 §5, issue #74), run
# inside the lab by `make lab-unraid-verify`. It proves the builder's guards
# can refuse and its --verify can fail, not only that they happened to pass:
#
#   - a loop device is refused unless it is the only one backed by one of this
#     variant's own images, and the L3 guard refuses everything that is not
#     one of the guest's virtio array disks;
#   - the L3 tier refuses to start outside a virtual machine;
#   - a tiny variant built in a scratch fixtures directory verifies clean, and
#     then fails --verify when a manifest hash is changed, when the parity disk
#     no longer holds the XOR of the data disks, and when a data file is
#     changed on disk.
#
# Runs only inside the hoserva-lab container, only under this lab's own $LAB.
set -euo pipefail

HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BUILDER="$HERE/unraid-fixture.sh"
# shellcheck source=scripts/devenv/unraid-fixture.sh
source "$BUILDER"

: "${HOSERVA_LAB_ID:?set HOSERVA_LAB_ID}"
LAB="/lab/$HOSERVA_LAB_ID"
mountpoint -q "$LAB" || die "$LAB is not this lab's mount: run inside the lab container"

fail=0
ok() { echo "ok: $*"; }
bad() {
  echo "FAIL: $*"
  fail=1
}

work=$(mktemp -d "$LAB/unraid-selftest.XXXXXX")
fixtures="$work/fixtures"
variant=unraid-selftest
OUT="$LAB/unraid/$variant"
LOOPS_TO_DETACH=()

cleanup_test() {
  local dev
  set +e
  for dev in "${LOOPS_TO_DETACH[@]}"; do losetup -d "$dev" 2>/dev/null; done
  if [[ -n ${tdev:-} ]]; then
    umount "$work/rw" 2>/dev/null
    losetup -d "$tdev" 2>/dev/null
  fi
  rm -rf -- "${LAB:?}/unraid/$variant" "${LAB:?}/unraid/unraid-guard" "$work"
}
trap cleanup_test EXIT

# ---- the guards, called as functions

# shellcheck disable=SC2034 # read by the sourced guard functions
TIER=l2
# shellcheck disable=SC2034
VARIANT=unraid-guard
IMG="$LAB/unraid/unraid-guard/img"
mkdir -p -- "$IMG"
truncate -s 8M "$IMG/disk1.img"
own=$(losetup --find --show "$IMG/disk1.img")
LOOPS_TO_DETACH+=("$own")
truncate -s 8M "$work/outside.img"
outside=$(losetup --find --show "$work/outside.img")
LOOPS_TO_DETACH+=("$outside")

if (assert_own_loop "$own" "$IMG/disk1.img") >/dev/null 2>&1; then
  ok "a loop device over this variant's own image is accepted"
else
  bad "the guard refused a loop device over this variant's own image"
fi

refused() {  # description, command...
  local desc=$1
  shift
  if ("$@") >/dev/null 2>&1; then bad "$desc was accepted"; else ok "$desc is refused"; fi
}

refused "an image outside the variant's img directory" assert_own_loop "$outside" "$work/outside.img"
refused "a loop device that is not the image's own" assert_own_loop "$outside" "$IMG/disk1.img"
refused "a block device that is not a loop device" assert_own_loop /dev/sda "$IMG/disk1.img"
refused "an image path that escapes through .." assert_own_loop "$own" "$IMG/../../escape.img"

# shellcheck disable=SC2154,SC2034 # the associative array is the builder's
D_TARGET[disk1]=disk1
TIER=l3
refused "a loop device on the L3 tier" assert_own_virtio disk1 "$own"
refused "the guest's OS disk" assert_own_virtio disk1 /dev/vda
refused "a virtio disk that does not exist" assert_own_virtio disk1 /dev/vdb
# shellcheck disable=SC2034
TIER=l2

# ---- the tiers

if out=$(bash "$BUILDER" --tier l3 "$variant" 2>&1); then
  bad "the l3 tier ran inside the lab container"
elif [[ $out == *"not inside a virtual machine"* ]]; then
  ok "the l3 tier refuses to start outside a virtual machine"
else
  bad "the l3 tier failed for another reason: $out"
fi

# ---- a tiny variant: build, verify, tamper

mkdir -p -- "$fixtures/$variant"
cp -a -- "$HERE/../../testdata/unraid-fixtures/common" "$fixtures/"
cat >"$fixtures/$variant/spec" <<'EOF'
unraid_version=6.12.15
unraid_release=2025-03-25
expect_parity_signature=xfs
disk parity kind=parity fs=none size=384M target=parity1
disk disk1  kind=data   fs=xfs  size=320M target=disk1
include ../common/spec-flash
EOF
cat >"$fixtures/$variant/seed" <<'EOF'
file|disk1|media/a.bin|size=100000
text|disk1|media/b.txt|content=hello\n
EOF

export HOSERVA_FIXTURES_DIR="$fixtures"
export HOSERVA_FIXTURE_NO_CAPTURE=1
rm -rf -- "${LAB:?}/unraid/$variant"

run() { bash "$BUILDER" --tier l2 "$@" 2>&1; }

if out=$(run "$variant"); then ok "the tiny variant builds"; else
  bad "the tiny variant did not build: $out"
  exit 1
fi
if out=$(run "$variant"); then bad "a second build over the first was accepted"; else
  if [[ $out == *"already holds a build"* ]]; then ok "a second build over the first is refused"; else bad "the second build failed for another reason: $out"; fi
fi
if out=$(run --verify "$variant"); then ok "an untouched build verifies"; else
  bad "an untouched build did not verify: $out"
  exit 1
fi

expected="$OUT/expected"
cp -- "$expected/manifest.sha256" "$work/manifest.orig"
first=$(grep -v '^#' "$expected/manifest.sha256" | head -n 1 | cut -c1)
flip=a
[[ $first == a ]] && flip=b
sed -i "0,/^[0-9a-f]/s/^[0-9a-f]/$flip/" "$expected/manifest.sha256"
if out=$(run --verify "$variant"); then bad "verify accepted a manifest with one changed hash"; else
  if [[ $out == *"manifest.sha256 differs"* ]]; then ok "verify fails when a manifest hash is changed"; else bad "verify failed for another reason: $out"; fi
fi
cp -- "$work/manifest.orig" "$expected/manifest.sha256"

# the parity disk: change one byte inside the compared window
read -r _ pstart _ < <(sfdisk -d "$OUT/img/parity.img" | sed -n 's/.*start= *\([0-9]*\), size= *\([0-9]*\),.*/x \1 \2/p')
poke_at=$((pstart * 512 + 300000))
orig=$(dd if="$OUT/img/parity.img" bs=1 skip="$poke_at" count=1 status=none | od -An -tu1 | tr -d ' ')
printf '%b' "$(printf '\\x%02x' $((255 - orig)))" | dd of="$OUT/img/parity.img" bs=1 seek="$poke_at" conv=notrunc status=none
if out=$(run --verify "$variant"); then bad "verify accepted a parity disk that is not the XOR of the data disks"; else
  if [[ $out == *"not the XOR of the data disks"* ]]; then ok "verify fails when the parity disk is not the XOR of the data disks"; else bad "verify failed for another reason: $out"; fi
fi
printf '%b' "$(printf '\\x%02x' "$orig")" | dd of="$OUT/img/parity.img" bs=1 seek="$poke_at" conv=notrunc status=none
if out=$(run --verify "$variant"); then ok "verify passes again once the parity byte is restored"; else bad "verify did not pass after the parity byte was restored: $out"; fi

# a data file changed on disk, behind the manifest's back
read -r _ dstart dsize < <(sfdisk -d "$OUT/img/disk1.img" | sed -n 's/.*start= *\([0-9]*\), size= *\([0-9]*\),.*/x \1 \2/p')
tdev=$(losetup --find --show --offset $((dstart * 512)) --sizelimit $((dsize * 512)) "$OUT/img/disk1.img")
mkdir -p -- "$work/rw"
mount -t xfs -o nouuid "$tdev" "$work/rw"
printf 'X' | dd of="$work/rw/media/b.txt" bs=1 count=1 conv=notrunc status=none
sync
umount "$work/rw"
losetup -d "$tdev"
tdev=""
if out=$(run --verify "$variant"); then bad "verify accepted a data file changed on disk"; else
  ok "verify fails when a data file is changed on disk"
fi

if ((fail)); then
  echo "unraid-fixture self-check: FAILED"
  exit 1
fi
echo "unraid-fixture self-check: all checks passed"
