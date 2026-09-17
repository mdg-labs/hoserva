package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sort"
	"time"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"

	"github.com/mdg-labs/hoserva/internal/store/transforms"
)

func closeQuietly(c io.Closer) { _ = c.Close() }

// bookkeepingTable is sqlite-migrate's own bookkeeping table name and row
// shape (Q60): this runner writes it by hand — its own transaction loop is
// what runs a Transform between one migration and the next, which
// sqlitemigrate.Runner.Apply has no hook for — but keeps the table itself
// identical to what sqlitemigrate.Runner.Apply would create, so
// `sqlite-migrate status`/`check`/`verify` keep working unmodified against
// the resulting database. It is this runner's own infrastructure, created
// outside the migration transaction so CurrentVersion can always be read,
// and is excluded on purpose from DumpSchema and from every fixture in
// testdata/db.
const bookkeepingTable = "schema_migrations"

var createBookkeepingSQL = fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
	version TEXT PRIMARY KEY,
	slug TEXT NOT NULL,
	checksum TEXT NOT NULL,
	applied_at TEXT NOT NULL
) STRICT
`, bookkeepingTable)

// zeroSchemaVersion is a fresh database's CurrentVersion — before any
// migration has ever applied — padded to snapshotPattern's fixed 14-digit
// width so a pre-first-migration snapshot's filename still matches it.
const zeroSchemaVersion = "00000000000000"

// sqlExecer is satisfied by both *sql.DB and *sql.Conn — Snapshot below
// needs whichever one the caller is already holding open, since VACUUM
// INTO must run outside any transaction (and, for Runner.Apply, on the
// same connection that then suspends foreign keys, per the PRAGMA
// foreign_keys connection-affinity note on Runner.Apply).
type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// sqlQuerier extends sqlExecer with QueryContext. Also satisfied by both
// *sql.DB and *sql.Conn — every bookkeeping read/write below takes one of
// these instead of assuming Runner.DB, specifically so Apply can route
// them through the single *sql.Conn it pins for its own duration. Reading
// through Runner.DB from inside Apply would ask the connection pool for a
// second connection while the first is still checked out, which
// self-deadlocks outright when the caller has limited the pool to one
// connection (as a production SQLite connection pool typically is).
type sqlQuerier interface {
	sqlExecer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Runner applies schema migrations to db at startup (doc 01 §4, D16, Q60).
// Migrations is normally Load()'s embedded result; tests pass a
// hand-built slice to exercise scenarios (a failing later migration, a
// failing foreign_key_check) that the real embedded migrations can't
// produce on demand.
type Runner struct {
	DB         *sql.DB
	Migrations []Migration
	Transforms []transforms.Transform
	// SnapshotDir must be its own dedicated directory (doc 01 §4's
	// /var/lib/hoserva/backups/pre-migration/ in production, a t.TempDir()
	// in a test) — never /var/lib/hoserva itself, or any directory shared
	// with anything besides this package's own snapshots: Snapshot chmods
	// the directory itself to 0700 on every call,
	// and PruneSnapshots removes every one of this package's own
	// snapshotPrefix-named files it doesn't need to keep, without
	// otherwise knowing what a shared directory's other contents are for.
	SnapshotDir string
}

// AppliedMigration is one row of the bookkeeping table — sqlite-migrate's
// own shape (Q60), reused directly rather than wrapped.
type AppliedMigration = sqlitemigrate.AppliedMigration

func ensureBookkeeping(ctx context.Context, execer sqlExecer) error {
	_, err := execer.ExecContext(ctx, createBookkeepingSQL)
	if err != nil {
		return fmt.Errorf("creating %s: %w", bookkeepingTable, err)
	}
	return nil
}

// Applied returns every migration the bookkeeping table records, ordered
// by version.
func (r *Runner) Applied(ctx context.Context) ([]AppliedMigration, error) {
	return appliedWith(ctx, r.DB)
}

func appliedWith(ctx context.Context, q sqlQuerier) ([]AppliedMigration, error) {
	if err := ensureBookkeeping(ctx, q); err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf(
		"SELECT version, slug, checksum, applied_at FROM %s ORDER BY version", bookkeepingTable))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", bookkeepingTable, err)
	}
	defer closeQuietly(rows)

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Slug, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CurrentVersion is the highest version recorded as applied, or
// zeroSchemaVersion for a fresh database.
func (r *Runner) CurrentVersion(ctx context.Context) (string, error) {
	return currentVersionWith(ctx, r.DB)
}

func currentVersionWith(ctx context.Context, q sqlQuerier) (string, error) {
	applied, err := appliedWith(ctx, q)
	if err != nil {
		return "", err
	}
	version := zeroSchemaVersion
	for _, a := range applied {
		if a.Version > version {
			version = a.Version
		}
	}
	return version, nil
}

// maxVersion is zeroSchemaVersion when migrations is empty.
func maxVersion(migrations []Migration) string {
	head := zeroSchemaVersion
	for _, m := range migrations {
		if m.Version > head {
			head = m.Version
		}
	}
	return head
}

func sortedMigrations(migrations []Migration) []Migration {
	sorted := make([]Migration, len(migrations))
	copy(sorted, migrations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Version < sorted[j].Version })
	return sorted
}

// Apply runs every pending migration and its bound data transform, in
// version order, in a single transaction (doc 01 §4). It:
//
//  1. Refuses to start if a migration recorded as applied no longer
//     matches what this binary embeds — either its checksum differs (an
//     "immutable" migration file edited after being applied) or this
//     binary no longer embeds it at all (also how a database newer than
//     this binary is refused: sqlitemigrate.PendingMigrations reports the
//     newer migration it doesn't recognize the same way, Q60).
//  2. Writes a pre-migration snapshot (VACUUM INTO) before the first
//     pending migration, and prunes to the last three upgrades' snapshots
//     only after the migration below actually commits.
//  3. Suspends foreign-key enforcement before BEGIN (PRAGMA foreign_keys is
//     a documented no-op once a transaction is open — this must happen on
//     the same underlying connection the transaction later uses, which is
//     why this method pins one *sql.Conn for its entire duration instead of
//     issuing calls through Runner.DB directly), applies every pending
//     migration and transform in one transaction, runs
//     PRAGMA foreign_key_check and PRAGMA integrity_check before COMMIT,
//     and restores foreign-key enforcement afterwards.
//  4. Rolls back and returns an error, leaving the database exactly as it
//     was, on any failure — a later migration in the batch failing, a
//     transform failing, or either PRAGMA check failing.
//
// There are no down migrations (doc 01 §4): reverting means restoring the
// snapshot this step took.
func (r *Runner) Apply(ctx context.Context) (applied []Migration, snapshotPath string, err error) {
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("acquiring a dedicated connection: %w", err)
	}
	defer closeQuietly(conn)

	if err := ensureBookkeeping(ctx, conn); err != nil {
		return nil, "", err
	}

	appliedRows, err := appliedWith(ctx, conn)
	if err != nil {
		return nil, "", err
	}

	sorted := sortedMigrations(r.Migrations)
	pending, err := sqlitemigrate.PendingMigrations(sorted, appliedRows)
	if err != nil {
		return nil, "", fmt.Errorf("checking pending migrations: %w", err)
	}
	if len(pending) == 0 {
		return nil, "", nil
	}

	dbVersion := zeroSchemaVersion
	for _, a := range appliedRows {
		if a.Version > dbVersion {
			dbVersion = a.Version
		}
	}

	snapshotPath, err = Snapshot(ctx, conn, r.SnapshotDir, dbVersion)
	if err != nil {
		return nil, "", fmt.Errorf("pre-migration snapshot: %w", err)
	}

	transformsByVersion := make(map[string]transforms.Transform, len(r.Transforms))
	for _, t := range r.Transforms {
		transformsByVersion[t.Version] = t
	}

	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return nil, snapshotPath, fmt.Errorf("suspending foreign keys: %w", err)
	}
	// Always try to restore enforcement, even on the error paths below —
	// a failed migration must not leave the connection with foreign keys
	// permanently suspended for whoever uses it next. The restore itself
	// uses a context that survives ctx's own cancellation: if ctx is
	// already cancelled by the time this runs, a
	// context-bound restore would fail silently and hand the pooled
	// connection back with enforcement still off. If the restore still
	// fails for some other reason, the connection is discarded instead of
	// returned to the pool with foreign keys off.
	defer func() {
		restoreCtx := context.WithoutCancel(ctx)
		if _, err := conn.ExecContext(restoreCtx, "PRAGMA foreign_keys = ON"); err != nil {
			_ = conn.Raw(func(driverConn any) error { return driver.ErrBadConn })
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, snapshotPath, fmt.Errorf("beginning migration transaction: %w", err)
	}

	for _, m := range pending {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			_ = tx.Rollback()
			return nil, snapshotPath, fmt.Errorf("applying migration %q: %w", m.Filename, err)
		}
		if t, ok := transformsByVersion[m.Version]; ok {
			if err := t.Fn(ctx, tx); err != nil {
				_ = tx.Rollback()
				return nil, snapshotPath, fmt.Errorf("data transform for migration %q: %w", m.Filename, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO %s (version, slug, checksum, applied_at) VALUES (?, ?, ?, ?)", bookkeepingTable),
			m.Version, m.Slug, m.Checksum, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			_ = tx.Rollback()
			return nil, snapshotPath, fmt.Errorf("recording migration %q as applied: %w", m.Filename, err)
		}
	}

	if violation, err := foreignKeyViolation(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, snapshotPath, fmt.Errorf("running foreign_key_check: %w", err)
	} else if violation != "" {
		_ = tx.Rollback()
		return nil, snapshotPath, fmt.Errorf("foreign_key_check failed after applying %d migration(s): %s — rolled back", len(pending), violation)
	}

	if problem, err := integrityProblem(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, snapshotPath, fmt.Errorf("running integrity_check: %w", err)
	} else if problem != "" {
		_ = tx.Rollback()
		return nil, snapshotPath, fmt.Errorf("integrity_check failed after applying %d migration(s): %s — rolled back", len(pending), problem)
	}

	if err := tx.Commit(); err != nil {
		return nil, snapshotPath, fmt.Errorf("committing migration transaction: %w", err)
	}

	// Pruning only ever runs once the migration this snapshot precedes is
	// known to have committed — never before, and never on a failed
	// attempt's snapshot, which stays on disk exactly as a pre-migration
	// snapshot should until a later, successful attempt's own
	// PruneSnapshots call supersedes it.
	if err := PruneSnapshots(r.SnapshotDir, KeepSnapshots, snapshotPath); err != nil {
		return pending, snapshotPath, fmt.Errorf("migrations committed, but pruning old snapshots failed: %w", err)
	}
	return pending, snapshotPath, nil
}

// foreignKeyViolation runs PRAGMA foreign_key_check inside tx — SQLite's
// documented procedure for verifying a rebuild's foreign keys before
// commit — and returns a description of the first violation found, or ""
// if there is none.
func foreignKeyViolation(ctx context.Context, tx *sql.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return "", err
	}
	defer closeQuietly(rows)
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var refTable string
		var fkid int
		if err := rows.Scan(&table, &rowid, &refTable, &fkid); err != nil {
			return "", err
		}
		return fmt.Sprintf("table %q row %v violates its foreign key to %q", table, rowid, refTable), nil
	}
	return "", rows.Err()
}

// integrityProblem runs PRAGMA integrity_check inside tx and returns the
// first reported problem, or "" when it reports "ok".
func integrityProblem(ctx context.Context, tx *sql.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return "", err
	}
	defer closeQuietly(rows)
	if rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return "", err
		}
		if result != "ok" {
			return result, nil
		}
	}
	return "", rows.Err()
}
