package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func migratedShareDB(t *testing.T) *ShareStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "share-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewShareStore(db)
}

func TestShareStore_InsertGetListUpdateDelete(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	rec := Share{
		Name:                  "media",
		CacheMode:             "cache-then-move",
		CreatePolicy:          "mspmfs",
		SMBEnabled:            true,
		SMBBrowseable:         true,
		SMBTimeMachine:        true,
		SMBTimeMachineMaxSize: "500G",
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	if err := st.Insert(ctx, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := st.Insert(ctx, rec); !errors.Is(err, ErrShareExists) {
		t.Fatalf("second Insert = %v, want ErrShareExists", err)
	}

	got, err := st.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "media" || got.CacheMode != "cache-then-move" || got.SMBTimeMachineMaxSize != "500G" || !got.SMBEnabled {
		t.Fatalf("Get = %+v", got)
	}
	if got.NFSEnabled || got.NFSSquash != "root_squash" || len(got.NFSHosts) != 0 {
		t.Fatalf("Get NFS defaults = %+v", got)
	}

	listed, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "media" {
		t.Fatalf("List = %+v", listed)
	}

	got.SMBGuest = true
	got.NFSEnabled = true
	got.NFSHosts = []string{"192.168.1.0/24", "client.home.arpa"}
	got.NFSSquash = "all_squash"
	got.UpdatedAt = now.Add(time.Hour)
	if err := st.Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err = st.Get(ctx, "media")
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if !got.SMBGuest {
		t.Fatal("Update did not persist smb_guest")
	}
	if !got.NFSEnabled || got.NFSSquash != "all_squash" || len(got.NFSHosts) != 2 || got.NFSHosts[0] != "192.168.1.0/24" {
		t.Fatalf("Update did not persist NFS: %+v", got)
	}

	if err := st.Delete(ctx, "media"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, "media"); !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrShareNotFound", err)
	}
	if err := st.Delete(ctx, "media"); !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("second Delete = %v, want ErrShareNotFound", err)
	}
}

func TestShareStore_GetMissing(t *testing.T) {
	_, err := migratedShareDB(t).Get(context.Background(), "nope")
	if !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("Get missing = %v, want ErrShareNotFound", err)
	}
}
