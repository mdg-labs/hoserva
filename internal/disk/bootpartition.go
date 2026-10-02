package disk

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// The cache may sit on a spare partition of the boot disk (doc 01 §6,
// doc 02 §4) — the one exception to "array disks are whole, non-boot
// disks". Hoserva formats that existing, unmounted, blank partition and
// nothing else: it never writes the boot disk's partition table, and a
// data or parity slot is never a partition.
var (
	// ErrBootPartitionNotCache refuses a boot-disk partition assigned to
	// any role but cache.
	ErrBootPartitionNotCache = errors.New("disk: a partition of the boot disk can only be the cache")
	// ErrBootPartitionNotSpare refuses a partition that is not, right
	// now, one of the boot disk's spare partitions (root, EFI, swap, a
	// mounted or fstab-named one, one holding any signature or held open
	// by another device), or whose identity cannot be re-resolved.
	ErrBootPartitionNotSpare = errors.New("disk: the partition is not a spare partition of the boot disk")
	// ErrBootPartitionNotBlank refuses a spare partition the blank probe
	// did not positively confirm carries no signature, including when no
	// probe was available or the probe failed.
	ErrBootPartitionNotBlank = errors.New("disk: the spare partition is not confirmed blank")
	// ErrBootPartitionAdopt refuses adopting an existing filesystem on a
	// boot-disk partition: only a blank partition is ever formatted.
	ErrBootPartitionAdopt = errors.New("disk: a boot-disk partition is formatted fresh, never adopted")
)

// IsPartition reports whether an assigned device is a partition rather
// than a whole disk: a kernel partition name (sda1, nvme0n1p1) or a
// by-id link carrying a "-partN" suffix.
func IsPartition(device, byIDName string) bool {
	if looksLikePartition(device) {
		return true
	}
	return partitionByIDSuffix.MatchString(byIDName) || partitionByIDSuffix.MatchString(filepath.Base(device))
}

type bootCacheTargetKey struct{}

// withBootCacheTarget marks target as the one boot-disk partition a
// Provider.Format call may format despite the boot-disk refusal. Only
// FormatAssignedProbed sets it, after resolveCachePartitionTarget has
// confirmed the partition is a blank spare; it travels in the context so
// it passes through every Provider wrapper unchanged.
func withBootCacheTarget(ctx context.Context, target string) context.Context {
	return context.WithValue(ctx, bootCacheTargetKey{}, target)
}

// bootCacheTargetAllowed reports whether dev is exactly the partition
// withBootCacheTarget named and is itself a partition path — the whole
// boot disk is never allowed, whatever the context says.
func bootCacheTargetAllowed(ctx context.Context, dev string) bool {
	target, _ := ctx.Value(bootCacheTargetKey{}).(string)
	return target != "" && target == dev && IsPartition(dev, "")
}

// resolveCachePartitionTarget confirms, immediately before mkfs, that a
// cache assignment naming a boot-disk partition still names a blank spare
// one: its parent is still the boot disk, a fresh List still reports it
// as a candidate under the same by-id name and PARTUUID, and the blank
// probe positively finds no signature on it. It returns the partition's
// /dev/disk/by-id path, which mkfs binds to. Any missing identity, absent
// probe or probe error refuses.
func resolveCachePartitionTarget(ctx context.Context, p Provider, probe BlankProber, a AssignedDisk) (string, error) {
	if a.ByIDName == "" || a.PartUUID == "" || (a.WWN == "" && a.Serial == "") {
		return "", fmt.Errorf("%w: %s has no stable identity to bind to", ErrBootPartitionNotSpare, a.Device)
	}
	disks, err := p.List(ctx)
	if err != nil {
		return "", err
	}
	found := false
	for _, d := range disks {
		sameDisk := (a.WWN != "" && d.WWN == a.WWN) || (a.WWN == "" && d.Serial == a.Serial)
		if !d.Boot || !sameDisk {
			continue
		}
		for _, c := range d.CachePartitions {
			if c.ByIDName == a.ByIDName && strings.EqualFold(c.PartUUID, a.PartUUID) {
				found = true
			}
		}
	}
	if !found {
		return "", fmt.Errorf("%w: %s", ErrBootPartitionNotSpare, a.Device)
	}
	target := identityOrDevice(a.ByIDName, a.Device)
	if probe == nil {
		return "", fmt.Errorf("%w: no probe available for %s", ErrBootPartitionNotBlank, a.Device)
	}
	blank, err := probe.ProbeBlank(ctx, target)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrBootPartitionNotBlank, a.Device, err)
	}
	if !blank {
		return "", fmt.Errorf("%w: %s", ErrBootPartitionNotBlank, a.Device)
	}
	return target, nil
}

// FindCachePartition returns the spare boot-disk partition listed for
// device (by its kernel path) and the boot disk it sits on.
func FindCachePartition(listed []Disk, device string) (CachePartition, Disk, bool) {
	for _, d := range listed {
		if !d.Boot {
			continue
		}
		for _, c := range d.CachePartitions {
			if c.Device == device {
				return c, d, true
			}
		}
	}
	return CachePartition{}, Disk{}, false
}

// IsBootDiskPartition reports whether device is any partition of a disk
// listed as the boot disk — spare or not.
func IsBootDiskPartition(listed []Disk, device string) bool {
	whole := WholeDiskDevice(device)
	if whole == device {
		return false
	}
	for _, d := range listed {
		if d.Boot && d.Device == whole {
			return true
		}
	}
	return false
}

// BindBootPartition resolves assigned.Device when it is a partition of a
// disk listed as the boot disk — the one check the API and its mock share.
// It reports isPartition=false for every other device, leaving it to the
// whole-disk and loop-device paths. A boot-disk partition is accepted only
// for the cache role (ErrBootPartitionNotCache otherwise, spare or not),
// never adopted (ErrBootPartitionAdopt), and only when it is one of the
// boot disk's listed spare partitions (ErrBootPartitionNotSpare
// otherwise). On success it returns assigned with the parent disk's
// identity, the partition's by-id name and PARTUUID, and the partition's
// size, which the caller keys by device into the plan's sizes.
func BindBootPartition(listed []Disk, assigned AssignedDisk, cacheRole bool) (bound AssignedDisk, size int64, isPartition bool, err error) {
	if !IsBootDiskPartition(listed, assigned.Device) {
		return assigned, 0, false, nil
	}
	if !cacheRole {
		return assigned, 0, true, fmt.Errorf("%w: %s", ErrBootPartitionNotCache, assigned.Device)
	}
	part, parent, ok := FindCachePartition(listed, assigned.Device)
	if !ok {
		return assigned, 0, true, fmt.Errorf("%w: %s", ErrBootPartitionNotSpare, assigned.Device)
	}
	if assigned.Adopt {
		return assigned, 0, true, fmt.Errorf("%w: %s", ErrBootPartitionAdopt, assigned.Device)
	}
	assigned.WWN = parent.WWN
	assigned.Serial = parent.Serial
	assigned.WeakIdentity = parent.WeakIdentity
	assigned.ByIDName = part.ByIDName
	assigned.PartUUID = part.PartUUID
	return assigned, part.Size, true, nil
}
