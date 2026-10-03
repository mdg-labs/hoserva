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
# seed (data), flash/ (the authored flash tree laid over common/flash).
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
LV_MNT=""
LV_LOOP=""
LV_LOCATION=""
SLOTS=()
APPLEDOUBLE=()
declare -A D_KIND D_FS D_SIZE D_TARGET D_POOL D_XFS
declare -A PART WHOLE
declare -A FACT_LABEL FACT_FEATURES
declare -A PARITY_SHA PARITY_SIG PARITY_KIND
LOOPS=()
MOUNTED=()
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
        case $val in parity | data | pool) ;; *) die "spec: disk '$slot': bad kind '$val'" ;; esac
        D_KIND[$slot]=$val
        ;;
      fs)
        case $val in xfs | btrfs | none) ;; *) die "spec: disk '$slot': bad fs '$val'" ;; esac
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
      *) die "spec: disk '$slot': unknown key '$key'" ;;
    esac
  done
  [[ -n ${D_KIND[$slot]:-} && -n ${D_FS[$slot]:-} && -n ${D_SIZE[$slot]:-} && -n ${D_TARGET[$slot]:-} ]] \
    || die "spec: disk '$slot' needs kind, fs, size and target"
  case ${D_KIND[$slot]} in
    parity) [[ $slot == parity || $slot == parity2 ]] || die "spec: parity disk must be 'parity' or 'parity2'" ;;
    data) [[ $slot == disk* && ${D_FS[$slot]} != none ]] || die "spec: data disk '$slot' needs a diskN name and a filesystem" ;;
    pool) [[ -n ${D_POOL[$slot]:-} && ${D_FS[$slot]} != none ]] || die "spec: pool disk '$slot' needs pool= and a filesystem" ;;
  esac
  [[ ${D_KIND[$slot]} != parity || ${D_FS[$slot]} == none ]] || die "spec: parity disk '$slot' has no filesystem"
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
        if [[ ${D_KIND[$s]} != pool ]]; then continue; fi
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

# Sets PART[slot] to the partition's block device; for l2 that is a loop
# device over the partition's own byte range of the image.
attach_part() {  # slot ro|rw
  local slot=$1 mode=$2 whole dev opts=()
  whole=${WHOLE[$slot]}
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
      partition_script "$bytes" | sfdisk -q "$whole" >/dev/null
      udevadm settle
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

format_part() {  # slot
  local slot=$1 dev mopts i
  dev=${PART[$slot]}
  assert_own_part "$slot"
  case ${D_FS[$slot]} in
    xfs)
      mopts=crc=1,finobt=1,reflink=1,bigtime=1,inobtcount=1,rmapbt=0
      i=sparse=1,nrext64=0
      if [[ ${D_XFS[$slot]:-default} == rmapbt-nrext64 ]]; then
        mopts=${mopts%rmapbt=0}rmapbt=1
        i=${i%nrext64=0}nrext64=1
      fi
      mkfs.xfs -q -f -K -m "$mopts" -i "$i" "$dev"
      ;;
    btrfs) mkfs.btrfs -q -f "$dev" ;;
  esac
}

mount_fs() {  # slot rw|ro
  local slot=$1 mode=$2 m dev
  m="$MNT/$slot"
  dev=${PART[$slot]}
  mkdir -p -- "$m"
  case ${D_FS[$slot]}:$mode in
    xfs:rw) mount -t xfs -o noatime,nouuid,inode64,logbufs=8,logbsize=32k,noquota "$dev" "$m" ;;
    xfs:ro) mount -t xfs -o ro,norecovery,nouuid,noatime,inode64,noquota "$dev" "$m" ;;
    btrfs:rw) mount -t btrfs -o noatime "$dev" "$m" ;;
    btrfs:ro) mount -t btrfs -o ro,rescue=nologreplay,noatime "$dev" "$m" ;;
  esac
  MOUNTED+=("$m")
}

unmount_all() {
  local i m
  for ((i = ${#MOUNTED[@]} - 1; i >= 0; i--)); do
    m=${MOUNTED[$i]}
    if mountpoint -q "$m" 2>/dev/null; then umount "$m" || die "cannot unmount $m"; fi
  done
  MOUNTED=()
}

# shellcheck disable=SC2317 # called from the EXIT trap
cleanup() {
  local rc=$? m dev img
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

# Records what is true of a mounted data or pool filesystem.
fs_facts() {  # slot
  local slot=$1 dev type label info f v feats=""
  dev=${PART[$slot]}
  type=$(probe_value TYPE "$dev")
  label=$(probe_value LABEL "$dev")
  [[ $type == "${D_FS[$slot]}" ]] || die "$slot: the filesystem is '$type', not ${D_FS[$slot]}"
  [[ -z $label ]] || die "$slot: the filesystem has a label ('$label'); Unraid's carry none"
  FACT_LABEL[$slot]=-
  if [[ $type == xfs ]]; then
    info=$(xfs_info "$MNT/$slot") || die "xfs_info $slot failed"
    for f in "${XFS_FEATURES[@]}"; do
      v=$(grep -oE "(^|[ ,])$f=[0-9]" <<<"$info" | head -n 1 | sed 's/.*=//') || true
      [[ -n $v ]] || die "$slot: xfs_info does not report $f"
      feats+=" $f=$v"
    done
    FACT_FEATURES[$slot]=${feats# }
  else
    FACT_FEATURES[$slot]=""
  fi
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
    if [[ ${D_KIND[$s]} == pool ]]; then continue; fi
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
    for s in "${SLOTS[@]}"; do
      if [[ ${D_KIND[$s]} == parity ]]; then continue; fi
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

declare -A LAYOUT_SCHEME LAYOUT_START LAYOUT_SECTORS LAYOUT_TYPE

record_layout() {  # slot
  local slot=$1
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

# ---------------------------------------------------------------- flash

disk_size_kib() { echo $((D_SIZE[$1] / 1024)); }

gen_disk_cfg() {
  local s n
  printf 'startArray="yes"\nspindownDelay="30"\nspinupGroups="no"\ndefaultFsType="xfs"\nqueueDepth="auto"\nmd_write_method="auto"\n'
  for s in "${SLOTS[@]}"; do
    case ${D_KIND[$s]} in
      parity) n=$(slot_index "$s"); printf 'diskIdSlot.%s="-"\ndiskSpindownDelay.%s="-1"\n' "$n" "$n" ;;
      data) n=$(slot_index "$s"); printf 'diskIdSlot.%s="-"\ndiskFsType.%s="%s"\ndiskSpindownDelay.%s="-1"\ndiskSpinupGroup.%s=""\n' "$n" "$n" "${D_FS[$s]}" "$n" "$n" ;;
    esac
  done
}

# Unraid's /var/local/emhttp/disks.ini, the slot to disk identity table that
# the prepare script copies into the flash (Q89).
gen_disks_ini() {
  local s letters=(b c d e f g h i j k l m n o p) i=0 type
  for s in "${SLOTS[@]}"; do
    case ${D_KIND[$s]} in
      parity) type=Parity ;;
      data) type=Data ;;
      pool) type=Cache ;;
    esac
    printf '["%s"]\nidx="%s"\nname="%s"\ndevice="sd%s"\nid="FIXTURE_%s"\nsize="%s"\nstatus="DISK_OK"\ntype="%s"\nfsType="%s"\nrotational="1"\nspindownDelay="-1"\nidSb="FIXTURE_%s"\nsizeSb="%s"\n' \
      "$s" "$(slot_index "$s")" "$s" "${letters[$i]}" "$(serial_of "$s")" "$(disk_size_kib "$s")" "$type" "${D_FS[$s]/none/}" "$(serial_of "$s")" "$(disk_size_kib "$s")"
    i=$((i + 1))
  done
}

appledouble_header() {
  printf '\000\005\026\007\000\002\000\000Mac OS X        \000\002\000\000\000\011\000\000\000\062\000\000\000\030\000\000\000\002\000\000\000\102\000\000\000\020'
  head -c 32 /dev/zero
}

build_flash() {
  local s p rel f serial
  rm -rf -- "$FLASH"
  mkdir -p -- "$FLASH"
  cp -a --no-preserve=ownership -- "$FIXTURES/common/flash/." "$FLASH/"
  if [[ -d $VDIR/flash ]]; then cp -a --no-preserve=ownership -- "$VDIR/flash/." "$FLASH/"; fi
  if [[ -n $OPTION && -d $VDIR/options/$OPTION/flash ]]; then cp -a --no-preserve=ownership -- "$VDIR/options/$OPTION/flash/." "$FLASH/"; fi
  mkdir -p -- "$FLASH/config/hoserva"
  printf '# Version %s %s\nAuthored for Hoserva migration fixtures; not a release note.\n' "$SPEC_VERSION" "$SPEC_RELEASE" >"$FLASH/changes.txt"
  gen_disk_cfg >"$FLASH/config/disk.cfg"
  gen_disks_ini >"$FLASH/config/hoserva/disks.ini"
  head -c 4096 /dev/urandom >"$FLASH/config/super.dat"
  head -c 4096 /dev/urandom >"$FLASH/config/super.old"
  for f in bzimage bzroot bzfirmware bzmodules; do printf 'placeholder, not a kernel image\n' >"$FLASH/$f"; done
  mkdir -p -- "$FLASH/EFI/boot" "$FLASH/previous"
  printf 'placeholder, not an EFI binary\n' >"$FLASH/EFI/boot/bootx64.efi"
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
  local flash=$1 at at_epoch newest=0 m f loc
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
    for expect in config/disk.cfg changes.txt syslinux/syslinux.cfg config/plugins/dockerMan/templates-user/my-notes.xml \
      config/plugins/dockerMan/templates-user/._my-notes.xml .git/HEAD; do
      grep -qxF -- "$expect" <<<"$list" || die "$z.zip has no $expect"
    done
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
  local t
  for t in sfdisk sgdisk mkfs.xfs mkfs.btrfs xfs_info blkid numfmt perl setfattr getfattr zip unzip sha256sum find losetup; do
    command -v "$t" >/dev/null 2>&1 || die "$t is not installed"
  done
}

build() {
  local s scan
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
  for s in "${SLOTS[@]}"; do record_layout "$s"; done
  set_parity_window
  for s in "${SLOTS[@]}"; do
    [[ ${D_KIND[$s]} == parity ]] && continue
    format_part "$s"
    mount_fs "$s" rw
  done
  build_flash
  DEFERRED_MODES=()
  seed_run "$VDIR/seed"
  if [[ -n $OPTION ]]; then
    [[ -f $VDIR/options/$OPTION/seed ]] || die "option '$OPTION' has no seed"
    seed_run "$VDIR/options/$OPTION/seed"
  fi
  scan="$WORK/scan"
  mkdir -p -- "$scan"
  for s in "${SLOTS[@]}"; do
    [[ ${D_KIND[$s]} == parity ]] && continue
    normalize_tree "$MNT/$s"
  done
  apply_deferred_modes
  sync
  for s in "${SLOTS[@]}"; do
    [[ ${D_KIND[$s]} == parity ]] && continue
    scan_tree "$MNT/$s" "$s" "$scan/$s.files" "$scan/$s.entries"
    fs_facts "$s"
  done
  scan_libvirt "$scan"
  unmount_all
  sync
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then write_parity "$s"; fi
  done
  sync
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then parity_facts "$s"; fi
  done
  assert_parity_signature
  render_expected "$scan" "$EXP"
  make_zips
  printf 'unraid-fixture: built %s (%s) under %s\n' "$VARIANT" "$TIER" "$OUT"
}

verify() {
  local s scan
  [[ -d $EXP ]] || die "$OUT has no build: build the variant first"
  check_tools
  WORK=$(mktemp -d "$OUT/work.XXXXXX")
  for s in "${SLOTS[@]}"; do existing_disk "$s"; done
  for s in "${SLOTS[@]}"; do record_layout "$s"; done
  set_parity_window
  scan="$WORK/scan"
  mkdir -p -- "$scan"
  for s in "${SLOTS[@]}"; do
    [[ ${D_KIND[$s]} == parity ]] && continue
    mount_fs "$s" ro
    scan_tree "$MNT/$s" "$s" "$scan/$s.files" "$scan/$s.entries"
    fs_facts "$s"
  done
  scan_libvirt "$scan"
  unmount_all
  for s in "${SLOTS[@]}"; do
    if [[ ${D_KIND[$s]} == parity ]]; then parity_facts "$s"; fi
  done
  assert_parity_signature
  render_expected "$scan" "$WORK/new"
  local f
  for f in manifest.sha256 entries.tsv layout.txt; do
    diff -u "$EXP/$f" "$WORK/new/$f" >&2 || die "$f differs from what the disks hold"
  done
  if [[ -e $EXP/domains.txt || -e $WORK/new/domains.txt ]]; then
    diff -u "$EXP/domains.txt" "$WORK/new/domains.txt" >&2 || die "domains.txt differs from what libvirt.img holds"
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
