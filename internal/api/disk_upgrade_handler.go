package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// dataDiskUpgradeSteps and parityDiskUpgradeSteps are planDiskUpgrade's
// own "copy/verify or parity-rebuild steps" (doc 02 §4 "Larger data
// disk"/"Larger parity disk", #116, #289), in the order the queued job
// actually runs them.
var dataDiskUpgradeSteps = []string{
	"with the array stopped, confirm snapraid diff has nothing to sync",
	"format the new disk",
	"copy the old disk's files to the new one, preserving ownership, xattrs and timestamps",
	"verify the copy against the old disk",
	"mount the new disk at the same mountpoint",
	"run snapraid diff and release the old disk once it reports nothing removed or updated",
	"leave the array stopped until you start it",
}

var parityDiskUpgradeSteps = []string{
	"copy the parity file to the new disk",
	"verify the copy byte for byte",
	"switch the configuration to the new parity disk",
	"run snapraid check against it",
	"release the old parity disk once it passes",
}

// arrayDiskRoleToAPI maps a persisted store.ArrayRole* spelling to
// apiv1.ArrayDiskRole — arrayRoleToAPI's own mapping (phase1_handler.go),
// but returning the enum planDiskUpgrade's own DiskUpgradePlan.role uses
// rather than PoolDiskEntryRole.
func arrayDiskRoleToAPI(role string) apiv1.ArrayDiskRole {
	switch role {
	case store.ArrayRoleParity:
		return apiv1.ArrayDiskRoleParity
	case store.ArrayRoleData:
		return apiv1.ArrayDiskRoleData
	default:
		return apiv1.ArrayDiskRoleCache
	}
}

// arrayDiskAtMountpoint returns the array_disks row at mountpoint,
// whatever its role — job.arrayDiskAtMountpoint's own lookup, reused here
// since planDiskUpgrade and upgradeDisk both need to know a slot's role
// before deciding which upgrade flow applies.
func arrayDiskAtMountpoint(disks []store.ArrayDisk, mountpoint string) (store.ArrayDisk, bool) {
	for _, d := range disks {
		if d.Mountpoint == mountpoint {
			return d, true
		}
	}
	return store.ArrayDisk{}, false
}

// parityBytesFrom returns every parity disk's own current size from
// sizes — job.parityDiskSizes' own logic, reused here for
// disk.DataDiskUpgradeExceedsParity's own preview in planDiskUpgrade.
func parityBytesFrom(disks []store.ArrayDisk, sizes map[string]int64) []int64 {
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

// PlanDiskUpgrade computes the upgrade plan for the array slot at
// req.Mountpoint, whichever role it holds (doc 02 §4 "Larger data
// disk"/"Larger parity disk", #116, #289): the replacement's own identity,
// the exact typed confirmation upgradeDisk requires, and — for a data
// disk — a refusal (invalid_plan) when the replacement would leave a
// parity disk smaller than it (Q20, disk.DataDiskUpgradeExceedsParity), or
// — for a parity disk — the fresh /mnt/parityN slot the new disk will
// occupy (Q71). A data disk in removal is refused (disk_leaving_array,
// #366): store.ReplaceDataDisk keeps the slot's removal state, so the new
// disk would inherit it. Read-only: nothing is formatted or persisted.
func (h *Handler) PlanDiskUpgrade(ctx context.Context, req *apiv1.DiskUpgradePlanRequest) (*apiv1.DiskUpgradePlan, error) {
	if h.Disks == nil || h.ArrayStore == nil {
		return nil, errArrayDisksNotConfigured()
	}
	disks, listed, err := h.currentArrayAndInventory(ctx)
	if err != nil {
		return nil, err
	}
	existing, ok := arrayDiskAtMountpoint(disks, req.Mountpoint)
	if !ok || (existing.Role != store.ArrayRoleData && existing.Role != store.ArrayRoleParity) {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if existing.LeavingArray() {
		return nil, errDiskLeavingArray(req.Mountpoint, existing.RemovalState)
	}

	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, apiv1.OptBool{}, listed)
	if err != nil {
		return nil, err
	}
	sizes := sizesFromListed(listed)

	plan := &apiv1.DiskUpgradePlan{
		Mountpoint:        req.Mountpoint,
		Role:              arrayDiskRoleToAPI(existing.Role),
		PreviousDevice:    existing.Device,
		ReplacementDevice: assigned.Device,
	}

	switch existing.Role {
	case store.ArrayRoleData:
		if disk.DataDiskUpgradeExceedsParity(sizes[assigned.Device], parityBytesFrom(disks, sizes)) {
			return nil, errInvalidPlan(job.ErrDataDiskUpgradeExceedsParity)
		}
		if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		plan.Steps = dataDiskUpgradeSteps
	case store.ArrayRoleParity:
		// A parity disk is always formatted XFS (Q20), regardless of what
		// req.Filesystem asked for.
		assigned.Filesystem = disk.XFS
		plan.ReplacementDevice = assigned.Device
		if err := job.ValidateParityDiskUpgrade(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		plan.Steps = parityDiskUpgradeSteps
		plan.NewMountpoint = apiv1.NewOptString(disk.NextParityMountpoint(storeMountpoints(disks)))
	}

	model, wwn, serial, size, currentFS := diskIdentityFields(assigned.Device, listed)
	plan.Model = model
	plan.Wwn = wwn
	plan.Serial = serial
	plan.SizeBytes = size
	plan.CurrentFilesystem = currentFS
	plan.Filesystem = filesystemToAPI(assigned.Filesystem)
	plan.Confirmation = job.SingleDiskConfirmation(assigned)
	return plan, nil
}

// UpgradeDisk queues a Topology job — job.TypeDiskUpgradeData or
// job.TypeDiskUpgradeParity, resolved from req.Mountpoint's own role —
// after re-validating the same checks planDiskUpgrade already computed. A
// stale or forged confirmation is refused (confirmation_required) before
// anything is submitted, and the queued job re-validates all of this
// again itself (doc 03 §3.2, #289). A data disk in removal is refused
// (disk_leaving_array) as planDiskUpgrade refuses it.
func (h *Handler) UpgradeDisk(ctx context.Context, req *apiv1.UpgradeDiskRequest) (*apiv1.Job, error) {
	if h.Disks == nil || h.ArrayStore == nil {
		return nil, errArrayDisksNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	disks, listed, err := h.currentArrayAndInventory(ctx)
	if err != nil {
		return nil, err
	}
	existing, ok := arrayDiskAtMountpoint(disks, req.Mountpoint)
	if !ok || (existing.Role != store.ArrayRoleData && existing.Role != store.ArrayRoleParity) {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if existing.LeavingArray() {
		return nil, errDiskLeavingArray(req.Mountpoint, existing.RemovalState)
	}

	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, apiv1.OptBool{}, listed)
	if err != nil {
		return nil, err
	}
	sizes := sizesFromListed(listed)

	switch existing.Role {
	case store.ArrayRoleData:
		if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
			return nil, errConfirmRequired
		}
		if disk.DataDiskUpgradeExceedsParity(sizes[assigned.Device], parityBytesFrom(disks, sizes)) {
			return nil, errInvalidPlan(job.ErrDataDiskUpgradeExceedsParity)
		}
		if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		old := disk.AssignedDisk{
			Device:       existing.Device,
			Filesystem:   disk.FilesystemType(existing.Filesystem),
			WWN:          existing.WWN,
			Serial:       existing.Serial,
			WeakIdentity: existing.WeakIdentity,
			ByIDName:     existing.ByIDName,
			FSUUID:       existing.FSUUID,
		}
		body, err := json.Marshal(job.DiskUpgradeDataParams{Confirmation: req.Confirmation, Mountpoint: req.Mountpoint, Old: old, Disk: assigned, Sizes: sizes})
		if err != nil {
			return nil, fmt.Errorf("encoding disk_upgrade_data params: %w", err)
		}
		j, err := h.Scheduler.Submit(ctx, job.TypeDiskUpgradeData, nil, body)
		if err != nil {
			return nil, mapSchedulerError(uuid.Nil, err)
		}
		return jobToAPI(j)
	case store.ArrayRoleParity:
		assigned.Filesystem = disk.XFS
		if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
			return nil, errConfirmRequired
		}
		if err := job.ValidateParityDiskUpgrade(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		newMountpoint, ok := req.NewMountpoint.Get()
		if !ok || newMountpoint == "" {
			return nil, errInvalidPlan(fmt.Errorf("disk: a parity upgrade requires newMountpoint from the matching plan"))
		}
		wantMountpoint := disk.NextParityMountpoint(storeMountpoints(disks))
		if newMountpoint != wantMountpoint {
			return nil, errInvalidPlan(fmt.Errorf("disk: newMountpoint %q no longer matches the array's current topology (now %q) — recompute the plan", newMountpoint, wantMountpoint))
		}
		body, err := json.Marshal(job.DiskUpgradeParityParams{Confirmation: req.Confirmation, Mountpoint: req.Mountpoint, NewMountpoint: newMountpoint, Disk: assigned, Sizes: sizes})
		if err != nil {
			return nil, fmt.Errorf("encoding disk_upgrade_parity params: %w", err)
		}
		j, err := h.Scheduler.Submit(ctx, job.TypeDiskUpgradeParity, nil, body)
		if err != nil {
			return nil, mapSchedulerError(uuid.Nil, err)
		}
		return jobToAPI(j)
	default:
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
}
