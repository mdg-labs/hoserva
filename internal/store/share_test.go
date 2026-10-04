package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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

// withoutForeignKeys pins the store to one connection with enforcement off,
// the state runtime connections are documented to run in, so an ON DELETE
// CASCADE cannot mask a delete that leaves child rows behind.
func withoutForeignKeys(t *testing.T, st *ShareStore) {
	t.Helper()
	st.db.SetMaxOpenConns(1)
	if _, err := st.db.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disabling foreign keys: %v", err)
	}
}

func TestShareStore_DeleteRemovesGrantsSoARecreatedShareInheritsNone(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	withoutForeignKeys(t, st)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	rec := Share{Name: "media", CacheMode: "array-only", CreatePolicy: "ff", SMBEnabled: true, CreatedAt: now, UpdatedAt: now}

	for _, stmt := range []string{
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('u-bob', 'bob', 'x', 'viewer', 0, '2026-10-04T12:00:00Z')`,
		`INSERT INTO user_groups (id, name, created_at) VALUES ('g-kids', 'kids', '2026-10-04T12:00:00Z')`,
	} {
		if _, err := st.db.Exec(stmt); err != nil {
			t.Fatalf("seeding %q: %v", stmt, err)
		}
	}
	if err := st.Insert(ctx, rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	other := rec
	other.Name = "backups"
	if err := st.Insert(ctx, other); err != nil {
		t.Fatalf("Insert other: %v", err)
	}
	for _, share := range []string{"media", "backups"} {
		if _, err := st.db.Exec(`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES (?, 'u-bob', 'read-write')`, share); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec(`INSERT INTO share_group_permissions (share_name, group_id, access) VALUES (?, 'g-kids', 'read-only')`, share); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Delete(ctx, "media"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Insert(ctx, rec); err != nil {
		t.Fatalf("re-Insert: %v", err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions WHERE share_name = 'media'`); n != 0 {
		t.Fatalf("recreated share inherited %d user grants, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_group_permissions WHERE share_name = 'media'`); n != 0 {
		t.Fatalf("recreated share inherited %d group grants, want 0", n)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions WHERE share_name = 'backups'`) +
		countRows(t, st, `SELECT COUNT(*) FROM share_group_permissions WHERE share_name = 'backups'`); n != 2 {
		t.Fatalf("another share's grants = %d rows, want 2 left alone", n)
	}
}

func TestShareStore_DeleteMissingLeavesGrantsAlone(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	withoutForeignKeys(t, st)
	if _, err := st.db.Exec(`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('u-bob', 'bob', 'x', 'viewer', 0, '2026-10-04T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES ('ghost', 'u-bob', 'read-only')`); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "ghost"); !errors.Is(err, ErrShareNotFound) {
		t.Fatalf("Delete missing = %v, want ErrShareNotFound", err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions`); n != 1 {
		t.Fatalf("a refused delete changed grants: %d rows", n)
	}
}

func TestShareStore_RestorePutsBackTheShareWithItsGrantsOrNothing(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	withoutForeignKeys(t, st)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	rec := Share{Name: "media", CacheMode: "array-only", CreatePolicy: "ff", SMBEnabled: true, CreatedAt: now, UpdatedAt: now}
	for _, stmt := range []string{
		`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('u-bob', 'bob', 'x', 'viewer', 0, '2026-10-04T12:00:00Z')`,
		`INSERT INTO user_groups (id, name, created_at) VALUES ('g-kids', 'kids', '2026-10-04T12:00:00Z')`,
	} {
		if _, err := st.db.Exec(stmt); err != nil {
			t.Fatalf("seeding %q: %v", stmt, err)
		}
	}
	if err := st.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO share_user_permissions (share_name, user_id, access) VALUES ('media', 'u-bob', 'none')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO share_group_permissions (share_name, group_id, access) VALUES ('media', 'g-kids', 'read-write')`); err != nil {
		t.Fatal(err)
	}

	grants, err := st.Remove(ctx, "media")
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if want := (ShareGrants{Users: []ShareGrant{{ID: "u-bob", Access: "none"}}, Groups: []ShareGrant{{ID: "g-kids", Access: "read-write"}}}); !reflect.DeepEqual(grants, want) {
		t.Fatalf("Remove returned %+v, want %+v", grants, want)
	}

	bad := ShareGrants{Users: []ShareGrant{{ID: "u-bob", Access: "none"}, {ID: "u-bob", Access: "read-only"}}}
	if err := st.Restore(ctx, rec, bad); err == nil {
		t.Fatal("Restore with a duplicate grant succeeded")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM shares`) + countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions`); n != 0 {
		t.Fatalf("a failed Restore left %d rows behind", n)
	}

	if err := st.Restore(ctx, rec, grants); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions WHERE share_name = 'media' AND user_id = 'u-bob' AND access = 'none'`) +
		countRows(t, st, `SELECT COUNT(*) FROM share_group_permissions WHERE share_name = 'media' AND group_id = 'g-kids' AND access = 'read-write'`); n != 2 {
		t.Fatalf("restored grants = %d matching rows, want 2", n)
	}
}
