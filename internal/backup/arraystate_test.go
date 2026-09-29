package backup

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func arrayRow(t *testing.T, db *sql.DB) (row [3]any, found bool) {
	t.Helper()
	var m, s int64
	var at string
	err := db.QueryRow(`SELECT maintenance, array_stopped, updated_at FROM array_maintenance WHERE id = 1`).Scan(&m, &s, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false
	}
	if err != nil {
		t.Fatalf("reading array_maintenance: %v", err)
	}
	return [3]any{m, s, at}, true
}

// Restoring the archive after KeepArrayState leaves the live array_maintenance
// row as it was, whatever the archive held: another state, or none.
func TestKeepArrayState_RestoreLeavesTheLiveRowExactly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name         string
		live, arc    string
		wantRow      [3]any
		wantRowFound bool
	}{
		{"live stopped, archive normal operation", `INSERT INTO array_maintenance VALUES (1, 1, 1, 'live-t')`, ``, [3]any{int64(1), int64(1), "live-t"}, true},
		{"live in maintenance, archive stopped", `INSERT INTO array_maintenance VALUES (1, 1, 0, 'live-t')`, `INSERT INTO array_maintenance VALUES (1, 1, 1, 'arc-t')`, [3]any{int64(1), int64(0), "live-t"}, true},
		{"live has no row, archive stopped", ``, `INSERT INTO array_maintenance VALUES (1, 1, 1, 'arc-t')`, [3]any{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
			arcPath := filepath.Join(t.TempDir(), "state.db")
			arc := openMigratedDB(t, arcPath)
			if tc.live != "" {
				mustExec(t, live, tc.live)
			}
			if tc.arc != "" {
				mustExec(t, arc, tc.arc)
			}
			fixtureFor(t, "shares").insert(t, arc, "from-archive", 0)
			if err := arc.Close(); err != nil {
				t.Fatal(err)
			}

			if err := KeepArrayState(ctx, live, arcPath); err != nil {
				t.Fatalf("KeepArrayState: %v", err)
			}
			if err := RestoreDatabase(ctx, live, arcPath); err != nil {
				t.Fatalf("RestoreDatabase: %v", err)
			}

			got, found := arrayRow(t, live)
			if found != tc.wantRowFound || got != tc.wantRow {
				t.Fatalf("array_maintenance after the restore = %v (found %v), want %v (found %v)", got, found, tc.wantRow, tc.wantRowFound)
			}
			var n int
			if err := live.QueryRow(`SELECT COUNT(*) FROM shares WHERE name = 'from-archive'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("the rest of the archive was not restored: shares named from-archive = %d, err %v", n, err)
			}
		})
	}
}

// A staged copy that cannot be written is an error, and the live database,
// which KeepArrayState only reads, keeps its row.
func TestKeepArrayState_UnwritableStagedCopyIsAnError(t *testing.T) {
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	mustExec(t, live, `INSERT INTO array_maintenance VALUES (1, 1, 1, 'live-t')`)

	err := KeepArrayState(context.Background(), live, filepath.Join(t.TempDir(), "missing.db"))
	if err == nil {
		t.Fatal("KeepArrayState succeeded on a staged database that does not exist")
	}
	if got, found := arrayRow(t, live); !found || got != [3]any{int64(1), int64(1), "live-t"} {
		t.Fatalf("live array_maintenance = %v (found %v), want it unchanged", got, found)
	}
}
