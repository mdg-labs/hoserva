package disk

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// AssignedDisk is one disk the array-setup wizard gave a role (doc 03
// §3.1 steps 2-3): which list of a TopologyPlan it appears in — Parity,
// Data or Cache — is its role. A disk left "Ignore" never appears in a
// TopologyPlan at all, which is the explicit assignment doc 03 §3.1 step
// 6 requires before anything is formatted. Adopt keeps its existing
// filesystem instead of formatting it, after AdoptCheck's read-only
// verification (Q23); Filesystem is what to format with when Adopt is
// false, and the filesystem AdoptCheck verifies when it is true.
//
// WWN, Serial, WeakIdentity and ByIDName are copied from the same
// disk.Provider.List call's Disk that populated the setup wizard's
// disk-discovery step (doc 03 §3.1 step 1) — the stable identity Device's
// own /dev/sdX path is not: it can change across a reboot or a controller
// reorder. FormatAssigned re-resolves Device against this identity
// immediately before formatting, and — whenever ByIDName is known — formats
// through the /dev/disk/by-id path it names instead of the plain, transient
// Device path, so the disk that is actually formatted is the one this
// identity names at the moment the format call runs, not whichever disk
// Device happened to point at when the plan was built (doc 02 §4). Validate
// refuses a weak-identity disk assigned as parity (Q21, doc 03 §3.1 step 2,
// doc 05's migration table). A disk with no by-id link at all — WWN, Serial
// and ByIDName all empty, as every disk in the loop-device lab is (doc 06
// §3) — has nothing to bind to; FormatAssigned then trusts Device as-is,
// exactly as it always has.
type AssignedDisk struct {
	Device       string
	Filesystem   FilesystemType
	Adopt        bool
	WWN          string
	Serial       string
	WeakIdentity bool
	ByIDName     string
}

// TopologyPlan is the array-setup Topology job's payload (doc 01 §4's
// Topology job class; doc 03 §3.1): every disk explicitly assigned a
// role. It is the shape a future internal/api handler binds a wizard
// submission to, and the literal string CheckConfirmation compares
// against is doc 03 §3.1 step 6's typed-confirm control.
type TopologyPlan struct {
	Parity []AssignedDisk
	Data   []AssignedDisk
	Cache  *AssignedDisk
}

var (
	// ErrNoParityDisks and ErrTooManyParityDisks enforce Q19: 1 or 2
	// parity disks in v1, never zero and never three or more.
	ErrNoParityDisks      = errors.New("disk: at least one parity disk is required (Q19)")
	ErrTooManyParityDisks = errors.New("disk: at most two parity disks are supported in v1 (Q19)")
	// ErrNoDataDisks is refused because a parity-only pool has nothing
	// to protect.
	ErrNoDataDisks = errors.New("disk: at least one data disk is required")
	// ErrParityTooSmall enforces Q20: a parity disk must be at least as
	// large as the largest data disk, or it cannot hold that disk's
	// reconstructed contents.
	ErrParityTooSmall = errors.New("disk: a parity disk must be at least as large as the largest data disk (Q20)")
	// ErrParityNotXFS and ErrParityCannotAdopt enforce Q20: parity disks
	// are always formatted fresh as XFS, never adopted — a migrated
	// parity disk's own contents are discarded anyway.
	ErrParityNotXFS      = errors.New("disk: parity disks are always formatted XFS (Q20)")
	ErrParityCannotAdopt = errors.New("disk: a parity disk is always formatted fresh, never adopted (Q20)")
	// ErrUnsupportedFilesystem enforces Q23: only XFS, ext4 and
	// single-device btrfs are supported for a data or cache disk,
	// formatted or adopted.
	ErrUnsupportedFilesystem = errors.New("disk: unsupported filesystem")
	// ErrConfirmationMismatch is CheckConfirmation's refusal: the typed
	// confirmation doc 03 §3.1 step 6 requires did not match this exact
	// plan.
	ErrConfirmationMismatch = errors.New("disk: typed confirmation does not match this plan")
	// ErrDeviceAssignedTwice refuses a device that appears in more than
	// one of Parity/Data/Cache — FormatPlan would otherwise format it
	// twice, and mount generation would assign one filesystem to more
	// than one role.
	ErrDeviceAssignedTwice = errors.New("disk: device is assigned more than one role")
	// ErrMissingSize refuses an assigned device sizes has no entry for,
	// or reports a size of zero or less for. A missing map key reads as
	// 0 in Go, which would otherwise let maxData stay 0 and the parity-
	// size check pass for a data disk the caller's own inventory never
	// actually reported.
	ErrMissingSize = errors.New("disk: no size reported for an assigned device")
	// ErrWeakIdentityParity refuses a weak-identity disk (a USB
	// enclosure's bridge chipset can hide the real disk's WWN and serial)
	// assigned as parity — allowed as a data disk, never as parity
	// (Q21, doc 03 §3.1 step 2, doc 05's migration table).
	ErrWeakIdentityParity = errors.New("disk: a weak-identity disk cannot be assigned as parity (Q21)")
	// ErrDiskIdentityChanged is FormatAssigned's refusal when no disk at
	// all currently matches an assigned device's stable identity (WWN or
	// serial) — the disk was pulled between discovery and the format
	// call, not merely renumbered to a different /dev path: that drift
	// alone is not refused, since formatting binds to the identity's own
	// /dev/disk/by-id path (when one is known) rather than to the plan's
	// original Device string.
	ErrDiskIdentityChanged = errors.New("disk: the confirmed disk no longer matches this device path")
)

// adoptableFilesystems are the filesystems Q23 allows adopting or
// formatting a data or cache disk with.
var adoptableFilesystems = map[FilesystemType]bool{XFS: true, EXT4: true, BTRFS: true}

// Validate checks the array-setup rules doc 02 §2 and §5 and Q19/Q20/Q23
// state: 1 or 2 parity disks, each at least as large as the largest data
// disk, always XFS and never adopted; at least one data disk, each a
// filesystem Q23 supports. sizes supplies every assigned device's size
// in bytes, from the same disk.Provider.List call that populated the
// setup wizard's disk-discovery step (doc 03 §3.1 step 1).
func (p TopologyPlan) Validate(sizes map[string]int64) error {
	switch {
	case len(p.Parity) == 0:
		return ErrNoParityDisks
	case len(p.Parity) > 2:
		return ErrTooManyParityDisks
	case len(p.Data) == 0:
		return ErrNoDataDisks
	}

	seen := make(map[string]bool, len(p.Parity)+len(p.Data)+1)
	assignOnce := func(dev string) error {
		if seen[dev] {
			return fmt.Errorf("%w: %s", ErrDeviceAssignedTwice, dev)
		}
		seen[dev] = true
		return nil
	}
	for _, d := range p.Parity {
		if err := assignOnce(d.Device); err != nil {
			return err
		}
	}
	for _, d := range p.Data {
		if err := assignOnce(d.Device); err != nil {
			return err
		}
	}
	if p.Cache != nil {
		if err := assignOnce(p.Cache.Device); err != nil {
			return err
		}
	}

	requireSize := func(dev string) (int64, error) {
		s, ok := sizes[dev]
		if !ok || s <= 0 {
			return 0, fmt.Errorf("%w: %s", ErrMissingSize, dev)
		}
		return s, nil
	}

	var maxData int64
	for _, d := range p.Data {
		if !adoptableFilesystems[d.Filesystem] {
			return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, d.Filesystem, d.Device)
		}
		s, err := requireSize(d.Device)
		if err != nil {
			return err
		}
		if s > maxData {
			maxData = s
		}
	}

	for _, d := range p.Parity {
		if d.Adopt {
			return fmt.Errorf("%w: %s", ErrParityCannotAdopt, d.Device)
		}
		if d.Filesystem != XFS {
			return fmt.Errorf("%w: %s", ErrParityNotXFS, d.Device)
		}
		if d.WeakIdentity {
			return fmt.Errorf("%w: %s", ErrWeakIdentityParity, d.Device)
		}
		s, err := requireSize(d.Device)
		if err != nil {
			return err
		}
		if s < maxData {
			return fmt.Errorf("%w: %s", ErrParityTooSmall, d.Device)
		}
	}

	if p.Cache != nil {
		if !adoptableFilesystems[p.Cache.Filesystem] {
			return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, p.Cache.Filesystem, p.Cache.Device)
		}
		if _, err := requireSize(p.Cache.Device); err != nil {
			return err
		}
	}

	return nil
}

// Confirmation returns the exact string doc 03 §3.1 step 6's typed-
// confirm control must collect before the API accepts this plan: every
// device it is about to erase, sorted so the text is deterministic.
// Adopted disks are excluded — they keep their data — so confirming one
// plan can never be replayed against a different one that erases more.
func (p TopologyPlan) Confirmation() string {
	var devices []string
	for _, d := range p.Parity {
		if !d.Adopt {
			devices = append(devices, d.Device)
		}
	}
	for _, d := range p.Data {
		if !d.Adopt {
			devices = append(devices, d.Device)
		}
	}
	if p.Cache != nil && !p.Cache.Adopt {
		devices = append(devices, p.Cache.Device)
	}
	sort.Strings(devices)

	if len(devices) == 0 {
		return "ADOPT ONLY — NOTHING ERASED"
	}
	return "ERASE " + strings.Join(devices, ", ")
}

// CheckConfirmation refuses (ErrConfirmationMismatch) unless got is
// exactly p's own Confirmation() text — the typed-confirmation
// requirement a future Topology job handler enforces at the API before
// FormatPlan ever runs (doc 03 §3.1 step 6).
func (p TopologyPlan) CheckConfirmation(got string) error {
	if got != p.Confirmation() {
		return ErrConfirmationMismatch
	}
	return nil
}

// findDevice returns the AssignedDisk exactly matching dev — the
// explicit-assignment check FormatAssigned enforces, and the source of
// the stable identity it re-resolves dev against before formatting.
func (p TopologyPlan) findDevice(dev string) (AssignedDisk, bool) {
	for _, d := range p.Parity {
		if d.Device == dev {
			return d, true
		}
	}
	for _, d := range p.Data {
		if d.Device == dev {
			return d, true
		}
	}
	if p.Cache != nil && p.Cache.Device == dev {
		return *p.Cache, true
	}
	return AssignedDisk{}, false
}
