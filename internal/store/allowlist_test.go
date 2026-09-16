package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store/storetest"
)

// classify is a small test helper: classifyStatement, but tolerant of
// trailing whitespace/semicolons the way splitStatements' own output is.
func classify(t *testing.T, stmt string) statementClass {
	t.Helper()
	stmts := splitStatements(stmt)
	if len(stmts) != 1 {
		t.Fatalf("splitStatements(%q) = %v, want exactly one statement", stmt, stmts)
	}
	return classifyStatement(stmts[0])
}

func TestClassifyStatement_Ordinary(t *testing.T) {
	cases := []string{
		"CREATE TABLE a (id INTEGER PRIMARY KEY);",
		"CREATE TABLE IF NOT EXISTS a (id INTEGER PRIMARY KEY);",
		"CREATE INDEX ai ON a (id);",
		"CREATE UNIQUE INDEX ai ON a (id);",
		"CREATE INDEX IF NOT EXISTS ai ON a (id);",
		"CREATE VIEW v AS SELECT id FROM a;",
		"CREATE VIEW IF NOT EXISTS v AS SELECT id FROM a;",
		"CREATE TRIGGER t AFTER INSERT ON a BEGIN SELECT 1; END;",
		"CREATE TRIGGER IF NOT EXISTS t AFTER INSERT ON a BEGIN SELECT 1; END;",
		"ALTER TABLE a ADD COLUMN note TEXT;",
		"ALTER TABLE a ADD note TEXT;",
		"ALTER TABLE a RENAME COLUMN b TO c;",
		"ALTER TABLE a RENAME b TO c;",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			if got := classify(t, sql); got != classOrdinary {
				t.Fatalf("classifyStatement(%q) = %v, want classOrdinary", sql, got)
			}
		})
	}
}

func TestClassifyStatement_ContractOnly(t *testing.T) {
	cases := []string{
		"DROP TABLE a;",
		"DROP TABLE IF EXISTS a;",
		"DROP INDEX ai;",
		"DROP VIEW v;",
		"DROP TRIGGER t;",
		"ALTER TABLE a DROP COLUMN b;",
		"ALTER TABLE a DROP b;",
		"ALTER TABLE a RENAME TO b;",
		"INSERT INTO a SELECT * FROM b;",
		"INSERT INTO a (id, v) SELECT id, v FROM b;",
	}
	for _, sql := range cases {
		t.Run(sql, func(t *testing.T) {
			if got := classify(t, sql); got != classContractOnly {
				t.Fatalf("classifyStatement(%q) = %v, want classContractOnly", sql, got)
			}
		})
	}
}

// TestClassifyStatement_Disallowed is the direct reproduction for every
// refused kind CheckAllowedStatements and CheckStatementVocabulary must
// reject outright, registered or not. DisallowedStatementExamples is
// shared with the dbcheck- and Runner.Apply-level reproductions below and
// in tools/dbcheck, so all three layers are driven by the same cases.
func TestClassifyStatement_Disallowed(t *testing.T) {
	for name, sql := range storetest.DisallowedStatementExamples {
		t.Run(name, func(t *testing.T) {
			if got := classify(t, sql); got != classDisallowed {
				t.Fatalf("classifyStatement(%q) = %v, want classDisallowed", sql, got)
			}
		})
	}
}

func TestCheckAllowedStatements_RefusesDisallowedRegardlessOfRegistration(t *testing.T) {
	cases := map[string]string{
		"EXPLAIN PRAGMA":         "EXPLAIN PRAGMA ignore_check_constraints = 1;",
		"EXPLAIN QUERY PLAN":     "EXPLAIN QUERY PLAN PRAGMA foreign_keys = OFF;",
		"CREATE TEMP TRIGGER":    "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TEMP TRIGGER evil AFTER INSERT ON a BEGIN DELETE FROM main.a; END;",
		"CREATE TABLE temp.x":    "CREATE TABLE temp.x (id INTEGER);",
		"CREATE TABLE AS SELECT": "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE archive AS SELECT * FROM a;",
		"WITH ... INSERT":        "CREATE TABLE a (id INTEGER PRIMARY KEY); WITH cte AS (SELECT 1 AS id) INSERT INTO a SELECT id FROM cte;",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			m := Migration{Version: 1, Filename: "0001_bad.sql", SQL: sql}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{}); !errors.Is(err, ErrDisallowedStatement) {
				t.Fatalf("CheckAllowedStatements(%q) unregistered = %v, want ErrDisallowedStatement", sql, err)
			}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{"0001_bad.sql": true}); !errors.Is(err, ErrDisallowedStatement) {
				t.Fatalf("CheckAllowedStatements(%q) registered as a contract step = %v, want ErrDisallowedStatement — this shape is never allowed, contract or not", sql, err)
			}
		})
	}
}

// DELETE and UPDATE are refused whether the migration is a registered
// contract step or not — the only DML a contract step may carry is its own
// INSERT ... SELECT copy step (doc 01 §4); a data change belongs to a Go
// transform.
func TestCheckAllowedStatements_RefusesDMLInOrdinaryAndContractMigrations(t *testing.T) {
	for _, dml := range []string{"DELETE FROM a;", "UPDATE a SET v = 1;"} {
		t.Run(dml, func(t *testing.T) {
			m := Migration{Version: 1, Filename: "0001_dml.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER); " + dml}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{}); err == nil {
				t.Fatalf("CheckAllowedStatements(%q) in an ordinary migration = nil, want an error", dml)
			}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{"0001_dml.sql": true}); err == nil {
				t.Fatalf("CheckAllowedStatements(%q) in a registered contract step = nil, want an error", dml)
			}
		})
	}
}

func TestCheckAllowedStatements_RefusesUnregisteredContractOnlyStatement(t *testing.T) {
	cases := map[string]string{
		"DROP TABLE":        "CREATE TABLE a (id INTEGER PRIMARY KEY); DROP TABLE a;",
		"DROP INDEX":        "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE INDEX ai ON a(id); DROP INDEX ai;",
		"ALTER DROP COLUMN": "CREATE TABLE a (id INTEGER PRIMARY KEY, b TEXT); ALTER TABLE a DROP COLUMN b;",
		"ALTER RENAME TO":   "CREATE TABLE a (id INTEGER PRIMARY KEY); ALTER TABLE a RENAME TO b;",
		"INSERT ... SELECT": "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO b SELECT * FROM a;",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			m := Migration{Version: 1, Filename: "0001_c.sql", SQL: sql}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{}); err == nil {
				t.Fatalf("CheckAllowedStatements(%q) unregistered = nil, want an error", sql)
			}
			if err := CheckAllowedStatements([]Migration{m}, map[string]bool{"0001_c.sql": true}); err != nil {
				t.Fatalf("CheckAllowedStatements(%q) registered as a contract step: %v, want nil", sql, err)
			}
		})
	}
}

func TestCheckAllowedStatements_PassesOrdinaryMigration(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
		{Version: 2, Filename: "0002_b.sql", SQL: "ALTER TABLE a ADD COLUMN note TEXT;"},
	}
	if err := CheckAllowedStatements(migrations, map[string]bool{}); err != nil {
		t.Fatalf("CheckAllowedStatements on ordinary migrations: %v", err)
	}
}

// CheckStatementVocabulary is the runner's own gate: it refuses exactly
// what CheckAllowedStatements calls classDisallowed, but never re-checks
// contract registration — a registered contract step's DROP/RENAME/
// INSERT...SELECT must still be allowed to run once it is already embedded
// in the binary.
func TestCheckStatementVocabulary_AllowsContractOnlyShapesUnconditionally(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE a_new (id INTEGER PRIMARY KEY);"},
		{Version: 2, Filename: "0002_rebuild.sql", SQL: "INSERT INTO a_new SELECT * FROM a; DROP TABLE a; ALTER TABLE a_new RENAME TO a;"},
	}
	if err := CheckStatementVocabulary(migrations); err != nil {
		t.Fatalf("CheckStatementVocabulary on a rebuild sequence: %v", err)
	}
}

func TestCheckStatementVocabulary_RefusesDisallowedStatement(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
		{Version: 2, Filename: "0002_bad.sql", SQL: "EXPLAIN PRAGMA ignore_check_constraints = 1;"},
	}
	if err := CheckStatementVocabulary(migrations); !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("CheckStatementVocabulary on a migration containing EXPLAIN PRAGMA = %v, want ErrDisallowedStatement", err)
	}
}

func TestCheckSchemaVocabulary_PassesOrdinaryStatements(t *testing.T) {
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY); ALTER TABLE a ADD COLUMN note TEXT;"
	if err := CheckSchemaVocabulary(schemaSQL); err != nil {
		t.Fatalf("CheckSchemaVocabulary on ordinary statements: %v", err)
	}
}

// CheckSchemaVocabulary refuses a classContractOnly shape exactly like a
// classDisallowed one — schema.sql describes the desired end state, never
// a step to get there, so a rebuild-only shape (here, DROP TABLE) never
// belongs in it, even though the same statement is allowed in a migration
// registered as a contract step.
func TestCheckSchemaVocabulary_RefusesContractOnlyStatement(t *testing.T) {
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY); DROP TABLE a;"
	if err := CheckSchemaVocabulary(schemaSQL); !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("CheckSchemaVocabulary on a schema containing DROP TABLE = %v, want ErrDisallowedStatement", err)
	}
}

func TestCheckSchemaVocabulary_RefusesDisallowedStatement(t *testing.T) {
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY); EXPLAIN PRAGMA ignore_check_constraints = 1;"
	if err := CheckSchemaVocabulary(schemaSQL); !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("CheckSchemaVocabulary on a schema containing EXPLAIN PRAGMA = %v, want ErrDisallowedStatement", err)
	}
}

// --- Runner.Apply-level reproductions: nothing is ever applied, and the
// pooled connection's own state (CHECK enforcement) is never touched,
// because CheckStatementVocabulary runs before Apply opens a transaction
// at all.

func applyAndExpectNothingApplied(t *testing.T, badSQL string) *Runner {
	t.Helper()
	ctx := context.Background()
	setup := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER CHECK (v > 0));"},
	}
	r, _ := newRunner(t, setup)
	if _, _, err := r.Apply(ctx); err != nil {
		t.Fatalf("setup Apply: %v", err)
	}
	r.Migrations = append(r.Migrations, Migration{Version: 2, Filename: "0002_bad.sql", SQL: badSQL})

	_, snapshotPath, err := r.Apply(ctx)
	if !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("Apply(%q) error = %v, want ErrDisallowedStatement", badSQL, err)
	}
	if snapshotPath != "" {
		t.Fatalf("Apply(%q) returned snapshot path %q, want empty — the refusal happens before Snapshot is ever taken", badSQL, snapshotPath)
	}
	version, err := r.CurrentVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Fatalf("CurrentVersion = %d, want 1 (the disallowed migration must not be recorded as applied)", version)
	}
	return r
}

func TestRunnerApply_RefusesExplainPragma_ChecksStillEnforced(t *testing.T) {
	ctx := context.Background()
	r := applyAndExpectNothingApplied(t, "EXPLAIN PRAGMA ignore_check_constraints = 1;")
	if _, err := r.DB.ExecContext(ctx, "INSERT INTO a (id, v) VALUES (1, -1)"); err == nil {
		t.Fatal("CHECK enforcement was disabled by a migration Apply should never have executed at all")
	}
}

func TestRunnerApply_RefusesExplainQueryPlanPragma(t *testing.T) {
	applyAndExpectNothingApplied(t, "EXPLAIN QUERY PLAN PRAGMA foreign_keys = OFF;")
}

func TestRunnerApply_RefusesCreateTempTriggerDeletingRows(t *testing.T) {
	ctx := context.Background()
	r := applyAndExpectNothingApplied(t, "CREATE TEMP TRIGGER evil AFTER INSERT ON a BEGIN DELETE FROM main.a; END;")
	if _, err := r.DB.ExecContext(ctx, "INSERT INTO a (id, v) VALUES (1, 1)"); err != nil {
		t.Fatalf("ordinary insert after the refused migration: %v", err)
	}
	var count int
	if err := r.DB.QueryRowContext(ctx, "SELECT count(*) FROM a").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 — the temp trigger must never have been created at all", count)
	}
}

func TestRunnerApply_RefusesCreateTableTempQualified(t *testing.T) {
	applyAndExpectNothingApplied(t, "CREATE TABLE temp.x (id INTEGER);")
}

func TestRunnerApply_RefusesDeleteInOrdinaryMigration(t *testing.T) {
	applyAndExpectNothingApplied(t, "DELETE FROM a;")
}

func TestRunnerApply_RefusesUpdateInOrdinaryMigration(t *testing.T) {
	applyAndExpectNothingApplied(t, "UPDATE a SET v = 2;")
}

func TestRunnerApply_RefusesCreateTableAsSelect(t *testing.T) {
	applyAndExpectNothingApplied(t, "CREATE TABLE archive AS SELECT * FROM a;")
}

func TestRunnerApply_RefusesWithInsert(t *testing.T) {
	applyAndExpectNothingApplied(t, "WITH cte AS (SELECT 1 AS id) INSERT INTO a (id) SELECT id FROM cte;")
}

// A registered contract step never reaches the runner with DELETE/UPDATE
// in it either — db-check refuses to let such a migration exist at all
// (TestCheckAllowedStatements_RefusesDMLInOrdinaryAndContractMigrations),
// so the runner's own defense in depth is exercised the same way as an
// ordinary migration's: CheckStatementVocabulary does not distinguish the
// two, by design (a contract step is not registered anywhere the runner
// reads at all).
func TestRunnerApply_RefusesDeleteEvenIfHypotheticallyEmbeddedFromAContractStep(t *testing.T) {
	applyAndExpectNothingApplied(t, "DELETE FROM a WHERE v < 0;")
}

// TestRunnerApply_RefusesEveryDisallowedStatementKind drives every case in
// DisallowedStatementExamples — the exact same set TestClassifyStatement_
// Disallowed and tools/dbcheck's own table-driven test use — through a
// real Runner.Apply, asserting nothing was ever applied. A mutant that
// wrongly classifies one of these kinds as allowed would let Apply
// actually execute it and record its migration as applied, which
// applyAndExpectNothingApplied's version check catches regardless of
// whether the statement itself is schema-visible (a PRAGMA, VACUUM or
// ATTACH changes nothing sqlite_master would show, but still bumps the
// bookkeeping version if Apply ever reaches it).
func TestRunnerApply_RefusesEveryDisallowedStatementKind(t *testing.T) {
	target := filepath.Join(t.TempDir(), "attach-target.db")
	for name, sql := range storetest.WithAttachTarget(target) {
		t.Run(name, func(t *testing.T) {
			applyAndExpectNothingApplied(t, sql)
		})
	}
}

// describeStatement never echoes more than a statement's own leading
// keywords into an error message.
func TestDescribeStatement_NamesLeadingWords(t *testing.T) {
	got := describeStatement("insert into a select * from b")
	if !strings.HasPrefix(got, "INSERT INTO A SELECT") {
		t.Fatalf("describeStatement = %q, want it to start with the statement's own leading words", got)
	}
}

// --- splitStatements mutant coverage: removing the skip for only one
// quoting style, or removing a beginDepth guard, must be individually
// caught by its own test.

func TestSplitStatements_DoubleQuotedIdentifierWithParenAndSemicolon(t *testing.T) {
	sql := `CREATE TABLE "a(" (x INTEGER); COMMIT;`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

func TestSplitStatements_BacktickIdentifierWithApostrophe(t *testing.T) {
	sql := "CREATE TABLE `it's` (x INTEGER); COMMIT;"
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

func TestSplitStatements_BracketIdentifierWithParenAndSemicolon(t *testing.T) {
	sql := `CREATE TABLE [a(] (x INTEGER); COMMIT;`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

// A CREATE TEMP TRIGGER (or CREATE TEMPORARY TRIGGER) must be recognized
// as opening its own BEGIN...END block exactly like a plain CREATE
// TRIGGER — isCreateTriggerLeading's own second leading-word branch. This
// is pinned at splitStatements directly, not through CheckDrift: a CREATE
// TEMP TRIGGER can never legitimately appear in schema.sql in the first
// place (CheckSchemaVocabulary refuses every TEMP object outright), so
// CheckDrift is not where this coverage belongs.
func TestSplitStatements_TempTriggerWithCaseEnd(t *testing.T) {
	sql := `
		CREATE TEMP TRIGGER a_classify AFTER INSERT ON a BEGIN
			UPDATE a SET v = CASE WHEN v > 0 THEN 1 ELSE 0 END WHERE id = NEW.id;
		END;
		COMMIT;
	`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

// A trigger literally named "begin" must not itself open a tracked block —
// only a statement's own leading CREATE [TEMP|TEMPORARY] TRIGGER words do
// that, never a bare "begin" token appearing anywhere else, including as
// the trigger's own name.
func TestSplitStatements_TriggerNamedBeginDoesNotConfuseDepthTracking(t *testing.T) {
	sql := `
		CREATE TRIGGER begin AFTER INSERT ON a BEGIN
			SELECT 1;
		END;
		COMMIT;
	`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

// "RENAME COLUMN begin TO x" must not be mistaken for opening a tracked
// BEGIN...END block — begin here is a column name, not the trigger-body
// keyword, and this statement isn't even a CREATE TRIGGER.
func TestSplitStatements_RenameColumnNamedBeginDoesNotConfuseDepthTracking(t *testing.T) {
	sql := `ALTER TABLE a RENAME COLUMN begin TO x; COMMIT;`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

// A bare CASE outside any CREATE TRIGGER body must never increment
// beginDepth at all — only a CASE already inside a tracked trigger body
// does. Without that guard, this CASE (with no matching END anywhere in
// the input) leaves beginDepth stuck above zero for the rest of the
// input, swallowing the semicolon before COMMIT into the same statement.
func TestSplitStatements_BareCaseOutsideTriggerBodyDoesNotOpenABlock(t *testing.T) {
	sql := `SELECT CASE; COMMIT;`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements", sql, stmts)
	}
}

// The trigger-body "begin" branch may only ever open the tracked block —
// set beginDepth from 0 to 1 — not reset it whenever a later, unrelated
// bare "begin" identifier appears deeper in the same trigger body. A bare
// "begin" used as a column value inside the body's own CASE expression,
// while a nested CASE has already raised beginDepth to 2, must never
// collapse that back down to 1: doing so lets the CASE's own END close the
// tracked block one END early, and the trigger body's own internal
// SELECT statement's semicolon — which must stay swallowed into this one
// top-level CREATE TRIGGER statement — gets mistaken for the boundary
// instead.
func TestSplitStatements_BareBeginDeepInTriggerBodyDoesNotResetDepth(t *testing.T) {
	sql := `
		CREATE TRIGGER t AFTER INSERT ON a BEGIN
			SELECT CASE WHEN begin > 0 THEN 1 ELSE 0 END;
			UPDATE a SET v = 2 WHERE id = NEW.id;
		END;
		COMMIT;
	`
	stmts := splitStatements(sql)
	if len(stmts) != 2 {
		t.Fatalf("splitStatements(%q) = %v, want 2 statements (the trigger definition, then COMMIT)", sql, stmts)
	}
}
