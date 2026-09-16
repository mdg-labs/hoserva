// Command dbmigration is `make db-migration NAME=<slug>` (Q60): it diffs
// internal/store/schema/schema.sql against the schema the existing
// migrations under internal/store/migrations/ produce, using sqldef's
// SQLite generator as a library (never shelling out to the sqlite3def
// binary, and never with its --enable-drop-table flag — Q60's default),
// and writes the result as the next numbered, immutable migration file.
//
// It never emits anything store.Destructive flags — a DROP TABLE, a DROP
// COLUMN, or a table rebuild (SQLite's only way to change a column's type
// or constraints) — and does not rely on sqldef's own --enable-drop-table
// gate to keep that promise: Q60's findings are that sqldef's gate is a
// bare substring check on "DROP TABLE" that does not gate DROP COLUMN at
// all (and a column rename comes out of the generator as ADD COLUMN +
// DROP COLUMN, which carries the same gap). A schema.sql change that needs
// one of these needs a hand-authored contract-step migration and a
// registration in internal/store/migrations/contracts (D16) — this tool
// refuses to generate one on its own.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqldefdb "github.com/sqldef/sqldef/database"
	sqldefsqlite "github.com/sqldef/sqldef/database/sqlite3"
	sqldefparser "github.com/sqldef/sqldef/parser"
	sqldefschema "github.com/sqldef/sqldef/schema"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func main() {
	name := flag.String("name", "", "slug for the new migration file, e.g. add_job_table")
	storeDir := flag.String("dir", "internal/store", "path to internal/store")
	flag.Parse()

	if err := run(*name, *storeDir); err != nil {
		fmt.Fprintln(os.Stderr, "db-migration:", err)
		os.Exit(1)
	}
}

func run(name, storeDir string) error {
	if name == "" {
		return fmt.Errorf("-name is required, e.g. -name add_job_table")
	}
	if !isSlug(name) {
		return fmt.Errorf("-name %q must be lowercase letters, digits and underscores only", name)
	}

	schemaPath := filepath.Join(storeDir, "schema", "schema.sql")
	migrationsDir := filepath.Join(storeDir, "migrations")

	desiredDDLs, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", schemaPath, err)
	}

	migrations, err := store.LoadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("loading existing migrations: %w", err)
	}

	// Verify every existing migration's checksum before generating a new
	// one. Without this, an edited "immutable" migration would sail
	// through unnoticed: AppendChecksum only ever adds the new file's own
	// entry, so nothing else re-checks the existing ones on this path —
	// db-check would catch it eventually, but only after `make db-migration`
	// had already re-approved it by generating the next migration on top of
	// the tampered one.
	checksumData, err := os.ReadFile(filepath.Join(migrationsDir, store.ChecksumsFile))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", store.ChecksumsFile, err)
	}
	if err == nil {
		recorded, err := store.ReadChecksums(checksumData)
		if err != nil {
			return err
		}
		if err := store.VerifyChecksums(migrations, recorded); err != nil {
			return err
		}
	}

	// Vet every existing migration's own statement vocabulary before
	// currentSchemaDDLs ever replays one of them: that replay executes
	// migration SQL verbatim against a live connection, so it must never
	// reach an ATTACH, a VACUUM INTO, a PRAGMA or anything else outside the
	// recognized vocabulary before it is refused. Contract registration is
	// not re-checked here — an existing migration already embedded on disk
	// was already reviewed against contracts when it was written or
	// generated (the same reasoning as the runner's own
	// CheckStatementVocabulary call).
	if err := store.CheckStatementVocabulary(migrations); err != nil {
		return fmt.Errorf("an existing migration is outside the recognized vocabulary: %w", err)
	}

	ctx := context.Background()
	currentDDLs, err := currentSchemaDDLs(ctx, migrations)
	if err != nil {
		return err
	}

	sqlParser := sqldefdb.NewParser(sqldefparser.ParserModeSQLite3)
	ddls, err := sqldefschema.GenerateIdempotentDDLs(
		sqldefschema.GeneratorModeSQLite3, sqlParser, string(desiredDDLs), currentDDLs,
		sqldefdb.GeneratorConfig{}, "",
	)
	if err != nil {
		return fmt.Errorf("diffing schema.sql against the existing migrations: %w", err)
	}

	var kept []string
	for _, ddl := range ddls {
		if reasons := store.Destructive(ddl); len(reasons) > 0 {
			fmt.Fprintf(os.Stderr, "db-migration: skipping a destructive statement (%s) this tool never generates automatically — %s;\ndb-migration: write a contract-step migration by hand and register it in %s if this is intended (D16)\n", strings.Join(reasons, ", "), ddl, store.ContractsFile)
			continue
		}
		kept = append(kept, ddl)
	}
	if len(kept) == 0 {
		return fmt.Errorf("no schema change to generate a migration for: schema.sql already matches the existing migrations, or the only difference is a destructive statement this tool refuses to generate automatically (a DROP TABLE, a DROP COLUMN, or a table rebuild) — write a contract-step migration by hand for that (D16); note that sqldef's SQLite generator cannot detect a column type, CHECK, UNIQUE or foreign-key-action change at all (Q60), so `make db-check`'s drift check, not this tool, is what catches schema.sql changes it silently misses")
	}

	nextVersion := 1
	for _, m := range migrations {
		if m.Version >= nextVersion {
			nextVersion = m.Version + 1
		}
	}
	filename := fmt.Sprintf("%04d_%s.sql", nextVersion, name)
	path := filepath.Join(migrationsDir, filename)

	var body strings.Builder
	fmt.Fprintf(&body, "-- Generated by `make db-migration` on %s.\n", time.Now().UTC().Format("2006-01-02"))
	fmt.Fprintf(&body, "-- Do not edit this file — it is immutable once created (D16). A mistake\n")
	fmt.Fprintf(&body, "-- is corrected by changing schema.sql and generating another migration.\n")
	for _, ddl := range kept {
		fmt.Fprintf(&body, "%s;\n", ddl)
	}

	// Run the same drift and vocabulary checks `make db-check` would, on
	// this migration before it is ever written: without this, db-migration
	// would write and checksum an "immutable" file for every schema.sql
	// change sqldef can generate *something* for, even when that something
	// doesn't reproduce schema.sql (its ADD COLUMN silently dropping a new
	// column's own REFERENCES clause is the known case — Q60), or contains
	// a statement outside the recognized migration vocabulary this tool's
	// own Destructive-based skip above doesn't recognize (a DROP INDEX,
	// for one — Destructive never reports it, since CheckAllowedStatements
	// is what actually refuses an unregistered DROP INDEX/VIEW/TRIGGER,
	// not the Destructive-based skip above). Refusing here means nothing
	// is ever written for a migration that would already fail db-check,
	// rather than leaving the author to notice only at the next
	// `make db-check` and then need to remove a file this tool calls
	// immutable.
	//
	// Vetted before CheckDrift, not after: CheckDrift replays candidate —
	// the existing migrations plus this new one — verbatim against a live
	// connection, so newMigration's own statements must be confirmed
	// classOrdinary first. Checked on its own, not on candidate: this tool
	// never writes a registered contract step, so the new migration's own
	// statements must all be classOrdinary, regardless of what an
	// existing, already reviewed migration on disk is registered for.
	newMigration := store.Migration{Version: nextVersion, Filename: filename, SQL: body.String()}
	if err := store.CheckAllowedStatements([]store.Migration{newMigration}, nil); err != nil {
		return fmt.Errorf("refusing to write %s: %w — this tool never generates a migration outside the recognized vocabulary; write a contract-step migration by hand for this instead (D16)", filename, err)
	}
	candidate := append(append([]store.Migration{}, migrations...), newMigration)
	if err := store.CheckDrift(ctx, string(desiredDDLs), candidate); err != nil {
		return fmt.Errorf("refusing to write %s: replaying it together with the existing migrations would not reproduce schema.sql (%w) — this is an open generation gap (docs/internal/13-open-questions.md Q60), not something this tool can work around on its own", filename, err)
	}

	if err := os.MkdirAll(migrationsDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	if err := store.AppendChecksum(migrationsDir, newMigration); err != nil {
		return fmt.Errorf("recording checksum: %w", err)
	}

	fmt.Printf("wrote %s\n", path)
	return nil
}

// currentSchemaDDLs materializes the schema the existing migrations
// produce by replaying them into a temporary SQLite database and dumping
// it with sqldef's own database layer — the same DumpDDLs a live-database
// sqlite3def invocation would use. This is deliberate: sqldef's other
// input mode (a plain "current.sql" file, database/file.Database) parses
// only CREATE-style DDL, not the ALTER TABLE statements a second or later
// migration may contain, and fails outright if it sees one.
func currentSchemaDDLs(ctx context.Context, migrations []store.Migration) (string, error) {
	tmp, err := os.CreateTemp("", "hoserva-db-migration-*.db")
	if err != nil {
		return "", err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer func() { _ = os.Remove(path) }()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	if err := store.ApplySchemaOnly(ctx, db, migrations); err != nil {
		_ = db.Close()
		return "", fmt.Errorf("replaying existing migrations: %w", err)
	}
	if err := db.Close(); err != nil {
		return "", fmt.Errorf("closing the temporary database: %w", err)
	}

	sqldefDB, err := sqldefsqlite.NewDatabase(sqldefdb.Config{DbName: path})
	if err != nil {
		return "", err
	}
	defer func() { _ = sqldefDB.Close() }()
	return sqldefDB.DumpDDLs()
}

func isSlug(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}
