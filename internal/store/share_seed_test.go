package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func seedShare(name string, grants ...SeedGrant) SeedShare {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	return SeedShare{
		Share: Share{
			Name: name, CacheMode: "array-only", CreatePolicy: "ff", SMBEnabled: true, SMBBrowseable: true,
			CreatedAt: now, UpdatedAt: now, MinFreeSpace: "1000K", TargetCacheMode: "cache-only",
			MigrationNotes: []string{"a note"},
		},
		Grants: grants,
	}
}

func countRows(t *testing.T, st *ShareStore, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestShareStore_SeedMigrationCreatesSharesUsersAndGrantsTogether(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	users := []SeedUser{{ID: "u-alice", Username: "alice", PasswordHash: "x", CreatedAt: now}, {ID: "u-bob", Username: "bob", PasswordHash: "x", CreatedAt: now}}
	seeded, err := st.SeedMigration(ctx, users, []SeedShare{
		seedShare("media", SeedGrant{"alice", "read-write"}, SeedGrant{"bob", "read-only"}),
		seedShare("docs"),
	})
	if err != nil {
		t.Fatalf("SeedMigration: %v", err)
	}
	if !reflect.DeepEqual(seeded.Shares, []string{"media", "docs"}) || len(seeded.Users) != 2 {
		t.Fatalf("seeded = %+v", seeded)
	}
	got, err := st.Get(ctx, "media")
	if err != nil || got.MinFreeSpace != "1000K" || got.TargetCacheMode != "cache-only" || !reflect.DeepEqual(got.MigrationNotes, []string{"a note"}) {
		t.Fatalf("Get = %+v, %v: the migration state was not stored", got, err)
	}
	var role, hash string
	if err := st.db.QueryRow(`SELECT role, password_hash FROM users WHERE username = 'alice'`).Scan(&role, &hash); err != nil || role != "share-only" {
		t.Fatalf("alice = %q, %v, want share-only", role, err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions WHERE share_name = 'media'`); n != 2 {
		t.Errorf("grants of media = %d, want 2", n)
	}
	var access string
	if err := st.db.QueryRow(`SELECT access FROM share_user_permissions WHERE share_name = 'media' AND user_id = 'u-bob'`).Scan(&access); err != nil || access != "read-only" {
		t.Errorf("bob's access = %q, %v", access, err)
	}
}

// A grant that names nobody fails the whole seed, and the user and the share
// created before it are gone with it.
func TestShareStore_SeedMigrationIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	users := []SeedUser{{ID: "u-alice", Username: "alice", PasswordHash: "x", CreatedAt: now}}
	_, err := st.SeedMigration(ctx, users, []SeedShare{seedShare("media", SeedGrant{"alice", "read-write"}), seedShare("docs", SeedGrant{"nobody", "read-only"})})
	if err == nil {
		t.Fatal("SeedMigration with a grant for an unknown user succeeded")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM shares`) + countRows(t, st, `SELECT COUNT(*) FROM users`) + countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions`); n != 0 {
		t.Errorf("%d rows were left by a failed seed", n)
	}
}

// What already exists is left as it is, and only the share this call created
// gets grants and is returned for the undo.
func TestShareStore_SeedMigrationLeavesExistingRowsAlone(t *testing.T) {
	ctx := context.Background()
	st := migratedShareDB(t)
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if _, err := st.db.Exec(`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('u-old', 'alice', 'h', 'viewer', 0, ?)`, now.Format(TimeFormat)); err != nil {
		t.Fatal(err)
	}
	existing := seedShare("media")
	existing.Share.CacheMode, existing.Share.MinFreeSpace = "cache-then-move", ""
	if err := st.Insert(ctx, existing.Share); err != nil {
		t.Fatal(err)
	}
	seeded, err := st.SeedMigration(ctx, []SeedUser{{ID: "u-new", Username: "alice", PasswordHash: "x", CreatedAt: now}},
		[]SeedShare{seedShare("media", SeedGrant{"alice", "read-write"}), seedShare("docs", SeedGrant{"alice", "read-only"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(seeded.Users) != 0 || !reflect.DeepEqual(seeded.ExistingUsers, []string{"alice"}) || !reflect.DeepEqual(seeded.Shares, []string{"docs"}) || !reflect.DeepEqual(seeded.ExistingShares, []string{"media"}) {
		t.Fatalf("seeded = %+v", seeded)
	}
	media, _ := st.Get(ctx, "media")
	if media.CacheMode != "cache-then-move" || media.MinFreeSpace != "" {
		t.Errorf("the existing share was changed: %+v", media)
	}
	var role string
	if err := st.db.QueryRow(`SELECT role FROM users WHERE username = 'alice'`).Scan(&role); err != nil || role != "viewer" {
		t.Errorf("the existing user's role = %q, %v, want viewer", role, err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions WHERE user_id = 'u-old'`); n != 1 {
		t.Errorf("grants for the existing user = %d, want 1 (docs only)", n)
	}

	if err := st.UnseedMigration(ctx, seeded); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "docs"); err == nil {
		t.Error("docs survived the undo")
	}
	if _, err := st.Get(ctx, "media"); err != nil {
		t.Errorf("the undo removed a share it did not create: %v", err)
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM users WHERE username = 'alice'`); n != 1 {
		t.Errorf("the undo removed a user it did not create")
	}
	if n := countRows(t, st, `SELECT COUNT(*) FROM share_user_permissions`); n != 0 {
		t.Errorf("grants left after the undo: %d", n)
	}
}
