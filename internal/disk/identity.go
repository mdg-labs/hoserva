package disk

import "strings"

// byIDDir is the real system directory udev keeps the stable /dev/disk/by-id
// symlinks in (Q21). It is a fixed convention, not a configurable path like
// Lister.ByIDDir (which exists only so List's enumeration can be tested
// against a synthetic tree): the by-id path built for an actual format or
// adopt-check call must always name the same directory a running system's
// kernel resolves, regardless of where a disk was originally enumerated
// from.
const byIDDir = "/dev/disk/by-id"

// Identity is a disk's stable identity, resolved from the set of
// /dev/disk/by-id symlinks the kernel creates for it (Q21) rather than its
// transient /dev/sdX name. WWN is preferred; Serial is the fallback used
// when no wwn-* link exists. WeakIdentity is true when the only by-id
// links found are usb-* ones — a USB enclosure's bridge chipset can hide
// the real disk's WWN and serial behind its own, so a weak-identity disk
// is allowed as a data disk (matched on filesystem UUID and size) but
// refused as a parity disk (Q21). ByIDName is the exact basename of
// whichever by-id link the identity above was taken from (preferring a
// wwn-* link when one exists) — the kernel keeps that symlink pointed at
// whichever device currently carries this identity, so IdentityPath
// reconstructs it as the reference to format or adopt-check through
// instead of a transient /dev/sdX path (doc 02 §4). It is empty exactly
// when WWN and Serial both are: no by-id link at all exists for this
// disk, as every disk in the loop-device lab is (doc 06 §3).
type Identity struct {
	WWN          string
	Serial       string
	WeakIdentity bool
	ByIDName     string
	// FSUUID is the filesystem UUID belonging to this identity's own disk
	// (#388) — never set by ResolveIdentity, since a disk's physical
	// identity is independent of what it is formatted with. A caller
	// building disk.ExpectedDisk sets it to the filesystem SQLite records
	// for the slot; a caller building the "present" identity passed to
	// StorageGate.Evaluate sets it to what disk.Disk.FSUUID currently
	// reads off that disk. Left empty by a caller that predates this
	// field, or a slot that has never been formatted: evaluate only
	// compares it when both sides have one, so an empty value on either
	// side is never reported as a mismatch.
	FSUUID string
}

// IdentityPath returns the /dev/disk/by-id path i's ByIDName names, or ""
// when ByIDName is empty — nothing to bind to, and a caller should use the
// plain device path as-is (doc 02 §4).
func (i Identity) IdentityPath() string {
	if i.ByIDName == "" {
		return ""
	}
	return byIDDir + "/" + i.ByIDName
}

// ResolveIdentity derives a disk's Identity from the basenames of every
// /dev/disk/by-id entry known to resolve to it. Partition entries (a
// "-partN" suffix) name a partition's identity, not the disk's, and must
// already be filtered out by the caller.
//
// A virtio- link (libvirt/udev's by-id name for a virtio-blk device,
// e.g. an L3 VM's array disk, doc 06 §4) is treated the same as a
// host-attached ata-/scsi-/nvme- disk's: real hardware never presents
// this prefix (libvirt does not support a <wwn> for a virtio-blk disk),
// so recognizing it cannot affect real-hardware identity resolution.
// Unlike those prefixes, a virtio- link carries no vendor_serial
// underscore convention to split on — the whole remainder after the
// prefix is the disk's own configured serial (doc 06 §6).
func ResolveIdentity(byIDNames []string) Identity {
	var wwn, wwnName string
	var hostSerial, hostName string
	var usbSerial, usbName string

	for _, name := range byIDNames {
		switch {
		case strings.HasPrefix(name, "wwn-"):
			if wwn == "" {
				wwn = strings.TrimPrefix(name, "wwn-")
				wwnName = name
			}
		case strings.HasPrefix(name, "ata-"), strings.HasPrefix(name, "scsi-"), strings.HasPrefix(name, "nvme-"):
			if hostSerial == "" {
				hostSerial = lastSegment(name)
				hostName = name
			}
		case strings.HasPrefix(name, "virtio-"):
			if hostSerial == "" {
				hostSerial = strings.TrimPrefix(name, "virtio-")
				hostName = name
			}
		case strings.HasPrefix(name, "usb-"):
			if usbSerial == "" {
				usbSerial = lastSegment(name)
				usbName = name
			}
		}
	}

	if wwn != "" {
		return Identity{WWN: wwn, Serial: hostSerial, ByIDName: wwnName}
	}
	if hostSerial != "" {
		return Identity{Serial: hostSerial, ByIDName: hostName}
	}
	if usbSerial != "" {
		return Identity{Serial: usbSerial, WeakIdentity: true, ByIDName: usbName}
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

// FSUUIDMismatch reports whether want and got were both positively read
// and differ (#388) — the one shared definition of "wrong filesystem"
// disk.StorageGate.evaluate, GetPool's own per-slot state
// (internal/api/phase1_handler.go's wrongFilesystem) and
// ConfirmReplacementTargetAbsent's own relaxation (internal/job) all call,
// rather than each keeping its own copy of the rule. It is deliberately
// fail-closed: an empty want (a slot never formatted) or an empty got (a
// present disk this build could not read a filesystem UUID for — udev's
// own cache not yet settled for an otherwise perfectly healthy disk is a
// real, ordinary case, not just a hypothetical one) is never a mismatch,
// since neither side is a positive read to compare — a caller that reads
// this as "safe to treat as gone" must have first confirmed both sides
// are genuinely known.
func FSUUIDMismatch(want, got string) bool {
	return want != "" && got != "" && want != got
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
