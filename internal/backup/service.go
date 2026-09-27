package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Service implements job.ConfigBackup (doc 10 §1, Q30).
type Service struct {
	DB           *sql.DB
	Paths        Paths
	Destinations []Destination
	Secrets      SecretSource
	Cipher       SecretCipher
	Recipient    *Recipient
	Hostname     string
	Version      string
	Now          func() time.Time
}

// Run builds a config archive, verifies it, and writes it to every enabled
// local destination with retention pruning. The same method is invoked as
// the nightly chain's last step and before self-updates and topology
// changes (doc 10 §1).
func (s *Service) Run(ctx context.Context) error {
	if s.DB == nil {
		return fmt.Errorf("backup: no database configured")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}

	staging, err := os.MkdirTemp("", "hoserva-config-staging-*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	_, err = BuildArchive(ctx, s.DB, s.Paths, s.Secrets, s.Cipher, s.Hostname, s.Version, now, staging, WithRecipient(s.Recipient))
	if err != nil {
		return err
	}

	// Packed into a directory private to this run — not staging, which
	// packArchive is about to walk, and not a name derived only from now,
	// which two runs in the same minute would compute identically and
	// race on (#405). buildArtifacts derives the encrypted archive and
	// identity sidecar paths from archivePath's own directory, so this
	// also isolates those.
	archiveDir, err := os.MkdirTemp("", "hoserva-config-archive-*")
	if err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(archiveDir) }()

	name := archiveName(now)
	archivePath := filepath.Join(archiveDir, name)
	if err := packArchive(staging, archivePath); err != nil {
		return fmt.Errorf("packing archive: %w", err)
	}

	passphrase := ""
	if s.Secrets != nil {
		if p, ok, err := s.Secrets.BackupPassphrase(ctx); err != nil {
			return fmt.Errorf("reading backup passphrase for verification: %w", err)
		} else if ok {
			passphrase = p
		}
	}
	if err := VerifyArchive(archivePath, passphrase); err != nil {
		return fmt.Errorf("verifying archive: %w", err)
	}

	var artifacts *encryptedArtifacts
	for _, dest := range s.Destinations {
		if !dest.Enabled {
			continue
		}
		writePath, writeName := archivePath, name
		if dest.Encrypt {
			if artifacts == nil {
				// Written into archiveDir (both derived from archivePath's
				// own directory), so archiveDir's own deferred removal
				// above covers these too.
				artifacts, err = s.buildArtifacts(archivePath, passphrase)
				if err != nil {
					return fmt.Errorf("encrypting archive for destination %q: %w", dest.ID, err)
				}
			}
			writePath, writeName = artifacts.ArchivePath, filepath.Base(artifacts.ArchivePath)
			if err := writeArchive(dest, artifacts.SidecarPath); err != nil {
				return fmt.Errorf("writing destination %q: %w", dest.ID, err)
			}
		}
		if err := writeArchive(dest, writePath); err != nil {
			return fmt.Errorf("writing destination %q: %w", dest.ID, err)
		}
		if err := pruneDestination(dest, now, writeName); err != nil {
			return fmt.Errorf("pruning destination %q: %w", dest.ID, err)
		}
	}
	return nil
}

// buildArtifacts age-encrypts archivePath to s.Recipient's public key and
// wraps the matching private identity under passphrase into a sidecar
// file (Q80) — called at most once per Run, and reused across every
// destination that requests encryption.
func (s *Service) buildArtifacts(archivePath, passphrase string) (*encryptedArtifacts, error) {
	if s.Recipient == nil || s.Recipient.Public == "" || s.Recipient.Identity == "" {
		return nil, fmt.Errorf("encryption requested but no onboarding recipient is available")
	}
	if passphrase == "" {
		return nil, fmt.Errorf("encryption requires a backup passphrase to be set")
	}
	sidecar, err := encryptWithPassphrase([]byte(s.Recipient.Identity), passphrase)
	if err != nil {
		return nil, fmt.Errorf("wrapping onboarding recipient identity: %w", err)
	}
	return buildEncryptedArtifacts(archivePath, s.Recipient.Public, sidecar)
}

func archiveName(now time.Time) string {
	return fmt.Sprintf("hoserva-config-%s.tar.zst", now.UTC().Format("2006-01-02T15-04"))
}
