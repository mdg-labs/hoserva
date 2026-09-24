package disk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrDiskNotAssigned is FormatAssigned's refusal: dev is not one of
// plan's own devices (doc 03 §3.1 step 6's "explicit list of which
// disks will be erased"). The check runs before Provider.Format is ever
// called, so a wrong device path is refused here rather than trusted to
// Provider alone.
var ErrDiskNotAssigned = errors.New("disk: not part of this array-setup plan")

// ErrUnmanagedDevice is FormatAssigned / FormatPlan's refusal of a
// format target that is neither a loop device (the lab) nor a whole disk
// currently in Provider.List. A partition path such as /dev/sda1 —
// standing in for a real disk a bug might name — is refused before
// Provider.Format, so mkfs never runs against it.
var ErrUnmanagedDevice = errors.New("disk: format target is not a loop device or a listed whole disk")

// FormatAssigned formats dev through p, refusing (ErrDiskNotAssigned)
// unless dev is exactly one of plan's own assigned devices. This is the
// guard every path that can format a disk goes through — FormatPlan
// below, and the future Topology job handler — so a bug that names the
// wrong device is refused before it reaches a real mkfs invocation.
//
// Immediately before formatting, it also re-resolves the assigned disk's
// own stable identity (WWN or serial, doc 02 §4) via a fresh p.List call,
// refusing (ErrDiskIdentityChanged) if no disk at all currently matches
// it — a disk pulled between discovery and this call. When a disk does
// still match, formatting runs against resolveFormatTarget's own result
// rather than dev directly: the identity's /dev/disk/by-id path when one
// is known (assigned.ByIDName), so the actual mkfs invocation binds to
// whichever physical disk the kernel currently resolves that symlink to,
// not to dev itself — a device name discovered when this plan was built
// is not guaranteed to still name the same physical disk by the time the
// plan actually runs, a controller reorder or a swapped cable in between,
// and the typed confirmation this plan was checked against (doc 03 §3.1
// step 6) named a specific disk, not a path that might now belong to a
// different one.
func FormatAssigned(ctx context.Context, p Provider, plan TopologyPlan, dev string, fs FilesystemType) error {
	assigned, ok := plan.findDevice(dev)
	if !ok {
		return fmt.Errorf("%w: %s", ErrDiskNotAssigned, dev)
	}
	if err := allowFormatDevice(assigned); err != nil {
		return err
	}
	target, err := resolveFormatTarget(ctx, p, dev, assigned.WWN, assigned.Serial, assigned.ByIDName)
	if err != nil {
		return err
	}
	return p.Format(ctx, target, fs)
}

// resolveFormatTarget re-reads p's disk inventory to confirm a disk
// still matches the identity (wwn/serial) already confirmed for dev, and
// returns the path the caller's destructive call (Format or AdoptCheck)
// should actually run against. When wwn and serial are both empty —
// no by-id link at all, as every disk in the loop-device lab is (doc 06
// §3) — there is nothing to re-verify or bind to, but dev's own Boot flag
// is still checked before returning it unchanged: trusting Device for a
// disk without a by-id identity does not excuse skipping the boot-device
// guard every other path through this function gets.
//
// Otherwise it refuses (ErrBootDevice) the moment the matched disk turns
// out to be the boot device — before ever constructing a by-id path for
// it — and refuses (ErrDiskIdentityChanged) when no disk at all currently
// carries the confirmed identity, a disk pulled between discovery and
// this call. Once a non-boot match is confirmed to still exist: when
// byIDName is known, it first refuses (ErrDiskIdentityChanged) if the
// matched disk's own current by-id name has since changed — matching by
// WWN/serial alone and then trusting the plan's stale byIDName could
// bind the format to whatever disk now owns that name, not the one just
// matched. Once confirmed unchanged, it returns that /dev/disk/by-id path
// rather than dev or whatever /dev/sdX path List happened to report it at
// just now — that path is a symlink the kernel keeps pointed at whichever
// device currently carries this identity, so the caller's own destructive
// exec — which runs after this function returns, not during it — still
// opens the right physical disk even if the /dev/sdX numbering changes
// again in the interval between this check and that exec. When byIDName
// is not known, there is nothing to bind a path to, so this instead falls
// back to the pre-#157 behavior of refusing (ErrDiskIdentityChanged)
// unless the matched disk's current device is still exactly dev —
// returning the stale dev unverified would defeat the whole check.
func resolveFormatTarget(ctx context.Context, p Provider, dev, wwn, serial, byIDName string) (string, error) {
	if wwn == "" && serial == "" {
		disks, err := p.List(ctx)
		if err != nil {
			return "", err
		}
		for _, d := range disks {
			if d.Device == dev && d.Boot {
				return "", fmt.Errorf("%s: %w", dev, ErrBootDevice)
			}
		}
		return dev, nil
	}
	disks, err := p.List(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range disks {
		matched := (wwn != "" && d.WWN == wwn) || (wwn == "" && d.Serial == serial)
		if !matched {
			continue
		}
		if d.Boot {
			return "", fmt.Errorf("%s: %w", dev, ErrBootDevice)
		}
		if byIDName != "" {
			if d.ByIDName != byIDName {
				return "", fmt.Errorf("%w: by-id name changed for %s", ErrDiskIdentityChanged, dev)
			}
			return identityOrDevice(byIDName, dev), nil
		}
		if d.Device != dev {
			return "", fmt.Errorf("%w: %s (now found at %s)", ErrDiskIdentityChanged, dev, d.Device)
		}
		return dev, nil
	}
	return "", fmt.Errorf("%w: no disk currently matches the identity confirmed for %s", ErrDiskIdentityChanged, dev)
}

// identityOrDevice returns byIDName's /dev/disk/by-id path, or dev
// unchanged when byIDName is empty (nothing to bind to).
func identityOrDevice(byIDName, dev string) string {
	if p := (Identity{ByIDName: byIDName}).IdentityPath(); p != "" {
		return p
	}
	return dev
}

// FormatPlan formats or adopts every disk in plan (doc 02 §4 "Adding a
// disk" steps 2-3, doc 03 §3.1 step 6): it checks confirmation against
// plan's own Confirmation() text, validates the plan against sizes, runs
// AdoptCheck's read-only verification (Q23) on every adopted disk first,
// and only once every one of those has passed does it format the
// non-adopted disks through FormatAssigned. Parity disks are never
// adopted (Validate enforces this) and so are always among the disks this
// formats — running every AdoptCheck first, before any format, means a
// later adopted disk failing its check is discovered before an earlier
// disk in the same plan has already been erased, never after. It never
// touches a device outside plan's own Parity/Data/Cache lists.
//
// Each adopted disk's check runs through resolveFormatTarget exactly as
// FormatAssigned's own format call does: when the disk's identity is
// known, AdoptCheck runs against the by-id path the kernel currently
// resolves that identity to, not the plan's plain, transient Device path,
// so a udev reassignment between plan construction and this call cannot
// make AdoptCheck (or the boot-disk refusal resolveFormatTarget also
// performs) land on the wrong physical disk. A disk with no by-id link at
// all keeps using its plain Device path, unchanged.
func FormatPlan(ctx context.Context, p Provider, r Runner, plan TopologyPlan, sizes map[string]int64, confirmation string) error {
	if err := plan.CheckConfirmation(confirmation); err != nil {
		return err
	}
	if err := plan.Validate(sizes); err != nil {
		return err
	}
	if err := CheckFormatTargets(plan); err != nil {
		return err
	}

	disks := plan.assignedDisks()

	for _, d := range disks {
		if d.Adopt {
			target, err := resolveFormatTarget(ctx, p, d.Device, d.WWN, d.Serial, d.ByIDName)
			if err != nil {
				return err
			}
			if err := AdoptCheck(ctx, r, target, d.Filesystem); err != nil {
				return err
			}
		}
	}

	for _, d := range disks {
		if d.Adopt {
			continue
		}
		if err := FormatAssigned(ctx, p, plan, d.Device, d.Filesystem); err != nil {
			return err
		}
	}
	return nil
}

// CheckFormatTargets refuses (ErrUnmanagedDevice) a partition path such
// as /dev/sda1 — standing in for a real disk a bug might name — so mkfs
// never runs against it. Whole disks and loop devices are left to the
// assignment, identity and boot guards.
func CheckFormatTargets(plan TopologyPlan) error {
	for _, d := range plan.assignedDisks() {
		if err := allowFormatDevice(d); err != nil {
			return err
		}
	}
	return nil
}

func allowFormatDevice(assigned AssignedDisk) error {
	if IsLoopDevice(assigned.Device) {
		return nil
	}
	if looksLikePartition(assigned.Device) {
		return fmt.Errorf("%w: %s", ErrUnmanagedDevice, assigned.Device)
	}
	return nil
}

// looksLikePartition reports whether dev names a partition of a whole
// disk (sda1, nvme0n1p1, mmcblk0p1, loop0p1). Whole disks (sda,
// nvme0n1, mmcblk0, loop0) do not match. The check uses WholeDiskDevice
// so eMMC partitions are refused the same way NVMe partitions already
// were — a dedicated regex that required n<digits> treated mmcblk0p1 as
// a whole disk.
func looksLikePartition(dev string) bool {
	return WholeDiskDevice(dev) != dev
}

// IsLoopDevice reports whether dev is a whole loop device (/dev/loopN).
func IsLoopDevice(dev string) bool {
	base := filepath.Base(dev)
	n, ok := strings.CutPrefix(base, "loop")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
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
//
// EXT4 turns off mke2fs's lazy_itable_init and lazy_journal_init
// defaults. With them on, mke2fs returns before the inode tables and
// journal are zeroed and the kernel's ext4lazyinit thread does it in the
// background after the first mount, writing to an otherwise idle array
// disk for minutes (issue #347; Q31 wants array disks flat once
// settled). Format is the explicit user action that may write to the
// disk, so the zeroing happens here instead.
func formatCommand(dev string, fs FilesystemType) ([]string, error) {
	switch fs {
	case XFS:
		return []string{"mkfs.xfs", "-f", dev}, nil
	case EXT4:
		return []string{"mkfs.ext4", "-F", "-E", "lazy_itable_init=0,lazy_journal_init=0", dev}, nil
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
