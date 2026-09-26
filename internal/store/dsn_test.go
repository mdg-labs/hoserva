package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestDSN_ReadThenWriteTxSurvivesAConcurrentWriteBetweenThem is #387
// finding 1's own regression, reproduced deterministically at the DSN
// level: SetRemovalState's own transaction (internal/store/array.go)
// reads the current removing disk, then writes the new state — exactly
// the window job.Scheduler.EnterMaintenance's persisted array_maintenance
// write (persistMaintenanceLocked) landed in when a user ran `array stop`
// while an evacuation was mid-step, turning a resumable job's own
// checkpoint into an outright "database is locked" failure instead of a
// resumable interrupted job.
//
// This opens the exact same window by hand: a goroutine begins a
// transaction, reads array_disks, signals it has read, and waits; the
// main goroutine then makes a second, single-statement, autocommit write
// to array_maintenance — the same shape persistMaintenanceLocked's own
// UpsertArrayMaintenance call makes — before letting the first
// transaction write and commit. Without store.DSN's _txlock=immediate,
// the first transaction's BEGIN takes no lock until its first write; by
// then the second write has already advanced the database past the
// snapshot the first transaction's own read was taken from, and SQLite
// refuses the upgrade with an immediate SQLITE_BUSY that busy_timeout
// never gets a chance to retry. With _txlock=immediate, the first
// transaction's BEGIN already holds the write lock before its own read
// runs, so the second write instead waits behind it — the ordinary,
// retried lock wait busy_timeout already covers.
func TestDSN_ReadThenWriteTxSurvivesAConcurrentWriteBetweenThem(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "txlock-test.db")
	db, err := sql.Open("sqlite", DSN(dbPath))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	st := NewArrayStore(db)
	twoDataDiskArray(t, st)

	readStarted := make(chan struct{})
	proceed := make(chan struct{})
	txDone := make(chan error, 1)
	writeDone := make(chan error, 1)

	go func() {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			txDone <- err
			return
		}
		defer func() { _ = tx.Rollback() }()

		var mountpoint string
		if err := tx.QueryRowContext(ctx, "SELECT mountpoint FROM array_disks WHERE role = 'data' AND role_index = 1").Scan(&mountpoint); err != nil {
			txDone <- err
			return
		}
		close(readStarted)
		<-proceed

		if _, err := tx.ExecContext(ctx, "UPDATE array_disks SET removal_state = 'evacuating', removal_job_id = 'job-1' WHERE mountpoint = ?", mountpoint); err != nil {
			txDone <- err
			return
		}
		txDone <- tx.Commit()
	}()

	<-readStarted
	// The concurrent write runs on its own goroutine, never waited for
	// before releasing the read-then-write transaction below: with
	// store.DSN's _txlock=immediate the first transaction's own BEGIN
	// already holds the write lock, so this write blocks behind it (and
	// only succeeds once proceed lets that transaction commit) —
	// deadlocking the test on a single busy_timeout wait if it were
	// called synchronously here instead.
	go func() {
		_, err := db.ExecContext(ctx,
			`INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, 1, 0, ?)
			 ON CONFLICT (id) DO UPDATE SET maintenance = excluded.maintenance, array_stopped = excluded.array_stopped, updated_at = excluded.updated_at`,
			time.Now().UTC().Format(TimeFormat),
		)
		writeDone <- err
	}()
	// Gives the goroutine above a chance to actually reach SQLite's own
	// lock wait before the read-then-write transaction below is let
	// through — without store.DSN's _txlock=immediate this write commits
	// immediately (deferred mode takes no lock until here), which is
	// exactly the interleaving that makes the first transaction's own
	// later write see a stale snapshot.
	time.Sleep(50 * time.Millisecond)
	close(proceed)

	if err := <-txDone; err != nil {
		if strings.Contains(err.Error(), "locked") || strings.Contains(err.Error(), "busy") {
			t.Fatalf("read-then-write transaction's own commit after a concurrent write landed between its read and write = %v, want nil (store.DSN's _txlock=immediate, #387 finding 1)", err)
		}
		t.Fatalf("read-then-write transaction: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("concurrent array_maintenance write: %v", err)
	}
}
