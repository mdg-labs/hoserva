package store

import (
	"context"
	"database/sql"
	"fmt"
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
		ScanFile: "upload-b.zip", ScanSize: 99, ScanReceivedAt: at.Add(time.Hour), ScanUnverifiedLayout: true, ScanError: "udev gone",
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
	if got.SourceFile != "upload-b.zip" || got.Report != nil || got.ScanFile != "" || got.ScanUnverifiedLayout || !got.SourceReceivedAt.IsZero() {
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
