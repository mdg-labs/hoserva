package parity

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newTestDB applies every real, embedded schema migration (D16, doc 01
// §4) to a fresh SQLite database in t.TempDir() — this package's own
// share_usage persistence tests exercise the table exactly as
// internal/store.Runner leaves it at daemon startup, never a hand-built
// CREATE TABLE (matching internal/job's own testdb_test.go).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

func TestUsageStore_NeverComputed(t *testing.T) {
	ctx := context.Background()
	s := NewUsageStore(newTestDB(t))

	if _, err := s.ComputedAt(ctx); !errors.Is(err, ErrUsageNeverComputed) {
		t.Fatalf("ComputedAt = %v, want ErrUsageNeverComputed", err)
	}
	_, ok, err := s.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatal("Get: ok = true before any computation ran")
	}
	_, _, ok, err = s.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if ok {
		t.Fatal("ListAll: ok = true before any computation ran")
	}
}

func TestUsageStore_ReplaceAndGet(t *testing.T) {
	ctx := context.Background()
	s := NewUsageStore(newTestDB(t))
	at := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	rows := []ShareUsage{
		{Share: "movies", Disk: "/mnt/disk1", Bytes: 100},
		{Share: "movies", Disk: "/mnt/disk2", Bytes: 200},
		{Share: "photos", Disk: "/mnt/disk1", Bytes: 30},
	}
	if err := s.Replace(ctx, rows, at); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	snap, ok, err := s.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false after Replace")
	}
	if !snap.ComputedAt.Equal(at) {
		t.Errorf("ComputedAt = %v, want %v", snap.ComputedAt, at)
	}
	want := map[string]int64{"/mnt/disk1": 100, "/mnt/disk2": 200}
	if len(snap.Disks) != len(want) || snap.Disks["/mnt/disk1"] != 100 || snap.Disks["/mnt/disk2"] != 200 {
		t.Errorf("Disks = %+v, want %+v", snap.Disks, want)
	}

	// A share with no persisted row (never had files) is a legitimate
	// zero, not "not yet synced" — ok is still true, matching a share
	// that has genuinely been through a sync with nothing on any disk.
	snap, ok, err = s.Get(ctx, "backups")
	if err != nil {
		t.Fatalf("Get(backups): %v", err)
	}
	if !ok {
		t.Fatal("Get(backups): ok = false, want true (computed, zero bytes)")
	}
	if len(snap.Disks) != 0 {
		t.Errorf("Disks = %+v, want empty", snap.Disks)
	}

	allAt, all, ok, err := s.ListAll(ctx)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if !ok {
		t.Fatal("ListAll: ok = false after Replace")
	}
	if !allAt.Equal(at) {
		t.Errorf("ListAll computedAt = %v, want %v", allAt, at)
	}
	if len(all) != 2 {
		t.Fatalf("ListAll = %+v, want 2 shares", all)
	}
	if all["photos"]["/mnt/disk1"] != 30 {
		t.Errorf("ListAll[photos] = %+v, want disk1=30", all["photos"])
	}
}

// TestUsageStore_ReplaceClearsPreviousRows confirms a disk a share no
// longer occupies has no row after the next Replace, rather than a stale
// one lingering (doc 02 §1 line 78: recomputed from scratch each sync).
func TestUsageStore_ReplaceClearsPreviousRows(t *testing.T) {
	ctx := context.Background()
	s := NewUsageStore(newTestDB(t))
	first := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	second := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

	if err := s.Replace(ctx, []ShareUsage{{Share: "movies", Disk: "/mnt/disk1", Bytes: 100}}, first); err != nil {
		t.Fatalf("Replace (first): %v", err)
	}
	if err := s.Replace(ctx, []ShareUsage{{Share: "movies", Disk: "/mnt/disk2", Bytes: 50}}, second); err != nil {
		t.Fatalf("Replace (second): %v", err)
	}

	snap, ok, err := s.Get(ctx, "movies")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get: ok = false")
	}
	if !snap.ComputedAt.Equal(second) {
		t.Errorf("ComputedAt = %v, want %v", snap.ComputedAt, second)
	}
	if _, stillThere := snap.Disks["/mnt/disk1"]; stillThere {
		t.Errorf("Disks still has disk1 after a Replace that no longer lists it: %+v", snap.Disks)
	}
	if snap.Disks["/mnt/disk2"] != 50 {
		t.Errorf("Disks[/mnt/disk2] = %d, want 50", snap.Disks["/mnt/disk2"])
	}
}
