package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// errDiskRemovalInProgress mirrors production's disk_removal_in_progress
// refusal (#359, doc 09 §4 step 2): only one disk is ever in removal at a
// time.
func errDiskRemovalInProgress(mountpoint string) error {
	return &mockError{code: "disk_removal_in_progress", statusCode: 409, message: fmt.Sprintf("disk %s is already being removed", mountpoint)}
}

// refuseIfAnotherDiskRemoving mirrors internal/api's own
// Handler.refuseIfAnotherDiskRemoving against this mock's fixed
// mockArrayDisks state.
func refuseIfAnotherDiskRemoving(disks []store.ArrayDisk, mountpoint string) error {
	for _, d := range disks {
		if d.RemovalState != "" && d.Mountpoint != mountpoint {
			return errDiskRemovalInProgress(d.Mountpoint)
		}
	}
	return nil
}

// refuseIfEvacuationPending mirrors job.Scheduler's own evacuation
// admission (#359), surfaced by internal/api as evacuation_pending: no new
// evacuation while another is queued, running or interrupted.
func refuseIfEvacuationPending(jobs map[uuid.UUID]apiv1.Job) error {
	for _, j := range jobs {
		if j.Type != apiv1.JobTypeEvacuation {
			continue
		}
		switch j.Status {
		case apiv1.JobStatusQueued, apiv1.JobStatusRunning, apiv1.JobStatusInterrupted:
			return &mockError{code: "evacuation_pending", statusCode: 409, message: fmt.Sprintf("job: an evacuation is already pending: job %s — resume it, or cancel it, before starting another", j.ID)}
		}
	}
	return nil
}

// mockRebalancePlan is planRebalance/startRebalance's own plan against
// this mock's fixed disk usage: mockPoolStatus's own disks are all
// equally filled by design (fresh-install has no array at all), so the
// honest answer is an empty plan — nothing needs to move — rather than
// fabricated move entries this mock has no real share filesystem behind.
func mockRebalancePlan() *apiv1.RebalancePlan {
	return &apiv1.RebalancePlan{
		Moves:        []apiv1.RebalanceMove{},
		Warnings:     []apiv1.RebalanceWarning{},
		Confirmation: job.RebalanceConfirmation(),
	}
}

// PlanRebalance mirrors internal/api's own PlanRebalance against this
// mock's fixed inventory.
func (h *handler) PlanRebalance(ctx context.Context) (*apiv1.RebalancePlan, error) {
	if _, _, err := h.mockArrayState(); err != nil {
		return nil, err
	}
	return mockRebalancePlan(), nil
}

// StartRebalance mirrors internal/api's own StartRebalance, with
// production's own confirmation check (job.RebalanceConfirmation).
func (h *handler) StartRebalance(ctx context.Context, req *apiv1.StartRebalanceRequest) (*apiv1.Job, error) {
	if _, _, err := h.mockArrayState(); err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.RebalanceConfirmation() != req.Confirmation {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeRebalance,
		Class:       apiv1.JobClassArrayWrite,
		Status:      apiv1.JobStatusQueued,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}

// PlanDiskEvacuation mirrors internal/api's own PlanDiskEvacuation against
// this mock's fixed inventory: the same disk-slot-not-found refusal, and
// the same honestly empty plan mockRebalancePlan returns.
func (h *handler) PlanDiskEvacuation(ctx context.Context, req *apiv1.EvacuateDiskPlanRequest) (*apiv1.EvacuationPlan, error) {
	disks, _, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	if _, ok := mockDataDiskAt(disks, req.Mountpoint); !ok {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if err := refuseIfAnotherDiskRemoving(disks, req.Mountpoint); err != nil {
		return nil, err
	}
	return &apiv1.EvacuationPlan{
		Mountpoint:   req.Mountpoint,
		Moves:        []apiv1.RebalanceMove{},
		Warnings:     []apiv1.RebalanceWarning{},
		Confirmation: job.EvacuationConfirmation(req.Mountpoint),
	}, nil
}

// EvacuateDisk mirrors internal/api's own EvacuateDisk, with production's
// own confirmation check (job.EvacuationConfirmation).
func (h *handler) EvacuateDisk(ctx context.Context, req *apiv1.EvacuateDiskRequest) (*apiv1.Job, error) {
	disks, _, err := h.mockArrayState()
	if err != nil {
		return nil, err
	}
	if _, ok := mockDataDiskAt(disks, req.Mountpoint); !ok {
		return nil, errDiskSlotNotFound(req.Mountpoint)
	}
	if err := refuseIfAnotherDiskRemoving(disks, req.Mountpoint); err != nil {
		return nil, err
	}
	if req.Confirmation == "" || job.EvacuationConfirmation(req.Mountpoint) != req.Confirmation {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := refuseIfEvacuationPending(h.jobs); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	j := apiv1.Job{
		ID:          uuid.New(),
		Type:        apiv1.JobTypeEvacuation,
		Class:       apiv1.JobClassArrayWrite,
		Status:      apiv1.JobStatusQueued,
		Resumable:   true,
		Cancellable: true,
		CreatedAt:   now,
	}
	h.jobs[j.ID] = j
	return &j, nil
}
