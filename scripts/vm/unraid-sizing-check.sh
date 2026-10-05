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
expected=$'parity1 2576980377600\ndisk1 335544320\ndisk2 335544320\ndisk3 2469606195200\ncache 60129542144'
if [[ $sizes == "$expected" ]]; then
  ok "the 7.x spec resolves to its own sizes, by target"
else
  bad "the 7.x spec resolved to: $sizes"
fi

# an option names the build's output directory and snapshot: the variant itself
# for the default option, <variant>-<option> for any other
run_name() {  # variant, option
  (
    VARIANT=$1 OPTION=$2
    unraid_require_variant
    printf '%s|%s' "$UNRAID_RUN" "$UNRAID_OPTION_ARG"
  )
}
for case in "unraid-with-vms||unraid-with-vms|" "unraid-with-vms|array|unraid-with-vms|--option 'array'" \
  "unraid-with-vms|alt|unraid-with-vms-alt|--option 'alt'" "unraid-with-vms|cache|unraid-with-vms-cache|--option 'cache'" \
  "unraid-no-cache||unraid-no-cache|"; do
  IFS='|' read -r v o want_run want_arg <<<"$case"
  if [[ $(run_name "$v" "$o") == "$want_run|$want_arg" ]]; then
    ok "$v${o:+ option $o} builds as $want_run"
  else
    bad "$v${o:+ option $o} resolved to $(run_name "$v" "$o")"
  fi
done
if out=$(run_name unraid-with-vms nope 2>&1); then bad "an option the variant does not have was accepted"; elif [[ $out == *"has no option 'nope'"* ]]; then ok "an option the variant does not have is refused"; else bad "an unknown option failed for another reason: $out"; fi
if out=$(run_name unraid-no-cache cache 2>&1); then bad "an option of a variant without options was accepted"; elif [[ $out == *"has no options"* ]]; then ok "an option of a variant without options is refused"; else bad "an option of a variant without options failed for another reason: $out"; fi

sizes=$(VARIANT=unraid-with-vms unraid_spec_sizes)
if [[ $sizes == $'parity1 402653184\ndisk1 335544320\ndisk2 335544320\ndisk3 335544320\ncache 60129542144' ]]; then
  ok "the unraid-with-vms spec resolves to its own sizes, by target"
else
  bad "the unraid-with-vms spec resolved to: $sizes"
fi

# The disks are sparse images, so each fixture is specced for what Hoserva does
# to it. mkfs.xfs refuses a filesystem under 300 MB, which a cache pool becomes
# at the point of no return. Every share is mounted with a 50G minfreespace, so
# a share on a pool with less than that free cannot take a new directory: a
# variant the suite imports needs, in its L3 guest, a cache (appdata moves onto
# it) and a data disk with more than that free, and a parity disk at least as
# large as its largest data disk. Whole-device hashes read every byte of an
# image, so the loop-device lab builds the same variant at its size= and only
# the L3 guest, which the suite uses, gets l3size=. A variant that refuses a
# disk is never imported and its cache only has to be formatted; unraid-with-vms
# is not run by the suite yet (migration-suite-coverage-check.sh names it as
# #97's), so its data disks are not held to the rule.
min_cache=1073741824
floor=53687091200
for spec in "$real_root"/testdata/unraid-fixtures/*/spec; do
  dir=$(dirname -- "$spec")
  variant=$(basename -- "$dir")
  refuses=0
  if [[ -f $dir/expect ]] && grep -qE '^disk [^ ]+ refuse ' "$dir/expect"; then refuses=1; fi
  want_cache=$floor
  if ((refuses)); then want_cache=$min_cache; fi
  biggest=0 parity=0 smallest_parity=0
  while read -r kind size l3size pool boot; do
    bytes=$(numfmt --from=iec "${size#size=}")
    l2bytes=$bytes
    if [[ $l3size == l3size=* ]]; then bytes=$(numfmt --from=iec "${l3size#l3size=}"); fi
    case "$kind" in
      kind=data) if ((bytes > biggest)); then biggest=$bytes; fi ;;
      kind=parity) if ((parity == 0 || bytes < smallest_parity)); then smallest_parity=$bytes; fi; parity=1 ;;
      kind=pool)
        [[ $pool == pool=cache ]] || continue
        if [[ $boot == boot=* ]]; then bytes=$((bytes - ${boot#boot=} * 1048576)); l2bytes=$((l2bytes - ${boot#boot=} * 1048576)); fi
        if ((bytes >= want_cache)); then ok "$variant: the cache pool has $bytes bytes in the guest, at least the $want_cache it needs"; else bad "$variant: the cache pool has $bytes bytes in the guest, less than the $want_cache it needs"; fi
        if ((l2bytes >= min_cache)); then ok "$variant: the cache pool has $l2bytes bytes in the lab, enough for mkfs.xfs"; else bad "$variant: the cache pool has $l2bytes bytes in the lab, less than the $min_cache mkfs.xfs needs"; fi
        ;;
    esac
  done < <(awk '$1 == "disk" { k = ""; s = ""; l = "-"; p = "-"; b = "-"; for (i = 3; i <= NF; i++) { if ($i ~ /^kind=/) k = $i; if ($i ~ /^size=/) s = $i; if ($i ~ /^l3size=/) l = $i; if ($i ~ /^pool=/) p = $i; if ($i ~ /^boot=/) b = $i } print k, s, l, p, b }' "$spec")
  if ((refuses)) || [[ $variant == unraid-with-vms ]]; then continue; fi
  if ((biggest > floor)); then ok "$variant: its largest data disk, $biggest bytes in the guest, is above mergerfs's $floor-byte minfreespace"; else bad "$variant: no data disk is above the $floor-byte minfreespace in the guest, so the import cannot create a share's directory"; fi
  if ((smallest_parity >= biggest)); then ok "$variant: every parity disk is at least as large as the largest data disk in the guest"; else bad "$variant: a parity disk of $smallest_parity bytes is smaller than a data disk of $biggest in the guest"; fi
done

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

# The shared-nvme topology has only parity1, disk1 and disk2; a variant that
# targets more gets the rest attached, at its spec sizes, for the fixture
# builder. A target no topology has is still refused (above).
plan=$(HOSERVA_VM_PLAN_DISKS=1 HOSERVA_VM_TOPOLOGY=shared-nvme VARIANT=unraid-7x-xfs-single-parity "$script_dir/create-vm.sh" 2>&1) || plan="create-vm.sh failed: $plan"
if [[ $plan == $'parity1:2576980377600\ndisk1:335544320\ndisk2:335544320\ndisk3:2469606195200\ncache:60129542144' ]]; then
  ok "a variant that targets disk 3 and the cache gets them attached beside the shared-nvme topology's three disks"
else
  bad "the shared-nvme plan of the 7.x variant was: $plan"
fi
plan=$(HOSERVA_VM_PLAN_DISKS=1 VARIANT=unraid-7x-xfs-single-parity "$script_dir/create-vm.sh" 2>&1) || plan="create-vm.sh failed: $plan"
if [[ $plan == $'parity1:2576980377600\ndisk1:335544320\ndisk2:335544320\ndisk3:2469606195200\ndisk4:4T\ndisk5:4T\ncache:60129542144' ]]; then
  ok "the separate topology keeps its seven disks and sizes the spec's"
else
  bad "the separate plan of the 7.x variant was: $plan"
fi

exit "$fail"
