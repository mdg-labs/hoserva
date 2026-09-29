package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// Service implements job.ConfigBackup (doc 10 §1, Q30).
type Service struct {
	DB    *sql.DB
	Paths Paths
	// Destinations is the fixed destination list a Service without a Store
	// writes to. With a Store, the stored destinations are used instead.
	Destinations []Destination
	// Store holds the operator-managed destinations (D4). RunReason reads
	// it on every run, so an added or removed destination takes effect on
	// the next backup without a restart.
	Store DestinationStore
	// Drills holds the last restore drill's result (RunDrill).
	Drills DrillStore
	// Rclone runs rclone for remote destinations. Nil uses ExecRclone.
	Rclone RcloneRunner
	// DestinationCipher seals a new remote destination's credentials under
	// the machine key (Q28); Cipher unseals them.
	DestinationCipher RecipientCipher
	Secrets           SecretSource
	Cipher            SecretCipher
	Recipient         *Recipient
	Hostname          string
	Version           string
	Now               func() time.Time

	// PoolRoot is the pool's own catch-all mount root (doc 02 §1, Q12) a
	// destination path is compared against, by path component rather than
	// string prefix (#409: "/mnt/username" is not under "/mnt/user"), to
	// decide whether it needs a mount check before every write. Empty uses
	// pool.CatchAllPath; a test points it at a directory it fully controls.
	PoolRoot string
	// PoolMounted reports whether PoolRoot is currently mounted (#409): the
	// same stat-based device-id comparison pool.Mounter's own Mount/Unmount
	// idempotency already uses (pool.IsMountedConfirmed), reused here rather
	// than duplicated. Nil uses pool.IsMountedConfirmed. A test injects a
	// fake so "pool unmounted while the array is stopped" is observable
	// without a real mount.
	PoolMounted func(path string) (bool, error)
	// Log receives one line per destination Run skips because the pool is
	// not mounted (#409) — defaults to log.Printf. Never the only record: a
	// caller that wants a skip reflected in a job's own output wraps Run or
	// RunReason and reads its returned error, which reports the case where
	// every enabled destination was skipped.
	Log func(format string, args ...any)
	// PoolWriteGate, when set, admits or refuses a write to a destination
	// under the pool's own mount root, in addition to the mount check
	// above (#409): ArraySequence.Stop closes it before it unmounts the
	// pool, so a write already admitted here finishes before that unmount
	// runs, and a write attempted after Close is refused outright rather
	// than raced against it. Nil never refuses — the zero-value default,
	// matching a Service built before job.ArraySequence is wired to one.
	PoolWriteGate *PoolWriteGate

	destMu sync.Mutex
}

// PoolWriteGate coordinates a config backup's write to a destination under
// the pool's own mount root with the array's own mount lifecycle (#409).
// The zero value is open, matching an unwired daemon where the mount check
// above already refuses an unmounted pool on its own.
type PoolWriteGate struct {
	mu       sync.Mutex
	closed   bool
	inFlight int
	waiters  []chan struct{}
}

// begin admits one pool-destination write, refusing once Close has run.
// Every successful call must be matched by exactly one call to end, once
// the write (and its prune) is done.
func (g *PoolWriteGate) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.inFlight++
	return true
}

// end records that a write admitted by begin has finished.
func (g *PoolWriteGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight == 0 {
		return
	}
	g.inFlight--
	if g.inFlight == 0 {
		for _, ch := range g.waiters {
			close(ch)
		}
		g.waiters = nil
	}
}

// Close refuses every new pool-destination write from this call onward and
// waits for any already in flight to finish, honoring ctx (#409):
// job.ArraySequence.Stop calls this before it unmounts the pool.
func (g *PoolWriteGate) Close(ctx context.Context) error {
	g.mu.Lock()
	g.closed = true
	if g.inFlight == 0 {
		g.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	g.waiters = append(g.waiters, ch)
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("backup: waiting for an in-flight pool-destination write to finish: %w", ctx.Err())
	}
}

// Open re-allows pool-destination writes (#409): job.ArraySequence.Start
// calls this once the pool is actually mounted again.
func (g *PoolWriteGate) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = false
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

	dests, err := s.loadDestinations(ctx)
	if err != nil {
		return err
	}
	name := resolveArchiveName(s.installationID(), now, reason, dests)
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
	var failures []error
	wrote := false
	skipped := false
	for _, dest := range dests {
		if !dest.Enabled {
			continue
		}

		release, why := s.admitDestination(dest)
		if why != "" {
			s.log("skipping destination %q: %s", dest.ID, why)
			skipped = true
			continue
		}
		written, err := s.writeDestination(ctx, dest, archivePath, name, passphrase, now, &artifacts)
		release()
		if written {
			wrote = true
		}
		if err != nil {
			failures = append(failures, err)
		}
	}

	// A destination that failed while another was written is logged and
	// left to the stale-destination alert (doc 10 §1); a run left with
	// nothing written anywhere fails closed (#409), so a pre-change backup
	// refuses the change it guards rather than report a snapshot that was
	// never taken. A Service with no enabled destination at all (nothing
	// to skip) is unrelated — that configuration is a no-op.
	if wrote {
		for _, err := range failures {
			s.log("%v", err)
		}
		return nil
	}
	if skipped {
		return errors.Join(append([]error{fmt.Errorf("backup: every enabled destination was skipped or unavailable")}, failures...)...)
	}
	return errors.Join(failures...)
}

func (s *Service) loadDestinations(ctx context.Context) ([]Destination, error) {
	if s.Store == nil {
		return s.Destinations, nil
	}
	dests, err := s.Store.ListDestinations(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing backup destinations: %w", err)
	}
	return dests, nil
}

// admitDestination decides whether dest may be written to right now. A
// destination under the pool's own mount root needs a live mount check
// before every write (#409): with the array stopped, CatchAllPath is a
// bare, empty directory on the root filesystem — os.MkdirAll inside
// writeArchive would happily create it there, the archive would be
// written onto the boot device without saying so, and it would be hidden
// the moment the pool mounts back over it. A destination anywhere else
// (the boot device, an external disk, a remote) is unaffected by the
// array's own mount state and is always admitted.
//
// PoolWriteGate.begin runs before the mount check itself (#409): once it
// admits a write, job.ArraySequence.Stop's own Close call cannot return —
// and so cannot let an unmount proceed — until the returned release runs,
// closing the exact race a mount check alone cannot: mount confirmed
// live, then torn down before the write that check was guarding ever
// reaches disk. A non-empty reason means the destination was not
// admitted; release is then a no-op.
func (s *Service) admitDestination(dest Destination) (release func(), reason string) {
	noop := func() {}
	if dest.isRemote() || !underPoolRoot(dest.Path, s.poolRoot()) {
		return noop, ""
	}
	release = noop
	if s.PoolWriteGate != nil {
		if !s.PoolWriteGate.begin() {
			return noop, "the pool is closed for writing while the array is stopping"
		}
		release = s.PoolWriteGate.end
	}
	mounted, err := s.poolMounted()
	switch {
	case err != nil:
		release()
		return noop, fmt.Sprintf("confirming the pool is mounted at %q: %v", s.poolRoot(), err)
	case !mounted:
		release()
		return noop, fmt.Sprintf("the pool is not mounted at %q", s.poolRoot())
	}
	return release, ""
}

// writeDestination writes the archive (and, when dest encrypts, its
// identity sidecar) to dest, records the success, and prunes. written is
// true once the archive itself is on the destination, even if the prune
// that follows fails.
func (s *Service) writeDestination(ctx context.Context, dest Destination, archivePath, name, passphrase string, now time.Time, artifacts **encryptedArtifacts) (written bool, err error) {
	if dest.isRemote() && !dest.Encrypt {
		return false, fmt.Errorf("writing destination %q: a remote destination is never written unencrypted (Q80)", dest.ID)
	}
	writePath, writeName, sidecarPath := archivePath, name, ""
	if dest.Encrypt {
		if *artifacts == nil {
			// Written into the run's own archive directory (both derived
			// from archivePath's own directory), so its deferred removal
			// covers these too.
			a, err := s.buildArtifacts(archivePath, passphrase)
			if err != nil {
				return false, fmt.Errorf("encrypting archive for destination %q: %w", dest.ID, err)
			}
			*artifacts = a
		}
		writePath, writeName, sidecarPath = (*artifacts).ArchivePath, filepath.Base((*artifacts).ArchivePath), (*artifacts).SidecarPath
	}

	target, err := s.targetFor(ctx, dest)
	if err != nil {
		return false, fmt.Errorf("preparing destination %q: %w", dest.ID, err)
	}
	if sidecarPath != "" {
		if err := target.write(ctx, sidecarPath); err != nil {
			return false, fmt.Errorf("writing destination %q: %w", dest.ID, err)
		}
	}
	if err := target.write(ctx, writePath); err != nil {
		return false, fmt.Errorf("writing destination %q: %w", dest.ID, err)
	}
	if s.Store != nil {
		if err := s.Store.RecordBackupSuccess(ctx, dest.ID, now); err != nil && !errors.Is(err, ErrDestinationNotFound) {
			return true, fmt.Errorf("recording the backup to destination %q: %w", dest.ID, err)
		}
	}
	if err := pruneTarget(ctx, target, s.archiveOwner(dest), dest.Retention, now, writeName); err != nil {
		return true, fmt.Errorf("pruning destination %q: %w", dest.ID, err)
	}
	return true, nil
}

// poolRoot is PoolRoot's default, pool.CatchAllPath.
func (s *Service) poolRoot() string {
	if s.PoolRoot != "" {
		return s.PoolRoot
	}
	return pool.CatchAllPath
}

// poolMounted is PoolMounted's default, pool.IsMountedConfirmed against
// poolRoot().
func (s *Service) poolMounted() (bool, error) {
	if s.PoolMounted != nil {
		return s.PoolMounted(s.poolRoot())
	}
	return pool.IsMountedConfirmed(s.poolRoot())
}

func (s *Service) log(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
		return
	}
	log.Printf("backup: "+format, args...)
}

// underPoolRoot reports whether path lies at or under root, compared by
// path component rather than string prefix (#409): "/mnt/username" is not
// under "/mnt/user" even though it shares that string prefix.
func underPoolRoot(path, root string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
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

// installationID is the id every archive name carries, so a destination
// shared with another installation can tell whose archive is whose. It is
// derived from the onboarding recipient's public key, which is generated
// once per installation and never rotated (Q80), and falls back to the
// hostname for a Service built without one.
func (s *Service) installationID() string {
	seed := "host:" + s.Hostname
	if s.Recipient != nil && s.Recipient.Public != "" {
		seed = "recipient:" + s.Recipient.Public
	}
	sum := sha256.Sum256([]byte("hoserva-backup-installation-v1\x00" + seed))
	return hex.EncodeToString(sum[:])[:12]
}

// archiveOwner is what retention on dest may remove: this installation's
// archives, plus legacy ones on the default destinations.
func (s *Service) archiveOwner(dest Destination) archiveOwner {
	return archiveOwner{installation: s.installationID(), legacy: isDefaultDestination(dest)}
}

// archiveName formats one candidate archive filename. suffix is 0 for the
// unadorned name; any value 2 or above appends "-<suffix>" to the
// timestamp to resolve a collision (#401) — 1 is never passed, since the
// first archive at a given second is the unsuffixed name. Second
// resolution (not doc 10 §1's original minute resolution) is itself part
// of that fix: two runs in the same minute now produce different base
// names before collision suffixing is even needed.
func archiveName(installation string, now time.Time, reason Reason, suffix int) string {
	ts := now.UTC().Format("2006-01-02T15-04-05")
	if suffix > 0 {
		ts = fmt.Sprintf("%s-%d", ts, suffix)
	}
	if reason != ReasonNone {
		return fmt.Sprintf("hoserva-config-%s-%s.%s.tar.zst", installation, ts, reason)
	}
	return fmt.Sprintf("hoserva-config-%s-%s.tar.zst", installation, ts)
}

// resolveArchiveName picks the first candidate archiveName produces that no
// enabled destination already has on disk, plain or age-encrypted (#401):
// second resolution alone still collides when two runs share a wall-clock
// second, as every fake-clock test in this package and any two real runs
// launched from the same request do. Checking every enabled destination,
// not just one, keeps a single archive name meaningful across all of them
// for the same run.
func resolveArchiveName(installation string, now time.Time, reason Reason, destinations []Destination) string {
	name := archiveName(installation, now, reason, 0)
	if !archiveNameTaken(name, destinations) {
		return name
	}
	for suffix := 2; ; suffix++ {
		name := archiveName(installation, now, reason, suffix)
		if !archiveNameTaken(name, destinations) {
			return name
		}
	}
}

func archiveNameTaken(name string, destinations []Destination) bool {
	for _, dest := range destinations {
		if !dest.Enabled || dest.isRemote() {
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
