package store

import "testing"

func TestDestructive_DropTable(t *testing.T) {
	reasons := Destructive(`DROP TABLE "old_shares";`)
	if !containsPrefix(reasons, "DROP TABLE") {
		t.Fatalf("Destructive(DROP TABLE) = %v, want it to include DROP TABLE", reasons)
	}
}

func TestDestructive_DropColumn(t *testing.T) {
	reasons := Destructive("ALTER TABLE `schema_info` DROP COLUMN `legacy`;")
	if !containsPrefix(reasons, "DROP COLUMN") {
		t.Fatalf("Destructive(DROP COLUMN) = %v, want it to include DROP COLUMN", reasons)
	}
}

// SQLite has no ALTER COLUMN. Per Q60's findings, sqldef's SQLite
// generator never emits one either — the only way a column's type or
// constraints actually change is the rebuild sequence SQLite's own docs
// prescribe: create a new table, copy the data across, drop the old
// table, rename the new one into place. That sequence is what this test
// represents "a column type change" with.
func TestDestructive_TableRebuild(t *testing.T) {
	sql := `
CREATE TABLE schema_info_new_ (id INTEGER PRIMARY KEY, installation_id TEXT NOT NULL, created_at INTEGER NOT NULL);
INSERT INTO schema_info_new_ SELECT id, installation_id, CAST(created_at AS INTEGER) FROM schema_info;
DROP TABLE schema_info;
ALTER TABLE schema_info_new_ RENAME TO schema_info;
`
	reasons := Destructive(sql)
	if !containsPrefix(reasons, "table rebuild") {
		t.Fatalf("Destructive(rebuild) = %v, want it to include a table rebuild reason", reasons)
	}
	if !containsPrefix(reasons, "DROP TABLE") {
		t.Fatalf("Destructive(rebuild) = %v, want it to also flag the DROP TABLE the rebuild contains", reasons)
	}
}

func TestDestructive_NonDestructive(t *testing.T) {
	reasons := Destructive("ALTER TABLE `schema_info` ADD COLUMN `note` TEXT;")
	if len(reasons) != 0 {
		t.Fatalf("Destructive(ADD COLUMN) = %v, want none", reasons)
	}
}

// SQLite's grammar makes COLUMN optional on DROP — "ALTER TABLE t DROP c"
// parses (confirmed directly against sqldefdb.NewParser(...).Parse), so a
// scan matching only "DROP COLUMN" would miss it.
func TestDestructive_DropColumnWithoutColumnKeyword(t *testing.T) {
	reasons := Destructive("ALTER TABLE a DROP c;")
	if !containsPrefix(reasons, "DROP COLUMN") {
		t.Fatalf("Destructive(DROP c, no COLUMN keyword) = %v, want it to include DROP COLUMN", reasons)
	}
}

// The same gap, with the dropped column itself quoted.
func TestDestructive_DropQuotedColumnWithoutColumnKeyword(t *testing.T) {
	reasons := Destructive(`ALTER TABLE a DROP "c";`)
	if !containsPrefix(reasons, "DROP COLUMN") {
		t.Fatalf(`Destructive(DROP "c") = %v, want it to include DROP COLUMN`, reasons)
	}
}

// A comment between two keywords must not defeat a scan that assumes a
// keyword gap is only ever whitespace.
func TestDestructive_DropTableWithCommentBetweenKeywords(t *testing.T) {
	reasons := Destructive("DROP/**/TABLE a;")
	if !containsPrefix(reasons, "DROP TABLE") {
		t.Fatalf("Destructive(DROP/**/TABLE) = %v, want it to include DROP TABLE", reasons)
	}
}

// A quoted, multi-word table name must not defeat the RENAME TO scan.
func TestDestructive_RenameToWithQuotedMultiWordTableName(t *testing.T) {
	reasons := Destructive(`ALTER TABLE "a b" RENAME TO c;`)
	if !containsPrefix(reasons, "table rebuild") {
		t.Fatalf(`Destructive(ALTER TABLE "a b" RENAME TO c) = %v, want it to include a table rebuild reason`, reasons)
	}
}

// A schema-qualified table name (main.t) must not confuse the DROP COLUMN
// scan either.
func TestDestructive_DropColumnWithSchemaQualifiedTable(t *testing.T) {
	reasons := Destructive("ALTER TABLE main.a DROP COLUMN b;")
	if !containsPrefix(reasons, "DROP COLUMN") {
		t.Fatalf("Destructive(schema-qualified DROP COLUMN) = %v, want it to include DROP COLUMN", reasons)
	}
}

// An unusual table name must not let a DROP COLUMN slip past unclassified —
// each of these is a table name SQLite itself accepts.
func TestDestructive_DropColumnWithUnusualTableName(t *testing.T) {
	cases := map[string]string{
		"double-quoted reserved word":  `ALTER TABLE "default" DROP COLUMN x;`,
		"bracket-quoted reserved word": `ALTER TABLE [default] DROP COLUMN x;`,
		"dollar sign":                  "ALTER TABLE t$1 DROP COLUMN x;",
		"non-ASCII letter":             "ALTER TABLE tä DROP COLUMN x;",
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			reasons := Destructive(sql)
			if !containsPrefix(reasons, "DROP COLUMN") {
				t.Fatalf("Destructive(%q) = %v, want it to include DROP COLUMN", sql, reasons)
			}
		})
	}
}

// RENAME COLUMN does not lose data — it must never be classified the same
// way ALTER TABLE ... RENAME TO (a whole-table rename, part of the
// documented rebuild sequence) is.
func TestDestructive_RenameColumnIsNotFlagged(t *testing.T) {
	reasons := Destructive("ALTER TABLE a RENAME COLUMN b TO c;")
	if len(reasons) != 0 {
		t.Fatalf("Destructive(RENAME COLUMN) = %v, want none", reasons)
	}
}

// The COLUMN keyword is optional on RENAME too, exactly as it is on DROP.
func TestDestructive_RenameColumnWithoutColumnKeywordIsNotFlagged(t *testing.T) {
	reasons := Destructive("ALTER TABLE a RENAME b TO c;")
	if len(reasons) != 0 {
		t.Fatalf("Destructive(RENAME b TO c) = %v, want none", reasons)
	}
}

// An ALTER TABLE shape parseAlterTable cannot place into one of SQLite's
// own fixed forms is classDisallowed, not classContractOnly — it is never
// permitted to run at all, so Destructive (the contract-registration
// reporting layer) does not report a reason for it; CheckAllowedStatements
// is what refuses it outright (see allowlist_test.go).
func TestDestructive_UnrecognizedAlterTableShapeIsNotReported(t *testing.T) {
	reasons := Destructive("ALTER TABLE a SOMETHING_UNRECOGNIZED;")
	if len(reasons) != 0 {
		t.Fatalf("Destructive(unrecognized ALTER TABLE) = %v, want none — CheckAllowedStatements refuses this outright instead", reasons)
	}
}

// A DML-bodied trigger is reported by Destructive under its own reason —
// the audit trail's own name for the shape (Q60), distinct from every
// other contract-only kind.
func TestDestructive_TriggerWithDMLBody(t *testing.T) {
	reasons := Destructive("CREATE TRIGGER a_touch AFTER INSERT ON a BEGIN UPDATE a SET v = 1 WHERE id = NEW.id; END;")
	if !containsPrefix(reasons, "CREATE TRIGGER with a DML body") {
		t.Fatalf("Destructive(DML-bodied trigger) = %v, want it to include the trigger_dml reason", reasons)
	}
}

// A trigger whose body is SELECT-only changes nothing, so it must never be
// reported as destructive.
func TestDestructive_TriggerWithSelectOnlyBodyIsNotFlagged(t *testing.T) {
	reasons := Destructive("CREATE TRIGGER a_touch AFTER INSERT ON a BEGIN SELECT 1; END;")
	if len(reasons) != 0 {
		t.Fatalf("Destructive(SELECT-only trigger) = %v, want none", reasons)
	}
}

func TestCheckSafety_FailsUnregisteredDMLBodiedTrigger(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER PRIMARY KEY, v INTEGER);"},
		{Version: 2, Filename: "0002_trigger.sql", SQL: "CREATE TRIGGER a_touch AFTER INSERT ON a BEGIN UPDATE a SET v = 1 WHERE id = NEW.id; END;"},
	}
	if err := CheckSafety(migrations, map[string]bool{}); err == nil {
		t.Fatal("expected an error for an unregistered DML-bodied trigger")
	}
	if err := CheckSafety(migrations, map[string]bool{"0002_trigger.sql": true}); err != nil {
		t.Fatalf("CheckSafety with the trigger's migration registered as a contract step: %v", err)
	}
}

// The same DML-bodied trigger, with its DML target column quoted "end",
// is reported by Destructive and refused by CheckSafety unregistered
// exactly like the plain-column case above — a quoted end/case/begin
// column must never hide a trigger's own DML from this reporting layer,
// since it is driven by the same classifyStatement (Q60).
func TestDestructive_TriggerWithDMLBodyAndQuotedEndColumn(t *testing.T) {
	reasons := Destructive(`CREATE TRIGGER a_touch AFTER INSERT ON a BEGIN UPDATE a SET "end" = 1 WHERE id = NEW.id; END;`)
	if !containsPrefix(reasons, "CREATE TRIGGER with a DML body") {
		t.Fatalf("Destructive(DML-bodied trigger, quoted end column) = %v, want it to include the trigger_dml reason", reasons)
	}
}

func TestCheckSafety_FailsUnregisteredDMLBodiedTriggerWithQuotedEndColumn(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: `CREATE TABLE a (id INTEGER PRIMARY KEY, "end" INTEGER);`},
		{Version: 2, Filename: "0002_trigger.sql", SQL: `CREATE TRIGGER a_touch AFTER INSERT ON a BEGIN UPDATE a SET "end" = 1 WHERE id = NEW.id; END;`},
	}
	if err := CheckSafety(migrations, map[string]bool{}); err == nil {
		t.Fatal("expected an error for an unregistered DML-bodied trigger with a quoted end column")
	}
	if err := CheckSafety(migrations, map[string]bool{"0002_trigger.sql": true}); err != nil {
		t.Fatalf("CheckSafety with the trigger's migration registered as a contract step: %v", err)
	}
}

func TestCheckSafety_FailsUnregistered(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"},
		{Version: 2, Filename: "0002_drop_a.sql", SQL: "DROP TABLE a;"},
	}
	if err := CheckSafety(migrations, map[string]bool{}); err == nil {
		t.Fatal("expected an error for an unregistered destructive migration")
	}
}

func TestCheckSafety_PassesWhenRegistered(t *testing.T) {
	migrations := []Migration{
		{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"},
		{Version: 2, Filename: "0002_drop_a.sql", SQL: "DROP TABLE a;"},
	}
	contracts := map[string]bool{"0002_drop_a.sql": true}
	if err := CheckSafety(migrations, contracts); err != nil {
		t.Fatalf("CheckSafety with a registered contract step: %v", err)
	}
}

func TestReadContracts_IgnoresBlankAndComments(t *testing.T) {
	data := []byte("# comment\n\n0002_drop_a.sql  reviewed by mdg\n")
	contracts := ReadContracts(data)
	if !contracts["0002_drop_a.sql"] {
		t.Fatalf("ReadContracts(%q) = %v, want 0002_drop_a.sql registered", data, contracts)
	}
	if len(contracts) != 1 {
		t.Fatalf("len(contracts) = %d, want 1", len(contracts))
	}
}

// A second trigger, or an ordinary statement, further down the same file
// must still be visible to Destructive's own leading-keyword scan — if a
// trigger's real closing END were ever lost (mistaken for closing only its
// own CASE...END, or never recognized as closing anything at all),
// splitStatements would swallow everything after it, including a real
// ALTER TABLE ... DROP, into one statement.
func TestSplitStatements_StatementsAfterTriggerWithCaseEndAreStillVisible(t *testing.T) {
	sql := `
		CREATE TRIGGER a_classify AFTER INSERT ON a BEGIN
			SELECT CASE WHEN NEW.v > 0 THEN 1 ELSE 0 END;
		END;
		COMMIT;
		ALTER TABLE t DROP c;
	`
	if reasons := Destructive(sql); !containsPrefix(reasons, "DROP COLUMN") {
		t.Fatalf("Destructive(%q) = %v, want the ALTER TABLE ... DROP after the trigger flagged", sql, reasons)
	}
}

func containsPrefix(reasons []string, prefix string) bool {
	for _, r := range reasons {
		if len(r) >= len(prefix) && r[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}
