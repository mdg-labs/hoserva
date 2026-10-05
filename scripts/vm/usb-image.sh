#!/usr/bin/env bash
# Attaches a disk image to this lab's own VM as a read-only USB disk, and
# detaches it again (doc 06 §5, issue #80): the Unraid USB stick of a fixture
# for `hoserva migrate scan --flash-device`. Under qemu:///session only, on
# this lab's own domain (vm_assert_own_domain), live: the guest sees a hot-plugged
# USB mass-storage device. The image is opened read-only by QEMU, so nothing
# the guest does can change it; the caller still compares its sha256.
#
#   usb-image.sh attach <image>   attach <image> as USB disk 'hoserva-stick'
#   usb-image.sh detach           detach it
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/vm/lib.sh
source "$script_dir/lib.sh"

vm_require_id
vm_assert_own_domain "$VM_DOMAIN"
vm_domain_running "$VM_DOMAIN" || die "domain '$VM_DOMAIN' is not running"

usb_target=sdz
usb_xml="$VM_STATE_DIR/usb-image.xml"

action=${1:-}
case "$action" in
  attach)
    image=${2:-}
    [[ -n "$image" && -f "$image" ]] || die "usage: usb-image.sh attach <image> (a file)"
    [[ "$image" == /* ]] || image="$PWD/$image"
    cat >"$usb_xml" <<EOF
<disk type='file' device='disk'>
  <driver name='qemu' type='raw'/>
  <source file='$(vm_xml_attr_escape "$image")'/>
  <target dev='$usb_target' bus='usb'/>
  <serial>hoserva-stick</serial>
  <readonly/>
</disk>
EOF
    virsh -c "$VM_CONNECT" attach-device "$VM_DOMAIN" "$usb_xml" --live >/dev/null
    echo "usb-image[$HOSERVA_LAB_ID]: attached $image read-only as USB disk $usb_target"
    ;;
  detach)
    [[ -f "$usb_xml" ]] || die "no USB image was attached by this lab"
    virsh -c "$VM_CONNECT" detach-device "$VM_DOMAIN" "$usb_xml" --live >/dev/null
    echo "usb-image[$HOSERVA_LAB_ID]: detached USB disk $usb_target"
    ;;
  *) die "usage: usb-image.sh attach <image> | detach" ;;
esac
