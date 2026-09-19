package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// VerifyArchive checks doc 10 §1's post-write requirements: the archive
// unpacks, manifest checksums match, state.db opens and passes
// PRAGMA integrity_check, and secrets.age decrypts when a passphrase is
// given.
func VerifyArchive(archivePath, passphrase string) error {
	dir, err := os.MkdirTemp("", "hoserva-backup-verify-*")
	if err != nil {
		return fmt.Errorf("creating verify temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := unpackArchive(archivePath, dir); err != nil {
		return fmt.Errorf("unpacking archive: %w", err)
	}

	manifest, err := readManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	for rel, want := range manifest.Checksums {
		got, err := hashFile(filepath.Join(dir, rel))
		if err != nil {
			return fmt.Errorf("hashing extracted %s: %w", rel, err)
		}
		if got != want {
			return fmt.Errorf("checksum mismatch for %s", rel)
		}
	}

	stateDB := filepath.Join(dir, "state.db")
	if _, err := os.Stat(stateDB); err != nil {
		return fmt.Errorf("state.db missing from archive: %w", err)
	}
	db, err := sql.Open("sqlite", stateDB)
	if err != nil {
		return fmt.Errorf("opening state.db: %w", err)
	}
	defer func() { _ = db.Close() }()

	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("running integrity_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("integrity_check failed: %s", result)
	}

	secretsPath := filepath.Join(dir, "secrets.age")
	if _, err := os.Stat(secretsPath); err == nil {
		if passphrase == "" {
			return fmt.Errorf("archive contains secrets.age but no passphrase was provided")
		}
		data, err := os.ReadFile(secretsPath)
		if err != nil {
			return fmt.Errorf("reading secrets.age: %w", err)
		}
		if _, err := decryptSecretsAge(data, passphrase); err != nil {
			return fmt.Errorf("decrypting secrets.age: %w", err)
		}
	}
	return nil
}
