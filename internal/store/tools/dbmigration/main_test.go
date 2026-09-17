package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

func setupStoreDir(t *testing.T, schemaSQL string, migrations []store.Migration) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema", "schema.sql"), []byte(schemaSQL), 0o644); err != nil {
		t.Fatal(err)
	}
	sums := make(map[string]string, len(migrations))
	for _, m := range migrations {
		if err := os.WriteFile(filepath.Join(dir, "migrations", m.Filename), []byte(m.SQL), 0o644); err != nil {
			t.Fatal(err)
		}
		sums[m.Filename] = store.ChecksumOf(m)
	}
	if err := os.WriteFile(filepath.Join(dir, "migrations", store.ChecksumsFile), store.FormatChecksums(sums), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The generator must never silently emit a DROP COLUMN (or any other
// statement store.Destructive flags) — a bare "DROP TABLE" substring
// check, the kind Q60 criticizes sqldef's --enable-drop-table for, would
// never gate DROP COLUMN at all.
func TestRun_RefusesDropColumn(t *testing.T) {
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);"}},
	)

	if err := run("drop_extra", dir); err == nil {
		t.Fatal("expected run to refuse generating a migration whose only change is a DROP COLUMN")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "0001_a.sql" && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}
}

// An existing migration containing a file-path ATTACH must be refused
// before run() ever replays it — currentSchemaDDLs would otherwise execute
// it against a live connection, creating a real file on disk. The target
// path lives inside t.TempDir() and is confirmed absent afterward, so this
// pins the refusal-before-execution ordering directly, not just that run()
// returns some error.
func TestRun_RefusesAttachInExistingMigrationBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "attach-target.db")
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "ATTACH DATABASE '" + target + "' AS x;"},
		},
	)

	if err := run("add_extra", dir); !errors.Is(err, store.ErrDisallowedStatement) {
		t.Fatalf("run on a store dir with an existing ATTACH migration = %v, want store.ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ATTACH target %s exists after run refused the existing migration containing it", target)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "0001_a.sql" && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}
}

// The same, for VACUUM INTO — SQLite's own way to write a whole database
// copy to an arbitrary path in one statement.
func TestRun_RefusesVacuumIntoInExistingMigrationBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "vacuum-target.db")
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "VACUUM INTO '" + target + "';"},
		},
	)

	if err := run("add_extra", dir); !errors.Is(err, store.ErrDisallowedStatement) {
		t.Fatalf("run on a store dir with an existing VACUUM INTO migration = %v, want store.ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("VACUUM INTO target %s exists after run refused the existing migration containing it", target)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "0001_a.sql" && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}
}

// The ordinary, safe path: a schema.sql change that only adds a column
// still generates a migration normally.
func TestRun_GeneratesAddColumnMigration(t *testing.T) {
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)

	if err := run("add_extra", dir); err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "migrations", "0002_add_extra.sql")); err != nil {
		t.Fatalf("expected 0002_add_extra.sql to be written: %v", err)
	}
}

// `make db-migration` must not silently re-approve an edit to an existing
// "immutable" migration: rehashing every file in the directory instead of
// verifying the existing entries first would let editing 0001 and running
// db-migration make db-check pass afterwards as if 0001 had always looked
// that way.
//
// schema.sql here differs from both the original and the edited
// migration, so this actually discriminates whether the checksum check is
// what stops `run`: without VerifyChecksums, sqldef would still find a
// real column to add (`more`) and `run` would succeed on the tampered
// tree; only VerifyChecksums catching the tampered 0001 first makes it
// fail. An "edited" migration whose content already equaled schema.sql
// would not discriminate this, since `run` would then fail with "no
// schema change to generate a migration for" regardless of whether the
// checksum check ran at all.
func TestRun_RefusesWhenAnExistingMigrationWasEdited(t *testing.T) {
	original := store.Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}
	dir := setupStoreDir(t, "CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT, more TEXT);", []store.Migration{original})

	originalChecksums, err := os.ReadFile(filepath.Join(dir, "migrations", store.ChecksumsFile))
	if err != nil {
		t.Fatal(err)
	}

	// Edit the "immutable" migration on disk after it was checksummed —
	// exactly the tampering VerifyChecksums exists to catch. Deliberately
	// not equal to schema.sql (see the comment above).
	edited := "CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);"
	if err := os.WriteFile(filepath.Join(dir, "migrations", original.Filename), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := run("add_extra", dir); err == nil {
		t.Fatal("expected run to refuse generating a migration on top of an edited existing migration")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != original.Filename && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}

	afterChecksums, err := os.ReadFile(filepath.Join(dir, "migrations", store.ChecksumsFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterChecksums) != string(originalChecksums) {
		t.Fatalf("checksums file changed despite refusing to generate a migration:\nbefore:\n%s\nafter:\n%s", originalChecksums, afterChecksums)
	}
}

// AppendChecksum itself must never recompute an existing entry, only add a
// new one. original's own .sql file is written to disk here so that a
// regression which rehashed every migration file already present in dir
// (instead of trusting the caller's map) would find something to
// (wrongly) rehash for original.Filename, rather than finding nothing and
// passing anyway.
func TestAppendChecksum_NeverRecomputesExistingEntries(t *testing.T) {
	dir := t.TempDir()
	original := store.Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}
	if err := os.WriteFile(filepath.Join(dir, original.Filename), []byte(original.SQL), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendChecksum(dir, original); err != nil {
		t.Fatal(err)
	}

	// A stale, deliberately wrong checksum for the existing entry — as if
	// the file had been edited after it was first registered (the on-disk
	// file itself still holds original's real, correct content, so a
	// mutation that rehashes-from-disk would recompute the real checksum
	// here and overwrite this deliberately wrong one).
	tampered := map[string]string{original.Filename: "0000000000000000000000000000000000000000000000000000000000000000"}
	if err := os.WriteFile(filepath.Join(dir, store.ChecksumsFile), store.FormatChecksums(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	next := store.Migration{Version: 2, Filename: "0002_b.sql", SQL: "ALTER TABLE a ADD COLUMN extra TEXT;"}
	if err := store.AppendChecksum(dir, next); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, store.ChecksumsFile))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := store.ReadChecksums(data)
	if err != nil {
		t.Fatal(err)
	}
	if sums[original.Filename] != tampered[original.Filename] {
		t.Fatalf("AppendChecksum recomputed an existing entry: got %q, want the untouched %q", sums[original.Filename], tampered[original.Filename])
	}
	if sums[next.Filename] != store.ChecksumOf(next) {
		t.Fatalf("AppendChecksum did not record the new migration's own checksum")
	}
}

// runThenCheckDrift runs `db-migration` for a schema.sql change, then loads
// the resulting migrations and confirms `make db-check`'s own checks
// (store.CheckDrift, store.CheckAllowedStatements) pass against them: the
// ordinary generate-then-check workflow must not fail on its own output.
func runThenCheckDrift(t *testing.T, schemaSQL string, existing []store.Migration) {
	t.Helper()
	dir := setupStoreDir(t, schemaSQL, existing)

	if err := run("next", dir); err != nil {
		t.Fatalf("run: %v", err)
	}

	migrations, err := store.LoadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if err := store.CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift on db-migration's own generated output: %v", err)
	}
	if err := store.CheckAllowedStatements(migrations, nil); err != nil {
		t.Fatalf("CheckAllowedStatements on db-migration's own generated output: %v", err)
	}
}

func TestRun_GeneratedAddColumnWithDefaultCurrentTimestamp_NoDrift(t *testing.T) {
	runThenCheckDrift(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)
}

func TestRun_GeneratedAddColumnWithCheck_NoDrift(t *testing.T) {
	runThenCheckDrift(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, qty INTEGER CHECK (qty >= 0));",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)
}

func TestRun_GeneratedAddColumnWithCollate_NoDrift(t *testing.T) {
	runThenCheckDrift(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)
}

// sqldef's ADD COLUMN generator silently drops a new column's own
// REFERENCES clause — `ALTER TABLE a ADD COLUMN parent_id integer` with no
// `REFERENCES parent(id)` at all, confirmed directly against
// schema.GenerateIdempotentDDLs. Unlike DEFAULT, CHECK and COLLATE (all of
// which the generator does carry through on ADD COLUMN, per the three
// tests above), `run` itself restores this one clause — read from
// schema.sql with store.SchemaColumnReferences — onto the generated
// statement before the allow-list and drift checks run (Q60), so the
// written migration still reproduces schema.sql exactly.
func TestRun_GeneratedAddColumnWithReferences_NoDrift(t *testing.T) {
	schemaSQL := "CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id));"
	dir := setupStoreDir(t, schemaSQL, []store.Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	})

	if err := run("next", dir); err != nil {
		t.Fatalf("run: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, "migrations", "0002_next.sql"))
	if err != nil {
		t.Fatalf("expected 0002_next.sql to be written: %v", err)
	}
	if !strings.Contains(string(written), "REFERENCES") && !strings.Contains(string(written), "references") {
		t.Fatalf("written migration does not carry the restored REFERENCES clause:\n%s", written)
	}

	migrations, err := store.LoadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if err := store.CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift on db-migration's own generated output: %v", err)
	}
	if err := store.CheckAllowedStatements(migrations, nil); err != nil {
		t.Fatalf("CheckAllowedStatements on db-migration's own generated output: %v", err)
	}
}

// A REFERENCES clause with its own ON DELETE/ON UPDATE action must round
// trip exactly, not just a bare REFERENCES <table>(<column>). sqldef's own
// parser rejects a single foreign-key clause combining both ON DELETE and
// ON UPDATE in the same CREATE TABLE (confirmed directly against
// schema.GenerateIdempotentDDLs — a parser limitation on the desired
// schema text itself, unrelated to ADD COLUMN or this restoration), so
// each action is exercised on its own column here.
func TestRun_GeneratedAddColumnWithReferencesAndActions_NoDrift(t *testing.T) {
	runThenCheckDrift(t,
		`CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE a (
			id INTEGER PRIMARY KEY,
			on_delete_parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE,
			on_update_parent_id INTEGER REFERENCES parent(id) ON UPDATE SET NULL
		);`,
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)
}

// A migration with no foreign-key column at all must be byte-identical to
// today's output — the REFERENCES restoration must never touch a
// generated statement it has nothing to restore onto.
func TestRun_GeneratedAddColumnWithoutReferences_UnchangedOutput(t *testing.T) {
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)

	if err := run("add_extra", dir); err != nil {
		t.Fatalf("run: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(dir, "migrations", "0002_add_extra.sql"))
	if err != nil {
		t.Fatalf("expected 0002_add_extra.sql to be written: %v", err)
	}
	const want = "-- Generated by `make db-migration` on " // date varies; check the statement line separately
	if !strings.HasPrefix(string(written), want) {
		t.Fatalf("written migration header changed:\n%s", written)
	}
	if !strings.Contains(string(written), "ALTER TABLE `a` ADD COLUMN `extra` text;\n") {
		t.Fatalf("written migration's own ADD COLUMN statement changed:\n%s", written)
	}
}

// SQLite itself refuses ALTER TABLE ... ADD COLUMN ... REFERENCES ...
// whenever foreign key constraints are enabled and the new column's own
// default is anything other than NULL. Restoring the clause onto that
// shape would trade sqldef's silent drop for a statement SQLite itself can
// refuse, so run must refuse it outright instead, with its own specific
// reason, rather than emit it.
func TestRun_RefusesReferencesWithNonNullDefault(t *testing.T) {
	dir := setupStoreDir(t,
		"CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFAULT 0);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)

	if err := run("next", dir); err == nil {
		t.Fatal("expected run to refuse restoring REFERENCES onto a column with a non-NULL DEFAULT")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "0001_a.sql" && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}
}

// A DROP INDEX is exactly the case Q60 confirms sqldef's own
// --enable-drop-table gate would never have caught: it is not a DROP
// TABLE, and Destructive itself deliberately does not report DROP
// INDEX/VIEW/TRIGGER (allowlist.go's classifyDestructive comment).
// Removing an index from schema.sql is the reachable path that makes
// sqldef emit DROP INDEX on its own — this tool's own
// CheckAllowedStatements call is what actually refuses to write it.
func TestRun_RefusesDroppedIndex(t *testing.T) {
	dir := setupStoreDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT); CREATE INDEX ai ON a(email);"}},
	)

	if err := run("drop_index", dir); err == nil {
		t.Fatal("expected run to refuse generating a migration whose only change is a DROP INDEX")
	}

	entries, err := os.ReadDir(filepath.Join(dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "0001_a.sql" && e.Name() != store.ChecksumsFile {
			t.Fatalf("run wrote a file despite refusing: %s", e.Name())
		}
	}
}

// A new table's own inline comment, including one with an apostrophe, must
// not break either db-migration's generation or db-check's own drift
// comparison of it.
func TestRun_GeneratedNewTableWithInlineComments_NoDrift(t *testing.T) {
	runThenCheckDrift(t, `
		CREATE TABLE a (
		    id INTEGER PRIMARY KEY, -- comment with an apostrophe: it's fine
		    note TEXT
		);
	`, nil)
}

// A new table carrying its own trigger
// with a real CASE...END inside it must generate and pass db-migration's
// own drift check cleanly end to end — this is the whole tool, not just
// splitStatements/CheckDrift in isolation, exercising the same CASE...END
// pairing every unit test above already covers.
func TestRun_GeneratedNewTableWithTriggerCaseEnd_NoDrift(t *testing.T) {
	runThenCheckDrift(t, `
		CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER);
		CREATE TRIGGER a_classify AFTER INSERT ON a BEGIN
			UPDATE a SET v = CASE WHEN v > 0 THEN 1 ELSE 0 END WHERE id = NEW.id;
		END;
	`, nil)
}

// Q60's own recorded gap: sqldef's parser rejects an unquoted column named
// `key` (a bare SQL-92 reserved word SQLite itself accepts unquoted), and
// canonicalize's own double-quote/backtick equivalence (allowlist.go's
// "why" comment on canonicalize) is what makes the quoted spelling here
// round-trip cleanly against sqldef's own backtick-quoted, lower-cased
// ADD COLUMN output rather than reporting false drift.
func TestRun_GeneratedAddColumnWithDoubleQuotedKey_NoDrift(t *testing.T) {
	runThenCheckDrift(t,
		`CREATE TABLE a (id INTEGER PRIMARY KEY, "key" TEXT);`,
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
	)
}

// A jobs table shaped like #19's own needs: AUTOINCREMENT, a CHECK IN
// list, DEFAULT CURRENT_TIMESTAMP and a partial index — all must generate
// and pass both the drift check and the allow-list end to end.
func TestRun_GeneratedJobsShapedTable_NoDrift(t *testing.T) {
	runThenCheckDrift(t, `
		CREATE TABLE jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			state TEXT NOT NULL CHECK (state IN ('pending', 'running', 'done', 'failed')),
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX jobs_pending ON jobs (created_at) WHERE state = 'pending';
	`, nil)
}

// A users/sessions pair shaped like #22's own needs: STRICT,
// WITHOUT ROWID, ON DELETE CASCADE and a CASE trigger.
func TestRun_GeneratedUsersSessionsShapedTables_NoDrift(t *testing.T) {
	runThenCheckDrift(t, `
		CREATE TABLE users (
			id INTEGER PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			is_admin INTEGER NOT NULL DEFAULT 0
		) STRICT;
		CREATE TABLE sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			role TEXT NOT NULL
		) STRICT, WITHOUT ROWID;
		CREATE TRIGGER sessions_role AFTER INSERT ON sessions BEGIN
			UPDATE sessions SET role = CASE WHEN (SELECT is_admin FROM users WHERE id = NEW.user_id) = 1 THEN 'admin' ELSE 'user' END WHERE token = NEW.token;
		END;
	`, nil)
}
