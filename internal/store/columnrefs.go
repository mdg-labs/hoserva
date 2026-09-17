package store

import "strings"

// ColumnKey names one column of one table, both folded to ASCII lowercase
// the same way SQLite itself resolves an unquoted name (asciiLower) —
// db-migration's own lookup from a generated ALTER TABLE ... ADD COLUMN
// statement (AddColumnTarget) into schema.sql's own column definitions
// (SchemaColumnReferences) needs the two sides to compare equal regardless
// of either one's original quoting or case.
type ColumnKey struct {
	Table  string
	Column string
}

// ColumnReference is one column's own inline REFERENCES clause, as
// declared in a CREATE TABLE statement in schema.sql.
type ColumnReference struct {
	// Clause is the foreign-key-clause's own text — REFERENCES, the
	// referenced table and column list, and any ON DELETE/ON UPDATE/MATCH/
	// DEFERRABLE that follows it — rendered from its own tokens so it can
	// be appended straight onto a generated ADD COLUMN statement.
	Clause string
	// DefaultIsNull is true when the column has no DEFAULT clause at all,
	// or an explicit DEFAULT NULL — the one shape SQLite always accepts on
	// ALTER TABLE ... ADD COLUMN together with a REFERENCES clause,
	// regardless of whether foreign key constraints are enabled. SQLite
	// refuses ADD COLUMN ... REFERENCES ... outright, when foreign key
	// constraints are enabled at prepare time, for any other DEFAULT.
	DefaultIsNull bool
}

// SchemaColumnReferences scans every CREATE TABLE statement in schemaSQL
// for a column's own inline REFERENCES clause — the clause sqldef's SQLite
// generator silently drops from a generated ADD COLUMN statement (Q60) —
// keyed by the table and column it belongs to. It reuses sqltoken.go's
// tokenizer and splitStatements, the same way allowlist.go's classifier
// does, rather than pattern-matching schema.sql's raw text.
func SchemaColumnReferences(schemaSQL string) map[ColumnKey]ColumnReference {
	out := map[ColumnKey]ColumnReference{}
	for _, stmt := range splitStatements(schemaSQL) {
		table, columns, ok := createTableColumns(stmt)
		if !ok {
			continue
		}
		for _, item := range columns {
			column, ref, ok := parseColumnDef(item)
			if !ok {
				continue
			}
			out[ColumnKey{Table: table, Column: column}] = ref
		}
	}
	return out
}

// createTableColumns reports stmt's own table name and its top-level
// column/table-constraint list items, split on every comma that sits
// outside a nested parenthesis (a CHECK expression, a foreign-key clause's
// own column list, ...). ok is false for anything other than a plain
// CREATE [IF NOT EXISTS] TABLE <name> ( ... ) [table-options] — including
// CREATE TEMP TABLE and CREATE TABLE ... AS SELECT, neither of which
// allowlist.go's own classifier ever lets a migration or schema.sql
// contain in the first place.
func createTableColumns(stmt string) (table string, columns [][]sqlToken, ok bool) {
	tokens := tokenize(stmt)
	i := 0
	if !isWord(tokens, i, "create") {
		return "", nil, false
	}
	i++
	if !isWord(tokens, i, "table") {
		return "", nil, false
	}
	i++
	i = consumeIfNotExists(tokens, i)
	nameEnd := qualifiedNameEnd(tokens, i)
	if nameEnd == i {
		return "", nil, false
	}
	table = asciiLower(tokens[nameEnd-1].text)
	i = nameEnd
	if !isPunct(tokens, i, "(") {
		return "", nil, false
	}
	depth := 0
	closeIdx := -1
	for j := i; j < len(tokens); j++ {
		switch {
		case isPunct(tokens, j, "("):
			depth++
		case isPunct(tokens, j, ")"):
			depth--
		}
		if depth == 0 {
			closeIdx = j
			break
		}
	}
	if closeIdx == -1 {
		return "", nil, false
	}
	return table, splitTopLevelCommaList(tokens[i+1 : closeIdx]), true
}

// splitTopLevelCommaList splits tokens on every "," at paren depth 0 —
// SQLite's own column/table-constraint list separator; a comma inside a
// nested "(...)" (a CHECK expression, a REFERENCES column list, ...) is
// never a separator between list items.
func splitTopLevelCommaList(tokens []sqlToken) [][]sqlToken {
	var items [][]sqlToken
	depth := 0
	start := 0
	for i, t := range tokens {
		switch {
		case t.kind == tokPunct && t.text == "(":
			depth++
		case t.kind == tokPunct && t.text == ")":
			depth--
		case t.kind == tokPunct && t.text == "," && depth == 0:
			items = append(items, tokens[start:i])
			start = i + 1
		}
	}
	items = append(items, tokens[start:])
	return items
}

// tableConstraintKeywords are the words that open a table-level constraint
// (optionally after its own "CONSTRAINT name" prefix) rather than a column
// definition. A column-def's own per-column "CONSTRAINT name" prefix
// (SQLite's column-constraint grammar) never appears as a list item's
// first token — a column-def always starts with the column's own name —
// so seeing one of these words, or "constraint" immediately followed by
// one, at item[0] always means a table-level constraint.
var tableConstraintKeywords = map[string]bool{
	"primary": true, "unique": true, "check": true, "foreign": true,
}

func isTableConstraintItem(item []sqlToken) bool {
	if len(item) == 0 || item[0].kind != tokWord {
		return false
	}
	if tableConstraintKeywords[item[0].text] {
		return true
	}
	return item[0].text == "constraint" && len(item) >= 3 && item[2].kind == tokWord && tableConstraintKeywords[item[2].text]
}

// parseColumnDef reads one column definition item (as splitTopLevelCommaList
// returns it) for its own name, whether it carries an inline REFERENCES
// clause, and whether its own DEFAULT (if any) is NULL. ok is false for a
// table-level constraint item, or a column definition with no REFERENCES
// clause of its own at all.
func parseColumnDef(item []sqlToken) (column string, ref ColumnReference, ok bool) {
	if isTableConstraintItem(item) || len(item) == 0 || item[0].kind != tokWord {
		return "", ColumnReference{}, false
	}
	column = asciiLower(item[0].text)

	refStart, refEnd := -1, -1
	defaultIsNull := true
	depth := 0
	for i := 1; i < len(item); i++ {
		t := item[i]
		switch {
		case t.kind == tokPunct && t.text == "(":
			depth++
		case t.kind == tokPunct && t.text == ")":
			depth--
		case depth == 0 && t.kind == tokWord && !t.quoted && t.text == "references" && refStart == -1:
			refStart = i
			refEnd = foreignKeyClauseEnd(item, i)
			i = refEnd - 1
		case depth == 0 && t.kind == tokWord && !t.quoted && t.text == "default":
			if i+1 < len(item) && item[i+1].kind == tokWord && !item[i+1].quoted && item[i+1].text == "null" {
				defaultIsNull = true
			} else {
				defaultIsNull = false
			}
		}
	}
	if refStart == -1 {
		return column, ColumnReference{}, false
	}
	return column, ColumnReference{Clause: renderTokens(item[refStart:refEnd]), DefaultIsNull: defaultIsNull}, true
}

// foreignKeyClauseEnd returns the index just past the foreign-key-clause
// starting at item[i] (item[i] is the REFERENCES keyword itself) — SQLite's
// own grammar for it: REFERENCES <table> [(<columns>)], then zero or more
// of ON (DELETE|UPDATE) (SET NULL|SET DEFAULT|CASCADE|RESTRICT|NO ACTION)
// or MATCH <name>, then an optional [NOT] DEFERRABLE
// [INITIALLY (DEFERRED|IMMEDIATE)]. Any token this doesn't recognize as
// one of these ends the clause where it stands, leaving it for whatever
// column-constraint (or the next list item) actually is next.
func foreignKeyClauseEnd(item []sqlToken, i int) int {
	j := i + 1
	j = qualifiedNameEnd(item, j)
	if j < len(item) && item[j].kind == tokPunct && item[j].text == "(" {
		depth := 0
		for j < len(item) {
			switch {
			case item[j].kind == tokPunct && item[j].text == "(":
				depth++
			case item[j].kind == tokPunct && item[j].text == ")":
				depth--
			}
			j++
			if depth == 0 {
				break
			}
		}
	}
	for {
		switch {
		case isWord(item, j, "on") && (isWord(item, j+1, "delete") || isWord(item, j+1, "update")):
			j += 2
			switch {
			case isWord(item, j, "set") && (isWord(item, j+1, "null") || isWord(item, j+1, "default")):
				j += 2
			case isWord(item, j, "cascade"), isWord(item, j, "restrict"):
				j++
			case isWord(item, j, "no") && isWord(item, j+1, "action"):
				j += 2
			default:
				return j
			}
			continue
		case isWord(item, j, "match") && j+1 < len(item) && item[j+1].kind == tokWord:
			j += 2
			continue
		}
		break
	}
	start := j
	if isWord(item, j, "not") && isWord(item, j+1, "deferrable") {
		j += 2
	} else if isWord(item, j, "deferrable") {
		j++
	}
	if j > start && isWord(item, j, "initially") && (isWord(item, j+1, "deferred") || isWord(item, j+1, "immediate")) {
		j += 2
	}
	return j
}

// renderTokens renders tokens back into valid, executable SQL text — never
// canonicalize/canonicalizeTokens, which re-quotes every word for
// comparison and is not meant to be read by anything but this package's
// own drift check. A quoted word is rendered backtick-quoted (sqldef's own
// generated ADD COLUMN statements already quote every identifier this way,
// so a spliced-on REFERENCES clause reads consistently with the statement
// it is appended to); a string literal renders as tokenize's own
// canonical single-quoted form, already valid SQL; every other token
// renders as its own exact text.
func renderTokens(tokens []sqlToken) string {
	var b strings.Builder
	for i, t := range tokens {
		if i > 0 && needsSpaceBetween(tokens[i-1], t) {
			b.WriteByte(' ')
		}
		b.WriteString(renderToken(t))
	}
	return b.String()
}

func renderToken(t sqlToken) string {
	if t.kind == tokWord && t.quoted {
		return "`" + strings.ReplaceAll(t.text, "`", "``") + "`"
	}
	return t.text
}

// needsSpaceBetween reports whether a space belongs between prev and cur in
// renderTokens' output. Omitting it around "(", ")", "," and "." matches
// how a qualified name or a column list ordinarily reads ("parent(id)",
// "a.b"); every other pair of tokens needs one, since two adjacent bare
// words or a word next to a string literal would otherwise merge into one
// token when the result is parsed again.
func needsSpaceBetween(prev, cur sqlToken) bool {
	switch {
	case cur.kind == tokPunct && (cur.text == "(" || cur.text == ")" || cur.text == "," || cur.text == "."):
		return false
	case prev.kind == tokPunct && (prev.text == "(" || prev.text == "."):
		return false
	default:
		return true
	}
}

// AddColumnTarget reports the table and column a generated
// "ALTER TABLE <table> ADD [COLUMN] <column> ..." statement targets, both
// folded to ASCII lowercase the same way SchemaColumnReferences keys its
// own map — so a lookup between the two never depends on either side's
// original quoting or case. ok is false for anything else, including every
// other ALTER TABLE form (RENAME, DROP).
func AddColumnTarget(stmt string) (table, column string, ok bool) {
	tokens := tokenize(stmt)
	if !isWord(tokens, 0, "alter") || !isWord(tokens, 1, "table") {
		return "", "", false
	}
	nameEnd := qualifiedNameEnd(tokens, 2)
	if nameEnd == 2 {
		return "", "", false
	}
	table = asciiLower(tokens[nameEnd-1].text)
	i := nameEnd
	if !isWord(tokens, i, "add") {
		return "", "", false
	}
	i++
	if isWord(tokens, i, "column") {
		i++
	}
	colEnd := qualifiedNameEnd(tokens, i)
	if colEnd == i {
		return "", "", false
	}
	column = asciiLower(tokens[colEnd-1].text)
	return table, column, true
}
