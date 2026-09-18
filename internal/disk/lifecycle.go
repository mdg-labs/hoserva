package disk

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
)

// DiskAddition is one disk being brought into an already-running array
// (doc 02 §4 "Adding a disk") or replacing one at an existing mountpoint
// (doc 02 §4 "Replacing a failed disk") — the same identity-and-format
// shape array setup's own AssignedDisk carries, for a single disk rather
// than a whole TopologyPlan: array setup assigns every disk's role at
// once, but a disk added or replaced afterwards is confirmed and formatted
// on its own, against the array's current state rather than a plan built
// before any of it existed.
type DiskAddition struct {
	Device       string
	Filesystem   FilesystemType
	Adopt        bool
	WWN          string
	Serial       string
	WeakIdentity bool
	ByIDName     string
}

// FormatForAddition formats or adopts a's own device (doc 02 §4 "Adding a
// disk" step 3, "Replacing a failed disk" step 3): an adopted disk is only
// ever verified read-only (AdoptCheck, Q23), never formatted; a disk being
// added fresh is formatted directly. Immediately before either, it
// re-resolves a's own stable identity (WWN or serial) through a fresh
// p.List call, refusing (ErrDiskIdentityChanged) if no disk at all
// currently matches it — the disk pulled between confirmation and this
// call. When a disk does still match, the format or adopt-check runs
// against resolveFormatTarget's own result rather than a.Device directly:
// the identity's /dev/disk/by-id path when one is known (a.ByIDName), so
// the actual mkfs or read-only-check invocation binds to whichever
// physical disk the kernel currently resolves that symlink to — the same
// drift FormatAssigned guards against for array setup (doc 02 §4): the
// device path a caller confirmed this addition against is not guaranteed
// to still be the same physical disk by the time formatting actually
// runs. A disk with no by-id link at all (WWN and Serial both empty, as
// every disk in the loop-device lab is, doc 06 §3) has nothing to bind
// to, and a.Device is trusted as given.
func FormatForAddition(ctx context.Context, p Provider, r Runner, a DiskAddition) error {
	target, err := resolveFormatTarget(ctx, p, a.Device, a.WWN, a.Serial, a.ByIDName)
	if err != nil {
		return err
	}
	if a.Adopt {
		return AdoptCheck(ctx, r, target, a.Filesystem)
	}
	return p.Format(ctx, target, a.Filesystem)
}

// DataDiskMountUnit builds the MountUnit for a data disk at where — the
// same shape MountPlan assigns each data disk at array setup (doc 01 §6)
// — for a single disk added after setup or a failed disk's replacement,
// which is mounted back at its own already-assigned mountpoint rather
// than a newly computed one.
func DataDiskMountUnit(where, uuid string, fs FilesystemType) MountUnit {
	return MountUnit{
		Where:       where,
		UUID:        uuid,
		Filesystem:  fs,
		Description: fmt.Sprintf("Hoserva data disk at %s", where),
	}
}

// dataMountpointPattern matches the standard data-disk mountpoint shape
// (doc 01 §6): "/mnt/disk" followed by its number, nothing else.
var dataMountpointPattern = regexp.MustCompile(`^/mnt/disk([0-9]+)$`)

// NextDataMountpoint returns the first "/mnt/diskN" (N >= 1) not already
// present in used (doc 02 §4 "Adding a disk" step 4) — used is normally
// every data disk currently in the array, so a disk added after an
// earlier one was removed reuses the first free slot rather than always
// growing past the highest N ever assigned. Any entry in used that
// doesn't match the standard "/mnt/diskN" shape is ignored: it names
// something else entirely (a cache or parity mountpoint, say) and can
// never collide with a data mountpoint this returns.
func NextDataMountpoint(used []string) string {
	taken := make(map[int]bool, len(used))
	for _, u := range used {
		m := dataMountpointPattern.FindStringSubmatch(u)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		taken[n] = true
	}
	for n := 1; ; n++ {
		if !taken[n] {
			return fmt.Sprintf("/mnt/disk%d", n)
		}
	}
}
