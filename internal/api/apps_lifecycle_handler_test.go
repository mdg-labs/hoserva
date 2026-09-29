package api_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

type appsFixture struct {
	h     *api.Handler
	fake  *container.FakeProvider
	hub   *container.Hub
	sched *job.Scheduler
	// stopped and storageDown are the array state Lifecycle reads.
	stopped     atomic.Bool
	storageDown atomic.Bool
	root        string
	cfgDir      string
}

func newAppsFixture(t *testing.T) *appsFixture {
	t.Helper()
	h, sched, reg := newTestHandler(t)
	base := t.TempDir()
	f := &appsFixture{
		h:      h,
		fake:   container.NewFakeProvider(),
		hub:    container.NewHub(),
		sched:  sched,
		root:   filepath.Join(base, "appdata"),
		cfgDir: filepath.Join(base, "appdata", "jellyfin"),
	}
	if err := os.MkdirAll(f.cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfgDir, "library.db"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.fake.AddContainer(container.Container{
		ID: "c1", Name: "jellyfin", Image: "jf", Tag: "10", State: "exited", Health: container.HealthNone,
		Mounts: []container.Mount{{Source: f.cfgDir, Destination: "/config", ReadWrite: true}},
	})
	life := &container.Lifecycle{
		Provider:     f.fake,
		Hub:          f.hub,
		Halted:       f.stopped.Load,
		StorageReady: func() bool { return !f.storageDown.Load() },
		AppdataRoots: func(context.Context) ([]string, error) { return []string{f.root}, nil },
	}
	h.Container = f.fake
	h.Lifecycle = life
	reg.Register(job.TypeContainerRecreate, false, job.RunContainerRecreate(func(ctx context.Context, id string) error {
		_, err := life.Recreate(ctx, id)
		return err
	}))
	return f
}

func statusOf(h *api.Handler, err error) (int, string) {
	if err == nil {
		return 200, ""
	}
	e := h.NewError(context.Background(), err)
	return e.StatusCode, e.Response.Code
}

func TestHandler_StartStopRestartApp(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()
	ch, unsub := f.hub.Subscribe()
	defer unsub()

	app, err := f.h.StartApp(ctx, apiv1.StartAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("StartApp: %v", err)
	}
	if app.State != apiv1.AppStateRunning {
		t.Fatalf("StartApp state = %s, want running", app.State)
	}
	if sc := <-ch; sc.State != "running" {
		t.Fatalf("published %+v, want running", sc)
	}
	if app, err = f.h.RestartApp(ctx, apiv1.RestartAppParams{ID: "jellyfin"}); err != nil || app.State != apiv1.AppStateRunning {
		t.Fatalf("RestartApp = %+v, %v", app, err)
	}
	<-ch
	if app, err = f.h.StopApp(ctx, apiv1.StopAppParams{ID: "jellyfin"}); err != nil || app.State != apiv1.AppStateExited {
		t.Fatalf("StopApp = %+v, %v", app, err)
	}
	if sc := <-ch; sc.State != "exited" {
		t.Fatalf("published %+v, want exited", sc)
	}
}

func TestHandler_AppActionsErrors(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()

	_, err := f.h.StartApp(ctx, apiv1.StartAppParams{ID: "nope"})
	if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
		t.Fatalf("unknown container = %d %s, want 404 app_not_found", st, code)
	}
	f.fake.FailOn("start", "c1", errors.New("port is already allocated"))
	_, err = f.h.StartApp(ctx, apiv1.StartAppParams{ID: "jellyfin"})
	st, code := statusOf(f.h, err)
	if st != 502 || code != "app_action_failed" {
		t.Fatalf("engine failure = %d %s, want 502 app_action_failed", st, code)
	}
	if !strings.Contains(f.h.NewError(ctx, err).Response.Message, "port is already allocated") {
		t.Fatalf("message = %q, want the Engine's own reason", f.h.NewError(ctx, err).Response.Message)
	}
	f.fake.SetUnavailable(nil)
	_, err = f.h.StopApp(ctx, apiv1.StopAppParams{ID: "jellyfin"})
	if st, code := statusOf(f.h, err); st != 503 || code != "docker_unavailable" {
		t.Fatalf("no Docker = %d %s, want 503 docker_unavailable", st, code)
	}
}

func TestHandler_LifecycleNotConfigured(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	_, err := h.StartApp(ctx, apiv1.StartAppParams{ID: "x"})
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("StartApp = %d %s, want 501 not_configured", st, code)
	}
	_, err = h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "x"})
	if st, _ := statusOf(h, err); st != 501 {
		t.Fatalf("RemoveApp = %d, want 501", st)
	}
	_, err = h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "x"})
	if st, _ := statusOf(h, err); st != 501 {
		t.Fatalf("RecreateApp = %d, want 501", st)
	}
}

// The data-loss scenario at the API boundary: a remove request that does
// not say deleteAppdata must leave the appdata alone.
func TestHandler_RemoveApp_KeepsAppdataUnlessAskedTo(t *testing.T) {
	f := newAppsFixture(t)

	res, err := f.h.RemoveApp(context.Background(), apiv1.RemoveAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}
	if len(res.DeletedPaths) != 0 {
		t.Fatalf("DeletedPaths = %v, want none", res.DeletedPaths)
	}
	if _, err := os.Stat(filepath.Join(f.cfgDir, "library.db")); err != nil {
		t.Fatalf("appdata gone after a remove that did not ask for it: %v", err)
	}
	if _, err := f.fake.Inspect(context.Background(), "jellyfin"); !errors.Is(err, container.ErrNotFound) {
		t.Fatalf("container still exists: %v", err)
	}
}

func TestHandler_RemoveApp_DeleteAppdata(t *testing.T) {
	f := newAppsFixture(t)

	res, err := f.h.RemoveApp(context.Background(), apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
	if err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}
	if len(res.DeletedPaths) != 1 {
		t.Fatalf("DeletedPaths = %v, want the one appdata directory", res.DeletedPaths)
	}
	if _, err := os.Stat(f.cfgDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("appdata directory still exists: %v", err)
	}
}

func TestHandler_RemoveApp_Refusals(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()

	if err := f.fake.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	_, err := f.h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
	if st, code := statusOf(f.h, err); st != 409 || code != "app_running" {
		t.Fatalf("running container = %d %s, want 409 app_running", st, code)
	}
	if err := f.fake.Stop(ctx, "c1"); err != nil {
		t.Fatal(err)
	}

	f.fake.AddContainer(container.Container{
		ID: "c2", Name: "other", State: "running",
		Mounts: []container.Mount{{Source: f.cfgDir, Destination: "/x", ReadWrite: true}},
	})
	_, err = f.h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
	if st, code := statusOf(f.h, err); st != 409 || code != "appdata_shared" {
		t.Fatalf("shared appdata = %d %s, want 409 appdata_shared", st, code)
	}
	if _, err := os.Stat(filepath.Join(f.cfgDir, "library.db")); err != nil {
		t.Fatalf("appdata deleted by a refused request: %v", err)
	}
	if _, err := f.fake.Inspect(ctx, "jellyfin"); err != nil {
		t.Fatalf("container removed by a refused request: %v", err)
	}
}

func TestHandler_RecreateApp_RunsAsAServiceJob(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()

	j, err := f.h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("RecreateApp: %v", err)
	}
	if j.Type != apiv1.JobTypeContainerRecreate || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want container_recreate in the service class", j.Type, j.Class)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	var recreated bool
	for _, c := range f.fake.Calls() {
		recreated = recreated || (c.Op == "recreate" && c.ID == "c1")
	}
	if !recreated {
		t.Fatalf("the job did not recreate the container: %v", f.fake.Calls())
	}
}

// A recreate whose pull or create fails fails the job and leaves the
// container as it was.
func TestHandler_RecreateApp_FailureFailsTheJobAndKeepsTheContainer(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()
	if err := f.fake.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	f.fake.FailOn("recreate", "c1", errors.New("pull access denied"))

	j, err := f.h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("RecreateApp: %v", err)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "pull access denied") {
		t.Fatalf("job = %s %q, want failed with the pull error", done.Status, done.ErrorMessage)
	}
	c, err := f.fake.Inspect(ctx, "jellyfin")
	if err != nil || c.State != "running" {
		t.Fatalf("container = %+v, %v; want it still running", c, err)
	}
}

func TestHandler_RecreateApp_UnknownContainer(t *testing.T) {
	f := newAppsFixture(t)
	_, err := f.h.RecreateApp(context.Background(), apiv1.RecreateAppParams{ID: "nope"})
	if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
		t.Fatalf("= %d %s, want 404 app_not_found", st, code)
	}
}

func TestHandler_GetAppStatsAndLogs(t *testing.T) {
	f := newAppsFixture(t)
	ctx := context.Background()

	_, err := f.h.GetAppStats(ctx, apiv1.GetAppStatsParams{ID: "jellyfin"})
	if st, code := statusOf(f.h, err); st != 409 || code != "app_not_running" {
		t.Fatalf("stats of a stopped container = %d %s, want 409 app_not_running", st, code)
	}
	if err := f.fake.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	f.fake.SetStats("c1", container.Stats{CPUPercent: 42.5, MemoryBytes: 1 << 20, MemoryLimitBytes: 1 << 30, NetworkRxBytes: 7, NetworkTxBytes: 8, BlockReadBytes: 9, BlockWriteBytes: 10})
	s, err := f.h.GetAppStats(ctx, apiv1.GetAppStatsParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetAppStats: %v", err)
	}
	if s.CpuPercent != 42.5 || s.MemoryBytes != 1<<20 || s.MemoryLimitBytes != 1<<30 || s.NetworkRxBytes != 7 || s.NetworkTxBytes != 8 || s.BlockReadBytes != 9 || s.BlockWriteBytes != 10 {
		t.Fatalf("stats = %+v", s)
	}

	f.fake.SetLogs("c1", "line one\nline two\n")
	logs, err := f.h.GetAppLogs(ctx, apiv1.GetAppLogsParams{ID: "jellyfin", Follow: apiv1.NewOptBool(true)})
	if err != nil {
		t.Fatalf("GetAppLogs: %v", err)
	}
	got, _ := io.ReadAll(logs.Data)
	if string(got) != "line one\nline two\n" {
		t.Fatalf("logs = %q", got)
	}
	_, err = f.h.GetAppLogs(ctx, apiv1.GetAppLogsParams{ID: "nope"})
	if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
		t.Fatalf("logs of an unknown container = %d %s, want 404 app_not_found", st, code)
	}
}

func TestHandler_GetApp_ReportsHealth(t *testing.T) {
	f := newAppsFixture(t)
	f.fake.AddContainer(container.Container{ID: "c9", Name: "sick", State: "running", Health: container.HealthUnhealthy})

	app, err := f.h.GetApp(context.Background(), apiv1.GetAppParams{ID: "sick"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if app.Health != apiv1.AppHealthUnhealthy {
		t.Fatalf("Health = %s, want unhealthy", app.Health)
	}
	plain, err := f.h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil || plain.Health != apiv1.AppHealthNone {
		t.Fatalf("Health = %v, %v; want none", plain, err)
	}
}

// A container killed outside Hoserva reaches an SSE client as a
// container_state event, decoded through the generated events package.
func TestEventsHandler_StreamsContainerStateFromTheEngine(t *testing.T) {
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "c1", Name: "jellyfin", State: "running"})
	hub := container.NewHub()
	w := &container.Watcher{Provider: fake, Hub: hub, Retry: 10 * time.Millisecond}
	wctx, stopWatcher := context.WithCancel(context.Background())
	defer stopWatcher()
	go w.Run(wctx)

	h := &api.EventsHandler{
		Hub:          job.NewHub(),
		ContainerHub: hub,
		Authenticate: func(r *http.Request) error { return nil },
		KeepAlive:    time.Hour,
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := make(chan events.Event, 1)
	go func() {
		ev, err := events.NewReader(resp.Body).Next()
		if err == nil {
			got <- ev
		}
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		// The kill races the subscriptions inside the watcher and the
		// handler; repeat it until the event is seen.
		if err := fake.Kill("c1"); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-got:
			if !ev.IsContainerStateEvent() {
				t.Fatalf("event type = %v, want container_state", ev.Type)
			}
			data := ev.ContainerStateEvent.Data
			if data.Name != "jellyfin" || data.State != events.AppStateExited {
				t.Fatalf("event = %+v, want jellyfin exited", data)
			}
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no container_state event for a killed container")
}

// The data-loss scenario at the API boundary: with the array stopped, or
// its storage not ready, start, restart and recreate are refused with a
// 409 and never reach the Engine, and no recreate job is queued.
func TestHandler_StartRestartRecreate_RefusedWhileTheArrayIsStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		down func(*appsFixture)
	}{
		{"maintenance mode", func(f *appsFixture) { f.stopped.Store(true) }},
		{"storage not ready", func(f *appsFixture) { f.storageDown.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppsFixture(t)
			ctx := context.Background()
			tc.down(f)

			_, err := f.h.StartApp(ctx, apiv1.StartAppParams{ID: "jellyfin"})
			if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
				t.Fatalf("StartApp = %d %s, want 409 array_stopped", st, code)
			}
			_, err = f.h.RestartApp(ctx, apiv1.RestartAppParams{ID: "jellyfin"})
			if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
				t.Fatalf("RestartApp = %d %s, want 409 array_stopped", st, code)
			}
			_, err = f.h.RecreateApp(ctx, apiv1.RecreateAppParams{ID: "jellyfin"})
			if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
				t.Fatalf("RecreateApp = %d %s, want 409 array_stopped", st, code)
			}
			if calls := f.fake.Calls(); len(calls) != 0 {
				t.Fatalf("the Engine saw %v although the array is stopped", calls)
			}
			jobs, err := f.h.Store.List(ctx, job.ListFilter{})
			if err != nil {
				t.Fatalf("listing jobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("a refused recreate still queued %d job(s)", len(jobs))
			}

			if _, err := f.h.StopApp(ctx, apiv1.StopAppParams{ID: "jellyfin"}); err != nil {
				t.Fatalf("StopApp on a stopped array: %v", err)
			}
			if _, err := f.h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin"}); err != nil {
				t.Fatalf("RemoveApp without appdata deletion on a stopped array: %v", err)
			}
		})
	}
}

// The data-loss scenario at the API boundary for appdata deletion: with the
// array stopped, or its storage not ready, the cache disk is not mounted, so
// deleting appdata would remove the container and report success while the
// real appdata stays on the disk. It is refused with a 409, before the
// container is removed, and nothing on disk is touched.
func TestHandler_RemoveApp_DeleteAppdataRefusedWhileTheArrayIsStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		down func(*appsFixture)
	}{
		{"maintenance mode", func(f *appsFixture) { f.stopped.Store(true) }},
		{"storage not ready", func(f *appsFixture) { f.storageDown.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppsFixture(t)
			ctx := context.Background()
			tc.down(f)

			_, err := f.h.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
			if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
				t.Fatalf("RemoveApp with deleteAppdata = %d %s, want 409 array_stopped", st, code)
			}
			if calls := f.fake.Calls(); len(calls) != 0 {
				t.Fatalf("the Engine saw %v although the remove was refused", calls)
			}
			if _, err := f.fake.Inspect(ctx, "jellyfin"); err != nil {
				t.Fatalf("container removed by a refused request: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.cfgDir, "library.db")); err != nil {
				t.Fatalf("appdata touched by a refused request: %v", err)
			}
		})
	}
}

// An array state Lifecycle cannot read refuses too, as a 503 rather than a
// start on a guess.
func TestHandler_Start_RefusedWhenTheArrayStateCannotBeRead(t *testing.T) {
	f := newAppsFixture(t)
	f.h.Lifecycle.Halted = nil

	_, err := f.h.StartApp(context.Background(), apiv1.StartAppParams{ID: "jellyfin"})
	if st, code := statusOf(f.h, err); st != 503 || code != "array_state_unknown" {
		t.Fatalf("StartApp = %d %s, want 503 array_state_unknown", st, code)
	}
	if calls := f.fake.Calls(); len(calls) != 0 {
		t.Fatalf("the Engine saw %v", calls)
	}
}
