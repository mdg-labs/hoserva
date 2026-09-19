package notify

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newTestDB applies every real, embedded schema migration (D16), so this
// package's persistence tests exercise the notify_* tables exactly as
// internal/store.Runner leaves them at daemon startup — never a
// hand-built CREATE TABLE (mirrors internal/job's own newTestDB). It opens
// through store.DSN, the same connection factory cmd/hoservad uses, so a
// test never gets a pragma (like busy_timeout alone, without
// foreign_keys) that the daemon's real connections don't also get.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return openTestDB(t, newTestDBPath(t))
}

// newTestDBPath returns a fresh temp-file database path for a test,
// without opening it — callers that need a second, independent
// connection to the same file (proving a fix doesn't depend on which
// pooled connection happens to run a query) use this plus openTestDB.
func newTestDBPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "notify-test.db")
}

// openTestDB opens dbPath through store.DSN and, if it is not yet
// migrated, applies every embedded schema migration.
func openTestDB(t *testing.T, dbPath string) *sql.DB {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}

	db, err := sql.Open("sqlite", store.DSN(dbPath))
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
