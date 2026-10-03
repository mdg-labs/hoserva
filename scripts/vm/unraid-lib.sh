# shellcheck shell=bash
# Shared helpers for building the synthetic Unraid source inside this lab's
# own L3 guest (doc 06 §5, issue #74). Sourced, not executed, by
# unraid-fixture.sh and unraid-capture.sh after lib.sh and vm_require_id.
#
# The guest is disposable and has no Hoserva installed yet, so packages are
# installed freely and the builder runs there as root on the guest's own
# virtio array disks. scripts/devenv/unraid-fixture.sh refuses any other disk.

UNRAID_GUEST_DIR=/home/hoserva/unraid-fixture
UNRAID_GUEST_OUT=/srv/unraid-fixtures

unraid_require_variant() {
  VARIANT="${VARIANT:-}"
  [[ -n "$VARIANT" ]] || die "set VARIANT (e.g. VARIANT=unraid-6.12-xfs-single-parity)"
  [[ "$VARIANT" =~ ^unraid-[a-z0-9][a-z0-9.-]*$ ]] || die "invalid VARIANT '$VARIANT'"
  [[ -d "$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT" ]] || die "no variant '$VARIANT' under testdata/unraid-fixtures/"
}

# unraid_spec_sizes: prints "<target> <bytes>" for each disk line of the
# variant's own spec, the sizes scripts/devenv/unraid-fixture.sh builds on.
# Any other disk line (one pulled in by an include) is not read here; the
# builder refuses a guest disk whose size differs from its spec.
unraid_spec_sizes() {
  local spec="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT/spec" line words w size target bytes seen=" "
  [[ -f "$spec" ]] || die "variant '$VARIANT' has no spec file"
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" == disk\ * ]] || continue
    read -r -a words <<<"$line"
    size="" target=""
    for w in "${words[@]:2}"; do
      case "$w" in
        size=*) size="${w#size=}" ;;
        target=*) target="${w#target=}" ;;
      esac
    done
    [[ -n "$size" && -n "$target" ]] || die "spec of '$VARIANT': disk '${words[1]:-}' needs size= and target="
    [[ "$seen" != *" $target "* ]] || die "spec of '$VARIANT': target '$target' is used by two disks"
    seen+="$target "
    bytes="$(numfmt --from=iec "$size")" || die "spec of '$VARIANT': disk '${words[1]}': bad size '$size'"
    printf '%s %s\n' "$target" "$bytes"
  done <"$spec"
}

unraid_require_guest() {
  vm_assert_own_domain "$VM_DOMAIN"
  vm_domain_exists "$VM_DOMAIN" || die "domain '$VM_DOMAIN' does not exist — run 'make vm-up' first"
  vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running — run 'make vm-up' or 'make vm-restore' first"
}

# The packages the builder and the capture need, on a stock Debian 13 guest.
unraid_guest_packages() {
  echo "unraid[$HOSERVA_LAB_ID]: installing the builder's packages in the guest"
  vm_ssh 'sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends xfsprogs btrfs-progs fdisk gdisk zip unzip attr dosfstools >/dev/null'
}

# Copies the builder, the capture helper, the prepare script and the fixture
# definitions into the guest, replacing any earlier copy.
unraid_guest_push() {
  echo "unraid[$HOSERVA_LAB_ID]: copying the builder and the fixture definitions to the guest"
  tar -C "$VM_REPO_ROOT" -cf - \
    scripts/devenv/unraid-fixture.sh scripts/vm/unraid-capture-guest.sh tools/unraid/prepare-migration.sh \
    testdata/unraid-fixtures \
    | vm_ssh "rm -rf -- '$UNRAID_GUEST_DIR' && mkdir -p '$UNRAID_GUEST_DIR' && tar -xf - -C '$UNRAID_GUEST_DIR'"
}

# unraid_guest_build <variant> [no-capture]: builds the variant on the guest's
# array disks; a rebuild replaces the earlier output directory.
unraid_guest_build() {
  local variant=$1 env_extra=""
  [[ "${2:-}" == no-capture ]] && env_extra="HOSERVA_FIXTURE_NO_CAPTURE=1"
  echo "unraid[$HOSERVA_LAB_ID]: building $variant on the guest's array disks"
  vm_ssh "sudo rm -rf --one-file-system -- '$UNRAID_GUEST_OUT/$variant' && sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' $env_extra bash '$UNRAID_GUEST_DIR/scripts/devenv/unraid-fixture.sh' --tier l3 '$variant'"
}

unraid_guest_verify() {
  local variant=$1 env_extra=""
  [[ "${2:-}" == no-capture ]] && env_extra="HOSERVA_FIXTURE_NO_CAPTURE=1"
  echo "unraid[$HOSERVA_LAB_ID]: verifying $variant through read-only mounts"
  vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' $env_extra bash '$UNRAID_GUEST_DIR/scripts/devenv/unraid-fixture.sh' --tier l3 --verify '$variant'"
}
