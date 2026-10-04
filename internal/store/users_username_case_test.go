package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const usernameCaseMigrationSlug = "add_users_username_lower_unique"

// usersDBBeforeCaseIndex returns a database migrated to the version before the
// case-insensitive username index, and every migration, in order.
func usersDBBeforeCaseIndex(t *testing.T) (*sql.DB, []Migration, *Runner) {
	t.Helper()
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var before []Migration
	found := false
	for _, m := range all {
		if m.Slug == usernameCaseMigrationSlug {
			found = true
			continue
		}
		before = append(before, m)
	}
	if !found {
		t.Fatalf("no migration with the slug %q", usernameCaseMigrationSlug)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "users.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&Runner{DB: db, Migrations: before, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatalf("applying the migrations before the case index: %v", err)
	}
	return db, all, &Runner{DB: db, Migrations: all, SnapshotDir: t.TempDir()}
}

func insertUser(t *testing.T, db *sql.DB, id, username string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES (?, ?, 'x', 'share-only', 0, '2026-10-04T00:00:00Z')`, id, username); err != nil {
		t.Fatalf("inserting user %s: %v", username, err)
	}
}

func userNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT username FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer closeQuietly(rows)
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// Usernames are unique without regard to case from the migration on, and the
// rows a database already held survive it.
func TestUsernameCaseMigration_KeepsEveryRowAndRefusesACaseVariant(t *testing.T) {
	db, _, runner := usersDBBeforeCaseIndex(t)
	insertUser(t, db, "u1", "alice")
	insertUser(t, db, "u2", "bob")
	insertUser(t, db, "u3", "Carol")

	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("upgrading a database with no case-only duplicates: %v", err)
	}
	if got := strings.Join(userNames(t, db), ","); got != "alice,bob,Carol" {
		t.Errorf("users after the upgrade = %s, want every row kept as it was", got)
	}
	for _, dup := range []string{"Alice", "ALICE", "carol"} {
		if _, err := db.Exec(`INSERT INTO users (id, username, password_hash, role, totp_last_step, created_at) VALUES ('dup', ?, 'x', 'share-only', 0, 'now')`, dup); err == nil {
			t.Errorf("a user named %q was accepted beside an account that differs only in case", dup)
		}
	}
}

// A database that already holds two names differing only in case is refused with
// a message naming them, before anything is changed: no row is lost, the old
// schema stays, and the daemon can be started again once one is renamed (D16).
func TestUsernameCaseMigration_RefusesACaseOnlyDuplicateAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	db, _, runner := usersDBBeforeCaseIndex(t)
	insertUser(t, db, "u1", "alice")
	insertUser(t, db, "u2", "Alice")
	insertUser(t, db, "u3", "bob")
	versionBefore, err := runner.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = runner.Apply(ctx)
	if err == nil {
		t.Fatal("the upgrade succeeded over two usernames that differ only in case")
	}
	for _, want := range []string{`"alice"`, `"Alice"`, "u1", "u2", "case"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not mention %s", err, want)
		}
	}
	if strings.Contains(err.Error(), "UNIQUE constraint") {
		t.Errorf("the refusal is the raw constraint failure, not a message about the names: %v", err)
	}
	if got := strings.Join(userNames(t, db), ","); got != "alice,Alice,bob" {
		t.Errorf("users after the refused upgrade = %s, want all three rows untouched", got)
	}
	if v, err := runner.CurrentVersion(ctx); err != nil || v != versionBefore {
		t.Errorf("version after the refused upgrade = %s (%v), want %s", v, err, versionBefore)
	}
	var idx int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'users_username_lower_idx'`).Scan(&idx); err != nil || idx != 0 {
		t.Errorf("the index exists after the refused upgrade (%d, %v)", idx, err)
	}

	if _, err := db.Exec(`UPDATE users SET username = 'alice-2' WHERE id = 'u2'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("upgrading again once the duplicate was renamed: %v", err)
	}
}

// Every released fixture database upgrades to head with every row of every table
// it held (D16).
func TestFixtures_UsernameCaseMigrationKeepsEveryRow(t *testing.T) {
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
			var idx int
			if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'users_username_lower_idx'`).Scan(&idx); err != nil || idx != 1 {
				t.Errorf("the case-insensitive username index is missing after upgrading %s (%d, %v)", fixture, idx, err)
			}
		})
	}
}
