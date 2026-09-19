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
)

func errInvalidPlan(err error) error {
	return &apiError{code: "invalid_plan", statusCode: 400, message: err.Error()}
}

func errUnmanagedDevice(err error) error {
	return &apiError{code: "unmanaged_device", statusCode: 400, message: err.Error()}
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
	return &apiError{code: failedCode, statusCode: 409, message: err.Error()}
}

func (h *Handler) StopArray(ctx context.Context, req *apiv1.StopArrayRequest) (*apiv1.SystemStatus, error) {
	if !req.Confirm {
		return nil, errConfirmRequired
	}
	if h.Array == nil {
		return nil, errArrayNotConfigured()
	}
	if err := h.Array.Stop(ctx); err != nil {
		return nil, mapArraySequenceError(err, "array_stop_failed")
	}
	return h.GetStatus(ctx)
}

func (h *Handler) StartArray(ctx context.Context) (*apiv1.SystemStatus, error) {
	if h.Array == nil {
		return nil, errArrayNotConfigured()
	}
	if err := h.Array.Start(ctx); err != nil {
		return nil, mapArraySequenceError(err, "array_start_failed")
	}
	return h.GetStatus(ctx)
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
