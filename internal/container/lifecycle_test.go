package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// appdataFixture lays out an appdata root, a media directory outside it and
// one stopped container mounting both, on a real temporary directory.
type appdataFixture struct {
	root, cfgDir, mediaDir string
	fake                   *FakeProvider
	l                      *Lifecycle
}

func newAppdataFixture(t *testing.T) *appdataFixture {
	t.Helper()
	base := t.TempDir()
	f := &appdataFixture{
		root:     filepath.Join(base, "cache", "appdata"),
		cfgDir:   filepath.Join(base, "cache", "appdata", "jellyfin"),
		mediaDir: filepath.Join(base, "pool", "media"),
	}
	for _, d := range []string{f.cfgDir, f.mediaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.cfgDir, "library.db"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.mediaDir, "movie.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.fake = NewFakeProvider()
	f.fake.AddContainer(Container{
		ID: "c1", Name: "jellyfin", State: "exited",
		Mounts: []Mount{
			{Source: f.cfgDir, Destination: "/config", ReadWrite: true},
			{Source: f.mediaDir, Destination: "/media", ReadWrite: false},
		},
	})
	f.l = arrayUp(&Lifecycle{
		Provider:     f.fake,
		Hub:          NewHub(),
		AppdataRoots: func(context.Context) ([]string, error) { return []string{f.root}, nil },
	})
	return f
}

// arrayUp gives l the array-state signals of a running array with ready
// storage, the only state in which Start, Restart and Recreate proceed.
func arrayUp(l *Lifecycle) *Lifecycle {
	l.Halted = func() bool { return false }
	l.StorageReady = func() bool { return true }
	return l
}

// The data-loss scenario (doc 02 §1): dockerd is reachable while the array
// is stopped, so a started container would bind empty directories on the
// boot device and write there. Every way the array can be unusable —
// maintenance mode, storage not ready, or no way to read either — refuses
// Start, Restart and Recreate before the Engine is called at all, and the
// same Lifecycle with a running array proceeds.
func TestStartRestartRecreate_RefusedUnlessTheArrayIsRunning(t *testing.T) {
	tests := []struct {
		name    string
		halted  func() bool
		ready   func() bool
		wantErr error
	}{
		{"maintenance mode", func() bool { return true }, func() bool { return true }, ErrArrayStopped},
		{"storage not ready", func() bool { return false }, func() bool { return false }, ErrArrayStopped},
		{"maintenance mode and storage not ready", func() bool { return true }, func() bool { return false }, ErrArrayStopped},
		{"no maintenance signal", nil, func() bool { return true }, ErrArrayStateUnknown},
		{"no storage signal", func() bool { return false }, nil, ErrArrayStateUnknown},
		{"no signals", nil, nil, ErrArrayStateUnknown},
	}
	for _, tc := range tests {
		for _, action := range []string{"start", "restart", "recreate"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				fake := NewFakeProvider()
				fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "exited"})
				l := &Lifecycle{Provider: fake, Hub: NewHub(), Halted: tc.halted, StorageReady: tc.ready}
				do := map[string]func(context.Context, string) (Container, error){
					"start": l.Start, "restart": l.Restart, "recreate": l.Recreate,
				}[action]

				_, err := do(context.Background(), "jellyfin")
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("%s error = %v, want %v", action, err, tc.wantErr)
				}
				if calls := fake.Calls(); len(calls) != 0 {
					t.Fatalf("the Engine saw %v although the %s was refused", calls, action)
				}
				if c, _ := fake.Inspect(context.Background(), "jellyfin"); c.State != "exited" {
					t.Fatalf("container is %s after a refused %s, want exited", c.State, action)
				}
			})
		}
	}
}

// Stop and a plain remove are how a container is taken out of a stopped
// array, so they never need it running.
func TestStopAndRemove_AllowedWhileTheArrayIsStopped(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running"})
	fake.AddContainer(Container{ID: "c2", Name: "portainer", State: "exited"})
	l := &Lifecycle{Provider: fake, Hub: NewHub(), Halted: func() bool { return true }, StorageReady: func() bool { return false }}
	ctx := context.Background()

	if _, err := l.Stop(ctx, "jellyfin"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := l.Remove(ctx, "portainer", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := l.Logs(ctx, "jellyfin", LogOptions{}); err != nil {
		t.Fatalf("Logs: %v", err)
	}
}

// The data-loss scenario for appdata deletion: with the array stopped the
// cache disk is not mounted, so every planned appdata directory is missing
// and would be skipped — the container removed, the call a success, the
// real appdata left on the cache disk to be picked up by a later install.
// Every way the array can be unusable refuses Remove with deleteAppdata
// before the Engine or any directory is touched. The fixture's directory
// exists here, so a Remove that got as far as deleting would show as a
// missing file.
func TestRemove_DeleteAppdataRefusedUnlessTheArrayIsRunning(t *testing.T) {
	tests := []struct {
		name    string
		halted  func() bool
		ready   func() bool
		wantErr error
	}{
		{"maintenance mode", func() bool { return true }, func() bool { return true }, ErrArrayStopped},
		{"storage not ready", func() bool { return false }, func() bool { return false }, ErrArrayStopped},
		{"maintenance mode and storage not ready", func() bool { return true }, func() bool { return false }, ErrArrayStopped},
		{"no maintenance signal", nil, func() bool { return true }, ErrArrayStateUnknown},
		{"no storage signal", func() bool { return false }, nil, ErrArrayStateUnknown},
		{"no signals", nil, nil, ErrArrayStateUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAppdataFixture(t)
			f.l.Halted, f.l.StorageReady = tc.halted, tc.ready

			res, err := f.l.Remove(context.Background(), "jellyfin", true)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Remove error = %v, want %v", err, tc.wantErr)
			}
			if len(res.DeletedPaths) != 0 {
				t.Fatalf("DeletedPaths = %v after a refused Remove", res.DeletedPaths)
			}
			if calls := f.fake.Calls(); len(calls) != 0 {
				t.Fatalf("the Engine saw %v although the Remove was refused", calls)
			}
			all, _ := f.fake.List(context.Background())
			if !mustHave(t, all, "jellyfin") {
				t.Fatal("the container was removed by a refused Remove")
			}
			if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
				t.Fatal("appdata was touched by a refused Remove")
			}
			if !exists(t, filepath.Join(f.mediaDir, "movie.mkv")) {
				t.Fatal("a directory outside the appdata root was touched by a refused Remove")
			}
		})
	}
}

// A remove that does not ask for appdata deletion never needs the array,
// however unusable: it is how a container is taken out of a stopped array.
func TestRemove_WithoutAppdataDeletionNeedsNoArrayState(t *testing.T) {
	f := newAppdataFixture(t)
	f.l.Halted, f.l.StorageReady = nil, nil

	if _, err := f.l.Remove(context.Background(), "jellyfin", false); err != nil {
		t.Fatalf("Remove without appdata deletion: %v", err)
	}
	if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatal("appdata was deleted by a remove that did not ask for it")
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatal(err)
	return false
}

func mustHave(t *testing.T, c []Container, name string) bool {
	t.Helper()
	for _, x := range c {
		if x.Name == name {
			return true
		}
	}
	return false
}

// A plain remove must never touch appdata: the default is to keep it.
func TestRemove_DefaultKeepsAppdata(t *testing.T) {
	f := newAppdataFixture(t)

	res, err := f.l.Remove(context.Background(), "jellyfin", false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 0 {
		t.Fatalf("DeletedPaths = %v, want none", res.DeletedPaths)
	}
	if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatal("appdata was deleted by a remove that did not ask for it")
	}
	all, _ := f.fake.List(context.Background())
	if mustHave(t, all, "jellyfin") {
		t.Fatal("container still exists after Remove")
	}
	for _, c := range f.fake.Calls() {
		if c.Op == "remove-volumes" {
			t.Fatalf("volumes were removed without asking: %v", f.fake.Calls())
		}
	}
}

func TestRemove_DeleteAppdataDeletesOnlyInsideTheAppdataRoot(t *testing.T) {
	f := newAppdataFixture(t)

	want, err := filepath.EvalSymlinks(f.cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.l.Remove(context.Background(), "jellyfin", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 1 || res.DeletedPaths[0] != want {
		t.Fatalf("DeletedPaths = %v, want [%s]", res.DeletedPaths, want)
	}
	if exists(t, f.cfgDir) {
		t.Fatal("appdata directory still exists")
	}
	if !exists(t, filepath.Join(f.mediaDir, "movie.mkv")) {
		t.Fatal("a bind mount outside the appdata root was deleted")
	}
	if !exists(t, f.root) {
		t.Fatal("the appdata root itself was deleted")
	}
	var volumes bool
	for _, c := range f.fake.Calls() {
		volumes = volumes || c.Op == "remove-volumes"
	}
	if !volumes {
		t.Fatal("deleting appdata did not ask the Engine to remove the anonymous volumes")
	}
}

// cancelAfterRemove cancels the request context as soon as the Engine has
// removed the container, the way a client disconnecting mid-request would.
type cancelAfterRemove struct {
	Provider
	cancel context.CancelFunc
}

func (p cancelAfterRemove) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	err := p.Provider.Remove(ctx, id, opts)
	p.cancel()
	return err
}

// The container is gone after the Engine removal, so a retry answers 404:
// every appdata directory must still be deleted when the client goes away.
func TestRemove_ClientDisconnectAfterTheEngineRemovalStillDeletesEveryDirectory(t *testing.T) {
	f := newAppdataFixture(t)
	first := filepath.Join(f.root, "twodirs-config")
	second := filepath.Join(f.root, "twodirs-cache")
	for _, d := range []string{first, second} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "x"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.fake.AddContainer(Container{
		ID: "c7", Name: "twodirs", State: "exited",
		Mounts: []Mount{
			{Source: first, Destination: "/config", ReadWrite: true},
			{Source: second, Destination: "/cache", ReadWrite: true},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.l.Provider = cancelAfterRemove{Provider: f.fake, cancel: cancel}

	res, err := f.l.Remove(ctx, "twodirs", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 2 || exists(t, first) || exists(t, second) {
		t.Fatalf("DeletedPaths = %v; directories left after a disconnect", res.DeletedPaths)
	}
}

func TestRemove_RunningContainerIsRefusedAndNothingIsDeleted(t *testing.T) {
	f := newAppdataFixture(t)
	if err := f.fake.Start(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}

	_, err := f.l.Remove(context.Background(), "jellyfin", true)
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("Remove error = %v, want ErrRunning", err)
	}
	if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatal("appdata was deleted although the container was refused")
	}
}

func TestRemove_WhenTheEngineFailsNothingIsDeleted(t *testing.T) {
	f := newAppdataFixture(t)
	f.fake.FailOn("remove", "c1", errors.New("engine exploded"))

	if _, err := f.l.Remove(context.Background(), "jellyfin", true); err == nil {
		t.Fatal("Remove succeeded although the Engine failed")
	}
	if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatal("appdata was deleted although the container was not removed")
	}
}

func TestRemove_AppdataSharedWithAnotherContainerIsRefusedBeforeAnythingIsRemoved(t *testing.T) {
	f := newAppdataFixture(t)
	f.fake.AddContainer(Container{
		ID: "c2", Name: "jellyfin-2", State: "running",
		Mounts: []Mount{{Source: filepath.Join(f.cfgDir, "cache"), Destination: "/cache", ReadWrite: true}},
	})

	_, err := f.l.Remove(context.Background(), "jellyfin", true)
	if !errors.Is(err, ErrAppdataShared) {
		t.Fatalf("Remove error = %v, want ErrAppdataShared", err)
	}
	all, _ := f.fake.List(context.Background())
	if !mustHave(t, all, "jellyfin") {
		t.Fatal("the container was removed although its appdata is shared")
	}
	if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatal("shared appdata was deleted")
	}
}

func TestRemove_AnotherContainerMountingTheWholeRootIsNoConflict(t *testing.T) {
	f := newAppdataFixture(t)
	f.fake.AddContainer(Container{
		ID: "c2", Name: "filebrowser", State: "running",
		Mounts: []Mount{{Source: f.root, Destination: "/srv", ReadWrite: true}},
	})

	if _, err := f.l.Remove(context.Background(), "jellyfin", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if exists(t, f.cfgDir) {
		t.Fatal("appdata directory still exists")
	}
}

// A candidate that lies inside another container's own appdata directory
// (adguard's certificates inside swag's config) is that container's data.
func TestRemove_AppdataInsideAnotherContainersAppdataIsRefused(t *testing.T) {
	f := newAppdataFixture(t)
	nested := filepath.Join(f.cfgDir, "etc", "letsencrypt")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(nested, "privkey.pem")
	if err := os.WriteFile(key, []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.fake.AddContainer(Container{
		ID: "c5", Name: "adguard", State: "exited",
		Mounts: []Mount{{Source: nested, Destination: "/certs", ReadWrite: true}},
	})
	f.fake.AddContainer(Container{
		ID: "c6", Name: "swag", State: "running",
		Mounts: []Mount{{Source: f.cfgDir, Destination: "/config", ReadWrite: true}},
	})

	_, err := f.l.Remove(context.Background(), "adguard", true)
	if !errors.Is(err, ErrAppdataShared) {
		t.Fatalf("Remove error = %v, want ErrAppdataShared", err)
	}
	all, _ := f.fake.List(context.Background())
	if !mustHave(t, all, "adguard") {
		t.Fatal("the container was removed although its appdata is inside another container's")
	}
	if !exists(t, key) {
		t.Fatal("a directory inside another container's appdata was deleted")
	}
}

func TestRemove_NoAppdataLocationRefusesBeforeRemoving(t *testing.T) {
	f := newAppdataFixture(t)
	f.l.AppdataRoots = func(context.Context) ([]string, error) { return nil, nil }

	_, err := f.l.Remove(context.Background(), "jellyfin", true)
	if !errors.Is(err, ErrAppdataUnavailable) {
		t.Fatalf("Remove error = %v, want ErrAppdataUnavailable", err)
	}
	all, _ := f.fake.List(context.Background())
	if !mustHave(t, all, "jellyfin") {
		t.Fatal("the container was removed although appdata deletion was impossible")
	}
}

// A symlink inside the appdata root that points elsewhere must not lead
// the deletion out of the root.
func TestRemove_SymlinkOutOfTheRootIsNotFollowed(t *testing.T) {
	f := newAppdataFixture(t)
	link := filepath.Join(f.root, "media-link")
	if err := os.Symlink(f.mediaDir, link); err != nil {
		t.Fatal(err)
	}
	f.fake.AddContainer(Container{
		ID: "c3", Name: "linked", State: "exited",
		Mounts: []Mount{{Source: link, Destination: "/data", ReadWrite: true}},
	})

	res, err := f.l.Remove(context.Background(), "linked", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 0 {
		t.Fatalf("DeletedPaths = %v, want none for a mount that resolves outside the root", res.DeletedPaths)
	}
	if !exists(t, filepath.Join(f.mediaDir, "movie.mkv")) {
		t.Fatal("a symlink led the deletion out of the appdata root")
	}
}

func TestRemove_MountingTheRootItselfDeletesNothing(t *testing.T) {
	f := newAppdataFixture(t)
	f.fake.AddContainer(Container{
		ID: "c4", Name: "wide", State: "exited",
		Mounts: []Mount{{Source: f.root, Destination: "/all", ReadWrite: true}},
	})

	res, err := f.l.Remove(context.Background(), "wide", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 0 || !exists(t, filepath.Join(f.cfgDir, "library.db")) {
		t.Fatalf("mounting the appdata root itself deleted %v", res.DeletedPaths)
	}
}

func TestRemove_UnknownContainer(t *testing.T) {
	f := newAppdataFixture(t)
	if _, err := f.l.Remove(context.Background(), "nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestStartStopRestart_PublishTheStateAfterTheAction(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "exited", Health: HealthNone})
	hub := NewHub()
	l := arrayUp(&Lifecycle{Provider: fake, Hub: hub})
	ch, unsub := hub.Subscribe()
	defer unsub()

	for _, tc := range []struct {
		name  string
		do    func(context.Context, string) (Container, error)
		state string
	}{
		{"start", l.Start, "running"},
		{"restart", l.Restart, "running"},
		{"stop", l.Stop, "exited"},
	} {
		c, err := tc.do(context.Background(), "jellyfin")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if c.State != tc.state {
			t.Fatalf("%s: State = %q, want %q", tc.name, c.State, tc.state)
		}
		select {
		case sc := <-ch:
			if sc.State != tc.state || sc.Name != "jellyfin" || sc.ID != "c1" {
				t.Fatalf("%s published %+v", tc.name, sc)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s published no state change", tc.name)
		}
	}
}

func TestStart_FailurePublishesNothing(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "exited"})
	fake.FailOn("start", "c1", errors.New("port already allocated"))
	hub := NewHub()
	l := arrayUp(&Lifecycle{Provider: fake, Hub: hub})
	ch, unsub := hub.Subscribe()
	defer unsub()

	if _, err := l.Start(context.Background(), "jellyfin"); err == nil {
		t.Fatal("Start succeeded")
	}
	select {
	case sc := <-ch:
		t.Fatalf("a failed start published %+v", sc)
	default:
	}
}

func TestLifecycle_UnavailableEngineIsReported(t *testing.T) {
	fake := NewFakeProvider()
	fake.SetUnavailable(nil)
	l := arrayUp(&Lifecycle{Provider: fake})
	if _, err := l.Start(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Start error = %v, want ErrUnavailable", err)
	}
}

// Unmanaged means Hoserva did not install the container: no stack, no
// template, no Hoserva label. Every action works on it through the same
// Provider calls as on any other container, and Lifecycle makes no call
// that reads or writes anything but the container itself
// (TestRecreateSpec_AddsNoLabels covers what Recreate hands the Engine).
func TestUnmanagedContainerIsFullyControllable(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "u1", Name: "portainer", State: "exited"})
	l := arrayUp(&Lifecycle{Provider: fake, Hub: NewHub()})
	ctx := context.Background()

	if _, err := l.Start(ctx, "portainer"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := l.Restart(ctx, "portainer"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, err := l.Recreate(ctx, "portainer"); err != nil {
		t.Fatalf("Recreate: %v", err)
	}
	if _, err := l.Stop(ctx, "portainer"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := l.Remove(ctx, "portainer", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var ops []string
	for _, c := range fake.Calls() {
		ops = append(ops, c.Op)
	}
	want := []string{"start", "restart", "recreate", "stop", "remove"}
	if len(ops) != len(want) {
		t.Fatalf("calls = %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("calls = %v, want %v", ops, want)
		}
	}
}

func TestRecreate_FailureLeavesTheContainerAsItWas(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running", Image: "jf", Tag: "10"})
	fake.FailOn("recreate", "c1", errors.New("pull access denied"))
	l := arrayUp(&Lifecycle{Provider: fake, Hub: NewHub()})

	_, err := l.Recreate(context.Background(), "jellyfin")
	if err == nil || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("Recreate = %v; want the injected pull failure", err)
	}
	c, err := fake.Inspect(context.Background(), "jellyfin")
	if err != nil || c.State != "running" {
		t.Fatalf("container = %+v, %v; want it still running", c, err)
	}
}

func TestHealth_UnhealthyContainerIsReported(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running", Health: HealthHealthy})
	if err := fake.SetHealth("c1", HealthUnhealthy); err != nil {
		t.Fatal(err)
	}
	l := &Lifecycle{Provider: fake}
	got, err := fake.Inspect(context.Background(), "jellyfin")
	if err != nil || got.Health != HealthUnhealthy {
		t.Fatalf("Inspect = %+v, %v; want health unhealthy", got, err)
	}
	all, _ := l.Provider.List(context.Background())
	if all[0].Health != HealthUnhealthy {
		t.Fatalf("List health = %q, want unhealthy", all[0].Health)
	}
}

func TestStats_StoppedContainerIsNotReportedAsIdle(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "exited"})
	l := &Lifecycle{Provider: fake}
	if _, err := l.Stats(context.Background(), "jellyfin"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stats error = %v, want ErrNotRunning", err)
	}
}

// gatedProvider holds the Engine call of start, restart or remove until the
// test lets it go, standing in for a slow dockerd.
type gatedProvider struct {
	*FakeProvider
	entered chan string
	release chan struct{}
}

func newGatedProvider(fake *FakeProvider) *gatedProvider {
	return &gatedProvider{FakeProvider: fake, entered: make(chan string, 4), release: make(chan struct{})}
}

func (g *gatedProvider) hold(op string) {
	g.entered <- op
	<-g.release
}

func (g *gatedProvider) Start(ctx context.Context, id string) error {
	g.hold("start")
	return g.FakeProvider.Start(ctx, id)
}

func (g *gatedProvider) Restart(ctx context.Context, id string) error {
	g.hold("restart")
	return g.FakeProvider.Restart(ctx, id)
}

func (g *gatedProvider) Remove(ctx context.Context, id string, opts RemoveOptions) error {
	g.hold("remove")
	return g.FakeProvider.Remove(ctx, id, opts)
}

// arrayHold is a Lifecycle.Admit that records how many actions are in
// flight and can be made to refuse, like the daemon's scheduler-backed one.
type arrayHold struct {
	mu       sync.Mutex
	inflight int
	admitted int
	refuse   error
	onFinish func()
}

func (h *arrayHold) admit() (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.refuse != nil {
		return nil, h.refuse
	}
	h.inflight++
	h.admitted++
	return func() {
		if h.onFinish != nil {
			h.onFinish()
		}
		h.mu.Lock()
		h.inflight--
		h.mu.Unlock()
	}, nil
}

func (h *arrayHold) counts() (inflight, admitted int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.inflight, h.admitted
}

// The data-loss scenario for an in-flight start or restart: array stop
// drains the hold before it lists the running containers, so the hold must
// cover the whole Engine call — a start that had already passed the array
// check and is still inside dockerd is what would land after that list.
func TestStartRestart_HoldTheArrayActionUntilTheEngineReturns(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			fake := NewFakeProvider()
			fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "exited"})
			gated := newGatedProvider(fake)
			hold := &arrayHold{}
			l := arrayUp(&Lifecycle{Provider: gated, Hub: NewHub(), Admit: hold.admit})
			do := map[string]func(context.Context, string) (Container, error){"start": l.Start, "restart": l.Restart}[action]

			done := make(chan error, 1)
			go func() { _, err := do(context.Background(), "jellyfin"); done <- err }()

			if op := <-gated.entered; op != action {
				t.Fatalf("Engine saw %q, want %q", op, action)
			}
			if inflight, _ := hold.counts(); inflight != 1 {
				t.Fatalf("in-flight actions while the Engine call is held = %d, want 1", inflight)
			}
			close(gated.release)
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if inflight, admitted := hold.counts(); inflight != 0 || admitted != 1 {
				t.Fatalf("after %s: in flight %d, admitted %d, want 0 and 1", action, inflight, admitted)
			}
		})
	}
}

// A refused admission ends the call before the array check and the Engine.
func TestStartRestartRemoveWithAppdata_RefusedByTheHold(t *testing.T) {
	for _, action := range []string{"start", "restart", "remove"} {
		t.Run(action, func(t *testing.T) {
			f := newAppdataFixture(t)
			hold := &arrayHold{refuse: fmt.Errorf("%w: it is in maintenance mode", ErrArrayStopped)}
			f.l.Admit = hold.admit
			ctx := context.Background()
			var err error
			switch action {
			case "start":
				_, err = f.l.Start(ctx, "jellyfin")
			case "restart":
				_, err = f.l.Restart(ctx, "jellyfin")
			case "remove":
				_, err = f.l.Remove(ctx, "jellyfin", true)
			}
			if !errors.Is(err, ErrArrayStopped) {
				t.Fatalf("%s error = %v, want ErrArrayStopped", action, err)
			}
			if calls := f.fake.Calls(); len(calls) != 0 {
				t.Fatalf("the Engine saw %v although the %s was refused", calls, action)
			}
			if !exists(t, filepath.Join(f.cfgDir, "library.db")) {
				t.Fatal("appdata was touched by a refused remove")
			}
		})
	}
}

// A remove with appdata is held from its array check until the appdata
// directories are gone: array stop must not unmount the cache in between,
// or the delete finds every directory missing and reports success.
func TestRemoveWithAppdata_HoldsTheArrayActionUntilTheDirectoriesAreDeleted(t *testing.T) {
	f := newAppdataFixture(t)
	gated := newGatedProvider(f.fake)
	f.l.Provider = gated
	hold := &arrayHold{}
	f.l.Admit = hold.admit
	cfgFile := filepath.Join(f.cfgDir, "library.db")
	var appdataAtRelease bool
	hold.onFinish = func() { appdataAtRelease = exists(t, cfgFile) }

	done := make(chan error, 1)
	go func() { _, err := f.l.Remove(context.Background(), "jellyfin", true); done <- err }()

	if op := <-gated.entered; op != "remove" {
		t.Fatalf("Engine saw %q, want remove", op)
	}
	if inflight, _ := hold.counts(); inflight != 1 {
		t.Fatalf("in-flight actions while the Engine remove is held = %d, want 1", inflight)
	}
	close(gated.release)
	if err := <-done; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if appdataAtRelease {
		t.Fatal("the hold was released before the appdata directories were deleted")
	}
	if inflight, _ := hold.counts(); inflight != 0 {
		t.Fatalf("in-flight actions after Remove = %d, want 0", inflight)
	}
}

// Removing without appdata deletion is how a container leaves a stopped
// array, so it is neither held nor refused by the hold.
func TestRemoveWithoutAppdata_IsNotHeld(t *testing.T) {
	f := newAppdataFixture(t)
	hold := &arrayHold{refuse: ErrArrayStopped}
	f.l.Admit = hold.admit
	if _, err := f.l.Remove(context.Background(), "jellyfin", false); err != nil {
		t.Fatalf("Remove without appdata deletion: %v", err)
	}
	if _, admitted := hold.counts(); admitted != 0 {
		t.Fatalf("admitted = %d, want 0", admitted)
	}
}
