package cache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// tempSuffix marks an in-flight copy (doc 09 §2): a file named
// "<name>.hoserva-moving-<uuid>" is never mistaken for real data — it is
// excluded from SnapRAID (parity.DefaultExcludes' "*.hoserva-moving-*")
// and hidden from share browsing — but it is visible through the union
// mount until the rename, which is why the suffix has to be unambiguous.
const tempSuffix = ".hoserva-moving-"

// DefaultGracePeriod is doc 09 §2's own default: a file modified more
// recently than this is still probably being written to.
const DefaultGracePeriod = 5 * time.Minute

// errTargetAppeared is copyMoveFile's rename hitting a dst that did not
// exist at processFile's Lstat check but does now — something else wrote
// it through the array mount during the copy. processFile maps this to
// ResultConflict rather than ResultFailed, since it is the same
// never-auto-resolved conflict the earlier check exists to catch.
var errTargetAppeared = errors.New("cache: array target appeared during copy")

// leftBehindReason is the Reason of every ResultLeftBehind entry.
const leftBehindReason = "still on the source; an earlier run of this job left it and its reason was not carried across the resume"

// socketReason is the Reason of every skipped socket entry.
const socketReason = "runtime-only: its owner recreates it when it starts"

// Share is what one mover pass needs about a single cache-then-move
// share. ArrayPath is the share's own array-only mergerfs mount (doc 02
// §1, pool.MoverTargetPath) — the mover writes only through it, so
// mergerfs — not this package — decides which disk a moved file lands on
// (CLAUDE.md: "one placement algorithm"). Branches are the share's
// per-disk directories on the data disks' own mountpoints, in that
// mount's branch order — the same directories its branches name through
// each disk's nosymfollow bind (pool.MoverTargetMount); when empty, the
// space pre-check falls back to ArrayPath's own pool-wide free space. Exclude is a set of
// path/filepath.Match patterns, tried against both a file's path relative
// to the share root and its base name.
type Share struct {
	Name         string
	CachePath    string
	ArrayPath    string
	Branches     []string
	MinFreeSpace int64
	Exclude      []string
	// PathPreserving marks a share using one of mergerfs's path-preserving
	// create policies (`ep*`, doc 09 §1). PlanRebalance warns rather than
	// silently proceeding when its own plan would place a file on a
	// branch that does not already hold that file's parent path — doing
	// so changes future placement behaviour for that directory under such
	// a policy (doc 09 §3's "Path-preserving caveat").
	PathPreserving bool
}

// Config is a mover run's tunable behaviour.
type Config struct {
	// GracePeriod overrides DefaultGracePeriod.
	GracePeriod time.Duration
	// VerifyChecksum adds a SHA-256 comparison between source and target
	// on top of the always-on size check (doc 09 §2: "verify (size
	// always; checksum optionally)").
	VerifyChecksum bool
	// SkipGracePeriod makes Run treat every file as eligible regardless
	// of how recently it was modified — doc 09 §2's own "[cache to array
	// relocation] behaves as a mover run limited to one share, without
	// the grace period" (#54, RelocateToArray). It is never set by a
	// scheduled or threshold-triggered mover pass, only by an explicit,
	// single-share relocation the user asked for.
	SkipGracePeriod bool
}

// Deps are Run's system-touching dependencies (CLAUDE.md: "every
// system-touching subsystem sits behind a package interface with a
// scriptable fake"). Zero-value fields are filled with the real
// implementation by withDefaults.
type Deps struct {
	Open  OpenChecker
	Avail func(path string) (int64, error)
	Now   func() time.Time
	UUID  func() string
	// FsyncDir fsyncs the target directory a copy was published in,
	// through its descriptor; defaults to unix.Fsync.
	FsyncDir func(dirfd int) error
	// Mknod creates a FIFO or device node named name in the directory
	// dirfd (mknodat(2)); defaults to unix.Mknodat. A filesystem or a
	// missing capability can refuse it, and the entry is then reported as
	// failed.
	Mknod func(dirfd int, name string, mode uint32, dev int) error
	// Sync is RelocateToCache's own dependency (#54): a caller-supplied
	// adapter onto parity.Engine.Sync, kept out of this package's own
	// imports the same way RunHooks avoids importing internal/job (see
	// doc.go) — the wiring that converts a []parity.ManifestEntry into a
	// real, threshold-guarded parity.SyncOpts.Manifest call belongs with
	// the rest of that wiring in internal/job, alongside RunMover. Run
	// and RelocateToArray never call it; it has no default and is
	// required by RelocateToCache and RunRebalance.
	Sync SyncFunc
	// Usage reports one branch's own filesystem usage (total and free
	// bytes) — PlanRebalance's target-selection input (doc 09 §3):
	// finding a share's own most- and least-full disk needs each
	// branch's fill level, not merely whether size bytes fits (Avail's
	// own, narrower question). Defaults to UsageBytes (statfs.go),
	// matching Avail's own per-path statfs pattern.
	Usage func(path string) (DiskUsage, error)
	// RebalanceBatchLimit caps a rebalance batch by plain file count,
	// independent of anything about the array's current state (doc 02
	// §2's RemovedFilesMax rule: a fixed count, not a percentage). Nil or
	// non-positive uses DefaultRebalanceBatchLimit, a fixed margin safely
	// under parity.DefaultRemovedFilesMax (500) — safe by construction
	// for an array of any size, unlike RebalancePercentLimit or
	// TrackedFileCount below, neither of which can be defaulted the same
	// way.
	RebalanceBatchLimit func() int
	// RebalancePercentLimit is the percent-rule threshold a rebalance
	// batch must stay under (parity's own RemovedUpdatedPercent rule).
	// Nil, non-positive or >=100 uses parity.DefaultRemovedUpdatedPercent
	// — safe to default because it is the guard's own threshold value,
	// not an estimate of the array's state: defaulting it to the same
	// constant the guard itself defaults to is the conservative choice
	// whenever a caller hasn't configured the guard differently.
	RebalancePercentLimit func() float64
	// TrackedFileCount returns the array's current tracked file count —
	// the same denominator the threshold guard's own next Evaluate call
	// will divide by when it computes RemovedUpdatedPercent
	// (guard.go's Evaluate, "totalBefore": DiffReport.RegularFilesBefore
	// of a fresh diff, the regular files across every disk without the
	// links PerDisk[mount].FilesBefore also counts). A real caller wires
	// this to something that calls parity.Engine.Diff and reads
	// RegularFilesBefore exactly the way Evaluate does — never an independent filesystem walk, which would count files
	// SnapRAID's own parity.DefaultExcludes excludes and so overestimate
	// the true tracked count in exactly the unsafe direction. This value
	// must never be larger than what the guard's own next Evaluate call
	// would actually use; a smaller (staler) number only makes batch
	// sizing more conservative, never less safe.
	//
	// There is deliberately no filesystem-walk fallback and no numeric
	// default: a nil TrackedFileCount makes RunRebalance refuse outright
	// (ErrRebalanceTrackedCountRequired) rather than guess at this one
	// value, unlike RebalanceBatchLimit and RebalancePercentLimit above,
	// both of which are safe to default because they are threshold
	// constants, not estimates of array state.
	TrackedFileCount func(ctx context.Context) (int, error)
}

func (d Deps) withDefaults() Deps {
	if d.Open == nil {
		d.Open = ProcOpenChecker{}
	}
	if d.Avail == nil {
		d.Avail = AvailableBytes
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.UUID == nil {
		d.UUID = uuid.NewString
	}
	if d.FsyncDir == nil {
		d.FsyncDir = fsyncDir
	}
	if d.Usage == nil {
		d.Usage = UsageBytes
	}
	if d.Mknod == nil {
		d.Mknod = unix.Mknodat
	}
	return d
}

// Checkpoint is Run's own resumable progress marker (Q29): the share
// being worked on, and the last relative path within it that Run fully
// handled (moved, skipped or failed — anything decided). Resuming skips
// forward past it rather than re-deciding files already decided; it
// never needs to skip already-moved files specially, since a moved
// file's source is gone and enumeration simply will not find it again.
type Checkpoint struct {
	ShareIndex int    `json:"share_index"`
	LastPath   string `json:"last_path"`
}

// RunHooks lets a caller observe and control one Run call without this
// package depending on internal/job — see doc.go. Every field is
// optional; a nil field is simply not called. StopRequested, SaveCheckpoint
// and SetProgress are deliberately shaped to be a direct passthrough from
// a *job.RunContext (StopRequested(), SaveCheckpoint, SetProgress).
type RunHooks struct {
	StopRequested  <-chan struct{}
	SaveCheckpoint func(data []byte) error
	SetProgress    func(pct int)
	Log            func(format string, args ...any)
}

func (h RunHooks) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

func (h RunHooks) checkpoint(cp Checkpoint) error {
	if h.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("cache: encode checkpoint: %w", err)
	}
	return h.SaveCheckpoint(data)
}

func (h RunHooks) progress(pct int) {
	if h.SetProgress != nil {
		h.SetProgress(pct)
	}
}

func (h RunHooks) stopRequested() bool {
	if h.StopRequested == nil {
		return false
	}
	select {
	case <-h.StopRequested:
		return true
	default:
		return false
	}
}

// Run executes one mover pass over shares, in order, resuming from
// initialCheckpoint when it is non-empty (doc 09 §2's algorithm, Q29).
// It returns the Report built so far even when it returns a non-nil
// error or stops early on a cancelled context or hooks.StopRequested —
// whatever was decided about a file before the stop is real and already
// checkpointed, matching "the scheduler marks the job interrupted once
// RunFunc returns, whatever it returns".
func Run(ctx context.Context, shares []Share, cfg Config, deps Deps, hooks RunHooks, initialCheckpoint []byte) (report Report, err error) {
	deps = deps.withDefaults()
	grace := cfg.GracePeriod
	switch {
	case cfg.SkipGracePeriod:
		grace = 0
	case grace <= 0:
		grace = DefaultGracePeriod
	}

	var cp Checkpoint
	if len(initialCheckpoint) > 0 {
		if err := json.Unmarshal(initialCheckpoint, &cp); err != nil {
			return Report{}, fmt.Errorf("cache: decode checkpoint: %w", err)
		}
	}

	report = Report{StartedAt: deps.Now()}
	// Every post-start return — including every early error return below
	// — must leave FinishedAt set: RunMover logs report.Summary() on an
	// error too (doc 09 §2's "honest reporting"), and Summary's duration
	// is FinishedAt.Sub(StartedAt), meaningless against a zero time.
	defer func() {
		if report.FinishedAt.IsZero() {
			report.FinishedAt = deps.Now()
		}
	}()

	plan := make([][]string, len(shares))
	total := 0
	for i, s := range shares {
		rels, err := enumerateEntries(s.CachePath)
		if err != nil {
			return report, fmt.Errorf("cache: enumerate share %q: %w", s.Name, err)
		}
		plan[i] = rels
		total += len(rels)
	}

	done := 0
shareLoop:
	for i, s := range shares {
		if i < cp.ShareIndex {
			done += len(plan[i])
			continue
		}
		if err := sweepStrayTemps(s.ArrayPath); err != nil {
			return report, fmt.Errorf("cache: clean up interrupted copies for share %q: %w", s.Name, err)
		}

		preCopyOpen, err := shareOpenChecker(ctx, deps.Open)
		if err != nil {
			return report, fmt.Errorf("cache: snapshot open files for share %q: %w", s.Name, err)
		}

		resumeAfter := ""
		if i == cp.ShareIndex {
			resumeAfter = cp.LastPath
		}

		for _, rel := range plan[i] {
			if resumeAfter != "" && rel <= resumeAfter {
				done++
				continue
			}
			if ctx.Err() != nil {
				report.Interrupted = true
				break shareLoop
			}
			if hooks.stopRequested() {
				report.Interrupted = true
				break shareLoop
			}

			entry := processFile(ctx, s, rel, grace, cfg, deps, preCopyOpen)
			report.add(entry)
			hooks.logf("mover: %s %s/%s%s", entry.Result, s.Name, rel, entry.reasonSuffix())

			done++
			if total > 0 {
				hooks.progress(done * 100 / total)
			}
			if err := hooks.checkpoint(Checkpoint{ShareIndex: i, LastPath: rel}); err != nil {
				return report, fmt.Errorf("cache: save checkpoint: %w", err)
			}
		}
	}

	report.FinishedAt = deps.Now()
	if !report.Interrupted && ctx.Err() != nil {
		return report, ctx.Err()
	}
	return report, nil
}

// shareOpenChecker returns the OpenChecker processFile's pre-copy check
// uses for one share's pass: a snapshot taken once, right here, when open
// implements Snapshotter (ProcOpenChecker does in production), so a pass
// over N files costs one /proc walk rather than N (#238). A checker that
// does not implement Snapshotter — including FakeOpenChecker, every
// existing test's double — is returned unchanged, so it is still called
// once per file exactly as before. The pre-unlink re-check in
// finishPendingDelete never goes through this: it always calls deps.Open
// directly, so it is guaranteed fresh against current process state.
func shareOpenChecker(ctx context.Context, open OpenChecker) (OpenChecker, error) {
	snapshotter, ok := open.(Snapshotter)
	if !ok {
		return open, nil
	}
	snap, err := snapshotter.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return snapshotChecker{snap}, nil
}

// processFile decides and, when eligible, executes one file's relocation
// (doc 09 §2's algorithm). It never returns an error itself — every
// outcome, including a real failure, is reported as an Entry so one bad
// file cannot abort an entire run. preCopyOpen answers the pre-copy open
// check, possibly from a snapshot taken once for the whole share
// (shareOpenChecker); the pre-unlink re-check inside finishPendingDelete
// always uses deps.Open directly instead, never preCopyOpen.
func processFile(ctx context.Context, s Share, rel string, grace time.Duration, cfg Config, deps Deps, preCopyOpen OpenChecker) Entry {
	src := filepath.Join(s.CachePath, rel)

	if matchExclude(s.Exclude, rel) {
		return Entry{Share: s.Name, Path: rel, Result: ResultSkippedExcluded}
	}

	srcInfo, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Entry{Share: s.Name, Path: rel, Result: ResultSkippedGone}
		}
		return Entry{Share: s.Name, Path: rel, Result: ResultFailed, Err: err.Error()}
	}
	entry := moveEntry(ctx, s, rel, srcInfo, grace, cfg, deps, preCopyOpen)
	entry.Kind = entryKind(srcInfo.Mode())
	if entry.Reason == "" && (entry.Result == ResultMoved || entry.Result == ResultMovedPendingDelete) {
		entry.Reason = hardLinkNote(srcInfo)
	}
	return entry
}

// moveEntry is processFile past the Lstat: srcInfo is src's own metadata,
// never followed through a symlink.
func moveEntry(ctx context.Context, s Share, rel string, srcInfo os.FileInfo, grace time.Duration, cfg Config, deps Deps, preCopyOpen OpenChecker) Entry {
	src := filepath.Join(s.CachePath, rel)
	dst := filepath.Join(s.ArrayPath, rel)

	if srcInfo.Mode()&fs.ModeSocket != 0 {
		return Entry{Share: s.Name, Path: rel, Result: ResultSkippedSocket, Reason: socketReason}
	}
	if entryKind(srcInfo.Mode()) == "other" {
		return Entry{Share: s.Name, Path: rel, Result: ResultSkippedNotRegular}
	}
	if deps.Now().Sub(srcInfo.ModTime()) < grace {
		return Entry{Share: s.Name, Path: rel, Result: ResultSkippedGrace}
	}

	// A previous run may have renamed the copy into place but not yet
	// removed the source — a duplicate, never a gap (doc 09 §2). Finish
	// that relocation rather than copying again — but only once dst is
	// established as actually being that copy: size alone is not enough,
	// since an array-side file can be independently rewritten to the same
	// size the cache source happens to have (isSamePendingCopy).
	if dstInfo, err := os.Lstat(dst); err == nil {
		same, checkErr := isSamePendingCopy(src, dst, srcInfo, dstInfo, cfg.VerifyChecksum)
		if checkErr != nil {
			return Entry{Share: s.Name, Path: rel, Result: ResultFailed, Err: checkErr.Error()}
		}
		if !same {
			return Entry{Share: s.Name, Path: rel, Result: ResultConflict}
		}
		entry := finishPendingDelete(ctx, s, rel, src, srcInfo.Size(), deps)
		if entry.Result == ResultMoved {
			entry.Reason = "completed a pending relocation from an earlier interrupted run"
		}
		return entry
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Entry{Share: s.Name, Path: rel, Result: ResultFailed, Err: err.Error()}
	}

	if canBeOpen(srcInfo.Mode()) {
		open, err := preCopyOpen.IsOpen(ctx, src)
		if err != nil {
			return Entry{Share: s.Name, Path: rel, Result: ResultFailed, Err: err.Error()}
		}
		if open {
			return Entry{Share: s.Name, Path: rel, Bytes: srcInfo.Size(), Result: ResultSkippedOpen}
		}
	}

	if !spaceAvailable(s, srcInfo.Size(), deps) {
		return Entry{Share: s.Name, Path: rel, Bytes: srcInfo.Size(), Result: ResultSkippedNoSpace}
	}

	if err := copyMoveFile(src, dst, s.ArrayPath, srcInfo, cfg, deps); err != nil {
		if errors.Is(err, errTargetAppeared) {
			return Entry{Share: s.Name, Path: rel, Result: ResultConflict}
		}
		return Entry{Share: s.Name, Path: rel, Result: ResultFailed, Err: err.Error()}
	}

	return finishPendingDelete(ctx, s, rel, src, srcInfo.Size(), deps)
}

// finishPendingDelete re-checks the source for an open handle — the
// re-check doc 09 §2 requires immediately before unlink — and removes it
// only when it is clear. A source that is (or became) open is left in
// place: both copies are complete and correct, so nothing is lost, and a
// later run completes the delete.
func finishPendingDelete(ctx context.Context, s Share, rel, src string, size int64, deps Deps) Entry {
	return removeSource(ctx, src, deps, Entry{Share: s.Name, Path: rel, Bytes: size})
}

// removeSource is the shared last step of every relocation: re-check src
// for an open handle and, only when it is clear, unlink it. entry carries
// the identifying fields; its Result is set here. A symlink is never
// "open" — the checker would stat through it to its target — so it is
// unlinked without that check.
func removeSource(ctx context.Context, src string, deps Deps, entry Entry) Entry {
	if info, err := os.Lstat(src); err != nil || canBeOpen(info.Mode()) {
		open, err := deps.Open.IsOpen(ctx, src)
		if err != nil {
			entry.Result, entry.Err = ResultMovedPendingDelete, err.Error()
			return entry
		}
		if open {
			entry.Result = ResultMovedPendingDelete
			return entry
		}
	}
	if err := os.Remove(src); err != nil {
		entry.Result, entry.Err = ResultFailed, err.Error()
		return entry
	}
	entry.Result = ResultMoved
	return entry
}

// isSamePendingCopy reports whether dst is very likely this mover's own
// completed-but-not-yet-unlinked copy of src from an earlier interrupted
// run, rather than some other file that independently came to occupy the
// same array-side path (doc 09 §2: a conflict is never auto-resolved).
// Size equality alone cannot tell those apart — a fixed-size file
// rewritten in place on the array is exactly what a same-size, different-
// content collision looks like — so this also requires dst's mtime to
// match src's exactly, the way copyMoveFile always leaves it for a copy
// this mover actually made. When the caller wants stronger assurance than
// that, it hashes both files: two different files coincidentally sharing
// both size and mtime is what VerifyChecksum exists to catch everywhere
// else in this package, and the pending-delete check is no exception.
func isSamePendingCopy(src, dst string, srcInfo, dstInfo os.FileInfo, verifyChecksum bool) (bool, error) {
	if dstInfo.Size() != srcInfo.Size() || !dstInfo.ModTime().Equal(srcInfo.ModTime()) {
		return false, nil
	}
	if !srcInfo.Mode().IsRegular() {
		return sameNode(src, dst, srcInfo, dstInfo)
	}
	if !dstInfo.Mode().IsRegular() {
		return false, nil
	}
	if !verifyChecksum {
		return true, nil
	}
	srcHash, err := hashFile(src)
	if err != nil {
		return false, fmt.Errorf("hash source for pending-delete check: %w", err)
	}
	dstHash, err := hashFile(dst)
	if err != nil {
		return false, fmt.Errorf("hash target for pending-delete check: %w", err)
	}
	return srcHash == dstHash, nil
}

// hashFile returns path's SHA-256 as read back from disk.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// spaceAvailable reports whether at least one of share's eligible disks
// has room for size once its own minfreespace headroom is kept (doc 09
// §2: "pre-check the array has room ... on at least one eligible disk").
// This mirrors mergerfs's own create-time eligibility at the filesystem
// level; it does not choose which branch — mergerfs still does that at
// the actual create (CLAUDE.md: "one placement algorithm").
func spaceAvailable(s Share, size int64, deps Deps) bool {
	branches := s.Branches
	if len(branches) == 0 {
		branches = []string{s.ArrayPath}
	}
	for _, b := range branches {
		avail, err := deps.Avail(b)
		if err != nil {
			continue
		}
		if avail-s.MinFreeSpace >= size {
			return true
		}
	}
	return false
}

// copyMoveFile performs doc 09 §2's copy step for one entry: write a
// temp-suffixed copy through dst's directory (so mergerfs places it),
// preserve mode, ownership, xattrs and timestamps, verify, fsync, then
// atomically rename it into place. Directories dst needs that do not exist
// yet are created with the owner, group and mode of src's. dst's directory
// is resolved once, from dstRoot, one component at a time without following
// a symlink (mkdirAllLike); the temp file, every change to it, its removal
// on failure and the rename into place are then made relative to that
// directory's descriptor and never by path (#656).
//
// What keeps all of that on the target depends on what dstRoot is. On a
// plain filesystem — the cache disk for a relocation to the cache, a data
// disk for a rebalance or an evacuation — the descriptor is the directory
// itself, so one swapped for a symlink while the copy runs cannot send
// anything outside the target tree. On the mover's array-only mergerfs
// mount — the mover and a relocation to the array — the descriptor is a
// FUSE node, and mergerfs carries out each of those calls by path on its
// branch, after a lookup the kernel may still be answering from its entry
// cache; there it is the branch that holds: every branch is a nosymfollow
// bind of its data disk (pool.MoverTargetMount, doc 02 §1), so the kernel
// refuses to follow a symlink swapped into that path at any moment.
//
// A symlink met among dst's directories — by the walk, by requireNoSymlinks
// or by the kernel on a branch — fails the copy with beneath.ErrSymlink and
// leaves src, which this never touches, in place. A regular file is copied
// with its holes intact; a symlink, FIFO or device node is recreated and
// never followed.
func copyMoveFile(src, dst, dstRoot string, srcInfo os.FileInfo, cfg Config, deps Deps) error {
	err := copyMoveFileAt(src, dst, dstRoot, srcInfo, cfg, deps)
	if errors.Is(err, unix.ELOOP) && !errors.Is(err, beneath.ErrSymlink) {
		return fmt.Errorf("%w: %w", beneath.ErrSymlink, err)
	}
	return err
}

func copyMoveFileAt(src, dst, dstRoot string, srcInfo os.FileInfo, cfg Config, deps Deps) error {
	parentfd, err := mkdirAllLike(filepath.Dir(src), dstRoot, filepath.Dir(dst), deps)
	if err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	defer func() { _ = unix.Close(parentfd) }()
	if srcInfo.Mode().IsRegular() {
		return copyRegular(src, dst, dstRoot, parentfd, srcInfo, cfg, deps)
	}
	return copyNode(src, dst, dstRoot, parentfd, srcInfo, deps)
}

// fdPath names the file or directory a descriptor holds open, for the few
// system calls that take only a path (chmod, setxattr). It resolves to that
// object itself, whatever the path it was opened by has become.
func fdPath(fd int) string { return "/proc/self/fd/" + strconv.Itoa(fd) }

func copyRegular(src, dst, dstRoot string, parentfd int, srcInfo os.FileInfo, cfg Config, deps Deps) error {
	name := filepath.Base(dst)
	tmpName := name + tempSuffix + deps.UUID()

	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = in.Close() }()
	if st, err := in.Stat(); err != nil {
		return fmt.Errorf("stat source: %w", err)
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("source is no longer a regular file")
	}

	tfd, err := unix.Openat(parentfd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(srcInfo.Mode().Perm()))
	if err != nil {
		return fmt.Errorf("create target: %w", &fs.PathError{Op: "openat", Path: tmpName, Err: err})
	}
	out := os.NewFile(uintptr(tfd), tmpName)
	cleanTemp := true
	defer func() {
		if cleanTemp {
			_ = out.Close()
			_ = unix.Unlinkat(parentfd, tmpName, 0)
		}
	}()

	var srcHash hashWriter
	var hash io.Writer
	if cfg.VerifyChecksum {
		srcHash = sha256.New()
		hash = srcHash
	}
	if err := copySparse(out, in, srcInfo.Size(), hash, hasHoles(srcInfo)); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if end, err := in.Seek(0, io.SeekEnd); err != nil || end != srcInfo.Size() {
		return fmt.Errorf("copy: source is %d bytes now, was %d when it was chosen", end, srcInfo.Size())
	}

	if st, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		if err := out.Chown(int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("preserve ownership: %w", err)
		}
	}
	if err := out.Chmod(srcInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve mode: %w", err)
	}
	if err := copyXattrs(src, fdPath(int(out.Fd()))); err != nil {
		return err
	}

	if cfg.VerifyChecksum {
		if err := verifyChecksum(parentfd, tmpName, srcHash); err != nil {
			return err
		}
	}

	if err := out.Sync(); err != nil {
		return fmt.Errorf("fsync target: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close target: %w", err)
	}
	// Set after every write and read of tmp above, so nothing after this
	// touches its atime/mtime again before the rename.
	mt := unix.NsecToTimespec(srcInfo.ModTime().UnixNano())
	if err := unix.UtimesNanoAt(parentfd, tmpName, []unix.Timespec{mt, mt}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("preserve timestamps: %w", err)
	}

	if err := requireNoSymlinks(dstRoot, filepath.Dir(dst)); err != nil {
		return err
	}
	renamed, err := publishTemp(parentfd, tmpName, name, deps)
	if renamed {
		cleanTemp = false
	}
	return err
}

// publishTemp renames tmp to name in the directory parentfd without
// replacing anything and makes the rename durable. renamed reports whether
// the rename happened, so a caller whose cleanup removes tmp stops once it
// is gone.
func publishTemp(parentfd int, tmp, name string, deps Deps) (renamed bool, err error) {
	if err := renameNoReplace(parentfd, tmp, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, errTargetAppeared
		}
		return false, fmt.Errorf("rename into place: %w", err)
	}

	// dst and src live on independent filesystems with independent
	// journals: a rename can commit to the array's journal while the
	// cache-side unlink (finishPendingDelete, still to come) commits to
	// the cache's — or the other way around — with no ordering between
	// them unless this fsyncs the rename's own directory entry first.
	// Without this, a power loss after finishPendingDelete's unlink
	// journals but before the rename does leaves the array holding only
	// "<name>.hoserva-moving-<uuid>", which the next run's
	// sweepStrayTemps discards as a stray copy — losing the file rather
	// than merely duplicating it (doc 09 §2).
	if err := deps.FsyncDir(parentfd); err != nil {
		return true, fmt.Errorf("fsync target directory: %w", err)
	}
	return true, nil
}

// fsyncDir fsyncs the directory dirfd itself, not any file in it, so a
// rename or create inside it is durable against power loss (the directory
// entry is its own piece of filesystem metadata, distinct from the file's
// own data).
func fsyncDir(dirfd int) error {
	if err := unix.Fsync(dirfd); err != nil {
		return fmt.Errorf("sync directory: %w", err)
	}
	return nil
}

// hashWriter is the subset of hash.Hash verifyChecksum needs.
type hashWriter interface {
	io.Writer
	Sum(b []byte) []byte
}

// verifyChecksum re-reads tmp, in the directory parentfd, from disk and compares its hash against
// srcHash — the bytes actually written, not merely the bytes that passed
// through memory during the copy (doc 09 §2: "verify ... checksum
// optionally").
func verifyChecksum(parentfd int, tmp string, srcHash hashWriter) error {
	fd, err := beneath.Open(parentfd, tmp, unix.O_RDONLY)
	if err != nil {
		return fmt.Errorf("verify: reopen target: %w", err)
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("verify: read target: %w", err)
	}
	want := fmt.Sprintf("%x", srcHash.Sum(nil))
	got := fmt.Sprintf("%x", h.Sum(nil))
	if want != got {
		return fmt.Errorf("verify: checksum mismatch (source %s, target %s)", want, got)
	}
	return nil
}

// matchExclude reports whether rel (or its base name) matches any of
// patterns.
func matchExclude(patterns []string, rel string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if ok, _ := filepath.Match(p, filepath.Base(rel)); ok {
			return true
		}
	}
	return false
}

// enumerateFiles returns every regular file under root, as paths
// relative to root, in sorted order — sorted so a Checkpoint's LastPath
// can resume deterministically. A root that does not exist yet enumerates
// as empty rather than erroring: a share with nothing on cache has
// nothing to move. In-flight temp files are never candidates: they are
// this package's own bookkeeping, on the array side, never on cache, but
// excluded here too as a defensive measure against a workspace built
// from a mix of production and lab state.
func enumerateFiles(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rels []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		if strings.Contains(d.Name(), tempSuffix) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	return rels, nil
}

// sweepStrayTemps removes every leftover "*.hoserva-moving-*" file under
// root, and an empty directory so named (one mkdirAllLike was interrupted
// in): an interrupted run's own partial copy. Run calls it only on each
// share's ArrayPath, so what a relocation to the cache or an evacuation
// left on its own target is not swept. Discarding it is always
// safe — the mover never unlinks a source before the corresponding
// rename completes, so the authoritative copy is still on cache and a
// fresh copy will be made on this same run's pass over it.
func sweepStrayTemps(root string) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.Contains(d.Name(), tempSuffix) {
				// A directory made for a copy, not yet renamed to its real
				// name: it is empty. One that is not is left alone.
				if err := os.Remove(path); err != nil && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
					return err
				}
				return fs.SkipDir
			}
			return nil
		}
		if strings.Contains(d.Name(), tempSuffix) {
			return os.Remove(path)
		}
		return nil
	})
}
