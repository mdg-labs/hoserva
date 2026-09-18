package disk

import (
	"context"
	"errors"
	"fmt"
)

// ErrDiskNotAssigned is FormatAssigned's refusal: dev is not one of
// plan's own devices (doc 03 §3.1 step 6's "explicit list of which
// disks will be erased"). The check runs before Provider.Format is ever
// called, so a wrong device path is refused here rather than trusted to
// Provider alone.
var ErrDiskNotAssigned = errors.New("disk: not part of this array-setup plan")

// FormatAssigned formats dev through p, refusing (ErrDiskNotAssigned)
// unless dev is exactly one of plan's own assigned devices. This is the
// guard every path that can format a disk goes through — FormatPlan
// below, and the future Topology job handler — so a bug that names the
// wrong device is refused before it reaches a real mkfs invocation.
func FormatAssigned(ctx context.Context, p Provider, plan TopologyPlan, dev string, fs FilesystemType) error {
	if !plan.hasDevice(dev) {
		return fmt.Errorf("%w: %s", ErrDiskNotAssigned, dev)
	}
	return p.Format(ctx, dev, fs)
}

// FormatPlan formats or adopts every disk in plan (doc 02 §4 "Adding a
// disk" steps 2-3, doc 03 §3.1 step 6): it checks confirmation against
// plan's own Confirmation() text, validates the plan against sizes, then
// formats every non-adopted disk through FormatAssigned and runs
// AdoptCheck's read-only verification on every adopted one (Q23). It
// never touches a device outside plan's own Parity/Data/Cache lists.
func FormatPlan(ctx context.Context, p Provider, r Runner, plan TopologyPlan, sizes map[string]int64, confirmation string) error {
	if err := plan.CheckConfirmation(confirmation); err != nil {
		return err
	}
	if err := plan.Validate(sizes); err != nil {
		return err
	}

	for _, d := range plan.assignedDisks() {
		if d.Adopt {
			if err := AdoptCheck(ctx, r, d.Device, d.Filesystem); err != nil {
				return err
			}
			continue
		}
		if err := FormatAssigned(ctx, p, plan, d.Device, d.Filesystem); err != nil {
			return err
		}
	}
	return nil
}

// assignedDisks returns every disk in p, in the order Parity, Data,
// Cache.
func (p TopologyPlan) assignedDisks() []AssignedDisk {
	all := make([]AssignedDisk, 0, len(p.Parity)+len(p.Data)+1)
	all = append(all, p.Parity...)
	all = append(all, p.Data...)
	if p.Cache != nil {
		all = append(all, *p.Cache)
	}
	return all
}

// formatCommand returns the argv Format execs for fs — one exec, never
// a shell (CLAUDE.md) — matching each tool's own manual for "build a
// fresh filesystem, overwriting any existing one".
func formatCommand(dev string, fs FilesystemType) ([]string, error) {
	switch fs {
	case XFS:
		return []string{"mkfs.xfs", "-f", dev}, nil
	case EXT4:
		return []string{"mkfs.ext4", "-F", dev}, nil
	case BTRFS:
		return []string{"mkfs.btrfs", "-f", dev}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFilesystem, fs)
	}
}

// adoptCheckCommand returns the argv AdoptCheck execs for fs — the
// read-only check each tool's own manual documents for "verify, never
// repair": xfs_repair -n, e2fsck -n, btrfs check --readonly (Q23). None
// of these write to dev.
func adoptCheckCommand(dev string, fs FilesystemType) ([]string, error) {
	switch fs {
	case XFS:
		return []string{"xfs_repair", "-n", dev}, nil
	case EXT4:
		return []string{"e2fsck", "-n", dev}, nil
	case BTRFS:
		return []string{"btrfs", "check", "--readonly", dev}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFilesystem, fs)
	}
}

// AdoptCheck runs the read-only filesystem check Q23 requires before
// adopting dev's existing filesystem instead of formatting it. A disk
// that fails its check is refused, never adopted.
func AdoptCheck(ctx context.Context, r Runner, dev string, fs FilesystemType) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	argv, err := adoptCheckCommand(dev, fs)
	if err != nil {
		return err
	}
	if _, err := r.Run(ctx, argv[0], argv[1:]...); err != nil {
		return fmt.Errorf("disk: %s failed its read-only %s check, refusing to adopt: %w", dev, fs, err)
	}
	return nil
}
