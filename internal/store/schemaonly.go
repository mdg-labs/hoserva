package store

import (
	"context"
	"database/sql"
	"fmt"
)

// ApplySchemaOnly replays migrations, in order, into db with no bookkeeping
// table and no transaction semantics. It exists for tooling that needs to
// materialize "the schema these migrations produce" as a plain question
// about schema.sql — db-migration to compute a diff, db-check to detect
// drift, the fixture-upgrade harness to build a starting point — never for
// the real startup path, which is Runner.Apply below and does carry
// bookkeeping and transaction safety.
func ApplySchemaOnly(ctx context.Context, db *sql.DB, migrations []Migration) error {
	for _, m := range migrations {
		if _, err := db.ExecContext(ctx, m.SQL); err != nil {
			return fmt.Errorf("applying %q: %w", m.Filename, err)
		}
	}
	return nil
}

// DumpSchema returns db's actual schema as SQLite stored it — the literal
// text of every CREATE TABLE/INDEX/TRIGGER/VIEW statement from
// sqlite_master, in a stable order. Two databases with byte-identical
// stored text here have the same schema by definition (it is SQLite's own
// literal record of it); this is what the fixture harness compares
// (unlike CheckDrift/schemastruct.go's canonicalized comparison, which
// also tolerates the re-serialization ALTER TABLE ... ADD COLUMN does to
// that stored text), independently of sqldef's own diffing (which, per
// Q60's findings, does not detect every kind of schema change).
// It excludes bookkeepingTable itself: that table is this runner's own
// infrastructure (runner.go), created outside the migration transaction so
// CurrentVersion can always be read, and its existence must not register
// as a schema change — neither drift against schema.sql nor a side effect
// of an otherwise fully rolled-back migration attempt.
func DumpSchema(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sql FROM sqlite_master
		WHERE sql IS NOT NULL AND type IN ('table', 'index', 'trigger', 'view')
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' AND name != '`+bookkeepingTable+`'
		ORDER BY type, name
	`)
	if err != nil {
		return "", fmt.Errorf("dumping schema: %w", err)
	}
	defer closeQuietly(rows)

	var out string
	for rows.Next() {
		var stmt string
		if err := rows.Scan(&stmt); err != nil {
			return "", fmt.Errorf("dumping schema: %w", err)
		}
		out += stmt + ";\n"
	}
	return out, rows.Err()
}
