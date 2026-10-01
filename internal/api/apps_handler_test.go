package api_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

type stackListsRows struct{ *stackMemStore }

func (s stackListsRows) List(context.Context) ([]store.Stack, error) {
	out := make([]store.Stack, 0, len(s.rows))
	for _, st := range s.rows {
		out = append(out, st)
	}
	return out, nil
}

type stackListFails struct{ *stackMemStore }

func (stackListFails) List(context.Context) ([]store.Stack, error) {
	return nil, errors.New("database is locked")
}

func composeLabels(project, workingDir string) map[string]string {
	return map[string]string{
		"com.docker.compose.project":             project,
		"com.docker.compose.project.working_dir": workingDir,
	}
}

// newStackedApps is a handler over a Docker with three containers: one an
// installed stack started, one that stack's project name run from another
// directory, and one started by hand.
func newStackedApps(t *testing.T) *api.Handler {
	t.Helper()
	h, root := newStacksHandler(t)
	mem := &stackMemStore{rows: map[string]store.Stack{"immich": {Name: "immich"}}}
	h.Stacks.Store = stackListsRows{mem}
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "c-stack", Name: "immich-server-1", State: "running", Labels: composeLabels("immich", filepath.Join(root, "immich"))})
	fake.AddContainer(container.Container{ID: "c-elsewhere", Name: "immich-by-hand", State: "running", Labels: composeLabels("immich", "/home/me/immich")})
	fake.AddContainer(container.Container{ID: "c-hand", Name: "portainer", State: "running"})
	h.Container = fake
	return h
}

func TestListApps_ReportsTheStackOfTheContainersItManagesAndNoneForTheRest(t *testing.T) {
	h := newStackedApps(t)

	list, err := h.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	got := map[string]apiv1.OptString{}
	for _, a := range list.Apps {
		got[a.Name] = a.Stack
	}
	if s, ok := got["immich-server-1"].Get(); !ok || s != "immich" {
		t.Fatalf("immich-server-1 stack = %v, want immich", got["immich-server-1"])
	}
	for _, name := range []string{"immich-by-hand", "portainer"} {
		if s, ok := got[name].Get(); ok {
			t.Fatalf("%s stack = %q, want none: no stack started it", name, s)
		}
	}
}

func TestListApps_AStackReadThatFailsFailsTheRequestInsteadOfReportingEveryContainerUnmanaged(t *testing.T) {
	h := newStackedApps(t)
	h.Stacks.Store = stackListFails{&stackMemStore{rows: map[string]store.Stack{}}}

	list, err := h.ListApps(context.Background())
	if err == nil {
		t.Fatalf("ListApps = %+v, nil; want an error", list)
	}
	if status, _ := statusOf(h, err); status != 500 {
		t.Fatalf("ListApps status = %d, want 500", status)
	}
}

func TestListApps_NoStackServiceMeansNoContainerIsManaged(t *testing.T) {
	h := newStackedApps(t)
	h.Stacks = nil

	list, err := h.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	for _, a := range list.Apps {
		if a.Stack.Set {
			t.Fatalf("%s has stack %q with no stack service", a.Name, a.Stack.Value)
		}
	}
}

func TestGetApp_ReportsTheStackAndFailsOnAStackReadThatFails(t *testing.T) {
	h := newStackedApps(t)

	app, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "immich-server-1"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if s, ok := app.Stack.Get(); !ok || s != "immich" {
		t.Fatalf("GetApp stack = %v, want immich", app.Stack)
	}
	app, err = h.GetApp(context.Background(), apiv1.GetAppParams{ID: "portainer"})
	if err != nil || app.Stack.Set {
		t.Fatalf("GetApp(portainer) = %+v, %v; want no stack", app, err)
	}

	h.Stacks.Store = stackListFails{&stackMemStore{rows: map[string]store.Stack{}}}
	if _, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "immich-server-1"}); err == nil {
		t.Fatal("GetApp succeeded although the stacks could not be read")
	}
}

// newMountsHandler is a handler over a Docker with one container mounting a
// path of every storage kind, and an array with two data disks and a cache.
func newMountsHandler(t *testing.T, withArray bool) (*api.Handler, *container.FakeProvider, *sql.DB) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "apps-mounts.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	arrays := store.NewArrayStore(db)
	if withArray {
		err := arrays.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
			{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "p", Mountpoint: "/mnt/parity1"},
			{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "d1", Mountpoint: "/mnt/disk1"},
			{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "d2", Mountpoint: "/mnt/disk2"},
			{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "ext4", FSUUID: "c", Mountpoint: "/mnt/cache"},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{
		ID: "c1", Name: "jellyfin", State: "exited",
		Mounts: []container.Mount{
			{Source: "/mnt/cache/appdata/jellyfin", Destination: "/config", ReadWrite: true},
			{Source: "/mnt/user/media/movies", Destination: "/movies"},
			{Source: "/mnt/disk2/scratch", Destination: "/scratch", ReadWrite: true},
			{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock", ReadWrite: true},
			{Destination: "/tmp", ReadWrite: true},
		},
	})
	return &api.Handler{Container: fake, ArrayStore: arrays}, fake, db
}

func locationOf(t *testing.T, app *apiv1.App, destination string) (apiv1.AppMountLocation, bool) {
	t.Helper()
	for _, m := range app.Mounts {
		if m.Destination == destination {
			return m.Location.Get()
		}
	}
	t.Fatalf("no mount %s in %+v", destination, app.Mounts)
	return apiv1.AppMountLocation{}, false
}

func TestGetAndListApps_PlaceEveryMountOnItsStorage(t *testing.T) {
	h, _, _ := newMountsHandler(t, true)
	got, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	list, err := h.ListApps(context.Background())
	if err != nil || len(list.Apps) != 1 {
		t.Fatalf("ListApps = %+v, %v", list, err)
	}
	for name, app := range map[string]*apiv1.App{"GetApp": got, "ListApps": &list.Apps[0]} {
		want := map[string]apiv1.AppMountLocation{
			"/config":              {Kind: apiv1.AppMountLocationKindCache},
			"/movies":              {Kind: apiv1.AppMountLocationKindPool, Share: apiv1.NewOptString("media")},
			"/scratch":             {Kind: apiv1.AppMountLocationKindDisk, Disk: apiv1.NewOptInt(2)},
			"/var/run/docker.sock": {Kind: apiv1.AppMountLocationKindOutside},
		}
		for dest, loc := range want {
			if l, ok := locationOf(t, app, dest); !ok || l != loc {
				t.Errorf("%s: location of %s = %+v (set %v), want %+v", name, dest, l, ok, loc)
			}
		}
		if _, ok := locationOf(t, app, "/tmp"); ok {
			t.Errorf("%s: a mount with no host path has a location", name)
		}
	}
}

func TestGetApp_WithoutAnArrayOnlyThePoolPathIsKnown(t *testing.T) {
	h, _, _ := newMountsHandler(t, false)
	app, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if l, _ := locationOf(t, app, "/movies"); l.Kind != apiv1.AppMountLocationKindPool {
		t.Fatalf("pool path with no array = %+v", l)
	}
	if l, _ := locationOf(t, app, "/config"); l.Kind != apiv1.AppMountLocationKindOutside {
		t.Fatalf("cache path with no array = %+v, want outside: no cache disk is known", l)
	}
}

func TestGetAndListApps_AnUnreadableTopologyFailsInsteadOfPlacingMountsOutsideTheArray(t *testing.T) {
	h, _, db := newMountsHandler(t, true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"}); err == nil {
		t.Fatal("GetApp succeeded although the array topology could not be read")
	}
	if _, err := h.ListApps(context.Background()); err == nil {
		t.Fatal("ListApps succeeded although the array topology could not be read")
	}
}

func TestGetApp_ReportsCreationStartAndRestarts(t *testing.T) {
	h, fake, _ := newMountsHandler(t, true)
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	started := time.Date(2026, 9, 30, 6, 30, 0, 0, time.UTC)
	for name, step := range map[string]func() error{
		"created":  func() error { return fake.SetCreatedAt("jellyfin", created) },
		"restarts": func() error { return fake.SetRestartCount("jellyfin", 2) },
	} {
		if err := step(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	never, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if at, _ := never.CreatedAt.Get(); !at.Equal(created) {
		t.Fatalf("createdAt = %v, want %v", never.CreatedAt, created)
	}
	if rc, ok := never.RestartCount.Get(); !ok || rc != 2 {
		t.Fatalf("restartCount = %v, want 2", never.RestartCount)
	}
	if never.StartedAt.Set {
		t.Fatalf("startedAt = %v for a container that never ran, want absent", never.StartedAt.Value)
	}

	if err := fake.SetStartedAt("jellyfin", started); err != nil {
		t.Fatal(err)
	}
	ran, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if at, ok := ran.StartedAt.Get(); !ok || !at.Equal(started) {
		t.Fatalf("startedAt = %v, want %v", ran.StartedAt, started)
	}

	list, err := h.ListApps(context.Background())
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if a := list.Apps[0]; a.CreatedAt.Set || a.StartedAt.Set || a.RestartCount.Set {
		t.Fatalf("the listing carries runtime facts: %+v", a)
	}
}

func TestGetApp_ARuntimeReadThatFailsFailsTheRequest(t *testing.T) {
	h, fake, _ := newMountsHandler(t, true)
	fake.FailOn("runtime", "jellyfin", errors.New("engine busy"))
	if _, err := h.GetApp(context.Background(), apiv1.GetAppParams{ID: "jellyfin"}); err == nil {
		t.Fatal("GetApp succeeded although the Engine's inspection failed")
	}
}
