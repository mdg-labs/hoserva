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
)

const defaultLogTail = 200

func errLifecycleNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "Docker is not configured on this daemon"}
}

// mapContainerError turns a container.Lifecycle error into the API's
// error shape. An Engine failure while acting on a container (a port that
// is taken, a mount that is missing) is reported with the Engine's own
// message, since that is what the person needs in order to fix it; every
// other unclassified error stays an opaque 500.
func mapContainerError(id string, err error, verb string) error {
	switch {
	case errors.Is(err, container.ErrNotFound):
		return errAppNotFound(id)
	case errors.Is(err, container.ErrUnavailable):
		return &apiError{code: "docker_unavailable", statusCode: 503, message: fmt.Sprintf("Docker is not installed or not reachable: %v", err)}
	case errors.Is(err, container.ErrRunning):
		return &apiError{code: "app_running", statusCode: 409, message: fmt.Sprintf("container %q is not stopped — stop it first", id)}
	case errors.Is(err, container.ErrNotRunning):
		return &apiError{code: "app_not_running", statusCode: 409, message: fmt.Sprintf("container %q is not running", id)}
	case errors.Is(err, container.ErrAppdataShared):
		return &apiError{code: "appdata_shared", statusCode: 409, message: err.Error()}
	case errors.Is(err, container.ErrAppdataUnavailable):
		return &apiError{code: "appdata_unavailable", statusCode: 409, message: err.Error()}
	default:
		return &apiError{code: "app_action_failed", statusCode: 502, message: fmt.Sprintf("%s container %q failed: %v", verb, id, err)}
	}
}

func (h *Handler) StartApp(ctx context.Context, params apiv1.StartAppParams) (*apiv1.App, error) {
	return h.appAction(ctx, params.ID, "starting", (*container.Lifecycle).Start)
}

func (h *Handler) StopApp(ctx context.Context, params apiv1.StopAppParams) (*apiv1.App, error) {
	return h.appAction(ctx, params.ID, "stopping", (*container.Lifecycle).Stop)
}

func (h *Handler) RestartApp(ctx context.Context, params apiv1.RestartAppParams) (*apiv1.App, error) {
	return h.appAction(ctx, params.ID, "restarting", (*container.Lifecycle).Restart)
}

func (h *Handler) appAction(ctx context.Context, id, verb string, do func(*container.Lifecycle, context.Context, string) (container.Container, error)) (*apiv1.App, error) {
	if h.Lifecycle == nil {
		return nil, errLifecycleNotConfigured()
	}
	c, err := do(h.Lifecycle, ctx, id)
	if err != nil {
		return nil, mapContainerError(id, err, verb)
	}
	app := containerToAPI(c)
	return &app, nil
}

// RecreateApp queues the recreate as a job: it pulls an image, which can
// take minutes, and must not hold a request open (doc 01 §4).
func (h *Handler) RecreateApp(ctx context.Context, params apiv1.RecreateAppParams) (*apiv1.Job, error) {
	if h.Lifecycle == nil {
		return nil, errLifecycleNotConfigured()
	}
	if h.Scheduler == nil {
		return nil, fmt.Errorf("job scheduler not configured")
	}
	c, err := h.Lifecycle.Provider.Inspect(ctx, params.ID)
	if err != nil {
		return nil, mapContainerError(params.ID, err, "recreating")
	}
	body, err := json.Marshal(job.ContainerRecreateParams{ID: c.Name})
	if err != nil {
		return nil, fmt.Errorf("encoding container_recreate params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeContainerRecreate, []string{"container:" + c.Name}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

func (h *Handler) RemoveApp(ctx context.Context, params apiv1.RemoveAppParams) (*apiv1.RemoveAppResult, error) {
	if h.Lifecycle == nil {
		return nil, errLifecycleNotConfigured()
	}
	res, err := h.Lifecycle.Remove(ctx, params.ID, params.DeleteAppdata.Or(false))
	if err != nil {
		if len(res.DeletedPaths) > 0 {
			return nil, &apiError{code: "appdata_delete_incomplete", statusCode: 500, message: fmt.Sprintf("%v (deleted: %v)", err, res.DeletedPaths)}
		}
		return nil, mapContainerError(params.ID, err, "removing")
	}
	paths := res.DeletedPaths
	if paths == nil {
		paths = []string{}
	}
	return &apiv1.RemoveAppResult{DeletedPaths: paths}, nil
}

func (h *Handler) GetAppLogs(ctx context.Context, params apiv1.GetAppLogsParams) (apiv1.GetAppLogsOK, error) {
	if h.Lifecycle == nil {
		return apiv1.GetAppLogsOK{}, errLifecycleNotConfigured()
	}
	rc, err := h.Lifecycle.Logs(ctx, params.ID, container.LogOptions{
		Tail:   int(params.Tail.Or(defaultLogTail)),
		Follow: params.Follow.Or(false),
	})
	if err != nil {
		return apiv1.GetAppLogsOK{}, mapContainerError(params.ID, err, "reading logs of")
	}
	return apiv1.GetAppLogsOK{Data: rc}, nil
}

func (h *Handler) GetAppStats(ctx context.Context, params apiv1.GetAppStatsParams) (*apiv1.AppStats, error) {
	if h.Lifecycle == nil {
		return nil, errLifecycleNotConfigured()
	}
	s, err := h.Lifecycle.Stats(ctx, params.ID)
	if err != nil {
		return nil, mapContainerError(params.ID, err, "reading stats of")
	}
	return &apiv1.AppStats{
		At:               s.At,
		CpuPercent:       s.CPUPercent,
		MemoryBytes:      int64(s.MemoryBytes),
		MemoryLimitBytes: int64(s.MemoryLimitBytes),
		NetworkRxBytes:   int64(s.NetworkRxBytes),
		NetworkTxBytes:   int64(s.NetworkTxBytes),
		BlockReadBytes:   int64(s.BlockReadBytes),
		BlockWriteBytes:  int64(s.BlockWriteBytes),
	}, nil
}
