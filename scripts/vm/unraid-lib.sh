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

# unraid_require_variant: validates VARIANT and the optional OPTION (a variant
# with an options/ directory builds one of them; the spec's default_option when
# OPTION is unset). Sets UNRAID_RUN, the name the builder gives the build's
# output directory and this lab's snapshot: the variant, plus -<option> for a
# non-default option. UNRAID_OPTION_ARG is the builder's own --option argument.
unraid_require_variant() {
  local vdir default
  VARIANT="${VARIANT:-}"
  OPTION="${OPTION:-}"
  [[ -n "$VARIANT" ]] || die "set VARIANT (e.g. VARIANT=unraid-6.12-xfs-single-parity)"
  [[ "$VARIANT" =~ ^unraid-[a-z0-9][a-z0-9.-]*$ ]] || die "invalid VARIANT '$VARIANT'"
  vdir="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT"
  [[ -d "$vdir" ]] || die "no variant '$VARIANT' under testdata/unraid-fixtures/"
  UNRAID_RUN="$VARIANT"
  UNRAID_OPTION_ARG=""
  if [[ -d "$vdir/options" ]]; then
    default="$(sed -n 's/^default_option=//p' "$vdir/spec")"
    [[ "$default" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "spec of '$VARIANT' has options but no default_option"
    if [[ -n "$OPTION" ]]; then
      [[ "$OPTION" =~ ^[a-z0-9][a-z0-9-]*$ && -d "$vdir/options/$OPTION" ]] || die "variant '$VARIANT' has no option '$OPTION'"
      UNRAID_OPTION_ARG="--option '$OPTION'"
      [[ "$OPTION" == "$default" ]] || UNRAID_RUN="$VARIANT-$OPTION"
    fi
  else
    [[ -z "$OPTION" ]] || die "variant '$VARIANT' has no options, but OPTION='$OPTION' was set"
  fi
}

# unraid_spec_sizes: prints "<target> <bytes>" for each disk line of the
# variant's own spec, the sizes scripts/devenv/unraid-fixture.sh builds on at L3
# (a disk's l3size= where it has one, else its size=).
# Any other disk line (one pulled in by an include) is not read here; the
# builder refuses a guest disk whose size differs from its spec.
unraid_spec_sizes() {
  local spec="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT/spec" line words w size target bytes seen=" "
  [[ -f "$spec" ]] || die "variant '$VARIANT' has no spec file"
  while IFS= read -r line || [[ -n "$line" ]]; do
    [[ "$line" == disk\ * ]] || continue
    read -r -a words <<<"$line"
    size="" l3size="" target=""
    for w in "${words[@]:2}"; do
      case "$w" in
        size=*) size="${w#size=}" ;;
        l3size=*) l3size="${w#l3size=}" ;;
        target=*) target="${w#target=}" ;;
      esac
    done
    [[ -z "$l3size" ]] || size="$l3size"
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

# unraid_spec_needs: prints "luks" when the variant's spec has a LUKS disk and
# "zfs" when it has a ZFS disk or the internal-boot layout (whose boot pool is
# ZFS), so the guest gets only the packages its variant builds with.
unraid_spec_needs() {
  local spec="$VM_REPO_ROOT/testdata/unraid-fixtures/$VARIANT/spec"
  [[ -f "$spec" ]] || die "variant '$VARIANT' has no spec file"
  if grep -qE '^disk .* fs=luks-xfs( |$)' "$spec"; then echo luks; fi
  if grep -qE '^disk .* (fs=zfs|boot=[a-z0-9]+)( |$)' "$spec"; then echo zfs; fi
}

# The packages the builder and the capture need, on a stock Debian 13 guest.
# A variant with LUKS disks adds cryptsetup. One that needs ZFS adds OpenZFS from
# Debian's contrib component, built by DKMS against the guest's own kernel
# (several minutes the first time); nothing is installed on the host.
unraid_guest_packages() {
  local needs
  echo "unraid[$HOSERVA_LAB_ID]: installing the builder's packages in the guest"
  vm_ssh 'sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends xfsprogs btrfs-progs e2fsprogs fdisk gdisk zip unzip attr dosfstools >/dev/null'
  needs="$(unraid_spec_needs)"
  if grep -qx luks <<<"$needs"; then
    echo "unraid[$HOSERVA_LAB_ID]: installing cryptsetup in the guest"
    vm_ssh 'sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends cryptsetup >/dev/null && sudo modprobe dm_crypt'
  fi
  if grep -qx zfs <<<"$needs"; then
    echo "unraid[$HOSERVA_LAB_ID]: installing OpenZFS in the guest (contrib, built by DKMS: this takes a few minutes the first time)"
    # shellcheck disable=SC2016 # $(uname -r) is the guest's kernel, expanded there
    vm_ssh 'sudo sed -i "/^Components:/{/contrib/!s/\$/ contrib/}" /etc/apt/sources.list.d/debian.sources && sudo apt-get update -qq && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends "linux-headers-$(uname -r)" zfs-dkms zfsutils-linux >/dev/null && sudo modprobe zfs'
  fi
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

# unraid_guest_build <variant> [no-capture]: builds the variant (and OPTION)
# on the guest's array disks; a rebuild replaces the earlier output directory.
unraid_guest_build() {
  local variant=$1 env_extra=""
  [[ "${2:-}" == no-capture ]] && env_extra="HOSERVA_FIXTURE_NO_CAPTURE=1"
  echo "unraid[$HOSERVA_LAB_ID]: building $UNRAID_RUN on the guest's array disks"
  vm_ssh "sudo rm -rf --one-file-system -- '$UNRAID_GUEST_OUT/$UNRAID_RUN' && sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' $env_extra bash '$UNRAID_GUEST_DIR/scripts/devenv/unraid-fixture.sh' --tier l3 $UNRAID_OPTION_ARG '$variant'"
}

unraid_guest_verify() {
  local variant=$1 env_extra=""
  [[ "${2:-}" == no-capture ]] && env_extra="HOSERVA_FIXTURE_NO_CAPTURE=1"
  echo "unraid[$HOSERVA_LAB_ID]: verifying $UNRAID_RUN through read-only mounts"
  vm_ssh "sudo env HOSERVA_LAB_ID='$HOSERVA_LAB_ID' $env_extra bash '$UNRAID_GUEST_DIR/scripts/devenv/unraid-fixture.sh' --tier l3 --verify $UNRAID_OPTION_ARG '$variant'"
}
