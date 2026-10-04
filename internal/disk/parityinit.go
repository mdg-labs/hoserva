package disk

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrMirroredBootCache refuses formatting a cache that is a partition of an
	// Unraid boot device while more than one Unraid boot device is attached. An
	// Unraid boot pool may be a mirrored pair, each member keeping its own
	// partition 4, and nothing here can tell which member Unraid reads its pool
	// from: a partition of a mirrored boot pool is never formatted, and neither
	// is the other member's.
	ErrMirroredBootCache = errors.New("disk: this cache is a partition of an Unraid boot device, and more than one Unraid boot device is attached (a mirrored boot pool): it is never formatted")
	// ErrUnraidCacheNotPartition refuses a whole-disk format of a disk that is an
	// Unraid boot device: that would erase Unraid's boot pool, partitions 1 to 3,
	// which only its own confirmed action does.
	ErrUnraidCacheNotPartition = errors.New("disk: this disk is an Unraid boot device: only its data partition can be the cache, never the whole disk")
)

// ErasePlan is what the point of no return erases (doc 05 §4 step 17) as the
// TopologyPlan the format guards and the typed confirmation work on: the
// former parity disks, always XFS (Q20), and the cache, formatted XFS. The data
// disks are not in it: they are adopted, never erased.
func (p AdoptionPlan) ErasePlan() TopologyPlan {
	var t TopologyPlan
	for _, d := range p.Parity {
		a := d.AssignedDisk
		a.Filesystem = XFS
		t.Parity = append(t.Parity, a)
	}
	if p.Cache != nil {
		a := p.Cache.AssignedDisk
		a.Filesystem = XFS
		t.Cache = &a
	}
	return t
}

// ParityInitFinishConfirmation is the typed confirmation of finishing a parity
// initialisation that stopped after the former parity and cache disks were
// formatted and recorded: it erases nothing, so there is no device to name.
const ParityInitFinishConfirmation = "FINISH PARITY INITIALISATION"

// ParityInitConfirmation is the exact typed confirmation the point of no return
// requires, naming every device it erases, in the style of
// TopologyPlan.Confirmation.
func (p AdoptionPlan) ParityInitConfirmation() string {
	return p.ErasePlan().Confirmation()
}

// FormatParityInit formats the former Unraid parity disks and the cache of plan,
// the one step of the migration that erases anything (doc 05 §4 step 17). It
// formats nothing but the devices the confirmation names, which is plan's parity
// and cache and never one of its data disks, and only once all of them have been
// resolved again from a fresh inventory:
//
//   - a parity disk is a whole disk resolved by its WWN or serial (never by the
//     device name it had when the mapping was confirmed), is neither the boot
//     disk, the Unraid stick nor an Unraid boot device, and is formatted through
//     its by-id path when it has one (resolveFormatTarget, Q21);
//   - a cache that is a whole disk is resolved the same way;
//   - a cache that is a spare partition of the disk this machine boots from is
//     formatted only after the partition is shown to be still one of its spare
//     partitions, by by-id name and PARTUUID, and probed blank
//     (resolveCachePartitionTarget);
//   - a cache that is a partition of an Unraid internal boot device is the data
//     partition udev lists for it, by by-id name and PARTUUID, and nothing else:
//     partitions 1 to 3, Unraid's boot pool, and the disk itself are never
//     formatted, and a second Unraid boot device attached (a mirrored boot pool)
//     refuses it (ErrMirroredBootCache).
//
// A refused disk erases nothing: every target is resolved before the first
// mkfs, and each is resolved again immediately before its own.
func FormatParityInit(ctx context.Context, p Provider, probe BlankProber, plan AdoptionPlan, confirmation string) error {
	if err := plan.Validate(); err != nil {
		return err
	}
	erase := plan.ErasePlan()
	if err := erase.CheckConfirmation(confirmation); err != nil {
		return err
	}
	if err := CheckFormatTargets(erase); err != nil {
		return err
	}
	type target struct {
		disk  AssignedDisk
		cache bool
	}
	var targets []target
	for _, d := range erase.Parity {
		targets = append(targets, target{disk: d})
	}
	if erase.Cache != nil {
		targets = append(targets, target{disk: *erase.Cache, cache: true})
	}
	for _, t := range targets {
		if _, _, err := resolveEraseTarget(ctx, p, probe, t.disk, t.cache); err != nil {
			return err
		}
	}
	for _, t := range targets {
		path, fctx, err := resolveEraseTarget(ctx, p, probe, t.disk, t.cache)
		if err != nil {
			return err
		}
		if err := p.Format(fctx, path, XFS); err != nil {
			return err
		}
	}
	return nil
}

// resolveEraseTarget confirms a disk of the erase plan is still the one the
// mapping named and returns the path its mkfs binds to, and the context its
// Format call runs under.
func resolveEraseTarget(ctx context.Context, p Provider, probe BlankProber, a AssignedDisk, cache bool) (string, context.Context, error) {
	listed, err := p.List(ctx)
	if err != nil {
		return "", ctx, err
	}
	parent, err := findAdoptable(listed, a.WWN, a.Serial)
	if err != nil {
		return "", ctx, err
	}
	if IsUnraidStick(parent) {
		return "", ctx, fmt.Errorf("%s: %w", parent.Device, ErrUnraidStick)
	}
	if cache && IsPartition(a.Device, a.ByIDName) {
		switch {
		case parent.Boot:
			target, err := resolveCachePartitionTarget(ctx, p, probe, a)
			if err != nil {
				return "", ctx, err
			}
			return target, withBootCacheTarget(ctx, target), nil
		case parent.UnraidBoot:
			target, err := resolveUnraidCachePartition(listed, parent, a)
			return target, ctx, err
		}
		return "", ctx, fmt.Errorf("%w: %s is a partition of neither the boot disk nor an Unraid boot device", ErrBootPartitionNotSpare, a.Device)
	}
	if parent.UnraidBoot {
		return "", ctx, fmt.Errorf("%s: %w", parent.Device, ErrUnraidCacheNotPartition)
	}
	target, err := resolveFormatTarget(ctx, p, a.Device, a.WWN, a.Serial, a.ByIDName)
	return target, ctx, err
}

// resolveUnraidCachePartition confirms a cache recorded as the data partition of
// an Unraid internal boot device is still that partition: its disk is not the
// disk this machine boots from, is the only Unraid boot device attached, and
// still lists the partition under the by-id name and PARTUUID recorded. It
// returns the partition's by-id path, which mkfs binds to.
func resolveUnraidCachePartition(listed []Disk, parent Disk, a AssignedDisk) (string, error) {
	if a.ByIDName == "" || a.PartUUID == "" || !partitionByIDSuffix.MatchString(a.ByIDName) {
		return "", fmt.Errorf("%w: %s has no partition identity to bind to", ErrAdoptNoCacheBinding, a.Device)
	}
	if parent.Boot {
		return "", fmt.Errorf("%s: %w", parent.Device, ErrBootDevice)
	}
	var boots []string
	for _, d := range listed {
		if d.UnraidBoot {
			boots = append(boots, d.Device)
		}
	}
	if len(boots) > 1 {
		return "", fmt.Errorf("%w (%s)", ErrMirroredBootCache, strings.Join(boots, ", "))
	}
	part := parent.UnraidDataPartition
	if !parent.UnraidBoot || part == nil || part.ByIDName != a.ByIDName || !strings.EqualFold(part.PartUUID, a.PartUUID) {
		return "", fmt.Errorf("%w: %s no longer lists the data partition %s (PARTUUID %s)", ErrAdoptDiskChanged, parent.Device, a.ByIDName, a.PartUUID)
	}
	return identityOrDevice(a.ByIDName, part.Device), nil
}
