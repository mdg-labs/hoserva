// Command dbcheck is `make db-check` (Q60, D16): it fails when a migration
// file has changed since it was generated, when replaying every migration
// does not produce exactly schema.sql, when a migration contains a
// statement outside the recognized migration vocabulary, or when it
// carries a statement only a registered contract step may contain (a drop,
// or a contract step's own INSERT ... SELECT) without being registered.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mdg-labs/hoserva/internal/store"
)

func main() {
	storeDir := flag.String("dir", "internal/store", "path to internal/store")
	flag.Parse()

	if err := run(*storeDir); err != nil {
		fmt.Fprintln(os.Stderr, "db-check:", err)
		os.Exit(1)
	}
}

func run(storeDir string) error {
	schemaPath := filepath.Join(storeDir, "schema", "schema.sql")
	migrationsDir := filepath.Join(storeDir, "migrations")

	schemaSQL, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", schemaPath, err)
	}

	migrations, err := store.LoadDir(migrationsDir)
	if err != nil {
		return fmt.Errorf("loading migrations: %w", err)
	}

	checksumData, err := os.ReadFile(filepath.Join(migrationsDir, store.ChecksumsFile))
	if err != nil {
		return fmt.Errorf("reading %s: %w", store.ChecksumsFile, err)
	}
	recorded, err := store.ReadChecksums(checksumData)
	if err != nil {
		return err
	}
	if err := store.VerifyChecksums(migrations, recorded); err != nil {
		return err
	}
	fmt.Println("db-check: checksums OK")

	// Vet every migration's own statement vocabulary — and whether a
	// contract-only shape is registered — before CheckDrift ever replays a
	// single one of them. CheckDrift executes migration SQL verbatim
	// against a live connection to build its comparison database; running
	// it before this check would let a migration reach an ATTACH, a
	// VACUUM INTO, a PRAGMA or any other statement outside the recognized
	// vocabulary before it is ever refused.
	contracts, err := store.LoadContractsFile(migrationsDir)
	if err != nil {
		return err
	}
	if err := store.CheckAllowedStatements(migrations, contracts); err != nil {
		return err
	}
	fmt.Println("db-check: every statement is in the recognized migration vocabulary")

	if err := store.CheckSafety(migrations, contracts); err != nil {
		return err
	}
	fmt.Println("db-check: no unregistered destructive migration")

	if err := store.CheckDrift(context.Background(), string(schemaSQL), migrations); err != nil {
		return err
	}
	fmt.Println("db-check: no drift — replaying every migration produces exactly schema.sql")

	return nil
}
