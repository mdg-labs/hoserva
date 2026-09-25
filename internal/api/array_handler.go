package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errInvalidPlan(err error) error {
	return &apiError{code: "invalid_plan", statusCode: 400, message: err.Error()}
}

func errUnmanagedDevice(err error) error {
	return &apiError{code: "unmanaged_device", statusCode: 400, message: err.Error()}
}

func errSlotDiskPresent(err error) error {
	return &apiError{code: "slot_disk_present", statusCode: 409, message: err.Error()}
}

func (h *Handler) CreateArray(ctx context.Context, req *apiv1.CreateArrayRequest) (*apiv1.Job, error) {
	if h.Disks == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "disk inventory is not configured on this daemon"}
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}

	listed, err := h.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}

	params, plan, err := diskFormatParamsFromRequest(req, listed)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || plan.CheckConfirmation(req.Confirmation) != nil {
		return nil, errConfirmRequired
	}
	if err := plan.Validate(params.Sizes); err != nil {
		return nil, errInvalidPlan(err)
	}
	if err := disk.CheckFormatTargets(plan); err != nil {
		return nil, errUnmanagedDevice(err)
	}

	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encoding disk_format params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeDiskFormat, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func diskFormatParamsFromRequest(req *apiv1.CreateArrayRequest, listed []disk.Disk) (job.DiskFormatParams, disk.TopologyPlan, error) {
	byDev := make(map[string]disk.Disk, len(listed))
	sizes := make(map[string]int64, len(listed))
	for _, d := range listed {
		byDev[d.Device] = d
		sizes[d.Device] = d.Size
	}

	var plan disk.TopologyPlan
	for _, a := range req.Disks {
		assigned, err := assignedFromRequest(a)
		if err != nil {
			return job.DiskFormatParams{}, disk.TopologyPlan{}, err
		}
		if d, ok := byDev[a.Device]; ok {
			if d.Boot {
				return job.DiskFormatParams{}, disk.TopologyPlan{}, errInvalidPlan(fmt.Errorf("disk: refusing to assign the boot device %s", a.Device))
			}
			assigned.WWN = d.WWN
			assigned.Serial = d.Serial
			assigned.WeakIdentity = d.WeakIdentity
			assigned.ByIDName = d.ByIDName
		} else if !disk.IsLoopDevice(a.Device) {
			return job.DiskFormatParams{}, disk.TopologyPlan{}, errUnmanagedDevice(fmt.Errorf("%w: %s", disk.ErrUnmanagedDevice, a.Device))
		}
		switch a.Role {
		case apiv1.ArrayDiskRoleParity:
			plan.Parity = append(plan.Parity, assigned)
		case apiv1.ArrayDiskRoleData:
			plan.Data = append(plan.Data, assigned)
		case apiv1.ArrayDiskRoleCache:
			if plan.Cache != nil {
				return job.DiskFormatParams{}, disk.TopologyPlan{}, errInvalidPlan(errors.New("disk: at most one cache disk can be assigned"))
			}
			c := assigned
			plan.Cache = &c
		default:
			return job.DiskFormatParams{}, disk.TopologyPlan{}, errInvalidPlan(fmt.Errorf("disk: unknown role %q", a.Role))
		}
	}

	params := job.DiskFormatParams{
		Confirmation: req.Confirmation,
		Parity:       plan.Parity,
		Data:         plan.Data,
		Cache:        plan.Cache,
		Sizes:        sizes,
	}
	if v, ok := req.CreatePolicy.Get(); ok {
		params.CreatePolicy = string(v)
	}
	if v, ok := req.MinFreeSpace.Get(); ok {
		params.MinFreeSpace = v
	}
	return params, plan, nil
}

func errArrayNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "array stop/start is not configured on this daemon"}
}

func mapArraySequenceError(err error, failedCode string) error {
	if errors.Is(err, job.ErrStorageNotReady) {
		return &apiError{code: "storage_not_ready", statusCode: 409, message: err.Error()}
	}
	if errors.Is(err, job.ErrDiskUpgradeDataPending) {
		return &apiError{code: "disk_upgrade_pending", statusCode: 409, message: err.Error()}
	}
	if errors.Is(err, job.ErrArrayDiskMismatch) {
		return &apiError{code: "array_disk_mismatch", statusCode: 409, message: err.Error()}
	}
	return &apiError{code: failedCode, statusCode: 409, message: err.Error()}
}

// CurrentArray returns the daemon's current array stop/start sequence.
// Safe for concurrent use with SetArray (#263): main.go's ArrayReady hook
// can replace it from the create-array job's own goroutine, after a live
// array creation, while this runs from a concurrent HTTP request goroutine
// or the daemon's own UPS/update shutdown paths at shutdown time.
func (h *Handler) CurrentArray() *job.ArraySequence {
	h.arrayMu.RLock()
	defer h.arrayMu.RUnlock()
	return h.Array
}

// SetArray replaces the daemon's current array stop/start sequence.
// main.go's ArrayReady hook is the only caller once the daemon is serving
// requests (#263) — every read goes through CurrentArray, never the Array
// field directly, from that point on.
func (h *Handler) SetArray(seq *job.ArraySequence) {
	h.arrayMu.Lock()
	defer h.arrayMu.Unlock()
	h.Array = seq
}

func (h *Handler) StopArray(ctx context.Context, req *apiv1.StopArrayRequest) (*apiv1.SystemStatus, error) {
	if !req.Confirm {
		return nil, errConfirmRequired
	}
	seq := h.CurrentArray()
	if seq == nil {
		return nil, errArrayNotConfigured()
	}
	if h.Scheduler != nil {
		pending, err := h.Scheduler.PendingDiskUpgradeData(ctx)
		if err != nil {
			return nil, err
		}
		if pending != nil {
			return nil, &apiError{code: "disk_upgrade_pending", statusCode: 409, message: fmt.Sprintf("the array is already stopped for data-disk upgrade %s — resume it, or cancel it to abort back to the old disk", pending.ID)}
		}
	}
	if err := seq.Stop(ctx); err != nil {
		return nil, mapArraySequenceError(err, "array_stop_failed")
	}
	return h.GetStatus(ctx)
}

func (h *Handler) StartArray(ctx context.Context) (*apiv1.SystemStatus, error) {
	seq := h.CurrentArray()
	if seq == nil {
		return nil, errArrayNotConfigured()
	}
	if err := seq.Start(ctx); err != nil {
		return nil, mapArraySequenceError(err, "array_start_failed")
	}
	return h.GetStatus(ctx)
}

func errArrayDisksNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "disk add/replace is not configured on this daemon"}
}

func errNoArrayYet() error {
	return &apiError{code: "invalid_plan", statusCode: 400, message: "create the array before adding or replacing a disk"}
}

func errDiskSlotNotFound(mountpoint string) error {
	return &apiError{code: "disk_slot_not_found", statusCode: 404, message: fmt.Sprintf("no data disk at %q", mountpoint)}
}

// currentArrayAndInventory loads the array's current topology and a fresh
// disk inventory in one call — every add/replace plan and apply handler
// below needs both: topology to validate Q19/Q20 and find the mountpoint
// slot being replaced, inventory to resolve the requested device's own
// identity and refuse the boot disk.
func (h *Handler) currentArrayAndInventory(ctx context.Context) ([]store.ArrayDisk, []disk.Disk, error) {
	_, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return nil, nil, errNoArrayYet()
		}
		return nil, nil, err
	}
	listed, err := h.Disks.List(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing disks: %w", err)
	}
	return disks, listed, nil
}

// resolveAssignedDisk builds the AssignedDisk a single-disk add or replace
// operates on: the requested filesystem (default XFS) and adopt flag, plus
// stable identity copied from listed when device is a disk Provider.List
// currently reports — refusing (errInvalidPlan) the boot device, and
// (errUnmanagedDevice) a device that is neither in listed nor a loop
// device (the lab), the same shape diskFormatParamsFromRequest already
// applies per-disk for create-array.
func resolveAssignedDisk(device string, fsOpt apiv1.OptArrayDiskFilesystem, adoptOpt apiv1.OptBool, listed []disk.Disk) (disk.AssignedDisk, error) {
	fs := disk.XFS
	if v, ok := fsOpt.Get(); ok {
		switch v {
		case apiv1.ArrayDiskFilesystemXfs:
			fs = disk.XFS
		case apiv1.ArrayDiskFilesystemExt4:
			fs = disk.EXT4
		case apiv1.ArrayDiskFilesystemBtrfs:
			fs = disk.BTRFS
		default:
			return disk.AssignedDisk{}, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrUnsupportedFilesystem, v))
		}
	}
	assigned := disk.AssignedDisk{Device: device, Filesystem: fs, Adopt: adoptOpt.Or(false)}
	if d, err := disk.LookupDisk(listed, device); err == nil {
		if d.Boot {
			return disk.AssignedDisk{}, errInvalidPlan(fmt.Errorf("disk: refusing to assign the boot device %s", device))
		}
		assigned.WWN = d.WWN
		assigned.Serial = d.Serial
		assigned.WeakIdentity = d.WeakIdentity
		assigned.ByIDName = d.ByIDName
		assigned.FSUUID = d.FSUUID
	} else if !disk.IsLoopDevice(device) {
		return disk.AssignedDisk{}, errUnmanagedDevice(fmt.Errorf("%w: %s", disk.ErrUnmanagedDevice, device))
	}
	return assigned, nil
}

func sizesFromListed(listed []disk.Disk) map[string]int64 {
	sizes := make(map[string]int64, len(listed))
	for _, d := range listed {
		sizes[d.Device] = d.Size
	}
	return sizes
}

func storeMountpoints(disks []store.ArrayDisk) []string {
	out := make([]string, 0, len(disks))
	for _, d := range disks {
		out = append(out, d.Mountpoint)
	}
	return out
}

func filesystemToAPI(fs disk.FilesystemType) apiv1.ArrayDiskFilesystem {
	switch fs {
	case disk.EXT4:
		return apiv1.ArrayDiskFilesystemExt4
	case disk.BTRFS:
		return apiv1.ArrayDiskFilesystemBtrfs
	default:
		return apiv1.ArrayDiskFilesystemXfs
	}
}

// diskIdentityFields fills a plan's own identity display fields (model,
// WWN, serial, size, current filesystem, finding 3 of #288's fix round)
// from listed's matching entry, by device path — the same lookup
// resolveAssignedDisk just used to build assigned. A device with no
// matching inventory entry (a loop device in the lab, doc 06 §3) leaves
// every field unset rather than guessing.
func diskIdentityFields(device string, listed []disk.Disk) (model, wwn, serial apiv1.OptString, size apiv1.OptInt64, currentFS apiv1.OptString) {
	d, err := disk.LookupDisk(listed, device)
	if err != nil {
		return
	}
	if d.Model != "" {
		model = apiv1.NewOptString(d.Model)
	}
	if d.WWN != "" {
		wwn = apiv1.NewOptString(d.WWN)
	}
	if d.Serial != "" {
		serial = apiv1.NewOptString(d.Serial)
	}
	size = apiv1.NewOptInt64(d.Size)
	if d.Filesystem != "" {
		currentFS = apiv1.NewOptString(d.Filesystem)
	}
	return
}

// PlanDiskAdd computes the add plan (doc 02 §4 "Adding a disk"): the
// target's own identity, the next free /mnt/diskN and the exact typed
// confirmation addDisk requires, having already run the same Q19/Q20/Q21
// checks (job.ValidateDiskAddition) addDisk itself re-runs at apply time.
// Read-only — nothing is formatted or persisted.
func (h *Handler) PlanDiskAdd(ctx context.Context, req *apiv1.AddDiskPlanRequest) (*apiv1.AddDiskPlan, error) {
	if h.Disks == nil || h.ArrayStore == nil {
		return nil, errArrayDisksNotConfigured()
	}
	disks, listed, err := h.currentArrayAndInventory(ctx)
	if err != nil {
		return nil, err
	}
	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if err := job.ValidateDiskAddition(disks, assigned, sizesFromListed(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	model, wwn, serial, size, currentFS := diskIdentityFields(assigned.Device, listed)
	return &apiv1.AddDiskPlan{
		Device:            assigned.Device,
		Model:             model,
		Wwn:               wwn,
		Serial:            serial,
		SizeBytes:         size,
		CurrentFilesystem: currentFS,
		Filesystem:        filesystemToAPI(assigned.Filesystem),
		Adopt:             assigned.Adopt,
		Mountpoint:        disk.NextDataMountpoint(storeMountpoints(disks)),
		Confirmation:      job.SingleDiskConfirmation(assigned),
	}, nil
}

// AddDisk queues a Topology job (job.TypeDiskAdd) after re-validating the
// same Q19/Q20/Q21/Q23 checks and typed confirmation planDiskAdd already
// computed — a stale or forged confirmation is refused
// (confirmation_required) before anything is submitted, and the queued
// job re-validates all of this again itself (doc 03 §3.1 step 6, extended
// to this operation).
func (h *Handler) AddDisk(ctx context.Context, req *apiv1.AddDiskRequest) (*apiv1.Job, error) {
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
	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
		return nil, errConfirmRequired
	}
	sizes := sizesFromListed(listed)
	if err := job.ValidateDiskAddition(disks, assigned, sizes); err != nil {
		return nil, errInvalidPlan(err)
	}
	body, err := json.Marshal(job.DiskAddParams{Confirmation: req.Confirmation, Disk: assigned, Sizes: sizes})
	if err != nil {
		return nil, fmt.Errorf("encoding disk_add params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeDiskAdd, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// PlanDiskReplace computes the replace plan (doc 02 §4 "Replacing a
// failed disk"): the replacement's own identity, the SnapRAID fix command
// this slot's own apply call would run and the exact typed confirmation
// replaceDisk requires, having already run the same Q19/Q20/Q21 checks
// (job.ValidateDiskReplacement) replaceDisk itself re-runs at apply time,
// and refused (slot_disk_present) unless the slot's own recorded disk is
// genuinely gone (job.ConfirmReplacementTargetAbsent, doc 02 §4 steps
// 1-2) — a disk that has not actually failed or been removed goes through
// the upgrade flow instead (#289), never replace. Read-only — nothing is
// formatted or persisted.
func (h *Handler) PlanDiskReplace(ctx context.Context, req *apiv1.ReplaceDiskPlanRequest) (*apiv1.ReplaceDiskPlan, error) {
	if h.Disks == nil || h.ArrayStore == nil {
		return nil, errArrayDisksNotConfigured()
	}
	disks, listed, err := h.currentArrayAndInventory(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, req.Mountpoint)
	if err != nil {
		if errors.Is(err, store.ErrArrayDiskNotFound) {
			return nil, errDiskSlotNotFound(req.Mountpoint)
		}
		return nil, err
	}
	if err := job.ConfirmReplacementTargetAbsent(req.Mountpoint, existing, listed); err != nil {
		return nil, errSlotDiskPresent(err)
	}
	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizesFromListed(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	label, err := job.DataDiskLabelForMountpoint(disks, req.Mountpoint)
	if err != nil {
		return nil, errInvalidPlan(err)
	}
	model, wwn, serial, size, currentFS := diskIdentityFields(assigned.Device, listed)
	return &apiv1.ReplaceDiskPlan{
		Mountpoint:        req.Mountpoint,
		PreviousDevice:    existing.Device,
		ReplacementDevice: assigned.Device,
		Model:             model,
		Wwn:               wwn,
		Serial:            serial,
		SizeBytes:         size,
		CurrentFilesystem: currentFS,
		Filesystem:        filesystemToAPI(assigned.Filesystem),
		Adopt:             assigned.Adopt,
		Rebuild:           fmt.Sprintf("snapraid fix -d %s", label),
		Confirmation:      job.SingleDiskConfirmation(assigned),
	}, nil
}

// ReplaceDisk queues a Topology job (job.TypeDiskReplace) after
// re-validating the same Q19/Q20/Q21 checks, the slot-disk-absent check
// and typed confirmation planDiskReplace already computed — a stale or
// forged confirmation is refused (confirmation_required) before anything
// is submitted, and the queued job re-validates all of this again itself.
func (h *Handler) ReplaceDisk(ctx context.Context, req *apiv1.ReplaceDiskRequest) (*apiv1.Job, error) {
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
	existing, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, req.Mountpoint)
	if err != nil {
		if errors.Is(err, store.ErrArrayDiskNotFound) {
			return nil, errDiskSlotNotFound(req.Mountpoint)
		}
		return nil, err
	}
	if err := job.ConfirmReplacementTargetAbsent(req.Mountpoint, existing, listed); err != nil {
		return nil, errSlotDiskPresent(err)
	}
	assigned, err := resolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
		return nil, errConfirmRequired
	}
	sizes := sizesFromListed(listed)
	if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizes); err != nil {
		return nil, errInvalidPlan(err)
	}
	body, err := json.Marshal(job.DiskReplaceParams{Confirmation: req.Confirmation, Mountpoint: req.Mountpoint, Disk: assigned, Sizes: sizes})
	if err != nil {
		return nil, fmt.Errorf("encoding disk_replace params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeDiskReplace, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func errDiskRemovalNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "disk removal is not configured on this daemon — it needs the array's parity engine"}
}

func errDiskNotEvacuated(mountpoint, state string) error {
	if state == "" {
		state = "not in removal"
	}
	return &apiError{code: "disk_not_evacuated", statusCode: 409, message: fmt.Sprintf("disk %s is %s — evacuate it before finishing its removal", mountpoint, state)}
}

// FinishDiskRemoval queues a Topology job (job.TypeDiskRemove) that
// finishes removing an evacuated data disk (doc 09 §4 steps 7-9, #358).
// It refuses synchronously, in this order: no parity engine (501), no
// data disk at the slot (404), a disk not evacuated or further along
// (409 disk_not_evacuated), a wrong confirmation (409). The job checks
// every one of these again itself, and everything else it needs.
func (h *Handler) FinishDiskRemoval(ctx context.Context, req *apiv1.FinishDiskRemovalRequest) (*apiv1.Job, error) {
	if engine, _, _, _ := h.CurrentParity(); engine == nil || h.ArrayStore == nil {
		return nil, errDiskRemovalNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	existing, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, req.Mountpoint)
	if err != nil {
		if errors.Is(err, store.ErrArrayDiskNotFound) {
			return nil, errDiskSlotNotFound(req.Mountpoint)
		}
		return nil, fmt.Errorf("finish disk removal: loading %s: %w", req.Mountpoint, err)
	}
	switch existing.RemovalState {
	case store.RemovalStateEvacuated, store.RemovalStateUnpooled, store.RemovalStateUnlisted:
	default:
		return nil, errDiskNotEvacuated(req.Mountpoint, existing.RemovalState)
	}
	if req.Confirmation == "" || job.EvacuationConfirmation(req.Mountpoint) != req.Confirmation {
		return nil, errConfirmRequired
	}
	body, err := json.Marshal(job.DiskRemoveParams{Mountpoint: req.Mountpoint, Confirmation: req.Confirmation})
	if err != nil {
		return nil, fmt.Errorf("encoding disk_remove params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeDiskRemove, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func assignedFromRequest(a apiv1.ArrayDiskAssignment) (disk.AssignedDisk, error) {
	fs := disk.XFS
	if v, ok := a.Filesystem.Get(); ok {
		switch v {
		case apiv1.ArrayDiskFilesystemXfs:
			fs = disk.XFS
		case apiv1.ArrayDiskFilesystemExt4:
			fs = disk.EXT4
		case apiv1.ArrayDiskFilesystemBtrfs:
			fs = disk.BTRFS
		default:
			return disk.AssignedDisk{}, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrUnsupportedFilesystem, v))
		}
	}
	return disk.AssignedDisk{
		Device:     a.Device,
		Filesystem: fs,
		Adopt:      a.Adopt.Or(false),
	}, nil
}
