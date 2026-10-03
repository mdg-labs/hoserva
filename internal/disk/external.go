package disk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ExternalMountRoot is where disks outside the array mount (Q72, doc 01
// §6, doc 02 §4): /mnt/disks/<label>, never a mergerfs branch or a
// SnapRAID data/parity path.
const ExternalMountRoot = "/mnt/disks"

var (
	// ErrInvalidExternalLabel is ValidateExternalLabel's refusal: the
	// label becomes a path segment under ExternalMountRoot.
	ErrInvalidExternalLabel = errors.New("disk: invalid external disk label")
	// ErrExternalInArray is MountExternal / FormatExternal's refusal of
	// a device that already holds a pool or parity role.
	ErrExternalInArray = errors.New("disk: disk is already in the array")
	// ErrExternalMounted is FormatExternal's refusal to erase a disk
	// that is still mounted.
	ErrExternalMounted = errors.New("disk: eject the disk before formatting")
	// ErrUnraidStick is the external-disk path's refusal of the Unraid USB
	// stick: it is the user's rollback (doc 05 §4 step 11, §5) and is only
	// ever read, through ReadOnlyMounter.
	ErrUnraidStick = errors.New("disk: this is the Unraid USB stick, which Hoserva only reads and never mounts read-write, formats or writes")
)

// The Unraid USB stick is a FAT filesystem labelled UNRAID (doc 05 §3).
const (
	UnraidStickFilesystem = "vfat"
	UnraidStickLabel      = "UNRAID"
)

// IsUnraidStick reports whether d is the Unraid USB stick by its filesystem
// and label, which is how udev reports it before anything is mounted.
func IsUnraidStick(d Disk) bool {
	return d.Filesystem == UnraidStickFilesystem && strings.EqualFold(d.Label, UnraidStickLabel)
}

// RefuseUnraidStick returns ErrUnraidStick when dev, or a disk holding the
// filesystem uuid, is the Unraid USB stick in disks. A uuid match catches a
// record made for the stick whose device name has since changed.
func RefuseUnraidStick(disks []Disk, dev, uuid string) error {
	whole := WholeDiskDevice(dev)
	for _, d := range disks {
		if !IsUnraidStick(d) {
			continue
		}
		if d.Device == dev || d.Device == whole || (uuid != "" && d.FSUUID != "" && strings.EqualFold(d.FSUUID, uuid)) {
			return fmt.Errorf("%s: %w", dev, ErrUnraidStick)
		}
	}
	return nil
}

// externalLabelPattern is the same shape pool.ValidateShareName uses: a
// label is a single path segment under /mnt/disks/, so it cannot contain
// a separator or "..".
var externalLabelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ValidateExternalLabel refuses a label that could make /mnt/disks/<label>
// something other than a single directory under ExternalMountRoot.
func ValidateExternalLabel(label string) error {
	if !externalLabelPattern.MatchString(label) {
		return fmt.Errorf("%w: %q", ErrInvalidExternalLabel, label)
	}
	return nil
}

// ExternalMountPoint returns /mnt/disks/<label> after validating label.
func ExternalMountPoint(label string) (string, error) {
	if err := ValidateExternalLabel(label); err != nil {
		return "", err
	}
	where := filepath.Join(ExternalMountRoot, label)
	if filepath.Dir(where) != ExternalMountRoot {
		return "", fmt.Errorf("%w: %q", ErrInvalidExternalLabel, label)
	}
	return where, nil
}

// ExternalMountUnit builds the MountUnit for an external disk: mounted by
// filesystem UUID at /mnt/disks/<label> (Q21, Q72).
func ExternalMountUnit(label, uuid string, fs FilesystemType) (MountUnit, error) {
	where, err := ExternalMountPoint(label)
	if err != nil {
		return MountUnit{}, err
	}
	if uuid == "" {
		return MountUnit{}, fmt.Errorf("disk: no filesystem UUID for external disk %q", label)
	}
	return MountUnit{
		Where:       where,
		UUID:        uuid,
		Filesystem:  fs,
		Description: fmt.Sprintf("Hoserva external disk %s", label),
	}, nil
}

// ExternalFormatPlan is the one-disk TopologyPlan FormatExternal checks
// confirmation against: the same "ERASE /dev/sdX" string array setup uses
// (doc 03 §3.1 step 6), never a second confirmation shape.
func ExternalFormatPlan(d AssignedDisk) TopologyPlan {
	return TopologyPlan{Data: []AssignedDisk{d}}
}

// FormatExternal formats d after the same typed confirmation FormatPlan
// uses. It does not call TopologyPlan.Validate — that rule set is for an
// array (parity, data counts) — and never writes the disk into a pool or
// parity role. The boot device is refused by FormatAssigned.
func FormatExternal(ctx context.Context, p Provider, r Runner, d AssignedDisk, confirmation string) error {
	plan := ExternalFormatPlan(d)
	if err := plan.CheckConfirmation(confirmation); err != nil {
		return err
	}
	if err := CheckFormatTargets(plan); err != nil {
		return err
	}
	if !adoptableFilesystems[d.Filesystem] {
		return fmt.Errorf("%w: %s on %s", ErrUnsupportedFilesystem, d.Filesystem, d.Device)
	}
	return FormatAssigned(ctx, p, plan, d.Device, d.Filesystem)
}

// MountExternal mounts unit by filesystem UUID. Nothing else mounts an
// external disk — there is no udev auto-mount (Q72).
func MountExternal(ctx context.Context, m UnitMounter, unit MountUnit) error {
	return m.Mount(ctx, unit)
}

// EjectExternal unmounts unit, then spins the device down. Unmount runs
// first so spindown never races a still-mounted filesystem.
func EjectExternal(ctx context.Context, m UnitMounter, p Provider, unit MountUnit, device string) error {
	if err := m.Unmount(ctx, unit); err != nil {
		return err
	}
	if err := p.Spindown(ctx, device); err != nil {
		return fmt.Errorf("disk: spinning down %s after unmount: %w", device, err)
	}
	return nil
}

// RefuseBootDevice returns ErrBootDevice when dev is the boot disk
// according to a fresh p.List. Used for mount, eject, format and
// assign-as-external so the boot device is never offered.
func RefuseBootDevice(ctx context.Context, p Provider, dev string) error {
	disks, err := p.List(ctx)
	if err != nil {
		return err
	}
	whole := WholeDiskDevice(dev)
	for _, d := range disks {
		if d.Boot && d.Device == whole {
			return fmt.Errorf("%s: %w", dev, ErrBootDevice)
		}
	}
	return nil
}

// IsExternalMountpoint reports whether path is under ExternalMountRoot
// (the stable bind-mount source / backup destination Q72 names).
func IsExternalMountpoint(path string) bool {
	clean := filepath.Clean(path)
	return clean == ExternalMountRoot || strings.HasPrefix(clean, ExternalMountRoot+"/")
}

// IsMountpoint reports whether path is currently a mount point.
func IsMountpoint(path string) (bool, error) {
	return isMountpoint(path)
}

// LookupDisk returns the inventory entry for dev, or ErrDiskNotFound.
func LookupDisk(disks []Disk, dev string) (Disk, error) {
	for _, d := range disks {
		if d.Device == dev {
			return d, nil
		}
	}
	return Disk{}, fmt.Errorf("%w: %s", ErrDiskNotFound, dev)
}
