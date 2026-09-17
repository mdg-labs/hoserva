// Package storetest holds test data shared between internal/store's own
// tests and internal/store/tools/dbcheck's tests. An ordinary _test.go file
// belongs only to the package it's compiled into and can't be imported from
// another package's tests, so this data lives in a real, importable package
// instead — one that only a *_test.go file ever imports, kept out of
// internal/store's own production API.
package storetest

import "fmt"

// DisallowedStatementExamples is one representative statement per kind
// internal/store's classifyStatement must refuse outright, keyed by a
// short name for a test's own subtest/case label. Every layer that has to
// prove the exact same rejection — the classifier's own unit test,
// `make db-check` (internal/store/tools/dbcheck), and Runner.Apply — drives
// this identical set of cases, rather than each layer keeping its own
// partial, silently-drifting copy.
var DisallowedStatementExamples = map[string]string{
	"EXPLAIN":                      "EXPLAIN SELECT 1;",
	"EXPLAIN PRAGMA":               "EXPLAIN PRAGMA ignore_check_constraints = 1;",
	"EXPLAIN QUERY PLAN PRAGMA":    "EXPLAIN QUERY PLAN PRAGMA foreign_keys = OFF;",
	"PRAGMA":                       "PRAGMA foreign_keys = OFF;",
	"schema-qualified PRAGMA":      "PRAGMA main.foreign_keys = OFF;",
	"BEGIN":                        "BEGIN;",
	"COMMIT":                       "COMMIT;",
	"ROLLBACK":                     "ROLLBACK;",
	"END":                          "END;",
	"END TRANSACTION":              "END TRANSACTION;",
	"SAVEPOINT":                    "SAVEPOINT s1;",
	"RELEASE":                      "RELEASE s1;",
	"VACUUM":                       "VACUUM;",
	"ATTACH":                       "ATTACH DATABASE 'attach-target.db' AS x;",
	"DETACH":                       "DETACH DATABASE x;",
	"REINDEX":                      "REINDEX a;",
	"ANALYZE":                      "ANALYZE;",
	"CREATE TEMP TABLE":            "CREATE TEMP TABLE a (id INTEGER);",
	"CREATE TEMPORARY TABLE":       "CREATE TEMPORARY TABLE a (id INTEGER);",
	"CREATE TABLE temp.-qualified": "CREATE TABLE temp.a (id INTEGER);",
	"CREATE TEMP TRIGGER":          "CREATE TEMP TRIGGER evil AFTER INSERT ON a BEGIN DELETE FROM main.a; END;",
	"CREATE TEMP VIEW":             "CREATE TEMP VIEW v AS SELECT 1;",
	"CREATE TEMP INDEX (invalid SQL, fails closed)": "CREATE TEMP INDEX ai ON a (id);",
	"CREATE TABLE AS SELECT":                        "CREATE TABLE archive AS SELECT * FROM a;",
	"WITH ... INSERT":                               "WITH cte AS (SELECT 1 AS id) INSERT INTO a SELECT id FROM cte;",
	"DELETE (ordinary shape)":                       "DELETE FROM a;",
	"UPDATE (ordinary shape)":                       "UPDATE a SET v = 1;",
	"INSERT ... VALUES":                             "INSERT INTO a VALUES (1);",
	"INSERT OR REPLACE ... SELECT":                  "INSERT OR REPLACE INTO a SELECT * FROM b;",
	"REPLACE INTO":                                  "REPLACE INTO a VALUES (1);",
	// A WHERE clause is required between the SELECT and ON CONFLICT for
	// SQLite's own grammar to parse this unambiguously as an upsert rather
	// than a syntax error — this proves the refusal below comes from the
	// classifier, not from SQLite rejecting malformed SQL.
	"INSERT ... SELECT ... ON CONFLICT DO UPDATE": "INSERT INTO a SELECT * FROM b WHERE true ON CONFLICT(id) DO UPDATE SET v = excluded.v;",
	"bare SELECT":                    "SELECT 1;",
	"unrecognized ALTER TABLE shape": "ALTER TABLE a SOMETHING_UNRECOGNIZED;",
	"unparseable garbage":            "THIS IS NOT VALID SQL AT ALL;",
	// TEMP-qualified with a case-insensitive, quoted schema name: SQLite
	// resolves "TEMP".x exactly as if TEMP had been written on the CREATE
	// statement itself, case-insensitively, regardless of quoting.
	"CREATE TABLE quoted, mixed-case TEMP-qualified": `CREATE TABLE "TEMP".x (id INTEGER);`,
}

// WithAttachTarget returns a copy of DisallowedStatementExamples with the
// "ATTACH" example's target rewritten to targetPath. A caller that
// actually executes these examples against a live connection (rather than
// only tokenizing and classifying them) uses this to point the one example
// that can touch the filesystem at a path inside its own t.TempDir(),
// rather than the bare relative filename above, which a classifier
// regression would otherwise create relative to the test binary's working
// directory.
func WithAttachTarget(targetPath string) map[string]string {
	out := make(map[string]string, len(DisallowedStatementExamples))
	for name, stmt := range DisallowedStatementExamples {
		out[name] = stmt
	}
	out["ATTACH"] = fmt.Sprintf("ATTACH DATABASE '%s' AS x;", targetPath)
	return out
}
