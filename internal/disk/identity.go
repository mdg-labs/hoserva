package disk

import "strings"

// Identity is a disk's stable identity, resolved from the set of
// /dev/disk/by-id symlinks the kernel creates for it (Q21) rather than its
// transient /dev/sdX name. WWN is preferred; Serial is the fallback used
// when no wwn-* link exists. WeakIdentity is true when the only by-id
// links found are usb-* ones — a USB enclosure's bridge chipset can hide
// the real disk's WWN and serial behind its own, so a weak-identity disk
// is allowed as a data disk (matched on filesystem UUID and size) but
// refused as a parity disk (Q21).
type Identity struct {
	WWN          string
	Serial       string
	WeakIdentity bool
}

// ResolveIdentity derives a disk's Identity from the basenames of every
// /dev/disk/by-id entry known to resolve to it. Partition entries (a
// "-partN" suffix) name a partition's identity, not the disk's, and must
// already be filtered out by the caller.
func ResolveIdentity(byIDNames []string) Identity {
	var wwn, hostSerial, usbSerial string

	for _, name := range byIDNames {
		switch {
		case strings.HasPrefix(name, "wwn-"):
			if wwn == "" {
				wwn = strings.TrimPrefix(name, "wwn-")
			}
		case strings.HasPrefix(name, "ata-"), strings.HasPrefix(name, "scsi-"), strings.HasPrefix(name, "nvme-"):
			if hostSerial == "" {
				hostSerial = lastSegment(name)
			}
		case strings.HasPrefix(name, "usb-"):
			if usbSerial == "" {
				usbSerial = lastSegment(name)
			}
		}
	}

	if wwn != "" {
		return Identity{WWN: wwn, Serial: hostSerial}
	}
	if hostSerial != "" {
		return Identity{Serial: hostSerial}
	}
	if usbSerial != "" {
		return Identity{Serial: usbSerial, WeakIdentity: true}
	}
	return Identity{WeakIdentity: true}
}

// Matches reports whether i and other identify the same physical disk
// (Q21): by WWN when both have one, else by serial. A weak-identity disk
// (a USB enclosure hiding its real WWN/serial) is only ever matched by
// serial, since that is the strongest signal it has.
func (i Identity) Matches(other Identity) bool {
	if i.WWN != "" && other.WWN != "" {
		return i.WWN == other.WWN
	}
	if i.Serial != "" && other.Serial != "" {
		return i.Serial == other.Serial
	}
	return false
}

// lastSegment returns the token after a by-id link name's final
// underscore, which is where the kernel's own by-id udev rules place the
// serial — for example "ata-WDC_WD80EFZX-68UW8N0_VGH0A1B2" or
// "usb-WD_easystore_25FB_575836314141304A4A3236-0:0".
func lastSegment(name string) string {
	name, _, _ = strings.Cut(name, "-0:0") // strip a trailing USB LUN suffix, if present
	idx := strings.LastIndex(name, "_")
	if idx == -1 {
		return name
	}
	return name[idx+1:]
}
