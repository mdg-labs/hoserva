package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mapAppUpdateError turns an error from container.Updater into the API's error
// shape. Only what the Engine or the updater itself classified gets a code;
// anything else, a database or snapshot-destination failure, stays an
// unclassified error rather than being blamed on Docker.
func mapAppUpdateError(id string, err error) error {
	switch {
	case errors.Is(err, container.ErrNothingToRevert):
		return &apiError{code: "nothing_to_revert", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrRevertUnavailable):
		return &apiError{code: "revert_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrUpdatePinned):
		return &apiError{code: "app_pinned", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrArrayStopped):
		return &apiError{code: "array_stopped", statusCode: 409, message: fmt.Sprintf("%v — nothing was changed", err)}
	case errors.Is(err, container.ErrArrayStateUnknown):
		return &apiError{code: "array_state_unknown", statusCode: 503, message: fmt.Sprintf("%v — nothing was changed", err)}
	case errors.Is(err, container.ErrNotFound), errors.Is(err, container.ErrUnavailable):
		return mapContainerError(id, err, "updating")
	default:
		return fmt.Errorf("updating container %q: %w", id, err)
	}
}

// appUpdater is the update execution, or the 501 every operation that needs
// Docker answers with when there is none.
func (h *Handler) appUpdater() (*container.Updater, error) {
	if h.AppUpdater == nil || h.AppUpdater.Lifecycle == nil {
		return nil, errLifecycleNotConfigured()
	}
	return h.AppUpdater, nil
}

func (h *Handler) submitContainerUpdate(ctx context.Context, resources []string, p job.ContainerUpdateParams) (*apiv1.Job, error) {
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("encoding container_update params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeContainerUpdate, resources, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// UpdateApp queues the update as a job: it pulls an image and may archive
// appdata first, which can take minutes, and must not hold a request open
// (doc 01 §4). Like RecreateApp it refuses up front, before any job exists,
// what the job would only fail on.
func (h *Handler) UpdateApp(ctx context.Context, params apiv1.UpdateAppParams) (*apiv1.Job, error) {
	u, err := h.appUpdater()
	if err != nil {
		return nil, err
	}
	c, err := u.CheckUpdate(ctx, params.ID)
	if err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	resources, err := u.UpdateScope(ctx, c.Name)
	if err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	return h.submitContainerUpdate(ctx, resources, job.ContainerUpdateParams{Mode: job.ContainerUpdateModeUpdate, Containers: []string{c.Name}})
}

// RevertApp queues the revert as a job after the same checks the job makes
// first, so a revert that cannot complete is refused with its reason and
// nothing is queued or changed.
func (h *Handler) RevertApp(ctx context.Context, params apiv1.RevertAppParams) (*apiv1.Job, error) {
	u, err := h.appUpdater()
	if err != nil {
		return nil, err
	}
	if err := u.CheckRevert(ctx, params.ID); err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	c, err := u.Lifecycle.Provider.Inspect(ctx, params.ID)
	if err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	resources, sharers, err := u.RevertScope(ctx, c.Name)
	if err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	return h.submitContainerUpdate(ctx, resources, job.ContainerUpdateParams{Mode: job.ContainerUpdateModeRevert, Containers: []string{c.Name}, Sharers: sharers})
}

// StartAppUpdates queues one job for several containers. Every container it
// names is looked up and checked before the job is queued, so a name that is
// unknown or cannot be updated queues nothing.
func (h *Handler) StartAppUpdates(ctx context.Context, req apiv1.OptStartAppUpdatesRequest) (*apiv1.StartAppUpdatesOK, error) {
	u, err := h.appUpdater()
	if err != nil {
		return nil, err
	}
	if err := u.Lifecycle.RequireArrayRunning(); err != nil {
		return nil, mapAppUpdateError("", err)
	}
	requested := req.Value.Containers
	skipped := []container.BulkSkip{}
	var names []string
	if len(requested) == 0 {
		sel, err := u.BulkTargets(ctx)
		if err != nil {
			return nil, mapAppUpdateError("", err)
		}
		names, skipped = sel.Containers, sel.Skipped
	} else {
		seen := map[string]bool{}
		for _, id := range requested {
			c, err := u.CheckUpdate(ctx, id)
			if err != nil {
				return nil, mapAppUpdateError(id, err)
			}
			if !seen[c.Name] {
				seen[c.Name] = true
				names = append(names, c.Name)
			}
		}
	}

	out := &apiv1.StartAppUpdatesOK{Containers: append([]string{}, names...), Skipped: make([]apiv1.AppUpdateSkipped, 0, len(skipped))}
	for _, s := range skipped {
		out.Skipped = append(out.Skipped, apiv1.AppUpdateSkipped{Container: s.Container, Reason: s.Reason})
	}
	if len(names) == 0 {
		return out, nil
	}
	var resources []string
	seen := map[string]bool{}
	for _, name := range names {
		scope, err := u.UpdateScope(ctx, name)
		if err != nil {
			return nil, mapAppUpdateError(name, err)
		}
		for _, r := range scope {
			if !seen[r] {
				seen[r] = true
				resources = append(resources, r)
			}
		}
	}
	j, err := h.submitContainerUpdate(ctx, resources, job.ContainerUpdateParams{Mode: job.ContainerUpdateModeUpdate, Containers: names})
	if err != nil {
		return nil, err
	}
	out.Job = apiv1.NewOptJob(*j)
	return out, nil
}

func (h *Handler) SetAppUpdatePolicy(ctx context.Context, req *apiv1.SetAppUpdatePolicyRequest, params apiv1.SetAppUpdatePolicyParams) (*apiv1.AppUpdatePolicy, error) {
	u, err := h.appUpdater()
	if err != nil {
		return nil, err
	}
	name, err := u.SetBulkExcluded(ctx, params.ID, req.BulkExcluded)
	if err != nil {
		return nil, mapAppUpdateError(params.ID, err)
	}
	return &apiv1.AppUpdatePolicy{Container: name, BulkExcluded: req.BulkExcluded}, nil
}

func (h *Handler) ListAppUpdateHistory(ctx context.Context) (*apiv1.ListAppUpdateHistoryOK, error) {
	u, err := h.appUpdater()
	if err != nil {
		return &apiv1.ListAppUpdateHistoryOK{Available: false, Records: []apiv1.AppUpdateRecord{}, Message: apiv1.NewOptString("Docker is not configured on this daemon")}, nil
	}
	records, err := u.Records(ctx)
	if err != nil {
		if errors.Is(err, container.ErrUnavailable) {
			return &apiv1.ListAppUpdateHistoryOK{Available: false, Records: []apiv1.AppUpdateRecord{}, Message: unavailableAppsMessage(err)}, nil
		}
		return nil, fmt.Errorf("reading the container update history: %w", err)
	}
	out := &apiv1.ListAppUpdateHistoryOK{Available: true, Records: make([]apiv1.AppUpdateRecord, 0, len(records))}
	for _, r := range records {
		rec := apiv1.AppUpdateRecord{
			ID: r.ID, Container: r.Container, Image: r.Image, PreviousImageId: r.PreviousImageID,
			UpdatedAt: r.UpdatedAt, KeepUntil: r.KeepUntil, Revertible: r.Revertible,
		}
		if r.SnapshotArchive != "" {
			rec.SnapshotArchive = apiv1.NewOptString(r.SnapshotArchive)
			rec.SnapshotDestinationId = apiv1.NewOptString(r.SnapshotDestination)
		}
		if !r.RevertedAt.IsZero() {
			rec.RevertedAt = apiv1.NewOptDateTime(r.RevertedAt)
		}
		out.Records = append(out.Records, rec)
	}
	return out, nil
}

func (h *Handler) GetAppSettings(ctx context.Context) (*apiv1.AppSettings, error) {
	if h.AppUpdater == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "container updates are not configured on this daemon"}
	}
	days, err := h.AppUpdater.KeepDays(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the container update settings: %w", err)
	}
	return &apiv1.AppSettings{ImageKeepDays: days}, nil
}

func (h *Handler) UpdateAppSettings(ctx context.Context, req *apiv1.AppSettings) (*apiv1.AppSettings, error) {
	if h.AppUpdater == nil {
		return nil, &apiError{code: "not_configured", statusCode: 501, message: "container updates are not configured on this daemon"}
	}
	if err := h.AppUpdater.SetKeepDays(ctx, req.ImageKeepDays); err != nil {
		if errors.Is(err, store.ErrImageKeepDays) {
			return nil, &apiError{code: "invalid_image_keep_days", statusCode: 400, message: err.Error()}
		}
		return nil, fmt.Errorf("saving the container update settings: %w", err)
	}
	return &apiv1.AppSettings{ImageKeepDays: req.ImageKeepDays}, nil
}
