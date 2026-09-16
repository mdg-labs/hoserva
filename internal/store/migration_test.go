package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Embedded(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one embedded migration")
	}
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Fatalf("migration %d: version = %d, want %d (contiguous from 1)", i, m.Version, i+1)
		}
	}
}

func TestLoadDir_RejectsBadFilename(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "not_a_migration.sql"), "CREATE TABLE x (id INTEGER);")

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected an error for a filename that doesn't match NNNN_slug.sql")
	}
}

func TestLoadDir_RejectsDuplicateVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "0001_a.sql"), "CREATE TABLE a (id INTEGER);")
	writeFile(t, filepath.Join(dir, "0001_b.sql"), "CREATE TABLE b (id INTEGER);")

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected an error for two migration files sharing a version")
	}
}

func TestLoadDir_RejectsNonContiguousVersions(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "0001_a.sql"), "CREATE TABLE a (id INTEGER);")
	writeFile(t, filepath.Join(dir, "0003_b.sql"), "CREATE TABLE b (id INTEGER);")

	if _, err := LoadDir(dir); err == nil {
		t.Fatal("expected an error for a gap in migration versions")
	}
}

func TestLoadDir_SkipsChecksumsAndContracts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "0001_a.sql"), "CREATE TABLE a (id INTEGER);")
	writeFile(t, filepath.Join(dir, ChecksumsFile), "deadbeef  0001_a.sql\n")
	writeFile(t, filepath.Join(dir, ContractsFile), "# nothing yet\n")

	migrations, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("len(migrations) = %d, want 1", len(migrations))
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
