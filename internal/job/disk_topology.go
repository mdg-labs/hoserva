package job

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// SingleDiskConfirmation returns the exact typed confirmation adding or
// replacing d requires (doc 03 §3.1 step 6's own convention, reused here
// rather than reinvented): "ERASE <device>", or "ADOPT ONLY — NOTHING
// ERASED" when d.Adopt is true. Both planDiskAdd and planDiskReplace
// (internal/api) call this so the plan they return and the confirmation
// RunDiskAdd/RunDiskReplace check are always the same string.
func SingleDiskConfirmation(d disk.AssignedDisk) string {
	return (disk.TopologyPlan{Data: []disk.AssignedDisk{d}}).Confirmation()
}

// storeDiskAssigned reduces a persisted store.ArrayDisk to the fields
// disk.TopologyPlan.Validate needs: it is never adopted or reformatted by
// this check, only sized and role-checked against the disk actually being
// added or replaced.
func storeDiskAssigned(r store.ArrayDisk) disk.AssignedDisk {
	return disk.AssignedDisk{
		Device:       r.Device,
		Filesystem:   disk.FilesystemType(r.Filesystem),
		WeakIdentity: r.WeakIdentity,
	}
}

// ErrDiskAlreadyMember refuses a device already identified as one of the
// array's own members by WWN or serial (Q21) — never by /dev/sdX, which
// renumbers across a reboot or hotplug and would otherwise let a stale or
// forged plan re-offer an existing member as if it were a fresh one.
var ErrDiskAlreadyMember = errors.New("disk: this disk is already a member of the array")

// refuseKnownIdentity refuses (ErrDiskAlreadyMember) d when its WWN or
// serial (disk.Identity.Matches, Q21) matches a disk already in disks, or
// — for a weak-identity d with no WWN or serial at all, as every disk in
// the loop-device lab is (doc 06 §3) — when its filesystem UUID matches a
// weak-identity disk already in disks. That fallback is the same rule
// matchArrayDisk (internal/api's GetPool reconciliation) and
// ConfirmReplacementTargetAbsent already use to recognise a weak-identity
// disk at a renumbered /dev/sdX path as the same physical disk: without
// it, a weak-identity array member unmounted and reattached at a new path
// has nothing WWN/serial can compare and would otherwise pass this check
// as if it were a fresh disk. Size is not compared here: AssignedDisk
// carries no capacity, and a same-UUID clone is still refused by
// UNIQUE(fs_uuid) on insert; GetPool's own matchArrayDisk is what must
// not treat a different-capacity clone as the live member (#327).
func refuseKnownIdentity(disks []store.ArrayDisk, d disk.AssignedDisk) error {
	target := disk.Identity{WWN: d.WWN, Serial: d.Serial}
	for _, r := range disks {
		if target.Matches(disk.Identity{WWN: r.WWN, Serial: r.Serial}) {
			return fmt.Errorf("%w: %s", ErrDiskAlreadyMember, d.Device)
		}
		if d.WeakIdentity && r.WeakIdentity && d.FSUUID != "" && d.FSUUID == r.FSUUID {
			return fmt.Errorf("%w: %s", ErrDiskAlreadyMember, d.Device)
		}
	}
	return nil
}

// ErrDiskIdentityDrifted refuses a disk_add or disk_replace job when the
// physical disk currently at the submitted target's device path is no
// longer the one the plan was confirmed against. A disk_add or
// disk_replace job can sit queued behind a running storage-class job for
// as long as that job runs (ClassTopology conflicts with every storage
// class, doc 01 §4), and a bus reset or hotplug in that window can put a
// different physical disk — including another array member renumbered
// onto the same path — at target.Device by the time the job actually
// runs.
var ErrDiskIdentityDrifted = errors.New("disk: the disk at this path no longer matches what was confirmed")

// confirmTargetIdentityUnchanged refuses (ErrDiskIdentityDrifted) unless
// target.Device still carries exactly the identity submitted for it: a
// fresh disk inventory's WWN, Serial, ByIDName, WeakIdentity and FSUUID for
// target.Device must all equal target's own, field for field, with no
// exception for an empty submitted value — a fresh, unformatted disk
// submits an empty FSUUID, and the fresh listing's FSUUID must still be
// empty too, or this refuses. A weak-identity disk with no WWN, serial or
// by-id link at all (Q21) has nothing but its FSUUID to compare, so
// skipping that field whenever it was submitted empty would leave this
// check unable to tell such a disk apart from any other holding data —
// including one that was never an array member at all, which
// refuseKnownIdentity's own membership check would then wave through.
// Comparing every field the confirmation dialog and the eventual format
// both key off, unconditionally, means a field this check does not know to
// compare cannot be the gap: any drift refuses, not just the ones this
// function happens to look for.
//
// A non-loop target absent from listed entirely is refused too — the only
// remaining window for that is the interval between this List call and
// the format itself. target.Device being a /dev/loopN path (the lab, doc
// 06 §3) is exempted from both checks: Lister.List never reports loop
// devices (internal/disk/enumerate.go), so they would otherwise always be
// refused as absent, and they never renumber onto another disk's path the
// way a real disk's /dev/sdX name can.
func confirmTargetIdentityUnchanged(listed []disk.Disk, target disk.AssignedDisk) (disk.AssignedDisk, error) {
	if disk.IsLoopDevice(target.Device) {
		return target, nil
	}
	current, err := disk.LookupDisk(listed, target.Device)
	if err != nil {
		return disk.AssignedDisk{}, fmt.Errorf("%w: %s is no longer listed", ErrDiskIdentityDrifted, target.Device)
	}
	if current.WWN != target.WWN || current.Serial != target.Serial || current.ByIDName != target.ByIDName || current.WeakIdentity != target.WeakIdentity || current.FSUUID != target.FSUUID {
		return disk.AssignedDisk{}, fmt.Errorf("%w: %s", ErrDiskIdentityDrifted, target.Device)
	}
	return target, nil
}

// ValidateDiskAddition re-runs array setup's own Q19/Q20/Q23 checks
// (disk.TopologyPlan.Validate) over the array's current topology plus d,
// the disk being added (doc 02 §4 "Adding a disk"): every parity disk
// already in the array must still be at least as large as the largest
// data disk once d joins it, d's own filesystem must be one Q23 allows,
// and d must not already be one of the array's own devices — by identity
// (Q21), not merely by its current /dev/sdX path. sizes must carry every
// device this plan touches — d's own device, plus every existing parity
// and data disk's device — or Validate refuses (disk.ErrMissingSize)
// rather than silently skip the check.
func ValidateDiskAddition(disks []store.ArrayDisk, d disk.AssignedDisk, sizes map[string]int64) error {
	if err := refuseKnownIdentity(disks, d); err != nil {
		return err
	}
	full := disk.TopologyPlan{Data: []disk.AssignedDisk{d}}
	for _, r := range disks {
		switch r.Role {
		case store.ArrayRoleParity:
			full.Parity = append(full.Parity, storeDiskAssigned(r))
		case store.ArrayRoleData:
			full.Data = append(full.Data, storeDiskAssigned(r))
		}
	}
	return full.Validate(sizes)
}

// ValidateDiskReplacement is ValidateDiskAddition for doc 02 §4 "Replacing
// a failed disk": the same Q19/Q20/Q23 and identity checks, over the
// array's current topology with the mountpoint slot being replaced
// excluded (its old device is leaving the array) and replacement — the
// new disk — in its place.
func ValidateDiskReplacement(disks []store.ArrayDisk, mountpoint string, replacement disk.AssignedDisk, sizes map[string]int64) error {
	remaining := make([]store.ArrayDisk, 0, len(disks))
	for _, r := range disks {
		if r.Mountpoint == mountpoint {
			continue
		}
		remaining = append(remaining, r)
	}
	if err := refuseKnownIdentity(remaining, replacement); err != nil {
		return err
	}
	full := disk.TopologyPlan{Data: []disk.AssignedDisk{replacement}}
	for _, r := range remaining {
		switch r.Role {
		case store.ArrayRoleParity:
			full.Parity = append(full.Parity, storeDiskAssigned(r))
		case store.ArrayRoleData:
			full.Data = append(full.Data, storeDiskAssigned(r))
		}
	}
	return full.Validate(sizes)
}

// ErrReplacementSlotDiskPresent refuses a replace whose slot's recorded
// disk is still mounted at the mountpoint, or still present in a fresh
// disk inventory by its stored identity (doc 02 §4 "Replacing a failed
// disk" steps 1-2): a disk that has not actually failed or been removed
// goes through the upgrade flow instead (#289), never replace — SnapRAID's
// own fix must never run against a slot whose real disk is still there.
var ErrReplacementSlotDiskPresent = errors.New("disk: the disk at this slot is still present; use the upgrade flow instead")

// ConfirmReplacementTargetAbsent refuses (ErrReplacementSlotDiskPresent)
// replacing old's slot at mountpoint unless old's own disk is genuinely
// gone: nothing is currently mounted at mountpoint, and no disk in listed
// still carries old's stored WWN or serial (disk.Identity.Matches, Q21),
// or — for a weak-identity disk with no WWN or serial at all, as every
// disk in the loop-device lab is (doc 06 §3) — its stored filesystem UUID
// and, when size is stored, equal size (the same fallback GetPool's own
// matchArrayDisk uses, #326/#327). It never trusts an unmount alone: a
// failed disk merely detached from its bay but still attached over another
// path would otherwise slip through. A stat error reading mountpoint (most
// commonly the path not existing) is treated as "not mounted" rather than
// propagated, the same fail-open reading applyArrayFromStore's own
// alreadyMounted already relies on for this exact check.
func ConfirmReplacementTargetAbsent(mountpoint string, old store.ArrayDisk, listed []disk.Disk) error {
	if mounted, err := disk.IsMountpoint(mountpoint); err == nil && mounted {
		return fmt.Errorf("%w: %s is still mounted", ErrReplacementSlotDiskPresent, mountpoint)
	}
	oldIdentity := disk.Identity{WWN: old.WWN, Serial: old.Serial}
	for _, inv := range listed {
		if oldIdentity.Matches(disk.Identity{WWN: inv.WWN, Serial: inv.Serial}) {
			return fmt.Errorf("%w: %s", ErrReplacementSlotDiskPresent, inv.Device)
		}
		if old.WeakIdentity && inv.WeakIdentity && old.FSUUID != "" && old.FSUUID == inv.FSUUID {
			if old.SizeSet && old.Size != inv.Size {
				continue
			}
			return fmt.Errorf("%w: %s", ErrReplacementSlotDiskPresent, inv.Device)
		}
	}
	return nil
}

// mountpointsOf returns every disk's mountpoint, in no particular role
// order — NextDataMountpoint only ever matches the "/mnt/diskN" shape
// against it, so parity and cache mountpoints mixed in are harmless.
func mountpointsOf(disks []store.ArrayDisk) []string {
	out := make([]string, 0, len(disks))
	for _, d := range disks {
		out = append(out, d.Mountpoint)
	}
	return out
}

// dataMountRoleIndex parses N out of a "/mnt/diskN" mountpoint — the same
// number NextDataMountpoint just picked — so a newly added disk's
// array_disks row gets the matching role_index the UNIQUE(role,
// role_index) constraint and every other data disk's own row already use.
func dataMountRoleIndex(mountpoint string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(mountpoint, "/mnt/disk"))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("job: unexpected data mountpoint %q", mountpoint)
	}
	return n, nil
}

// DataDiskLabelForMountpoint returns the SnapRAID "dN" label a rendered
// snapraid.conf currently assigns the data disk at mountpoint — the same
// position-based numbering parity.Layout.Render uses (data disks in
// role_index order, "dN" by position) — for display in a replace plan
// preview only (internal/api's planDiskReplace). RunDiskReplace never
// trusts this: it resolves the real label fresh from a live
// parity.ParityStatus once the replacement's own config has been
// regenerated, so a mismatch here (only possible after an earlier disk
// removal has left a role_index gap, #274) affects nothing but the plan's
// own preview text.
func DataDiskLabelForMountpoint(disks []store.ArrayDisk, mountpoint string) (string, error) {
	i := 0
	for _, d := range disks {
		if d.Role != store.ArrayRoleData {
			continue
		}
		i++
		if d.Mountpoint == mountpoint {
			return fmt.Sprintf("d%d", i), nil
		}
	}
	return "", fmt.Errorf("job: no data disk at %s", mountpoint)
}

// ValidateParityDiskUpgrade is ValidateDiskReplacement for a parity slot
// (doc 02 §4 "Larger parity disk", #289): the same Q19/Q20/Q21/Q23 and
// identity checks, over the array's current topology with the parity slot
// being upgraded excluded (its old device is leaving that slot, the new
// one taking over its role_index at a fresh mountpoint) and replacement —
// the new, larger parity disk — in its place.
func ValidateParityDiskUpgrade(disks []store.ArrayDisk, mountpoint string, replacement disk.AssignedDisk, sizes map[string]int64) error {
	remaining := make([]store.ArrayDisk, 0, len(disks))
	for _, r := range disks {
		if r.Mountpoint == mountpoint {
			continue
		}
		remaining = append(remaining, r)
	}
	if err := refuseKnownIdentity(remaining, replacement); err != nil {
		return err
	}
	full := disk.TopologyPlan{Parity: []disk.AssignedDisk{replacement}}
	for _, r := range remaining {
		switch r.Role {
		case store.ArrayRoleParity:
			full.Parity = append(full.Parity, storeDiskAssigned(r))
		case store.ArrayRoleData:
			full.Data = append(full.Data, storeDiskAssigned(r))
		}
	}
	return full.Validate(sizes)
}

// ErrDataDiskUpgradeExceedsParity is planDiskUpgrade's and
// RunDiskUpgradeData's own refusal (doc 02 §4, Q71, Q20): the replacement
// data disk would leave a parity disk smaller than it, the same rule
// disk.DataDiskUpgradeExceedsParity checks directly against the array's
// current parity disks — "when a new data disk would be larger than the
// current parity, the flow offers [a parity upgrade] first."
var ErrDataDiskUpgradeExceedsParity = errors.New("disk: this data disk upgrade would leave a parity disk smaller than the new disk; upgrade parity first")

// arrayDiskAtMountpoint returns the array_disks row at mountpoint,
// whatever its role — GetDataDiskByMountpoint's own lookup is data-only
// (doc 02 §4's replace flow is specific to data disks), but a disk
// upgrade (#289) targets a parity slot exactly as often as a data one, so
// this checks disks already read from one GetArray call rather than
// adding a second, role-specific store round trip.
func arrayDiskAtMountpoint(disks []store.ArrayDisk, mountpoint string) (store.ArrayDisk, bool) {
	for _, d := range disks {
		if d.Mountpoint == mountpoint {
			return d, true
		}
	}
	return store.ArrayDisk{}, false
}

// parityDiskSizes returns every parity disk's own current size from
// sizes (the same disk.Provider.List snapshot a plan or job run already
// has), skipping a parity disk sizes has no entry for rather than
// treating a missing key as zero (disk.DataDiskUpgradeExceedsParity would
// otherwise never trigger against a parity disk this snapshot simply
// didn't see).
func parityDiskSizes(disks []store.ArrayDisk, sizes map[string]int64) []int64 {
	var out []int64
	for _, d := range disks {
		if d.Role != store.ArrayRoleParity {
			continue
		}
		if s, ok := sizes[d.Device]; ok {
			out = append(out, s)
		}
	}
	return out
}

// diskUpgradeStagingPath is the scratch mountpoint a data disk upgrade
// mounts the new, larger disk at while it copies and verifies the old
// disk's tree (doc 02 §4 "Larger data disk", #289) — never a store-
// tracked slot, and never mounted through config.Generator's own unit
// files: disk.RunDataDiskUpgrade mounts and unmounts it directly through
// its own Mounter dependency, so this only ever needs to be a stable,
// collision-free path, not a persisted one.
func diskUpgradeStagingPath(mountpoint string) string {
	return "/mnt/.hoserva-disk-upgrade-staging" + mountpoint
}

// confirmMountedUUID refuses unless the filesystem mounted at unit.Where
// carries unit.UUID — the disk a caller is about to trust, not whichever
// disk happens to be mounted there. A mismatch is never worked around.
func confirmMountedUUID(ctx context.Context, r disk.Runner, unit disk.MountUnit) error {
	mounted, err := disk.MountedUUID(ctx, r, unit.Where)
	if err != nil {
		return fmt.Errorf("confirming the filesystem mounted at %s: %w", unit.Where, err)
	}
	if mounted != unit.UUID {
		return fmt.Errorf("%s is mounted with filesystem UUID %s, want %s — refusing", unit.Where, mounted, unit.UUID)
	}
	return nil
}

// formatTargetOf returns the path FormatForAddition's own destructive call
// actually binds to for d: its /dev/disk/by-id path when ByIDName is
// known, so a filesystem-UUID read after formatting lands on the same
// physical disk the format itself just ran against, or d.Device unchanged
// when there is nothing to bind to (Q21).
func formatTargetOf(d disk.AssignedDisk) string {
	if p := (disk.Identity{ByIDName: d.ByIDName}).IdentityPath(); p != "" {
		return p
	}
	return d.Device
}
