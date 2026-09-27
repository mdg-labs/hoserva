package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func buildArchiveWithSecrets(t *testing.T, dir string) string {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}

	src := &FakeSecretSource{
		Passphrase: "test-pass",
		HasPass:    true,
		Secrets: []DatabaseSecret{{
			Table:      "notify_channels",
			Column:     "secret",
			RowID:      "ch1",
			Ciphertext: []byte{0x5a},
		}},
	}

	staging := filepath.Join(dir, "staging")
	if _, err := BuildArchive(ctx, db, Paths{}, src, FakeSecretCipher{}, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}

	archivePath := filepath.Join(dir, "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}
	return archivePath
}

// TestVerifyArchive_RequiresPassphraseWhenSecretsAgePresent is the
// existing nightly-backup behaviour VerifyArchiveForImport must not
// change: the full VerifyArchive keeps refusing an archive with
// secrets.age and no passphrase.
func TestVerifyArchive_RequiresPassphraseWhenSecretsAgePresent(t *testing.T) {
	archivePath := buildArchiveWithSecrets(t, t.TempDir())
	if err := VerifyArchive(archivePath, ""); err == nil {
		t.Fatal("VerifyArchive(archive with secrets.age, no passphrase) = nil, want an error")
	}
	if err := VerifyArchive(archivePath, "test-pass"); err != nil {
		t.Fatalf("VerifyArchive(archive with secrets.age, correct passphrase): %v", err)
	}
}

// TestVerifyArchiveForImport_SkipsSecretsAgeWithoutPassphrase is #269's
// own requirement: config import (doc 10 §1) restores the database only
// — #62 restores secrets — so an archive with secrets.age must still
// pass checksum and integrity verification with no passphrase at all.
func TestVerifyArchiveForImport_SkipsSecretsAgeWithoutPassphrase(t *testing.T) {
	archivePath := buildArchiveWithSecrets(t, t.TempDir())
	if err := VerifyArchiveForImport(archivePath); err != nil {
		t.Fatalf("VerifyArchiveForImport(archive with secrets.age, no passphrase): %v", err)
	}
}

// TestVerifyArchiveForImport_StillCatchesChecksumMismatch confirms the
// import-only verifier still runs the same structural checks as
// VerifyArchive — only the secrets.age passphrase requirement is skipped.
func TestVerifyArchiveForImport_StillCatchesChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatalf("opening live database: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatalf("creating table: %v", err)
	}
	staging := filepath.Join(dir, "staging")
	if _, err := BuildArchive(ctx, db, Paths{}, nil, nil, "host", "test", time.Now(), staging); err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	// Corrupt the staged state.db after the manifest checksum was
	// computed over its original bytes.
	if err := (func() error {
		f, err := sql.Open("sqlite", filepath.Join(staging, "state.db"))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.Exec("INSERT INTO t (id) VALUES (999)")
		return err
	})(); err != nil {
		t.Fatalf("corrupting staged state.db: %v", err)
	}
	archivePath := filepath.Join(dir, "archive.tar.zst")
	if err := packArchive(staging, archivePath); err != nil {
		t.Fatalf("packArchive: %v", err)
	}

	if err := VerifyArchiveForImport(archivePath); err == nil {
		t.Fatal("VerifyArchiveForImport(archive with tampered state.db) = nil, want a checksum-mismatch error")
	}
}
