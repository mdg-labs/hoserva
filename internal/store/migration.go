// Package store is the runner from doc 01 §4 (D16, Q60): a small runner in
// internal/store with migrations embedded via go:embed. Migration
// generation, checksumming and drift checking are sqlite-migrate's
// (github.com/mdg-labs/sqlite-migrate) — this package only loads the
// migrations it generated and applies the pending ones at daemon startup,
// alongside the data transforms a schema diff alone can't express
// (internal/store/transforms).
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"

	sqlitemigrate "github.com/mdg-labs/sqlite-migrate"
)

//go:embed migrations
var embedded embed.FS

// migrationsDir is the directory embedded above, and the one `sqlite-migrate
// generate`/`check` (via `make db-migration`/`make db-check`) read from disk
// directly — generating a new migration needs a real path, not just the
// embed.FS.
const migrationsDir = "migrations"

// Migration is one migration file sqlite-migrate generated: its sortable
// timestamp version, its filename, and the exact SQL body it wrote — reused
// directly from the library rather than wrapped, so the checksum this
// package records in its bookkeeping table (runner.go) is always the same
// value `sqlite-migrate status`/`check`/`verify` compute for the same file.
type Migration = sqlitemigrate.Migration

// migrationFilenamePattern mirrors sqlite-migrate's own
// "<timestamp>_<slug>.sql" convention. sqlitemigrate.LoadDir silently skips
// any directory entry that doesn't match it, so a mistyped or stray .sql
// file would otherwise never be applied and never be reported — this
// package rejects it instead of loading around it.
var migrationFilenamePattern = regexp.MustCompile(`^[0-9]{14}_[A-Za-z0-9]+(?:_[A-Za-z0-9]+)*\.sql$`)

// rejectMalformedFilenames fails loudly on any ".sql" entry directly inside
// dir that doesn't match migrationFilenamePattern, before handing dir to
// sqlitemigrate.LoadDir.
func rejectMalformedFilenames(fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return fmt.Errorf("reading migration directory %q: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if !migrationFilenamePattern.MatchString(name) {
			return fmt.Errorf("migration file %q does not match the <timestamp>_<slug>.sql convention", name)
		}
	}
	return nil
}

// Load reads every migration file embedded from internal/store/migrations,
// in version order.
func Load() ([]Migration, error) {
	if err := rejectMalformedFilenames(embedded, migrationsDir); err != nil {
		return nil, err
	}
	return sqlitemigrate.LoadDir(context.Background(), embedded, migrationsDir)
}

// LoadDir reads migrations from a real directory on disk.
func LoadDir(dir string) ([]Migration, error) {
	fsys := os.DirFS(dir)
	if err := rejectMalformedFilenames(fsys, "."); err != nil {
		return nil, err
	}
	return sqlitemigrate.LoadDir(context.Background(), fsys, ".")
}
