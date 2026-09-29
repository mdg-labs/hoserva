package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
)

// mockApps is deterministic per instance (doc 06 §8): every screen must be
// reachable with no real Docker daemon behind this mock. Every container
// here is, honestly, unmanaged — this part builds no stacks table (#278
// is where "managed" starts meaning something), so nothing in this list
// claims otherwise.
func mockApps() []apiv1.App {
	return []apiv1.App{
		{
			ID:     "3f2a9c1e4b5d",
			Name:   "jellyfin",
			Image:  "lscr.io/linuxserver/jellyfin",
			Tag:    "10.9.7",
			State:  apiv1.AppStateRunning,
			Status: "Up 3 hours (healthy)",
			Health: apiv1.AppHealthHealthy,
			Ports: []apiv1.AppPort{
				{
					HostIP:        apiv1.NewOptString("0.0.0.0"),
					HostPort:      apiv1.NewOptInt(8096),
					ContainerPort: 8096,
					Protocol:      apiv1.AppPortProtocolTCP,
				},
			},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/jellyfin"),
					Destination: "/config",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
				{
					Source:      apiv1.NewOptString("/mnt/user/media"),
					Destination: "/data/media",
					Mode:        apiv1.NewOptString("ro"),
					ReadWrite:   false,
				},
			},
		},
		{
			ID:     "7d3f1a9e5c20",
			Name:   "postgres",
			Image:  "postgres",
			Tag:    "16.4",
			State:  apiv1.AppStateRunning,
			Status: "Up 3 hours",
			Health: apiv1.AppHealthNone,
			Ports:  []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/postgres"),
					Destination: "/var/lib/postgresql/data",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
			},
		},
		{
			ID:     "9b1d7e2f6a3c",
			Name:   "portainer",
			Image:  "portainer/portainer-ce",
			Tag:    "2.21.4",
			State:  apiv1.AppStateExited,
			Status: "Exited (0) 2 days ago",
			Health: apiv1.AppHealthNone,
			Ports:  []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/var/run/docker.sock"),
					Destination: "/var/run/docker.sock",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
			},
		},
		{
			ID:     "4c8e0d2a7b91",
			Name:   "transcoder",
			Image:  "example/transcoder",
			Tag:    "1.4.0",
			State:  apiv1.AppStateExited,
			Status: "Exited (0) 5 hours ago",
			Health: apiv1.AppHealthNone,
			Ports:  []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/jellyfin/transcode"),
					Destination: "/transcode",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
				},
			},
		},
	}
}

func mockAppImages() []apiv1.AppImage {
	return []apiv1.AppImage{
		{
			ID:        "sha256:1a2b3c4d5e6f",
			RepoTags:  []string{"lscr.io/linuxserver/jellyfin:10.9.7"},
			SizeBytes: 456 * 1024 * 1024,
			CreatedAt: apiv1.NewOptDateTime(time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)),
		},
		{
			ID:        "sha256:6f5e4d3c2b1a",
			RepoTags:  []string{"portainer/portainer-ce:2.21.4"},
			SizeBytes: 289 * 1024 * 1024,
			CreatedAt: apiv1.NewOptDateTime(time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)),
		},
	}
}

func (h *handler) ListApps(ctx context.Context) (*apiv1.ListAppsOK, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	return &apiv1.ListAppsOK{Available: true, Apps: append([]apiv1.App{}, h.apps...)}, nil
}

func errAppNotFound(id string) error {
	return &mockError{code: "app_not_found", statusCode: 404, message: fmt.Sprintf("no container %q", id)}
}

// findApp returns the index of the container with this ID or name, with
// h.appsMu held by the caller.
func (h *handler) findApp(id string) (int, error) {
	for i, app := range h.apps {
		if app.ID == id || app.Name == id {
			return i, nil
		}
	}
	return -1, errAppNotFound(id)
}

func (h *handler) GetApp(ctx context.Context, params apiv1.GetAppParams) (*apiv1.App, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	i, err := h.findApp(params.ID)
	if err != nil {
		return nil, err
	}
	app := h.apps[i]
	return &app, nil
}

func (h *handler) setAppState(id string, state apiv1.AppState, status string) (*apiv1.App, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	i, err := h.findApp(id)
	if err != nil {
		return nil, err
	}
	h.apps[i].State = state
	h.apps[i].Status = status
	app := h.apps[i]
	return &app, nil
}

// requireArrayRunning mirrors container.Lifecycle.RequireArrayRunning as
// hoservad wires it: Start, Restart, Recreate and RemoveApp with
// deleteAppdata are refused, before
// anything else is looked at, while the array is in maintenance mode or the
// storage target has not been reached. In hoservad the storage target is
// never reached with no array configured, and with a degraded array only
// once it is acknowledged; the mock stands that in with the scenario's own
// array layout and its degraded acknowledgement.
func (h *handler) requireArrayRunning() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return &mockError{code: "array_stopped", statusCode: 409, message: "container: the array is stopped: it is in maintenance mode — start the array first"}
	}
	if mockArrayDisks(h.scenario) == nil || (h.scenario == "degraded" && !h.degradedAcknowledged) {
		return &mockError{code: "array_stopped", statusCode: 409, message: "container: the array is stopped: its storage is not ready — wait for the array to come up"}
	}
	return nil
}

func (h *handler) StartApp(ctx context.Context, params apiv1.StartAppParams) (*apiv1.App, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	return h.setAppState(params.ID, apiv1.AppStateRunning, "Up 1 second")
}

func (h *handler) StopApp(ctx context.Context, params apiv1.StopAppParams) (*apiv1.App, error) {
	return h.setAppState(params.ID, apiv1.AppStateExited, "Exited (0) just now")
}

func (h *handler) RestartApp(ctx context.Context, params apiv1.RestartAppParams) (*apiv1.App, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	return h.setAppState(params.ID, apiv1.AppStateRunning, "Up 1 second")
}

// RecreateApp records the queued job like every other mock job
// submission: this mock has no scheduler. Like production, it refuses an
// array that is not running with array_stopped before it looks for the
// container; production's Scheduler.Submit would refuse new jobs during
// maintenance mode (Q70) only after that.
func (h *handler) RecreateApp(ctx context.Context, params apiv1.RecreateAppParams) (*apiv1.Job, error) {
	if err := h.requireArrayRunning(); err != nil {
		return nil, err
	}
	h.appsMu.Lock()
	_, err := h.findApp(params.ID)
	h.appsMu.Unlock()
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	job := apiv1.Job{
		ID:        uuid.New(),
		Type:      apiv1.JobTypeContainerRecreate,
		Class:     apiv1.JobClassService,
		Status:    apiv1.JobStatusQueued,
		CreatedAt: time.Now().UTC(),
	}
	h.jobs[job.ID] = job
	return &job, nil
}

const mockAppdataRoot = "/mnt/cache/appdata/"

// RemoveApp mirrors production: asking for appdata deletion is refused with
// array_stopped, before anything else is looked at, while the array is not
// running (a plain remove is not); only a stopped container is removed, and
// appdata is deleted only when asked for — then only the mounts inside the
// appdata location, which the mock reports without touching any disk. The
// location is the cache disk's, as in hoservad (container.CacheAppdataRoots):
// no scenario has a cache disk, so asking for appdata deletion is refused
// with appdata_unavailable everywhere the mock has an array.
func (h *handler) RemoveApp(ctx context.Context, params apiv1.RemoveAppParams) (*apiv1.RemoveAppResult, error) {
	if params.DeleteAppdata.Or(false) {
		if err := h.requireArrayRunning(); err != nil {
			return nil, err
		}
	}
	return h.removeApp(params, container.CacheAppdataRoots(mockArrayDisks(h.scenario)) != nil)
}

func (h *handler) removeApp(params apiv1.RemoveAppParams, appdataAvailable bool) (*apiv1.RemoveAppResult, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	i, err := h.findApp(params.ID)
	if err != nil {
		return nil, err
	}
	app := h.apps[i]
	switch app.State {
	case apiv1.AppStateCreated, apiv1.AppStateExited, apiv1.AppStateDead:
	default:
		return nil, &mockError{code: "app_running", statusCode: 409, message: fmt.Sprintf("container %q is not stopped — stop it first", params.ID)}
	}
	deleted := []string{}
	if params.DeleteAppdata.Or(false) {
		if !appdataAvailable {
			return nil, &mockError{code: "appdata_unavailable", statusCode: 409, message: "no appdata location is configured, so appdata cannot be deleted"}
		}
		for _, m := range app.Mounts {
			if src, ok := m.Source.Get(); ok && strings.HasPrefix(src, mockAppdataRoot) {
				if other, shared := h.appdataSharedWith(app.ID, src); shared {
					return nil, &mockError{code: "appdata_shared", statusCode: 409, message: fmt.Sprintf("appdata is shared with another container: %s is used by %s", src, other)}
				}
				deleted = append(deleted, src)
			}
		}
	}
	h.apps = append(h.apps[:i], h.apps[i+1:]...)
	return &apiv1.RemoveAppResult{DeletedPaths: deleted}, nil
}

// appdataSharedWith mirrors container.planAppdataDeletion's shared-appdata
// refusal: another container mounts src, something inside it, or a
// directory of appdata that contains it. h.appsMu is held by the caller.
func (h *handler) appdataSharedWith(id, src string) (string, bool) {
	for _, o := range h.apps {
		if o.ID == id {
			continue
		}
		for _, m := range o.Mounts {
			other, ok := m.Source.Get()
			if !ok {
				continue
			}
			inside := strings.HasPrefix(src, other+"/") && strings.HasPrefix(other, mockAppdataRoot)
			if other == src || strings.HasPrefix(other, src+"/") || inside {
				return o.Name, true
			}
		}
	}
	return "", false
}

func (h *handler) GetAppLogs(ctx context.Context, params apiv1.GetAppLogsParams) (apiv1.GetAppLogsOK, error) {
	h.appsMu.Lock()
	i, err := h.findApp(params.ID)
	var name string
	if err == nil {
		name = h.apps[i].Name
	}
	h.appsMu.Unlock()
	if err != nil {
		return apiv1.GetAppLogsOK{}, err
	}

	lines := []string{
		fmt.Sprintf("[%s] starting", name),
		fmt.Sprintf("[%s] listening", name),
		fmt.Sprintf("[%s] ready", name),
	}
	if tail := int(params.Tail.Or(200)); tail < len(lines) {
		lines = lines[len(lines)-tail:]
	}
	head := strings.Join(lines, "\n")
	if len(lines) > 0 {
		head += "\n"
	}
	if !params.Follow.Or(false) {
		return apiv1.GetAppLogsOK{Data: strings.NewReader(head)}, nil
	}
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		if _, err := io.WriteString(pw, head); err != nil {
			return
		}
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(pw, "[%s] heartbeat %d\n", name, n); err != nil {
					return
				}
			}
		}
	}()
	return apiv1.GetAppLogsOK{Data: pr}, nil
}

// flushAppLogs flushes a container's log response after every write, so a
// followed log shows each heartbeat as it is written instead of when the
// response buffer fills — the production daemon does the same
// (internal/api.FlushLogStream).
func flushAppLogs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/apps/") && strings.HasSuffix(r.URL.Path, "/logs") {
			w = &flushingWriter{ResponseWriter: w, flusher: f}
		}
		next.ServeHTTP(w, r)
	})
}

type flushingWriter struct {
	http.ResponseWriter
	flusher http.Flusher
}

func (w *flushingWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	w.flusher.Flush()
}

func (w *flushingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.flusher.Flush()
	return n, err
}

func (h *handler) GetAppStats(ctx context.Context, params apiv1.GetAppStatsParams) (*apiv1.AppStats, error) {
	h.appsMu.Lock()
	defer h.appsMu.Unlock()
	i, err := h.findApp(params.ID)
	if err != nil {
		return nil, err
	}
	if h.apps[i].State != apiv1.AppStateRunning {
		return nil, &mockError{code: "app_not_running", statusCode: 409, message: fmt.Sprintf("container %q is not running", params.ID)}
	}
	return &apiv1.AppStats{
		At:               time.Now().UTC(),
		CpuPercent:       12.5,
		MemoryBytes:      512 * 1024 * 1024,
		MemoryLimitBytes: 4 * 1024 * 1024 * 1024,
		NetworkRxBytes:   48 * 1024 * 1024,
		NetworkTxBytes:   9 * 1024 * 1024,
		BlockReadBytes:   130 * 1024 * 1024,
		BlockWriteBytes:  12 * 1024 * 1024,
	}, nil
}

func (h *handler) ListAppImages(ctx context.Context) (*apiv1.ListAppImagesOK, error) {
	return &apiv1.ListAppImagesOK{Available: true, Images: mockAppImages()}, nil
}
