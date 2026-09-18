package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"

	"github.com/mdg-labs/hoserva/internal/store/transforms"
)

// v is a fixed-width, 14-digit test version — sqlite-migrate's own
// sortable timestamp format (Q60), so ordering and the snapshot filename
// pattern behave exactly as they do against real, generated migrations.
func v(n int) string { return fmt.Sprintf("%014d", n) }

// testMigration builds a Migration the way sqlitemigrate.Load would from a
// real file: its Checksum is computed from sqlText, so a "tampered"
// migration built from different SQL always gets a different checksum,
// rather than a hand-picked one a test could forget to change.
func testMigration(version, slug, sqlText string) Migration {
	return Migration{
		Version:  version,
		Slug:     slug,
		Filename: version + "_" + slug + ".sql",
		SQL:      sqlText,
		Checksum: sqlitemigrate.Checksum(sqlText),
	}
}

func newRunner(t *testing.T, migrations []Migration) (*Runner, *sql.DB) {
	t.Helper()
	db := openTestDB(t)
	return &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}, db
}

func TestRunner_AppliesAllPending(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);"),
		testMigration(v(2), "b", "ALTER TABLE a ADD COLUMN note TEXT;"),
	}
	r, db := newRunner(t, migrations)

	applied, snapshotPath, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("len(applied) = %d, want 2", len(applied))
	}
	if snapshotPath == "" {
		t.Fatal("expected a snapshot path")
	}

	version, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != v(2) {
		t.Fatalf("CurrentVersion = %q, want %q", version, v(2))
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO a (id, note) VALUES (1, 'x')"); err != nil {
		t.Fatalf("resulting schema doesn't match what was migrated: %v", err)
	}
}

func TestRunner_NoOpWhenNothingPending(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);"),
	}
	r, _ := newRunner(t, migrations)

	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	applied, snapshotPath, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if applied != nil {
		t.Fatalf("second Apply applied = %v, want nil", applied)
	}
	if snapshotPath != "" {
		t.Fatalf("second Apply took a snapshot with nothing pending: %q", snapshotPath)
	}
}

// A database with a migration recorded as applied that this binary's own
// Migrations slice doesn't include — whether because a newer binary
// already upgraded it, or because the file was deleted — is refused via
// sqlitemigrate.PendingMigrations' own MissingMigrationError (Q60): no
// bespoke "newer database" detection lives in this package.
func TestRunner_RefusesNewerDatabase(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);"),
	}
	r, db := newRunner(t, migrations)

	if err := ensureBookkeeping(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s (version, slug, checksum, applied_at) VALUES (?, ?, ?, ?)", bookkeepingTable),
		v(99), "future", "x", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	_, _, err := r.Apply(ctx)
	var missing *sqlitemigrate.MissingMigrationError
	if !errors.As(err, &missing) {
		t.Fatalf("Apply err = %v, want *sqlitemigrate.MissingMigrationError", err)
	}
	if missing.Version != v(99) {
		t.Fatalf("MissingMigrationError.Version = %q, want %q", missing.Version, v(99))
	}
}

func TestRunner_RefusesTamperedEmbeddedMigration(t *testing.T) {
	ctx := context.Background()
	original := testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	r, db := newRunner(t, []Migration{original})
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// A later build embeds a migration with the same version but different
	// content than what was actually applied — the exact scenario doc 01
	// §4 says must refuse to start.
	tampered := testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);")
	r2 := &Runner{DB: db, Migrations: []Migration{tampered}, SnapshotDir: r.SnapshotDir}

	_, _, err := r2.Apply(ctx)
	var mismatch *sqlitemigrate.ChecksumMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("Apply err = %v, want *sqlitemigrate.ChecksumMismatchError", err)
	}
}

// The data-loss scenario this issue exists to prevent: a later migration
// in a multi-migration batch fails, and both the schema change and the
// data change an earlier migration in that same batch made must not be
// left applied. This seeds a real row so the assertion can notice a row
// an earlier migration in the same failing batch modified, not only a
// changed schema.
//
// The third migration's own failure is a duplicate CREATE TABLE — an
// ordinary statement only SQLite itself rejects once actually run inside
// the transaction, proving rollback once real work is already in flight,
// not merely that something was refused before Apply ever began.
//
// This is the data-loss regression test the issue calls for by name
// (mirroring #18, ported from the retired allow-list/checksum stack to
// this runner's own sqlite-migrate-backed apply loop): it compares the
// on-disk database *file* byte-for-byte, not just its schema and row
// content, against a real SQLite file — never a mock — so a mutation that
// left some other, unobserved page written would still be caught.
func TestRunner_InjectedFailureLeavesDatabaseUnchanged_LaterMigrationInBatch(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	setup := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT NOT NULL);"),
	}
	r := &Runner{DB: db, Migrations: setup, SnapshotDir: t.TempDir()}
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("setup Apply: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO a (id, v) VALUES (1, 'seed')"); err != nil {
		t.Fatal(err)
	}

	before, err := DumpSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	beforeVersion, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var seedBefore string
	if err := db.QueryRowContext(ctx, "SELECT v FROM a WHERE id = 1").Scan(&seedBefore); err != nil {
		t.Fatal(err)
	}
	// Closing before the byte comparison below forces every page SQLite
	// still holds only in its own in-process cache out to dbPath, so the
	// comparison is of the real file's committed bytes, not a snapshot
	// that happens to omit an unflushed page either side of Apply.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r.DB = db
	batchV2 := testMigration(v(2), "update", "CREATE TABLE noop (id INTEGER PRIMARY KEY);")
	batchV3 := testMigration(v(3), "bad", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	r.Migrations = append(r.Migrations, batchV2, batchV3)
	r.Transforms = []transforms.Transform{{
		Version:  batchV2.Version,
		Checksum: batchV2.Checksum,
		Name:     "test-only: change the seeded row",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE a SET v = 'changed-by-the-batch' WHERE id = 1")
			return err
		},
	}}

	if _, _, err := r.Apply(ctx); err == nil {
		t.Fatal("expected Apply to fail on the duplicate table in the third migration")
	}

	after, err := DumpSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("schema changed despite a failed migration batch:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	afterVersion, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterVersion != beforeVersion {
		t.Fatalf("CurrentVersion = %q, want unchanged %q", afterVersion, beforeVersion)
	}
	var seedAfter string
	if err := db.QueryRowContext(ctx, "SELECT v FROM a WHERE id = 1").Scan(&seedAfter); err != nil {
		t.Fatal(err)
	}
	if seedAfter != seedBefore {
		t.Fatalf("row content changed despite a failed migration batch: got %q, want unchanged %q", seedAfter, seedBefore)
	}
	assertIntegrityOK(t, ctx, db)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("database file changed byte-for-byte despite a failed migration batch (before %d bytes, after %d bytes) — a failed Apply must leave the file exactly as it was, not just its logical content", len(beforeBytes), len(afterBytes))
	}
}

// This test corrupts a real database deterministically to force a genuine
// integrity_check failure, rather than faking one through a swappable call
// site: SQLite's own `PRAGMA writable_schema` mechanism repoints an
// index's rootpage at a different object's page without touching the
// object it actually indexes, and bumping `PRAGMA schema_version` forces
// the very next statement on the same connection to reparse the tampered
// catalog.
//
// The corruption runs as a test-only data transform (transforms.Fn is Go
// code Apply calls directly against the open *sql.Tx), not as the
// migration's own SQL text, so this test isolates integrity_check itself,
// not any refusal of the migration's own statement shape.
func TestRunner_RealIntegrityCheckFailureRollsBackBatch(t *testing.T) {
	ctx := context.Background()
	setupMigration := testMigration(v(1), "setup", `
		CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT);
		CREATE TABLE b (id INTEGER PRIMARY KEY);
		CREATE INDEX ai ON a(v);
	`)
	noopMigration := testMigration(v(2), "noop", "CREATE TABLE noop (id INTEGER PRIMARY KEY);")
	seedData := transforms.Transform{
		Version:  setupMigration.Version,
		Checksum: setupMigration.Checksum,
		Name:     "test-only: seed rows for the corruption below",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO a (id, v) VALUES (1, 'x'), (2, 'y')"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO b (id) VALUES (1)")
			return err
		},
	}
	corruptIndex := transforms.Transform{
		Version:  noopMigration.Version,
		Checksum: noopMigration.Checksum,
		Name:     "test-only: corrupt index ai's rootpage via writable_schema",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			for _, stmt := range []string{
				"PRAGMA writable_schema = ON",
				"UPDATE sqlite_master SET rootpage = (SELECT rootpage FROM sqlite_master WHERE name = 'b') WHERE name = 'ai'",
				"PRAGMA schema_version = 999999999",
				"PRAGMA writable_schema = OFF",
			} {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("test-only corruption step %q: %w", stmt, err)
				}
			}
			return nil
		},
	}
	r, db := newRunner(t, []Migration{setupMigration, noopMigration})
	r.Transforms = []transforms.Transform{seedData, corruptIndex}

	_, _, err := r.Apply(ctx)
	if err == nil {
		t.Fatal("expected Apply to fail on a corrupted index that fails integrity_check")
	}
	if !strings.Contains(err.Error(), "integrity_check failed") {
		t.Fatalf("Apply err = %v, want it to name integrity_check as the failure — otherwise this isn't proof integrity_check itself ran and caught the corruption", err)
	}

	version, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != zeroSchemaVersion {
		t.Fatalf("CurrentVersion = %q, want %q (neither migration must be recorded as applied after a failing integrity_check)", version, zeroSchemaVersion)
	}
	var hasA int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name = 'a'").Scan(&hasA); err != nil {
		t.Fatal(err)
	}
	if hasA != 0 {
		t.Fatal("table a exists despite a failing integrity_check — the whole batch, including migration 1, must roll back")
	}
}

// The other injected-failure scenario the issue calls for by name: a
// failing PRAGMA foreign_key_check. Foreign keys are suspended for the
// whole migration transaction (doc 01 §4), so a migration that inserts a
// row violating a declared foreign key does not fail at INSERT time — it
// must be caught by foreign_key_check before COMMIT, and the whole batch
// rolled back.
func TestRunner_InjectedFailureLeavesDatabaseUnchanged_ForeignKeyCheck(t *testing.T) {
	ctx := context.Background()
	setupMigration := testMigration(v(1), "setup", `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));
	`)
	r, db := newRunner(t, []Migration{setupMigration})
	r.Transforms = []transforms.Transform{{
		Version:  setupMigration.Version,
		Checksum: setupMigration.Checksum,
		Name:     "test-only: seed the parent row",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (1)")
			return err
		},
	}}
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("setup Apply: %v", err)
	}

	before, err := DumpSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var rowsBefore int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM child").Scan(&rowsBefore); err != nil {
		t.Fatal(err)
	}

	orphanMigration := testMigration(v(2), "orphan", "CREATE TABLE noop (id INTEGER PRIMARY KEY);")
	r.Migrations = append(r.Migrations, orphanMigration)
	r.Transforms = append(r.Transforms, transforms.Transform{
		Version:  orphanMigration.Version,
		Checksum: orphanMigration.Checksum,
		Name:     "test-only: insert a row that violates the declared foreign key",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO child (id, parent_id) VALUES (1, 999)")
			return err
		},
	})

	_, _, err = r.Apply(ctx)
	if err == nil {
		t.Fatal("expected Apply to fail foreign_key_check on an orphaned insert")
	}
	if !strings.Contains(err.Error(), "foreign_key_check") {
		t.Fatalf("Apply err = %v, want it to name foreign_key_check as the failure", err)
	}

	after, err := DumpSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("schema changed despite a failed migration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var rowsAfter int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM child").Scan(&rowsAfter); err != nil {
		t.Fatal(err)
	}
	if rowsAfter != rowsBefore {
		t.Fatalf("child row count = %d, want unchanged %d", rowsAfter, rowsBefore)
	}
	version, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != v(1) {
		t.Fatalf("CurrentVersion = %q, want %q (the orphan migration must not be recorded as applied)", version, v(1))
	}
	assertIntegrityOK(t, ctx, db)
}

// Checking only that a snapshot path was returned would let a mutation
// that snapshotted after commit (or not at all) pass silently. This opens
// the snapshot Apply returns and checks it holds the state from *before*
// the migration it precedes, not after.
func TestRunner_SnapshotHoldsPreMigrationState(t *testing.T) {
	ctx := context.Background()
	setup := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT NOT NULL);"),
	}
	r, db := newRunner(t, setup)
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("setup Apply: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO a (id, v) VALUES (1, 'before')"); err != nil {
		t.Fatal(err)
	}

	snapshotMigration := testMigration(v(2), "b", "ALTER TABLE a ADD COLUMN extra TEXT;")
	r.Migrations = append(r.Migrations, snapshotMigration)
	r.Transforms = []transforms.Transform{{
		Version:  snapshotMigration.Version,
		Checksum: snapshotMigration.Checksum,
		Name:     "test-only: change the seeded row",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE a SET v = 'after'")
			return err
		},
	}}
	_, snapshotPath, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if snapshotPath == "" {
		t.Fatal("expected a snapshot path")
	}

	snap, err := sql.Open("sqlite", snapshotPath)
	if err != nil {
		t.Fatalf("opening snapshot: %v", err)
	}
	defer closeQuietly(snap)

	var version string
	if err := snap.QueryRowContext(ctx, fmt.Sprintf("SELECT max(version) FROM %s", bookkeepingTable)).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != v(1) {
		t.Fatalf("snapshot's recorded schema version = %q, want %q (pre-migration)", version, v(1))
	}

	var val string
	if err := snap.QueryRowContext(ctx, "SELECT v FROM a WHERE id = 1").Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != "before" {
		t.Fatalf("snapshot row v = %q, want %q (pre-migration)", val, "before")
	}

	var extra sql.NullString
	err = snap.QueryRowContext(ctx, "SELECT extra FROM a WHERE id = 1").Scan(&extra)
	if err == nil {
		t.Fatal("snapshot has the column the migration it precedes adds — it should have only the pre-migration schema")
	}
}

// A test database opened without FK enforcement to begin with makes
// PRAGMA foreign_keys = OFF a no-op with nothing to suspend, so this
// exercises foreign-key suspension properly: it opens the connection with
// enforcement genuinely
// on (confirmed below, so the test would fail if it weren't), then runs a
// migration that rebuilds `parent` — SQLite's documented way to change a
// column's type or constraints, and also its documented case where DROP
// TABLE processes a child's ON DELETE CASCADE if foreign_keys is left on
// during the drop. If Apply's suspension didn't work, this would
// cascade-delete every child row referencing the dropped parent table.
func TestRunner_SuspendsForeignKeyEnforcementDuringRebuild(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1) // pin to the exact connection Apply uses and restores

	setupMigration := testMigration(v(1), "setup", `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id) ON DELETE CASCADE);
	`)
	r := &Runner{DB: db, Migrations: []Migration{setupMigration}, SnapshotDir: t.TempDir(), Transforms: []transforms.Transform{{
		Version:  setupMigration.Version,
		Checksum: setupMigration.Checksum,
		Name:     "test-only: seed a parent and its child",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, "INSERT INTO parent (id) VALUES (1)"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO child (id, parent_id) VALUES (1, 1)")
			return err
		},
	}}}
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("setup Apply: %v", err)
	}

	var enforced int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatal(err)
	}
	if enforced != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d before the rebuild, want 1 — this test proves nothing about suspension if enforcement wasn't on to begin with", enforced)
	}

	r.Migrations = append(r.Migrations, testMigration(v(2), "rebuild_parent", `
		CREATE TABLE parent_new (id INTEGER PRIMARY KEY, note TEXT);
		INSERT INTO parent_new (id) SELECT id FROM parent;
		DROP TABLE parent;
		ALTER TABLE parent_new RENAME TO parent;
	`))
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("rebuild Apply: %v", err)
	}

	var childCount int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM child").Scan(&childCount); err != nil {
		t.Fatal(err)
	}
	if childCount != 1 {
		t.Fatalf("child rows = %d, want 1 — foreign key enforcement must be suspended during a migration's table rebuild, or SQLite cascade-deletes them when the parent is dropped", childCount)
	}
}

func TestRunner_RestoresForeignKeyEnforcementAfterApply(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		testMigration(v(1), "a", `
			CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id));
		`),
	}
	db := openTestDB(t)
	db.SetMaxOpenConns(1) // pin to the exact connection Apply used
	r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}

	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var enforced int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatal(err)
	}
	if enforced != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d after Apply, want 1 (restored)", enforced)
	}

	if _, err := db.ExecContext(ctx, "INSERT INTO child (id, parent_id) VALUES (1, 999)"); err == nil {
		t.Fatal("expected foreign key enforcement to reject an orphaned insert after Apply")
	}
}

func TestRunner_TransformRunsInSameTransactionAsItsMigration(t *testing.T) {
	ctx := context.Background()
	migrations := []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY, size_mb INTEGER NOT NULL);"),
		testMigration(v(2), "add_bytes", "ALTER TABLE a ADD COLUMN size_bytes INTEGER;"),
	}
	db := openTestDB(t)
	r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}

	// Version 1's own transaction seeds a row; version 3's transform must
	// see it, in the same transaction as the ALTER TABLE that added
	// size_bytes.
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO a (id, size_mb) VALUES (1, 4)"); err != nil {
		t.Fatal(err)
	}

	backfillMigration := testMigration(v(3), "add_bytes_backfill", "CREATE TABLE backfill_marker (id INTEGER PRIMARY KEY);")
	r.Migrations = append(r.Migrations, backfillMigration)
	r.Transforms = []transforms.Transform{{
		Version:  backfillMigration.Version,
		Checksum: backfillMigration.Checksum,
		Name:     "backfill size_bytes from size_mb",
		Fn: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, "UPDATE a SET size_bytes = size_mb * 1024 * 1024")
			return err
		},
	}}

	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("second Apply: %v", err)
	}

	var bytes int64
	if err := db.QueryRowContext(ctx, "SELECT size_bytes FROM a WHERE id = 1").Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	if bytes != 4*1024*1024 {
		t.Fatalf("size_bytes = %d, want %d", bytes, 4*1024*1024)
	}
}

func TestRunner_TransformFailureRollsBackItsMigrationToo(t *testing.T) {
	ctx := context.Background()
	onlyMigration := testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	migrations := []Migration{onlyMigration}
	db := openTestDB(t)
	r := &Runner{
		DB:         db,
		Migrations: migrations,
		Transforms: []transforms.Transform{{
			Version:  onlyMigration.Version,
			Checksum: onlyMigration.Checksum,
			Name:     "always fails",
			Fn: func(ctx context.Context, tx *sql.Tx) error {
				return errors.New("injected transform failure")
			},
		}},
		SnapshotDir: t.TempDir(),
	}

	if _, _, err := r.Apply(ctx); err == nil {
		t.Fatal("expected Apply to fail when its transform fails")
	}

	schema, err := DumpSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if schema != "" {
		t.Fatalf("schema = %q, want empty — the migration must roll back with its failed transform", schema)
	}
}

// This tests, at the level of the exact mechanism, what Apply's
// foreign-key-restore defer depends on: PRAGMA foreign_keys = ON through
// a context that is already cancelled fails outright, while the same
// call through context.WithoutCancel(ctx) succeeds regardless — exactly
// the difference between the caller's ctx and the context Apply's
// restore defer actually uses. Driving this through the whole of
// Runner.Apply would mean racing Apply's own internal cancellation timing
// to land exactly between commit and the deferred restore, which no fixed
// cancellation point can guarantee deterministically; this isolates the
// one fact the restore's correctness depends on instead.
func TestRunner_ForeignKeyRestoreSurvivesCallerContextCancellation(t *testing.T) {
	db := openTestDB(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(conn)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := conn.ExecContext(cancelled, "PRAGMA foreign_keys = ON"); err == nil {
		t.Fatal("expected PRAGMA foreign_keys = ON through an already-cancelled context to fail — otherwise this isn't exercising the mechanism this test targets")
	}

	if _, err := conn.ExecContext(context.WithoutCancel(cancelled), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("PRAGMA foreign_keys = ON through context.WithoutCancel(cancelled ctx) failed: %v — Apply's restore defer depends on this succeeding even when the caller's context is already done", err)
	}
}

// A mutation that pruned before a migration (instead of only after it
// commits), or that pruned even when the batch failed, needs enough real
// history to notice a wrongly-evicted *older* snapshot. This applies three
// real, successful upgrades (so KeepSnapshots snapshots already exist, none
// of them eligible for pruning yet on their own), then a fourth migration
// that fails, and asserts the very first upgrade's own snapshot — the one
// most at risk of eviction from a premature or on-failure prune — is still
// there.
//
// The fourth migration's own failure is a duplicate CREATE TABLE, an
// ordinary statement that only actually fails once SQLite itself runs it —
// proof this isn't refused before Snapshot is ever reached.
func TestRunner_FailedMigrationNeverPrunesExistingSnapshots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t)
	r := &Runner{DB: db, SnapshotDir: dir, Migrations: []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);"),
	}}
	_, oldestSnapshot, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	if oldestSnapshot == "" {
		t.Fatal("expected a snapshot path from the first Apply")
	}

	r.Migrations = append(r.Migrations, testMigration(v(2), "b", "ALTER TABLE a ADD COLUMN note TEXT;"))
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("apply v2: %v", err)
	}

	r.Migrations = append(r.Migrations, testMigration(v(3), "c", "ALTER TABLE a ADD COLUMN note2 TEXT;"))
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("apply v3: %v", err)
	}

	if _, err := os.Stat(oldestSnapshot); err != nil {
		t.Fatalf("oldest snapshot missing before the failing attempt even runs: %v", err)
	}

	r.Migrations = append(r.Migrations, testMigration(v(4), "bad", "CREATE TABLE a (id INTEGER PRIMARY KEY);"))
	_, failedSnapshot, err := r.Apply(ctx)
	if err == nil {
		t.Fatal("expected the duplicate table to fail once executed")
	}
	if failedSnapshot == "" {
		t.Fatal("expected Apply to have taken a snapshot before the migration it precedes ran and failed — the refusal did not happen before Snapshot")
	}
	if _, err := os.Stat(failedSnapshot); err != nil {
		t.Fatalf("the failing attempt's own snapshot is missing: %v", err)
	}

	if _, err := os.Stat(oldestSnapshot); err != nil {
		t.Fatalf("a failed migration attempt pruned an existing snapshot it must never touch: %v", err)
	}
}

// Five real, successful upgrades through Apply itself (not PruneSnapshots
// called directly) must leave exactly KeepSnapshots files on disk — the
// newest three fromVersions' own snapshots, not the three most recently
// written by wall clock, which happen to be the same order here but are a
// different rule (snapshot_test.go's own PruneSnapshots tests cover that
// distinction directly).
func TestRunner_ApplyKeepsOnlyNewestThreeSnapshots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := openTestDB(t)
	r := &Runner{DB: db, SnapshotDir: dir, Migrations: []Migration{
		testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);"),
	}}

	_, snap, err := r.Apply(ctx)
	if err != nil {
		t.Fatalf("apply v1: %v", err)
	}
	snapshots := []string{snap}

	for i := 2; i <= 5; i++ {
		r.Migrations = append(r.Migrations, testMigration(v(i), "b", fmt.Sprintf("ALTER TABLE a ADD COLUMN note%d TEXT;", i)))
		_, snap, err := r.Apply(ctx)
		if err != nil {
			t.Fatalf("apply v%d: %v", i, err)
		}
		snapshots = append(snapshots, snap)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != KeepSnapshots {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("snapshot directory has %d entries, want %d: %v", len(entries), KeepSnapshots, names)
	}

	for _, old := range snapshots[:len(snapshots)-KeepSnapshots] {
		if _, err := os.Stat(old); err == nil {
			t.Fatalf("snapshot %q from an older upgrade was not pruned", old)
		}
	}
	for _, newest := range snapshots[len(snapshots)-KeepSnapshots:] {
		if _, err := os.Stat(newest); err != nil {
			t.Fatalf("snapshot %q from one of the newest %d upgrades is missing: %v", newest, KeepSnapshots, err)
		}
	}
}

// TestRunner_ForeignKeyRestoreSurvivesCallerContextCancellation exercises
// the isolated mechanism but never Apply itself, so a regression in how
// Apply's own restore defer builds its context — using ctx instead of
// context.WithoutCancel(ctx) — could still pass the rest of the suite.
// This drives it through the real Apply: a transform that cancels the
// caller's own context and then fails is a deterministic point an
// Apply-level test can reach without racing Apply's own internal timing.
func TestRunner_ForeignKeyRestoreSurvivesCancellationDuringApply(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	onlyMigration := testMigration(v(1), "a", "CREATE TABLE a (id INTEGER PRIMARY KEY);")
	migrations := []Migration{onlyMigration}
	db := openTestDB(t)
	db.SetMaxOpenConns(1) // pin to the exact connection Apply uses and restores
	r := &Runner{
		DB:         db,
		Migrations: migrations,
		Transforms: []transforms.Transform{{
			Version:  onlyMigration.Version,
			Checksum: onlyMigration.Checksum,
			Name:     "cancels the caller's context, then fails",
			Fn: func(ctx context.Context, tx *sql.Tx) error {
				cancel()
				return errors.New("injected: context cancelled mid-transform")
			},
		}},
		SnapshotDir: t.TempDir(),
	}

	if _, _, err := r.Apply(ctx); err == nil {
		t.Fatal("expected Apply to fail when its transform fails")
	}

	var enforced int
	if err := db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatalf("querying the connection after a cancelled-context failure: %v", err)
	}
	if enforced != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d after a cancelled-context failure, want 1 (restored)", enforced)
	}
}

func assertIntegrityOK(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatal(err)
	}
	if result != "ok" {
		t.Fatalf("integrity_check = %q, want ok", result)
	}
}
