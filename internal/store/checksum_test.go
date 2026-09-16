package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyChecksums_OK(t *testing.T) {
	m := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}
	recorded := map[string]string{m.Filename: ChecksumOf(m)}

	if err := VerifyChecksums([]Migration{m}, recorded); err != nil {
		t.Fatalf("VerifyChecksums: %v", err)
	}
}

// This is the immutability guarantee D16 depends on: a migration file that
// changed after it was generated must be caught, not silently re-applied
// with different content than whatever was checksummed (and, in a real
// upgrade, possibly already applied to someone's database).
func TestVerifyChecksums_DetectsEditedMigration(t *testing.T) {
	original := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}
	recorded := map[string]string{original.Filename: ChecksumOf(original)}

	edited := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER, extra TEXT);"}

	err := VerifyChecksums([]Migration{edited}, recorded)
	if err == nil {
		t.Fatal("expected an error for a migration whose content changed since it was checksummed")
	}
}

func TestVerifyChecksums_DetectsMissingChecksum(t *testing.T) {
	m := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}

	if err := VerifyChecksums([]Migration{m}, map[string]string{}); err == nil {
		t.Fatal("expected an error for a migration with no recorded checksum")
	}
}

func TestVerifyChecksums_DetectsStaleEntry(t *testing.T) {
	recorded := map[string]string{"0002_gone.sql": "deadbeef"}

	if err := VerifyChecksums(nil, recorded); err == nil {
		t.Fatal("expected an error for a checksum recorded for a file that no longer exists")
	}
}

func TestAppendChecksum_AddsNewEntryOnly(t *testing.T) {
	dir := t.TempDir()
	a := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}
	if err := AppendChecksum(dir, a); err != nil {
		t.Fatalf("AppendChecksum (first): %v", err)
	}
	b := Migration{Version: 2, Filename: "0002_b.sql", SQL: "ALTER TABLE a ADD COLUMN v TEXT;"}
	if err := AppendChecksum(dir, b); err != nil {
		t.Fatalf("AppendChecksum (second): %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ReadChecksums(data)
	if err != nil {
		t.Fatal(err)
	}
	if sums[a.Filename] != ChecksumOf(a) || sums[b.Filename] != ChecksumOf(b) {
		t.Fatalf("checksums = %v, want both migrations recorded correctly", sums)
	}
}

func TestAppendChecksum_RefusesExistingFilename(t *testing.T) {
	dir := t.TempDir()
	a := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER);"}
	if err := AppendChecksum(dir, a); err != nil {
		t.Fatalf("AppendChecksum (first): %v", err)
	}

	edited := Migration{Version: 1, Filename: "0001_a.sql", SQL: "CREATE TABLE a (id INTEGER, extra TEXT);"}
	if err := AppendChecksum(dir, edited); err == nil {
		t.Fatal("expected AppendChecksum to refuse silently overwriting an existing entry")
	}

	data, err := os.ReadFile(filepath.Join(dir, ChecksumsFile))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ReadChecksums(data)
	if err != nil {
		t.Fatal(err)
	}
	if sums[a.Filename] != ChecksumOf(a) {
		t.Fatalf("existing checksum was overwritten: got %q, want %q", sums[a.Filename], ChecksumOf(a))
	}
}

func TestReadWriteChecksums_RoundTrip(t *testing.T) {
	sums := map[string]string{
		"0002_b.sql": "bbbb",
		"0001_a.sql": "aaaa",
	}
	data := FormatChecksums(sums)
	got, err := ReadChecksums(data)
	if err != nil {
		t.Fatalf("ReadChecksums: %v", err)
	}
	if len(got) != len(sums) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(sums))
	}
	for k, v := range sums {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}
