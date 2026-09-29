package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

// heldProvider holds the Engine call of start, restart or remove until the
// test lets it go, standing in for a slow dockerd.
type heldProvider struct {
	*container.FakeProvider
	entered chan string
	release chan struct{}
}

func newHeldProvider(fake *container.FakeProvider) *heldProvider {
	return &heldProvider{FakeProvider: fake, entered: make(chan string, 4), release: make(chan struct{})}
}

func (p *heldProvider) hold(op string) {
	p.entered <- op
	<-p.release
}

func (p *heldProvider) Start(ctx context.Context, id string) error {
	p.hold("start")
	return p.FakeProvider.Start(ctx, id)
}

func (p *heldProvider) Restart(ctx context.Context, id string) error {
	p.hold("restart")
	return p.FakeProvider.Restart(ctx, id)
}

func (p *heldProvider) Remove(ctx context.Context, id string, opts container.RemoveOptions) error {
	p.hold("remove")
	return p.FakeProvider.Remove(ctx, id, opts)
}

// cacheMount is a Disks entry whose unmount records whether the appdata
// directory was still there and then makes the cache disappear, the way an
// unmounted cache disk hides its directories.
type cacheMount struct {
	root, appdata string
	sawAppdata    atomic.Bool
}

func (m *cacheMount) Where() string               { return m.root }
func (m *cacheMount) Mount(context.Context) error { return nil }
func (m *cacheMount) Unmount(context.Context) error {
	if _, err := os.Stat(m.appdata); err == nil {
		m.sawAppdata.Store(true)
	}
	return os.RemoveAll(m.root)
}

// stopHarness is the daemon's array stop over the real scheduler, with the
// container service wired by wireContainers the way main.go wires it.
type stopHarness struct {
	w      *maintenanceRestartWiring
	seq    *job.ArraySequence
	fake   *container.FakeProvider
	held   *heldProvider
	cache  *cacheMount
	apps   *appServices
	stopOK chan error
}

func newStopHarness(t *testing.T) *stopHarness {
	t.Helper()
	w := newMaintenanceRestartWiring(t, filepath.Join(t.TempDir(), "apps-stop.db"))
	t.Cleanup(func() { _ = w.db.Close() })

	root := filepath.Join(t.TempDir(), "cache")
	appdata := filepath.Join(root, "appdata", "jellyfin")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdata, "library.db"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{
		ID: "a", Name: "jellyfin", State: "exited",
		Mounts: []container.Mount{{Source: appdata, Destination: "/config", ReadWrite: true}},
	})
	held := newHeldProvider(fake)
	apps := newContainers(held, t.TempDir(), nil, nil)
	apps.Lifecycle.AppdataRoots = func(context.Context) ([]string, error) {
		return []string{filepath.Join(root, "appdata")}, nil
	}
	wireContainers(w.handler, w.registry, apps, w.scheduler.InMaintenance, func() bool { return true }, arrayActionAdmit(w.scheduler))

	var mountCalls int32
	seq := fakeArraySequence(w, &mountCalls)
	seq.Services = []job.ArrayService{apps.arrayService()}
	cache := &cacheMount{root: root, appdata: appdata}
	seq.Disks = []job.ArrayMount{cache}
	w.handler.SetArray(seq)
	return &stopHarness{w: w, seq: seq, fake: fake, held: held, cache: cache, apps: apps, stopOK: make(chan error, 1)}
}

func (h *stopHarness) beginStop(ctx context.Context) {
	go func() {
		_, err := h.w.handler.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true})
		h.stopOK <- err
	}()
}

func (h *stopHarness) requireStopWaiting(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.stopOK:
		t.Fatalf("array stop returned (%v) while an /apps call was still inside the Engine", err)
	case <-time.After(100 * time.Millisecond):
	}
	if !h.w.scheduler.InMaintenance() {
		t.Fatal("maintenance mode is not active while array stop waits")
	}
}

func (h *stopHarness) requireStopDone(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.stopOK:
		if err != nil {
			t.Fatalf("StopArray: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("array stop did not return after the /apps call finished")
	}
}

// An /apps start or restart that passed its array check and is still inside
// dockerd when `array stop` begins is waited for: array stop lists the
// containers only after it, stops the container the call started, and
// `array start` starts it again. Without the wait the list is taken first,
// the container starts after it and is left running while the pool unmounts
// — with dockerd's live-restore on, nothing else would stop it.
func TestArrayStop_WaitsForAnInFlightStartOrRestartAndStopsItsContainer(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			h := newStopHarness(t)
			ctx := context.Background()

			call := func() error {
				if action == "start" {
					_, err := h.w.handler.StartApp(ctx, apiv1.StartAppParams{ID: "jellyfin"})
					return err
				}
				_, err := h.w.handler.RestartApp(ctx, apiv1.RestartAppParams{ID: "jellyfin"})
				return err
			}
			appDone := make(chan error, 1)
			go func() { appDone <- call() }()
			if op := <-h.held.entered; op != action {
				t.Fatalf("Engine saw %q, want %q", op, action)
			}

			h.beginStop(ctx)
			h.requireStopWaiting(t)

			close(h.held.release)
			if err := <-appDone; err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			h.requireStopDone(t)

			if s := containerState(t, h.fake, "jellyfin"); s != "exited" {
				t.Fatalf("jellyfin is %s after array stop, want exited: the container the in-flight %s started was not stopped", s, action)
			}
			if _, err := h.w.handler.StartApp(ctx, apiv1.StartAppParams{ID: "jellyfin"}); err == nil {
				t.Fatal("a start after array stop succeeded")
			}
			if _, err := h.w.handler.StartArray(ctx); err != nil {
				t.Fatalf("StartArray: %v", err)
			}
			if s := containerState(t, h.fake, "jellyfin"); s != "running" {
				t.Fatalf("jellyfin is %s after array start, want running: it was not among the containers array stop recorded", s)
			}
		})
	}
}

// A remove with appdata deletion held between its array check and the
// directory deletion is waited for: the cache unmounts only afterwards, so
// the call returns success only with the appdata really deleted.
func TestArrayStop_WaitsForAnInFlightRemoveWithAppdataBeforeTheCacheUnmounts(t *testing.T) {
	h := newStopHarness(t)
	ctx := context.Background()

	type removed struct {
		res *apiv1.RemoveAppResult
		err error
	}
	appDone := make(chan removed, 1)
	go func() {
		res, err := h.w.handler.RemoveApp(ctx, apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
		appDone <- removed{res, err}
	}()
	if op := <-h.held.entered; op != "remove" {
		t.Fatalf("Engine saw %q, want remove", op)
	}

	h.beginStop(ctx)
	h.requireStopWaiting(t)
	if h.cache.sawAppdata.Load() {
		t.Fatal("the cache was unmounted while the remove was still deleting appdata")
	}

	close(h.held.release)
	got := <-appDone
	if got.err != nil {
		t.Fatalf("RemoveApp: %v", got.err)
	}
	if len(got.res.DeletedPaths) != 1 {
		t.Fatalf("DeletedPaths = %v, want the jellyfin appdata directory", got.res.DeletedPaths)
	}
	h.requireStopDone(t)
	if h.cache.sawAppdata.Load() {
		t.Fatal("the appdata directory still existed when the cache unmounted: the remove reported success without deleting it")
	}
}

// Once maintenance mode has begun, a start, restart or remove with appdata
// is refused with array_stopped before the Engine is called, by the hold
// itself and not only by the maintenance flag Lifecycle also reads.
func TestArrayActionAdmit_RefusesOnceMaintenanceBegins(t *testing.T) {
	w := newMaintenanceRestartWiring(t, filepath.Join(t.TempDir(), "apps-admit.db"))
	t.Cleanup(func() { _ = w.db.Close() })
	admit := arrayActionAdmit(w.scheduler)

	release, err := admit()
	if err != nil {
		t.Fatalf("admit before maintenance: %v", err)
	}
	release()

	if err := w.scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := admit(); !errors.Is(err, container.ErrArrayStopped) {
		t.Fatalf("admit during maintenance = %v, want ErrArrayStopped", err)
	}

	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "exited"})
	apps := newContainers(fake, t.TempDir(), nil, nil)
	apps.Lifecycle.AppdataRoots = func(context.Context) ([]string, error) { return []string{t.TempDir()}, nil }
	// Halted reads false and storage ready, so only the hold can refuse.
	wireContainers(w.handler, w.registry, apps, func() bool { return false }, func() bool { return true }, admit)
	for name, call := range map[string]func() error{
		"start": func() error {
			_, err := w.handler.StartApp(context.Background(), apiv1.StartAppParams{ID: "jellyfin"})
			return err
		},
		"restart": func() error {
			_, err := w.handler.RestartApp(context.Background(), apiv1.RestartAppParams{ID: "jellyfin"})
			return err
		},
		"remove": func() error {
			_, err := w.handler.RemoveApp(context.Background(), apiv1.RemoveAppParams{ID: "jellyfin", DeleteAppdata: apiv1.NewOptBool(true)})
			return err
		},
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s succeeded after maintenance began", name)
		}
		if e := w.handler.NewError(context.Background(), err); e.StatusCode != http.StatusConflict || e.Response.Code != "array_stopped" {
			t.Fatalf("%s error = %d %s, want 409 array_stopped", name, e.StatusCode, e.Response.Code)
		}
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("the Engine saw %v", calls)
	}
}
