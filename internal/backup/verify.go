package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// VerifyArchive checks doc 10 §1's post-write requirements: the archive
// unpacks holding only regular files and directories, each name once, the
// manifest lists exactly the files it holds, its checksums match, state.db
// opens and passes
// PRAGMA integrity_check, and secrets.age decrypts when a passphrase is
// given. Used by the nightly backup chain, which always has the
// passphrase (or no secrets.age to check) since it just built the
// archive itself.
func VerifyArchive(archivePath, passphrase string) error {
	return verifyArchive(archivePath, passphrase, true)
}

// VerifyArchiveForImport runs the same checksum and PRAGMA integrity_check
// validation as VerifyArchive, but never requires a passphrase or
// decrypts secrets.age: config import (doc 10 §1) restores the database,
// custom config, templates and stacks from the archive's files, and a
// caller importing an archive it did not just build, and may have no
// passphrase for yet, must still be able to validate the part it is
// about to restore from.
func VerifyArchiveForImport(archivePath string) error {
	return verifyArchive(archivePath, "", false)
}

// ExtractVerifiedArchive unpacks archivePath into a new temporary
// directory, verifies what it unpacked the way VerifyArchiveForImport does
// and returns the directory, which the caller removes. A restore reads its
// files from this one tree and never from a second extraction, so what it
// writes is what was verified: RestoreFiles compares each file with the
// manifest again as it copies it. On an error the directory is already gone.
func ExtractVerifiedArchive(archivePath string) (string, error) {
	dir, err := os.MkdirTemp("", "hoserva-backup-verify-*")
	if err != nil {
		return "", fmt.Errorf("creating verify temp dir: %w", err)
	}
	if err := unpackArchive(archivePath, dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("unpacking archive: %w", err)
	}
	if err := verifyTree(dir, "", false); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// checkManifestCoversArchive requires the manifest and the unpacked
// archive to name exactly the same regular files, manifest.json aside: a
// file the manifest does not list is never checksummed, and a listed file
// that is absent (or a name that points outside the archive) is not what the
// manifest describes.
func checkManifestCoversArchive(dir string, manifest Manifest) error {
	present := map[string]struct{}{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		present[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("listing extracted archive: %w", err)
	}
	for rel := range manifest.Checksums {
		if _, ok := present[rel]; !ok {
			return fmt.Errorf("manifest lists %s, which is not in the archive", rel)
		}
	}
	for rel := range present {
		if rel == "manifest.json" {
			continue
		}
		if _, ok := manifest.Checksums[rel]; !ok {
			return fmt.Errorf("archive contains %s, which is not listed in the manifest", rel)
		}
	}
	return nil
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
	return verifyTree(dir, passphrase, requireSecretsPassphrase)
}

func verifyTree(dir, passphrase string, requireSecretsPassphrase bool) error {
	manifest, err := readManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("reading manifest: %w", err)
	}
	if err := checkManifestCoversArchive(dir, manifest); err != nil {
		return err
	}
	for rel, want := range manifest.Checksums {
		got, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
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
