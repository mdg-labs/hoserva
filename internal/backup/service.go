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

// Reason marks an archive as taken immediately before a destructive change,
// exempting it from ordinary daily-tier pruning (doc 10 §1, #401). The zero
// value, ReasonNone, is an ordinary nightly archive.
type Reason string

const (
	ReasonNone        Reason = ""
	ReasonPreImport   Reason = "pre-import"
	ReasonPreUpdate   Reason = "pre-update"
	ReasonPreTopology Reason = "pre-topology"
)

func (r Reason) valid() bool {
	switch r {
	case ReasonNone, ReasonPreImport, ReasonPreUpdate, ReasonPreTopology:
		return true
	default:
		return false
	}
}

// Run builds an ordinary config archive, verifies it, and writes it to
// every enabled local destination with retention pruning. It is job.
// ConfigBackup and update.ConfigBackup's method — the nightly chain's last
// step reaches it through this exact signature — so a caller that needs to
// mark the archive as taken before a destructive change calls RunReason
// directly, or through an adapter satisfying one of those two interfaces
// (cmd/hoservad/update.go's preUpdateBackup).
func (s *Service) Run(ctx context.Context) error {
	return s.RunReason(ctx, ReasonNone)
}

// RunReason is Run, with the archive marked as taken before a destructive
// change (doc 10 §1, #401): the pre-import safety backup
// (internal/api/pool_handler.go's ImportConfig) and the pre-update backup
// (cmd/hoservad/update.go) both call it so retention keeps their archive
// even when a later same-day backup would otherwise take today's daily-tier
// slot and prune it.
func (s *Service) RunReason(ctx context.Context, reason Reason) error {
	if !reason.valid() {
		return fmt.Errorf("backup: unknown reason %q", reason)
	}
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

	name := resolveArchiveName(now, reason, s.Destinations)
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

// archiveName formats one candidate archive filename. suffix is 0 for the
// unadorned name; any value 2 or above appends "-<suffix>" to the
// timestamp to resolve a collision (#401) — 1 is never passed, since the
// first archive at a given second is the unsuffixed name. Second
// resolution (not doc 10 §1's original minute resolution) is itself part
// of that fix: two runs in the same minute now produce different base
// names before collision suffixing is even needed.
func archiveName(now time.Time, reason Reason, suffix int) string {
	ts := now.UTC().Format("2006-01-02T15-04-05")
	if suffix > 0 {
		ts = fmt.Sprintf("%s-%d", ts, suffix)
	}
	if reason != ReasonNone {
		return fmt.Sprintf("hoserva-config-%s.%s.tar.zst", ts, reason)
	}
	return fmt.Sprintf("hoserva-config-%s.tar.zst", ts)
}

// resolveArchiveName picks the first candidate archiveName produces that no
// enabled destination already has on disk, plain or age-encrypted (#401):
// second resolution alone still collides when two runs share a wall-clock
// second, as every fake-clock test in this package and any two real runs
// launched from the same request do. Checking every enabled destination,
// not just one, keeps a single archive name meaningful across all of them
// for the same run.
func resolveArchiveName(now time.Time, reason Reason, destinations []Destination) string {
	name := archiveName(now, reason, 0)
	if !archiveNameTaken(name, destinations) {
		return name
	}
	for suffix := 2; ; suffix++ {
		name := archiveName(now, reason, suffix)
		if !archiveNameTaken(name, destinations) {
			return name
		}
	}
}

func archiveNameTaken(name string, destinations []Destination) bool {
	for _, dest := range destinations {
		if !dest.Enabled {
			continue
		}
		if _, err := os.Stat(filepath.Join(dest.Path, name)); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(dest.Path, name+".age")); err == nil {
			return true
		}
	}
	return false
}
