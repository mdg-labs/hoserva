#!/usr/bin/env bash
# Builds a synthetic Unraid source for the migration tests (doc 06 §5, doc 05
# §2): data disks, parity, an optional cache, a flash tree and its Flash Backup
# zips, and the expected result, without Unraid. No agent runs Unraid or
# connects to a real Unraid server (D20).
#
#   unraid-fixture.sh --tier l2|l3 [--option <name>] <variant>             build a variant
#   unraid-fixture.sh --tier l2|l3 [--option <name>] --verify <variant>    re-read it and diff
#
# A variant with an options/ directory (unraid-with-vms) builds one of its
# options: --option, or HOSERVA_FIXTURE_OPTION, or the spec's default_option.
# An option adds its own seed (options/<name>/seed) and lays options/<name>/flash
# over the variant's flash tree. A non-default option builds under
# <variant>-<option> so it never collides with the default one.
#
# It runs as root on whichever system owns the block devices, never on the
# development host:
#   l2  inside the lab container (make lab-unraid-fixture), on loop devices
#       backed by images under this lab's own /lab/<id>/unraid/<variant>/img;
#   l3  inside the lab's L3 guest (make vm-unraid-fixture), on the guest's own
#       virtio array disks, found by the serial scripts/vm/create-vm.sh gave
#       them. Each must be exactly its spec size= (make vm-up VARIANT=<variant>
#       creates them so); any other size is refused before a disk is written.
# Any other device is refused. A variant is defined in
# testdata/unraid-fixtures/<variant>/: spec (disk roles, filesystems, sizes),
# seed (data), flash/ (the authored flash tree laid over common/flash), and, for
# the variants that exercise the scan's refusals, expect (the verdict the scan
# must give each disk).
#
# A variant whose disks need ZFS (fs=zfs, boot=) or device-mapper (fs=luks-xfs)
# builds on the L3 tier only: the lab image has no OpenZFS, and the lab
# container's device set (loop devices and FUSE, Q45) has no /dev/mapper/control,
# so the l2 tier refuses it before anything is written.
#
# What it writes under its output directory (l2: /lab/<id>/unraid/<run>/,
# l3: /srv/unraid-fixtures/<run>/, <run> being the variant, plus -<option> for
# a non-default option):
#   expected/manifest.sha256   every file: sha256, size, mode, owner, disk,
#                              path; per-disk and per-share counts and bytes
#   expected/entries.tsv       directories, symlinks, FIFOs, sockets, device
#                              nodes, sparse files and user.* attributes
#   expected/layout.txt        partition scheme, filesystem facts, parity
#   expected/domains.txt       only when libvirt.img exists at domain.cfg's
#                              IMAGE_FILE: where it is, and each libvirt
#                              domain it holds with its vdisks (sha256),
#                              firmware, bridges and passthrough addresses
#   expected/scan.txt          only with an expect file: the verdict for every
#                              disk (adopt, refuse and why, recreated, boot
#                              device), each checked against what the builder
#                              measured on the disk, and the scan's warnings
#   expected/source-disks.sha256   only when a disk is refused: the sha256 of every
#                              whole source device, taken before any scan, so a
#                              test can assert nothing wrote to them
#   expected/flash.sha256      every file of the flash tree
#   expected/flash-backup.zip  the flash as Unraid's flash_backup packs it
#   expected/flash-hand-zipped.zip   the flash as a user zipping /boot would
#   flash/                     the flash tree itself
# Nothing here is committed: it is regenerated on every build.
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

# Unraid's "MBR: 4K-aligned" layout covers disks that MBR can address (a
# partition table holds at most 2^32-1 sectors); larger disks get GPT (doc 05
# §1.1, doc 08 §2). A 2000G disk is MBR, a 2T (2 TiB) one is already GPT.
MBR_LIMIT_BYTES=$((4294967295 * 512))
PARITY_WINDOW_BYTES=$((64 * 1024 * 1024))
PART_START_SECTOR=64
GPT_LINUX_TYPE=0FC63DAF-8483-4772-8E79-3D69D8477DE4
FLASH_MTIME='2026-09-15 10:00:00 UTC'
XFS_FEATURES=(crc finobt sparse reflink bigtime inobtcount rmapbt nrext64)
# Unraid's internal-boot layout (doc 08 §2, mkbootable): partitions 1 to 3 are
# the boot area, 4 the data area.
BOOT_ZFS_POOL=flash
BOOT_ZFS_DATASET=flash/boot

die() { printf 'unraid-fixture: %s\n' "$*" >&2; exit 1; }

TIER=""
VARIANT=""
OPTION=""
RUN=""
MODE=build
FIXTURES=""
VDIR=""
LAB=""
OUT=""
IMG=""
MNT=""
EXP=""
FLASH=""
WORK=""

SPEC_VERSION=""
SPEC_RELEASE=""
SPEC_EXPECT_PARITY_SIGNATURE=""
SPEC_CAPTURE=""
SPEC_LUKS_PASSPHRASE=""
L3_ONLY=""
LV_MNT=""
LV_LOOP=""
LV_LOCATION=""
SLOTS=()
APPLEDOUBLE=()
declare -A D_KIND D_FS D_SIZE D_TARGET D_POOL D_XFS D_GROUP D_BOOT D_CORRUPT
declare -A PART WHOLE FSDEV
declare -A FACT_LABEL FACT_FEATURES
declare -A PARITY_SHA PARITY_SIG PARITY_KIND
LOOPS=()
MOUNTED=()
LUKS_OPEN=()
ZPOOLS=()
SETRO=()
DEFERRED_MODES=()
PARITY_WINDOW=0

# ----------------------------------------------------------------- spec

parse_disk() {
  local line=$1 words=() w key val slot
  read -r -a words <<<"$line"
  slot=${words[1]:-}
  [[ $slot =~ ^(parity2?|disk[1-9][0-9]?|[a-z][a-z0-9]{0,15})$ ]] || die "spec: bad disk slot '$slot'"
  [[ -z ${D_KIND[$slot]:-} ]] || die "spec: disk '$slot' is declared twice"
  for w in "${words[@]:2}"; do
    key=${w%%=*}
    val=${w#*=}
    case $key in
      kind)
        case $val in parity | data | pool | boot) ;; *) die "spec: disk '$slot': bad kind '$val'" ;; esac
        D_KIND[$slot]=$val
        ;;
      fs)
        case $val in xfs | btrfs | ext4 | luks-xfs | zfs | none) ;; *) die "spec: disk '$slot': bad fs '$val'" ;; esac
        D_FS[$slot]=$val
        ;;
      size)
        D_SIZE[$slot]=$(numfmt --from=iec "$val") || die "spec: disk '$slot': bad size '$val'"
        ;;
      target)
        [[ $val =~ ^[a-z][a-z0-9]*$ ]] || die "spec: disk '$slot': bad target '$val'"
        D_TARGET[$slot]=$val
        ;;
      pool)
        [[ $val == "$slot" ]] || die "spec: pool disk '$slot' must be named like its pool"
        D_POOL[$slot]=$val
        ;;
      xfs)
        case $val in default | rmapbt-nrext64) ;; *) die "spec: disk '$slot': bad xfs '$val'" ;; esac
        D_XFS[$slot]=$val
        ;;
      group)
        [[ $val =~ ^[a-z][a-z0-9]*$ ]] || die "spec: disk '$slot': bad group '$val'"
        D_GROUP[$slot]=$val
        ;;
      boot)
        [[ $val == dedicated || $val =~ ^[1-9][0-9]{3,5}$ ]] || die "spec: disk '$slot': boot= is 'dedicated' or the boot area in MiB (4096 or more)"
        if [[ $val != dedicated ]]; then ((val >= 4096)) || die "spec: disk '$slot': a boot area of $val MiB is below the 4096 MiB Unraid's wizard requires"; fi
        D_BOOT[$slot]=$val
        ;;
      corrupt)
        [[ $val == xfs-metadata ]] || die "spec: disk '$slot': bad corrupt '$val'"
        D_CORRUPT[$slot]=$val
        ;;
      *) die "spec: disk '$slot': unknown key '$key'" ;;
    esac
  done
  [[ -n ${D_KIND[$slot]:-} && -n ${D_FS[$slot]:-} && -n ${D_SIZE[$slot]:-} && -n ${D_TARGET[$slot]:-} ]] \
    || die "spec: disk '$slot' needs kind, fs, size and target"
  case ${D_KIND[$slot]} in
    parity) [[ $slot == parity || $slot == parity2 ]] || die "spec: parity disk must be 'parity' or 'parity2'" ;;
    data) [[ $slot == disk* && ${D_FS[$slot]} != none ]] || die "spec: data disk '$slot' needs a diskN name and a filesystem" ;;
    pool) [[ -n ${D_POOL[$slot]:-} && ${D_FS[$slot]} != none ]] || die "spec: pool disk '$slot' needs pool= and a filesystem" ;;
    boot) [[ $slot == boot && ${D_BOOT[$slot]:-} == dedicated && ${D_FS[$slot]} == none ]] || die "spec: a boot disk is named 'boot', has fs=none and boot=dedicated" ;;
  esac
  [[ ${D_KIND[$slot]} != parity || ${D_FS[$slot]} == none ]] || die "spec: parity disk '$slot' has no filesystem"
  [[ -z ${D_BOOT[$slot]:-} || ${D_KIND[$slot]} == boot || ( ${D_KIND[$slot]} == pool && ${D_BOOT[$slot]} != dedicated ) ]] \
    || die "spec: disk '$slot': boot=dedicated is for the boot disk, boot=<MiB> for a pool that shares the boot device"
  [[ -z ${D_CORRUPT[$slot]:-} || ${D_FS[$slot]} == xfs ]] || die "spec: disk '$slot': corrupt=xfs-metadata needs fs=xfs"
  [[ -z ${D_GROUP[$slot]:-} || ( ${D_FS[$slot]} == btrfs && ${D_KIND[$slot]} == data ) ]] || die "spec: disk '$slot': group= is for btrfs data disks"
  SLOTS+=("$slot")
}

parse_spec() {
  local file=$1 dir line
  [[ -f $file ]] || die "missing spec file $file"
  dir=$(dirname -- "$file")
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ -z ${line//[[:space:]]/} || $line == \#* ]]; then continue; fi
    case $line in
      unraid_version=*) SPEC_VERSION=${line#*=} ;;
      unraid_release=*) SPEC_RELEASE=${line#*=} ;;
      expect_parity_signature=*) SPEC_EXPECT_PARITY_SIGNATURE=${line#*=} ;;
      default_option=*) ;; # read by resolve_option
      capture=*)
        SPEC_CAPTURE=${line#*=}
        [[ $SPEC_CAPTURE == none ]] || die "spec $file: capture= can only be 'none'"
        ;;
      luks_passphrase=*) SPEC_LUKS_PASSPHRASE=${line#*=} ;;
      include\ *) parse_spec "$dir/${line#include }" ;;
      appledouble\ *) APPLEDOUBLE+=("${line#appledouble }") ;;
      disk\ *) parse_disk "$line" ;;
      *) die "spec $file: cannot read line: $line" ;;
    esac
  done <"$file"
}

slot_index() {
  local slot=$1 i
  case $slot in
    parity) echo 0 ;;
    parity2) echo 29 ;;
    disk*) echo "${slot#disk}" ;;
    *)
      i=0
      for s in "${SLOTS[@]}"; do
        if [[ ${D_KIND[$s]} != pool && ${D_KIND[$s]} != boot ]]; then continue; fi
        i=$((i + 1))
        if [[ $s == "$slot" ]]; then break; fi
      done
      echo $((29 + i))
      ;;
  esac
}

role_of() {  # the name the tier knows the disk by
  case $TIER in
    l2) echo "$1" ;;
    l3) echo "${D_TARGET[$1]}" ;;
  esac
}

serial_of() {  # slot -> the serial rendered into the flash tree
  local s
  s="$(role_of "$1")-hoserva-${HOSERVA_LAB_ID}"
  echo "${s:0:20}"
}

# The filesystem each slot holds once its container is open, the type libblkid
# reports on the partition itself, and the value Unraid writes as diskFsType.
inner_fs() { case ${D_FS[$1]} in luks-xfs) echo xfs ;; *) echo "${D_FS[$1]}" ;; esac; }
raw_type() { case ${D_FS[$1]} in luks-xfs) echo crypto_LUKS ;; zfs) echo zfs_member ;; *) echo "${D_FS[$1]}" ;; esac; }
unraid_fstype() { case ${D_FS[$1]} in luks-xfs) echo luks:xfs ;; none) echo "" ;; *) echo "${D_FS[$1]}" ;; esac; }

is_boot_layout() { [[ -n ${D_BOOT[$1]:-} ]]; }

boot_slot() {
  local s
  for s in "${SLOTS[@]}"; do
    if is_boot_layout "$s"; then
      echo "$s"
      return 0
    fi
  done
  return 1
}

# The slots that share one multi-device btrfs filesystem: the first declared is
# the one that is formatted, mounted and seeded; the others are its members.
group_primary() {  # slot -> primary slot of its group, or itself
  local s=$1 t
  [[ -n ${D_GROUP[$s]:-} ]] || {
    echo "$s"
    return 0
  }
  for t in "${SLOTS[@]}"; do
    if [[ ${D_GROUP[$t]:-} == "${D_GROUP[$s]}" ]]; then
      echo "$t"
      return 0
    fi
  done
}
group_members() {  # slot -> every slot of its group, primary first
  local s=$1 t
  if [[ -z ${D_GROUP[$s]:-} ]]; then
    echo "$s"
    return 0
  fi
  for t in "${SLOTS[@]}"; do
    if [[ ${D_GROUP[$t]:-} == "${D_GROUP[$s]}" ]]; then echo "$t"; fi
  done
}
is_group_member() { [[ -n ${D_GROUP[$1]:-} && $(group_primary "$1") != "$1" ]]; }

# Every slot that holds a filesystem the builder formats and mounts: data and
# pool disks except the other members of a multi-device btrfs.
fs_slots() {
  local s
  for s in "${SLOTS[@]}"; do
    case ${D_KIND[$s]} in
      parity | boot) continue ;;
    esac
    if is_group_member "$s"; then continue; fi
    echo "$s"
  done
}

check_spec() {
  local s g n=0 reasons=()
  local -A gcount=()
  for s in "${SLOTS[@]}"; do
    case ${D_FS[$s]} in
      zfs) reasons+=("$s is a ZFS disk") ;;
      luks-xfs) reasons+=("$s is a LUKS container") ;;
    esac
    if is_boot_layout "$s"; then
      n=$((n + 1))
      reasons+=("$s has the internal-boot layout, whose boot pool is ZFS")
    fi
    if [[ -n ${D_GROUP[$s]:-} ]]; then gcount[${D_GROUP[$s]}]=$((${gcount[${D_GROUP[$s]}]:-0} + 1)); fi
    if [[ ${D_FS[$s]} == luks-xfs && -z $SPEC_LUKS_PASSPHRASE ]]; then die "spec: disk '$s' is LUKS, but the spec has no luks_passphrase="; fi
  done
  ((n <= 1)) || die "spec: at most one disk can carry the internal-boot layout"
  for g in "${!gcount[@]}"; do
    ((gcount[$g] >= 2)) || die "spec: btrfs group '$g' has a single disk; a multi-device filesystem needs two or more"
  done
  if [[ -n $SPEC_LUKS_PASSPHRASE && ! $SPEC_LUKS_PASSPHRASE =~ ^[A-Za-z0-9._-]+$ ]]; then die "spec: luks_passphrase must be letters, digits, '.', '_' and '-'"; fi
  if ((${#reasons[@]} > 0)); then L3_ONLY=$(IFS=';'; printf '%s' "${reasons[*]}"); fi
}

# ----------------------------------------------------------------- tier

resolve_option() {
  local opt=${OPTION:-${HOSERVA_FIXTURE_OPTION:-}} def
  def=$(sed -n 's/^default_option=//p' "$VDIR/spec")
  if [[ -d $VDIR/options ]]; then
    [[ $def =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "$VARIANT has options but its spec names no default_option"
    OPTION=${opt:-$def}
    [[ $OPTION =~ ^[a-z0-9][a-z0-9-]*$ && -d $VDIR/options/$OPTION ]] || die "$VARIANT has no option '$OPTION'"
  else
    [[ -z $opt ]] || die "$VARIANT has no options, but option '$opt' was asked for"
    OPTION=""
  fi
  RUN=$VARIANT
  if [[ -n $OPTION && $OPTION != "$def" ]]; then RUN=$VARIANT-$OPTION; fi
}

init_tier() {
  local base
  [[ $EUID -eq 0 ]] || die "must run as root, inside the lab container or the L3 guest"
  [[ ${HOSERVA_LAB_ID:-} =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ && $HOSERVA_LAB_ID != *..* ]] \
    || die "set a valid HOSERVA_LAB_ID"
  case $TIER in
    l2)
      LAB="/lab/$HOSERVA_LAB_ID"
      if [[ ! -d $LAB ]] || ! mountpoint -q "$LAB"; then die "$LAB is not this lab's mount: run inside the lab container (make lab-unraid-fixture)"; fi
      base="$LAB/unraid"
      ;;
    l3)
      systemd-detect-virt --vm --quiet || die "not inside a virtual machine: run inside the L3 guest (make vm-unraid-fixture)"
      if systemd-detect-virt --container --quiet; then die "inside a container, not the L3 guest"; fi
      base="${HOSERVA_FIXTURE_OUT:-/srv/unraid-fixtures}"
      ;;
    *) die "--tier must be l2 or l3" ;;
  esac
  FIXTURES=$(realpath -- "${HOSERVA_FIXTURES_DIR:-$SCRIPT_DIR/../../testdata/unraid-fixtures}")
  [[ $VARIANT =~ ^unraid-[a-z0-9][a-z0-9.-]*$ ]] || die "bad variant name '$VARIANT'"
  VDIR="$FIXTURES/$VARIANT"
  [[ -d $VDIR && -f $VDIR/spec && -f $VDIR/seed ]] || die "no variant '$VARIANT' under $FIXTURES"
  resolve_option
  OUT="$base/$RUN"
  IMG="$OUT/img"
  MNT="$OUT/mnt"
  EXP="$OUT/expected"
  FLASH="$OUT/flash"
}

# L2: a loop device is accepted only when losetup itself reports it attached
# to exactly one of this lab's own images under $IMG, and the image's path,
# canonicalised, is under $IMG.
assert_own_loop() {
  local dev=$1 img=$2 resolved_img resolved_root resolved
  resolved_img=$(realpath -m -- "$img") || die "refusing $img: cannot resolve path"
  resolved_root=$(realpath -m -- "$IMG")
  case $resolved_img in
    "$resolved_root"/*.img) ;;
    *) die "refusing $img: not an image under $IMG" ;;
  esac
  [[ $dev =~ ^/dev/loop[0-9]+$ ]] || die "refusing non-loop device: $dev"
  resolved=$(losetup -j "$resolved_img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  [[ -n $resolved && $resolved == "$dev" ]] || die "refusing $dev: not the only loop device backed by $img"
}

# L3: the guest's own virtio array disk for this slot, never the OS disk
# (vda) and never anything whose serial is not the one create-vm.sh gave it.
assert_own_virtio() {
  local slot=$1 dev=$2 name serial
  [[ $dev =~ ^/dev/vd[b-z]$ ]] || die "refusing $dev: not one of the guest's virtio array disks"
  [[ -b $dev ]] || die "refusing $dev: not a block device"
  name=${dev##*/}
  serial=$(cat -- "/sys/block/$name/serial" 2>/dev/null) || die "refusing $dev: no serial"
  [[ $serial == "$(serial_of "$slot")" ]] || die "refusing $dev: its serial does not match this lab's $(role_of "$slot") disk"
}

assert_unused() {
  local dev=$1 mounts
  mounts=$(lsblk -nr -o MOUNTPOINTS "$dev") || die "refusing $dev: lsblk cannot tell whether it is in use"
  [[ -z ${mounts//[[:space:]]/} ]] || die "refusing $dev: it or one of its partitions is mounted"
}

assert_own_part() {  # slot
  local slot=$1
  case $TIER in
    l2) assert_own_loop "${PART[$slot]}" "${WHOLE[$slot]}" ;;
    l3) assert_own_virtio "$slot" "${WHOLE[$slot]}" ;;
  esac
}

# L3: the guest disk must be exactly the size the spec gives it, or the build
# would lay out other partitions than the L2 build of the same variant.
assert_spec_size() {  # slot bytes
  local slot=$1 actual=$2
  [[ $actual =~ ^[0-9]+$ ]] || die "refusing $(role_of "$slot"): cannot read the size of its disk"
  ((actual == D_SIZE[$slot])) \
    || die "refusing $(role_of "$slot") (slot $slot): the guest disk is $actual bytes, the spec says ${D_SIZE[$slot]} bytes (make vm-up VARIANT=$VARIANT sizes the disks from the spec)"
}

l3_whole_dev() {
  local slot=$1 link dev
  link="/dev/disk/by-id/virtio-$(serial_of "$slot")"
  [[ -e $link ]] || die "the guest has no disk $link (this lab's $(role_of "$slot") disk)"
  dev=$(readlink -f -- "$link")
  assert_own_virtio "$slot" "$dev"
  printf '%s' "$dev"
}

# ------------------------------------------------------------ partitions

partition_script() {  # bytes
  if (($1 <= MBR_LIMIT_BYTES)); then
    printf 'label: dos\nunit: sectors\n\nstart=%d, type=83\n' "$PART_START_SECTOR"
  else
    # The partition ends at the last usable sector, 34 sectors before the end
    # of the disk; sfdisk would otherwise round the end down to a multiple of 64.
    printf 'label: gpt\nunit: sectors\nfirst-lba: %d\n\nstart=%d, size=%d, type=%s\n' \
      "$PART_START_SECTOR" "$PART_START_SECTOR" $(($1 / 512 - 33 - PART_START_SECTOR)) "$GPT_LINUX_TYPE"
  fi
}

# Sets L_SCHEME, L_START, L_SECTORS, L_TYPE, L_TOTAL from the partition table
# on a disk or image, the way sfdisk reports it.
read_layout() {
  local whole=$1 dump line total info
  dump=$(sfdisk -d "$whole") || die "sfdisk -d $whole failed"
  L_SCHEME=$(sed -n 's/^label: //p' <<<"$dump")
  line=$(grep -E ' : +start=' <<<"$dump" || true)
  [[ $(wc -l <<<"$line") -eq 1 && -n $line ]] || die "$whole: expected exactly one partition"
  L_START=$(sed -n 's/.*start= *\([0-9]*\),.*/\1/p' <<<"$line")
  L_SECTORS=$(sed -n 's/.*size= *\([0-9]*\),.*/\1/p' <<<"$line")
  L_TYPE=$(sed -n 's/.*type=\([^,]*\).*/\1/p' <<<"$line")
  if [[ -b $whole ]]; then total=$(blockdev --getsz "$whole"); else total=$(($(stat -c %s -- "$whole") / 512)); fi
  L_TOTAL=$total
  case $L_SCHEME in
    dos) L_SCHEME=mbr ;;
    gpt) ;;
    *) die "$whole: unexpected partition table '$L_SCHEME'" ;;
  esac
  [[ $L_START == "$PART_START_SECTOR" ]] || die "$whole: partition starts at sector $L_START, not $PART_START_SECTOR"
  if [[ $L_SCHEME == gpt ]]; then
    # one partition, ending 33 sectors before the end of the disk
    ((L_START + L_SECTORS == L_TOTAL - 33)) || die "$whole: the GPT partition does not end 33 sectors before the end of the disk"
    info=$(sgdisk -i 1 "$whole") || die "sgdisk -i 1 $whole failed"
    grep -q "^Partition GUID code: $GPT_LINUX_TYPE " <<<"$info" || die "$whole: sgdisk does not report the Linux filesystem type"
    grep -q "^First sector: $L_START " <<<"$info" || die "$whole: sgdisk reports another first sector"
    grep -q "^Last sector: $((L_START + L_SECTORS - 1)) " <<<"$info" || die "$whole: sgdisk reports another last sector"
  fi
}

register_loop() { LOOPS+=("$1"); }

boot_part() { printf '%s%s' "$1" "$2"; }  # whole device, partition number (virtio names)

# The boot device: the layout Unraid's mkbootable writes (doc 08 §2). L3 only.
write_boot_table() {  # slot
  local slot=$1 whole=${WHOLE[$1]} mib boot_mib
  mib=$((D_SIZE[$slot] / 1048576))
  if [[ ${D_BOOT[$slot]} == dedicated ]]; then boot_mib=$((mib - 1)); else boot_mib=${D_BOOT[$slot]}; fi
  ((boot_mib >= 4096 && boot_mib < mib)) || die "$slot: a ${boot_mib} MiB boot area does not fit a ${mib} MiB device with a data partition"
  sgdisk "$whole" \
    --new=1:1M:+1M --typecode=1:ef02 --change-name=1:'BIOS Boot Partition' \
    --new=2:0:+510M --typecode=2:ef00 --change-name=2:'EFI System Partition' \
    --new=3:0:+$((boot_mib - 512))M --typecode=3:8300 --change-name=3:'Unraid Boot Partition' \
    --new=4:0:0 --typecode=4:8300 >/dev/null
  udevadm settle
}

# Sets PART[slot] to the data area, partition 4, and makes every partition
# of the device read-only when asked.
attach_boot_parts() {  # slot ro|rw
  local slot=$1 mode=$2 whole=${WHOLE[$1]} n dev
  assert_own_virtio "$slot" "$whole"
  for n in 1 2 3 4; do
    dev=$(boot_part "$whole" "$n")
    [[ -b $dev ]] || die "$whole has no partition $n"
    if [[ $mode == ro ]]; then
      blockdev --setro "$dev"
      SETRO+=("$dev")
    fi
  done
  PART[$slot]=$(boot_part "$whole" 4)
}

# Sets PART[slot] to the partition's block device; for l2 that is a loop
# device over the partition's own byte range of the image.
attach_part() {  # slot ro|rw
  local slot=$1 mode=$2 whole dev opts=()
  whole=${WHOLE[$slot]}
  if is_boot_layout "$slot"; then
    attach_boot_parts "$slot" "$mode"
    return 0
  fi
  read_layout "$whole"
  case $TIER in
    l2)
      if [[ $mode == ro ]]; then opts=(--read-only); fi
      dev=$(losetup --find --show "${opts[@]}" --offset $((L_START * 512)) --sizelimit $((L_SECTORS * 512)) "$whole")
      register_loop "$dev"
      PART[$slot]=$dev
      assert_own_part "$slot"
      rm -f /run/blkid/blkid.tab* 2>/dev/null || true
      ;;
    l3)
      dev="${whole}1"
      [[ -b $dev ]] || die "$whole has no partition 1"
      PART[$slot]=$dev
      if [[ $mode == ro ]]; then
        blockdev --setro "$dev"
        SETRO+=("$dev")
      fi
      ;;
  esac
}

new_disk() {  # slot (build only)
  local slot=$1 whole bytes
  bytes=${D_SIZE[$slot]}
  case $TIER in
    l2)
      mkdir -p -- "$IMG"
      whole="$IMG/$slot.img"
      [[ ! -e $whole ]] || die "refusing to recreate existing image $whole"
      truncate -s "$bytes" "$whole"
      WHOLE[$slot]=$whole
      partition_script "$bytes" | sfdisk --no-reread -q "$whole" >/dev/null
      ;;
    l3)
      whole=${WHOLE[$slot]}
      wipefs -a -q "$whole"
      if is_boot_layout "$slot"; then
        write_boot_table "$slot"
      else
        partition_script "$bytes" | sfdisk -q "$whole" >/dev/null
        udevadm settle
      fi
      ;;
  esac
  attach_part "$slot" rw
}

existing_disk() {  # slot ro (verify)
  local slot=$1
  case $TIER in
    l2) WHOLE[$slot]="$IMG/$slot.img"; [[ -f ${WHOLE[$slot]} ]] || die "no image ${WHOLE[$slot]}: build the variant first" ;;
    l3) WHOLE[$slot]=$(l3_whole_dev "$slot") ;;
  esac
  attach_part "$slot" ro
}

# ------------------------------------------------------------ filesystems

luks_name() { printf 'hoserva-fx-%s' "$1"; }

# Opens the LUKS container of a slot as /dev/mapper/hoserva-fx-<slot>.
luks_open() {  # slot rw|ro
  local slot=$1 mode=$2 name opts=()
  name=$(luks_name "$slot")
  if [[ $mode == ro ]]; then opts=(--readonly); fi
  [[ ! -e /dev/mapper/$name ]] || die "/dev/mapper/$name already exists"
  printf '%s' "$SPEC_LUKS_PASSPHRASE" | cryptsetup open --type luks --key-file=- "${opts[@]}" "${PART[$slot]}" "$name" \
    || die "$slot: cannot open its LUKS container"
  LUKS_OPEN+=("$name")
  FSDEV[$slot]=/dev/mapper/$name
}

zfs_import_ro() {  # slot
  local slot=$1
  zpool import -N -o readonly=on -o cachefile=none -d "${PART[$slot]}" "$slot" || die "$slot: cannot import its ZFS pool read-only"
  ZPOOLS+=("$slot")
  FSDEV[$slot]=$slot
}

# Makes the slot's filesystem device available for an existing disk: the
# partition itself, the opened LUKS container or the imported ZFS pool.
activate_fs() {  # slot rw|ro
  local slot=$1 mode=$2
  case ${D_FS[$slot]} in
    luks-xfs) luks_open "$slot" "$mode" ;;
    zfs) zfs_import_ro "$slot" ;;
    *) FSDEV[$slot]=${PART[$slot]} ;;
  esac
}

format_part() {  # slot
  local slot=$1 dev mopts i m members=() devs=()
  dev=${PART[$slot]}
  assert_own_part "$slot"
  FSDEV[$slot]=$dev
  case ${D_FS[$slot]} in
    xfs | luks-xfs)
      if [[ ${D_FS[$slot]} == luks-xfs ]]; then
        printf '%s' "$SPEC_LUKS_PASSPHRASE" | cryptsetup luksFormat --batch-mode --type luks2 --pbkdf pbkdf2 --pbkdf-force-iterations 1000 --key-file=- "$dev" \
          || die "$slot: cannot create its LUKS container"
        luks_open "$slot" rw
        dev=${FSDEV[$slot]}
      fi
      mopts=crc=1,finobt=1,reflink=1,bigtime=1,inobtcount=1,rmapbt=0
      i=sparse=1,nrext64=0
      if [[ ${D_XFS[$slot]:-default} == rmapbt-nrext64 ]]; then
        mopts=${mopts%rmapbt=0}rmapbt=1
        i=${i%nrext64=0}nrext64=1
      fi
      mkfs.xfs -q -f -K -m "$mopts" -i "$i" "$dev"
      ;;
    ext4) mkfs.ext4 -q -F -E lazy_itable_init=0,lazy_journal_init=0 "$dev" ;;
    btrfs)
      if [[ -n ${D_GROUP[$slot]:-} ]]; then
        mapfile -t members < <(group_members "$slot")
        for m in "${members[@]}"; do
          assert_own_part "$m"
          devs+=("${PART[$m]}")
        done
        mkfs.btrfs -q -f -d raid1 -m raid1 "${devs[@]}"
      else
        mkfs.btrfs -q -f "$dev"
      fi
      ;;
    zfs)
      zpool create -f -o ashift=12 -o cachefile=none -O mountpoint=legacy -O xattr=sa -O atime=off "$slot" "$dev"
      ZPOOLS+=("$slot")
      FSDEV[$slot]=$slot
      ;;
  esac
}

mount_fs() {  # slot rw|ro
  local slot=$1 mode=$2 m dev devopt="" t members=()
  m="$MNT/$slot"
  [[ -n ${FSDEV[$slot]:-} ]] || activate_fs "$slot" "$mode"
  dev=${FSDEV[$slot]}
  mkdir -p -- "$m"
  if [[ -n ${D_GROUP[$slot]:-} ]]; then
    mapfile -t members < <(group_members "$slot")
    for t in "${members[@]:1}"; do devopt+=",device=${PART[$t]}"; done
  fi
  case $(inner_fs "$slot"):$mode in
    xfs:rw) mount -t xfs -o noatime,nouuid,inode64,logbufs=8,logbsize=32k,noquota "$dev" "$m" ;;
    xfs:ro) mount -t xfs -o ro,norecovery,nouuid,noatime,inode64,noquota "$dev" "$m" ;;
    btrfs:rw) mount -t btrfs -o "noatime$devopt" "$dev" "$m" ;;
    btrfs:ro) mount -t btrfs -o "ro,rescue=nologreplay,noatime$devopt" "$dev" "$m" ;;
    ext4:rw) mount -t ext4 -o noatime "$dev" "$m" ;;
    ext4:ro) mount -t ext4 -o ro,noload,noatime "$dev" "$m" ;;
    zfs:rw) mount -t zfs "$dev" "$m" ;;
    zfs:ro) mount -t zfs -o ro "$dev" "$m" ;;
  esac
  MOUNTED+=("$m")
}

# Unmounts everything in reverse order, then closes the LUKS containers and
# exports the ZFS pools the run opened. A step that fails stops the run: the
# disks are never reported as built or verified over a mount or mapping that is
# still holding them.
unmount_all() {
  local i m name pool
  for ((i = ${#MOUNTED[@]} - 1; i >= 0; i--)); do
    m=${MOUNTED[$i]}
    if mountpoint -q "$m" 2>/dev/null; then umount "$m" || die "cannot unmount $m"; fi
  done
  MOUNTED=()
  for pool in "${ZPOOLS[@]}"; do
    zpool export "$pool" || die "cannot export the ZFS pool $pool"
  done
  ZPOOLS=()
  for name in "${LUKS_OPEN[@]}"; do
    cryptsetup close "$name" || die "cannot close /dev/mapper/$name"
  done
  LUKS_OPEN=()
  FSDEV=()
}

# shellcheck disable=SC2317 # called from the EXIT trap
cleanup() {
  local rc=$? m dev img name pool
  set +e
  if [[ -n $LV_MNT ]] && mountpoint -q "$LV_MNT" 2>/dev/null; then umount "$LV_MNT" 2>/dev/null; fi
  if [[ -n $LV_LOOP ]]; then
    img=$(losetup --noheadings --output BACK-FILE "$LV_LOOP" 2>/dev/null)
    case $img in
      "$MNT"/*) losetup -d "$LV_LOOP" ;;
    esac
  fi
  for m in "${MOUNTED[@]}"; do
    if mountpoint -q "$m" 2>/dev/null; then umount "$m" 2>/dev/null; fi
  done
  for pool in "${ZPOOLS[@]}"; do
    if zpool list -H -o name "$pool" >/dev/null 2>&1; then
      zpool export "$pool" 2>/dev/null
      if zpool list -H -o name "$pool" >/dev/null 2>&1; then
        printf 'unraid-fixture: cannot export the ZFS pool %s\n' "$pool" >&2
        if ((rc == 0)); then rc=1; fi
      fi
    fi
  done
  for name in "${LUKS_OPEN[@]}"; do
    if [[ -e /dev/mapper/$name ]]; then
      cryptsetup close "$name" 2>/dev/null
      if [[ -e /dev/mapper/$name ]]; then
        printf 'unraid-fixture: cannot close /dev/mapper/%s\n' "$name" >&2
        if ((rc == 0)); then rc=1; fi
      fi
    fi
  done
  for dev in "${SETRO[@]}"; do blockdev --setrw "$dev" 2>/dev/null; done
  for dev in "${LOOPS[@]}"; do
    # detach only a loop device that is still backed by one of our own images
    img=$(losetup --noheadings --output BACK-FILE "$dev" 2>/dev/null)
    case $img in
      "$IMG"/*.img*) losetup -d "$dev" ;;
    esac
  done
  if [[ -n $WORK ]] && ! { [[ -n $LV_MNT ]] && mountpoint -q "$LV_MNT" 2>/dev/null; }; then rm -rf --one-file-system -- "$WORK"; fi
  for m in ${LV_MNT:+"$LV_MNT"} "${MOUNTED[@]}"; do
    if mountpoint -q "$m" 2>/dev/null; then
      printf 'unraid-fixture: cannot unmount %s; make lab-destroy clears it\n' "$m" >&2
      if ((rc == 0)); then rc=1; fi
    fi
  done
  return $rc
}

# ------------------------------------------------------------- libvirt.img

# Reads a quoted KEY="value" of the flash tree's domain.cfg.
flash_domain_cfg() {  # key
  sed -n "s/^$1=\"\(.*\)\"\$/\1/p" "$FLASH/config/domain.cfg" | head -n 1
}

# A loop device is accepted only when losetup reports it attached to exactly
# this one file on one of the fixture's own mounted disks.
assert_own_lv_loop() {  # dev image
  local dev=$1 img=$2 resolved
  [[ $dev =~ ^/dev/loop[0-9]+$ ]] || die "refusing non-loop device: $dev"
  case $(realpath -m -- "$img") in
    "$(realpath -m -- "$MNT")"/*) ;;
    *) die "refusing $img: not a file on one of the fixture's mounted disks" ;;
  esac
  resolved=$(losetup -j "$img" --output NAME --noheadings 2>/dev/null | tr -d '[:space:]')
  [[ -n $resolved && $resolved == "$dev" ]] || die "refusing $dev: not the only loop device backed by $img"
}

lv_attach() {  # image rw|ro
  local img=$1 mode=$2 opts=()
  if [[ $mode == ro ]]; then opts=(--read-only); fi
  LV_LOOP=$(losetup --find --show "${opts[@]}" -- "$img")
  assert_own_lv_loop "$LV_LOOP" "$img"
  LV_MNT="$WORK/libvirt"
  mkdir -p -- "$LV_MNT"
  case $mode in
    rw) mount -t btrfs -o noatime "$LV_LOOP" "$LV_MNT" ;;
    ro) mount -t btrfs -o ro,rescue=nologreplay,noatime "$LV_LOOP" "$LV_MNT" ;;
  esac
}

lv_detach() {
  umount "$LV_MNT" || die "cannot unmount $LV_MNT"
  losetup -d "$LV_LOOP"
  LV_MNT=""
  LV_LOOP=""
}

# seed op libvirtimg: the image of Unraid's VM Manager at $T, a btrfs loop
# image of domain.cfg's IMAGE_SIZE GiB holding the variant's authored tree
# (libvirt/qemu/*.xml) and what the libvirt service itself creates.
seed_libvirt_img() {  # slot path args...
  local path=$2 size tree src declared gib nv xmls=()
  shift 2
  size=$(arg_value size "$@") || die "seed: libvirtimg: no size"
  tree=$(arg_value tree "$@") || die "seed: libvirtimg: no tree"
  [[ $tree =~ ^[a-z][a-z0-9-]*$ ]] || die "seed: libvirtimg: bad tree '$tree'"
  src="$VDIR/$tree"
  [[ -d $src/qemu ]] || die "seed: libvirtimg: $src has no qemu/"
  declared=$(flash_domain_cfg IMAGE_FILE)
  [[ $declared == "/mnt/user/$path" ]] || die "seed: libvirtimg: placed at /mnt/user/$path, but domain.cfg's IMAGE_FILE is '$declared'"
  gib=$(flash_domain_cfg IMAGE_SIZE)
  size=$(numfmt --from=iec "$size") || die "seed: libvirtimg: bad size"
  [[ $gib =~ ^[1-9][0-9]*$ ]] || die "seed: libvirtimg: domain.cfg has no usable IMAGE_SIZE ('$gib')"
  ((size == gib * 1073741824)) || die "seed: libvirtimg: size $size is not domain.cfg's IMAGE_SIZE ($gib GiB)"
  [[ ! -e $T && ! -L $T ]] || die "seed: libvirtimg: $T exists"
  truncate -s "$size" "$T"
  mkfs.btrfs -q -f "$T"
  lv_attach "$T" rw
  cp -a --no-preserve=ownership -- "$src/." "$LV_MNT/"
  mkdir -p -- "$LV_MNT/qemu/nvram" "$LV_MNT/qemu/snapshot" "$LV_MNT/qemu/snapshotdb" "$LV_MNT/qemu/swtpm/tpm-states"
  xmls=("$src"/qemu/*.xml)
  [[ -f ${xmls[0]} ]] || die "seed: libvirtimg: $src/qemu holds no domain XML"
  while IFS= read -r nv; do
    [[ $nv == /etc/libvirt/qemu/nvram/* && $nv != *..* ]] || die "seed: libvirtimg: unexpected nvram path '$nv'"
    head -c 131072 /dev/urandom >"$LV_MNT/${nv#/etc/libvirt/}"
  done < <(cat -- "${xmls[@]}" | sed -n 's|.*<nvram>\([^<]*\)</nvram>.*|\1|p')
  sync
  lv_detach
}

# -------------------------------------------------------------- seeding

seed_target() {  # slot path -> sets T
  local slot=$1 path=$2
  [[ -n $path && $path != /* && $path != .. && $path != ../* && $path != */../* && $path != */.. ]] || die "seed: bad path '$path'"
  [[ -n ${D_KIND[$slot]:-} && ${D_KIND[$slot]} != parity ]] || die "seed: no disk '$slot' in the spec"
  mountpoint -q "$MNT/$slot" || die "seed: '$slot' is not mounted"
  T="$MNT/$slot/$path"
}

apply_xattrs() {  # path args...
  local path=$1 a nv
  shift
  for a in "$@"; do
    case $a in
      xattr=*)
        nv=${a#xattr=}
        setfattr -n "${nv%%=*}" -v "${nv#*=}" -- "$path"
        ;;
    esac
  done
}

arg_value() {  # key args...
  local key=$1 a
  shift
  for a in "$@"; do
    if [[ $a == "$key="* ]]; then
      printf '%s' "${a#*=}"
      return 0
    fi
  done
  return 1
}

seed_run() {
  local file=$1 dir line f=() op slot path size off len a
  [[ -f $file ]] || die "missing seed file $file"
  dir=$(dirname -- "$file")
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ -z ${line//[[:space:]]/} || $line == \#* ]]; then continue; fi
    IFS='|' read -r -a f <<<"$line"
    op=${f[0]}
    if [[ $op == include ]]; then
      seed_run "$dir/${f[1]}"
      continue
    fi
    slot=${f[1]:-}
    path=$(printf '%b' "${f[2]:-}")
    seed_target "$slot" "$path"
    mkdir -p -- "$(dirname -- "$T")"
    case $op in
      dir) mkdir -p -- "$T" ;;
      file)
        size=$(arg_value size "${f[@]:3}") || die "seed: $line: no size"
        head -c "$size" /dev/urandom >"$T"
        apply_xattrs "$T" "${f[@]:3}"
        ;;
      text)
        a=$(arg_value content "${f[@]:3}") || die "seed: $line: no content"
        printf '%b' "$a" >"$T"
        ;;
      symlink)
        a=$(arg_value target "${f[@]:3}") || die "seed: $line: no target"
        ln -s -- "$a" "$T"
        ;;
      fifo) mkfifo -- "$T" ;;
      socket)
        (cd -- "$(dirname -- "$T")" && perl -MSocket -e 'socket(my $s, PF_UNIX, SOCK_STREAM, 0) or die "socket: $!"; bind($s, sockaddr_un($ARGV[0])) or die "bind: $!"' -- "$(basename -- "$T")")
        ;;
      chardev)
        mknod -m 0666 -- "$T" c "$(arg_value major "${f[@]:3}")" "$(arg_value minor "${f[@]:3}")"
        ;;
      sparse)
        size=$(numfmt --from=iec "$(arg_value size "${f[@]:3}")")
        truncate -s "$size" "$T"
        for a in "${f[@]:3}"; do
          [[ $a == data=* ]] || continue
          len=${a#data=}
          off=$(numfmt --from=iec "${len#*@}")
          len=$(numfmt --from=iec "${len%@*}")
          head -c "$len" /dev/urandom | dd of="$T" bs=1 seek="$off" conv=notrunc status=none
        done
        ;;
      libvirtimg) seed_libvirt_img "$slot" "$path" "${f[@]:3}" ;;
      xattr) apply_xattrs "$T" "${f[@]:3}" ;;
      mode) DEFERRED_MODES+=("$T|$(arg_value mode "${f[@]:3}")") ;;
      *) die "seed: unknown operation '$op'" ;;
    esac
  done <"$file"
}

# Files and directories the way Unraid's shares leave them: owned by
# nobody:users, files 0666 and directories 0777.
normalize_tree() {
  local m=$1
  chown -hR 99:100 -- "$m"
  find "$m" -type d -exec chmod 0777 {} +
  find "$m" \( -type f -o -type p -o -type s \) -exec chmod 0666 {} +
}

apply_deferred_modes() {
  local d
  for d in "${DEFERRED_MODES[@]}"; do chmod "${d#*|}" -- "${d%|*}"; done
}

# ----------------------------------------------------------------- scan

# Walks a mounted filesystem and appends one tab-separated line per regular
# file to FILES and one per everything else to ENTRIES. The same function
# runs at build time (read-write mount) and when verifying (read-only,
# norecovery mount), so a difference is a difference in the disk.
scan_tree() {  # mountpoint slot files entries
  local mnt=$1 slot=$2 files=$3 entries=$4 rec typ size mode own blocks tgt path full hash rdev line name val
  while IFS= read -r -d '' rec; do
    IFS=$'\x1f' read -r typ size mode own blocks tgt path <<<"$rec"
    full="$mnt/$path"
    case $typ in
      f)
        hash=$(sha256sum -- "$full")
        printf '%s\t%s\t%s\t%s\t%s\t%s\n' "${hash%% *}" "$size" "$mode" "$own" "$slot" "$path" >>"$files"
        if ((size >= 1048576 && blocks * 512 < size / 2)); then
          printf 'sparse\t%s\t%s\tsize=%s\t%s\t%s\n' "$slot" "$path" "$size" "$mode" "$own" >>"$entries"
        fi
        ;;
      d) printf 'dir\t%s\t%s\t-\t%s\t%s\n' "$slot" "$path" "$mode" "$own" >>"$entries" ;;
      l) printf 'symlink\t%s\t%s\t%s\t%s\t%s\n' "$slot" "$path" "$tgt" "$mode" "$own" >>"$entries" ;;
      p) printf 'fifo\t%s\t%s\t-\t%s\t%s\n' "$slot" "$path" "$mode" "$own" >>"$entries" ;;
      s) printf 'socket\t%s\t%s\t-\t%s\t%s\n' "$slot" "$path" "$mode" "$own" >>"$entries" ;;
      c)
        rdev="$((16#$(stat -c %t -- "$full"))):$((16#$(stat -c %T -- "$full")))"
        printf 'chardev\t%s\t%s\t%s\t%s\t%s\n' "$slot" "$path" "$rdev" "$mode" "$own" >>"$entries"
        ;;
      *) die "$slot: unsupported file type '$typ' at $path" ;;
    esac
    if [[ $typ == f || $typ == d ]]; then
      while IFS= read -r line; do
        [[ $line == user.* ]] || continue
        name=${line%%=*}
        val=${line#*=}
        printf 'xattr\t%s\t%s\t%s=%s\t-\t-\n' "$slot" "$path" "$name" "$val" >>"$entries"
      done < <(getfattr -h --absolute-names -d -m '^user\.' -e hex -- "$full")
    fi
  done < <(find "$mnt" -mindepth 1 -printf '%y\037%s\037%m\037%U:%G\037%b\037%l\037%P\0')
}

probe_value() {  # key dev -> value or empty when there is no signature
  local out rc=0
  out=$(blkid -p -s "$1" -o value "$2" 2>/dev/null) || rc=$?
  case $rc in
    0) printf '%s' "$out" ;;
    2) ;;
    *) die "blkid $2 failed ($rc)" ;;
  esac
}

btrfs_devices() {  # partition -> the number of devices its filesystem spans
  local n
  n=$(btrfs inspect-internal dump-super "$1" | sed -n 's/^num_devices[[:space:]]*//p') || die "btrfs inspect-internal dump-super $1 failed"
  [[ $n =~ ^[1-9][0-9]*$ ]] || die "$1: cannot read the number of btrfs devices"
  printf '%s' "$n"
}

# Records what is true of a mounted data or pool filesystem.
fs_facts() {  # slot
  local slot=$1 dev type label info f v feats="" n luks
  dev=${PART[$slot]}
  type=$(probe_value TYPE "$dev")
  [[ $type == "$(raw_type "$slot")" ]] || die "$slot: the partition holds '$type', not $(raw_type "$slot")"
  case ${D_FS[$slot]} in
    zfs)
      label=$(probe_value LABEL "$dev")
      [[ $label == "$slot" ]] || die "$slot: the ZFS pool is named '$label', not $slot"
      FACT_LABEL[$slot]=$label
      FACT_FEATURES[$slot]="zpool=$label"
      return 0
      ;;
    luks-xfs)
      label=$(probe_value LABEL "$dev")
      [[ -z $label ]] || die "$slot: the LUKS container has a label ('$label')"
      luks=$(cryptsetup luksDump "$dev" | sed -n 's/^Version:[[:space:]]*//p')
      [[ $luks =~ ^[12]$ ]] || die "$slot: cannot read the LUKS version"
      feats="luks=$luks"
      type=$(probe_value TYPE "${FSDEV[$slot]}")
      [[ $type == xfs ]] || die "$slot: the LUKS container holds '$type', not xfs"
      dev=${FSDEV[$slot]}
      ;;
  esac
  label=$(probe_value LABEL "$dev")
  [[ -z $label ]] || die "$slot: the filesystem has a label ('$label'); Unraid's carry none"
  FACT_LABEL[$slot]=-
  case $(inner_fs "$slot") in
    xfs)
      info=$(xfs_info "$MNT/$slot") || die "xfs_info $slot failed"
      for f in "${XFS_FEATURES[@]}"; do
        v=$(grep -oE "(^|[ ,])$f=[0-9]" <<<"$info" | head -n 1 | sed 's/.*=//') || true
        [[ -n $v ]] || die "$slot: xfs_info does not report $f"
        feats+=" $f=$v"
      done
      ;;
    btrfs)
      n=$(btrfs_devices "$dev")
      ((n == $(group_members "$slot" | wc -l))) || die "$slot: its btrfs filesystem spans $n devices, the spec's group has $(group_members "$slot" | wc -l)"
      feats+=" devices=$n"
      ;;
  esac
  FACT_FEATURES[$slot]=${feats# }
}

# The other devices of a multi-device btrfs filesystem are not mounted
# themselves; what is recorded for them is what the superblock says.
member_facts() {  # slot
  local slot=$1 dev=${PART[$1]} type n
  type=$(probe_value TYPE "$dev")
  [[ $type == btrfs ]] || die "$slot: the partition holds '$type', not btrfs"
  [[ -z $(probe_value LABEL "$dev") ]] || die "$slot: the filesystem has a label; Unraid's carry none"
  n=$(btrfs_devices "$dev")
  ((n == $(group_members "$slot" | wc -l))) || die "$slot: its btrfs filesystem spans $n devices, the spec's group has $(group_members "$slot" | wc -l)"
  FACT_LABEL[$slot]=-
  FACT_FEATURES[$slot]="devices=$n member-of=$(group_primary "$slot")"
}

xor_stream() {  # window dev...
  perl -e '
    my ($win, @devs) = @ARGV;
    my @fh;
    for my $d (@devs) { open(my $f, "<:raw", $d) or die "open $d: $!"; push @fh, $f; }
    binmode(STDOUT);
    my ($done, $chunk) = (0, 1048576);
    while ($done < $win) {
      my $n = $win - $done < $chunk ? $win - $done : $chunk;
      my $acc;
      for my $f (@fh) {
        my $buf;
        my $got = read($f, $buf, $n);
        die "short read" unless defined $got && $got == $n;
        $acc = defined $acc ? ($acc ^ $buf) : $buf;
      }
      print $acc;
      $done += $n;
    }
  ' -- "$@"
}

data_slots() {
  local s
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == data ]]; then echo "$s"; fi
  done
}

set_parity_window() {
  local s bytes win=$PARITY_WINDOW_BYTES
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == pool || ${D_KIND[$s]} == boot ]]; then continue; fi
    bytes=$(blockdev --getsize64 "${PART[$s]}")
    if ((bytes < win)); then win=$bytes; fi
  done
  PARITY_WINDOW=$win
}

window_sha() {  # dev
  head -c "$PARITY_WINDOW" -- "$1" | sha256sum | cut -d' ' -f1
}

# Parity: the bytewise XOR of the data partitions for the first parity disk,
# opaque bytes for the second. Only the first PARITY_WINDOW bytes are written;
# the rest of the parity partition stays as it was (sparse for l2).
write_parity() {  # slot (build only)
  local slot=$1 dev=${PART[$1]} data=() s
  assert_own_part "$slot"
  for s in $(data_slots); do data+=("${PART[$s]}"); done
  case $slot in
    parity)
      xor_stream "$PARITY_WINDOW" "${data[@]}" | dd of="$dev" bs=1M conv=notrunc status=none
      ;;
    *)
      head -c "$PARITY_WINDOW" /dev/urandom | dd of="$dev" bs=1M conv=notrunc status=none
      ;;
  esac
}

parity_facts() {  # slot
  local slot=$1 data=() s sig xor_sha
  for s in $(data_slots); do data+=("${PART[$s]}"); done
  PARITY_SHA[$slot]=$(window_sha "${PART[$slot]}")
  sig=$(probe_value TYPE "${PART[$slot]}")
  PARITY_SIG[$slot]=${sig:-none}
  case $slot in
    parity)
      PARITY_KIND[$slot]=xor
      xor_sha=$(xor_stream "$PARITY_WINDOW" "${data[@]}" | sha256sum | cut -d' ' -f1)
      [[ $xor_sha == "${PARITY_SHA[$slot]}" ]] || die "$slot: the parity disk is not the XOR of the data disks"
      ;;
    *) PARITY_KIND[$slot]=opaque ;;
  esac
}

# ------------------------------------------------------------ domains

# Reads every domain of the libvirt.img at domain.cfg's IMAGE_FILE, when one
# exists on a disk, and writes $1/domains.txt: the image, then each domain's
# firmware, bridges, vdisks (sha256 from the manifest scan), passthrough
# addresses and nvram. Sets LV_LOCATION to where the image is, array or cache.
scan_libvirt() {  # scan-dir
  local scan=$1 declared rel s hit=() slot img rows kind name a b c facts vfacts
  declared=$(flash_domain_cfg IMAGE_FILE)
  [[ $declared == /mnt/user/* ]] || return 0
  rel=${declared#/mnt/user/}
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then continue; fi
    if [[ -f $MNT/$s/$rel ]]; then hit+=("$s"); fi
  done
  ((${#hit[@]} > 0)) || return 0
  ((${#hit[@]} == 1)) || die "libvirt.img is on more than one disk (${hit[*]})"
  slot=${hit[0]}
  img="$MNT/$slot/$rel"
  case ${D_KIND[$slot]} in
    data) LV_LOCATION=array ;;
    pool) LV_LOCATION=cache ;;
  esac
  rows="$WORK/domains.rows"
  facts=$(domain_file_facts "$scan" "$rel") || die "libvirt.img is not in the scan of its disk"
  lv_attach "$img" ro
  perl -e '
    use strict;
    use warnings;
    my $dir = shift @ARGV;
    opendir(my $dh, "$dir/qemu") or die "no qemu/ in the image: $!";
    my @files = sort grep { /\.xml$/ } readdir $dh;
    die "the image holds no domain XML" unless @files;
    for my $f (@files) {
      open(my $fh, "<:raw", "$dir/qemu/$f") or die "open $f: $!";
      my $x = do { local $/; <$fh> };
      close $fh;
      my ($name) = $x =~ m{<name>([^<]+)</name>} or die "$f: no <name>";
      die "$f: the file name is not the domain name \"$name\"" unless "$name.xml" eq $f;
      my ($uuid) = $x =~ m{<uuid>([^<]+)</uuid>} or die "$f: no <uuid>";
      my ($loader) = $x =~ m{<loader[^>]*>([^<]+)</loader>};
      my ($nvram) = $x =~ m{<nvram>([^<]+)</nvram>};
      my @bridges = $x =~ m{<interface type=.bridge.>.*?<source bridge=.([^\x27"]+).}gs;
      printf "domain\t%s\tfile=qemu/%s\tuuid=%s\tfirmware=%s\tloader=%s\tbridge=%s\n", $name, $f, $uuid,
        defined $loader ? "uefi" : "bios", $loader // "-", @bridges ? join(",", @bridges) : "-";
      my @disks = $x =~ m{<disk type=.file. device=.(\w+).>(.*?)</disk>}gs;
      my $ndisks = () = $x =~ m{<disk }g;
      die "$f: a disk that is not a file-backed one" unless $ndisks * 2 == @disks;
      while (my ($dev, $body) = splice(@disks, 0, 2)) {
        my ($src) = $body =~ m{<source file=.([^\x27"]+).} or die "$f: a disk with no source file";
        printf "%s\t%s\t%s\n", $dev eq "cdrom" ? "media" : "vdisk", $name, $src;
      }
      for my $h ($x =~ m{<hostdev [^>]*type=.pci.[^>]*>(.*?)</hostdev>}gs) {
        my ($src) = $h =~ m{<source>(.*?)</source>}s or die "$f: a pci hostdev with no source";
        my ($d, $b, $s, $fn) = $src =~ m{<address domain=.0x([0-9a-f]+). bus=.0x([0-9a-f]+). slot=.0x([0-9a-f]+). function=.0x([0-9a-f]+).} or die "$f: a pci hostdev with no address";
        printf "pci\t%s\t%04x:%02x:%02x.%x\n", $name, hex($d), hex($b), hex($s), hex($fn);
      }
      for my $h ($x =~ m{<hostdev [^>]*type=.usb.[^>]*>(.*?)</hostdev>}gs) {
        my ($v) = $h =~ m{<vendor id=.0x([0-9a-f]+).} or die "$f: a usb hostdev with no vendor";
        my ($p) = $h =~ m{<product id=.0x([0-9a-f]+).} or die "$f: a usb hostdev with no product";
        printf "usb\t%s\t%04x:%04x\n", $name, hex($v), hex($p);
      }
      printf "nvram\t%s\t%s\n", $name, $nvram if defined $nvram;
    }
  ' -- "$LV_MNT" >"$rows" || die "cannot read the domains of $img"
  {
    printf '# hoserva unraid fixture domains, version 1\n'
    printf '# image: where libvirt.img is; domain: file, uuid, firmware, loader, bridges;\n'
    printf '# vdisk, media, nvram: owner, path, sha256 and bytes (/etc/libvirt/... for nvram); pci, usb: owner, address (vendor:product for usb)\n'
    printf '# expect-scan: the status of the scan'"'"'s libvirt.img row once the capture is read\n'
    printf 'image\t%s\tdisk=%s\tlocation=%s\t%s\n' "$declared" "$slot" "$LV_LOCATION" "$facts"
    while IFS=$'\t' read -r kind name a b c; do
      case $kind in
        vdisk | media)
          [[ $a == /mnt/user/* ]] || die "domain '$name': $kind '$a' is not under /mnt/user/"
          vfacts=$(domain_file_facts "$scan" "${a#/mnt/user/}") || die "domain '$name': $kind '$a' is not on exactly one disk"
          printf '%s\t%s\t%s\t%s\n' "$kind" "$name" "$a" "$vfacts"
          ;;
        nvram)
          [[ $a == /etc/libvirt/qemu/nvram/* && -f $LV_MNT/${a#/etc/libvirt/} ]] || die "domain '$name': its nvram '$a' is not in the image"
          printf 'nvram\t%s\t%s\tsha256=%s\tbytes=%s\n' "$name" "$a" "$(sha256sum -- "$LV_MNT/${a#/etc/libvirt/}" | cut -d' ' -f1)" "$(stat -c %s -- "$LV_MNT/${a#/etc/libvirt/}")"
          ;;
        *) printf '%s\t%s\t%s' "$kind" "$name" "$a"; if [[ -n ${b:-} ]]; then printf '\t%s' "$b"; fi; if [[ -n ${c:-} ]]; then printf '\t%s' "$c"; fi; printf '\n' ;;
      esac
    done <"$rows"
    if [[ $LV_LOCATION == cache ]]; then printf 'expect-scan\tlibvirt.img\twarn\n'; else printf 'expect-scan\tlibvirt.img\tinfo\n'; fi
  } >"$scan/domains.txt"
  lv_detach
}

# sha256 and bytes of the one file with this path under a share, from the scan.
domain_file_facts() {  # scan-dir path-below-/mnt/user
  cat -- "$1"/*.files | LC_ALL=C awk -F'\t' -v p="$2" '$6 == p { n++; h = $1; b = $2 } END { if (n != 1) exit 1; printf "sha256=%s\tbytes=%s", h, b }'
}

# ------------------------------------------------------------- manifest

render_expected() {  # scan-dir out-dir
  local scan=$1 out=$2 s f line
  mkdir -p -- "$out"
  : >"$scan/files"
  : >"$scan/entries"
  for s in "${SLOTS[@]}"; do
    if [[ -f $scan/$s.files ]]; then cat -- "$scan/$s.files" >>"$scan/files"; fi
    if [[ -f $scan/$s.entries ]]; then cat -- "$scan/$s.entries" >>"$scan/entries"; fi
  done
  {
    printf '# hoserva unraid fixture manifest, version 1\n'
    printf '# variant: %s\n' "$VARIANT"
    if [[ -n $OPTION ]]; then printf '# option: %s\n' "$OPTION"; fi
    printf '# columns: sha256, size, mode, owner, disk, path (tab separated; the path is relative to the disk root)\n'
    for s in $(fs_slots); do
      LC_ALL=C awk -F'\t' -v s="$s" '$5 == s { n++; b += $2 } END { printf "# disk %s files=%d bytes=%d\n", s, n, b }' "$scan/files"
    done
    LC_ALL=C awk -F'\t' '{ split($6, p, "/"); n[p[1]]++; b[p[1]] += $2 } END { for (k in n) printf "# share %s files=%d bytes=%d\n", k, n[k], b[k] }' "$scan/files" | LC_ALL=C sort
    LC_ALL=C sort -t $'\t' -k5,5 -k6,6 "$scan/files"
  } >"$out/manifest.sha256"
  {
    printf '# hoserva unraid fixture entries, version 1\n'
    printf '# columns: type, disk, path, detail, mode, owner (tab separated)\n'
    LC_ALL=C sort -t $'\t' -k2,2 -k3,3 -k1,1 -k4,4 "$scan/entries"
  } >"$out/entries.tsv"
  {
    printf '# hoserva unraid fixture layout, version 1\n'
    printf 'variant %s\n' "$VARIANT"
    if [[ -n $OPTION ]]; then printf 'option %s\n' "$OPTION"; fi
    printf 'unraid_version %s\n' "$SPEC_VERSION"
    for s in "${SLOTS[@]}"; do
      if is_boot_layout "$s"; then
        line="disk $s kind=${D_KIND[$s]} scheme=gpt layout=internal-boot boot=${D_BOOT[$s]} fs=${D_FS[$s]}"
        if [[ ${D_KIND[$s]} != boot ]]; then
          line+=" label=${FACT_LABEL[$s]}"
          if [[ -n ${FACT_FEATURES[$s]} ]]; then line+=" ${FACT_FEATURES[$s]}"; fi
        fi
        printf '%s\n' "$line"
        printf '%s\n' "${BOOT_LAYOUT[$s]}"
        continue
      fi
      line="disk $s kind=${D_KIND[$s]} scheme=${LAYOUT_SCHEME[$s]} start=${LAYOUT_START[$s]} sectors=${LAYOUT_SECTORS[$s]} type=${LAYOUT_TYPE[$s]} fs=${D_FS[$s]}"
      if [[ ${D_KIND[$s]} != parity ]]; then
        line+=" label=${FACT_LABEL[$s]}"
        if [[ -n ${FACT_FEATURES[$s]} ]]; then line+=" ${FACT_FEATURES[$s]}"; fi
      fi
      printf '%s\n' "$line"
    done
    for s in "${SLOTS[@]}"; do
      if [[ ${D_KIND[$s]} != parity ]]; then continue; fi
      f=$(data_slots | tr '\n' ',')
      printf 'parity %s kind=%s window=%s sha256=%s signature=%s data=%s\n' "$s" "${PARITY_KIND[$s]}" "$PARITY_WINDOW" "${PARITY_SHA[$s]}" "${PARITY_SIG[$s]}" "${f%,}"
    done
  } >"$out/layout.txt"
  if [[ -f $scan/domains.txt ]]; then cp -- "$scan/domains.txt" "$out/domains.txt"; fi
}

declare -A LAYOUT_SCHEME LAYOUT_START LAYOUT_SECTORS LAYOUT_TYPE BOOT_LAYOUT

record_layout() {  # slot
  local slot=$1
  if is_boot_layout "$slot"; then return 0; fi
  read_layout "${WHOLE[$slot]}"
  LAYOUT_SCHEME[$slot]=$L_SCHEME
  LAYOUT_START[$slot]=$L_START
  LAYOUT_SECTORS[$slot]=$L_SECTORS
  LAYOUT_TYPE[$slot]=$L_TYPE
}

assert_parity_signature() {
  local slot expected=$SPEC_EXPECT_PARITY_SIGNATURE
  [[ -n $expected ]] || return 0
  for slot in "${SLOTS[@]}"; do
    if [[ $slot != parity ]]; then continue; fi
    [[ ${PARITY_SIG[$slot]} == "$expected" ]] || die "$slot: the parity disk's signature is '${PARITY_SIG[$slot]}', the spec expects '$expected'"
  done
}

# ------------------------------------------------------- internal boot

# The boot area of the device: the FAT32 EFI system partition and the ZFS pool
# "flash" whose dataset flash/boot holds the flash tree, the way mkbootable
# makes them (doc 08 §2). The data area, partition 4, is formatted by
# format_part when the device also holds a pool.
create_boot_pool() {  # slot
  local slot=$1 whole=${WHOLE[$1]} esp zfs
  assert_own_virtio "$slot" "$whole"
  esp=$(boot_part "$whole" 2)
  zfs=$(boot_part "$whole" 3)
  mkfs.fat -F32 -n EFI "$esp" >/dev/null
  zpool create -f -m none -o compatibility=grub2 -o ashift=12 -o autotrim=on -o cachefile=none "$BOOT_ZFS_POOL" "$zfs"
  ZPOOLS+=("$BOOT_ZFS_POOL")
  zfs create -o mountpoint=legacy -o compression=lz4 -o atime=off -o xattr=sa "$BOOT_ZFS_DATASET"
  mkdir -p -- "$MNT/flash-boot"
  mount -t zfs "$BOOT_ZFS_DATASET" "$MNT/flash-boot"
  MOUNTED+=("$MNT/flash-boot")
  BOOT_POOL_GUID=$(zpool get -H -o value guid "$BOOT_ZFS_POOL")
  [[ $BOOT_POOL_GUID =~ ^[0-9]+$ ]] || die "cannot read the GUID of the ZFS pool $BOOT_ZFS_POOL"
}

fill_boot_pool() {
  mountpoint -q "$MNT/flash-boot" || die "the boot dataset is not mounted"
  cp -a --no-preserve=ownership -- "$FLASH/." "$MNT/flash-boot/"
  sync
}

# Reads the boot device back the way the scan sees it: the partition names and
# types from the udev database, the filesystem facts from libblkid, and the
# pool's GUID. Dies when the table is not Unraid's internal-boot layout.
record_boot_layout() {  # slot
  local slot=$1 whole=${WHOLE[$1]} dump n dev props name type line start size fs label want_name want_type text="" guid
  local -a names=('BIOS\x20Boot\x20Partition' 'EFI\x20System\x20Partition' 'Unraid\x20Boot\x20Partition' '')
  local -a types=(21686148-6449-6e6f-744e-656564454649 c12a7328-f81f-11d2-ba4b-00a0c93ec93b 0fc63daf-8483-4772-8e79-3d69d8477de4 0fc63daf-8483-4772-8e79-3d69d8477de4)
  dump=$(sfdisk -d "$whole") || die "sfdisk -d $whole failed"
  [[ $(sed -n 's/^label: //p' <<<"$dump") == gpt ]] || die "$slot: the boot device has no GPT"
  udevadm settle
  for n in 1 2 3 4; do
    dev=$(boot_part "$whole" "$n")
    props=$(udevadm info -q property -n "$dev") || die "udevadm info $dev failed"
    name=$(sed -n 's/^ID_PART_ENTRY_NAME=//p' <<<"$props")
    type=$(sed -n 's/^ID_PART_ENTRY_TYPE=//p' <<<"$props")
    want_name=${names[$((n - 1))]}
    want_type=${types[$((n - 1))]}
    [[ $name == "$want_name" ]] || die "$slot: partition $n is named '$name', Unraid's internal-boot layout names it '$want_name'"
    [[ $type == "$want_type" ]] || die "$slot: partition $n has type '$type', Unraid's internal-boot layout has '$want_type'"
    line=$(grep -E "^$dev : " <<<"$dump") || die "$slot: sfdisk does not list $dev"
    start=$(sed -n 's/.*start= *\([0-9]*\),.*/\1/p' <<<"$line")
    size=$(sed -n 's/.*size= *\([0-9]*\),.*/\1/p' <<<"$line")
    fs=$(probe_value TYPE "$dev")
    label=$(probe_value LABEL "$dev")
    text+="part $slot $n name=${name:--} type=$type start=$start sectors=$size fs=${fs:--} label=${label:--}"$'\n'
  done
  guid=$(probe_value UUID "$(boot_part "$whole" 3)")
  [[ $(probe_value TYPE "$(boot_part "$whole" 2)") == vfat && $(probe_value LABEL "$(boot_part "$whole" 2)") == EFI ]] || die "$slot: partition 2 is not FAT32 labelled EFI"
  [[ $(probe_value TYPE "$(boot_part "$whole" 3)") == zfs_member && $(probe_value LABEL "$(boot_part "$whole" 3)") == "$BOOT_ZFS_POOL" ]] \
    || die "$slot: partition 3 is not a member of the ZFS pool $BOOT_ZFS_POOL"
  text+="zpool $slot name=$BOOT_ZFS_POOL dataset=$BOOT_ZFS_DATASET guid=$guid"
  BOOT_LAYOUT[$slot]=$text
}

# Verify: the flash tree the boot pool holds, read back from the device
# through a read-only import, must be the flash tree this build recorded.
verify_boot_pool() {  # slot
  local slot=$1 zfs
  zfs=$(boot_part "${WHOLE[$slot]}" 3)
  zpool import -N -o readonly=on -o cachefile=none -d "$zfs" "$BOOT_ZFS_POOL" || die "$slot: cannot import the boot pool read-only"
  ZPOOLS+=("$BOOT_ZFS_POOL")
  mkdir -p -- "$MNT/flash-boot"
  mount -t zfs -o ro "$BOOT_ZFS_DATASET" "$MNT/flash-boot"
  MOUNTED+=("$MNT/flash-boot")
  (cd -- "$MNT/flash-boot" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) >"$WORK/boot-pool.sha256"
  diff -u "$EXP/flash.sha256" "$WORK/boot-pool.sha256" >&2 || die "$slot: the boot pool does not hold the flash tree"
}

# ------------------------------------------------------ expected scan result

# What the scan must say about each disk is authored in the variant's expect
# file; the builder measures every claim on the disks themselves and refuses to
# build a variant whose expect file says something the disks do not show.
declare -A EXPECT_DISK M_VERDICT M_FACTS
EXPECT_WARNS=()
EXPECT_CONFIG_SOURCE=""
HAVE_EXPECT=0

load_expect() {
  local file=$VDIR/expect line w slot verdict reason s warn=0 cfg=0
  [[ -f $file ]] || return 0
  HAVE_EXPECT=1
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ -z ${line//[[:space:]]/} || $line == \#* ]]; then continue; fi
    read -r -a w <<<"$line"
    case ${w[0]} in
      disk)
        slot=${w[1]:-}
        verdict=${w[2]:-}
        reason=${w[3]:--}
        [[ -n ${D_KIND[$slot]:-} ]] || die "expect: no disk '$slot' in the spec"
        [[ -z ${EXPECT_DISK[$slot]:-} ]] || die "expect: disk '$slot' is listed twice"
        case "$verdict $reason" in
          "parity -" | "adopt -" | "recreated -" | "boot-device -" | "boot-device cache-recreated" | \
            "refuse integrity" | "refuse encrypted" | "refuse zfs" | "refuse multi-device-btrfs") ;;
          *) die "expect: cannot read the verdict of disk '$slot': $line" ;;
        esac
        EXPECT_DISK[$slot]="$verdict $reason"
        ;;
      warn)
        [[ ${w[1]:-} == capture-missing && ${#w[@]} -eq 2 ]] || die "expect: cannot read: $line"
        EXPECT_WARNS+=("${w[1]}")
        ;;
      config-source)
        [[ ${w[1]:-} == zip && ${#w[@]} -eq 2 ]] || die "expect: cannot read: $line"
        EXPECT_CONFIG_SOURCE=zip
        ;;
      *) die "expect: cannot read: $line" ;;
    esac
  done <"$file"
  for s in "${SLOTS[@]}"; do
    [[ -n ${EXPECT_DISK[$s]:-} ]] || die "expect: disk '$s' has no verdict"
  done
  if ((${#EXPECT_WARNS[@]} > 0)); then warn=1; fi
  if [[ -n $EXPECT_CONFIG_SOURCE ]]; then cfg=1; fi
  if [[ $SPEC_CAPTURE == none ]]; then
    ((warn)) || die "expect: the spec says capture=none, so the expect file must record the capture-missing warning"
  else
    ((!warn)) || die "expect: the capture-missing warning, but the spec has a capture"
  fi
  if boot_slot >/dev/null; then
    ((cfg)) || die "expect: the spec has an internal-boot disk, so the expect file must say config-source zip"
  else
    ((!cfg)) || die "expect: config-source zip, but the spec has no internal-boot disk"
  fi
}

# The Unraid slot number of a data disk, whose diskFsType.N the flash carries.
flash_fs_type() {  # slot
  local n
  n=$(slot_index "$1")
  sed -n "s/^diskFsType\\.$n=\"\\(.*\\)\"\$/\\1/p" "$FLASH/config/disk.cfg" | head -n 1
}

# Runs the read-only check Q23 names for the filesystem, on the unmounted
# partition. Sets CHECK_TOOL and CHECK_RC; an exit status that is neither clean
# nor the tool's own "this filesystem has errors" is a failure of the check, not
# a verdict.
run_adopt_check() {  # slot fs
  local slot=$1 fs=$2 dev=${PART[$1]} argv=() bad_rc rc=0
  case $fs in
    xfs) argv=(xfs_repair -n "$dev"); bad_rc=1; CHECK_TOOL=xfs_repair-n ;;
    ext4) argv=(e2fsck -n "$dev"); bad_rc=4; CHECK_TOOL=e2fsck-n ;;
    btrfs) argv=(btrfs check --readonly "$dev"); bad_rc=1; CHECK_TOOL=btrfs-check-readonly ;;
    *) die "$slot: no read-only check for '$fs'" ;;
  esac
  "${argv[@]}" >/dev/null 2>&1 || rc=$?
  if ((rc != 0 && rc != bad_rc)); then die "$slot: ${argv[*]} exited $rc, which is neither clean nor the tool's report of errors"; fi
  CHECK_RC=$rc
}

# Sets M_VERDICT[slot] ("<verdict> <reason>") and M_FACTS[slot] from the disk.
measure_slot() {  # slot
  local slot=$1 dev=${PART[$1]} raw flash n facts
  case ${D_KIND[$slot]} in
    parity)
      M_VERDICT[$slot]="parity -"
      M_FACTS[$slot]="kind=parity"
      return 0
      ;;
    boot)
      M_VERDICT[$slot]="boot-device -"
      M_FACTS[$slot]="layout=internal-boot partition3=zfs_member pool=$BOOT_ZFS_POOL boot=${D_BOOT[$slot]}"
      return 0
      ;;
    pool)
      if is_boot_layout "$slot"; then
        M_VERDICT[$slot]="boot-device cache-recreated"
        M_FACTS[$slot]="layout=internal-boot partition3=zfs_member pool=$BOOT_ZFS_POOL partition4=$(probe_value TYPE "$dev") boot=${D_BOOT[$slot]}"
      else
        M_VERDICT[$slot]="recreated -"
        M_FACTS[$slot]="pool=${D_POOL[$slot]} fs=$(probe_value TYPE "$dev")"
      fi
      return 0
      ;;
  esac
  raw=$(probe_value TYPE "$dev")
  flash=$(flash_fs_type "$slot")
  case $raw in
    crypto_LUKS)
      [[ $flash == luks:* ]] || die "$slot: the partition is a LUKS container, but the flash says diskFsType '$flash'"
      cryptsetup isLuks "$dev" || die "$slot: libblkid reports LUKS, cryptsetup does not"
      M_VERDICT[$slot]="refuse encrypted"
      M_FACTS[$slot]="partition=crypto_LUKS flash=$flash"
      ;;
    zfs_member)
      [[ $flash == zfs ]] || die "$slot: the partition is a ZFS member, but the flash says diskFsType '$flash'"
      M_VERDICT[$slot]="refuse zfs"
      M_FACTS[$slot]="partition=zfs_member flash=$flash"
      ;;
    xfs | ext4 | btrfs)
      [[ $flash == "$raw" ]] || die "$slot: the partition is $raw, but the flash says diskFsType '$flash'"
      n=1
      facts="fs=$raw"
      if [[ $raw == btrfs ]]; then
        n=$(btrfs_devices "$dev")
        facts+=" devices=$n"
      fi
      if ((n > 1)); then
        M_VERDICT[$slot]="refuse multi-device-btrfs"
        M_FACTS[$slot]="partition=btrfs devices=$n flash=$flash"
      else
        run_adopt_check "$slot" "$raw"
        if ((CHECK_RC == 0)); then M_VERDICT[$slot]="adopt -"; else M_VERDICT[$slot]="refuse integrity"; fi
        M_FACTS[$slot]="$facts flash=$flash check=$CHECK_TOOL exit=$CHECK_RC"
      fi
      ;;
    *) die "$slot: the partition holds '$raw', which no variant of this builder expects" ;;
  esac
}

# Measures every disk and requires each to match the expect file.
check_expect() {
  local s
  ((HAVE_EXPECT)) || return 0
  for s in "${SLOTS[@]}"; do
    measure_slot "$s"
    [[ ${M_VERDICT[$s]} == "${EXPECT_DISK[$s]}" ]] \
      || die "expect: disk '$s': the expect file says '${EXPECT_DISK[$s]}', the disk shows '${M_VERDICT[$s]}' (${M_FACTS[$s]})"
  done
}

have_refusal() {
  local s
  ((HAVE_EXPECT)) || return 1
  for s in "${SLOTS[@]}"; do
    if [[ ${EXPECT_DISK[$s]%% *} == refuse ]]; then return 0; fi
  done
  return 1
}

render_scan() {  # out-dir
  local out=$1 s v r w refused=() adopted=()
  {
    printf '# hoserva unraid fixture expected scan result, version 1\n'
    printf '# disk: slot, verdict, reason, then what the builder measured on the disk\n'
    for s in "${SLOTS[@]}"; do
      read -r v r <<<"${M_VERDICT[$s]}"
      printf 'disk\t%s\t%s\t%s' "$s" "$v" "$r"
      for w in ${M_FACTS[$s]}; do printf '\t%s' "$w"; done
      printf '\n'
      case $v in
        refuse) refused+=("$s") ;;
        adopt) adopted+=("$s") ;;
      esac
    done
    printf 'refused\t%s\n' "$(IFS=,; printf '%s' "${refused[*]:--}")"
    printf 'adopted\t%s\n' "$(IFS=,; printf '%s' "${adopted[*]:--}")"
    for w in "${EXPECT_WARNS[@]}"; do
      printf 'warn\t%s\ttemplates=all-unknown\tpreselected=none\n' "$w"
    done
    if [[ -n $EXPECT_CONFIG_SOURCE ]]; then printf 'config-source\tzip\tusb-stick=none\n'; fi
  } >"$out/scan.txt"
}

# The sha256 of every whole source device, one line per disk, taken before any
# scan reads them. The same function runs when verifying.
source_hashes() {
  local s bytes hash
  printf '# hoserva unraid fixture source disks, version 1\n'
  printf '# columns: sha256 of the whole device, bytes, slot (tab separated)\n'
  for s in "${SLOTS[@]}"; do
    if [[ $TIER == l3 ]]; then
      blockdev --flushbufs "${WHOLE[$s]}"
      bytes=$(blockdev --getsize64 "${WHOLE[$s]}")
    else
      bytes=$(stat -c %s -- "${WHOLE[$s]}")
    fi
    hash=$(sha256sum -- "${WHOLE[$s]}" | cut -d' ' -f1)
    printf '%s\t%s\t%s\n' "$hash" "$bytes" "$s"
  done
}

# XFS metadata damage that leaves the files readable: the first bytes of the
# root block of allocation group 0's free-space B-tree (its magic number, level
# and record count) are zeroed, so xfs_repair -n reports a bad B-tree block
# while every inode and extent is untouched.
corrupt_xfs_metadata() {  # slot
  local slot=$1 dev=${PART[$1]} bs root
  assert_own_part "$slot"
  bs=$(xfs_db -r -c 'sb 0' -c 'p blocksize' "$dev" | sed -n 's/^blocksize = //p')
  root=$(xfs_db -r -c 'agf 0' -c 'p bnoroot' "$dev" | sed -n 's/^bnoroot = //p')
  [[ $bs =~ ^[0-9]+$ && $root =~ ^[0-9]+$ ]] || die "$slot: cannot find the free-space B-tree of allocation group 0"
  head -c 16 /dev/zero | dd of="$dev" bs=1 seek=$((root * bs)) conv=notrunc status=none
  sync
}

# ---------------------------------------------------------------- flash

disk_size_kib() { echo $((D_SIZE[$1] / 1024)); }

gen_disk_cfg() {
  local s n first=xfs
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == data ]]; then
      first=$(unraid_fstype "$s")
      break
    fi
  done
  printf 'startArray="yes"\nspindownDelay="30"\nspinupGroups="no"\ndefaultFsType="%s"\nqueueDepth="auto"\nmd_write_method="auto"\n' "$first"
  for s in "${SLOTS[@]}"; do
    case ${D_KIND[$s]} in
      parity) n=$(slot_index "$s"); printf 'diskIdSlot.%s="-"\ndiskSpindownDelay.%s="-1"\n' "$n" "$n" ;;
      data) n=$(slot_index "$s"); printf 'diskIdSlot.%s="-"\ndiskFsType.%s="%s"\ndiskSpindownDelay.%s="-1"\ndiskSpinupGroup.%s=""\n' "$n" "$n" "$(unraid_fstype "$s")" "$n" "$n" ;;
    esac
  done
}

# Unraid's /var/local/emhttp/disks.ini, the slot to disk identity table that
# the prepare script copies into the flash (Q89). An internal-boot device also
# has the "flash" section of type Boot (a USB stick's is Flash), and its pool a
# bootPool of dedicated, or yes when the device also holds the pool (doc 08 §2).
gen_disks_ini() {
  local s letters=(b c d e f g h i j k l m n o p) i=0 type
  for s in "${SLOTS[@]}"; do
    case ${D_KIND[$s]} in
      parity) type=Parity ;;
      data) type=Data ;;
      pool | boot) type=Cache ;;
    esac
    printf '["%s"]\nidx="%s"\nname="%s"\ndevice="sd%s"\nid="FIXTURE_%s"\nsize="%s"\nstatus="DISK_OK"\ntype="%s"\nfsType="%s"\nrotational="1"\nspindownDelay="-1"\nidSb="FIXTURE_%s"\nsizeSb="%s"\n' \
      "$s" "$(slot_index "$s")" "$s" "${letters[$i]}" "$(serial_of "$s")" "$(disk_size_kib "$s")" "$type" "$(unraid_fstype "$s")" "$(serial_of "$s")" "$(disk_size_kib "$s")"
    if is_boot_layout "$s"; then
      if [[ ${D_BOOT[$s]} == dedicated ]]; then printf 'bootPool="dedicated"\n'; else printf 'bootPool="yes"\n'; fi
      printf '["flash"]\nidx="54"\nname="flash"\ndevice="sd%s"\nid="FIXTURE_%s"\nsize="%s"\nstatus="DISK_OK"\ntype="Boot"\nfsType="zfs"\nrotational="1"\nspindownDelay="-1"\nidSb="FIXTURE_%s"\nsizeSb="%s"\n' \
        "${letters[$i]}" "$(serial_of "$s")" "$(disk_size_kib "$s")" "$(serial_of "$s")" "$(disk_size_kib "$s")"
    fi
    i=$((i + 1))
  done
}

appledouble_header() {
  printf '\000\005\026\007\000\002\000\000Mac OS X        \000\002\000\000\000\011\000\000\000\062\000\000\000\030\000\000\000\002\000\000\000\102\000\000\000\020'
  head -c 32 /dev/zero
}

# The flash tree of an internal-boot server has grub/ and an empty efi/ mount
# point where a USB stick has syslinux/ and EFI/ (doc 08 §2); the scan must
# require neither.
build_flash() {
  local s p rel f serial
  rm -rf -- "$FLASH"
  mkdir -p -- "$FLASH"
  cp -a --no-preserve=ownership -- "$FIXTURES/common/flash/." "$FLASH/"
  if boot_slot >/dev/null; then rm -rf -- "$FLASH/syslinux"; fi
  if [[ $SPEC_CAPTURE == none && -e $VDIR/flash/config/hoserva ]]; then die "$VARIANT: the spec says capture=none, but flash/config/hoserva exists"; fi
  if [[ -d $VDIR/flash ]]; then cp -a --no-preserve=ownership -- "$VDIR/flash/." "$FLASH/"; fi
  if [[ -n $OPTION && -d $VDIR/options/$OPTION/flash ]]; then cp -a --no-preserve=ownership -- "$VDIR/options/$OPTION/flash/." "$FLASH/"; fi
  if [[ $SPEC_CAPTURE != none ]]; then mkdir -p -- "$FLASH/config/hoserva"; fi
  printf '# Version %s %s\nAuthored for Hoserva migration fixtures; not a release note.\n' "$SPEC_VERSION" "$SPEC_RELEASE" >"$FLASH/changes.txt"
  gen_disk_cfg >"$FLASH/config/disk.cfg"
  if [[ $SPEC_CAPTURE != none ]]; then gen_disks_ini >"$FLASH/config/hoserva/disks.ini"; fi
  head -c 4096 /dev/urandom >"$FLASH/config/super.dat"
  head -c 4096 /dev/urandom >"$FLASH/config/super.old"
  for f in bzimage bzroot bzfirmware bzmodules; do printf 'placeholder, not a kernel image\n' >"$FLASH/$f"; done
  mkdir -p -- "$FLASH/previous"
  if boot_slot >/dev/null; then
    mkdir -p -- "$FLASH/grub" "$FLASH/efi"
    printf 'set default=0\nset timeout=3\nmenuentry "Fixture OS" {\n  linux /bzimage unraiduuid=%s\n  initrd /bzroot\n}\n' "$BOOT_POOL_GUID" >"$FLASH/grub/grub.cfg"
  else
    mkdir -p -- "$FLASH/EFI/boot"
    printf 'placeholder, not an EFI binary\n' >"$FLASH/EFI/boot/bootx64.efi"
  fi
  for f in bzimage bzroot; do printf 'placeholder, the previous release\n' >"$FLASH/previous/$f"; done
  printf '# Version 6.12.10 2024-01-01\nAuthored for Hoserva migration fixtures; not a release note.\n' >"$FLASH/previous/changes.txt"
  mkdir -p -- "$FLASH/.git/refs/heads" "$FLASH/.git/objects"
  printf 'ref: refs/heads/master\n' >"$FLASH/.git/HEAD"
  printf '[core]\n\trepositoryformatversion = 0\n\tbare = false\n' >"$FLASH/.git/config"
  printf 'config/wireguard/** filter=noprivatekeys\n' >"$FLASH/.gitattributes"
  for rel in "${APPLEDOUBLE[@]}"; do
    [[ -f $FLASH/$rel ]] || die "appledouble: $rel is not in the flash tree"
    appledouble_header >"$FLASH/$(dirname -- "$rel")/._$(basename -- "$rel")"
  done
  for s in "${SLOTS[@]}"; do
    serial=$(serial_of "$s")
    p="s|@ID:$s@|FIXTURE_$serial|g"
    while IFS= read -r -d '' f; do sed -i -e "$p" -- "$f"; done < <(grep -rlZ -e "@ID:$s@" "$FLASH/config" || true)
  done
  if grep -rqE '@ID:[a-z0-9]+@' "$FLASH/config"; then die "the flash tree names a disk the spec does not declare"; fi

  find "$FLASH" -not -path "$FLASH/config/hoserva/*" -exec touch -h -d "$FLASH_MTIME" {} +
  if [[ -f $FLASH/config/hoserva/capture.json ]]; then
    local at
    at=$(sed -n 's/.*"captured_at": *"\([^"]*\)".*/\1/p' "$FLASH/config/hoserva/capture.json")
    [[ -n $at ]] || die "capture.json has no captured_at"
    find "$FLASH/config/hoserva" -exec touch -h -d "$at" {} +
  fi
}

check_capture() {  # flash-dir
  local flash=$1 at at_epoch newest=0 m f loc bs shared mode
  if [[ $SPEC_CAPTURE == none ]]; then
    [[ ! -e $flash/config/hoserva ]] || die "$VARIANT has capture=none, but its flash has config/hoserva/"
    return 0
  fi
  if [[ ! -f $flash/config/hoserva/capture.json ]]; then
    [[ ${HOSERVA_FIXTURE_NO_CAPTURE:-} == 1 ]] && return 0
    die "$VARIANT has no capture under flash/config/hoserva/: generate it with make vm-unraid-capture"
  fi
  [[ -f $flash/config/hoserva/containers.json && -f $flash/config/hoserva/networks.json && -f $flash/config/hoserva/disks.ini ]] \
    || die "$VARIANT: the capture is incomplete"
  if [[ -n $LV_LOCATION ]]; then
    loc=$(sed -n 's/.*"libvirt_img_location": *"\([^"]*\)".*/\1/p' "$flash/config/hoserva/capture.json")
    [[ $loc == "$LV_LOCATION" ]] || die "capture.json records libvirt.img as '${loc:-nothing}', but it is on the $LV_LOCATION: regenerate the capture with make vm-unraid-capture"
  fi
  if bs=$(boot_slot); then
    mode=$(sed -n 's/.*"boot": {"mode": "\([^"]*\)", "filesystem": "\([^"]*\)".*/\1 \2/p' "$flash/config/hoserva/capture.json")
    [[ $mode == "internal zfs" ]] || die "capture.json records boot mode and filesystem '${mode:-nothing}', but $bs is an internal boot device: regenerate the capture with make vm-unraid-capture"
    grep -q '"devices": \[{"name"' "$flash/config/hoserva/capture.json" || die "capture.json names no boot device: regenerate the capture with make vm-unraid-capture"
    if [[ ${D_BOOT[$bs]} == dedicated ]]; then shared=false; else shared=true; fi
    grep -q "\"shared_with_data_pool\": $shared}" "$flash/config/hoserva/capture.json" \
      || die "capture.json does not say shared_with_data_pool is $shared for $bs: regenerate the capture with make vm-unraid-capture"
  fi
  at=$(sed -n 's/.*"captured_at": *"\([^"]*\)".*/\1/p' "$flash/config/hoserva/capture.json")
  at_epoch=$(date -u -d "$at" +%s) || die "capture.json: bad captured_at '$at'"
  while IFS= read -r -d '' f; do
    m=$(stat -c %Y -- "$f")
    if ((m > newest)); then newest=$m; fi
  done < <(find "$flash/config/plugins/dockerMan/templates-user" -type f -print0)
  ((at_epoch > newest)) || die "capture.json is not newer than every template (captured_at $at)"
}

make_zips() {
  local entries=() e
  mkdir -p -- "$EXP"
  (
    cd -- "$FLASH"
    shopt -s dotglob nullglob
    for e in *; do
      case $e in -*) die "refusing to zip an entry starting with '-': $e" ;; esac
    done
    rm -f -- "$EXP/flash-backup.zip" "$EXP/flash-hand-zipped.zip"
    # Unraid's flash_backup: chdir to /boot, zip every top-level entry but
    # prev and previous.
    for e in *; do
      case $e in prev | previous) ;; *) entries+=("$e") ;; esac
    done
    TZ=UTC zip -qrX "$EXP/flash-backup.zip" "${entries[@]}"
    entries=(*)
    TZ=UTC zip -qrX "$EXP/flash-hand-zipped.zip" "${entries[@]}"
  )
  (cd -- "$FLASH" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) >"$EXP/flash.sha256"
}

verify_zips() {
  local z dir list expect
  for z in flash-backup flash-hand-zipped; do
    [[ -f $EXP/$z.zip ]] || die "missing $EXP/$z.zip"
    list=$(unzip -Z1 "$EXP/$z.zip")
    for expect in config/disk.cfg changes.txt bzimage config/plugins/dockerMan/templates-user/my-notes.xml \
      config/plugins/dockerMan/templates-user/._my-notes.xml .git/HEAD; do
      grep -qxF -- "$expect" <<<"$list" || die "$z.zip has no $expect"
    done
    if boot_slot >/dev/null; then
      grep -qxF -- grub/grub.cfg <<<"$list" || die "$z.zip has no grub/grub.cfg"
      grep -qE '^(syslinux|EFI)/' <<<"$list" && die "$z.zip of an internal-boot server holds syslinux/ or EFI/"
    else
      grep -qxF -- syslinux/syslinux.cfg <<<"$list" || die "$z.zip has no syslinux/syslinux.cfg"
    fi
    grep -qE '^config/' <<<"$list" || die "$z.zip: config/ is not at its root"
    grep -qE '^(boot|flash)/' <<<"$list" && die "$z.zip: entries are not rooted at /boot"
    dir="$WORK/$z"
    mkdir -p -- "$dir"
    unzip -q -o "$EXP/$z.zip" -d "$dir"
    if [[ $z == flash-backup ]]; then
      grep -qE '^previous/' <<<"$list" && die "$z.zip contains previous/"
      (cd -- "$dir" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) >"$WORK/$z.sha256"
      grep -v '  ./previous/' "$EXP/flash.sha256" >"$WORK/$z.expected" || die "no files left in flash.sha256 after dropping previous/"
      diff -u "$WORK/$z.expected" "$WORK/$z.sha256" >&2 || die "$z.zip does not hold the flash tree"
    else
      grep -qE '^previous/' <<<"$list" || die "$z.zip has no previous/"
      (cd -- "$dir" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum) >"$WORK/$z.sha256"
      diff -u "$EXP/flash.sha256" "$WORK/$z.sha256" >&2 || die "$z.zip does not hold the flash tree"
    fi
  done
  check_capture "$FLASH"
}

# ---------------------------------------------------------------- build

check_tools() {
  local t s needs_zfs=0 tools=(sfdisk sgdisk mkfs.xfs mkfs.btrfs mkfs.ext4 xfs_info xfs_repair xfs_db e2fsck btrfs blkid numfmt perl setfattr getfattr zip unzip sha256sum find losetup)
  for s in "${SLOTS[@]}"; do
    case ${D_FS[$s]} in
      luks-xfs) tools+=(cryptsetup) ;;
      zfs) needs_zfs=1 ;;
    esac
    if is_boot_layout "$s"; then
      needs_zfs=1
      tools+=(mkfs.fat udevadm)
    fi
  done
  if ((needs_zfs)); then tools+=(zpool zfs); fi
  for t in "${tools[@]}"; do
    command -v "$t" >/dev/null 2>&1 || die "$t is not installed"
  done
  if ((needs_zfs)) && ! grep -q '^zfs ' /proc/modules; then
    modprobe zfs || die "the zfs kernel module is not available in this guest"
  fi
}

# Zeroes the first and last 2 MiB of a partition (all of a smaller one), where
# a ZFS pool keeps its labels, so a signature left by an earlier build of another
# variant on the same disk cannot be read as a second filesystem beside the new
# one.
scrub_partition_edges() {  # block device
  local dev=$1 sectors edge=4096
  sectors=$(blockdev --getsz "$dev")
  [[ $sectors =~ ^[1-9][0-9]*$ ]] || die "refusing $dev: cannot read its size"
  if ((sectors <= 2 * edge)); then
    dd if=/dev/zero of="$dev" bs=512 count="$sectors" status=none
    return 0
  fi
  dd if=/dev/zero of="$dev" bs=512 count="$edge" status=none
  dd if=/dev/zero of="$dev" bs=512 seek=$((sectors - edge)) count="$edge" status=none
}

build() {
  local s scan m
  [[ ! -e $OUT/expected ]] || die "$OUT already holds a build; remove it first"
  if [[ $TIER == l3 ]] && dpkg-query -W -f '${Status}' hoserva 2>/dev/null | grep -q 'install ok installed'; then
    die "Hoserva is installed in this guest: build the fixture before deploying the .deb"
  fi
  check_tools
  mkdir -p -- "$OUT" "$MNT"
  WORK=$(mktemp -d "$OUT/work.XXXXXX")
  if [[ $TIER == l3 ]]; then
    # every target is identified and checked before the first one is written
    for s in "${SLOTS[@]}"; do
      WHOLE[$s]=$(l3_whole_dev "$s")
      assert_unused "${WHOLE[$s]}"
      assert_spec_size "$s" "$(blockdev --getsize64 "${WHOLE[$s]}" 2>/dev/null)"
    done
  fi
  for s in "${SLOTS[@]}"; do new_disk "$s"; done
  if [[ $TIER == l3 ]]; then
    for s in "${SLOTS[@]}"; do
      if is_boot_layout "$s"; then
        for m in 2 3 4; do scrub_partition_edges "$(boot_part "${WHOLE[$s]}" "$m")"; done
      else
        scrub_partition_edges "${PART[$s]}"
      fi
    done
  fi
  for s in "${SLOTS[@]}"; do record_layout "$s"; done
  set_parity_window
  if s=$(boot_slot); then create_boot_pool "$s"; fi
  for s in $(fs_slots); do
    format_part "$s"
    mount_fs "$s" rw
  done
  for s in "${SLOTS[@]}"; do
    if is_boot_layout "$s"; then record_boot_layout "$s"; fi
  done
  build_flash
  if boot_slot >/dev/null; then fill_boot_pool; fi
  DEFERRED_MODES=()
  seed_run "$VDIR/seed"
  if [[ -n $OPTION ]]; then
    [[ -f $VDIR/options/$OPTION/seed ]] || die "option '$OPTION' has no seed"
    seed_run "$VDIR/options/$OPTION/seed"
  fi
  scan="$WORK/scan"
  mkdir -p -- "$scan"
  for s in $(fs_slots); do normalize_tree "$MNT/$s"; done
  apply_deferred_modes
  sync
  for s in $(fs_slots); do
    scan_tree "$MNT/$s" "$s" "$scan/$s.files" "$scan/$s.entries"
    fs_facts "$s"
  done
  for s in "${SLOTS[@]}"; do
    if is_group_member "$s"; then member_facts "$s"; fi
  done
  scan_libvirt "$scan"
  unmount_all
  sync
  for s in "${SLOTS[@]}"; do
    if [[ -n ${D_CORRUPT[$s]:-} ]]; then corrupt_xfs_metadata "$s"; fi
  done
  check_expect
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then write_parity "$s"; fi
  done
  sync
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then parity_facts "$s"; fi
  done
  assert_parity_signature
  render_expected "$scan" "$EXP"
  if ((HAVE_EXPECT)); then render_scan "$EXP"; fi
  if have_refusal; then source_hashes >"$EXP/source-disks.sha256"; fi
  make_zips
  printf 'unraid-fixture: built %s (%s) under %s\n' "$VARIANT" "$TIER" "$OUT"
}

verify() {
  local s scan f
  [[ -d $EXP ]] || die "$OUT has no build: build the variant first"
  check_tools
  WORK=$(mktemp -d "$OUT/work.XXXXXX")
  for s in "${SLOTS[@]}"; do existing_disk "$s"; done
  if have_refusal; then
    [[ -f $EXP/source-disks.sha256 ]] || die "$OUT recorded no source-disks.sha256, but the variant refuses disks"
    source_hashes >"$WORK/source-disks.before"
    diff -u "$EXP/source-disks.sha256" "$WORK/source-disks.before" >&2 || die "a source disk differs from the sha256 recorded when it was built"
  elif [[ -e $EXP/source-disks.sha256 ]]; then
    die "$OUT recorded source-disks.sha256, but the variant refuses no disk"
  fi
  for s in "${SLOTS[@]}"; do record_layout "$s"; done
  set_parity_window
  scan="$WORK/scan"
  mkdir -p -- "$scan"
  for s in $(fs_slots); do
    mount_fs "$s" ro
    scan_tree "$MNT/$s" "$s" "$scan/$s.files" "$scan/$s.entries"
    fs_facts "$s"
  done
  for s in "${SLOTS[@]}"; do
    if is_group_member "$s"; then member_facts "$s"; fi
    if is_boot_layout "$s"; then record_boot_layout "$s"; fi
  done
  if s=$(boot_slot); then verify_boot_pool "$s"; fi
  scan_libvirt "$scan"
  unmount_all
  check_expect
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then parity_facts "$s"; fi
  done
  assert_parity_signature
  render_expected "$scan" "$WORK/new"
  if ((HAVE_EXPECT)); then render_scan "$WORK/new"; fi
  for f in manifest.sha256 entries.tsv layout.txt; do
    diff -u "$EXP/$f" "$WORK/new/$f" >&2 || die "$f differs from what the disks hold"
  done
  if ((HAVE_EXPECT)); then
    diff -u "$EXP/scan.txt" "$WORK/new/scan.txt" >&2 || die "scan.txt differs from what the disks show"
  fi
  if [[ -e $EXP/domains.txt || -e $WORK/new/domains.txt ]]; then
    diff -u "$EXP/domains.txt" "$WORK/new/domains.txt" >&2 || die "domains.txt differs from what libvirt.img holds"
  fi
  if have_refusal; then
    source_hashes >"$WORK/source-disks.after"
    diff -u "$WORK/source-disks.before" "$WORK/source-disks.after" >&2 || die "verifying wrote to a source disk"
  fi
  verify_zips
  printf 'unraid-fixture: %s verified: %s files across %s disks match the manifest\n' "$VARIANT" \
    "$(grep -vc '^#' "$EXP/manifest.sha256")" "$(grep -c '^# disk ' "$EXP/manifest.sha256")"
}

main() {
  while (($#)); do
    case $1 in
      --tier)
        [[ $# -ge 2 ]] || die "--tier needs a value"
        TIER=$2
        shift 2
        ;;
      --option)
        [[ $# -ge 2 && -n $2 ]] || die "--option needs a value"
        OPTION=$2
        shift 2
        ;;
      --verify)
        MODE=verify
        shift
        ;;
      -*) die "unknown option $1" ;;
      *)
        [[ -z $VARIANT ]] || die "one variant at a time"
        VARIANT=$1
        shift
        ;;
    esac
  done
  [[ -n $VARIANT && -n $TIER ]] || die "usage: unraid-fixture.sh --tier l2|l3 [--option <name>] [--verify] <variant>"
  init_tier
  parse_spec "$VDIR/spec"
  ((${#SLOTS[@]} > 0)) || die "the spec declares no disks"
  check_spec
  if [[ $TIER == l2 && -n $L3_ONLY ]]; then
    die "$VARIANT cannot be built on the l2 tier ($L3_ONLY): the lab image has no OpenZFS and the lab container has no device-mapper. Build it in the L3 guest: make vm-up VARIANT=$VARIANT, then make vm-unraid-fixture VARIANT=$VARIANT"
  fi
  load_expect
  [[ $SPEC_VERSION =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && $SPEC_RELEASE =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "the spec needs unraid_version=X.Y.Z and unraid_release=YYYY-MM-DD"
  trap cleanup EXIT
  case $MODE in
    build) build ;;
    verify) verify ;;
  esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  main "$@"
fi
