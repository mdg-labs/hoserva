package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
)

// NoteArrayStateKept is the code of the note every preview carries: an
// in-place import keeps the running array's maintenance and stopped state
// instead of restoring the archive's.
const NoteArrayStateKept = "array_state_kept"

// KeepArrayState makes the archive database at stagedPath carry the live
// database's array_maintenance row exactly, or no row when the live database
// has none, so that RestoreDatabase of stagedPath leaves the array's state
// where it is (doc 10 §1, doc 02 §4). The row records the machine's physical
// state (`array stop` for a disk swap), not configuration to roll back;
// restoring an archive's row, or its absence, would return a stopped array to
// normal operation.
//
// The live row travels inside the restore's single write transaction, so
// there is no moment at which the live database holds the archive's array
// state. A failure here changes nothing live: stagedPath is the caller's own
// staged copy, which it discards. The caller must hold the scheduler's
// database-restore hold and keep the array state from changing until the
// restore returns (job.Scheduler.WithArrayStateHeld).
func KeepArrayState(ctx context.Context, live *sql.DB, stagedPath string) error {
	var maintenance, stopped int64
	var updatedAt string
	found := true
	err := live.QueryRowContext(ctx, `SELECT maintenance, array_stopped, updated_at FROM array_maintenance WHERE id = 1`).
		Scan(&maintenance, &stopped, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		found = false
	} else if err != nil {
		return fmt.Errorf("reading the live array state: %w", err)
	}

	staged, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: stagedPath, RawQuery: "mode=rw"}).String())
	if err != nil {
		return fmt.Errorf("opening the staged archive database: %w", err)
	}
	tx, err := staged.BeginTx(ctx, nil)
	if err != nil {
		_ = staged.Close()
		return fmt.Errorf("beginning the staged archive update: %w", err)
	}
	err = func() error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM array_maintenance`); err != nil {
			return err
		}
		if !found {
			return nil
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO array_maintenance (id, maintenance, array_stopped, updated_at) VALUES (1, ?, ?, ?)`,
			maintenance, stopped, updatedAt)
		return err
	}()
	if err != nil {
		_ = tx.Rollback()
		_ = staged.Close()
		return fmt.Errorf("writing the live array state into the staged archive database: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = staged.Close()
		return fmt.Errorf("writing the live array state into the staged archive database: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("closing the staged archive database: %w", err)
	}
	return nil
}
