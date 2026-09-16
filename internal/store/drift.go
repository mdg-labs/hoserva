package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	_ "modernc.org/sqlite" // registers the "sqlite" driver used throughout this package (Q6)
)

// closeQuietly is for a deferred cleanup Close whose error carries no
// action a caller could take differently — the operation it's cleaning up
// after already returned its own, more specific error where one exists.
func closeQuietly(c io.Closer) { _ = c.Close() }

// CheckDrift confirms that replaying every migration into an empty database
// produces exactly schema.sql (doc 01 §4, doc 06 §2): it applies schemaSQL
// to one temporary database and migrations to another, then compares every
// stored table, explicit index, trigger and view's own text, canonicalized
// by sqltoken.go's tokenizer — see schemastruct.go for what that
// comparison actually covers and why. It deliberately does not ask sqldef
// to do this comparison at all — Q60's own findings are that sqlite3def's
// diff silently reports "Nothing is
// modified" for a column type, CHECK, UNIQUE or foreign-key-action change
// on SQLite, so relying on it here would let exactly the drift this check
// exists to catch through undetected.
func CheckDrift(ctx context.Context, schemaSQL string, migrations []Migration) error {
	// Vet before ever executing: schema.sql is the reviewed source of
	// truth, but this function still runs it verbatim against a live
	// connection below, so it must never reach an ATTACH, a VACUUM INTO, a
	// PRAGMA or anything else outside the ordinary schema vocabulary
	// (allowlist.go's CheckSchemaVocabulary).
	if err := CheckSchemaVocabulary(schemaSQL); err != nil {
		return err
	}

	fromSchema, err := newTempDB()
	if err != nil {
		return err
	}
	defer closeQuietly(fromSchema)
	if _, err := fromSchema.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("applying schema.sql to a temporary database: %w", err)
	}
	want, err := readSchemaObjects(ctx, fromSchema)
	if err != nil {
		return err
	}

	fromMigrations, err := newTempDB()
	if err != nil {
		return err
	}
	defer closeQuietly(fromMigrations)
	if err := ApplySchemaOnly(ctx, fromMigrations, migrations); err != nil {
		return fmt.Errorf("replaying migrations into a temporary database: %w", err)
	}
	got, err := readSchemaObjects(ctx, fromMigrations)
	if err != nil {
		return err
	}

	if diff := diffSchemaObjects(want, got); diff != "" {
		return fmt.Errorf("schema drift: replaying every migration does not produce schema.sql:\n%s", diff)
	}
	return nil
}

// newTempDB opens a fresh, private in-memory SQLite database. Each call
// gets its own database (a bare ":memory:" DSN would be shared across
// connections in the same process under some drivers, which is not what a
// "temporary, disposable database" means here), so callers never need to
// clean up a file on disk for this.
func newTempDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file::memory:?cache=private")
	if err != nil {
		return nil, fmt.Errorf("opening temporary database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
