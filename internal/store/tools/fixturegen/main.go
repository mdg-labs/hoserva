// Command fixturegen builds one testdata/db fixture (doc 06 §2): a
// database that has already gone through Runner.Apply at some released
// schema version, with representative rows, committed like a golden file
// and never regenerated in place — a later schema version gets a new
// fixture file, not an edit to this one.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func main() {
	out := flag.String("out", "", "path to write the fixture database to (must not already exist)")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "fixturegen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if out == "" {
		return fmt.Errorf("-out is required")
	}
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("%s already exists — fixtures are never regenerated in place (doc 06 §2); delete it deliberately first if this is really intended", out)
	}

	ctx := context.Background()
	migrations, err := store.Load()
	if err != nil {
		return err
	}

	db, err := sql.Open("sqlite", out)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	snapshotDir, err := os.MkdirTemp("", "hoserva-fixturegen-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(snapshotDir) }()

	r := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: snapshotDir}
	if _, _, err := r.Apply(ctx); err != nil {
		return fmt.Errorf("applying migrations to the fixture: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO schema_info (id, installation_id, created_at) VALUES (1, '4b1f6b6e-2f2a-4b7a-9e3a-2f7a8f6a9c11', '2026-01-01T00:00:00Z')"); err != nil {
		return fmt.Errorf("seeding representative data: %w", err)
	}

	version, err := r.CurrentVersion(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s at schema version %d\n", out, version)
	return nil
}
