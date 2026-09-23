package cache_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newResultTestDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "mover-result.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestResultStore_EmptyBeforeAnySave(t *testing.T) {
	s := cache.NewResultStore(newResultTestDB(t))
	if _, err := s.LastRun(context.Background()); err != cache.ErrNoMoverRun {
		t.Fatalf("LastRun = %v, want ErrNoMoverRun", err)
	}
	if _, err := s.CacheUsage(context.Background()); err != cache.ErrNoMoverRun {
		t.Fatalf("CacheUsage = %v, want ErrNoMoverRun", err)
	}
}

func TestResultStore_SaveFromReportPersistsRunAndUsage(t *testing.T) {
	ctx := context.Background()
	s := cache.NewResultStore(newResultTestDB(t))

	root := t.TempDir()
	cacheMount := filepath.Join(root, "cache")
	appdata := filepath.Join(cacheMount, "appdata")
	pending := filepath.Join(cacheMount, "movies")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pending, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdata, "db.sqlite"), []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "movie.mkv"), []byte("bbbbbbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheMount, "stray.bin"), []byte("cc"), 0o644); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 9, 23, 2, 0, 0, 0, time.UTC)
	finished := started.Add(5 * time.Second)
	report := cache.Report{
		StartedAt:  started,
		FinishedAt: finished,
		Entries: []cache.Entry{
			{Share: "movies", Path: "a.mkv", Bytes: 100, Result: cache.ResultMoved},
			{Share: "movies", Path: "open.mkv", Bytes: 50, Result: cache.ResultSkippedOpen, Reason: "held by pid 1"},
		},
	}
	shares := []cache.UsageShare{
		{Name: "appdata", Path: appdata, Mode: "cache-only"},
		{Name: "movies", Path: pending, Mode: "cache-then-move"},
	}
	if err := s.SaveFromReport(ctx, report, cacheMount, shares); err != nil {
		t.Fatalf("SaveFromReport: %v", err)
	}

	run, err := s.LastRun(ctx)
	if err != nil {
		t.Fatalf("LastRun: %v", err)
	}
	if run.FilesMoved != 1 || run.BytesMoved != 100 {
		t.Fatalf("moved = %d/%d, want 1/100", run.FilesMoved, run.BytesMoved)
	}
	if run.DurationMs != 5000 {
		t.Fatalf("DurationMs = %d, want 5000", run.DurationMs)
	}
	if len(run.Skipped) != 1 || run.Skipped[0].Result != cache.ResultSkippedOpen {
		t.Fatalf("Skipped = %+v, want one skipped_open", run.Skipped)
	}
	if run.Skipped[0].Reason != "held by pid 1" {
		t.Fatalf("Skipped[0].Reason = %q, want held by pid 1", run.Skipped[0].Reason)
	}

	usage, err := s.CacheUsage(ctx)
	if err != nil {
		t.Fatalf("CacheUsage: %v", err)
	}
	if usage.AppdataBytes != 4 {
		t.Fatalf("AppdataBytes = %d, want 4", usage.AppdataBytes)
	}
	if usage.PendingMovesBytes != 8 {
		t.Fatalf("PendingMovesBytes = %d, want 8", usage.PendingMovesBytes)
	}
	if usage.OtherBytes < 2 {
		t.Fatalf("OtherBytes = %d, want at least the stray 2-byte file", usage.OtherBytes)
	}
}

func TestPersistedRunFromReport_ZeroStartedIsIgnoredBySave(t *testing.T) {
	s := cache.NewResultStore(newResultTestDB(t))
	if err := s.SaveFromReport(context.Background(), cache.Report{}, "/tmp", nil); err != nil {
		t.Fatalf("SaveFromReport(zero): %v", err)
	}
	if _, err := s.LastRun(context.Background()); err != cache.ErrNoMoverRun {
		t.Fatalf("LastRun after zero report = %v, want ErrNoMoverRun", err)
	}
}
