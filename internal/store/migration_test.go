package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_Embedded confirms this package's own go:embed wiring and its
// malformed-filename pre-scan (rejectMalformedFilenames); sqlite-migrate's
// own LoadDir behavior (duplicate versions, etc.) lives, and is tested, in
// github.com/mdg-labs/sqlite-migrate itself.
func TestLoad_Embedded(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one embedded migration")
	}
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Fatalf("migration %d (%s) is not strictly after migration %d (%s)", i, migrations[i].Version, i-1, migrations[i-1].Version)
		}
	}
}

func TestLoadDir_ReadsMigrationsFromDisk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "20260101000001_a.sql"), "CREATE TABLE a (id INTEGER) STRICT;")
	writeFile(t, filepath.Join(dir, "20260101000002_b.sql"), "ALTER TABLE a ADD COLUMN note TEXT;")

	migrations, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(migrations) != 2 {
		t.Fatalf("len(migrations) = %d, want 2", len(migrations))
	}
	if migrations[0].Version != "20260101000001" || migrations[1].Version != "20260101000002" {
		t.Fatalf("migrations not read in version order: %+v", migrations)
	}
}

func TestLoadDir_RejectsMalformedFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "20260101000001_a.sql"), "CREATE TABLE a (id INTEGER) STRICT;")
	writeFile(t, filepath.Join(dir, "not-a-migration.sql"), "CREATE TABLE b (id INTEGER) STRICT;")

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("LoadDir: expected an error for a malformed migration filename, got nil")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
