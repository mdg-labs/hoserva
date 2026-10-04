package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newMigrationSessionTestStore(t *testing.T) *MigrationSessionStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "migration.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewMigrationSessionStore(db)
}

func TestMigrationSessionStore_PutGetDelete(t *testing.T) {
	ctx := context.Background()
	st := newMigrationSessionTestStore(t)
	if _, found, err := st.Get(ctx); err != nil || found {
		t.Fatalf("Get on an empty table = %v, %v, want none", found, err)
	}
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	want := MigrationSession{
		SourceFile: "upload-a.zip", SourceSize: 1234, SourceReceivedAt: at, Report: []byte(`{"verdict":"go"}`),
		ScanFile: "upload-b.zip", ScanSize: 99, ScanReceivedAt: at.Add(time.Hour), ScanUnverifiedLayout: true, ScanFullChecksums: true, ScanError: "udev gone",
		Verify: []byte(`{"status":"failed"}`),
	}
	if err := st.Put(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.Get(ctx)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, %v, %v; want %+v", got, found, err, want)
	}

	// A second Put replaces the whole row, clearing what the new value leaves empty.
	if err := st.Put(ctx, MigrationSession{SourceFile: "upload-b.zip"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.Get(ctx)
	if got.SourceFile != "upload-b.zip" || got.Report != nil || got.ScanFile != "" || got.ScanUnverifiedLayout || got.ScanFullChecksums || got.Verify != nil || !got.SourceReceivedAt.IsZero() {
		t.Fatalf("Get after a replacing Put = %+v", got)
	}

	if err := st.Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := st.Get(ctx); found {
		t.Fatal("the session survived Delete")
	}
	if err := st.Delete(ctx); err != nil {
		t.Fatalf("Delete of nothing = %v", err)
	}
}

// The migration_session migration only adds a table: upgrading every released
// fixture database keeps every row of every table it already had (D16).
func TestFixtures_MigrationSessionMigrationKeepsEveryRow(t *testing.T) {
	ctx := context.Background()
	fixtures, err := filepath.Glob("../../testdata/db/*.db")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures under testdata/db (err %v)", err)
	}
	migrations, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			ro, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro", fixture))
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(ro)
			before := tableRowCounts(t, ctx, ro)

			work := filepath.Join(t.TempDir(), "upgrade.db")
			copyFile(t, fixture, work)
			db, err := sql.Open("sqlite", work)
			if err != nil {
				t.Fatal(err)
			}
			defer closeQuietly(db)
			if _, _, err := (&Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
				t.Fatalf("upgrading %s: %v", fixture, err)
			}
			after := tableRowCounts(t, ctx, db)
			for table, n := range before {
				if after[table] != n {
					t.Errorf("table %s had %d rows before the upgrade and %d after", table, n, after[table])
				}
			}
			if n, ok := after["migration_session"]; !ok || n != 0 {
				t.Errorf("migration_session after the upgrade: present=%v rows=%d, want present and empty", ok, n)
			}
			if err := NewMigrationSessionStore(db).Put(ctx, MigrationSession{SourceFile: "upload-x.zip"}); err != nil {
				t.Fatalf("saving a session on the upgraded database: %v", err)
			}
		})
	}
}

// Adding scan_full_checksums keeps a session row the table already held, and the
// scan it recorded reads as one that hashes a sample, not every file (D16).
func TestMigrationSessionMigration_ScanFullChecksumsKeepsAnExistingRow(t *testing.T) {
	ctx := context.Background()
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(db)
	if _, err := db.ExecContext(ctx, read("20261002202210_add_migration_session.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO migration_session (id, source_file, report, scan_file, scan_unverified_layout) VALUES (1, 'upload-a.zip', '{"verdict":"go"}', 'upload-b.zip', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, read("20261003114757_add_migration_scan_full_checksums.sql")); err != nil {
		t.Fatal(err)
	}
	// The store reads every column of the current schema.
	if _, err := db.ExecContext(ctx, read("20261004084035_add_migration_session_verify.sql")); err != nil {
		t.Fatal(err)
	}
	got, found, err := NewMigrationSessionStore(db).Get(ctx)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if got.SourceFile != "upload-a.zip" || string(got.Report) != `{"verdict":"go"}` || got.ScanFile != "upload-b.zip" || !got.ScanUnverifiedLayout || got.ScanFullChecksums {
		t.Errorf("the row after the migration = %+v", got)
	}
}

// Adding verify keeps a session row the table already held, and it reads as one
// no verify has run against (D16).
func TestMigrationSessionMigration_VerifyKeepsAnExistingRow(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(db)
	for _, name := range []string{"20261002202210_add_migration_session.sql", "20261003114757_add_migration_scan_full_checksums.sql"} {
		raw, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO migration_session (id, source_file, report, scan_full_checksums) VALUES (1, 'upload-a.zip', '{"verdict":"go"}', 1)`); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("migrations", "20261004084035_add_migration_session_verify.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(raw)); err != nil {
		t.Fatal(err)
	}
	got, found, err := NewMigrationSessionStore(db).Get(ctx)
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if got.SourceFile != "upload-a.zip" || string(got.Report) != `{"verdict":"go"}` || !got.ScanFullChecksums || got.Verify != nil {
		t.Errorf("the row after the migration = %+v", got)
	}
}
