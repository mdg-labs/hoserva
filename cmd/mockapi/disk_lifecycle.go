package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mockArrayDisks is the array topology mockDiskInventory's non-external
// devices already stand in for (mockPoolStatus's own device/role/
// mountpoint assignments, doc 02 §4), so planDiskAdd/planDiskReplace and
// their apply operations validate against the same shape the pool page
// already shows for this scenario. fresh-install has no array yet. Every
// present disk's serial matches its own mockDiskInventory entry, so
// job.ConfirmReplacementTargetAbsent's own identity check recognises it as
// still present the same way production's would. The degraded scenario's
// disk4 (mirroring mockPoolStatus's own #326 "missing" entry) carries no
// serial and no matching inventory entry at all — genuinely absent, the
// one slot this scenario's own `disk replace` demo can actually replace.
func mockArrayDisks(scenario string) []store.ArrayDisk {
	if scenario == "fresh-install" {
		return nil
	}
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", Serial: "WD-WCC4E1234567", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", Serial: "WD-WCC4E7654321", Mountpoint: "/mnt/disk2"},
		{Role: store.ArrayRoleData, RoleIndex: 3, Device: "/dev/sdd", Filesystem: "xfs", Serial: "WD-WCC4E9999999", Mountpoint: "/mnt/disk3"},
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sde", Filesystem: "xfs", Serial: "WD-WCC7E0000001", Mountpoint: "/mnt/parity"},
	}
	if scenario == "degraded" {
		disks = append(disks, store.ArrayDisk{Role: store.ArrayRoleData, RoleIndex: 4, Device: "/dev/sdx", Filesystem: "xfs", Mountpoint: "/mnt/disk4"})
		// disk3 is mid-evacuation in this scenario (#359, doc 09 §4 step
		// 2), the same way disk2 (below) is already marked failed —
		// PlanDiskEvacuation/EvacuateDisk mirror production's own
		// disk_removal_in_progress refusal for every mountpoint but this
		// one.
		disks[2].RemovalState = store.RemovalStateEvacuating
	}
	return disks
}

func errNoArrayYet() error {
	return &mockError{code: "invalid_plan", statusCode: 400, message: "create the array before adding or replacing a disk"}
}

func errDiskSlotNotFound(mountpoint string) error {
	return &mockError{code: "disk_slot_not_found", statusCode: 404, message: fmt.Sprintf("no data disk at %q", mountpoint)}
}

func errUnmanagedDevice(err error) error {
	return &mockError{code: "unmanaged_device", statusCode: 400, message: err.Error()}
}

func errSlotDiskPresent(err error) error {
	return &mockError{code: "slot_disk_present", statusCode: 409, message: err.Error()}
}

func errArrayNotStopped() error {
	return &mockError{code: "array_not_stopped", statusCode: 409, message: "stop the array first (\"hoserva array stop\") — a data-disk upgrade runs only once the array's stop sequence has completed"}
}

// errDiskUpgradePending mirrors production's disk_upgrade_pending refusal
// of a second data-disk upgrade, `array start` and `array stop` while one
// is pending (doc 02 §4 E6, E7, E8).
func errDiskUpgradePending(id uuid.UUID) error {
	return &mockError{code: "disk_upgrade_pending", statusCode: 409, message: fmt.Sprintf("data-disk upgrade %s is pending — resume it, or cancel it to abort back to the old disk", id)}
}

// pendingDiskUpgradeLocked returns a data-disk upgrade job that has not
// ended, as production's Scheduler.PendingDiskUpgradeData does. Callers
// hold h.mu.
func (h *handler) pendingDiskUpgradeLocked() (uuid.UUID, bool) {
	for id, j := range h.jobs {
		if j.Type != apiv1.JobTypeDiskUpgradeData {
			continue
		}
		switch j.Status {
		case apiv1.JobStatusQueued, apiv1.JobStatusRunning, apiv1.JobStatusInterrupted:
			return id, true
		}
	}
	return uuid.UUID{}, false
}

// errMaintenanceMode mirrors internal/api's mapSchedulerError for
// job.ErrMaintenanceMode: production's Scheduler.Submit refuses a
// parity-disk upgrade, like every job but a data-disk upgrade, with this
// code, status and message while the array is stopped.
func errMaintenanceMode() error {
	return &mockError{code: "maintenance_mode", statusCode: 409, message: "maintenance mode is active — no new jobs are accepted"}
}

// mockResolveAssignedDisk mirrors internal/api's resolveAssignedDisk
// against this mock's own fixed inventory (#288): same filesystem default,
// same boot-device and unmanaged-device refusals, so a client exercising
// this mock sees the same shape of refusal production would give it.
func mockResolveAssignedDisk(device string, fsOpt apiv1.OptArrayDiskFilesystem, adoptOpt apiv1.OptBool, listed []apiv1.DiskInventoryEntry) (disk.AssignedDisk, error) {
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
	found := false
	for _, d := range listed {
		if d.Device != device {
			continue
		}
		found = true
		if d.Boot {
			return disk.AssignedDisk{}, errInvalidPlan(fmt.Errorf("disk: refusing to assign the boot device %s", device))
		}
		assigned.WWN = d.Wwn.Or("")
		assigned.Serial = d.Serial.Or("")
		assigned.WeakIdentity = d.WeakIdentity.Or(false)
		break
	}
	if !found && !disk.IsLoopDevice(device) {
		return disk.AssignedDisk{}, errUnmanagedDevice(fmt.Errorf("%w: %s", disk.ErrUnmanagedDevice, device))
	}
	return assigned, nil
}

func mockSizes(listed []apiv1.DiskInventoryEntry) map[string]int64 {
	sizes := make(map[string]int64, len(listed))
	for _, d := range listed {
		sizes[d.Device] = d.SizeBytes
	}
	return sizes
}

func mockStoreMountpoints(disks []store.ArrayDisk) []string {
	out := make([]string, 0, len(disks))
	for _, d := range disks {
		out = append(out, d.Mountpoint)
	}
	return out
}

func mockFilesystemToAPI(fs disk.FilesystemType) apiv1.ArrayDiskFilesystem {
	switch fs {
	case disk.EXT4:
		return apiv1.ArrayDiskFilesystemExt4
	case disk.BTRFS:
		return apiv1.ArrayDiskFilesystemBtrfs
	default:
		return apiv1.ArrayDiskFilesystemXfs
	}
}

// mockInventoryAsDisks converts listed to the []disk.Disk shape
// job.ConfirmReplacementTargetAbsent compares stored identity against —
// reusing the identical check production's PlanDiskReplace/ReplaceDisk
// run, not a second one.
func mockInventoryAsDisks(listed []apiv1.DiskInventoryEntry) []disk.Disk {
	out := make([]disk.Disk, 0, len(listed))
	for _, d := range listed {
		out = append(out, disk.Disk{
			Device:       d.Device,
			WWN:          d.Wwn.Or(""),
			Serial:       d.Serial.Or(""),
			WeakIdentity: d.WeakIdentity.Or(false),
		})
	}
	return out
}

// mockIdentityFields mirrors internal/api's diskIdentityFields against
// this mock's own fixed inventory, for AddDiskPlan/ReplaceDiskPlan's
// display fields (finding 3).
func mockIdentityFields(device string, listed []apiv1.DiskInventoryEntry) (model, wwn, serial apiv1.OptString, size apiv1.OptInt64, currentFS apiv1.OptString) {
	for _, d := range listed {
		if d.Device != device {
			continue
		}
		model = d.Model
		wwn = d.Wwn
		serial = d.Serial
		size = apiv1.NewOptInt64(d.SizeBytes)
		currentFS = d.Filesystem
		return
	}
	return
}

func (h *handler) mockArrayState() ([]store.ArrayDisk, []apiv1.DiskInventoryEntry, error) {
	disks := mockArrayDisks(h.scenario)
	if disks == nil {
		return nil, nil, errNoArrayYet()
	}
	return disks, mockDiskInventory(h.scenario), nil
}

func (h *handler) PlanDiskAdd(ctx context.Context, req *apiv1.AddDiskPlanRequest) (*apiv1.AddDiskPlan, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if err := job.ValidateDiskAddition(disks, assigned, mockSizes(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	model, wwn, serial, size, currentFS := mockIdentityFields(assigned.Device, listed)
	return &apiv1.AddDiskPlan{
		Device:            assigned.Device,
		Model:             model,
		Wwn:               wwn,
		Serial:            serial,
		SizeBytes:         size,
		CurrentFilesystem: currentFS,
		Filesystem:        mockFilesystemToAPI(assigned.Filesystem),
		Adopt:             assigned.Adopt,
		Mountpoint:        disk.NextDataMountpoint(mockStoreMountpoints(disks)),
		Confirmation:      job.SingleDiskConfirmation(assigned),
	}, nil
}

func (h *handler) AddDisk(ctx context.Context, req *apiv1.AddDiskRequest) (*apiv1.Job, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
		return nil, errConfirmRequired()
	}
	if err := job.ValidateDiskAddition(disks, assigned, mockSizes(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:        uuid.New(),
		Type:      apiv1.JobTypeDiskAdd,
		Class:     apiv1.JobClassTopology,
		Status:    apiv1.JobStatusQueued,
		Resumable: false,
		CreatedAt: now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}

func (h *handler) PlanDiskReplace(ctx context.Context, req *apiv1.ReplaceDiskPlanRequest) (*apiv1.ReplaceDiskPlan, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	existing, ok := mockDataDiskAt(disks, req.Mountpoint)
	if !ok {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if err := job.ConfirmReplacementTargetAbsent(req.Mountpoint, existing, mockInventoryAsDisks(listed)); err != nil {
		return nil, errSlotDiskPresent(err)
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, mockSizes(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	label, err := job.DataDiskLabelForMountpoint(disks, req.Mountpoint)
	if err != nil {
		return nil, errInvalidPlan(err)
	}
	model, wwn, serial, size, currentFS := mockIdentityFields(assigned.Device, listed)
	return &apiv1.ReplaceDiskPlan{
		Mountpoint:        req.Mountpoint,
		PreviousDevice:    existing.Device,
		ReplacementDevice: assigned.Device,
		Model:             model,
		Wwn:               wwn,
		Serial:            serial,
		SizeBytes:         size,
		CurrentFilesystem: currentFS,
		Filesystem:        mockFilesystemToAPI(assigned.Filesystem),
		Adopt:             assigned.Adopt,
		Rebuild:           fmt.Sprintf("snapraid fix -d %s", label),
		Confirmation:      job.SingleDiskConfirmation(assigned),
	}, nil
}

func (h *handler) ReplaceDisk(ctx context.Context, req *apiv1.ReplaceDiskRequest) (*apiv1.Job, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	existing, ok := mockDataDiskAt(disks, req.Mountpoint)
	if !ok {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if err := job.ConfirmReplacementTargetAbsent(req.Mountpoint, existing, mockInventoryAsDisks(listed)); err != nil {
		return nil, errSlotDiskPresent(err)
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, req.Adopt, listed)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
		return nil, errConfirmRequired()
	}
	if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, mockSizes(listed)); err != nil {
		return nil, errInvalidPlan(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:        uuid.New(),
		Type:      apiv1.JobTypeDiskReplace,
		Class:     apiv1.JobClassTopology,
		Status:    apiv1.JobStatusQueued,
		Resumable: false,
		CreatedAt: now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}

func mockDataDiskAt(disks []store.ArrayDisk, mountpoint string) (store.ArrayDisk, bool) {
	for _, d := range disks {
		if d.Role == store.ArrayRoleData && d.Mountpoint == mountpoint {
			return d, true
		}
	}
	return store.ArrayDisk{}, false
}

// mockArrayDiskAt mirrors internal/api's arrayDiskAtMountpoint against
// this mock's own fixed inventory: a disk upgrade (#289) targets a parity
// slot exactly as often as a data one, unlike mockDataDiskAt's own
// data-only lookup.
func mockArrayDiskAt(disks []store.ArrayDisk, mountpoint string) (store.ArrayDisk, bool) {
	for _, d := range disks {
		if d.Mountpoint == mountpoint {
			return d, true
		}
	}
	return store.ArrayDisk{}, false
}

var mockDataDiskUpgradeSteps = []string{
	"with the array stopped, confirm snapraid diff has nothing to sync",
	"format the new disk",
	"copy the old disk's files to the new one, preserving ownership, xattrs and timestamps",
	"verify the copy against the old disk",
	"mount the new disk at the same mountpoint",
	"run snapraid diff and release the old disk once it reports nothing removed or updated",
	"leave the array stopped until you start it",
}

var mockParityDiskUpgradeSteps = []string{
	"copy the parity file to the new disk",
	"verify the copy byte for byte",
	"switch the configuration to the new parity disk",
	"run snapraid check against it",
	"release the old parity disk once it passes",
}

func mockArrayDiskRoleToAPI(role string) apiv1.ArrayDiskRole {
	switch role {
	case store.ArrayRoleParity:
		return apiv1.ArrayDiskRoleParity
	case store.ArrayRoleData:
		return apiv1.ArrayDiskRoleData
	default:
		return apiv1.ArrayDiskRoleCache
	}
}

func mockParityBytesFrom(disks []store.ArrayDisk, sizes map[string]int64) []int64 {
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

// PlanDiskUpgrade mirrors internal/api's own PlanDiskUpgrade (doc 02 §4
// "Larger data disk"/"Larger parity disk", #289) against this mock's
// fixed inventory, with production's own validation.
func (h *handler) PlanDiskUpgrade(ctx context.Context, req *apiv1.DiskUpgradePlanRequest) (*apiv1.DiskUpgradePlan, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	existing, ok := mockArrayDiskAt(disks, req.Mountpoint)
	if !ok || (existing.Role != store.ArrayRoleData && existing.Role != store.ArrayRoleParity) {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, apiv1.OptBool{}, listed)
	if err != nil {
		return nil, err
	}
	sizes := mockSizes(listed)

	plan := &apiv1.DiskUpgradePlan{
		Mountpoint:        req.Mountpoint,
		Role:              mockArrayDiskRoleToAPI(existing.Role),
		PreviousDevice:    existing.Device,
		ReplacementDevice: assigned.Device,
	}
	switch existing.Role {
	case store.ArrayRoleData:
		if disk.DataDiskUpgradeExceedsParity(sizes[assigned.Device], mockParityBytesFrom(disks, sizes)) {
			return nil, errInvalidPlan(job.ErrDataDiskUpgradeExceedsParity)
		}
		if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		plan.Steps = mockDataDiskUpgradeSteps
	case store.ArrayRoleParity:
		assigned.Filesystem = disk.XFS
		plan.ReplacementDevice = assigned.Device
		if err := job.ValidateParityDiskUpgrade(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		plan.Steps = mockParityDiskUpgradeSteps
		plan.NewMountpoint = apiv1.NewOptString(disk.NextParityMountpoint(mockStoreMountpoints(disks)))
	}

	model, wwn, serial, size, currentFS := mockIdentityFields(assigned.Device, listed)
	plan.Model = model
	plan.Wwn = wwn
	plan.Serial = serial
	plan.SizeBytes = size
	plan.CurrentFilesystem = currentFS
	plan.Filesystem = mockFilesystemToAPI(assigned.Filesystem)
	plan.Confirmation = job.SingleDiskConfirmation(assigned)
	return plan, nil
}

// UpgradeDisk mirrors internal/api's own UpgradeDisk against this mock's
// fixed inventory, with production's own validation.
func (h *handler) UpgradeDisk(ctx context.Context, req *apiv1.UpgradeDiskRequest) (*apiv1.Job, error) {
	disks, listed, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	existing, ok := mockArrayDiskAt(disks, req.Mountpoint)
	if !ok || (existing.Role != store.ArrayRoleData && existing.Role != store.ArrayRoleParity) {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	assigned, err := mockResolveAssignedDisk(req.Device, req.Filesystem, apiv1.OptBool{}, listed)
	if err != nil {
		return nil, err
	}
	sizes := mockSizes(listed)

	var jobType apiv1.JobType
	switch existing.Role {
	case store.ArrayRoleData:
		if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
			return nil, errConfirmRequired()
		}
		if disk.DataDiskUpgradeExceedsParity(sizes[assigned.Device], mockParityBytesFrom(disks, sizes)) {
			return nil, errInvalidPlan(job.ErrDataDiskUpgradeExceedsParity)
		}
		if err := job.ValidateDiskReplacement(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		// Production's Scheduler.Submit: a pending upgrade refuses a second
		// one, and only a completed array stop admits it (doc 02 §4 E8).
		h.mu.Lock()
		pendingID, pending := h.pendingDiskUpgradeLocked()
		stopped := h.maintenance
		h.mu.Unlock()
		if pending {
			return nil, errDiskUpgradePending(pendingID)
		}
		if !stopped {
			return nil, errArrayNotStopped()
		}
		jobType = apiv1.JobTypeDiskUpgradeData
	case store.ArrayRoleParity:
		assigned.Filesystem = disk.XFS
		if req.Confirmation == "" || job.SingleDiskConfirmation(assigned) != req.Confirmation {
			return nil, errConfirmRequired()
		}
		if err := job.ValidateParityDiskUpgrade(disks, req.Mountpoint, assigned, sizes); err != nil {
			return nil, errInvalidPlan(err)
		}
		newMountpoint, ok := req.NewMountpoint.Get()
		wantMountpoint := disk.NextParityMountpoint(mockStoreMountpoints(disks))
		if !ok || newMountpoint == "" || newMountpoint != wantMountpoint {
			return nil, errInvalidPlan(fmt.Errorf("disk: newMountpoint must be %q, the matching plan's own value", wantMountpoint))
		}
		// A parity-disk upgrade is refused in maintenance mode like any job
		// other than a data-disk upgrade (doc 02 §4 E8).
		h.mu.Lock()
		inMaintenance := h.maintenance
		h.mu.Unlock()
		if inMaintenance {
			return nil, errMaintenanceMode()
		}
		jobType = apiv1.JobTypeDiskUpgradeParity
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:          uuid.New(),
		Type:        jobType,
		Class:       apiv1.JobClassTopology,
		Status:      apiv1.JobStatusQueued,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}

// FinishDiskRemoval mirrors internal/api's own FinishDiskRemoval and its
// validation order against this mock's fixed array: not_configured with
// no array (production has no parity engine then), disk_slot_not_found,
// disk_not_evacuated unless the disk is evacuated or further along, then
// confirmation_required (job.EvacuationConfirmation). No scenario has an
// evacuated disk, so every scenario refuses.
func (h *handler) FinishDiskRemoval(ctx context.Context, req *apiv1.FinishDiskRemovalRequest) (*apiv1.Job, error) {
	disks := mockArrayDisks(h.scenario)
	if disks == nil {
		return nil, &mockError{code: "not_configured", statusCode: 501, message: "disk removal is not configured on this daemon — it needs the array's parity engine"}
	}
	d, ok := mockDataDiskAt(disks, req.Mountpoint)
	if !ok {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	switch d.RemovalState {
	case store.RemovalStateEvacuated, store.RemovalStateUnpooled, store.RemovalStateUnlisted:
	default:
		state := d.RemovalState
		if state == "" {
			state = "not in removal"
		}
		return nil, &mockError{code: "disk_not_evacuated", statusCode: 409, message: fmt.Sprintf("disk %s is %s — evacuate it before finishing its removal", req.Mountpoint, state)}
	}
	if req.Confirmation == "" || job.EvacuationConfirmation(req.Mountpoint) != req.Confirmation {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	j := apiv1.Job{
		ID:        uuid.New(),
		Type:      apiv1.JobTypeDiskRemove,
		Class:     apiv1.JobClassTopology,
		Status:    apiv1.JobStatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	h.jobs[j.ID] = j
	return &j, nil
}
