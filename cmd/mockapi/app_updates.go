package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mockUpdateAvailable is whether ListAppUpdates reports a newer image for
// the container: its fixed fixture gives one to jellyfin only.
func mockUpdateAvailable(name string) bool {
	return name == "jellyfin"
}

// mockUpdatePinned is whether the container is pinned to an image digest in
// the mock's fixture: it cannot be updated, and the update check skips it.
func mockUpdatePinned(name string) bool {
	return name == "transcoder"
}

func errNothingToRevert(format string, args ...any) error {
	return &mockError{code: "nothing_to_revert", statusCode: 409, message: fmt.Sprintf("%s: %s", container.ErrNothingToRevert, fmt.Sprintf(format, args...))}
}

func errRevertUnavailable(format string, args ...any) error {
	return &mockError{code: "revert_unavailable", statusCode: 409, message: fmt.Sprintf("%s: %s", container.ErrRevertUnavailable, fmt.Sprintf(format, args...))}
}

func errAppPinned(name string) error {
	return &mockError{code: "app_pinned", statusCode: 409, message: fmt.Sprintf("%s: %s", container.ErrUpdatePinned, name)}
}

// recordMockUpdate notes an update the mock has "made", as production's job
// writes its record: the previous image is kept for the configured period
// and, for a container whose appdata sits under the cache disk's appdata
// directory, a pre-update snapshot is named. A container the update check
// shows no newer image for is already on the newest one, and the job
// records nothing for it. Only the newest record of a container can be
// reverted. h.appsMu is held by the caller.
func (h *handler) recordMockUpdate(app apiv1.App, at time.Time) {
	if !mockUpdateAvailable(app.Name) {
		return
	}
	rec := apiv1.AppUpdateRecord{
		ID:              int64(len(h.updateRecords) + 1),
		Container:       app.Name,
		Image:           app.Image + ":" + app.Tag,
		PreviousImageId: "sha256:previous-" + app.Name,
		UpdatedAt:       at,
		KeepUntil:       at.AddDate(0, 0, h.imageKeepDays),
	}
	for _, m := range app.Mounts {
		if src, ok := m.Source.Get(); ok && strings.HasPrefix(src, mockAppdataRoot) {
			rec.SnapshotArchive = apiv1.NewOptString(fmt.Sprintf("hoserva-appdata-0123456789ab-%s-%s.pre-update.tar.zst", app.Name, at.UTC().Format("2006-01-02T15-04-05")))
			rec.SnapshotDestinationId = apiv1.NewOptString("pool")
			break
		}
	}
	for i := range h.updateRecords {
		if h.updateRecords[i].Container == app.Name {
			h.updateRecords[i].Revertible = false
		}
	}
	rec.Revertible = true
	h.updateRecords = append(h.updateRecords, rec)
}

// UpdateApp mirrors production: the array is checked before the container is
// looked for, a container pinned to a digest is refused, and the update is
// recorded as the job would write it, which is not at all when there is no
// newer image.
func (h *handler) UpdateApp(ctx context.Context, params apiv1.UpdateAppParams) (*apiv1.Job, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	h.appsMu.Lock()
	i, err := h.findApp(params.ID)
	var app apiv1.App
	if err == nil {
		app = h.apps[i]
	}
	h.appsMu.Unlock()
	if err != nil {
		return nil, err
	}
	if mockUpdatePinned(app.Name) {
		return nil, errAppPinned(app.Name)
	}
	job, err := h.queueServiceJob(apiv1.JobTypeContainerUpdate)
	if err != nil {
		return nil, err
	}
	h.appsMu.Lock()
	h.recordMockUpdate(app, time.Now().UTC())
	h.appsMu.Unlock()
	return job, nil
}

// RevertApp refuses with nothing_to_revert for a container whose newest
// update is missing or already reverted, and with revert_unavailable once the
// previous image's keep period is over, like production, and otherwise marks
// it reverted.
func (h *handler) RevertApp(ctx context.Context, params apiv1.RevertAppParams) (*apiv1.Job, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	h.appsMu.Lock()
	i, err := h.findApp(params.ID)
	if err != nil {
		h.appsMu.Unlock()
		return nil, err
	}
	name := h.apps[i].Name
	latest := -1
	for j := range h.updateRecords {
		if h.updateRecords[j].Container == name {
			latest = j
		}
	}
	switch {
	case latest < 0:
		h.appsMu.Unlock()
		return nil, errNothingToRevert("%s has no recorded update", name)
	case h.updateRecords[latest].RevertedAt.Set:
		h.appsMu.Unlock()
		return nil, errNothingToRevert("its latest update was already reverted")
	case !time.Now().Before(h.updateRecords[latest].KeepUntil):
		keepUntil := h.updateRecords[latest].KeepUntil
		h.appsMu.Unlock()
		return nil, errRevertUnavailable("the previous image was only kept until %s", keepUntil.UTC().Format(time.RFC3339))
	}
	h.appsMu.Unlock()
	job, err := h.queueServiceJob(apiv1.JobTypeContainerUpdate)
	if err != nil {
		return nil, err
	}
	h.appsMu.Lock()
	h.updateRecords[latest].RevertedAt = apiv1.NewOptDateTime(time.Now().UTC())
	h.updateRecords[latest].Revertible = false
	h.appsMu.Unlock()
	return job, nil
}

// StartAppUpdates mirrors production: every named container is looked up
// before the job is queued, naming one updates it whether or not it opted
// out, and with none named the targets are the containers ListAppUpdates
// shows an update for, minus those that opted out.
func (h *handler) StartAppUpdates(ctx context.Context, req apiv1.OptStartAppUpdatesRequest) (*apiv1.StartAppUpdatesOK, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	out := &apiv1.StartAppUpdatesOK{Containers: []string{}, Skipped: []apiv1.AppUpdateSkipped{}}
	var targets []apiv1.App
	requested := req.Value.Containers

	h.appsMu.Lock()
	if len(requested) == 0 {
		for _, a := range h.apps {
			switch {
			case !mockUpdateAvailable(a.Name):
			case h.bulkExcluded[a.Name]:
				out.Skipped = append(out.Skipped, apiv1.AppUpdateSkipped{Container: a.Name, Reason: container.BulkReasonExcluded})
			default:
				targets = append(targets, a)
			}
		}
	} else {
		seen := map[string]bool{}
		for _, id := range requested {
			i, err := h.findApp(id)
			if err != nil {
				h.appsMu.Unlock()
				return nil, err
			}
			if mockUpdatePinned(h.apps[i].Name) {
				h.appsMu.Unlock()
				return nil, errAppPinned(h.apps[i].Name)
			}
			if !seen[h.apps[i].Name] {
				seen[h.apps[i].Name] = true
				targets = append(targets, h.apps[i])
			}
		}
	}
	h.appsMu.Unlock()

	sort.Slice(out.Skipped, func(i, j int) bool { return out.Skipped[i].Container < out.Skipped[j].Container })
	if len(requested) == 0 {
		sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	}
	if len(targets) == 0 {
		return out, nil
	}
	job, err := h.queueServiceJob(apiv1.JobTypeContainerUpdate)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	h.appsMu.Lock()
	for _, a := range targets {
		out.Containers = append(out.Containers, a.Name)
		h.recordMockUpdate(a, now)
	}
	h.appsMu.Unlock()
	out.Job = apiv1.NewOptJob(*job)
	return out, nil
}

func (h *handler) SetAppUpdatePolicy(ctx context.Context, req *apiv1.SetAppUpdatePolicyRequest, params apiv1.SetAppUpdatePolicyParams) (*apiv1.AppUpdatePolicy, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	i, err := h.findApp(params.ID)
	if err != nil {
		return nil, err
	}
	name := h.apps[i].Name
	h.bulkExcluded[name] = req.BulkExcluded
	return &apiv1.AppUpdatePolicy{Container: name, BulkExcluded: req.BulkExcluded}, nil
}

func (h *handler) ListAppUpdateHistory(ctx context.Context) (*apiv1.ListAppUpdateHistoryOK, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	records := make([]apiv1.AppUpdateRecord, 0, len(h.updateRecords))
	for i := len(h.updateRecords) - 1; i >= 0; i-- {
		records = append(records, h.updateRecords[i])
	}
	return &apiv1.ListAppUpdateHistoryOK{Available: true, Records: records}, nil
}

func (h *handler) GetAppSettings(ctx context.Context) (*apiv1.AppSettings, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	return &apiv1.AppSettings{ImageKeepDays: h.imageKeepDays}, nil
}

func (h *handler) UpdateAppSettings(ctx context.Context, req *apiv1.AppSettings) (*apiv1.AppSettings, error) {
	if req.ImageKeepDays < 1 || req.ImageKeepDays > store.MaxImageKeepDays {
		return nil, &mockError{code: "invalid_image_keep_days", statusCode: 400, message: fmt.Sprintf("%s: %d", store.ErrImageKeepDays, req.ImageKeepDays)}
	}
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	h.imageKeepDays = req.ImageKeepDays
	return &apiv1.AppSettings{ImageKeepDays: h.imageKeepDays}, nil
}
