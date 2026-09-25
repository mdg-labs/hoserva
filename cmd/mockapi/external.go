package main

import (
	"context"
	"fmt"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
)

const mockExternalDevice = "/dev/sdf"
const mockExternalLabel = "backup"

func mockUSBDisk() apiv1.DiskInventoryEntry {
	return apiv1.DiskInventoryEntry{
		Device:       mockExternalDevice,
		SizeBytes:    mockDiskSize,
		Model:        apiv1.NewOptString("Samsung Portable SSD"),
		Serial:       apiv1.NewOptString("S5GWNG0N123456"),
		Filesystem:   apiv1.NewOptString("xfs"),
		Label:        apiv1.NewOptString(mockExternalLabel),
		SmartStatus:  apiv1.NewOptString("ok"),
		ContainsData: apiv1.NewOptBool(true),
	}
}

func defaultMockExternal() apiv1.ExternalDisk {
	return apiv1.ExternalDisk{
		Label:             mockExternalLabel,
		Device:            mockExternalDevice,
		MountPoint:        "/mnt/disks/" + mockExternalLabel,
		ContainerPath:     "/mnt/disks/" + mockExternalLabel,
		Mounted:           false,
		BackupDestination: false,
		Boot:              false,
		Filesystem:        apiv1.NewOptString("xfs"),
		FsUuid:            apiv1.NewOptString("uuid-ext-backup"),
		SizeBytes:         apiv1.NewOptNilInt64(mockDiskSize),
		Model:             apiv1.NewOptString("Samsung Portable SSD"),
		Serial:            apiv1.NewOptString("S5GWNG0N123456"),
	}
}

func (h *handler) getExternal(label string) (apiv1.ExternalDisk, error) {
	h.externalMu.Lock()
	defer h.externalMu.Unlock()
	if d, ok := h.external[label]; ok {
		return d, nil
	}
	if label != mockExternalLabel {
		return apiv1.ExternalDisk{}, &mockError{code: "external_disk_not_found", statusCode: 404, message: fmt.Sprintf("no external disk %q", label)}
	}
	d := defaultMockExternal()
	h.external[label] = d
	return d, nil
}

func (h *handler) putExternal(d apiv1.ExternalDisk) {
	h.externalMu.Lock()
	defer h.externalMu.Unlock()
	h.external[string(d.Label)] = d
}

func (h *handler) ListExternalDisks(ctx context.Context) (*apiv1.ListExternalDisksOK, error) {
	d, err := h.getExternal(mockExternalLabel)
	if err != nil {
		return nil, err
	}
	return &apiv1.ListExternalDisksOK{Disks: []apiv1.ExternalDisk{d}}, nil
}

func (h *handler) RegisterExternalDisk(ctx context.Context, req *apiv1.RegisterExternalDiskRequest) (*apiv1.ExternalDisk, error) {
	label := string(req.Label)
	if err := disk.ValidateExternalLabel(label); err != nil {
		return nil, errInvalidPlan(err)
	}
	// mirrors internal/api's own RegisterExternalDisk/RefuseBootDevice
	// (external_handler.go, disk/external.go), in the same order: a
	// device this mock's own inventory (mockDiskInventory) reports as
	// the boot disk is refused first; one already holding an array role
	// (mockArrayDisks) is refused next; one it has never heard of, and
	// that isn't a loop device, is refused as unmanaged — none of the
	// three silently registered.
	found := false
	for _, e := range mockDiskInventory(h.scenario) {
		if e.Device != req.Device {
			continue
		}
		found = true
		if e.Boot {
			return nil, errInvalidPlan(fmt.Errorf("%s: %w", req.Device, disk.ErrBootDevice))
		}
		break
	}
	for _, d := range mockArrayDisks(h.scenario) {
		if d.Device == req.Device {
			return nil, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrExternalInArray, req.Device))
		}
	}
	if !found && !disk.IsLoopDevice(req.Device) {
		return nil, errUnmanagedDevice(fmt.Errorf("%w: %s", disk.ErrUnmanagedDevice, req.Device))
	}
	d := defaultMockExternal()
	d.Label = req.Label
	d.Device = req.Device
	d.MountPoint = "/mnt/disks/" + label
	d.ContainerPath = d.MountPoint
	d.BackupDestination = req.BackupDestination.Or(false)
	h.putExternal(d)
	return &d, nil
}

func (h *handler) UpdateExternalDisk(ctx context.Context, req *apiv1.UpdateExternalDiskRequest, params apiv1.UpdateExternalDiskParams) (*apiv1.ExternalDisk, error) {
	d, err := h.getExternal(string(params.Label))
	if err != nil {
		return nil, err
	}
	if v, ok := req.BackupDestination.Get(); ok {
		d.BackupDestination = v
		h.putExternal(d)
	}
	return &d, nil
}

func (h *handler) MountExternalDisk(ctx context.Context, params apiv1.MountExternalDiskParams) (*apiv1.ExternalDisk, error) {
	d, err := h.getExternal(string(params.Label))
	if err != nil {
		return nil, err
	}
	d.Mounted = true
	h.putExternal(d)
	return &d, nil
}

func (h *handler) EjectExternalDisk(ctx context.Context, params apiv1.EjectExternalDiskParams) (*apiv1.ExternalDisk, error) {
	d, err := h.getExternal(string(params.Label))
	if err != nil {
		return nil, err
	}
	d.Mounted = false
	h.putExternal(d)
	return &d, nil
}

func (h *handler) FormatExternalDisk(ctx context.Context, req *apiv1.FormatExternalDiskRequest, params apiv1.FormatExternalDiskParams) (*apiv1.ExternalDisk, error) {
	d, err := h.getExternal(string(params.Label))
	if err != nil {
		return nil, err
	}
	plan := disk.ExternalFormatPlan(disk.AssignedDisk{Device: d.Device, Filesystem: disk.XFS})
	if req.Confirmation == "" || plan.CheckConfirmation(req.Confirmation) != nil {
		return nil, errConfirmRequired()
	}
	return &d, nil
}
