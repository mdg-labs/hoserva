package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckDrift_NoDriftForRealSchema(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	schemaSQL, err := schemaSQLForTest(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift on the real schema.sql and its own migrations: %v", err)
	}
}

func TestCheckDrift_DetectsExtraTable(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}
	// schema.sql has moved on to a second table that no migration produces.
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);\nCREATE TABLE b (id INTEGER PRIMARY KEY);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a table schema.sql has that no migration produces")
	}
}

// Q60's own finding: sqlite3def's SQLite diff reports "Nothing is
// modified" for a column type change on an existing column — it has no
// SQLite rebuild strategy. CheckDrift must not rely on that diff to decide
// whether schema.sql and the migrations agree, or this exact case would
// pass when it should fail.
func TestCheckDrift_DetectsColumnTypeChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT NOT NULL);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, note INTEGER NOT NULL);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a column type change the migrations never applied")
	}
}

func TestCheckDrift_NoFalsePositiveForFormatting(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift on matching schema and migrations: %v", err)
	}
}

// Regression test for a real bug caught while building this check (not a
// hypothetical): SQLite's own `ALTER TABLE ... ADD COLUMN` rewrites the
// *stored* CREATE TABLE text in sqlite_master by splicing in the ALTER
// statement's own wording verbatim — backtick-quoted, lower-case type
// keyword — which does not match a hand-written schema.sql's unquoted,
// upper-case text even though the two schemas are identical. A literal
// sqlite_master.sql text comparison flags this as drift on every single
// ADD COLUMN migration; CheckDrift must not.
func TestCheckDrift_NoFalsePositiveForAddColumnRewriting(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT NOT NULL);"},
		{Version: 2, Filename: "0002_b.sql", SQL: "ALTER TABLE `a` ADD COLUMN `extra` text;"},
	}
	schemaSQL := "CREATE TABLE a (\n    id INTEGER PRIMARY KEY,\n    note TEXT NOT NULL,\n    extra TEXT\n);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift flagged drift from ADD COLUMN's own re-serialization, not a real schema difference: %v", err)
	}
}

// None of a table/column CHECK, UNIQUE, a foreign key's target or
// ON DELETE/UPDATE action, COLLATE, STRICT, WITHOUT ROWID or a generated
// column is visible to a plain PRAGMA table_info comparison — a scan that
// only read that PRAGMA back would miss each one removed from schema.sql
// with no corresponding migration change. Every case here must fail.

func TestCheckDrift_DetectsRemovedColumnCheck(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY CHECK (id = 1));"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a removed column-level CHECK constraint")
	}
}

func TestCheckDrift_DetectsAddedTableCheck(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, lo INTEGER, hi INTEGER);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, lo INTEGER, hi INTEGER, CHECK (lo <= hi));"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added table-level CHECK constraint")
	}
}

func TestCheckDrift_DetectsAddedUnique(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT NOT NULL);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT NOT NULL UNIQUE);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added UNIQUE constraint (a bare autoindex with no CREATE INDEX text at all)")
	}
}

func TestCheckDrift_DetectsForeignKeyActionChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id));
		`},
	}
	schemaSQL := `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE);
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a foreign key's added ON DELETE CASCADE action")
	}
}

func TestCheckDrift_DetectsForeignKeyTargetChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE a (id INTEGER PRIMARY KEY);
			CREATE TABLE b (id INTEGER PRIMARY KEY);
			CREATE TABLE c (id INTEGER PRIMARY KEY, ref INTEGER REFERENCES a(id));
		`},
	}
	schemaSQL := `
		CREATE TABLE a (id INTEGER PRIMARY KEY);
		CREATE TABLE b (id INTEGER PRIMARY KEY);
		CREATE TABLE c (id INTEGER PRIMARY KEY, ref INTEGER REFERENCES b(id));
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a foreign key retargeted to a different table")
	}
}

func TestCheckDrift_DetectsCollateChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT COLLATE NOCASE);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added COLLATE clause")
	}
}

func TestCheckDrift_DetectsStrictChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY) STRICT;"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added STRICT table option")
	}
}

func TestCheckDrift_DetectsWithoutRowidChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id TEXT PRIMARY KEY);"},
	}
	schemaSQL := "CREATE TABLE a (id TEXT PRIMARY KEY) WITHOUT ROWID;"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added WITHOUT ROWID table option")
	}
}

func TestCheckDrift_DetectsGeneratedColumnChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, w INTEGER, h INTEGER, area INTEGER);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, w INTEGER, h INTEGER, area INTEGER GENERATED ALWAYS AS (w * h) STORED);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a column that became a generated column")
	}
}

// The rest of this file covers every case a clause-by-clause extraction
// would miss once comments, a second clause on the same column, or
// something no PRAGMA exposes at all is involved. The tokenizer-based
// whole-statement comparison (sqltoken.go, schemastruct.go) is meant to
// catch these without enumerating any of them individually.

func TestCheckDrift_DetectsAddedOnConflictClause(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT UNIQUE);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT UNIQUE ON CONFLICT REPLACE);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added ON CONFLICT REPLACE clause (which silently deletes rows on conflict)")
	}
}

func TestCheckDrift_DetectsAddedAutoincrement(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY AUTOINCREMENT);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added AUTOINCREMENT")
	}
}

func TestCheckDrift_DetectsPartialIndexWhereClause(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER, email TEXT); CREATE INDEX ai ON a(email);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER, email TEXT); CREATE INDEX ai ON a(email) WHERE active = 1;"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added partial-index WHERE clause")
	}
}

func TestCheckDrift_DetectsExpressionIndexChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT); CREATE INDEX ai ON a(email);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, email TEXT); CREATE INDEX ai ON a(lower(email));"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an index column becoming an expression")
	}
}

func TestCheckDrift_DetectsSecondCheckOnSameColumn(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, qty INTEGER CHECK (qty >= 0));"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, qty INTEGER CHECK (qty >= 0) CHECK (qty <= 1000));"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a second CHECK constraint added to a column that already had one")
	}
}

func TestCheckDrift_DetectsDeferrableChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id));
		`},
	}
	schemaSQL := `
		CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED);
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added DEFERRABLE clause")
	}
}

// A CHECK's own expression is free to contain a COLLATE — this must not be
// mistaken for the column's own COLLATE clause, and must still compare
// correctly as part of the CHECK's text.
func TestCheckDrift_DetectsCollateInsideCheckExpression(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT CHECK (name = name COLLATE NOCASE));"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT CHECK (name = name COLLATE BINARY));"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a changed COLLATE inside a CHECK expression")
	}
}

// A comma inside a comment must never be mistaken for a column separator,
// and an apostrophe inside a comment must never be mistaken for the start
// of a string literal.
func TestCheckDrift_NoFalsePositiveForCommentsWithCommaAndApostrophe(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE a (
				id INTEGER PRIMARY KEY, -- comma, comma, comma
				note TEXT -- it's an apostrophe
			);
		`},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift flagged drift from comment text, not a real schema difference: %v", err)
	}
}

// A CHECK constraint inside a /* ... */ comment must never be counted as a
// real constraint.
func TestCheckDrift_NoFalsePositiveForCheckInsideBlockComment(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY /* CHECK (id > 0) */);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf("CheckDrift flagged a CHECK inside a block comment as a real constraint: %v", err)
	}
}

// DEFAULT and NOT NULL are changes Q60 names sqldef's own diff as unable
// to detect either.

func TestCheckDrift_DetectsDefaultChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER DEFAULT 0);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER DEFAULT 1);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a changed DEFAULT value")
	}
}

func TestCheckDrift_DetectsNotNullChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, note TEXT NOT NULL);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an added NOT NULL constraint")
	}
}

func TestCheckDrift_DetectsTriggerTextChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT, touched_at TEXT);
			CREATE TRIGGER a_touch AFTER UPDATE ON a BEGIN
				UPDATE a SET touched_at = 'old' WHERE id = NEW.id;
			END;
		`},
	}
	schemaSQL := `
		CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT, touched_at TEXT);
		CREATE TRIGGER a_touch AFTER UPDATE ON a BEGIN
			UPDATE a SET touched_at = 'new' WHERE id = NEW.id;
		END;
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a changed trigger body")
	}
}

func TestCheckDrift_DetectsViewTextChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT);
			CREATE VIEW a_view AS SELECT id, v FROM a WHERE v IS NOT NULL;
		`},
	}
	schemaSQL := `
		CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT);
		CREATE VIEW a_view AS SELECT id, v FROM a WHERE v IS NULL;
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a changed view definition")
	}
}

func TestCheckDrift_DetectsIndexColumnListChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT, y TEXT); CREATE INDEX ai ON a(x);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, x TEXT, y TEXT); CREATE INDEX ai ON a(x, y);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for an index gaining a second column")
	}
}

// A trigger body containing its own CASE...END must compare correctly
// (and, when identical on both sides, never falsely flag drift) — a
// splitStatements-style depth-tracking bug would also make CheckDrift
// itself misread this trigger's stored text.
func TestCheckDrift_NoFalsePositiveForTriggerWithCaseEnd(t *testing.T) {
	sql := `
		CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER);
		CREATE TRIGGER a_classify AFTER INSERT ON a BEGIN
			UPDATE a SET v = CASE WHEN v > 0 THEN 1 ELSE 0 END WHERE id = NEW.id;
		END;
	`
	migrations := []Migration{{Version: 1, Filename: "0001_a.sql", SQL: sql}}

	if err := CheckDrift(context.Background(), sql, migrations); err != nil {
		t.Fatalf("CheckDrift flagged a trigger with its own CASE...END as drift: %v", err)
	}
}

// The opposite direction of TestCheckDrift_DetectsExtraTable: an object the
// migrations create that schema.sql no longer has must be detected too.
func TestCheckDrift_DetectsTableOnlyMigrationsProduce(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY); CREATE TABLE b (id INTEGER PRIMARY KEY);"},
	}
	// schema.sql has moved on and no longer has table b at all.
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a table the migrations produce that schema.sql no longer has")
	}
}

func TestCheckDrift_DetectsChangedGeneratedExpression(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, w INTEGER, h INTEGER, area INTEGER GENERATED ALWAYS AS (w * h) STORED);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, w INTEGER, h INTEGER, area INTEGER GENERATED ALWAYS AS (w + h) STORED);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a changed generated-column expression")
	}
}

// SQLite falls back to treating a double-quoted token as a string
// literal whenever it doesn't resolve to a column, and case is real data
// in a string literal, so DEFAULT "Active" and DEFAULT "active" are two
// different default values — the tokenizer must never fold a
// double-quoted token's case.
func TestCheckDrift_DetectsDoubleQuotedDefaultCaseChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT "active");`},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT "Active");`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for DEFAULT "active" vs DEFAULT "Active"`)
	}
}

// The same fallback applies to a bare, unquoted word directly after
// DEFAULT — SQLite accepts one and treats it as a string literal, not an
// identifier, so DEFAULT pending and DEFAULT Pending are two different
// values too.
func TestCheckDrift_DetectsBareDefaultCaseChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT pending);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT Pending);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for DEFAULT pending vs DEFAULT Pending")
	}
}

// The same double-quoted-as-string-literal fallback inside a CHECK's own
// IN (...) list.
func TestCheckDrift_DetectsDoubleQuotedCheckListCaseChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `CREATE TABLE a (id INTEGER PRIMARY KEY, s TEXT CHECK (s IN ("a", "b")));`},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, s TEXT CHECK (s IN ("A", "b")));`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for CHECK (s IN ("a","b")) vs CHECK (s IN ("A","b"))`)
	}
}

// A single-quoted string literal differing only in case must also be
// detected — this was never folded, but is worth pinning explicitly
// alongside the double-quoted and bare-word cases above.
func TestCheckDrift_DetectsSingleQuotedDefaultCaseChange(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT 'active');"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT 'Active');"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for DEFAULT 'active' vs DEFAULT 'Active'")
	}
}

// SQLite only ever folds ASCII case when resolving an unquoted or
// backtick-/bracket-quoted identifier — strings.ToLower is Unicode-aware
// and would fold "Ä" onto "ä", which SQLite itself never does, hiding two
// genuinely different quoted identifiers as one.
func TestCheckDrift_DetectsNonASCIICaseDifferenceInQuotedIdentifier(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, `ä` TEXT);"},
	}
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY, `Ä` TEXT);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a quoted identifier differing only in non-ASCII case (`ä` vs `Ä`), which SQLite treats as two different identifiers")
	}
}

// A double-quoted identifier's content can itself contain a space or a
// word that reads like other tokens — "id integer" is one column
// literally named `id integer`. Joining its bare content with the same
// single-space separator used between every other token pair would make
// this compare equal to the two bare words id and integer, even though
// SQLite gives the two schemas a genuinely different column name and
// type.
func TestCheckDrift_DetectsQuotedIdentifierNotMergingWithBareWords(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `CREATE TABLE a ("id integer" TEXT);`},
	}
	schemaSQL := "CREATE TABLE a (id integer TEXT);"

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected between a quoted column "id integer" and the two bare words id integer`)
	}
}

// The other direction: a single double-quoted column name must compare
// equal to the exact same name written backtick-quoted — sqldef's own
// ADD COLUMN generator always backtick-quotes and lower-cases the column
// it adds (confirmed directly against schema.GenerateIdempotentDDLs), so a
// schema.sql column declared with double quotes must not report false
// drift against the migration that actually added it.
func TestCheckDrift_NoFalsePositiveForDoubleQuotedVsBacktickIdentifier(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, `key` TEXT);"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, "key" TEXT);`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf(`CheckDrift("key" double-quoted vs `+"`key`"+` backtick-quoted): %v, want no drift`, err)
	}
}

// The DEFAULT-bareword case-preservation exception must fire only after
// a bare, unquoted DEFAULT keyword — not after a column literally named
// "default" (quoted). Firing it after a quoted "default" too would keep
// the word right after it in its exact case unfolded, so a type keyword
// spelled differently only in case (`text` vs `TEXT`) — an ordinary
// formatting difference SQLite treats as identical — would be wrongly
// reported as drift.
func TestCheckDrift_NoFalsePositiveForCaseAfterQuotedDefaultColumnName(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `CREATE TABLE a ("default" text);`},
	}
	schemaSQL := `CREATE TABLE a ("default" TEXT);`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err != nil {
		t.Fatalf(`CheckDrift flagged text vs TEXT as drift after a quoted "default" column name: %v`, err)
	}
}

// The drift and dump queries'
// NOT LIKE 'sqlite\_%' ESCAPE '\' must exclude only SQLite's own internal
// sqlite_% tables, never an ordinary table whose name merely starts with
// "sqlite" followed by a literal underscore-shaped prefix that isn't a
// LIKE wildcard match by coincidence — "_" is itself a LIKE wildcard
// (matches any one character), so an unescaped pattern would also exclude
// a table like sqlitex_config, which does not start with "sqlite_" at
// all.
// A bare, unquoted keyword or literal must never compare equal to a
// double-quoted word with the same folded text — SQLite gives the two a
// genuinely different meaning (a keyword/literal vs. a string or
// identifier fallback), and canonicalize must keep them apart.

func TestCheckDrift_DetectsBareDefaultNullVsQuotedNullString(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT NULL);"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, status TEXT DEFAULT "null");`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for DEFAULT NULL vs DEFAULT "null"`)
	}
}

func TestCheckDrift_DetectsBareDefaultTrueVsQuotedTrueString(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER DEFAULT TRUE);"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, active INTEGER DEFAULT "true");`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for DEFAULT TRUE vs DEFAULT "true"`)
	}
}

func TestCheckDrift_DetectsBareCurrentTimestampVsQuotedString(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, created_at TEXT DEFAULT CURRENT_TIMESTAMP);"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, created_at TEXT DEFAULT "current_timestamp");`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for DEFAULT CURRENT_TIMESTAMP vs DEFAULT "current_timestamp"`)
	}
}

// The general case, unrelated to the DEFAULT-bareword exception: a bare
// NULL in a partial index's own WHERE clause is the NULL literal, not the
// double-quoted fallback string/identifier "null".
func TestCheckDrift_DetectsBarePartialIndexIsNullVsQuotedNullString(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, deleted_at TEXT); CREATE INDEX ai ON a(id) WHERE deleted_at IS NULL;"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, deleted_at TEXT); CREATE INDEX ai ON a(id) WHERE deleted_at IS "null";`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for a partial index's IS NULL vs IS "null"`)
	}
}

func TestCheckDrift_DetectsBareCurrentDateInCheckVsQuotedString(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, d TEXT CHECK (d <= current_date));"},
	}
	schemaSQL := `CREATE TABLE a (id INTEGER PRIMARY KEY, d TEXT CHECK (d <= "current_date"));`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected for a CHECK's bare current_date vs the quoted string "current_date"`)
	}
}

// SQLite does not resolve names inside a trigger's own body at creation
// time: a bare done referencing no column fails only when the trigger
// fires, while the double-quoted "done" instead falls back to a string
// literal — a real difference canonicalize must not fold away just because
// the two spellings would be equivalent inside a column definition.
func TestCheckDrift_DetectsBareVsQuotedWordInsideTriggerBody(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `
			CREATE TABLE a (id INTEGER PRIMARY KEY, state TEXT);
			CREATE TRIGGER a_touch AFTER UPDATE ON a BEGIN
				UPDATE a SET state = done WHERE id = NEW.id;
			END;
		`},
	}
	schemaSQL := `
		CREATE TABLE a (id INTEGER PRIMARY KEY, state TEXT);
		CREATE TRIGGER a_touch AFTER UPDATE ON a BEGIN
			UPDATE a SET state = "done" WHERE id = NEW.id;
		END;
	`

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal(`expected drift to be detected between a trigger body's bare "done" and the double-quoted "done"`)
	}
}

func TestCheckDrift_DoesNotExcludeTableNamedLikeSqliteInternalPrefix(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE sqlitex_config (id INTEGER PRIMARY KEY);"},
	}
	// schema.sql has moved on and no longer has this table — if the LIKE
	// pattern wrongly excluded it as if it were an internal sqlite_ table,
	// CheckDrift would report no drift here, which would be wrong.
	schemaSQL := ""

	if err := CheckDrift(context.Background(), schemaSQL, migrations); err == nil {
		t.Fatal("expected drift to be detected for a table named sqlitex_config that schema.sql no longer has — it must not be excluded as if it were an internal sqlite_ table")
	}
}

// CheckDrift executes schema.sql verbatim against a live connection to
// build its own comparison database, so a schema.sql containing an ATTACH
// must never reach that execution — CheckSchemaVocabulary runs first for
// exactly this reason. The target path lives inside t.TempDir() and is
// confirmed absent afterward, pinning the refusal-before-execution
// ordering directly rather than only that CheckDrift returns some error.
func TestCheckDrift_RefusesAttachInSchemaBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "attach-target.db")
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);\nATTACH DATABASE '" + target + "' AS x;"
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}

	if err := CheckDrift(context.Background(), schemaSQL, migrations); !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("CheckDrift on a schema.sql containing ATTACH = %v, want ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("ATTACH target %s exists after CheckDrift refused the schema containing it", target)
	}
}

// The same, for VACUUM INTO — SQLite's own way to write a whole database
// copy to an arbitrary path in one statement.
func TestCheckDrift_RefusesVacuumIntoInSchemaBeforeEverExecutingIt(t *testing.T) {
	target := filepath.Join(t.TempDir(), "vacuum-target.db")
	schemaSQL := "CREATE TABLE a (id INTEGER PRIMARY KEY);\nVACUUM INTO '" + target + "';"
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY);"},
	}

	if err := CheckDrift(context.Background(), schemaSQL, migrations); !errors.Is(err, ErrDisallowedStatement) {
		t.Fatalf("CheckDrift on a schema.sql containing VACUUM INTO = %v, want ErrDisallowedStatement", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("VACUUM INTO target %s exists after CheckDrift refused the schema containing it", target)
	}
}
