package job

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newTestDB applies every real, embedded schema migration (D16, doc 01 §4)
// to a fresh SQLite database in t.TempDir(), so this package's persistence
// tests exercise the jobs table exactly as internal/store.Runner leaves it
// at daemon startup — never a hand-built CREATE TABLE.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}

	// busy_timeout: this package's own tests deliberately run several job
	// goroutines' writes concurrently (doc 01 §4's whole point is that
	// unrelated classes/scopes run at once) — SQLite allows only one
	// writer at a time, and without a busy timeout a second writer gets
	// SQLITE_BUSY immediately rather than waiting the (sub-millisecond, in
	// practice) moment for the first one to finish. Whatever wires the
	// daemon's own production *sql.DB needs the same pragma; Store itself
	// takes a DBTX and has no DSN to set it on.
	dbPath := filepath.Join(t.TempDir(), "jobs-test.db")
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
