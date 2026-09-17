package store

import (
	"errors"
	"fmt"
	"strings"
)

// statementClass is what classifyStatement sorts a single top-level
// statement into. A deny-list can only ever name the spellings its authors
// already thought of; every SQLite statement that is not one of the shapes
// the migration generator or a documented rebuild step actually produces
// (classOrdinary, classContractOnly) is classDisallowed, including any
// shape this package's own parsing gives up on — there is no third,
// "unrecognized but probably fine" outcome.
type statementClass int

const (
	classDisallowed statementClass = iota
	classOrdinary
	classContractOnly
)

// classifyStatement decides what class stmt (one top-level statement, as
// splitStatements returns it) belongs to, from its own token sequence
// alone. It never looks at what migration the statement came from, or
// whether that migration is a registered contract step — CheckAllowedStatements
// combines this with contract registration; the runner's
// CheckStatementVocabulary uses it directly, because a migration already
// embedded in the binary was already reviewed against contracts when it
// was written.
func classifyStatement(stmt string) statementClass {
	tokens := tokenize(stmt)
	switch {
	case isWord(tokens, 0, "create"):
		return classifyCreate(tokens)
	case isWord(tokens, 0, "alter") && isWord(tokens, 1, "table"):
		return classifyAlterTable(tokens)
	case isWord(tokens, 0, "drop"):
		return classifyDrop(tokens)
	case isWord(tokens, 0, "insert"):
		return classifyInsert(tokens)
	default:
		return classDisallowed
	}
}

// isWordCI reports whether tokens[i] is a word token whose text, folded to
// ASCII lowercase, equals want. SQLite resolves a database (schema) name —
// the "temp" in temp.foo — case-insensitively regardless of how it was
// quoted, unlike a keyword, which can only ever function as a keyword
// unquoted in the first place; isWord's exact-text comparison is enough
// for a keyword position but not for a name SQLite itself case-folds.
func isWordCI(tokens []sqlToken, i int, want string) bool {
	return i < len(tokens) && tokens[i].kind == tokWord && asciiLower(tokens[i].text) == want
}

func isPunct(tokens []sqlToken, i int, text string) bool {
	return i < len(tokens) && tokens[i].kind == tokPunct && tokens[i].text == text
}

// nameIsTempQualified reports whether the name starting at tokens[i] is
// written as temp.<name> — SQLite puts the object in the temp schema
// exactly as if TEMP/TEMPORARY had been written on the CREATE statement
// itself, regardless of that keyword's presence.
func nameIsTempQualified(tokens []sqlToken, i int) bool {
	return isWordCI(tokens, i, "temp") && isPunct(tokens, i+1, ".") && i+2 < len(tokens) && tokens[i+2].kind == tokWord
}

func consumeIfNotExists(tokens []sqlToken, i int) int {
	if isWord(tokens, i, "if") && isWord(tokens, i+1, "not") && isWord(tokens, i+2, "exists") {
		return i + 3
	}
	return i
}

func consumeIfExists(tokens []sqlToken, i int) int {
	if isWord(tokens, i, "if") && isWord(tokens, i+1, "exists") {
		return i + 2
	}
	return i
}

// classifyCreate handles every CREATE statement shape. TEMP/TEMPORARY
// applies only to TABLE, TRIGGER and VIEW in SQLite's own grammar — there
// is no CREATE TEMP INDEX — so UNIQUE and TEMP are peeled off separately
// rather than as one shared "modifier" step.
func classifyCreate(tokens []sqlToken) statementClass {
	i := 1
	switch {
	case isWord(tokens, i, "table"):
		return classifyCreateTable(tokens, i+1, false)
	case isWord(tokens, i, "temp") || isWord(tokens, i, "temporary"):
		i++
		switch {
		case isWord(tokens, i, "table"):
			return classifyCreateTable(tokens, i+1, true)
		case isWord(tokens, i, "trigger"):
			return classifyCreateTrigger(tokens, i+1, true)
		case isWord(tokens, i, "view"):
			return classifyCreateView(tokens, i+1, true)
		default:
			return classDisallowed
		}
	case isWord(tokens, i, "unique") && isWord(tokens, i+1, "index"):
		return classifyCreateIndex(tokens, i+2)
	case isWord(tokens, i, "index"):
		return classifyCreateIndex(tokens, i+1)
	case isWord(tokens, i, "trigger"):
		return classifyCreateTrigger(tokens, i+1, false)
	case isWord(tokens, i, "view"):
		return classifyCreateView(tokens, i+1, false)
	default:
		return classDisallowed
	}
}

// classifyCreateTable allows CREATE TABLE [IF NOT EXISTS] name (...) —
// never TEMP, never a temp.-qualified name, and never CREATE TABLE ... AS
// SELECT: the generator only ever emits a column-definition list, and
// letting a SELECT populate a new table is a data operation that belongs
// to a Go transform, never to generated or hand-written migration SQL.
func classifyCreateTable(tokens []sqlToken, i int, isTemp bool) statementClass {
	if isTemp {
		return classDisallowed
	}
	i = consumeIfNotExists(tokens, i)
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return classDisallowed
	}
	if isWord(tokens, nameEnd, "as") {
		return classDisallowed
	}
	return classOrdinary
}

func classifyCreateIndex(tokens []sqlToken, i int) statementClass {
	i = consumeIfNotExists(tokens, i)
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	if qualifiedNameEnd(tokens, i) == i {
		return classDisallowed
	}
	return classOrdinary
}

// classifyCreateTrigger decides a trigger's class from its event clause's
// own table (legitimately full of INSERT/UPDATE/DELETE keywords: BEFORE
// INSERT, UPDATE OF col, ...) and everything after it — the FOR EACH ROW/
// WHEN clause and the BEGIN...END body itself. A trigger that changes data
// (INSERT/UPDATE/DELETE/REPLACE, Q60) fires again on every future write at
// ordinary runtime, not just once like a migration's own transform, so it
// is a data change and not merely a schema change — classContractOnly,
// exactly like the rebuild shapes below, requires a reviewed, registered
// contract step (D16). A trigger whose remainder contains only SELECT
// (including SELECT RAISE(...)) stays classOrdinary.
func classifyCreateTrigger(tokens []sqlToken, i int, isTemp bool) statementClass {
	if isTemp {
		return classDisallowed
	}
	i = consumeIfNotExists(tokens, i)
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return classDisallowed
	}
	afterTable, ok := triggerEventClauseEnd(tokens, nameEnd)
	if !ok {
		return classDisallowed
	}
	if triggerRemainderHasDML(tokens, afterTable) {
		return classContractOnly
	}
	return classOrdinary
}

// triggerEventClauseEnd returns the index just past the (possibly
// schema-qualified) table name in a CREATE TRIGGER statement's own fixed
// event clause — [BEFORE|AFTER|INSTEAD OF] {DELETE|INSERT|UPDATE
// [OF column, ...]} ON <table> — given i pointing right after the
// trigger's own name. This is the one part of the statement legitimately
// full of the words INSERT/UPDATE/DELETE (the event itself, an UPDATE OF
// column list); everything from here to the statement's end is instead
// scanned by triggerRemainderHasDML. ok is false when the header does not
// match this grammar at all, which fails closed to classDisallowed in the
// caller rather than guessing.
func triggerEventClauseEnd(tokens []sqlToken, i int) (int, bool) {
	switch {
	case isWord(tokens, i, "before"), isWord(tokens, i, "after"):
		i++
	case isWord(tokens, i, "instead") && isWord(tokens, i+1, "of"):
		i += 2
	}
	switch {
	case isWord(tokens, i, "delete"), isWord(tokens, i, "insert"):
		i++
	case isWord(tokens, i, "update"):
		i++
		if isWord(tokens, i, "of") {
			i++
			for {
				nameEnd := qualifiedNameEnd(tokens, i)
				if nameEnd == i {
					return 0, false
				}
				i = nameEnd
				if !isPunct(tokens, i, ",") {
					break
				}
				i++
			}
		}
	default:
		return 0, false
	}
	if !isWord(tokens, i, "on") {
		return 0, false
	}
	i++
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return 0, false
	}
	return nameEnd, true
}

// triggerRemainderHasDML reports whether any unquoted word token from index
// i to the statement's end is insert, update, delete or replace. SQLite has
// no way to nest a real INSERT/UPDATE/DELETE/REPLACE statement inside a FOR
// EACH ROW clause, a WHEN expr, or a body statement's own sub-select or CASE
// expression (confirmed directly: none of those positions accept a DML
// statement at all), so an unquoted occurrence of one of these words past
// the event clause's own table is always either a real top-level body
// statement's leading keyword or — the one accepted false positive — an
// unquoted replace used as an identifier — a column, table or alias name
// (SELECT replace FROM a) — or as the REPLACE(...) scalar function, in an
// otherwise SELECT-only trigger; either way it pushes the trigger to
// classContractOnly, the safe direction for that ambiguity (Q60), so this
// fails closed by construction rather than needing to locate BEGIN or
// track CASE...END nesting at all. A quoted
// token ("update", `delete`, [replace], 'insert') never counts — SQLite
// resolves it as a column, not a keyword, wherever it legally appears —
// which keeps a quoted column named "update"/"delete" from ever being
// mistaken for the keyword.
func triggerRemainderHasDML(tokens []sqlToken, i int) bool {
	for ; i < len(tokens); i++ {
		t := tokens[i]
		if t.kind != tokWord || t.quoted {
			continue
		}
		switch t.text {
		case "insert", "update", "delete", "replace":
			return true
		}
	}
	return false
}

// isCreateTriggerStatement reports whether stmt's own leading tokens are
// (non-TEMP) CREATE TRIGGER — the one classContractOnly shape
// CheckSchemaVocabulary accepts (see its own doc comment): unlike every
// other contract-only shape, a DML-bodied trigger is not a transitional
// rebuild step that disappears once the change is complete — it is itself
// the desired end state, so schema.sql has to be able to declare it for
// CheckDrift's replay-vs-schema.sql comparison to ever match once a
// migration registers one.
func isCreateTriggerStatement(tokens []sqlToken) bool {
	return isWord(tokens, 0, "create") && isWord(tokens, 1, "trigger")
}

func classifyCreateView(tokens []sqlToken, i int, isTemp bool) statementClass {
	if isTemp {
		return classDisallowed
	}
	i = consumeIfNotExists(tokens, i)
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	if qualifiedNameEnd(tokens, i) == i {
		return classDisallowed
	}
	return classOrdinary
}

// alterTableForm is SQLite's own fixed set of forms after
// "ALTER TABLE <name>" — nothing else is valid ALTER TABLE syntax at all.
type alterTableForm int

const (
	alterUnrecognized alterTableForm = iota
	alterAdd
	alterRenameColumn
	alterRenameTable
	alterDropColumn
)

// parseAlterTable reads the form starting at tokens[i], the token right
// after ALTER TABLE <name>. COLUMN is optional on both DROP and RENAME
// <old> TO <new>, and RENAME TO <new> (no source column at all) is the
// distinct whole-table-rename form.
func parseAlterTable(tokens []sqlToken, i int) alterTableForm {
	switch {
	case isWord(tokens, i, "add"):
		return alterAdd
	case isWord(tokens, i, "drop"):
		j := i + 1
		if isWord(tokens, j, "column") {
			j++
		}
		if qualifiedNameEnd(tokens, j) == j {
			return alterUnrecognized
		}
		return alterDropColumn
	case isWord(tokens, i, "rename"):
		j := i + 1
		if isWord(tokens, j, "to") {
			if qualifiedNameEnd(tokens, j+1) == j+1 {
				return alterUnrecognized
			}
			return alterRenameTable
		}
		if isWord(tokens, j, "column") {
			j++
		}
		colEnd := qualifiedNameEnd(tokens, j)
		if colEnd == j || !isWord(tokens, colEnd, "to") {
			return alterUnrecognized
		}
		if qualifiedNameEnd(tokens, colEnd+1) == colEnd+1 {
			return alterUnrecognized
		}
		return alterRenameColumn
	default:
		return alterUnrecognized
	}
}

// classifyAlterTable allows ADD [COLUMN] and RENAME [COLUMN] <a> TO <b> in
// an ordinary migration. RENAME TO <newtable> is also SQLite's documented
// rebuild sequence's own final step, and this classifier has no way to
// tell "the exchange-of-identity rename that finishes a rebuild" apart
// from "a plain, harmless table rename" — both are the exact same syntax —
// so it treats every whole-table rename as contract-only, the safe
// direction for an ambiguous case doc 01 §4's rebuild sequence already
// requires contract review for anyway. DROP [COLUMN] is likewise
// contract-only. Anything parseAlterTable cannot place into one of these
// four forms is refused outright, never passed through as either kind of
// allowed (fail closed).
func classifyAlterTable(tokens []sqlToken) statementClass {
	i := 2 // past ALTER TABLE
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return classDisallowed
	}
	switch parseAlterTable(tokens, nameEnd) {
	case alterAdd, alterRenameColumn:
		return classOrdinary
	case alterRenameTable, alterDropColumn:
		return classContractOnly
	default:
		return classDisallowed
	}
}

// classifyDrop allows DROP TABLE|INDEX|VIEW|TRIGGER [IF EXISTS] <name> only
// as a contract step — SQLite's documented rebuild sequence's own DROP of
// the old table, or the deliberate removal of an index/view/trigger no
// longer needed once its data or replacement is already in place.
func classifyDrop(tokens []sqlToken) statementClass {
	i := 1
	switch {
	case isWord(tokens, i, "table"), isWord(tokens, i, "index"), isWord(tokens, i, "view"), isWord(tokens, i, "trigger"):
		i++
	default:
		return classDisallowed
	}
	i = consumeIfExists(tokens, i)
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	if qualifiedNameEnd(tokens, i) == i {
		return classDisallowed
	}
	return classContractOnly
}

// classifyInsert allows exactly INSERT INTO <name> [(cols)] SELECT ... as a
// contract step — SQLite's documented rebuild sequence's own copy step,
// the one DML shape a migration may ever contain. INSERT ... VALUES, any
// OR-modifier (INSERT OR REPLACE, ...), and every other DML statement are
// disallowed outright: a data change that is not literally copying a
// rebuild's rows forward belongs in a Go data transform (doc 01 §4), never
// in migration SQL. A trailing upsert-clause (ON CONFLICT ... DO UPDATE/DO
// NOTHING) turns that same copy step into a conditional update of rows that
// already exist, which is exactly the data change this shape exists to
// rule out (Q60) — it is refused here rather than accepted as a copy.
func classifyInsert(tokens []sqlToken) statementClass {
	if !isWord(tokens, 1, "into") {
		return classDisallowed
	}
	i := 2
	if nameIsTempQualified(tokens, i) {
		return classDisallowed
	}
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return classDisallowed
	}
	i = nameEnd
	if isPunct(tokens, i, "(") {
		depth := 0
		for i < len(tokens) {
			switch {
			case isPunct(tokens, i, "("):
				depth++
			case isPunct(tokens, i, ")"):
				depth--
			}
			i++
			if depth == 0 {
				break
			}
		}
		if depth != 0 {
			return classDisallowed
		}
	}
	if !isWord(tokens, i, "select") {
		return classDisallowed
	}
	if hasTopLevelOnConflict(tokens, i) {
		return classDisallowed
	}
	return classContractOnly
}

// hasTopLevelOnConflict reports whether tokens[from:] — the SELECT that
// follows a contract step's INSERT INTO ... SELECT — carries an upsert-
// clause (ON CONFLICT) at paren depth 0. Depth tracking, rather than a raw
// text scan, is what keeps this from matching an ON CONFLICT that could
// only ever appear as string data inside the select's own expressions:
// SQLite's grammar has nowhere inside a SELECT for an INSERT's own
// upsert-clause to legally nest, so a real one always sits at depth 0,
// right where the select statement itself ends.
func hasTopLevelOnConflict(tokens []sqlToken, from int) bool {
	depth := 0
	for i := from; i < len(tokens); i++ {
		switch {
		case isPunct(tokens, i, "("):
			depth++
		case isPunct(tokens, i, ")"):
			depth--
		case depth == 0 && isWord(tokens, i, "on") && isWord(tokens, i+1, "conflict"):
			return true
		}
	}
	return false
}

// describeStatement names a refused statement for an error message,
// without echoing arbitrary user data back verbatim: only its own leading
// keywords, the same ones classifyStatement dispatches on.
func describeStatement(stmt string) string {
	words := leadingWords(stmt, 4)
	if len(words) == 0 {
		return "an unparseable statement"
	}
	return strings.ToUpper(strings.Join(words, " "))
}

// ErrDisallowedStatement is wrapped into every error CheckAllowedStatements,
// CheckStatementVocabulary and CheckSchemaVocabulary return for a
// classDisallowed statement. Testing for this exact sentinel with
// errors.Is, rather than any non-nil error, is what makes a mutation that
// wrongly classifies one of these statements as allowed distinguishable
// from something else — SQLite itself refusing the same statement text for
// an unrelated reason (a missing table, a column-count mismatch) — once
// the classifier itself no longer refuses it at all.
var ErrDisallowedStatement = errors.New("statement outside the recognized migration vocabulary")

// CheckAllowedStatements is the authoritative gate `make db-check` runs:
// every top-level statement in every migration must be classOrdinary, or
// classContractOnly in a migration registered in contracts. Anything
// classDisallowed is refused regardless of registration — EXPLAIN, every
// PRAGMA, transaction control, VACUUM, ATTACH/DETACH, REINDEX, ANALYZE,
// any DML besides a contract step's own INSERT ... SELECT, every DROP
// statement's own bare SELECT/DDL variants this classifier didn't
// recognize, and any statement shape this package cannot fully parse.
func CheckAllowedStatements(migrations []Migration, contracts map[string]bool) error {
	for _, m := range migrations {
		isContractStep := contracts[m.Filename]
		for _, stmt := range splitStatements(m.SQL) {
			switch classifyStatement(stmt) {
			case classOrdinary:
				continue
			case classContractOnly:
				if isContractStep {
					continue
				}
				return fmt.Errorf("migration %q contains %s, which is only allowed in a registered contract step of an expand/contract change (D16) — register it in internal/store/migrations/%s if this is intended", m.Filename, describeStatement(stmt), ContractsFile)
			default:
				return fmt.Errorf("migration %q contains a statement outside the recognized migration vocabulary (%s) — refusing to apply it: %w", m.Filename, describeStatement(stmt), ErrDisallowedStatement)
			}
		}
	}
	return nil
}

// CheckStatementVocabulary is the runner's own defense in depth, run
// before Apply opens its transaction: it refuses a classDisallowed
// statement exactly like CheckAllowedStatements, but never re-checks
// contract registration, because a migration already embedded in this
// binary already passed that review, on this same classifier, when it was
// generated or hand-written — the runner's job is to make sure nothing
// slipped past that review at all, not to re-derive which migrations were
// approved as contract steps from a contracts file this binary does not
// ship.
func CheckStatementVocabulary(migrations []Migration) error {
	for _, m := range migrations {
		for _, stmt := range splitStatements(m.SQL) {
			if classifyStatement(stmt) == classDisallowed {
				return fmt.Errorf("migration %q contains a statement outside the recognized migration vocabulary (%s) — refusing to apply it: %w", m.Filename, describeStatement(stmt), ErrDisallowedStatement)
			}
		}
	}
	return nil
}

// CheckSchemaVocabulary refuses any statement in schema.sql outside the
// ordinary migration vocabulary (classOrdinary) — CREATE TABLE/INDEX/VIEW/
// TRIGGER and ALTER TABLE ADD/RENAME COLUMN, never TEMP, never DROP, never
// DML — with exactly one classContractOnly exception: a CREATE TRIGGER
// whose body carries DML (isCreateTriggerStatement). Every other
// contract-only shape (a DROP, a table rebuild, a contract step's own
// INSERT ... SELECT) is a transitional step of an expand/contract change
// that leaves no trace in the desired end state once it's done, so none of
// them ever belongs in schema.sql — those still refuse exactly like
// classDisallowed. A DML-bodied trigger is different: once a registered
// contract step creates one, the trigger itself *is* part of the desired
// end state (D16 only requires review of the change that adds it, not that
// the object stop existing), so schema.sql has to be able to declare it —
// otherwise CheckDrift's replay-vs-schema.sql comparison could never agree
// once such a trigger exists at all. CheckDrift executes schema.sql
// verbatim to build its own comparison database (drift.go); this is what
// has to run first, so that execution never reaches an ATTACH, a VACUUM
// INTO, a PRAGMA or any other statement able to touch the filesystem or a
// connection's own settings.
func CheckSchemaVocabulary(schemaSQL string) error {
	for _, stmt := range splitStatements(schemaSQL) {
		class := classifyStatement(stmt)
		if class == classOrdinary {
			continue
		}
		if class == classContractOnly && isCreateTriggerStatement(tokenize(stmt)) {
			continue
		}
		return fmt.Errorf("schema.sql contains a statement outside the recognized schema vocabulary (%s) — refusing to use it: %w", describeStatement(stmt), ErrDisallowedStatement)
	}
	return nil
}
