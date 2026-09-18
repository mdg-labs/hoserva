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
type AssignedDisk struct {
	Device     string
	Filesystem FilesystemType
	Adopt      bool
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

	var maxData int64
	for _, d := range p.Data {
		if !adoptableFilesystems[d.Filesystem] {
			return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, d.Filesystem, d.Device)
		}
		if s := sizes[d.Device]; s > maxData {
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
		if sizes[d.Device] < maxData {
			return fmt.Errorf("%w: %s", ErrParityTooSmall, d.Device)
		}
	}

	if p.Cache != nil && !adoptableFilesystems[p.Cache.Filesystem] {
		return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, p.Cache.Filesystem, p.Cache.Device)
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

// hasDevice reports whether dev is exactly one of p's own assigned
// devices — the explicit-assignment check FormatAssigned enforces.
func (p TopologyPlan) hasDevice(dev string) bool {
	for _, d := range p.Parity {
		if d.Device == dev {
			return true
		}
	}
	for _, d := range p.Data {
		if d.Device == dev {
			return true
		}
	}
	return p.Cache != nil && p.Cache.Device == dev
}
