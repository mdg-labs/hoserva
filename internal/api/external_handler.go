package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errExternalNotFound(label string) error {
	return &apiError{code: "external_disk_not_found", statusCode: 404, message: fmt.Sprintf("no external disk %q", label)}
}

func errExternalExists(err error) error {
	return &apiError{code: "external_disk_exists", statusCode: 409, message: err.Error()}
}

func (h *Handler) diskRunner() disk.Runner {
	if h.DiskRunner != nil {
		return h.DiskRunner
	}
	return disk.CommandRunner{}
}

func (h *Handler) diskMounter() disk.UnitMounter {
	if h.DiskMounter != nil {
		return h.DiskMounter
	}
	return disk.DirectMounter{Runner: h.diskRunner()}
}

func (h *Handler) externalStore() *store.ExternalStore {
	if h.ArrayStore == nil {
		return nil
	}
	return h.ArrayStore.External()
}

func (h *Handler) arrayDevices(ctx context.Context) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if h.ArrayStore == nil {
		return out, nil
	}
	_, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil {
		if errors.Is(err, store.ErrNoArray) {
			return out, nil
		}
		return nil, err
	}
	for _, d := range disks {
		out[d.Device] = struct{}{}
	}
	return out, nil
}

func (h *Handler) ListExternalDisks(ctx context.Context) (*apiv1.ListExternalDisksOK, error) {
	ext := h.externalStore()
	var registered []store.ExternalDisk
	if ext != nil {
		var err error
		registered, err = ext.ListExternalDisks(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing external disks: %w", err)
		}
	}

	arrayDevs, err := h.arrayDevices(ctx)
	if err != nil {
		return nil, err
	}

	var inventory []disk.Disk
	if h.Disks != nil {
		inventory, err = h.Disks.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing disks: %w", err)
		}
	}

	byDevice := make(map[string]disk.Disk, len(inventory))
	for _, d := range inventory {
		byDevice[d.Device] = d
	}

	seen := map[string]struct{}{}
	out := make([]apiv1.ExternalDisk, 0, len(registered)+len(inventory))
	for _, row := range registered {
		seen[row.Device] = struct{}{}
		out = append(out, externalToAPI(row, byDevice[row.Device]))
	}
	for _, d := range inventory {
		if d.Boot {
			continue
		}
		if _, inArray := arrayDevs[d.Device]; inArray {
			continue
		}
		if _, already := seen[d.Device]; already {
			continue
		}
		if disk.ValidateExternalLabel(d.Label) != nil {
			continue
		}
		row := store.ExternalDisk{
			Label:      d.Label,
			Device:     d.Device,
			Filesystem: d.Filesystem,
			FSUUID:     d.FSUUID,
			Mountpoint: mustExternalMount(d.Label),
		}
		out = append(out, externalToAPI(row, d))
	}
	return &apiv1.ListExternalDisksOK{Disks: out}, nil
}

func (h *Handler) RegisterExternalDisk(ctx context.Context, req *apiv1.RegisterExternalDiskRequest) (*apiv1.ExternalDisk, error) {
	ext := h.externalStore()
	if ext == nil || h.Disks == nil {
		return nil, errExternalNotConfigured()
	}
	label := string(req.Label)
	if err := disk.ValidateExternalLabel(label); err != nil {
		return nil, errInvalidPlan(err)
	}
	if err := disk.RefuseBootDevice(ctx, h.Disks, req.Device); err != nil {
		return nil, errInvalidPlan(err)
	}
	arrayDevs, err := h.arrayDevices(ctx)
	if err != nil {
		return nil, err
	}
	if _, inArray := arrayDevs[req.Device]; inArray {
		return nil, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrExternalInArray, req.Device))
	}

	listed, err := h.Disks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing disks: %w", err)
	}
	inv, err := disk.LookupDisk(listed, req.Device)
	if err != nil {
		if !disk.IsLoopDevice(req.Device) {
			return nil, errUnmanagedDevice(fmt.Errorf("%w: %s", disk.ErrUnmanagedDevice, req.Device))
		}
		inv = disk.Disk{Device: req.Device}
	}

	fsUUID := pendingExternalUUID(label)
	fs := inv.Filesystem
	if inv.FSUUID != "" {
		fsUUID = inv.FSUUID
	} else if inv.Filesystem != "" {
		if uuid, uuidErr := disk.FilesystemUUID(ctx, h.diskRunner(), req.Device); uuidErr == nil {
			fsUUID = uuid
		}
	}

	mountpoint, err := disk.ExternalMountPoint(label)
	if err != nil {
		return nil, errInvalidPlan(err)
	}
	row := store.ExternalDisk{
		Label:             label,
		Device:            req.Device,
		Filesystem:        fs,
		FSUUID:            fsUUID,
		WWN:               inv.WWN,
		Serial:            inv.Serial,
		ByIDName:          inv.ByIDName,
		WeakIdentity:      inv.WeakIdentity,
		Mountpoint:        mountpoint,
		BackupDestination: req.BackupDestination.Or(false),
	}
	if err := ext.PutExternalDisk(ctx, row); err != nil {
		if errors.Is(err, store.ErrExternalExists) {
			return nil, errExternalExists(err)
		}
		return nil, err
	}
	apiDisk := externalToAPI(row, inv)
	return &apiDisk, nil
}

func (h *Handler) UpdateExternalDisk(ctx context.Context, req *apiv1.UpdateExternalDiskRequest, params apiv1.UpdateExternalDiskParams) (*apiv1.ExternalDisk, error) {
	row, _, err := h.externalRow(ctx, string(params.Label))
	if err != nil {
		return nil, err
	}
	if v, ok := req.BackupDestination.Get(); ok {
		if err := h.externalStore().SetBackupDestination(ctx, row.Label, v); err != nil {
			return nil, err
		}
		row.BackupDestination = v
	}
	return h.externalAPI(ctx, row)
}

func (h *Handler) MountExternalDisk(ctx context.Context, params apiv1.MountExternalDiskParams) (*apiv1.ExternalDisk, error) {
	row, inv, err := h.externalRow(ctx, string(params.Label))
	if err != nil {
		return nil, err
	}
	if err := disk.RefuseBootDevice(ctx, h.Disks, row.Device); err != nil {
		return nil, errInvalidPlan(err)
	}
	if isPendingUUID(row.FSUUID) {
		return nil, errInvalidPlan(fmt.Errorf("disk: format %q before mounting", row.Label))
	}
	unit, err := disk.ExternalMountUnit(row.Label, row.FSUUID, disk.FilesystemType(row.Filesystem))
	if err != nil {
		return nil, errInvalidPlan(err)
	}
	if unit.Filesystem == "" {
		unit.Filesystem = disk.XFS
	}
	if err := disk.MountExternal(ctx, h.diskMounter(), unit); err != nil {
		return nil, fmt.Errorf("mounting external disk %s: %w", row.Label, err)
	}
	apiDisk := externalToAPI(row, inv)
	apiDisk.Mounted = true
	return &apiDisk, nil
}

func (h *Handler) EjectExternalDisk(ctx context.Context, params apiv1.EjectExternalDiskParams) (*apiv1.ExternalDisk, error) {
	row, inv, err := h.externalRow(ctx, string(params.Label))
	if err != nil {
		return nil, err
	}
	unit, err := disk.ExternalMountUnit(row.Label, row.FSUUID, disk.FilesystemType(row.Filesystem))
	if err != nil {
		if isPendingUUID(row.FSUUID) {
			unit = disk.MountUnit{Where: row.Mountpoint}
		} else {
			return nil, errInvalidPlan(err)
		}
	}
	if err := disk.EjectExternal(ctx, h.diskMounter(), h.Disks, unit, row.Device); err != nil {
		return nil, fmt.Errorf("ejecting external disk %s: %w", row.Label, err)
	}
	apiDisk := externalToAPI(row, inv)
	apiDisk.Mounted = false
	return &apiDisk, nil
}

func (h *Handler) FormatExternalDisk(ctx context.Context, req *apiv1.FormatExternalDiskRequest, params apiv1.FormatExternalDiskParams) (*apiv1.ExternalDisk, error) {
	label := string(params.Label)
	row, inv, registered, err := h.resolveExternal(ctx, label, false)
	if err != nil {
		return nil, err
	}
	if req.Confirmation == "" {
		return nil, errConfirmRequired
	}
	if mounted, mErr := disk.IsMountpoint(row.Mountpoint); mErr == nil && mounted {
		return nil, errInvalidPlan(disk.ErrExternalMounted)
	}

	fs := disk.XFS
	if v, ok := req.Filesystem.Get(); ok {
		switch v {
		case apiv1.ArrayDiskFilesystemXfs:
			fs = disk.XFS
		case apiv1.ArrayDiskFilesystemExt4:
			fs = disk.EXT4
		case apiv1.ArrayDiskFilesystemBtrfs:
			fs = disk.BTRFS
		default:
			return nil, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrUnsupportedFilesystem, v))
		}
	}

	assigned := disk.AssignedDisk{
		Device:       row.Device,
		Filesystem:   fs,
		WWN:          row.WWN,
		Serial:       row.Serial,
		WeakIdentity: row.WeakIdentity,
		ByIDName:     row.ByIDName,
	}
	plan := disk.ExternalFormatPlan(assigned)
	if plan.CheckConfirmation(req.Confirmation) != nil {
		return nil, errConfirmRequired
	}
	if err := disk.RefuseBootDevice(ctx, h.Disks, row.Device); err != nil {
		return nil, errInvalidPlan(err)
	}
	if err := disk.FormatExternal(ctx, h.Disks, h.diskRunner(), assigned, req.Confirmation); err != nil {
		if errors.Is(err, disk.ErrConfirmationMismatch) {
			return nil, errConfirmRequired
		}
		if errors.Is(err, disk.ErrBootDevice) {
			return nil, errInvalidPlan(err)
		}
		return nil, err
	}

	if !registered {
		if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
			Device: row.Device,
			Label:  apiv1.ExternalDiskLabel(label),
		}); err != nil {
			return nil, err
		}
		row, inv, _, err = h.resolveExternal(ctx, label, false)
		if err != nil {
			return nil, err
		}
	}

	uuid, err := disk.FilesystemUUID(ctx, h.diskRunner(), formatTarget(assigned))
	if err != nil {
		return nil, h.persistPendingUUID(ctx, row, fs, err)
	}
	row.Filesystem = string(fs)
	row.FSUUID = uuid
	if err := h.externalStore().UpdateExternalDisk(ctx, row); err != nil {
		return nil, err
	}
	apiDisk := externalToAPI(row, inv)
	return &apiDisk, nil
}

func (h *Handler) persistPendingUUID(ctx context.Context, row store.ExternalDisk, fs disk.FilesystemType, probeErr error) error {
	row.Filesystem = string(fs)
	row.FSUUID = pendingExternalUUID(row.Label)
	if err := h.externalStore().UpdateExternalDisk(ctx, row); err != nil {
		return fmt.Errorf("%w (persisting pending UUID: %v)", probeErr, err)
	}
	return probeErr
}

func formatTarget(d disk.AssignedDisk) string {
	if p := (disk.Identity{ByIDName: d.ByIDName}).IdentityPath(); p != "" {
		return p
	}
	return d.Device
}

func (h *Handler) externalRow(ctx context.Context, label string) (store.ExternalDisk, disk.Disk, error) {
	row, inv, _, err := h.resolveExternal(ctx, label, true)
	return row, inv, err
}

func (h *Handler) resolveExternal(ctx context.Context, label string, persist bool) (store.ExternalDisk, disk.Disk, bool, error) {
	ext := h.externalStore()
	if ext == nil || h.Disks == nil {
		return store.ExternalDisk{}, disk.Disk{}, false, errExternalNotConfigured()
	}
	if err := disk.ValidateExternalLabel(label); err != nil {
		return store.ExternalDisk{}, disk.Disk{}, false, errInvalidPlan(err)
	}
	row, err := ext.GetExternalDisk(ctx, label)
	if err != nil && !errors.Is(err, store.ErrExternalNotFound) {
		return store.ExternalDisk{}, disk.Disk{}, false, err
	}
	registered := err == nil
	if errors.Is(err, store.ErrExternalNotFound) {
		if persist {
			row, err = h.registerFromInventory(ctx, ext, label)
			if err != nil {
				return store.ExternalDisk{}, disk.Disk{}, false, err
			}
			registered = true
		} else {
			row, err = h.rowFromInventory(ctx, label)
			if err != nil {
				return store.ExternalDisk{}, disk.Disk{}, false, err
			}
		}
	}
	arrayDevs, err := h.arrayDevices(ctx)
	if err != nil {
		return store.ExternalDisk{}, disk.Disk{}, registered, err
	}
	if _, inArray := arrayDevs[row.Device]; inArray {
		return store.ExternalDisk{}, disk.Disk{}, registered, errInvalidPlan(fmt.Errorf("%w: %s", disk.ErrExternalInArray, row.Device))
	}
	listed, err := h.Disks.List(ctx)
	if err != nil {
		return store.ExternalDisk{}, disk.Disk{}, registered, fmt.Errorf("listing disks: %w", err)
	}
	inv, lookupErr := disk.LookupDisk(listed, row.Device)
	if lookupErr != nil {
		inv = disk.Disk{Device: row.Device, Size: 0}
	}
	return row, inv, registered, nil
}

func (h *Handler) inventoryDisk(ctx context.Context, label string) (disk.Disk, error) {
	listed, err := h.Disks.List(ctx)
	if err != nil {
		return disk.Disk{}, fmt.Errorf("listing disks: %w", err)
	}
	arrayDevs, err := h.arrayDevices(ctx)
	if err != nil {
		return disk.Disk{}, err
	}
	for _, d := range listed {
		if d.Boot {
			continue
		}
		if _, inArray := arrayDevs[d.Device]; inArray {
			continue
		}
		if d.Label == label {
			return d, nil
		}
	}
	return disk.Disk{}, errExternalNotFound(label)
}

func (h *Handler) rowFromInventory(ctx context.Context, label string) (store.ExternalDisk, error) {
	match, err := h.inventoryDisk(ctx, label)
	if err != nil {
		return store.ExternalDisk{}, err
	}
	fsUUID := pendingExternalUUID(label)
	if match.FSUUID != "" {
		fsUUID = match.FSUUID
	} else if match.Filesystem != "" {
		if uuid, uuidErr := disk.FilesystemUUID(ctx, h.diskRunner(), match.Device); uuidErr == nil {
			fsUUID = uuid
		}
	}
	mountpoint, err := disk.ExternalMountPoint(label)
	if err != nil {
		return store.ExternalDisk{}, errInvalidPlan(err)
	}
	return store.ExternalDisk{
		Label:        label,
		Device:       match.Device,
		Filesystem:   match.Filesystem,
		FSUUID:       fsUUID,
		WWN:          match.WWN,
		Serial:       match.Serial,
		ByIDName:     match.ByIDName,
		WeakIdentity: match.WeakIdentity,
		Mountpoint:   mountpoint,
	}, nil
}

func (h *Handler) registerFromInventory(ctx context.Context, ext *store.ExternalStore, label string) (store.ExternalDisk, error) {
	match, err := h.inventoryDisk(ctx, label)
	if err != nil {
		return store.ExternalDisk{}, err
	}
	req := &apiv1.RegisterExternalDiskRequest{Device: match.Device, Label: apiv1.ExternalDiskLabel(label)}
	if _, err := h.RegisterExternalDisk(ctx, req); err != nil {
		return store.ExternalDisk{}, err
	}
	return ext.GetExternalDisk(ctx, label)
}

func (h *Handler) externalAPI(ctx context.Context, row store.ExternalDisk) (*apiv1.ExternalDisk, error) {
	var inv disk.Disk
	if h.Disks != nil {
		listed, err := h.Disks.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing disks: %w", err)
		}
		inv, _ = disk.LookupDisk(listed, row.Device)
	}
	out := externalToAPI(row, inv)
	return &out, nil
}

func errExternalNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "external disks are not configured on this daemon"}
}

func pendingExternalUUID(label string) string {
	return "pending:" + label
}

func isPendingUUID(uuid string) bool {
	return uuid == "" || strings.HasPrefix(uuid, "pending:")
}

func mustExternalMount(label string) string {
	where, err := disk.ExternalMountPoint(label)
	if err != nil {
		return disk.ExternalMountRoot + "/" + label
	}
	return where
}

func externalToAPI(row store.ExternalDisk, inv disk.Disk) apiv1.ExternalDisk {
	mounted := false
	if row.Mountpoint != "" {
		if m, err := disk.IsMountpoint(row.Mountpoint); err == nil {
			mounted = m
		}
	}
	out := apiv1.ExternalDisk{
		Label:             apiv1.ExternalDiskLabel(row.Label),
		Device:            row.Device,
		MountPoint:        row.Mountpoint,
		ContainerPath:     row.Mountpoint,
		Mounted:           mounted,
		BackupDestination: row.BackupDestination,
		Boot:              false,
	}
	if row.Filesystem != "" {
		out.Filesystem = apiv1.NewOptString(row.Filesystem)
	} else if inv.Filesystem != "" {
		out.Filesystem = apiv1.NewOptString(inv.Filesystem)
	}
	if row.FSUUID != "" && !isPendingUUID(row.FSUUID) {
		out.FsUuid = apiv1.NewOptString(row.FSUUID)
	}
	if inv.Size > 0 {
		out.SizeBytes = apiv1.NewOptNilInt64(inv.Size)
	}
	if inv.Model != "" {
		out.Model = apiv1.NewOptString(inv.Model)
	}
	serial := row.Serial
	if serial == "" {
		serial = inv.Serial
	}
	if serial != "" {
		out.Serial = apiv1.NewOptString(serial)
	}
	return out
}
