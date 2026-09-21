//go:build lab

// Runs only inside the loop-device lab (doc 06 §3, Q45), the same pattern
// snapraid_lab_test.go documents at its own top: compiled on the host
// with `go test -tags lab -c`, executed with `docker compose exec -T lab
// <binary>` inside the lab container. Exercises #223's real, end-to-end
// path a hand-authored fixture cannot: a real `snapraid sync` followed by
// a real `snapraid list`, parsed and persisted into a real (embedded
// migrations) SQLite database.

package parity

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func labTestDB(t *testing.T) *sql.DB {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "usage-lab-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("opening lab test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return db
}

// TestLabComputeShareUsage_RealSyncAndList seeds two shares across the
// lab's real disks, syncs for real, and confirms ComputeShareUsage
// (List, parsed, aggregated and persisted) reports the real byte counts —
// the end-to-end claim this issue makes: the figures come from
// SnapRAID's own tracked state, never a live walk. Share directory names
// are unique to this file (p223movies/p223photos), never "movies" or
// "photos" — disk1-3 are shared, accumulated state across every lab test
// file in this package within one test binary run (lifecycle_lab_test.go
// and guard_lab_test.go both already write under "movies"), so an exact
// byte-total assertion needs a share name nothing else in this package
// ever writes to.
func TestLabComputeShareUsage_RealSyncAndList(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := labEngine(t, lab)
	engine.Usage = NewUsageStore(labTestDB(t))

	writeFile(t, filepath.Join(mounts[0], "p223movies", "a.bin"), 40_000)
	writeFile(t, filepath.Join(mounts[1], "p223movies", "b.bin"), 60_000)
	writeFile(t, filepath.Join(mounts[0], "p223photos", "p.bin"), 5_000)

	syncOnce(t, ctx, engine)

	if err := engine.ComputeShareUsage(ctx); err != nil {
		t.Fatalf("ComputeShareUsage: %v", err)
	}

	snap, ok, err := engine.Usage.Get(ctx, "p223movies")
	if err != nil {
		t.Fatalf("Usage.Get(p223movies): %v", err)
	}
	if !ok {
		t.Fatal("Usage.Get(p223movies): ok = false after ComputeShareUsage")
	}
	total := int64(0)
	for _, b := range snap.Disks {
		total += b
	}
	if total != 100_000 {
		t.Fatalf("p223movies total bytes = %d, want 100000 (Disks=%+v)", total, snap.Disks)
	}
	if len(snap.Disks) != 2 {
		t.Fatalf("p223movies Disks = %+v, want files on 2 disks", snap.Disks)
	}

	photos, ok, err := engine.Usage.Get(ctx, "p223photos")
	if err != nil {
		t.Fatalf("Usage.Get(p223photos): %v", err)
	}
	if !ok {
		t.Fatal("Usage.Get(p223photos): ok = false")
	}
	photosTotal := int64(0)
	for _, b := range photos.Disks {
		photosTotal += b
	}
	if photosTotal != 5_000 {
		t.Fatalf("p223photos total bytes = %d, want 5000", photosTotal)
	}
}
