#!/usr/bin/env bash
# Self-check of how `make vm-up VARIANT=<variant>` sizes the guest's array
# disks from a fixture spec (doc 06 §5, issue #570). No VM, no libvirt and no
# real HOSERVA_LAB_ID: it reads the committed specs, and runs create-vm.sh
# against a scratch repository root, where every refusal under test comes
# before the first image would be fetched or created.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
real_root="$(cd "$script_dir/../.." && pwd)"
fail=0
ok() { echo "ok: $*"; }
bad() {
  echo "FAIL: $*"
  fail=1
}

work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
export HOSERVA_LAB_ID=unraid-sizing-check
export HOSERVA_VM_REPO_ROOT="$work/repo"

# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"
# shellcheck source=scripts/vm/unraid-lib.sh
source "$script_dir/unraid-lib.sh"
VM_REPO_ROOT="$real_root"

sizes=$(VARIANT=unraid-7x-xfs-single-parity unraid_spec_sizes)
expected=$'parity1 2576980377600\ndisk1 335544320\ndisk2 335544320\ndisk3 2469606195200\ncache 268435456'
if [[ $sizes == "$expected" ]]; then
  ok "the 7.x spec resolves to its own sizes, by target"
else
  bad "the 7.x spec resolved to: $sizes"
fi

# create-vm.sh must stop on each of these with the named message, and before
# it creates any image.
fixtures="$HOSERVA_VM_REPO_ROOT/testdata/unraid-fixtures"
refuse() {  # description, expected message, variant, [env...]
  local desc=$1 want=$2 variant=$3 out
  shift 3
  if out=$(env "$@" VARIANT="$variant" timeout 30 "$script_dir/create-vm.sh" 2>&1); then
    bad "$desc was accepted"
    return
  fi
  if [[ $out != *"$want"* ]]; then
    bad "$desc failed for another reason: $out"
    return
  fi
  if compgen -G "$HOSERVA_VM_REPO_ROOT/.vm/$HOSERVA_LAB_ID/img/*.qcow2" >/dev/null; then
    bad "$desc left a disk image behind"
    return
  fi
  ok "$desc is refused before any image is created"
}

mkdir -p -- "$fixtures/unraid-nosize" "$fixtures/unraid-badsize" "$fixtures/unraid-twice" "$fixtures/unraid-far" "$fixtures/unraid-empty"
printf 'disk disk1 kind=data fs=xfs target=disk1\n' >"$fixtures/unraid-nosize/spec"
printf 'disk disk1 kind=data fs=xfs size=12Q target=disk1\n' >"$fixtures/unraid-badsize/spec"
printf 'disk disk1 kind=data fs=xfs size=1G target=disk1\ndisk disk2 kind=data fs=xfs size=1G target=disk1\n' >"$fixtures/unraid-twice/spec"
printf 'disk disk9 kind=data fs=xfs size=1G target=disk9\n' >"$fixtures/unraid-far/spec"
printf 'unraid_version=6.12.15\n' >"$fixtures/unraid-empty/spec"

refuse "a variant without a directory" "no variant 'unraid-missing'" unraid-missing
refuse "a variant name that is not one" "invalid VARIANT" 'unraid-../x'
refuse "a disk line without a size" "needs size= and target=" unraid-nosize
refuse "a disk line with an unreadable size" "bad size '12Q'" unraid-badsize
refuse "two disks with one target" "is used by two disks" unraid-twice
refuse "a target no disk of the topology has" "targets disk 'disk9'" unraid-far
refuse "a target beyond the shared-nvme topology" "targets disk 'disk9'" unraid-far HOSERVA_VM_TOPOLOGY=shared-nvme
refuse "a spec with no disk lines" "has no disk lines" unraid-empty
cp -a -- "$real_root/testdata/unraid-fixtures/unraid-7x-xfs-single-parity" "$fixtures/"
refuse "a variant whose disk 3 the shared-nvme topology lacks" "targets disk 'disk3'" unraid-7x-xfs-single-parity HOSERVA_VM_TOPOLOGY=shared-nvme

exit "$fail"
