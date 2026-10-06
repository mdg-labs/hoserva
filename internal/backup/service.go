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

	"github.com/mdg-labs/hoserva/internal/disk"
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
	// MigrationUnfinished reports whether an Unraid migration is anywhere
	// between its adoption and its point of no return. The pool is mounted
	// read-only while it is pending and read-write once parity initialises
	// (doc 05 §4), but is left alone throughout. A write to a destination
	// under the pool root is refused for as long as it is true, and when it
	// cannot be read: an error is never taken to mean "no migration". Nil
	// skips the check, as a Service built for a host with no migration
	// state does.
	MigrationUnfinished func(ctx context.Context) (bool, error)

	// ExternalRoot is where external disks mount, /mnt/disks/<label> (Q72),
	// a destination path is compared against by path component to decide
	// whether the disk it lives on must be mounted before every write. Empty
	// uses disk.ExternalMountRoot; a test points it at a directory it fully
	// controls.
	ExternalRoot string
	// ExternalMounted reports whether path is a mount point in the kernel
	// mount table. Nil reads /proc/self/mountinfo (disk.KernelMounts); a test
	// injects a fake so an ejected disk is observable without a real mount.
	ExternalMounted func(ctx context.Context, path string) (bool, error)
	// ExternalGates, when set, gives each external disk a write gate that an
	// eject of that disk closes (#454): a write admitted to /mnt/disks/<label>
	// finishes before the disk is unmounted, and none is admitted after. Nil
	// never refuses — the mount check alone then guards the write.
	ExternalGates *ExternalWriteGates

	destMu sync.Mutex
	// archiveMu keeps a config backup's write and prune of a destination
	// from landing between a restore drill's listing of it and its fetch of
	// the archive it picked (#444).
	archiveMu sync.Mutex
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
		return fmt.Errorf("backup: waiting for an in-flight destination write to finish: %w", ctx.Err())
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
	_, err := s.RunReasonArchive(ctx, reason)
	return err
}

// WrittenArchive is the archive a run wrote and the names of the
// destinations it wrote it to.
type WrittenArchive struct {
	Name         string
	Destinations []string
	// Failed lists the destinations a step failed on while the run still
	// wrote another: a write that did not land, or one that landed and
	// then failed to record its success or to prune. A destination can be
	// in both lists.
	Failed []DestinationFailure
	// SecretsSealed is whether the archive holds a secrets.age.
	SecretsSealed bool
}

// DestinationFailure is one destination's failed step in a run.
type DestinationFailure struct {
	Destination string
	Err         error
}

// RunOption changes how RunReasonArchive builds its archive.
type RunOption func(*runOptions)

type runOptions struct {
	sealSecrets string
	hostRoot    string
	hostFiles   []string
}

// CaptureHostFiles saves each of rels, relative to root, in the archive
// (WithHostFiles), so a change that replaces them leaves a copy. A file that
// cannot be saved fails the run.
func CaptureHostFiles(root string, rels []string) RunOption {
	return func(o *runOptions) {
		o.hostRoot = root
		o.hostFiles = rels
	}
}

// SealSecretsWith seals the archive's secrets.age under passphrase, and
// verifies it against that passphrase, instead of the configured backup
// passphrase. The identity sidecar and an encrypted destination's artifacts
// still use the configured one. Empty, it changes nothing.
func SealSecretsWith(passphrase string) RunOption {
	return func(o *runOptions) { o.sealSecrets = passphrase }
}

// RunReasonArchive is RunReason that also reports the archive it wrote, so
// a caller guarding a destructive change can name the backup to restore
// from. The result is empty when the run fails.
func (s *Service) RunReasonArchive(ctx context.Context, reason Reason, opts ...RunOption) (WrittenArchive, error) {
	var run runOptions
	for _, opt := range opts {
		opt(&run)
	}
	if !reason.valid() {
		return WrittenArchive{}, fmt.Errorf("backup: unknown reason %q", reason)
	}
	if s.DB == nil {
		return WrittenArchive{}, fmt.Errorf("backup: no database configured")
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}

	staging, err := os.MkdirTemp("", "hoserva-config-staging-*")
	if err != nil {
		return WrittenArchive{}, fmt.Errorf("creating staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	manifest, err := BuildArchive(ctx, s.DB, s.Paths, s.Secrets, s.Cipher, s.Hostname, s.Version, now, staging, WithRecipient(s.Recipient), WithSecretsPassphrase(run.sealSecrets), WithHostFiles(run.hostRoot, run.hostFiles))
	if err != nil {
		return WrittenArchive{}, err
	}

	// Packed into a directory private to this run — not staging, which
	// packArchive is about to walk, and not a name derived only from now,
	// which two runs in the same minute would compute identically and
	// race on (#405). buildArtifacts derives the encrypted archive and
	// identity sidecar paths from archivePath's own directory, so this
	// also isolates those.
	archiveDir, err := os.MkdirTemp("", "hoserva-config-archive-*")
	if err != nil {
		return WrittenArchive{}, fmt.Errorf("creating archive directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(archiveDir) }()

	dests, err := s.loadDestinations(ctx)
	if err != nil {
		return WrittenArchive{}, err
	}
	name := resolveArchiveName(s.installationID(), now, reason, dests)
	archivePath := filepath.Join(archiveDir, name)
	if err := packArchive(staging, archivePath); err != nil {
		return WrittenArchive{}, fmt.Errorf("packing archive: %w", err)
	}

	passphrase := ""
	if s.Secrets != nil {
		if p, ok, err := s.Secrets.BackupPassphrase(ctx); err != nil {
			return WrittenArchive{}, fmt.Errorf("reading backup passphrase for verification: %w", err)
		} else if ok {
			passphrase = p
		}
	}
	verifyWith := passphrase
	if run.sealSecrets != "" {
		verifyWith = run.sealSecrets
	}
	if err := VerifyArchive(archivePath, verifyWith); err != nil {
		return WrittenArchive{}, fmt.Errorf("verifying archive: %w", err)
	}

	_, sealed := manifest.Checksums["secrets.age"]
	var artifacts *encryptedArtifacts
	var failures []error
	var writtenTo []string
	var failed []DestinationFailure
	wrote := false
	var skips []error
	for _, dest := range dests {
		if !dest.Enabled {
			continue
		}

		release, why := s.admitWrite(ctx, dest)
		if why != "" {
			s.log("skipping destination %q: %s", dest.ID, why)
			skips = append(skips, fmt.Errorf("destination %q skipped: %s", dest.ID, why))
			failed = append(failed, DestinationFailure{Destination: destinationLabel(dest), Err: errors.New(why)})
			continue
		}
		written, err := s.writeDestination(ctx, dest, archivePath, name, passphrase, now, &artifacts)
		release()
		if written {
			wrote = true
			writtenTo = append(writtenTo, destinationLabel(dest))
		}
		if err != nil {
			failures = append(failures, err)
			failed = append(failed, DestinationFailure{Destination: destinationLabel(dest), Err: err})
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
		return WrittenArchive{Name: name, Destinations: writtenTo, Failed: failed, SecretsSealed: sealed}, nil
	}
	if len(skips) > 0 {
		return WrittenArchive{}, errors.Join(append(append([]error{fmt.Errorf("backup: every enabled destination was skipped or unavailable")}, skips...), failures...)...)
	}
	return WrittenArchive{}, errors.Join(failures...)
}

func destinationLabel(d Destination) string {
	if d.Name != "" {
		return d.Name
	}
	return d.ID
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
// the moment the pool mounts back over it. A destination on an external
// disk (under /mnt/disks/<label>) needs the same check against that
// disk's own mount, for the same reason: with the disk ejected, the write
// would create the directory on the boot device. A destination anywhere else
// (the boot device, a remote) is unaffected by any mount state and is always
// admitted.
//
// A gate slot is taken before the mount check itself, for a pool destination
// from PoolWriteGate (#409) and for an external one from that disk's gate in
// ExternalGates (#454): once it admits a write, job.ArraySequence.Stop's
// Close, or an eject of the disk, cannot return — and so cannot let an
// unmount proceed — until the returned release runs, closing the exact race
// a mount check alone cannot: mount confirmed live, then torn down before
// the write that check was guarding ever reaches disk. A non-empty reason
// means the destination was not admitted; release is then a no-op.
func (s *Service) admitDestination(ctx context.Context, dest Destination) (release func(), reason string) {
	noop := func() {}
	if dest.isRemote() {
		return noop, ""
	}
	if mountPoint, ok := externalMountPoint(dest.Path, s.externalRoot()); ok {
		return s.admitExternal(ctx, mountPoint)
	}
	if !underPoolRoot(dest.Path, s.poolRoot()) {
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

// admitWrite is admitDestination for a caller that writes: a destination
// under the pool root is also refused while a migration is unfinished: the
// pool is read-only while it is pending (doc 05 §4), where the write would
// fail with EROFS, and is left alone until the migration finishes. Reading
// from the pool stays admitted.
func (s *Service) admitWrite(ctx context.Context, dest Destination) (release func(), reason string) {
	if s.MigrationUnfinished != nil && s.isPoolDestination(dest) {
		unfinished, err := s.MigrationUnfinished(ctx)
		switch {
		case err != nil:
			return func() {}, fmt.Sprintf("confirming no migration is pending: %v", err)
		case unfinished:
			return func() {}, "the pool is not written to until the Unraid migration is finished"
		}
	}
	return s.admitDestination(ctx, dest)
}

func (s *Service) isPoolDestination(dest Destination) bool {
	if dest.isRemote() {
		return false
	}
	if _, ok := externalMountPoint(dest.Path, s.externalRoot()); ok {
		return false
	}
	return underPoolRoot(dest.Path, s.poolRoot())
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
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
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

// externalMountPoint returns the mount point of the external disk a path
// under root belongs to: root/<label>, whatever lies below it. A path that
// is root itself has no disk to be mounted, so root is returned and is
// never in the mount table.
func externalMountPoint(path, root string) (string, bool) {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if !underPoolRoot(path, root) {
		return "", false
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(path, root), string(filepath.Separator))
	if rel == "" {
		return root, true
	}
	label, _, _ := strings.Cut(rel, string(filepath.Separator))
	return filepath.Join(root, label), true
}

// admitExternal takes mountPoint's disk gate slot, if the disk has one,
// before confirming the disk is mounted. The external root itself is no disk
// and has no gate; it is never in the mount table.
func (s *Service) admitExternal(ctx context.Context, mountPoint string) (release func(), reason string) {
	noop := func() {}
	release = noop
	if mountPoint != filepath.Clean(s.externalRoot()) {
		label := filepath.Base(mountPoint)
		var ok bool
		if release, ok = s.ExternalGates.begin(label); !ok {
			return noop, fmt.Sprintf("the external disk %q is ejected or being ejected", label)
		}
	}
	if why := s.externalMountRefusal(ctx, mountPoint); why != "" {
		release()
		return noop, why
	}
	return release, ""
}

// externalMountRefusal is empty when mountPoint is confirmed mounted. A
// mount table that cannot be read is a refusal too: an external
// destination on an ejected disk would otherwise be created on the boot
// device (doc 10 §1). The table is the kernel's mountinfo — the disk itself
// is never listed or read (Q13).
func (s *Service) externalMountRefusal(ctx context.Context, mountPoint string) string {
	mounted, err := s.externalMounted(ctx, mountPoint)
	switch {
	case err != nil:
		return fmt.Sprintf("confirming the external disk is mounted at %q: %v", mountPoint, err)
	case !mounted:
		return fmt.Sprintf("the external disk is not mounted at %q", mountPoint)
	}
	return ""
}

func (s *Service) externalRoot() string {
	if s.ExternalRoot != "" {
		return s.ExternalRoot
	}
	return disk.ExternalMountRoot
}

func (s *Service) externalMounted(ctx context.Context, path string) (bool, error) {
	if s.ExternalMounted != nil {
		return s.ExternalMounted(ctx, path)
	}
	return disk.KernelMounts{}.IsMounted(ctx, path)
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
