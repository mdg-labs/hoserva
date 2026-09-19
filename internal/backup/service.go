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

	_, err = BuildArchive(ctx, s.DB, s.Paths, s.Secrets, s.Cipher, s.Hostname, s.Version, now, staging)
	if err != nil {
		return err
	}

	name := archiveName(now)
	archivePath := filepath.Join(os.TempDir(), name)
	defer func() { _ = os.Remove(archivePath) }()
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

	for _, dest := range s.Destinations {
		if !dest.Enabled {
			continue
		}
		if err := writeArchive(dest, archivePath); err != nil {
			return fmt.Errorf("writing destination %q: %w", dest.ID, err)
		}
		if err := pruneDestination(dest, now, name); err != nil {
			return fmt.Errorf("pruning destination %q: %w", dest.ID, err)
		}
	}
	return nil
}

func archiveName(now time.Time) string {
	return fmt.Sprintf("hoserva-config-%s.tar.zst", now.UTC().Format("2006-01-02T15-04"))
}
