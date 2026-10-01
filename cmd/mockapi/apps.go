package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// mockApps is deterministic per instance (doc 06 §8): every screen must be
// reachable with no real Docker daemon behind this mock. jellyfin is the one
// container an installed stack manages (mockStack); the rest are started by
// hand, so they are unmanaged. Their mounts cover every storage location kind
// (cache, pool, data disk, outside the array), and each carries the
// created, started and restart facts only getApp reports (withoutRuntime).
func mockApps() []apiv1.App {
	now := time.Now().UTC()
	return []apiv1.App{
		{
			ID:           "3f2a9c1e4b5d",
			Name:         "jellyfin",
			Image:        "lscr.io/linuxserver/jellyfin",
			Tag:          "10.9.7",
			State:        apiv1.AppStateRunning,
			Status:       "Up 3 hours (healthy)",
			Health:       apiv1.AppHealthHealthy,
			Stack:        apiv1.NewOptString(mockStack),
			CreatedAt:    apiv1.NewOptDateTime(now.Add(-30 * 24 * time.Hour)),
			StartedAt:    apiv1.NewOptDateTime(now.Add(-3 * time.Hour)),
			RestartCount: apiv1.NewOptInt(1),
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
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindCache}),
				},
				{
					Source:      apiv1.NewOptString("/mnt/user/media"),
					Destination: "/data/media",
					Mode:        apiv1.NewOptString("ro"),
					ReadWrite:   false,
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindPool, Share: apiv1.NewOptString("media")}),
				},
			},
		},
		{
			ID:           "7d3f1a9e5c20",
			Name:         "postgres",
			Image:        "postgres",
			Tag:          "16.4",
			State:        apiv1.AppStateRunning,
			Status:       "Up 3 hours",
			Health:       apiv1.AppHealthNone,
			CreatedAt:    apiv1.NewOptDateTime(now.Add(-45 * 24 * time.Hour)),
			StartedAt:    apiv1.NewOptDateTime(now.Add(-3 * time.Hour)),
			RestartCount: apiv1.NewOptInt(0),
			Ports:        []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/postgres"),
					Destination: "/var/lib/postgresql/data",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindCache}),
				},
			},
		},
		{
			ID:           "9b1d7e2f6a3c",
			Name:         "portainer",
			Image:        "portainer/portainer-ce",
			Tag:          "2.21.4",
			State:        apiv1.AppStateExited,
			Status:       "Exited (0) 2 days ago",
			Health:       apiv1.AppHealthNone,
			CreatedAt:    apiv1.NewOptDateTime(now.Add(-60 * 24 * time.Hour)),
			StartedAt:    apiv1.NewOptDateTime(now.Add(-50 * time.Hour)),
			RestartCount: apiv1.NewOptInt(0),
			Ports:        []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/var/run/docker.sock"),
					Destination: "/var/run/docker.sock",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindOutside}),
				},
			},
		},
		{
			ID:           "4c8e0d2a7b91",
			Name:         "transcoder",
			Image:        "example/transcoder",
			Tag:          "1.4.0",
			State:        apiv1.AppStateExited,
			Status:       "Exited (0) 5 hours ago",
			Health:       apiv1.AppHealthNone,
			CreatedAt:    apiv1.NewOptDateTime(now.Add(-7 * 24 * time.Hour)),
			StartedAt:    apiv1.NewOptDateTime(now.Add(-6 * time.Hour)),
			RestartCount: apiv1.NewOptInt(4),
			Ports:        []apiv1.AppPort{},
			Mounts: []apiv1.AppMount{
				{
					Source:      apiv1.NewOptString("/mnt/cache/appdata/jellyfin/transcode"),
					Destination: "/transcode",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindCache}),
				},
				{
					Source:      apiv1.NewOptString("/mnt/disk1/scratch"),
					Destination: "/scratch",
					Mode:        apiv1.NewOptString("rw"),
					ReadWrite:   true,
					Location:    apiv1.NewOptAppMountLocation(apiv1.AppMountLocation{Kind: apiv1.AppMountLocationKindDisk, Disk: apiv1.NewOptInt(1)}),
				},
			},
		},
	}
}

// mockStack is the installed stack that manages jellyfin. It is not named
// after a catalog template, so installing that template through the mock is
// never refused as an existing stack.
const mockStack = "media-server"

// mockStacksFor is the stacks table that goes with apps: one stack for every
// stack name an app reports, so GetStack answers for what listApps names.
func mockStacksFor(apps []apiv1.App) map[string]apiv1.Stack {
	stacks := map[string]apiv1.Stack{}
	for _, a := range apps {
		if name, ok := a.Stack.Get(); ok {
			stacks[name] = apiv1.Stack{
				Name:        name,
				Template:    apiv1.StackTemplate{Source: "hoserva", ID: "jellyfin", Revision: "1"},
				InstalledAt: time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC),
			}
		}
	}
	return stacks
}

// scenarioApps is the Docker a scenario reports: its apps.json fixture when
// it has one (the empty Docker of fresh-install, the missing Docker of
// migration-pending), the mock's default containers otherwise. A non-empty
// down is the reason Docker is unreachable.
func scenarioApps(scenario string) (apps []apiv1.App, down string, err error) {
	raw, err := fixtures.AppsJSON(scenario)
	if errors.Is(err, fs.ErrNotExist) {
		return mockApps(), "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("load apps fixture: %w", err)
	}
	var list apiv1.ListAppsOK
	if err := list.UnmarshalJSON(raw); err != nil {
		return nil, "", fmt.Errorf("decode apps fixture: %w", err)
	}
	if err := list.Validate(); err != nil {
		return nil, "", fmt.Errorf("apps fixture fails validation: %w", err)
	}
	if !list.Available {
		return list.Apps, list.Message.Or("Docker is not installed or not reachable"), nil
	}
	return list.Apps, "", nil
}

func errDockerUnavailable(reason string) error {
	return &mockError{code: "docker_unavailable", statusCode: 503, message: reason}
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
	if h.appsDown != "" {
		return &apiv1.ListAppsOK{Available: false, Apps: []apiv1.App{}, Message: apiv1.NewOptString(h.appsDown)}, nil
	}
	apps := make([]apiv1.App, 0, len(h.apps))
	for _, app := range h.apps {
		apps = append(apps, withoutRuntime(app))
	}
	return &apiv1.ListAppsOK{Available: true, Apps: apps}, nil
}

// withoutRuntime is the app as listApps and the start, stop and restart
// operations report it, which carries neither the created, started and
// restart facts nor, for those three, the mounts' locations; only getApp
// reports them.
func withoutRuntime(app apiv1.App) apiv1.App {
	app.CreatedAt = apiv1.OptDateTime{}
	app.StartedAt = apiv1.OptDateTime{}
	app.RestartCount = apiv1.OptInt{}
	return app
}

// withoutLocations is withoutRuntime for the start, stop and restart
// responses, which do not place the mounts either.
func withoutLocations(app apiv1.App) apiv1.App {
	app = withoutRuntime(app)
	mounts := make([]apiv1.AppMount, len(app.Mounts))
	for i, m := range app.Mounts {
		m.Location = apiv1.OptAppMountLocation{}
		mounts[i] = m
	}
	app.Mounts = mounts
	return app
}

func errAppNotFound(id string) error {
	return &mockError{code: "app_not_found", statusCode: 404, message: fmt.Sprintf("no container %q", id)}
}

// findApp returns the index of the container with this ID or name, with
// h.appsMu held by the caller.
func (h *handler) findApp(id string) (int, error) {
	if h.appsDown != "" {
		return -1, errDockerUnavailable(h.appsDown)
	}
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
	if state == apiv1.AppStateRunning {
		h.apps[i].StartedAt = apiv1.NewOptDateTime(time.Now().UTC())
	}
	app := withoutLocations(h.apps[i])
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
	return h.queueServiceJob(apiv1.JobTypeContainerRecreate)
}

// queueServiceJob records a queued service-class job, refused during
// maintenance mode like production's Scheduler.Submit.
func (h *handler) queueServiceJob(t apiv1.JobType) (*apiv1.Job, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maintenance {
		return nil, errMaintenanceMode()
	}
	job := apiv1.Job{
		ID:        uuid.New(),
		Type:      t,
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

// flushAppLogs flushes a container's or a job's log response after every
// write, so a followed log shows each heartbeat as it is written instead of
// when the response buffer fills — the production daemon does the same
// (internal/api.FlushLogStream).
func flushAppLogs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAppLogs := strings.HasPrefix(r.URL.Path, "/api/v1/apps/") && strings.HasSuffix(r.URL.Path, "/logs")
		isJobLog := strings.HasPrefix(r.URL.Path, "/api/v1/jobs/") && strings.HasSuffix(r.URL.Path, "/log")
		if f, ok := w.(http.Flusher); ok && r.Method == http.MethodGet && (isAppLogs || isJobLog) {
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
	h.appsMu.Lock()
	down := h.appsDown
	h.appsMu.Unlock()
	if down != "" {
		return &apiv1.ListAppImagesOK{Available: false, Images: []apiv1.AppImage{}, Message: apiv1.NewOptString(down)}, nil
	}
	return &apiv1.ListAppImagesOK{Available: true, Images: mockAppImages()}, nil
}

// ListAppUpdates answers a fixed mix of every status, so each label the UI
// shows for a check result is reachable without a registry: by container
// name, and not_checked for any other.
func (h *handler) ListAppUpdates(ctx context.Context) (*apiv1.ListAppUpdatesOK, error) {
	h.appsMu.Lock()
	apps := append([]apiv1.App{}, h.apps...)
	down := h.appsDown
	excluded := make(map[string]bool, len(h.bulkExcluded))
	for n, ex := range h.bulkExcluded {
		excluded[n] = ex
	}
	h.appsMu.Unlock()
	if down != "" {
		return &apiv1.ListAppUpdatesOK{Available: false, Updates: []apiv1.AppUpdate{}, Message: apiv1.NewOptString(down)}, nil
	}
	checkedAt := time.Date(2026, 9, 30, 6, 14, 0, 0, time.UTC)
	updates := make([]apiv1.AppUpdate, 0, len(apps))
	for _, a := range apps {
		u := apiv1.AppUpdate{Container: a.Name, Image: a.Image, Tag: a.Tag, Status: apiv1.AppUpdateStatusNotChecked, BulkExcluded: apiv1.NewOptBool(excluded[a.Name])}
		switch a.Name {
		case "jellyfin":
			u.Status = apiv1.AppUpdateStatusUpdateAvailable
			u.Kind = apiv1.NewOptAppUpdateKind(apiv1.AppUpdateKindNewVersion)
			u.AvailableTag = apiv1.NewOptString("10.10.3")
			u.CheckedAt = apiv1.NewOptDateTime(checkedAt)
		case "postgres":
			u.Status = apiv1.AppUpdateStatusUpToDate
			u.CheckedAt = apiv1.NewOptDateTime(checkedAt)
		case "transcoder":
			u.Status = apiv1.AppUpdateStatusNotChecked
			u.Message = apiv1.NewOptString("the container is pinned to an image digest, so there is no tag to look for an update of")
		case "portainer":
			u.Status = apiv1.AppUpdateStatusSkipped
			u.Message = apiv1.NewOptString("the registry is rate limiting requests; skipped until the next daily check")
			u.CheckedAt = apiv1.NewOptDateTime(checkedAt)
		}
		updates = append(updates, u)
	}
	return &apiv1.ListAppUpdatesOK{Available: true, Updates: updates}, nil
}
