package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// schemaObject is one stored CREATE TABLE/INDEX/TRIGGER/VIEW statement,
// identified by its SQL object type and name, holding sqlite_master's own
// "sql" text canonicalized by sqltoken.go's tokenizer. Comparing this
// whole canonical stream, per object, rather than reading out an
// enumerated list of clauses one at a time, is what makes every column,
// its type, NOT NULL, DEFAULT, CHECK (all of them), COLLATE,
// ON CONFLICT, DEFERRABLE, AUTOINCREMENT, a generated column's expression,
// column order, every table constraint including the autoindex a bare
// UNIQUE creates (declared inline, so it's part of the table's own text),
// STRICT and WITHOUT ROWID (a trailing table option, also part of the
// text), and — for an index — a partial WHERE clause or an expression
// column, are all just more tokens in the statement already being
// compared. Nothing here needs enumerating a second time through a PRAGMA.
//
// This still isn't a literal `sqlite_master.sql` text comparison: SQLite's
// own `ALTER TABLE ... ADD COLUMN` rewrites the *stored* CREATE TABLE text
// by splicing in the ALTER's own column-definition text verbatim —
// backtick-quoted, lower-case type keyword, its own comma placement —
// which would not match a hand-written schema.sql's unquoted, upper-case
// text even when the two schemas are identical. Tokenizing normalizes
// exactly that: case, quoting style and whitespace are gone before either
// side is compared, so a spliced column is just more tokens in the same
// position as schema.sql's own hand-written one (see
// TestCheckDrift_NoFalsePositiveForAddColumnRewriting).
type schemaObject struct {
	kind string // "table", "index", "trigger" or "view" — sqlite_master.type
	name string
}

// readSchemaObjects reads every named table, explicit index (an autoindex
// has no row with sql IS NOT NULL — SQLite exposes those only through
// PRAGMA index_list, and their structure is already part of the owning
// table's own text above), trigger and view out of db, canonicalized.
func readSchemaObjects(ctx context.Context, db *sql.DB) (map[schemaObject]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND type IN ('table', 'index', 'trigger', 'view')
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name != '`+bookkeepingTable+`'
		ORDER BY type, name
	`)
	if err != nil {
		return nil, fmt.Errorf("reading sqlite_master: %w", err)
	}
	defer closeQuietly(rows)

	out := map[schemaObject]string{}
	for rows.Next() {
		var kind, name, stmt string
		if err := rows.Scan(&kind, &name, &stmt); err != nil {
			return nil, err
		}
		// A trigger's or view's own body has no column-definition list for
		// canonicalize's quoted/bare equivalence to protect at all — see
		// canonicalizeBody's own doc comment.
		var canon string
		if kind == "trigger" || kind == "view" {
			canon = canonicalizeBody(stmt)
		} else {
			canon = canonicalize(stmt)
		}
		out[schemaObject{kind: kind, name: strings.ToLower(name)}] = canon
	}
	return out, rows.Err()
}

// diffSchemaObjects describes every difference between want and got, or ""
// if there is none.
func diffSchemaObjects(want, got map[schemaObject]string) string {
	all := map[schemaObject]bool{}
	for k := range want {
		all[k] = true
	}
	for k := range got {
		all[k] = true
	}
	keys := make([]schemaObject, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].name < keys[j].name
	})

	var diffs []string
	for _, k := range keys {
		w, wOK := want[k]
		g, gOK := got[k]
		switch {
		case !wOK:
			diffs = append(diffs, fmt.Sprintf("%s %q exists but is not in schema.sql:\n  migrations: %s", k.kind, k.name, g))
		case !gOK:
			diffs = append(diffs, fmt.Sprintf("%s %q is in schema.sql but no migration produces it:\n  schema.sql: %s", k.kind, k.name, w))
		case w != g:
			diffs = append(diffs, fmt.Sprintf("%s %q differs:\n  schema.sql: %s\n  migrations: %s", k.kind, k.name, w, g))
		}
	}
	return strings.Join(diffs, "\n")
}
