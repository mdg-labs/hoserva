package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

func errAppdataNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "appdata backup is not configured on this daemon"}
}

func mapAppdataError(err error) error {
	switch {
	case errors.Is(err, backup.ErrAppdataContainerNotFound):
		return &apiError{code: "container_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, backup.ErrAppdataArchiveNotFound):
		return &apiError{code: "archive_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, backup.ErrDestinationNotFound):
		return &apiError{code: "backup_destination_not_found", statusCode: 404, message: "no backup destination with that id"}
	case errors.Is(err, backup.ErrAppdataArchiveInvalid):
		return &apiError{code: "appdata_archive_invalid", statusCode: 400, message: err.Error()}
	case errors.Is(err, backup.ErrAppdataBusy):
		return &apiError{code: "appdata_busy", statusCode: 409, message: err.Error()}
	case errors.Is(err, backup.ErrAppdataNoDestination):
		return &apiError{code: "no_appdata_destination", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrUnavailable):
		return &apiError{code: "docker_unavailable", statusCode: 503, message: fmt.Sprintf("Docker is not installed or not reachable: %v", err)}
	case errors.Is(err, container.ErrArrayStopped):
		return &apiError{code: "array_stopped", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrArrayStateUnknown):
		return &apiError{code: "array_state_unknown", statusCode: 503, message: err.Error()}
	default:
		return err
	}
}

func appdataContainerToAPI(c backup.AppdataContainer) apiv1.AppdataBackupContainer {
	out := apiv1.AppdataBackupContainer{
		Name:          c.Name,
		Image:         c.Image,
		Running:       c.Running,
		Stop:          c.Stop,
		Included:      c.Included,
		DatabaseImage: c.DatabaseImage,
	}
	if w := c.Warning(); w != "" {
		out.Warning = apiv1.NewOptNilString(w)
	}
	return out
}

func (h *Handler) GetAppdataBackup(ctx context.Context) (*apiv1.AppdataBackupConfig, error) {
	if h.Appdata == nil {
		return nil, errAppdataNotConfigured()
	}
	scope, err := h.Appdata.Config(ctx)
	if err != nil {
		return nil, mapAppdataError(err)
	}
	out := &apiv1.AppdataBackupConfig{Containers: make([]apiv1.AppdataBackupContainer, 0, len(scope))}
	for _, c := range scope {
		out.Containers = append(out.Containers, appdataContainerToAPI(c))
	}
	return out, nil
}

func (h *Handler) SetAppdataBackupContainer(ctx context.Context, req *apiv1.SetAppdataBackupContainerRequest, params apiv1.SetAppdataBackupContainerParams) (*apiv1.AppdataBackupContainer, error) {
	if h.Appdata == nil {
		return nil, errAppdataNotConfigured()
	}
	c, err := h.Appdata.SetPolicy(ctx, params.Name, req.Stop, req.Included)
	if err != nil {
		return nil, mapAppdataError(err)
	}
	out := appdataContainerToAPI(c)
	return &out, nil
}

// SubmitAppdataBackup queues an appdata_backup job for the named
// containers, or every included one, scoped to the containers it will
// stop so a recreate of one of them queues behind it. It is the one entry
// both the API and the schedule use.
func SubmitAppdataBackup(ctx context.Context, svc *backup.AppdataService, sched *job.Scheduler, requested []string) (*job.Job, error) {
	if svc == nil {
		return nil, errAppdataNotConfigured()
	}
	if sched == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if err := svc.RequireArrayRunning(); err != nil {
		return nil, mapAppdataError(err)
	}
	names, err := svc.ScopeNames(ctx, requested)
	if err != nil {
		return nil, mapAppdataError(err)
	}
	body, err := json.Marshal(job.AppdataBackupParams{Containers: requested})
	if err != nil {
		return nil, fmt.Errorf("encoding appdata_backup params: %w", err)
	}
	j, err := sched.Submit(ctx, job.TypeAppdataBackup, containerResources(names), body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return j, nil
}

func containerResources(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = "container:" + n
	}
	return out
}

func (h *Handler) StartAppdataBackup(ctx context.Context, req apiv1.OptStartAppdataBackupRequest) (*apiv1.Job, error) {
	j, err := SubmitAppdataBackup(ctx, h.Appdata, h.Scheduler, req.Value.Containers)
	if err != nil {
		return nil, err
	}
	return jobToAPI(j)
}

func (h *Handler) ListAppdataArchives(ctx context.Context, params apiv1.ListAppdataArchivesParams) (*apiv1.ListAppdataArchivesOK, error) {
	if h.Appdata == nil {
		return nil, errAppdataNotConfigured()
	}
	archives, unavailable, err := h.Appdata.ListArchives(ctx, params.Container.Or(""))
	if err != nil {
		return nil, mapAppdataError(err)
	}
	out := &apiv1.ListAppdataArchivesOK{
		Archives:    make([]apiv1.AppdataArchive, 0, len(archives)),
		Unavailable: make([]apiv1.ListAppdataArchivesOKUnavailableItem, 0, len(unavailable)),
	}
	for _, a := range archives {
		item := apiv1.AppdataArchive{
			Name: a.Name, Container: a.Container, DestinationId: a.DestinationID, DestinationName: a.DestinationName,
			CreatedAt: a.ModTime, Size: a.Size, Encrypted: a.Encrypted,
		}
		if a.Reason != backup.ReasonNone {
			item.Reason = apiv1.NewOptNilString(string(a.Reason))
		}
		out.Archives = append(out.Archives, item)
	}
	for _, u := range unavailable {
		out.Unavailable = append(out.Unavailable, apiv1.ListAppdataArchivesOKUnavailableItem{DestinationId: u.DestinationID, Message: u.Message})
	}
	return out, nil
}

func (h *Handler) RestoreAppdata(ctx context.Context, req *apiv1.RestoreAppdataRequest) (*apiv1.Job, error) {
	if h.Appdata == nil {
		return nil, errAppdataNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	if !req.Confirm {
		return nil, &apiError{code: "confirmation_required", statusCode: 400, message: "restoring overwrites the container's appdata: send confirm: true"}
	}
	if err := h.Appdata.RequireArrayRunning(); err != nil {
		return nil, mapAppdataError(err)
	}
	if err := h.Appdata.FindArchive(ctx, req.Container, req.Archive, req.DestinationId); err != nil {
		return nil, mapAppdataError(err)
	}
	body, err := json.Marshal(job.AppdataRestoreParams{Container: req.Container, Archive: req.Archive, DestinationID: req.DestinationId})
	if err != nil {
		return nil, fmt.Errorf("encoding appdata_restore params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeAppdataRestore, []string{"container:" + req.Container}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}
