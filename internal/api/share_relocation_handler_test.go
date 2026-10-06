package api_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newShareRelocationTestHandler wires an *api.Handler with both a real
// share.Service (so startShareRelocation's own existence check runs
// against real state) and a fresh job.Scheduler/Registry, sharing one
// SQLite database the way a real daemon does — this issue's own handler
// needs both, unlike newShareTestHandler and newTestHandler, which each
// wire only one.
func newShareRelocationTestHandler(t *testing.T) (*api.Handler, *job.Scheduler, *job.Registry) {
	t.Helper()
	return newShareRelocationTestHandlerWithCache(t, true)
}

// newShareRelocationTestHandlerWithCache is newShareRelocationTestHandler
// with the array's cache disk optional, for the refusal a relocation to
// the cache gets on an array that has none.
func newShareRelocationTestHandlerWithCache(t *testing.T, withCache bool) (*api.Handler, *job.Scheduler, *job.Registry) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "share-relocation-handler.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	layout := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "d1", Mountpoint: filepath.Join(root, "disk1")},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "c", Mountpoint: filepath.Join(root, "cache")},
	}
	if !withCache {
		layout = layout[:2]
	}
	for _, d := range layout {
		if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}, layout); err != nil {
		t.Fatal(err)
	}

	svc := &share.Service{
		Shares:   store.NewShareStore(db),
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       share.OSFS{},
		Mounter:  recordingShareMounter{},
		Now:      func() time.Time { return time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC) },
		CatchAll: filepath.Join(root, "user"),
	}
	if err := os.MkdirAll(svc.CatchAll, 0o755); err != nil {
		t.Fatal(err)
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)

	h := &api.Handler{Shares: svc, Scheduler: scheduler, Store: jobStore, Logs: logs}
	return h, scheduler, registry
}

// syncFuncFromEngine adapts a parity.Engine into a cache.SyncFunc the way
// a real daemon's own share-relocation wiring must (cache.SyncFunc's own
// doc comment): draining the returned progress channel and mapping a
// blocked or failed sync to a plain error.
func syncFuncFromEngine(e parity.Engine) cache.SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := e.Sync(ctx, parity.SyncOpts{Manifest: manifest})
		if err != nil {
			return err
		}
		var last parity.Progress
		for p := range ch {
			last = p
		}
		return last.Err
	}
}

// TestHandler_StartShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched
// is this issue's own central safety test at the HTTP boundary
// (CLAUDE.md: "anything that can lose data gets its test before its
// implementation"): a startShareRelocation request must fail the job,
// not bypass the threshold guard, when the array-side sync it triggers
// is blocked — the array original must survive.
func TestHandler_StartShareRelocation_ToCache_GuardBlocked_LeavesArrayUntouched(t *testing.T) {
	ctx := context.Background()
	h, s, r := newShareRelocationTestHandler(t)
	if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.CacheThenMove}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	base := t.TempDir()
	relocShare := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		Branches:  []string{filepath.Join(base, "disk1", "docs")},
	}
	src := filepath.Join(relocShare.Branches[0], "report.pdf")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("report bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	eng := parity.NewFakeEngine()
	eng.Sleep = func(time.Duration) {}
	eng.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerRemovedCount}})
	r.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
		Open:  cache.NewFakeOpenChecker(),
		Share: func(context.Context, string) (cache.Share, error) { return relocShare, nil },
		Sync:  syncFuncFromEngine(eng),
	}))

	req := &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToCache}
	got, err := h.StartShareRelocation(ctx, req, apiv1.StartShareRelocationParams{Name: "docs"})
	if err != nil {
		t.Fatalf("StartShareRelocation: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s, want failed", finished.Status)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("array original must survive a blocked sync: %v", err)
	}
}

func TestHandler_StartShareRelocation_UnknownShareIs404(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newShareRelocationTestHandler(t)

	req := &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}
	_, err := h.StartShareRelocation(ctx, req, apiv1.StartShareRelocationParams{Name: "ghost"})
	status := apiError(t, h, err)
	if status.StatusCode != 404 {
		t.Fatalf("StartShareRelocation(unknown share) status = %d, want 404", status.StatusCode)
	}
}

// TestHandler_StartShareRelocation_ToArray_SubmitsAndRuns proves the
// happy path reaches RelocateToArray through the real scheduler, from
// the handler down — the same job.TypeShareRelocation job the CLI and
// UI would submit through this operation (D18: no second invocation
// path).
func TestHandler_StartShareRelocation_ToArray_SubmitsAndRuns(t *testing.T) {
	ctx := context.Background()
	h, s, r := newShareRelocationTestHandler(t)
	if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.CacheThenMove}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	base := t.TempDir()
	relocShare := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		ArrayPath: filepath.Join(base, "array", "docs"),
	}
	src := filepath.Join(relocShare.CachePath, "report.pdf")
	for _, d := range []string{filepath.Dir(src), relocShare.ArrayPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(src, []byte("report bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	r.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
		Open:  cache.NewFakeOpenChecker(),
		Share: func(context.Context, string) (cache.Share, error) { return relocShare, nil },
	}))

	req := &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}
	got, err := h.StartShareRelocation(ctx, req, apiv1.StartShareRelocationParams{Name: "docs"})
	if err != nil {
		t.Fatalf("StartShareRelocation: %v", err)
	}
	if got.Type != apiv1.JobTypeShareRelocation {
		t.Fatalf("job type = %s, want share_relocation", got.Type)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	dst := filepath.Join(relocShare.ArrayPath, "report.pdf")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("expected relocated file on the array: %v", err)
	}
}

// TestHandler_StartShareRelocation_WithoutCacheDiskIsRefusedBeforeQueueing
// pins that a relocation in either direction on an array with no cache disk
// is refused at the API with 409 no_cache_disk and queues no job, rather
// than answering 200 for a job that then fails: the relocation job needs
// the share's cache path whichever way the files move.
func TestHandler_StartShareRelocation_WithoutCacheDiskIsRefusedBeforeQueueing(t *testing.T) {
	for _, to := range []apiv1.StartShareRelocationRequestTo{
		apiv1.StartShareRelocationRequestToCache,
		apiv1.StartShareRelocationRequestToArray,
	} {
		t.Run(string(to), func(t *testing.T) {
			ctx := context.Background()
			h, _, r := newShareRelocationTestHandlerWithCache(t, false)
			if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.ArrayOnly}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			r.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
				Open:  cache.NewFakeOpenChecker(),
				Share: func(context.Context, string) (cache.Share, error) { return cache.Share{Name: "docs"}, nil },
			}))

			_, err := h.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: to}, apiv1.StartShareRelocationParams{Name: "docs"})
			status := apiError(t, h, err)
			if status.StatusCode != 409 || status.Response.Code != "no_cache_disk" {
				t.Fatalf("StartShareRelocation(to %s, no cache disk) = %d %q, want 409 no_cache_disk", to, status.StatusCode, status.Response.Code)
			}
			jobs, err := h.Store.List(ctx, job.ListFilter{})
			if err != nil {
				t.Fatalf("listing jobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("a refused relocation queued %d job(s), want none", len(jobs))
			}
		})
	}
}

// TestHandler_StartShareRelocation_RefusedAtSubmitWhileGated pins, with a
// cache disk present so the no_cache_disk refusal is out of the way, that the
// scheduler's own admission checks still refuse a relocation in either
// direction: while the array is stopped (maintenance mode) and while an
// Unraid migration is pending. Each refusal is a 409 and queues no job.
func TestHandler_StartShareRelocation_RefusedAtSubmitWhileGated(t *testing.T) {
	gates := []struct {
		name     string
		enter    func(t *testing.T, s *job.Scheduler)
		wantCode string
	}{
		{
			name: "maintenance_mode",
			enter: func(t *testing.T, s *job.Scheduler) {
				if err := s.EnterMaintenance(context.Background()); err != nil {
					t.Fatalf("EnterMaintenance: %v", err)
				}
			},
			wantCode: "maintenance_mode",
		},
		{
			name: "migration_in_progress",
			enter: func(_ *testing.T, s *job.Scheduler) {
				s.SetMigrationPending(func(context.Context) (bool, error) { return true, nil })
			},
			wantCode: "migration_in_progress",
		},
	}
	for _, g := range gates {
		for _, to := range []apiv1.StartShareRelocationRequestTo{
			apiv1.StartShareRelocationRequestToCache,
			apiv1.StartShareRelocationRequestToArray,
		} {
			t.Run(g.name+"/"+string(to), func(t *testing.T) {
				ctx := context.Background()
				h, s, r := newShareRelocationTestHandlerWithCache(t, true)
				if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.CacheThenMove}); err != nil {
					t.Fatalf("Create: %v", err)
				}
				r.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
					Open:  cache.NewFakeOpenChecker(),
					Share: func(context.Context, string) (cache.Share, error) { return cache.Share{Name: "docs"}, nil },
				}))
				g.enter(t, s)

				_, err := h.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: to}, apiv1.StartShareRelocationParams{Name: "docs"})
				status := apiError(t, h, err)
				if status.StatusCode != 409 || status.Response.Code != g.wantCode {
					t.Fatalf("StartShareRelocation(to %s) = %d %q, want 409 %s", to, status.StatusCode, status.Response.Code, g.wantCode)
				}
				jobs, err := h.Store.List(ctx, job.ListFilter{})
				if err != nil {
					t.Fatalf("listing jobs: %v", err)
				}
				if len(jobs) != 0 {
					t.Fatalf("a refused relocation queued %d job(s), want none", len(jobs))
				}
			})
		}
	}
}

type precheckFixture struct {
	h    *api.Handler
	docs cache.Share
	open *cache.FakeOpenChecker
	apps *container.FakeProvider
}

// newPrecheckFixture wires the relocation handler the way main.go does for
// getShareRelocationPrecheck: a share store, a container provider and the
// share-resolution hook, with a fake open-file checker in place of /proc.
func newPrecheckFixture(t *testing.T) *precheckFixture {
	t.Helper()
	ctx := context.Background()
	h, _, _ := newShareRelocationTestHandler(t)
	if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.CacheThenMove}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	base := t.TempDir()
	docs := cache.Share{
		Name:      "docs",
		CachePath: filepath.Join(base, "cache", "docs"),
		Branches:  []string{filepath.Join(base, "disk1", "docs")},
	}
	for _, f := range []string{
		filepath.Join(docs.CachePath, "live.db"),
		filepath.Join(docs.Branches[0], "old.pdf"),
		filepath.Join(docs.Branches[0], "idle.txt"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	open := cache.NewFakeOpenChecker()
	apps := container.NewFakeProvider()
	h.Container = apps
	h.RelocationOpen = open
	h.RelocationShare = func(context.Context, string) (cache.Share, error) { return docs, nil }
	return &precheckFixture{h: h, docs: docs, open: open, apps: apps}
}

// TestHandler_GetShareRelocationPrecheck_ListsContainersAndOpenFiles proves a
// container that bind-mounts the share, by its pool path, its cache path or an
// array branch, is listed with whether it is active, and that the open files
// Precheck finds come back share-relative. A container that mounts something
// else is not listed.
func TestHandler_GetShareRelocationPrecheck_ListsContainersAndOpenFiles(t *testing.T) {
	ctx := context.Background()
	f := newPrecheckFixture(t)
	f.apps.AddContainer(container.Container{ID: "a1", Name: "pool-user", State: "running", Mounts: []container.Mount{{Source: "/mnt/user/docs/db", Destination: "/data"}}})
	f.apps.AddContainer(container.Container{ID: "a2", Name: "cache-user", State: "exited", Mounts: []container.Mount{{Source: f.docs.CachePath, Destination: "/data"}}})
	f.apps.AddContainer(container.Container{ID: "a3", Name: "branch-user", State: "paused", Mounts: []container.Mount{{Source: filepath.Join(f.docs.Branches[0], "sub"), Destination: "/data"}}})
	f.apps.AddContainer(container.Container{ID: "a4", Name: "elsewhere", State: "running", Mounts: []container.Mount{{Source: "/mnt/user/media", Destination: "/data"}}})
	f.open.SetOpen(filepath.Join(f.docs.CachePath, "live.db"), true)

	got, err := f.h.GetShareRelocationPrecheck(ctx, apiv1.GetShareRelocationPrecheckParams{Name: "docs"})
	if err != nil {
		t.Fatalf("GetShareRelocationPrecheck: %v", err)
	}
	if !got.DockerAvailable {
		t.Fatal("dockerAvailable = false with a reachable provider")
	}
	type row struct {
		name   string
		active bool
	}
	var rows []row
	for _, c := range got.Containers {
		rows = append(rows, row{c.Name, c.Active})
	}
	want := []row{{"pool-user", true}, {"cache-user", false}, {"branch-user", true}}
	if len(rows) != len(want) {
		t.Fatalf("containers = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("containers = %+v, want %+v", rows, want)
		}
	}
	if len(got.Containers[0].Mounts) != 1 || got.Containers[0].Mounts[0] != "/mnt/user/docs/db" {
		t.Fatalf("pool-user mounts = %v", got.Containers[0].Mounts)
	}
	if len(got.OpenPaths) != 1 || got.OpenPaths[0] != "live.db" {
		t.Fatalf("openPaths = %v, want [live.db]", got.OpenPaths)
	}
}

// TestHandler_GetShareRelocationPrecheck_NothingUsingTheShare pins the empty
// answer: reachable Docker, no matching mount and no open file give empty
// lists, not nulls, and dockerAvailable true.
func TestHandler_GetShareRelocationPrecheck_NothingUsingTheShare(t *testing.T) {
	f := newPrecheckFixture(t)
	f.apps.AddContainer(container.Container{ID: "a4", Name: "elsewhere", State: "running", Mounts: []container.Mount{{Source: "/mnt/user/media", Destination: "/data"}}})

	got, err := f.h.GetShareRelocationPrecheck(context.Background(), apiv1.GetShareRelocationPrecheckParams{Name: "docs"})
	if err != nil {
		t.Fatalf("GetShareRelocationPrecheck: %v", err)
	}
	if !got.DockerAvailable || got.Containers == nil || len(got.Containers) != 0 || got.OpenPaths == nil || len(got.OpenPaths) != 0 {
		t.Fatalf("answer = %+v, want available with empty non-nil lists", got)
	}
}

// TestHandler_GetShareRelocationPrecheck_DockerState separates "Docker is not
// reachable" (available=false, no error) from a listing failure, which fails
// the request instead of reading as "no container uses the share".
func TestHandler_GetShareRelocationPrecheck_DockerState(t *testing.T) {
	params := apiv1.GetShareRelocationPrecheckParams{Name: "docs"}

	t.Run("not configured", func(t *testing.T) {
		f := newPrecheckFixture(t)
		f.h.Container = nil
		got, err := f.h.GetShareRelocationPrecheck(context.Background(), params)
		if err != nil || got.DockerAvailable {
			t.Fatalf("answer = %+v, %v; want dockerAvailable=false and no error", got, err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		f := newPrecheckFixture(t)
		f.apps.SetUnavailable(nil)
		got, err := f.h.GetShareRelocationPrecheck(context.Background(), params)
		if err != nil || got.DockerAvailable {
			t.Fatalf("answer = %+v, %v; want dockerAvailable=false and no error", got, err)
		}
	})
	t.Run("listing fails", func(t *testing.T) {
		f := newPrecheckFixture(t)
		f.apps.SetUnavailable(errors.New("engine returned 500"))
		got, err := f.h.GetShareRelocationPrecheck(context.Background(), params)
		if err == nil {
			t.Fatalf("a failed listing answered %+v with no error", got)
		}
	})
}

// TestHandler_GetShareRelocationPrecheck_Refusals pins the refusals shared
// with startShareRelocation: an unknown share, an array with no cache disk,
// the scheduler's maintenance-mode and unfinished-migration admission checks
// and a daemon whose resolution hook is not wired never answer 200.
func TestHandler_GetShareRelocationPrecheck_Refusals(t *testing.T) {
	ctx := context.Background()
	params := apiv1.GetShareRelocationPrecheckParams{Name: "docs"}

	t.Run("unknown share", func(t *testing.T) {
		f := newPrecheckFixture(t)
		_, err := f.h.GetShareRelocationPrecheck(ctx, apiv1.GetShareRelocationPrecheckParams{Name: "nope"})
		if status := apiError(t, f.h, err); status.StatusCode != 404 {
			t.Fatalf("status = %d, want 404", status.StatusCode)
		}
	})
	t.Run("no cache disk", func(t *testing.T) {
		h, _, _ := newShareRelocationTestHandlerWithCache(t, false)
		if _, err := h.Shares.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.ArrayOnly}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		h.RelocationShare = func(context.Context, string) (cache.Share, error) { return cache.Share{Name: "docs"}, nil }
		_, err := h.GetShareRelocationPrecheck(ctx, params)
		status := apiError(t, h, err)
		if status.StatusCode != 409 || status.Response.Code != "no_cache_disk" {
			t.Fatalf("answer = %d %q, want 409 no_cache_disk", status.StatusCode, status.Response.Code)
		}
	})
	t.Run("scheduler gates", func(t *testing.T) {
		gates := []struct {
			name     string
			enter    func(t *testing.T, s *job.Scheduler)
			wantCode string
		}{
			{
				name: "maintenance_mode",
				enter: func(t *testing.T, s *job.Scheduler) {
					if err := s.EnterMaintenance(ctx); err != nil {
						t.Fatalf("EnterMaintenance: %v", err)
					}
				},
				wantCode: "maintenance_mode",
			},
			{
				name: "migration_in_progress",
				enter: func(_ *testing.T, s *job.Scheduler) {
					s.SetMigrationPending(func(context.Context) (bool, error) { return true, nil })
				},
				wantCode: "migration_in_progress",
			},
		}
		for _, g := range gates {
			t.Run(g.name, func(t *testing.T) {
				f := newPrecheckFixture(t)
				g.enter(t, f.h.Scheduler)
				got, err := f.h.GetShareRelocationPrecheck(ctx, params)
				if err == nil {
					t.Fatalf("answered %+v while the scheduler refuses a relocation", got)
				}
				status := apiError(t, f.h, err)
				if status.StatusCode != 409 || status.Response.Code != g.wantCode {
					t.Fatalf("answer = %d %q, want 409 %s", status.StatusCode, status.Response.Code, g.wantCode)
				}
				jobs, err := f.h.Store.List(ctx, job.ListFilter{})
				if err != nil {
					t.Fatalf("listing jobs: %v", err)
				}
				if len(jobs) != 0 {
					t.Fatalf("the precheck queued %d job(s), want none", len(jobs))
				}
			})
		}
	})
	t.Run("resolution hook not wired", func(t *testing.T) {
		f := newPrecheckFixture(t)
		f.h.RelocationShare = nil
		_, err := f.h.GetShareRelocationPrecheck(ctx, params)
		if status := apiError(t, f.h, err); status.StatusCode != 501 {
			t.Fatalf("status = %d, want 501", status.StatusCode)
		}
	})
	t.Run("resolution fails", func(t *testing.T) {
		f := newPrecheckFixture(t)
		f.h.RelocationShare = func(context.Context, string) (cache.Share, error) { return cache.Share{}, errors.New("boom") }
		if got, err := f.h.GetShareRelocationPrecheck(ctx, params); err == nil {
			t.Fatalf("a failed resolution answered %+v", got)
		}
	})
}
