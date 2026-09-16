package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/store/storetest"
)

// `make db-check` only ever runs against a clean tree in CI, so a
// regression that dropped one of run's checks entirely would pass every CI
// run this repository has. Each test below builds its own store dir (never
// the real internal/store) and drives run(dir) directly, one failure mode
// at a time.

func setupCheckDir(t *testing.T, schemaSQL string, migrations []store.Migration, contracts string) string {
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
	if contracts != "" {
		if err := os.WriteFile(filepath.Join(dir, "migrations", store.ContractsFile), []byte(contracts), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRun_PassesOnAConsistentStoreDir(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
		"",
	)
	if err := run(dir); err != nil {
		t.Fatalf("run on a consistent store dir: %v", err)
	}
}

func TestRun_FailsOnEditedMigration(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
		"",
	)
	// Edit the migration on disk after it was checksummed.
	if err := os.WriteFile(filepath.Join(dir, "migrations", "0001_a.sql"), []byte("CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dir); err == nil {
		t.Fatal("expected run to fail on an edited migration")
	}
}

func TestRun_FailsOnDrift(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY, extra TEXT);",
		[]store.Migration{{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"}},
		"",
	)
	if err := run(dir); err == nil {
		t.Fatal("expected run to fail on drift between schema.sql and the migrations")
	}
}

func TestRun_FailsOnUnregisteredDestructiveMigration(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE b (id INTEGER PRIMARY KEY);"},
			{Version: 2, Filename: "0002_drop_b.sql", SQL: "DROP TABLE b;"},
		},
		"",
	)
	if err := run(dir); err == nil {
		t.Fatal("expected run to fail on an unregistered destructive migration")
	}
}

func TestRun_PassesOnRegisteredContractStep(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE b (id INTEGER PRIMARY KEY);"},
			{Version: 2, Filename: "0002_drop_b.sql", SQL: "DROP TABLE b;"},
		},
		"0002_drop_b.sql  reviewed\n",
	)
	if err := run(dir); err != nil {
		t.Fatalf("run with a registered contract step: %v", err)
	}
}

// SAVEPOINT/RELEASE is deliberately used here, not COMMIT: it replays
// cleanly with no drift and no execution error of its own (confirmed
// directly against ApplySchemaOnly), so this test only ever fails if run's
// own CheckAllowedStatements call is what catches it — a COMMIT with no
// open transaction would fail at the drift-check stage regardless of
// whether the allow-list runs at all, which would not pin this.
func TestRun_FailsOnTransactionControlStatement(t *testing.T) {
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY); SAVEPOINT s1; RELEASE s1;"},
		},
		"",
	)
	if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
		t.Fatalf("run on a migration containing a transaction-control statement = %v, want store.ErrDisallowedStatement", err)
	}
}

// One `run` test per refused kind the allow-list must reject outright,
// registered as a contract step or not.
func TestRun_FailsOnEveryRefusedStatementKind(t *testing.T) {
	cases := map[string]string{
		"EXPLAIN PRAGMA":                  "EXPLAIN PRAGMA ignore_check_constraints = 1;",
		"EXPLAIN QUERY PLAN":              "EXPLAIN QUERY PLAN PRAGMA foreign_keys = OFF;",
		"CREATE TEMP TRIGGER":             "CREATE TEMP TRIGGER evil AFTER INSERT ON a BEGIN DELETE FROM main.a; END;",
		"CREATE TABLE temp.x":             "CREATE TABLE temp.x (id INTEGER);",
		"CREATE TABLE AS SELECT":          "CREATE TABLE archive AS SELECT * FROM a;",
		"WITH ... INSERT":                 "WITH cte AS (SELECT 1 AS id) INSERT INTO a (id) SELECT id FROM cte;",
		"DELETE in an ordinary migration": "DELETE FROM a;",
		"UPDATE in an ordinary migration": "UPDATE a SET id = 1;",
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			dir := setupCheckDir(t,
				"CREATE TABLE a (id INTEGER PRIMARY KEY);",
				[]store.Migration{
					{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
					{Version: 2, Filename: "0002_bad.sql", SQL: bad},
				},
				"",
			)
			if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
				t.Fatalf("run on %s = %v, want store.ErrDisallowedStatement", name, err)
			}
		})
	}
}

// TestRun_FailsOnEveryDisallowedStatementKind drives storetest.
// DisallowedStatementExamples — the exact same set TestClassifyStatement_
// Disallowed (internal/store) and the Runner.Apply-level reproduction
// use — through run() itself, one migration file per case. Asserting
// store.ErrDisallowedStatement specifically, not just any error, is what
// this pins: a mutation that wrongly classifies one of these kinds as
// allowed would let run() reach CheckDrift instead, which for several of
// these kinds (RELEASE, DETACH, an INSERT ... VALUES with the wrong column
// count, INSERT OR REPLACE referencing a table that doesn't exist, ...)
// fails on replay anyway, with some other error `err == nil` could never
// tell apart from this one.
func TestRun_FailsOnEveryDisallowedStatementKind(t *testing.T) {
	target := filepath.Join(t.TempDir(), "attach-target.db")
	for name, bad := range storetest.WithAttachTarget(target) {
		t.Run(name, func(t *testing.T) {
			dir := setupCheckDir(t,
				"CREATE TABLE a (id INTEGER PRIMARY KEY);",
				[]store.Migration{
					{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
					{Version: 2, Filename: "0002_bad.sql", SQL: bad},
				},
				"",
			)
			if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
				t.Fatalf("run on %s = %v, want store.ErrDisallowedStatement", name, err)
			}
		})
	}
}

// A migration containing a file-path ATTACH must be refused before run()
// ever replays it — CheckDrift's own ApplySchemaOnly would otherwise
// execute it against a live connection, creating a real file on disk. The
// target path lives inside t.TempDir() and is confirmed absent afterward,
// so this pins the refusal-before-execution ordering directly, not just
// that run() returns some error.
func TestRun_RefusesAttachBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "attach-target.db")
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
			{Version: 2, Filename: "0002_bad.sql", SQL: "ATTACH DATABASE '" + target + "' AS x;"},
		},
		"",
	)
	if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
		t.Fatalf("run on a migration containing ATTACH = %v, want store.ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ATTACH target %s exists after run refused the migration containing it", target)
	}
}

// The same, for VACUUM INTO — SQLite's own way to write a whole database
// copy to an arbitrary path in one statement.
func TestRun_RefusesVacuumIntoBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "vacuum-target.db")
	dir := setupCheckDir(t,
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		[]store.Migration{
			{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
			{Version: 2, Filename: "0002_bad.sql", SQL: "VACUUM INTO '" + target + "';"},
		},
		"",
	)
	if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
		t.Fatalf("run on a migration containing VACUUM INTO = %v, want store.ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("VACUUM INTO target %s exists after run refused the migration containing it", target)
	}
}

// The same DML shapes are refused even when the migration carrying them is
// registered as a contract step — a contract step's only DML is its own
// documented INSERT ... SELECT copy.
func TestRun_FailsOnDMLEvenInARegisteredContractStep(t *testing.T) {
	for _, dml := range []string{"DELETE FROM a;", "UPDATE a SET id = 1;"} {
		t.Run(dml, func(t *testing.T) {
			dir := setupCheckDir(t,
				"CREATE TABLE a (id INTEGER PRIMARY KEY);",
				[]store.Migration{
					{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
					{Version: 2, Filename: "0002_bad.sql", SQL: dml},
				},
				"0002_bad.sql  reviewed\n",
			)
			if err := run(dir); !errors.Is(err, store.ErrDisallowedStatement) {
				t.Fatalf("run on %q even though its migration is registered = %v, want store.ErrDisallowedStatement", dml, err)
			}
		})
	}
}
