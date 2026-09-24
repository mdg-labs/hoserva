package main

import (
	"context"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
)

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
	if req.Confirmation == "" || job.EvacuationConfirmation(req.Mountpoint) != req.Confirmation {
		return nil, errConfirmRequired()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
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
