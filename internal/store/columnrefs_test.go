package store

import "testing"

// The gap Q60 records: sqldef's generator silently drops a new column's
// own REFERENCES clause. SchemaColumnReferences is what db-migration reads
// that clause back from, so it must find it, keyed by the exact table and
// column it belongs to.
func TestSchemaColumnReferences_FindsInlineReference(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id));")

	ref, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]
	if !ok {
		t.Fatalf("SchemaColumnReferences did not find a.parent_id's own REFERENCES clause: %+v", refs)
	}
	if ref.Clause != "references parent(id)" {
		t.Fatalf("Clause = %q, want %q", ref.Clause, "references parent(id)")
	}
	if !ref.DefaultIsNull {
		t.Fatal("a.parent_id has no DEFAULT clause at all, so DefaultIsNull should be true")
	}
}

// ON DELETE/ON UPDATE actions, in either order, must round-trip into the
// clause exactly, since dbmigration splices this text verbatim onto a
// generated ADD COLUMN statement.
func TestSchemaColumnReferences_CarriesOnDeleteOnUpdateActions(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) ON DELETE CASCADE ON UPDATE SET NULL);")

	ref, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]
	if !ok {
		t.Fatal("SchemaColumnReferences did not find a.parent_id")
	}
	want := "references parent(id) on delete cascade on update set null"
	if ref.Clause != want {
		t.Fatalf("Clause = %q, want %q", ref.Clause, want)
	}
}

// A REFERENCES clause with an explicit DEFAULT NULL is exactly as safe to
// restore onto ADD COLUMN as no DEFAULT at all.
func TestSchemaColumnReferences_DefaultNullIsSafe(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFAULT NULL);")

	ref, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]
	if !ok {
		t.Fatal("SchemaColumnReferences did not find a.parent_id")
	}
	if !ref.DefaultIsNull {
		t.Fatal("an explicit DEFAULT NULL should report DefaultIsNull = true")
	}
}

// SQLite refuses ALTER TABLE ... ADD COLUMN ... REFERENCES ... whenever
// foreign key constraints are enabled and the new column's own default is
// anything other than NULL — a non-NULL DEFAULT alongside REFERENCES must
// report DefaultIsNull = false so dbmigration never emits that shape.
func TestSchemaColumnReferences_NonNullDefaultIsUnsafe(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFAULT 0);")

	ref, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]
	if !ok {
		t.Fatal("SchemaColumnReferences did not find a.parent_id")
	}
	if ref.DefaultIsNull {
		t.Fatal("a DEFAULT 0 alongside REFERENCES should report DefaultIsNull = false")
	}
}

// A column with no REFERENCES clause at all must not appear in the map —
// dbmigration relies on a missing entry to mean "nothing to restore",
// leaving a generated ADD COLUMN statement it doesn't recognize untouched.
func TestSchemaColumnReferences_OmitsColumnsWithoutReferences(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE a (id INTEGER PRIMARY KEY, name TEXT);")

	if _, ok := refs[ColumnKey{Table: "a", Column: "name"}]; ok {
		t.Fatal("a.name has no REFERENCES clause and should not appear in the map")
	}
}

// A table-level FOREIGN KEY constraint is not an inline column REFERENCES
// clause — ADD COLUMN can never add a table-level constraint at all, and
// classifying its own leading "FOREIGN" as if it were a column named
// "foreign" would misattribute this to a wrong, invented column.
func TestSchemaColumnReferences_IgnoresTableLevelForeignKeyConstraint(t *testing.T) {
	refs := SchemaColumnReferences("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, parent_id INTEGER, FOREIGN KEY (parent_id) REFERENCES parent(id));")

	if _, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]; ok {
		t.Fatal("a table-level FOREIGN KEY constraint must not be read as parent_id's own inline REFERENCES clause")
	}
	if len(refs) != 0 {
		t.Fatalf("expected no inline REFERENCES clauses at all, got %+v", refs)
	}
}

// A CHECK expression that happens to contain a comma, and a table-level
// PRIMARY KEY/UNIQUE constraint, must not be split as if they were their
// own column definitions, and must not be mistaken for a REFERENCES
// clause either.
func TestSchemaColumnReferences_HandlesCommasInsideCheckAndTableConstraints(t *testing.T) {
	refs := SchemaColumnReferences(`CREATE TABLE parent (id INTEGER PRIMARY KEY);
		CREATE TABLE a (
			id INTEGER,
			qty INTEGER CHECK (qty IN (1, 2, 3)),
			parent_id INTEGER REFERENCES parent(id),
			PRIMARY KEY (id, parent_id)
		);`)

	if len(refs) != 1 {
		t.Fatalf("expected exactly one REFERENCES clause, got %+v", refs)
	}
	if _, ok := refs[ColumnKey{Table: "a", Column: "parent_id"}]; !ok {
		t.Fatalf("expected a.parent_id's own REFERENCES clause, got %+v", refs)
	}
}

func TestAddColumnTarget_ParsesGeneratedStatement(t *testing.T) {
	table, column, ok := AddColumnTarget("ALTER TABLE `a` ADD COLUMN `parent_id` integer")
	if !ok {
		t.Fatal("AddColumnTarget did not recognize a plain generated ADD COLUMN statement")
	}
	if table != "a" || column != "parent_id" {
		t.Fatalf("AddColumnTarget = (%q, %q), want (\"a\", \"parent_id\")", table, column)
	}
}

func TestAddColumnTarget_RefusesOtherAlterTableForms(t *testing.T) {
	for _, stmt := range []string{
		"ALTER TABLE `a` RENAME COLUMN `b` TO `c`",
		"ALTER TABLE `a` RENAME TO `b`",
		"ALTER TABLE `a` DROP COLUMN `b`",
		"CREATE TABLE a (id INTEGER)",
	} {
		if _, _, ok := AddColumnTarget(stmt); ok {
			t.Fatalf("AddColumnTarget(%q) = ok, want false", stmt)
		}
	}
}

// AddColumnTarget and SchemaColumnReferences fold names the same way, so a
// generated statement's own backtick-quoted names look up cleanly against
// a schema.sql that declared the column with a different quoting style
// (double-quoted, or a different case).
func TestAddColumnTarget_FoldingMatchesSchemaColumnReferences(t *testing.T) {
	refs := SchemaColumnReferences(`CREATE TABLE "Parent" (id INTEGER PRIMARY KEY); CREATE TABLE a (id INTEGER PRIMARY KEY, "Parent_ID" INTEGER REFERENCES "Parent"(id));`)

	table, column, ok := AddColumnTarget("ALTER TABLE `a` ADD COLUMN `parent_id` integer")
	if !ok {
		t.Fatal("AddColumnTarget did not recognize the generated statement")
	}
	if _, ok := refs[ColumnKey{Table: table, Column: column}]; !ok {
		t.Fatalf("lookup with AddColumnTarget's own (%q, %q) did not find schema.sql's differently-quoted column: %+v", table, column, refs)
	}
}
