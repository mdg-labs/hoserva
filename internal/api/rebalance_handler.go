package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errRebalanceNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "rebalancing is not configured on this daemon"}
}

// errEvacuationPlan classifies a cache.PlanEvacuation error: only its own
// three documented refusals (ErrEvacuationWontFit,
// ErrEvacuationNoOtherBranch, ErrEvacuationUnsupportedEntry — matched with
// errors.Is, never a raw string compare, since PlanEvacuation always
// wraps them with the share/path/entry that tripped them) become a 400
// invalid_plan. PlanEvacuation also walks the disk being evacuated with
// plain os.Stat/os.Lstat and filepath.WalkDir (refuseUnsupportedEntries
// and enumerateFiles, internal/cache/evacuate.go and mover.go) while it
// is still being planned for removal — a failing disk can return a raw
// I/O error (EIO, and other errors that are not documented refusals)
// that has nothing to do with the plan being invalid, and reporting it
// as invalid_plan would tell the caller to fix a plan that was never the
// problem (the known-escapes "os.IsNotExist on a wrapped error" class of
// bug, generalized to any undistinguished error). Everything else is
// left as an unclassified internal error, which Handler.NewError already
// logs server-side and reports as an opaque 500.
func errEvacuationPlan(context string, err error) error {
	if errors.Is(err, cache.ErrEvacuationWontFit) || errors.Is(err, cache.ErrEvacuationNoOtherBranch) || errors.Is(err, cache.ErrEvacuationUnsupportedEntry) {
		return errInvalidPlan(err)
	}
	return fmt.Errorf("%s: %w", context, err)
}

// rebalanceMovesToAPI and rebalanceWarningsToAPI translate
// cache.PlanRebalance/cache.PlanEvacuation's own output into the
// generated schema (D18) both planRebalance and planDiskEvacuation
// return, shared since both plan a cache.RebalancePlan.
func rebalanceMovesToAPI(moves []cache.RebalanceMove) []apiv1.RebalanceMove {
	out := make([]apiv1.RebalanceMove, 0, len(moves))
	for _, m := range moves {
		out = append(out, apiv1.RebalanceMove{
			Share:        m.Share,
			RelPath:      m.RelPath,
			SourceBranch: m.SourceBranch,
			TargetBranch: m.TargetBranch,
			SizeBytes:    m.Size,
		})
	}
	return out
}

func rebalanceWarningsToAPI(warnings []cache.RebalanceWarning) []apiv1.RebalanceWarning {
	out := make([]apiv1.RebalanceWarning, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, apiv1.RebalanceWarning{Share: w.Share, Reason: w.Reason})
	}
	return out
}

// PlanRebalance computes the rebalance plan (doc 09 §3) from the array's
// current share layout, purely for display: nothing is copied, synced or
// deleted. startRebalance recomputes this same plan fresh immediately
// before submitting the job, so this preview is never itself trusted at
// confirm time.
func (h *Handler) PlanRebalance(ctx context.Context) (*apiv1.RebalancePlan, error) {
	if h.RebalanceShares == nil {
		return nil, errRebalanceNotConfigured()
	}
	shares, err := h.RebalanceShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("rebalance plan: loading shares: %w", err)
	}
	plan, err := cache.PlanRebalance(ctx, shares, cache.RebalanceConfig{}, cache.Deps{})
	if err != nil {
		return nil, fmt.Errorf("rebalance plan: %w", err)
	}
	return &apiv1.RebalancePlan{
		Moves:        rebalanceMovesToAPI(plan.Moves),
		Warnings:     rebalanceWarningsToAPI(plan.Warnings),
		Confirmation: job.RebalanceConfirmation(),
	}, nil
}

// StartRebalance recomputes the rebalance plan fresh — never the plan a
// client might supply, which could name branches this job has no
// business touching — and, once req.Confirmation matches the fixed
// phrase planRebalance always returns, queues job.TypeRebalance with that
// freshly computed plan as its own payload (RunRebalance then runs
// exactly that plan, never recomputing it itself).
func (h *Handler) StartRebalance(ctx context.Context, req *apiv1.StartRebalanceRequest) (*apiv1.Job, error) {
	if h.RebalanceShares == nil {
		return nil, errRebalanceNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if req.Confirmation == "" || job.RebalanceConfirmation() != req.Confirmation {
		return nil, errConfirmRequired
	}
	shares, err := h.RebalanceShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("start rebalance: loading shares: %w", err)
	}
	plan, err := cache.PlanRebalance(ctx, shares, cache.RebalanceConfig{}, cache.Deps{})
	if err != nil {
		return nil, fmt.Errorf("start rebalance: %w", err)
	}
	body, err := json.Marshal(job.RebalanceParams{Plan: plan})
	if err != nil {
		return nil, fmt.Errorf("encoding rebalance params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeRebalance, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// evacuationDataDisk confirms req's mountpoint currently names a data
// disk slot, the same doc 02 §4 restriction PlanDiskReplace already
// applies — evacuation only ever makes sense for a data disk.
func (h *Handler) evacuationDataDisk(ctx context.Context, mountpoint string) (store.ArrayDisk, error) {
	existing, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, mountpoint)
	if err != nil {
		if errors.Is(err, store.ErrArrayDiskNotFound) {
			return store.ArrayDisk{}, errDiskSlotNotFound(mountpoint)
		}
		return store.ArrayDisk{}, err
	}
	return existing, nil
}

// PlanDiskEvacuation computes the evacuation plan for the data disk at
// req.Mountpoint (doc 09 §4 steps 1 and 3 — the fit pre-check and
// enumerating what would move; step 2's no-create switch is not applied by
// either this or EvacuateDisk, api/openapi.yaml's own description),
// purely for display: nothing is copied, synced or deleted, and the disk
// keeps taking new writes.
func (h *Handler) PlanDiskEvacuation(ctx context.Context, req *apiv1.EvacuateDiskPlanRequest) (*apiv1.EvacuationPlan, error) {
	if h.RebalanceShares == nil || h.ArrayStore == nil {
		return nil, errRebalanceNotConfigured()
	}
	if _, err := h.evacuationDataDisk(ctx, req.Mountpoint); err != nil {
		return nil, err
	}
	shares, err := h.RebalanceShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("evacuation plan: loading shares: %w", err)
	}
	plan, err := cache.PlanEvacuation(ctx, req.Mountpoint, shares, cache.Deps{})
	if err != nil {
		return nil, errEvacuationPlan("evacuation plan", err)
	}
	return &apiv1.EvacuationPlan{
		Mountpoint:   req.Mountpoint,
		Moves:        rebalanceMovesToAPI(plan.Moves),
		Warnings:     rebalanceWarningsToAPI(plan.Warnings),
		Confirmation: job.EvacuationConfirmation(req.Mountpoint),
	}, nil
}

// EvacuateDisk recomputes the evacuation plan fresh — startRebalance's
// own reasoning — and, once req.Confirmation matches the phrase
// planDiskEvacuation returned for this mountpoint, queues
// job.TypeEvacuation with that freshly computed plan as its own payload.
func (h *Handler) EvacuateDisk(ctx context.Context, req *apiv1.EvacuateDiskRequest) (*apiv1.Job, error) {
	if h.RebalanceShares == nil || h.ArrayStore == nil {
		return nil, errRebalanceNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if req.Confirmation == "" || job.EvacuationConfirmation(req.Mountpoint) != req.Confirmation {
		return nil, errConfirmRequired
	}
	if _, err := h.evacuationDataDisk(ctx, req.Mountpoint); err != nil {
		return nil, err
	}
	shares, err := h.RebalanceShares(ctx)
	if err != nil {
		return nil, fmt.Errorf("evacuate disk: loading shares: %w", err)
	}
	plan, err := cache.PlanEvacuation(ctx, req.Mountpoint, shares, cache.Deps{})
	if err != nil {
		return nil, errEvacuationPlan("evacuate disk", err)
	}
	body, err := json.Marshal(job.EvacuationParams{Mountpoint: req.Mountpoint, Plan: plan})
	if err != nil {
		return nil, fmt.Errorf("encoding evacuation params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeEvacuation, nil, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}
