// Package store is the runner from doc 01 §4 (D16, Q60): "a small runner in
// internal/store with migrations embedded via go:embed". It loads the
// schema migrations embedded from internal/store/migrations/, applies the
// pending ones inside a single transaction with a pre-migration snapshot,
// and holds the checks (checksums, drift, data-safety) that make `make
// db-check` mean something. Typed queries against the resulting schema
// live in the sibling, generated internal/store/db package.
package store

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations
var embedded embed.FS

// migrationsDir is the directory embedded above, and the one the
// db-migration and db-check tools (internal/store/tools) read from disk
// directly — generating a new migration or its checksums needs a real
// path, not just the embed.FS.
const migrationsDir = "migrations"

// ChecksumsFile and ContractsFile live alongside the migration files
// themselves, inside migrationsDir, so there is exactly one place that
// tracks "which migration" for both concerns.
const (
	ChecksumsFile = "checksums"
	ContractsFile = "contracts"
)

var migrationFilePattern = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// Migration is one immutable, numbered schema change (doc 01 §4). Version
// is parsed from the filename's leading zero-padded number — the only
// ordering source; nothing here trusts file modification times.
type Migration struct {
	Version  int
	Filename string
	SQL      string
}

// Load reads every migration file embedded from internal/store/migrations,
// in version order. It fails closed: a file that doesn't match the
// generator's own naming convention, or a duplicate version, is an error
// rather than a silently skipped file.
func Load() ([]Migration, error) {
	return loadFS(embedded, migrationsDir)
}

// LoadDir reads migrations from a real directory on disk — used by the
// db-migration and db-check tools, which need to write a new file or
// inspect one before it is embedded by a rebuild.
func LoadDir(dir string) ([]Migration, error) {
	return loadFS(os.DirFS(dir), ".")
}

func loadFS(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading migrations directory: %w", err)
	}

	var out []Migration
	seen := map[int]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == ChecksumsFile || name == ContractsFile {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match the generator's naming convention (NNNN_slug.sql)", name)
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration file %q: %w", name, err)
		}
		if prev, ok := seen[version]; ok {
			return nil, fmt.Errorf("duplicate migration version %d: %q and %q", version, prev, name)
		}
		seen[version] = name

		content, err := fs.ReadFile(fsys, joinPath(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", name, err)
		}
		out = append(out, Migration{Version: version, Filename: name, SQL: string(content)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := range out {
		if i > 0 && out[i].Version != out[i-1].Version+1 {
			return nil, fmt.Errorf("migration versions are not contiguous: %d follows %d", out[i].Version, out[i-1].Version)
		}
	}
	return out, nil
}

func joinPath(dir, name string) string {
	if dir == "." || dir == "" {
		return name
	}
	return strings.TrimSuffix(dir, "/") + "/" + name
}
