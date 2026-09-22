package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/config"
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
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("report bytes"), 0o640); err != nil {
		t.Fatal(err)
	}

	r.Register(job.TypeShareRelocation, true, job.RunShareRelocation(job.ShareRelocationDeps{
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
