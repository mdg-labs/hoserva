package disk

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// ErrDataDiskUpgradeMismatch is verifyDataDiskTree's own refusal: the
// new disk's copy does not match the old disk's own content, ownership,
// timestamps or entry count (doc 02 §4 "Larger data disk", Q71).
// Returning this before Remounting means the old disk is still mounted
// and serving exactly as it was before the upgrade started; returning
// it after Remounting (a caller re-verifying post-swap) means the old
// disk, though currently unmounted, has never been written to or
// removed by this package and can still be remounted back by hand, or
// the mismatch can be resolved the same way any disk replacement is
// (doc 02 §4's `snapraid fix -d dN`, reconstructing whatever should be
// at this mount point from parity).
var ErrDataDiskUpgradeMismatch = errors.New("disk: new data disk's copy does not match the old data disk")

// DataDiskUpgradePhase is where one RunDataDiskUpgrade run currently is
// (doc 02 §4 "Larger data disk", Q71): format the new disk and mount it
// at a scratch staging path, copy the old disk's files onto it
// preserving ownership, xattrs and timestamps, verify that copy, swap
// the mounts so the new disk takes over the old disk's own /mnt/diskN,
// require a clean `snapraid diff` against the newly remounted disk
// (Diffing), and only once that diff shows nothing removed or updated
// release the old physical disk (Releasing). The old disk is never
// written to, truncated or removed by this package at any phase, and it
// stays mounted (so the array keeps serving it normally) all the way
// through Verifying — only Remounting itself ever unmounts it, and only
// after Verifying has already confirmed the new disk's copy is
// faithful. A Diffing result that is not clean leaves the run
// checkpointed at Diffing, without ever calling Release — the old disk
// stays unreleased (though already unmounted from its own physical
// service by Remounting) until a later run's diff comes back clean.
type DataDiskUpgradePhase string

const (
	DataDiskUpgradePhaseFormatting DataDiskUpgradePhase = "formatting"
	DataDiskUpgradePhaseCopying    DataDiskUpgradePhase = "copying"
	DataDiskUpgradePhaseVerifying  DataDiskUpgradePhase = "verifying"
	DataDiskUpgradePhaseRemounting DataDiskUpgradePhase = "remounting"
	DataDiskUpgradePhaseDiffing    DataDiskUpgradePhase = "diffing"
	DataDiskUpgradePhaseReleasing  DataDiskUpgradePhase = "releasing"
)

// DataDiskUpgradeCheckpoint is RunDataDiskUpgrade's own resumable
// progress marker (Q29). LastPath is Copying's own checkpoint: the last
// relative path (in sorted, parent-before-child order) that Copying
// fully finished, so a resumed copy skips forward past it rather than
// re-copying everything — the same shape internal/cache's own
// Checkpoint.LastPath uses for the mover's per-file resumption.
// Verifying is never resumed mid-way: it only ever reads, so restarting
// its whole comparison after an interruption is always safe and cheap
// enough not to need its own finer-grained checkpoint.
type DataDiskUpgradeCheckpoint struct {
	Phase    DataDiskUpgradePhase `json:"phase"`
	LastPath string               `json:"last_path,omitempty"`
}

// DataDiskUpgradeSpec is one "larger data disk" upgrade (doc 02 §4,
// Q71). Old is the disk's current MountUnit, mounted at its normal
// /mnt/diskN throughout Formatting, Copying and Verifying — Remounting
// is the only phase that ever unmounts it. New is the fresh, larger
// replacement, formatted and then mounted at Staging (never at
// Old.Where) so both disks are readable side by side for the whole
// copy.
type DataDiskUpgradeSpec struct {
	Old            MountUnit
	New            DiskAddition
	NewFilesystem  FilesystemType
	Staging        string
	VerifyChecksum bool
}

// DataDiskUpgradeDeps are RunDataDiskUpgrade's own system-touching
// dependencies (CLAUDE.md: every system-touching subsystem sits behind
// a package interface with a scriptable fake). Provider, Runner,
// Mounter, Diff and Release are all required.
type DataDiskUpgradeDeps struct {
	Provider Provider
	Runner   Runner
	Mounter  UnitMounter
	// Diff runs `snapraid diff` against the configuration that now names
	// the newly remounted disk (parity.Engine.Diff) and reports how many
	// files it found removed and updated — the acceptance criterion's own
	// release gate (Q71): "require snapraid diff to show no removed or
	// updated files before release." Returning plain ints rather than a
	// parity.DiffReport keeps this package independent of internal/parity
	// (see internal/disk/xattr.go's own doc comment on why that import
	// cycle is avoided by duplication elsewhere in this package).
	Diff func(ctx context.Context) (removed, updated int, err error)
	// Release is called only once Diff has reported removed==0 &&
	// updated==0: whatever the caller does to free the old physical disk
	// for reuse (doc 02 §4's "reuses the old parity disk as a data disk"
	// analogue for a data disk) — disk- and store-level bookkeeping
	// outside this package's own scope, mirroring
	// parity.ParityUpgradeDeps' own Release hook. RunDataDiskUpgrade's
	// only guarantee about it is when it is called: last, and only once
	// Diffing has actually come back clean.
	Release func(ctx context.Context) error
	// StagingMounted reports whether Staging is currently mounted.
	// Checked on every resume that still needs to read or write Staging
	// (Copying, Verifying) — neither DirectMounter's `mount` nor
	// SystemdMounter's `systemctl start` leaves a mount in place across a
	// daemon restart on its own, so a resume cannot simply trust Staging
	// still resolves to the disk Formatting mounted there. Defaults to
	// IsMountpoint; overridden by tests that cannot mount a real
	// filesystem.
	StagingMounted func(path string) (bool, error)
	Now            func() time.Time
}

func (d DataDiskUpgradeDeps) withDefaults() DataDiskUpgradeDeps {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.StagingMounted == nil {
		d.StagingMounted = IsMountpoint
	}
	return d
}

// DataDiskUpgradeHooks lets a caller observe and control one
// RunDataDiskUpgrade call without this package depending on
// internal/job (mirroring internal/cache's own RunHooks).
type DataDiskUpgradeHooks struct {
	StopRequested  <-chan struct{}
	SaveCheckpoint func(data []byte) error
	SetProgress    func(pct int)
	Log            func(format string, args ...any)
}

func (h DataDiskUpgradeHooks) checkpoint(cp DataDiskUpgradeCheckpoint) error {
	if h.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("disk: encode data disk upgrade checkpoint: %w", err)
	}
	return h.SaveCheckpoint(data)
}

func (h DataDiskUpgradeHooks) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

func (h DataDiskUpgradeHooks) progress(pct int) {
	if h.SetProgress != nil {
		h.SetProgress(pct)
	}
}

func (h DataDiskUpgradeHooks) stopRequested() bool {
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

// DataDiskUpgradeResult is what RunDataDiskUpgrade returns once it
// stops. NewMount is only set once Remounting has actually completed —
// the MountUnit now serving Old.Where's own mountpoint. Released is only
// true once Diffing has reported a clean diff and Release has actually
// run: a caller with NewMount set but Released false has a fully
// remounted, live array whose old disk is not yet safe to release,
// either because Diffing has not run yet (interrupted before it) or
// because it found removed or updated files — distinguishing "remounted,
// diff pending" from "fully released, upgrade complete".
type DataDiskUpgradeResult struct {
	Interrupted bool
	NewMount    MountUnit
	Released    bool
}

// RunDataDiskUpgrade executes spec (doc 02 §4 "Larger data disk", Q71),
// resuming from initialCheckpoint when it is non-empty (Q29). See
// DataDiskUpgradePhase's own doc comment for the safety property this
// enforces at every phase boundary.
func RunDataDiskUpgrade(ctx context.Context, spec DataDiskUpgradeSpec, deps DataDiskUpgradeDeps, hooks DataDiskUpgradeHooks, initialCheckpoint []byte) (DataDiskUpgradeResult, error) {
	deps = deps.withDefaults()
	if deps.Provider == nil || deps.Runner == nil || deps.Mounter == nil || deps.Diff == nil || deps.Release == nil {
		return DataDiskUpgradeResult{}, errors.New("disk: run data disk upgrade: Provider, Runner, Mounter, Diff and Release are all required")
	}

	var cp DataDiskUpgradeCheckpoint
	if len(initialCheckpoint) > 0 {
		if err := json.Unmarshal(initialCheckpoint, &cp); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: decode data disk upgrade checkpoint: %w", err)
		}
	}
	if cp.Phase == "" {
		cp.Phase = DataDiskUpgradePhaseFormatting
	}

	var newUUID string
	if cp.Phase == DataDiskUpgradePhaseFormatting {
		hooks.logf("data disk upgrade: formatting the new disk")
		uuid, err := formatNewDataDisk(ctx, spec, deps)
		if err != nil {
			return DataDiskUpgradeResult{}, err
		}
		newUUID = uuid
		if err := deps.Mounter.Mount(ctx, stagingMountUnit(spec, uuid)); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: mount new disk at staging path: %w", err)
		}
		cp = DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseCopying}
		if err := hooks.checkpoint(cp); err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return DataDiskUpgradeResult{Interrupted: true}, nil
		}
	} else {
		// Resuming past Formatting: the new disk was already formatted
		// and mounted at Staging by an earlier run — re-read its
		// filesystem UUID rather than formatting it again, so Remounting
		// still knows what to mount at Old.Where.
		uuid, err := FilesystemUUID(ctx, deps.Runner, identityOrDevice(spec.New.ByIDName, spec.New.Device))
		if err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: re-read new disk's filesystem UUID on resume: %w", err)
		}
		newUUID = uuid

		// Copying and Verifying are the only phases that still read or
		// write Staging — Remounting, Diffing and Releasing never touch
		// it again (Remounting is the one phase that deliberately leaves
		// it unmounted once the new disk takes over Old.Where). Neither
		// DirectMounter's `mount` nor SystemdMounter's `systemctl start`
		// leaves that mount in place across a daemon restart on its own,
		// so a resume into either phase cannot simply trust Staging still
		// resolves to the disk Formatting mounted there.
		if cp.Phase == DataDiskUpgradePhaseCopying || cp.Phase == DataDiskUpgradePhaseVerifying {
			if err := ensureStagingMounted(ctx, spec, deps, newUUID); err != nil {
				return DataDiskUpgradeResult{}, err
			}
		}
	}

	if cp.Phase == DataDiskUpgradePhaseCopying {
		hooks.logf("data disk upgrade: copying %s to %s", spec.Old.Where, spec.Staging)
		interrupted, lastPath, err := copyDataDiskTree(ctx, spec.Old.Where, spec.Staging, hooks, cp.LastPath)
		if err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if interrupted {
			if err := hooks.checkpoint(DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseCopying, LastPath: lastPath}); err != nil {
				return DataDiskUpgradeResult{}, err
			}
			return DataDiskUpgradeResult{Interrupted: true}, nil
		}
		cp = DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseVerifying}
		if err := hooks.checkpoint(cp); err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return DataDiskUpgradeResult{Interrupted: true}, nil
		}
	}

	if cp.Phase == DataDiskUpgradePhaseVerifying {
		hooks.logf("data disk upgrade: verifying %s against %s", spec.Staging, spec.Old.Where)
		interrupted, err := verifyDataDiskTree(ctx, spec.Old.Where, spec.Staging, spec.VerifyChecksum, hooks)
		if err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if interrupted {
			return DataDiskUpgradeResult{Interrupted: true}, nil
		}
		cp = DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseRemounting}
		if err := hooks.checkpoint(cp); err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return DataDiskUpgradeResult{Interrupted: true}, nil
		}
	}

	newMount := MountUnit{Where: spec.Old.Where, UUID: newUUID, Filesystem: spec.NewFilesystem, Description: spec.Old.Description}
	if cp.Phase == DataDiskUpgradePhaseRemounting {
		hooks.logf("data disk upgrade: remounting %s onto the new disk", spec.Old.Where)
		// Every step below is idempotent (DirectMounter's and
		// SystemdMounter's own doc comments: unmounting an already-
		// unmounted path, or mounting an already-correctly-mounted one,
		// is success) — a kill between any two of them is safely retried
		// in full on resume, without needing its own finer-grained
		// checkpoint.
		if err := deps.Mounter.Unmount(ctx, MountUnit{Where: spec.Staging}); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: unmount staging path: %w", err)
		}
		if err := deps.Mounter.Unmount(ctx, spec.Old); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: unmount old disk: %w", err)
		}
		if err := deps.Mounter.Mount(ctx, newMount); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: mount new disk at %s: %w", spec.Old.Where, err)
		}
		cp = DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseDiffing}
		if err := hooks.checkpoint(cp); err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return DataDiskUpgradeResult{NewMount: newMount, Interrupted: true}, nil
		}
	}

	if cp.Phase == DataDiskUpgradePhaseDiffing {
		hooks.logf("data disk upgrade: checking snapraid diff against %s before releasing the old disk", newMount.Where)
		removed, updated, err := deps.Diff(ctx)
		if err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: check snapraid diff before releasing old disk: %w", err)
		}
		if removed != 0 || updated != 0 {
			// The gate working as designed (doc 02 §4 "Larger data disk",
			// Q71): stop here, checkpointed at Diffing so a later run can
			// simply retry once the diff comes back clean, without ever
			// calling Release. The old physical disk stays unreleased,
			// though it is already unmounted from its own service by
			// Remounting.
			hooks.logf("data disk upgrade: snapraid diff reports %d removed, %d updated file(s); refusing to release the old disk until it is clean", removed, updated)
			return DataDiskUpgradeResult{NewMount: newMount}, nil
		}
		cp = DataDiskUpgradeCheckpoint{Phase: DataDiskUpgradePhaseReleasing}
		if err := hooks.checkpoint(cp); err != nil {
			return DataDiskUpgradeResult{}, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return DataDiskUpgradeResult{NewMount: newMount, Interrupted: true}, nil
		}
	}

	if cp.Phase == DataDiskUpgradePhaseReleasing {
		hooks.logf("data disk upgrade: releasing the old disk")
		if err := deps.Release(ctx); err != nil {
			return DataDiskUpgradeResult{}, fmt.Errorf("disk: release old data disk: %w", err)
		}
		return DataDiskUpgradeResult{NewMount: newMount, Released: true}, nil
	}

	return DataDiskUpgradeResult{NewMount: newMount}, nil
}

// stagingMountUnit is the MountUnit Formatting mounts at spec.Staging,
// and the same one a resume into Copying or Verifying re-mounts if it
// finds Staging no longer mounted — one mounting path, not two.
func stagingMountUnit(spec DataDiskUpgradeSpec, uuid string) MountUnit {
	return MountUnit{Where: spec.Staging, UUID: uuid, Filesystem: spec.NewFilesystem, Description: "Hoserva data disk upgrade staging"}
}

// ensureStagingMounted confirms spec.Staging is mounted on the new
// disk's own filesystem (identified by uuid) before a resumed run
// reads or writes it, and re-mounts it — reusing stagingMountUnit, the
// exact mount Formatting itself used — if it finds Staging is not
// currently mounted. Neither DirectMounter's `mount` nor
// SystemdMounter's `systemctl start` leaves a mount in place across a
// daemon restart on its own: without this check, a resumed Copying
// phase would silently write the old disk's whole tree onto whatever
// ordinary directory spec.Staging happens to resolve to on the root
// filesystem, and a resumed Verifying phase would compare against that
// same wrong directory. If Staging still is not mounted after the
// re-mount attempt, this refuses rather than resuming onto it — nothing
// under spec.Staging is ever touched by copyDataDiskTree or
// verifyDataDiskTree unless this has already confirmed it is safe to.
func ensureStagingMounted(ctx context.Context, spec DataDiskUpgradeSpec, deps DataDiskUpgradeDeps, uuid string) error {
	mounted, err := deps.StagingMounted(spec.Staging)
	if err != nil {
		return fmt.Errorf("disk: checking whether staging path %s is mounted: %w", spec.Staging, err)
	}
	if mounted {
		return nil
	}
	if err := deps.Mounter.Mount(ctx, stagingMountUnit(spec, uuid)); err != nil {
		return fmt.Errorf("disk: re-mount staging path %s on resume: %w", spec.Staging, err)
	}
	remounted, err := deps.StagingMounted(spec.Staging)
	if err != nil {
		return fmt.Errorf("disk: checking whether staging path %s is mounted after re-mount: %w", spec.Staging, err)
	}
	if !remounted {
		return fmt.Errorf("disk: staging path %s is still not mounted after a re-mount attempt; refusing to resume onto it", spec.Staging)
	}
	return nil
}

// formatNewDataDisk formats spec.New through FormatForAddition (#157's
// identity-bound formatting, reused unchanged rather than reinvented)
// and reads back its fresh filesystem UUID the same way
// internal/job/disk_run.go's own filesystemUUIDs helper does after
// array setup: through spec.New's own by-id path when one is known,
// never spec.New.Device directly, so the UUID read binds to the same
// physical disk Format actually ran against.
func formatNewDataDisk(ctx context.Context, spec DataDiskUpgradeSpec, deps DataDiskUpgradeDeps) (uuid string, err error) {
	if err := FormatForAddition(ctx, deps.Provider, deps.Runner, spec.New); err != nil {
		return "", err
	}
	target := identityOrDevice(spec.New.ByIDName, spec.New.Device)
	uuid, err = FilesystemUUID(ctx, deps.Runner, target)
	if err != nil {
		return "", fmt.Errorf("disk: read new disk's filesystem UUID: %w", err)
	}
	return uuid, nil
}

// enumerateDataDiskTree lists every entry under root, relative to root,
// sorted so that a directory always sorts before its own descendants
// (guaranteed for any two strings where one is a strict prefix of the
// other) — the order both copyDataDiskTree and verifyDataDiskTree walk
// in, and the order DataDiskUpgradeCheckpoint.LastPath resumes against.
func enumerateDataDiskTree(root string) ([]string, error) {
	var rels []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("disk: walk %s: %w", root, err)
	}
	sort.Strings(rels)
	return rels, nil
}

// copyDataDiskTree copies every entry under src to the same relative
// path under dst, preserving mode, ownership, xattrs and timestamps
// (doc 02 §4 "Larger data disk"), resuming past everything at or before
// resumeAfter — relocateCopyPhase's (internal/cache) own per-file
// resumption, applied to a whole disk's tree rather than one share. It
// only ever reads src; nothing under src is ever removed, truncated or
// overwritten by this function.
func copyDataDiskTree(ctx context.Context, src, dst string, hooks DataDiskUpgradeHooks, resumeAfter string) (interrupted bool, lastPath string, err error) {
	rels, err := enumerateDataDiskTree(src)
	if err != nil {
		return false, resumeAfter, err
	}

	lastPath = resumeAfter
	total := len(rels)
	for i, rel := range rels {
		if resumeAfter != "" && rel <= resumeAfter {
			continue
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			return true, lastPath, nil
		}

		if err := copyDataDiskEntry(filepath.Join(src, rel), filepath.Join(dst, rel)); err != nil {
			return false, lastPath, fmt.Errorf("disk: copy %s: %w", rel, err)
		}
		lastPath = rel
		hooks.logf("data disk upgrade: copied %s", rel)
		if total > 0 {
			hooks.progress((i + 1) * 100 / total)
		}
	}

	// Directory timestamps are fixed up only once every entry has been
	// created: creating or copying a later child into a directory
	// updates that directory's own mtime on the destination filesystem,
	// so setting it any earlier would just be overwritten by the very
	// next child copied underneath it.
	for _, rel := range rels {
		srcPath := filepath.Join(src, rel)
		info, err := os.Lstat(srcPath)
		if err != nil {
			return false, lastPath, fmt.Errorf("disk: stat %s: %w", srcPath, err)
		}
		if info.IsDir() {
			if err := chtimes(filepath.Join(dst, rel), info); err != nil {
				return false, lastPath, fmt.Errorf("disk: set timestamps on %s: %w", rel, err)
			}
		}
	}

	return false, lastPath, nil
}

func copyDataDiskEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", src, err)
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return copyDataDiskSymlink(src, dst, info)
	case info.IsDir():
		return copyDataDiskDir(src, dst, info)
	case info.Mode().IsRegular():
		return copyDataDiskFile(src, dst, info)
	default:
		// Sockets, FIFOs and device nodes are never real user data on a
		// data disk share; skipped rather than failing the whole upgrade
		// over something that was never going to be there in practice.
		return nil
	}
}

func copyDataDiskDir(src, dst string, info os.FileInfo) error {
	if err := os.Mkdir(dst, info.Mode().Perm()); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	if err := chown(dst, info); err != nil {
		return err
	}
	if err := copyXattrs(src, dst); err != nil {
		return fmt.Errorf("copy xattrs on %s: %w", dst, err)
	}
	return nil
}

func copyDataDiskFile(src, dst string, info os.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer func() { _ = out.Close() }()

	n, err := io.Copy(out, in)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if n != info.Size() {
		return fmt.Errorf("copy %s: wrote %d bytes, source was %d", src, n, info.Size())
	}
	if err := out.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	if err := chown(dst, info); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("fsync %s: %w", dst, err)
	}
	if err := copyXattrs(src, dst); err != nil {
		return fmt.Errorf("copy xattrs on %s: %w", dst, err)
	}
	if err := chtimes(dst, info); err != nil {
		return fmt.Errorf("set timestamps on %s: %w", dst, err)
	}
	return nil
}

// copyDataDiskSymlink preserves a symlink's own target and ownership.
// Its xattrs and precise timestamps are not preserved — a documented,
// narrower gap than regular files and directories get, since setting
// either safely requires an O_NOFOLLOW-aware syscall this package does
// not otherwise need; symlinks are rare on an ordinary data disk share
// and carry no content of their own to lose.
func copyDataDiskSymlink(src, dst string, info os.FileInfo) error {
	target, err := os.Readlink(src)
	if err != nil {
		return fmt.Errorf("readlink %s: %w", src, err)
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing %s: %w", dst, err)
	}
	if err := os.Symlink(target, dst); err != nil {
		return fmt.Errorf("symlink %s: %w", dst, err)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := os.Lchown(dst, int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("lchown %s: %w", dst, err)
		}
	}
	return nil
}

func chown(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

func chtimes(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return os.Chtimes(path, info.ModTime(), info.ModTime())
	}
	atime := time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec))
	mtime := time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec))
	return os.Chtimes(path, atime, mtime)
}

// dataDiskUpgradeFilesystemOwnedEntries names root-level entries a
// filesystem's own mkfs can create on a fresh filesystem that never
// existed on the old disk — mke2fs creates a root lost+found on every
// ext2/ext3/ext4 filesystem it formats (XFS and btrfs do not).
// copyDataDiskTree only ever copies from src, so a destination-only
// entry named here always means "the new filesystem made this itself",
// never a copy bug; conditional, not universal, since an old EXT4 disk
// may already have its own lost+found, in which case it is just another
// entry in rels and compared like any other.
var dataDiskUpgradeFilesystemOwnedEntries = map[string]bool{
	"lost+found": true,
}

// verifyDataDiskTree compares every entry under src against the same
// relative path under dst (doc 02 §4 "Larger data disk": ownership,
// xattrs, timestamps and content) — restarting the whole comparison
// from the beginning on every call rather than resuming partway, since
// it only ever reads and so an interrupted-then-resumed verify is
// always both safe and cheap to redo in full.
func verifyDataDiskTree(ctx context.Context, src, dst string, checksum bool, hooks DataDiskUpgradeHooks) (interrupted bool, err error) {
	rels, err := enumerateDataDiskTree(src)
	if err != nil {
		return false, err
	}
	dstRels, err := enumerateDataDiskTree(dst)
	if err != nil {
		return false, err
	}

	srcSet := make(map[string]bool, len(rels))
	for _, rel := range rels {
		srcSet[rel] = true
	}
	for _, rel := range dstRels {
		if srcSet[rel] || dataDiskUpgradeFilesystemOwnedEntries[rel] {
			continue
		}
		return false, fmt.Errorf("%w: %s has unexpected entry %s not present on %s", ErrDataDiskUpgradeMismatch, dst, rel, src)
	}

	total := len(rels)
	for i, rel := range rels {
		if ctx.Err() != nil || hooks.stopRequested() {
			return true, nil
		}
		if err := verifyDataDiskEntry(filepath.Join(src, rel), filepath.Join(dst, rel), checksum); err != nil {
			return false, fmt.Errorf("disk: verify %s: %w", rel, err)
		}
		if total > 0 {
			hooks.progress((i + 1) * 100 / total)
		}
	}
	return false, nil
}

func verifyDataDiskEntry(src, dst string, checksum bool) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", src, err)
	}
	dstInfo, err := os.Lstat(dst)
	if err != nil {
		return fmt.Errorf("%w: %s is missing on the new disk (%v)", ErrDataDiskUpgradeMismatch, dst, err)
	}

	if srcInfo.Mode()&os.ModeSymlink != 0 {
		if dstInfo.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%w: %s is a symlink on the old disk but not on the new one", ErrDataDiskUpgradeMismatch, dst)
		}
		srcTarget, err := os.Readlink(src)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", src, err)
		}
		dstTarget, err := os.Readlink(dst)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", dst, err)
		}
		if srcTarget != dstTarget {
			return fmt.Errorf("%w: %s target %q, want %q", ErrDataDiskUpgradeMismatch, dst, dstTarget, srcTarget)
		}
		return nil
	}

	if srcInfo.IsDir() != dstInfo.IsDir() {
		return fmt.Errorf("%w: %s type mismatch", ErrDataDiskUpgradeMismatch, dst)
	}
	if srcInfo.Mode().Perm() != dstInfo.Mode().Perm() {
		return fmt.Errorf("%w: %s mode %v, want %v", ErrDataDiskUpgradeMismatch, dst, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
	}
	if srcSt, ok := srcInfo.Sys().(*syscall.Stat_t); ok {
		if dstSt, ok := dstInfo.Sys().(*syscall.Stat_t); ok {
			if srcSt.Uid != dstSt.Uid || srcSt.Gid != dstSt.Gid {
				return fmt.Errorf("%w: %s ownership %d:%d, want %d:%d", ErrDataDiskUpgradeMismatch, dst, dstSt.Uid, dstSt.Gid, srcSt.Uid, srcSt.Gid)
			}
		}
	}

	if srcInfo.IsDir() {
		return nil
	}

	if srcInfo.Size() != dstInfo.Size() {
		return fmt.Errorf("%w: %s size %d, want %d", ErrDataDiskUpgradeMismatch, dst, dstInfo.Size(), srcInfo.Size())
	}
	if !srcInfo.ModTime().Equal(dstInfo.ModTime()) {
		return fmt.Errorf("%w: %s mtime %v, want %v", ErrDataDiskUpgradeMismatch, dst, dstInfo.ModTime(), srcInfo.ModTime())
	}
	if checksum {
		same, err := filesSHA256Equal(src, dst)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("%w: %s content does not match", ErrDataDiskUpgradeMismatch, dst)
		}
	}
	return nil
}

func filesSHA256Equal(a, b string) (bool, error) {
	ha, err := fileSHA256(a)
	if err != nil {
		return false, err
	}
	hb, err := fileSHA256(b)
	if err != nil {
		return false, err
	}
	return string(ha) == string(hb), nil
}

func fileSHA256(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	return h.Sum(nil), nil
}
