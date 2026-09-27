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
// given. Used by the nightly backup chain, which always has the
// passphrase (or no secrets.age to check) since it just built the
// archive itself.
func VerifyArchive(archivePath, passphrase string) error {
	return verifyArchive(archivePath, passphrase, true)
}

// VerifyArchiveForImport runs the same checksum and PRAGMA integrity_check
// validation as VerifyArchive, but never requires a passphrase or
// decrypts secrets.age: config import (doc 10 §1, #269) restores the
// database only — #62 restores the rest, including secrets — so a
// caller importing an archive it did not just build, and may have no
// passphrase for yet, must still be able to validate the part it is
// about to restore from.
func VerifyArchiveForImport(archivePath string) error {
	return verifyArchive(archivePath, "", false)
}

func verifyArchive(archivePath, passphrase string, requireSecretsPassphrase bool) error {
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

	if !requireSecretsPassphrase {
		return nil
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
