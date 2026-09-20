package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/update"
)

func (h *Handler) updateEngine() (*update.Engine, error) {
	if h.Updates == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "self-update is not configured on this daemon"}
	}
	return h.Updates, nil
}

func mapUpdateError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, update.ErrConfirmRequired) {
		return errConfirmRequired
	}
	if errors.Is(err, update.ErrNotAvailable) {
		return &apiError{code: "update_not_available", statusCode: 409, message: err.Error()}
	}
	if errors.Is(err, update.ErrNoPrevious) {
		return &apiError{code: "rollback_not_available", statusCode: 409, message: err.Error()}
	}
	if errors.Is(err, update.ErrChecksumMismatch) {
		return &apiError{code: "update_checksum_mismatch", statusCode: 409, message: err.Error()}
	}
	var blocked update.ErrBlocked
	if errors.As(err, &blocked) {
		return &apiError{code: "update_blocked", statusCode: 409, message: err.Error()}
	}
	return err
}

func (h *Handler) GetUpdateStatus(ctx context.Context) (*apiv1.UpdateStatus, error) {
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	row, err := eng.PersistedSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting update status: %w", err)
	}
	st, err := eng.Status(ctx, row.CheckEnabled)
	if err != nil {
		return nil, fmt.Errorf("getting update status: %w", err)
	}
	return updateStatusToAPI(st), nil
}

func (h *Handler) UpdateUpdateSettings(ctx context.Context, req *apiv1.UpdateUpdateSettingsRequest) (*apiv1.UpdateStatus, error) {
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	var channel *update.Channel
	if v, ok := req.Channel.Get(); ok {
		c := update.Channel(v)
		channel = &c
	}
	var enabled *bool
	if v, ok := req.CheckEnabled.Get(); ok {
		enabled = &v
	}
	st, err := eng.SetSettings(ctx, channel, enabled)
	if err != nil {
		return nil, mapUpdateError(err)
	}
	return updateStatusToAPI(st), nil
}

func (h *Handler) CheckForUpdate(ctx context.Context) (*apiv1.UpdateStatus, error) {
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	st, err := eng.Check(ctx)
	if err != nil {
		return nil, mapUpdateError(err)
	}
	return updateStatusToAPI(st), nil
}

func (h *Handler) ApplyUpdate(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired
	}
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	if err := eng.Apply(ctx); err != nil {
		return nil, mapUpdateError(err)
	}
	st, err := eng.Status(ctx, true)
	if err != nil {
		return nil, err
	}
	return updateStatusToAPI(st), nil
}

func (h *Handler) RollbackUpdate(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired
	}
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	if err := eng.Rollback(ctx); err != nil {
		return nil, mapUpdateError(err)
	}
	st, err := eng.Status(ctx, true)
	if err != nil {
		return nil, err
	}
	return updateStatusToAPI(st), nil
}

func (h *Handler) RebootHost(ctx context.Context, req *apiv1.ConfirmUpdateRequest) (*apiv1.UpdateStatus, error) {
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired
	}
	eng, err := h.updateEngine()
	if err != nil {
		return nil, err
	}
	if err := eng.Reboot(ctx); err != nil {
		return nil, mapUpdateError(err)
	}
	st, err := eng.Status(ctx, false)
	if err != nil {
		return nil, err
	}
	return updateStatusToAPI(st), nil
}

func updateStatusToAPI(st update.Status) *apiv1.UpdateStatus {
	out := &apiv1.UpdateStatus{
		CurrentVersion:       st.CurrentVersion,
		Channel:              apiv1.UpdateChannel(st.Channel),
		CheckEnabled:         st.CheckEnabled,
		RebootRequired:       st.RebootRequired,
		PendingDebianUpdates: debianUpdatesToAPI(st.PendingDebianUpdates),
		Dependencies:         depsToAPI(st.Dependencies),
	}
	if st.Available != nil {
		out.AvailableVersion = apiv1.NewOptString(st.Available.Version)
		out.AvailableTag = apiv1.NewOptString(st.Available.Tag)
		if st.Available.Changelog != "" {
			out.Changelog = apiv1.NewOptString(st.Available.Changelog)
		}
	}
	if st.PreviousVersion != "" {
		out.PreviousVersion = apiv1.NewOptString(st.PreviousVersion)
	}
	if st.BlockingJob != nil {
		out.BlockingJob = apiv1.NewOptBlockingJob(blockingJobToAPI(st.BlockingJob))
	}
	return out
}

func debianUpdatesToAPI(in []update.PendingUpdate) []apiv1.DebianPackageUpdate {
	out := make([]apiv1.DebianPackageUpdate, 0, len(in))
	for _, p := range in {
		out = append(out, apiv1.DebianPackageUpdate{
			Name:             p.Name,
			InstalledVersion: p.InstalledVersion,
			CandidateVersion: p.CandidateVersion,
		})
	}
	return out
}

func depsToAPI(in []update.DependencyStatus) []apiv1.PackageDependencyStatus {
	out := make([]apiv1.PackageDependencyStatus, 0, len(in))
	for _, d := range in {
		out = append(out, apiv1.PackageDependencyStatus{
			Name:             d.Name,
			InstalledVersion: d.InstalledVersion,
			TestedFloor:      d.TestedFloor,
			InRange:          d.InRange,
		})
	}
	return out
}

func blockingJobToAPI(j *job.Job) apiv1.BlockingJob {
	id, _ := uuid.Parse(j.ID)
	return apiv1.BlockingJob{
		ID:    id,
		Type:  apiv1.JobType(j.Type),
		Class: apiv1.JobClass(j.Class),
	}
}
