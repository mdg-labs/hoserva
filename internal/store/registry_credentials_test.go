package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func newRegistryCredentialTestStore(t *testing.T) (*RegistryCredentialStore, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "registry.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewRegistryCredentialStore(db), db
}

func TestRegistryCredentialStore_PutGetListDelete(t *testing.T) {
	ctx := context.Background()
	st, _ := newRegistryCredentialTestStore(t)
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)

	if _, found, err := st.Get(ctx, "ghcr.io"); err != nil || found {
		t.Fatalf("Get on an empty table = %v, %v, want none", found, err)
	}
	if err := st.Put(ctx, "quay.io", []byte("sealed-quay"), at); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "ghcr.io", []byte("sealed-1"), at); err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, "ghcr.io", []byte("sealed-2"), at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.Get(ctx, "ghcr.io")
	if err != nil || !found || !bytes.Equal(got.Sealed, []byte("sealed-2")) || !got.UpdatedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("Get = %+v, %v, %v, want the replaced value", got, found, err)
	}
	list, err := st.List(ctx)
	if err != nil || len(list) != 2 || list[0].Registry != "ghcr.io" || list[1].Registry != "quay.io" {
		t.Fatalf("List = %+v, %v, want one row per registry, sorted", list, err)
	}
	if deleted, err := st.Delete(ctx, "ghcr.io"); err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	if deleted, err := st.Delete(ctx, "ghcr.io"); err != nil || deleted {
		t.Fatalf("a second Delete = %v, %v, want it reported as not found", deleted, err)
	}
	if list, _ := st.List(ctx); len(list) != 1 || list[0].Registry != "quay.io" {
		t.Fatalf("List after the delete = %+v", list)
	}
}

// The registry credentials migration only adds a table: upgrading every
// released fixture database keeps every row of every table it already had,
// and leaves the new table empty, with a credential written afterwards
// readable.
func TestFixtures_RegistryCredentialsMigrationKeepsEveryRow(t *testing.T) {
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
			r := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
			if _, _, err := r.Apply(ctx); err != nil {
				t.Fatalf("upgrading %s: %v", fixture, err)
			}
			after := tableRowCounts(t, ctx, db)
			for table, n := range before {
				if after[table] != n {
					t.Errorf("table %s had %d rows before the upgrade and %d after", table, n, after[table])
				}
			}
			if n, ok := after["registry_credentials"]; !ok || n != 0 {
				t.Errorf("registry_credentials after the upgrade: present=%v rows=%d, want present and empty", ok, n)
			}
			st := NewRegistryCredentialStore(db)
			if err := st.Put(ctx, "ghcr.io", []byte("sealed"), time.Now()); err != nil {
				t.Fatalf("saving a credential on the upgraded database: %v", err)
			}
		})
	}
}
