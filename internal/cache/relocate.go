package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// SyncFunc requests one threshold-guarded parity sync, carrying
// manifest as SyncOpts.Manifest (Q15) — RelocateToCache's own hook onto
// parity.Engine.Sync, kept out of this package's own dependencies the
// same way RunHooks avoids importing internal/job (doc.go). It owns
// submitting the sync and waiting for it to finish; a blocked sync's
// error (parity.ErrGuardBlocked, wrapped) is returned unchanged so
// RelocateToCache reacts exactly as it would to any other guard block —
// this package never supplies parity.SyncOpts.Confirm on a caller's
// behalf (CLAUDE.md: "nothing syncs past a tripped guard").
type SyncFunc func(ctx context.Context, manifest []parity.ManifestEntry) error

// RelocateToArray relocates share's cache-side files to the array (doc 09
// §2: "Cache → array: identical to a mover run restricted to one share,
// ignoring the grace period"). It is Run itself, restricted to share
// alone and with cfg.SkipGracePeriod forced on regardless of what the
// caller passed — a single-share relocation the user explicitly asked
// for is never held back by the scheduled mover's own "give it five
// minutes" caution.
func RelocateToArray(ctx context.Context, share Share, cfg Config, deps Deps, hooks RunHooks, initialCheckpoint []byte) (Report, error) {
	cfg.SkipGracePeriod = true
	return Run(ctx, []Share{share}, cfg, deps, hooks, initialCheckpoint)
}

// RelocatePhase is where a RelocateToCache run currently is in Q14's
// array-involved order — copy and verify everything, sync, delete the
// sources, then sync again: copying builds and verifies every array-side
// file's own copy on cache; syncing runs the guarded parity sync those
// copies must clear before anything is deleted (doc 09 §2's "a sync runs
// before the originals are removed"); deleting removes the array
// originals only once that sync has completed; finalSync runs the
// trailing guarded sync Q14 requires so parity is never left stale
// against the deletes finalSync itself accounts for.
type RelocatePhase string

const (
	RelocatePhaseCopying   RelocatePhase = "copying"
	RelocatePhaseSyncing   RelocatePhase = "syncing"
	RelocatePhaseDeleting  RelocatePhase = "deleting"
	RelocatePhaseFinalSync RelocatePhase = "final_sync"
)

// RelocateCheckpoint is RelocateToCache's own resumable progress marker
// (Q29). Manifest travels in the checkpoint from the moment the copy
// phase finishes: it is what the syncing, deleting and finalSync phases
// all depend on, and neither of the latter two can safely re-derive it
// from the filesystem once the delete phase may have already removed
// some of its own sources.
type RelocateCheckpoint struct {
	Phase RelocatePhase `json:"phase"`
	// BranchIndex and LastPath resume the copy phase's own walk over
	// share.Branches, the same shape Checkpoint.ShareIndex/LastPath uses
	// for Run's walk over shares.
	BranchIndex int    `json:"branch_index"`
	LastPath    string `json:"last_path"`
	// Manifest is every file the copy phase verified onto cache, set once
	// that phase finishes and carried unchanged through the rest of this
	// checkpoint's own life.
	Manifest []parity.ManifestEntry `json:"manifest,omitempty"`
	// DeletedCount is how many of Manifest's entries the delete phase has
	// already finished (in order) — resuming the delete phase skips
	// straight to Manifest[DeletedCount:] rather than re-checking entries
	// already decided.
	DeletedCount int `json:"deleted_count,omitempty"`
}

func (h RunHooks) checkpointRelocate(cp RelocateCheckpoint) error {
	if h.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("cache: encode relocate checkpoint: %w", err)
	}
	return h.SaveCheckpoint(data)
}

// RelocateToCache relocates share's array-side files to the cache (doc
// 09 §2, Q14, Q15): copy each from its own data disk to
// share.CachePath, verify, run one threshold-guarded sync carrying a
// manifest of what was copied, delete the array originals — never
// before that sync, since deleting from a data disk weakens recovery of
// the array's other disks until the next one runs — and then run a
// second threshold-guarded sync so parity is never left stale against
// the deletes this run itself just made (Q14's "sync, delete the
// sources, then sync again"). deps.Sync is required; RelocateToCache
// never invents a substitute for it.
//
// share.Branches must be the share's own per-disk directories in
// pool.MoverTargetMount's own branch order (mover.go's Share doc
// comment) — RelocateToCache derives each file's owning data disk as
// filepath.Dir(branch), the same "<disk>/<share>" shape
// pool.shareBranches builds every branch from.
func RelocateToCache(ctx context.Context, share Share, cfg Config, deps Deps, hooks RunHooks, initialCheckpoint []byte) (Report, error) {
	deps = deps.withDefaults()
	if deps.Sync == nil {
		return Report{}, errors.New("cache: relocate to cache: Deps.Sync is required")
	}

	var cp RelocateCheckpoint
	if len(initialCheckpoint) > 0 {
		if err := json.Unmarshal(initialCheckpoint, &cp); err != nil {
			return Report{}, fmt.Errorf("cache: decode relocate checkpoint: %w", err)
		}
	}
	if cp.Phase == "" {
		cp.Phase = RelocatePhaseCopying
	}

	report := Report{StartedAt: deps.Now()}
	defer func() {
		if report.FinishedAt.IsZero() {
			report.FinishedAt = deps.Now()
		}
	}()

	manifest := cp.Manifest

	if cp.Phase == RelocatePhaseCopying {
		var interrupted bool
		var err error
		manifest, interrupted, err = relocateCopyPhase(ctx, share, cfg, deps, hooks, cp, &report)
		if err != nil {
			return report, err
		}
		if interrupted {
			report.Interrupted = true
			return report, nil
		}
		cp = RelocateCheckpoint{Phase: RelocatePhaseSyncing, Manifest: manifest}
		if err := hooks.checkpointRelocate(cp); err != nil {
			return report, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			report.Interrupted = true
			return report, nil
		}
	}

	// Nothing was copied — every eligible file was a conflict, held
	// open, or already gone — so there is nothing to protect with a sync
	// and nothing to delete; running either against an empty manifest
	// would be a no-op sync for no reason.
	if cp.Phase == RelocatePhaseSyncing && len(manifest) == 0 {
		cp = RelocateCheckpoint{Phase: RelocatePhaseDeleting, Manifest: manifest}
	} else if cp.Phase == RelocatePhaseSyncing {
		if err := deps.Sync(ctx, manifest); err != nil {
			return report, fmt.Errorf("cache: relocate to cache: sync: %w", err)
		}
		cp = RelocateCheckpoint{Phase: RelocatePhaseDeleting, Manifest: manifest}
		if err := hooks.checkpointRelocate(cp); err != nil {
			return report, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			report.Interrupted = true
			return report, nil
		}
	}

	if cp.Phase == RelocatePhaseDeleting {
		interrupted, err := relocateDeletePhase(ctx, share, deps, hooks, &report, cp.Manifest, cp.DeletedCount)
		if err != nil {
			return report, err
		}
		if interrupted {
			report.Interrupted = true
			return report, nil
		}
		cp = RelocateCheckpoint{Phase: RelocatePhaseFinalSync, Manifest: cp.Manifest}
		if err := hooks.checkpointRelocate(cp); err != nil {
			return report, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			report.Interrupted = true
			return report, nil
		}
	}

	// An empty manifest already skipped straight from copying to deleting
	// above and deleted nothing (doc 09 §2) — there is nothing a trailing
	// sync needs to account for either.
	if cp.Phase == RelocatePhaseFinalSync && len(cp.Manifest) > 0 {
		if err := deps.Sync(ctx, cp.Manifest); err != nil {
			return report, fmt.Errorf("cache: relocate to cache: final sync: %w", err)
		}
	}

	report.FinishedAt = deps.Now()
	return report, nil
}

// relocateCopyPhase walks share.Branches in order, copying and verifying
// every eligible file onto share.CachePath (copyMoveFile, reused
// unchanged from the mover — it already writes a temp-suffixed copy,
// verifies it, fsyncs it and renames it into place regardless of which
// direction src and dst run). It never touches a source file. Terminal
// outcomes (skip, conflict, failure) are recorded in report immediately;
// a successful copy is not — it is only a manifest entry, since whether
// it ever becomes a ResultMoved depends on the delete phase, still to
// come.
func relocateCopyPhase(ctx context.Context, share Share, cfg Config, deps Deps, hooks RunHooks, resume RelocateCheckpoint, report *Report) (manifest []parity.ManifestEntry, interrupted bool, err error) {
	plan := make([][]string, len(share.Branches))
	total := 0
	for i, b := range share.Branches {
		rels, e := enumerateFiles(b)
		if e != nil {
			return nil, false, fmt.Errorf("cache: enumerate branch %q: %w", b, e)
		}
		plan[i] = rels
		total += len(rels)
	}

	preCopyOpen, err := shareOpenChecker(ctx, deps.Open)
	if err != nil {
		return nil, false, fmt.Errorf("cache: snapshot open files for share %q: %w", share.Name, err)
	}

	done := 0
branchLoop:
	for i, b := range share.Branches {
		resumeAfter := ""
		if i == resume.BranchIndex {
			resumeAfter = resume.LastPath
		}
		disk := filepath.Dir(b)

		for _, rel := range plan[i] {
			// A file at or before the saved checkpoint was already reached
			// by an earlier, interrupted run — but relocateCopyItem is
			// idempotent for one it already copied and verified onto
			// cache: it copies nothing and simply returns that file's own
			// manifest entry. Calling it here, rather than skipping the
			// file outright, is what keeps a resumed run's manifest
			// complete — the copy phase's own checkpoint carries no
			// manifest of its own (RelocateCheckpoint's doc comment), so
			// this is the only way a resumed run ever learns that file
			// belongs in it.
			resumed := i < resume.BranchIndex || (resumeAfter != "" && rel <= resumeAfter)

			if ctx.Err() != nil || hooks.stopRequested() {
				interrupted = true
				break branchLoop
			}

			entry, me := relocateCopyItem(ctx, share, disk, b, rel, cfg, deps, preCopyOpen)
			if entry != nil {
				report.add(*entry)
				hooks.logf("relocate: %s %s/%s%s", entry.Result, share.Name, rel, entry.reasonSuffix())
			}
			if me != nil {
				manifest = append(manifest, *me)
			}

			done++
			if resumed {
				// Already past this file's own checkpoint — nothing new to
				// save.
				continue
			}
			if total > 0 {
				hooks.progress(done * 50 / total)
			}
			if err := hooks.checkpointRelocate(RelocateCheckpoint{Phase: RelocatePhaseCopying, BranchIndex: i, LastPath: rel}); err != nil {
				return manifest, false, fmt.Errorf("cache: save relocate checkpoint: %w", err)
			}
		}
	}
	return manifest, interrupted, nil
}

// relocateCopyItem decides and, when eligible, executes one array-side
// file's copy onto cache. Exactly one of its two return values is
// non-nil: entry for a terminal outcome the copy phase must report now,
// or manifestEntry for a file that is now safely and verifiably on
// cache, awaiting the sync and delete phases still to come. preCopyOpen
// answers the pre-copy open check, possibly from a snapshot taken once
// for the whole copy phase (relocateCopyPhase's own shareOpenChecker
// call); the pre-unlink re-check inside finishRelocateDelete always uses
// deps.Open directly instead, never preCopyOpen.
func relocateCopyItem(ctx context.Context, share Share, disk, branch, rel string, cfg Config, deps Deps, preCopyOpen OpenChecker) (entry *Entry, manifestEntry *parity.ManifestEntry) {
	src := filepath.Join(branch, rel)
	dst := filepath.Join(share.CachePath, rel)

	srcInfo, statErr := os.Lstat(src)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return &Entry{Share: share.Name, Path: rel, Result: ResultSkippedGone}, nil
		}
		return &Entry{Share: share.Name, Path: rel, Result: ResultFailed, Err: statErr.Error()}, nil
	}
	if !srcInfo.Mode().IsRegular() {
		return &Entry{Share: share.Name, Path: rel, Result: ResultSkippedNotRegular}, nil
	}

	// A previous, interrupted run may have already copied and verified
	// this file onto cache without this run ever having reached the
	// delete phase for it — finish nothing here, just recognize it and
	// fold it back into the manifest, the same "duplicate, never a gap"
	// principle Run's own resume logic follows.
	if dstInfo, derr := os.Lstat(dst); derr == nil {
		same, checkErr := isSamePendingCopy(src, dst, srcInfo, dstInfo, cfg.VerifyChecksum)
		if checkErr != nil {
			return &Entry{Share: share.Name, Path: rel, Result: ResultFailed, Err: checkErr.Error()}, nil
		}
		if !same {
			return &Entry{Share: share.Name, Path: rel, Result: ResultConflict}, nil
		}
		return nil, &parity.ManifestEntry{RelPath: filepath.Join(share.Name, rel), Size: srcInfo.Size(), MTime: srcInfo.ModTime(), SourceDisk: disk, TargetDisk: filepath.Dir(share.CachePath)}
	} else if !errors.Is(derr, fs.ErrNotExist) {
		return &Entry{Share: share.Name, Path: rel, Result: ResultFailed, Err: derr.Error()}, nil
	}

	open, oerr := preCopyOpen.IsOpen(ctx, src)
	if oerr != nil {
		return &Entry{Share: share.Name, Path: rel, Result: ResultFailed, Err: oerr.Error()}, nil
	}
	if open {
		return &Entry{Share: share.Name, Path: rel, Bytes: srcInfo.Size(), Result: ResultSkippedOpen}, nil
	}

	if err := copyMoveFile(src, dst, srcInfo, cfg, deps); err != nil {
		if errors.Is(err, errTargetAppeared) {
			return &Entry{Share: share.Name, Path: rel, Result: ResultConflict}, nil
		}
		return &Entry{Share: share.Name, Path: rel, Result: ResultFailed, Err: err.Error()}, nil
	}

	return nil, &parity.ManifestEntry{RelPath: filepath.Join(share.Name, rel), Size: srcInfo.Size(), MTime: srcInfo.ModTime(), SourceDisk: disk, TargetDisk: filepath.Dir(share.CachePath)}
}

// relocateDeletePhase removes manifest's own array-side sources, in
// order, starting from startIndex (Checkpoint.DeletedCount on resume) —
// the delete phase only ever runs after RelocateToCache's own sync call
// has already succeeded, so every source it touches here is already
// safely duplicated and verified on cache.
func relocateDeletePhase(ctx context.Context, share Share, deps Deps, hooks RunHooks, report *Report, manifest []parity.ManifestEntry, startIndex int) (interrupted bool, err error) {
	for i := startIndex; i < len(manifest); i++ {
		if ctx.Err() != nil || hooks.stopRequested() {
			return true, nil
		}

		me := manifest[i]
		entry := finishRelocateDelete(ctx, share, me, deps)
		report.add(entry)
		hooks.logf("relocate: %s %s/%s%s", entry.Result, share.Name, entry.Path, entry.reasonSuffix())

		if err := hooks.checkpointRelocate(RelocateCheckpoint{Phase: RelocatePhaseDeleting, Manifest: manifest, DeletedCount: i + 1}); err != nil {
			return false, fmt.Errorf("cache: save relocate checkpoint: %w", err)
		}
	}
	return false, nil
}

// finishRelocateDelete re-checks me's array-side source for an open
// handle — doc 09 §2's re-check-immediately-before-unlink step, applied
// here exactly as Run's own finishPendingDelete applies it — and removes
// it only when it is clear. A source that is (or became) open is left in
// place: both copies are complete and correct, so nothing is lost, and a
// future run's own resume completes the delete.
func finishRelocateDelete(ctx context.Context, share Share, me parity.ManifestEntry, deps Deps) Entry {
	// me.RelPath is disk-relative (share.Name/rel, matching
	// DiffFile.RelPath's own shape — ManifestEntry's doc comment) so it
	// already carries the share prefix src needs; rel strips that same
	// prefix back off for Entry.Path, which stays share-relative like
	// every other Entry this package produces.
	src := filepath.Join(me.SourceDisk, me.RelPath)
	rel := strings.TrimPrefix(me.RelPath, share.Name+"/")

	open, err := deps.Open.IsOpen(ctx, src)
	if err != nil {
		return Entry{Share: share.Name, Path: rel, Bytes: me.Size, Result: ResultMovedPendingDelete, Err: err.Error()}
	}
	if open {
		return Entry{Share: share.Name, Path: rel, Bytes: me.Size, Result: ResultMovedPendingDelete}
	}
	if err := os.Remove(src); err != nil {
		return Entry{Share: share.Name, Path: rel, Bytes: me.Size, Result: ResultFailed, Err: err.Error()}
	}
	return Entry{Share: share.Name, Path: rel, Bytes: me.Size, Result: ResultMoved}
}

// PrecheckResult is what a caller shows before starting a relocation
// (doc 09 §2: "Containers using the share are listed before starting,
// with an offer to stop them — relocating a live database is the same
// hazard as moving an open file"). OpenPaths is every file, relative to
// share, that some process currently holds open across both the cache
// and array sides — this package's only available signal, since
// internal/container does not exist yet (doc 06 §3) and a client or
// container reached through the union mount shows up as held open by
// mergerfs itself, never attributably by name (openchecker.go's own doc
// comment). A caller that already knows which containers bind-mount this
// share can cross-reference that list against OpenPaths itself; this
// package has no way to do that lookup on its own.
type PrecheckResult struct {
	OpenPaths []string
}

// Precheck reports every currently-open file under share, both its cache
// path and every array branch, without moving or deleting anything.
func Precheck(ctx context.Context, share Share, deps Deps) (PrecheckResult, error) {
	deps = deps.withDefaults()
	var result PrecheckResult

	open, err := shareOpenChecker(ctx, deps.Open)
	if err != nil {
		return PrecheckResult{}, fmt.Errorf("cache: precheck: snapshot open files for share %q: %w", share.Name, err)
	}

	roots := append([]string{share.CachePath}, share.Branches...)
	for _, root := range roots {
		rels, err := enumerateFiles(root)
		if err != nil {
			return PrecheckResult{}, fmt.Errorf("cache: precheck: enumerate %q: %w", root, err)
		}
		for _, rel := range rels {
			isOpen, err := open.IsOpen(ctx, filepath.Join(root, rel))
			if err != nil {
				return PrecheckResult{}, fmt.Errorf("cache: precheck: %q: %w", filepath.Join(root, rel), err)
			}
			if isOpen {
				result.OpenPaths = append(result.OpenPaths, rel)
			}
		}
	}
	return result, nil
}
