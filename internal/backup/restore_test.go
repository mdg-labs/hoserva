package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func dsnWAL(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
}

// TestRestoreDatabase_LivePoolSeesRestoredContentImmediately is the
// primitive-level proof behind #269's fix: a pooled WAL-mode connection
// left open across the restore, with a write still un-checkpointed in its
// own WAL, must see the restored content on its very next statement — not
// the pre-restore rows, and not an error. copyFileAtomic (the deleted
// file-rename approach this replaces) failed exactly this scenario: see
// internal/api's TestImportConfig_ReplacesRunningDatabaseWithoutCorruption
// for the same failure reproduced through the real handler.
func TestRestoreDatabase_LivePoolSeesRestoredContentImmediately(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "live.db")

	db, err := sql.Open("sqlite", dsnWAL(livePath))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 'before')"); err != nil {
		t.Fatalf("seeding row: %v", err)
	}
	// Leaves a write un-checkpointed in the live pool's own WAL — the
	// exact condition the reported corruption needed.
	if _, err := db.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (2, 'still-in-wal')"); err != nil {
		t.Fatalf("seeding second row: %v", err)
	}

	srcPath := filepath.Join(dir, "src.db")
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("opening source database: %v", err)
	}
	if _, err := src.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		t.Fatalf("creating source table: %v", err)
	}
	if _, err := src.ExecContext(ctx, "INSERT INTO t (id, v) VALUES (1, 'restored')"); err != nil {
		t.Fatalf("seeding source row: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("closing source database: %v", err)
	}

	if err := RestoreDatabase(ctx, db, srcPath); err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}

	// Keep using the SAME pool for one more statement — must see the
	// restored row, not the pre-restore one, and the un-checkpointed row
	// must be gone.
	var v string
	if err := db.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("reading restored row on the live pool: %v", err)
	}
	if v != "restored" {
		t.Fatalf("live pool read after restore = %q, want %q", v, "restored")
	}
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM t").Scan(&count); err != nil {
		t.Fatalf("counting rows on the live pool: %v", err)
	}
	if count != 1 {
		t.Fatalf("live pool row count after restore = %d, want 1 (the pre-restore id=2 row must be gone)", count)
	}

	// Close and reopen fresh, standing in for a daemon restart.
	if err := db.Close(); err != nil {
		t.Fatalf("closing live pool: %v", err)
	}
	fresh, err := sql.Open("sqlite", dsnWAL(livePath))
	if err != nil {
		t.Fatalf("reopening restored database: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	var check string
	if err := fresh.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		t.Fatalf("integrity_check after restart: %v", err)
	}
	if check != "ok" {
		t.Fatalf("integrity_check after restart = %q, want ok", check)
	}
	if err := fresh.QueryRowContext(ctx, "SELECT v FROM t WHERE id = 1").Scan(&v); err != nil {
		t.Fatalf("reading restored row after restart: %v", err)
	}
	if v != "restored" {
		t.Fatalf("read after restart = %q, want %q", v, "restored")
	}
}

// TestRestoreDatabase_SourceFileUntouched confirms RestoreDatabase opens
// srcPath read-only: restoring must never mutate the verified archive copy
// it is reading from.
func TestRestoreDatabase_SourceFileUntouched(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "live.db")
	db, err := sql.Open("sqlite", dsnWAL(livePath))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}

	srcPath := filepath.Join(dir, "src.db")
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatalf("opening source database: %v", err)
	}
	if _, err := src.ExecContext(ctx, "CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating source table: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("closing source database: %v", err)
	}
	before, err := hashFile(srcPath)
	if err != nil {
		t.Fatalf("hashing source before restore: %v", err)
	}

	if err := RestoreDatabase(ctx, db, srcPath); err != nil {
		t.Fatalf("RestoreDatabase: %v", err)
	}

	after, err := hashFile(srcPath)
	if err != nil {
		t.Fatalf("hashing source after restore: %v", err)
	}
	if before != after {
		t.Fatalf("source file changed during restore: before=%s after=%s", before, after)
	}
}
