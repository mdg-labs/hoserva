package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newMoverResultHandler builds Handler the way cmd/hoservad/main.go does
// for the mover path (#273): ResultStore on the handler, TypeMover
// registered with RunMover that persists into the same store.
func newMoverResultHandler(t *testing.T) (*api.Handler, *job.Scheduler, cache.Share, *cache.ResultStore) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "mover-handler.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	share := cache.Share{
		Name:      "movies",
		CachePath: filepath.Join(base, "cache", "movies"),
		ArrayPath: filepath.Join(base, "array", "movies"),
	}
	src := filepath.Join(share.CachePath, "movie.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("movie bytes"), 0o640); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	results := cache.NewResultStore(db)
	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	registry.Register(job.TypeMover, true, job.RunMover(job.MoverDeps{
		Shares: func(ctx context.Context) ([]cache.Share, error) {
			return []cache.Share{share}, nil
		},
		Results: results,
		UsagePlan: func(ctx context.Context) (string, []cache.UsageShare, error) {
			return filepath.Dir(share.CachePath), []cache.UsageShare{{
				Name: share.Name,
				Path: share.CachePath,
				Mode: "cache-then-move",
			}}, nil
		},
	}))
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)

	h := &api.Handler{
		Scheduler:    scheduler,
		Store:        jobStore,
		Logs:         logs,
		MoverResults: results,
	}
	return h, scheduler, share, results
}

// TestHandler_GetLastMoverRun_ReadsPersistedResult proves the API
// operation hoservad serves reads a real mover run's result through the
// same ResultStore wiring main.go uses (#273 Reachable-via).
func TestHandler_GetLastMoverRun_ReadsPersistedResult(t *testing.T) {
	ctx := context.Background()
	h, s, share, _ := newMoverResultHandler(t)

	null, err := h.GetLastMoverRun(ctx)
	if err != nil {
		t.Fatalf("GetLastMoverRun before run: %v", err)
	}
	if !null.IsNull() {
		t.Fatal("GetLastMoverRun before any run should be null")
	}

	got, err := h.StartMover(ctx)
	if err != nil {
		t.Fatalf("StartMover: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s, want succeeded", finished.Status)
	}

	res, err := h.GetLastMoverRun(ctx)
	if err != nil {
		t.Fatalf("GetLastMoverRun: %v", err)
	}
	run, ok := res.Get()
	if !ok {
		t.Fatal("GetLastMoverRun is null after a finished run")
	}
	if run.FilesMoved != 1 || run.BytesMoved != int64(len("movie bytes")) {
		t.Fatalf("moved = %d/%d, want 1/%d", run.FilesMoved, run.BytesMoved, len("movie bytes"))
	}
	if run.DurationMs < 0 {
		t.Fatalf("DurationMs = %d, want >= 0", run.DurationMs)
	}
	if _, err := os.Stat(filepath.Join(share.ArrayPath, "movie.mkv")); err != nil {
		t.Fatalf("file should have been moved: %v", err)
	}

	usageNil, err := h.GetCacheUsage(ctx)
	if err != nil {
		t.Fatalf("GetCacheUsage: %v", err)
	}
	usage, ok := usageNil.Get()
	if !ok {
		t.Fatal("GetCacheUsage is null after a finished run")
	}
	if usage.PendingMovesBytes != 0 {
		t.Fatalf("PendingMovesBytes = %d, want 0", usage.PendingMovesBytes)
	}
}
