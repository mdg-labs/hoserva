package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func errRebalanceNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "rebalancing is not configured on this daemon"}
}

// isEvacuationPlanRefusal reports whether err is one of
// cache.PlanEvacuation's own four documented refusals
// (ErrEvacuationWontFit, ErrEvacuationNoOtherBranch,
// ErrEvacuationUnsupportedEntry, ErrEvacuationNonShareContent — matched
// with errors.Is, never a raw string compare, since PlanEvacuation always
// wraps them with the share/path/entry that tripped them) rather than a
// raw I/O error. PlanEvacuation walks the disk being evacuated with plain
// os.Stat/os.Lstat, os.ReadDir and filepath.WalkDir
// (nonShareTopLevelEntries, refuseUnsupportedEntries and enumerateFiles,
// internal/cache/evacuate.go and mover.go) while it is still being
// planned for removal — a failing disk can return a raw I/O error (EIO,
// and other errors that are not documented refusals) that has nothing to
// do with the plan being invalid, and reporting it as invalid_plan would
// tell the caller to fix a plan that was never the problem (the
// known-escapes "os.IsNotExist on a wrapped error" class of bug,
// generalized to any undistinguished error).
func isEvacuationPlanRefusal(err error) bool {
	return errors.Is(err, cache.ErrEvacuationWontFit) || errors.Is(err, cache.ErrEvacuationNoOtherBranch) || errors.Is(err, cache.ErrEvacuationUnsupportedEntry) || errors.Is(err, cache.ErrEvacuationNonShareContent)
}

// errEvacuationPlan classifies a cache.PlanEvacuation error the same way
// isEvacuationPlanRefusal does, for EvacuateDisk's own re-plan (whose
// response is a *apiv1.Job, so its 400 still goes through the shared
// Error schema): one of the four documented refusals becomes a 400
// invalid_plan; everything else is left as an unclassified internal
// error, which Handler.NewError already logs server-side and reports as
// an opaque 500.
func errEvacuationPlan(context string, err error) error {
	if isEvacuationPlanRefusal(err) {
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

// sharesOffLeavingDisks loads the array's share layout through load and
// drops every branch on a data disk leaving the array
// (store.ArrayDisk.LeavingArray, #366) other than keep: a rebalance
// neither reads from nor writes to a disk in removal, and an
// evacuation's plan needs the disk it evacuates, whatever its removal
// state, as its source. keep is "" for a rebalance. A branch's disk is
// filepath.Dir(branch), the "<disk>/<share>" shape every cache.Share
// branch has.
func (h *Handler) sharesOffLeavingDisks(ctx context.Context, load func(ctx context.Context) ([]cache.Share, error), keep string) ([]cache.Share, error) {
	_, disks, err := h.ArrayStore.GetArray(ctx)
	if err != nil && !errors.Is(err, store.ErrNoArray) {
		return nil, fmt.Errorf("reading the array's disks: %w", err)
	}
	leaving := make(map[string]bool)
	for _, d := range disks {
		if d.Role == store.ArrayRoleData && d.LeavingArray() && d.Mountpoint != keep {
			leaving[d.Mountpoint] = true
		}
	}
	shares, err := load(ctx)
	if err != nil {
		return nil, err
	}
	if len(leaving) == 0 {
		return shares, nil
	}
	out := make([]cache.Share, 0, len(shares))
	for _, s := range shares {
		branches := make([]string, 0, len(s.Branches))
		for _, b := range s.Branches {
			if !leaving[filepath.Dir(b)] {
				branches = append(branches, b)
			}
		}
		s.Branches = branches
		out = append(out, s)
	}
	return out, nil
}

// PlanRebalance computes the rebalance plan (doc 09 §3) from the array's
// current share layout, purely for display: nothing is copied, synced or
// deleted. startRebalance recomputes this same plan fresh immediately
// before submitting the job, so this preview is never itself trusted at
// confirm time. Disks leaving the array are not part of the plan
// (sharesOffLeavingDisks).
func (h *Handler) PlanRebalance(ctx context.Context) (*apiv1.RebalancePlan, error) {
	_, _, _, rebalanceShares := h.CurrentParity()
	if rebalanceShares == nil || h.ArrayStore == nil {
		return nil, errRebalanceNotConfigured()
	}
	shares, err := h.sharesOffLeavingDisks(ctx, rebalanceShares, "")
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
	_, _, _, rebalanceShares := h.CurrentParity()
	if rebalanceShares == nil || h.ArrayStore == nil {
		return nil, errRebalanceNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if req.Confirmation == "" || job.RebalanceConfirmation() != req.Confirmation {
		return nil, errConfirmRequired
	}
	shares, err := h.sharesOffLeavingDisks(ctx, rebalanceShares, "")
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

// errDiskLeavingArray is the 409 for an operation on a data disk that is
// already leaving the array (#366): an evacuation of a disk that has
// left the pool, and a replace or upgrade of a disk in removal.
func errDiskLeavingArray(mountpoint, state string) error {
	return &apiError{code: "disk_leaving_array", statusCode: 409, message: fmt.Sprintf("disk %s is being removed from the array (%s)", mountpoint, state)}
}

// evacuationDataDisk confirms req's mountpoint currently names a data
// disk slot, the same doc 02 §4 restriction PlanDiskReplace already
// applies — evacuation only ever makes sense for a data disk — and
// refuses (errDiskLeavingArray) a disk that has already left the pool
// (store.ArrayDisk.LeftPool): only finishDiskRemoval takes it further.
// An "evacuating" or "evacuated" disk passes, so evacuating it again
// resumes or repeats its evacuation.
func (h *Handler) evacuationDataDisk(ctx context.Context, mountpoint string) (store.ArrayDisk, error) {
	existing, err := h.ArrayStore.GetDataDiskByMountpoint(ctx, mountpoint)
	if err != nil {
		if errors.Is(err, store.ErrArrayDiskNotFound) {
			return store.ArrayDisk{}, errDiskSlotNotFound(mountpoint)
		}
		return store.ArrayDisk{}, err
	}
	if existing.LeftPool() {
		return store.ArrayDisk{}, errDiskLeavingArray(mountpoint, existing.RemovalState)
	}
	return existing, nil
}

// errDiskRemovalInProgress is refuseIfAnotherDiskRemoving's own 409: only
// one disk is ever in removal at a time (doc 09 §4's own Open questions,
// #359) — the *Removing mount builders each take a single removingDisk.
func errDiskRemovalInProgress(mountpoint string) error {
	return &apiError{code: "disk_removal_in_progress", statusCode: 409, message: fmt.Sprintf("disk %s is already being removed", mountpoint)}
}

// refuseIfAnotherDiskRemoving refuses (errDiskRemovalInProgress) when a
// data disk other than mountpoint is already in removal (#359): both
// PlanDiskEvacuation and EvacuateDisk call this before computing a plan,
// so a second evacuation can never even preview against a pool topology
// that a different evacuation's own no-create switch is still changing.
// mountpoint itself is never refused: a repeat plan or evacuate call for
// the disk already in removal is exactly the "resume" case, not a second
// disk entering it.
func (h *Handler) refuseIfAnotherDiskRemoving(ctx context.Context, mountpoint string) error {
	removing, _, err := h.ArrayStore.RemovingDisk(ctx)
	if err != nil {
		return fmt.Errorf("checking for a disk already in removal: %w", err)
	}
	if removing != "" && removing != mountpoint {
		return errDiskRemovalInProgress(removing)
	}
	return nil
}

// PlanDiskEvacuation computes the evacuation plan for the data disk at
// req.Mountpoint (doc 09 §4 steps 1 and 3 — the fit pre-check and
// enumerating what would move; step 2's no-create switch is not applied by
// either this or EvacuateDisk, api/openapi.yaml's own description),
// purely for display: nothing is copied, synced or deleted, and the disk
// keeps taking new writes.
func (h *Handler) PlanDiskEvacuation(ctx context.Context, req *apiv1.EvacuateDiskPlanRequest) (apiv1.PlanDiskEvacuationRes, error) {
	_, _, _, rebalanceShares := h.CurrentParity()
	if rebalanceShares == nil || h.ArrayStore == nil {
		return nil, errRebalanceNotConfigured()
	}
	if _, err := h.evacuationDataDisk(ctx, req.Mountpoint); err != nil {
		return nil, err
	}
	if err := h.refuseIfAnotherDiskRemoving(ctx, req.Mountpoint); err != nil {
		return nil, err
	}
	shares, err := h.sharesOffLeavingDisks(ctx, rebalanceShares, req.Mountpoint)
	if err != nil {
		return nil, fmt.Errorf("evacuation plan: loading shares: %w", err)
	}
	plan, err := cache.PlanEvacuation(ctx, req.Mountpoint, shares, cache.Deps{})
	if err != nil {
		// A refusal carries its own EvacuationPlanRefusal body — including
		// NonSharePaths, so the caller can show exactly what blocks
		// evacuation — rather than the shared Error schema's bare
		// code/message, which can't carry the paths structurally (#367).
		// An unclassified error (e.g. a failing disk's raw I/O error) is
		// still reported through the error return, as an opaque 500.
		if !isEvacuationPlanRefusal(err) {
			return nil, fmt.Errorf("evacuation plan: %w", err)
		}
		return &apiv1.EvacuationPlanRefusal{
			Code:          "invalid_plan",
			Message:       err.Error(),
			NonSharePaths: nonSharePathsToAPI(plan.NonShareContent),
		}, nil
	}
	return &apiv1.EvacuationPlan{
		Mountpoint:    req.Mountpoint,
		Moves:         rebalanceMovesToAPI(plan.Moves),
		Warnings:      rebalanceWarningsToAPI(plan.Warnings),
		NonSharePaths: nonSharePathsToAPI(plan.NonShareContent),
		Confirmation:  job.EvacuationConfirmation(req.Mountpoint),
	}, nil
}

// nonSharePathsToAPI never returns a nil slice: EvacuationPlan.NonSharePaths
// is required (D18), so a plan with none must still encode `[]`, not a
// JSON null, the same reasoning rebalanceMovesToAPI/rebalanceWarningsToAPI
// already apply to their own required array fields.
func nonSharePathsToAPI(paths []string) []string {
	out := make([]string, 0, len(paths))
	out = append(out, paths...)
	return out
}

// EvacuateDisk recomputes the evacuation plan fresh — startRebalance's
// own reasoning — and, once req.Confirmation matches the phrase
// planDiskEvacuation returned for this mountpoint, queues
// job.TypeEvacuation with that freshly computed plan as its own payload.
func (h *Handler) EvacuateDisk(ctx context.Context, req *apiv1.EvacuateDiskRequest) (*apiv1.Job, error) {
	_, _, _, rebalanceShares := h.CurrentParity()
	if rebalanceShares == nil || h.ArrayStore == nil {
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
	if err := h.refuseIfAnotherDiskRemoving(ctx, req.Mountpoint); err != nil {
		return nil, err
	}
	shares, err := h.sharesOffLeavingDisks(ctx, rebalanceShares, req.Mountpoint)
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
