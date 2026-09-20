package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func migratedHostDB(t *testing.T) *HostConfigStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "host-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewHostConfigStore(db)
}

func TestHostConfigStore_PutGetAndUpsert(t *testing.T) {
	ctx := context.Background()
	st := migratedHostDB(t)

	if _, err := st.Get(ctx, "samba"); err != sql.ErrNoRows {
		t.Fatalf("Get on empty store = %v, want sql.ErrNoRows", err)
	}

	rec := HostConfig{
		Kind:      "samba",
		Decision:  "leave",
		Facts:     `{"path":"samba/smb.conf","shares":["media"]}`,
		AppliedAt: time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC),
	}
	if err := st.Put(ctx, rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get(ctx, "samba")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != rec.Kind || got.Decision != rec.Decision || got.Facts != rec.Facts {
		t.Fatalf("got %+v, want %+v", got, rec)
	}

	rec.Decision = "import"
	rec.Facts = `{"path":"samba/smb.conf","shares":["media","homes"]}`
	if err := st.Put(ctx, rec); err != nil {
		t.Fatalf("upsert Put: %v", err)
	}
	got, err = st.Get(ctx, "samba")
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != "import" {
		t.Fatalf("decision after upsert = %q, want import", got.Decision)
	}

	listed, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("List = %d rows, want 1", len(listed))
	}
}
