package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	f.l = &Lifecycle{
		Provider:     f.fake,
		Hub:          NewHub(),
		AppdataRoots: func(context.Context) ([]string, error) { return []string{f.root}, nil },
	}
	return f
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
	l := &Lifecycle{Provider: fake, Hub: hub}
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
	l := &Lifecycle{Provider: fake, Hub: hub}
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
	l := &Lifecycle{Provider: fake}
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
	l := &Lifecycle{Provider: fake, Hub: NewHub()}
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
	l := &Lifecycle{Provider: fake, Hub: NewHub()}

	if _, err := l.Recreate(context.Background(), "jellyfin"); err == nil {
		t.Fatal("Recreate succeeded")
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
