package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// DefaultSkewTolerancePercent is PlanRebalance's own default (doc 09 §3):
// a share's most- and least-full disk are considered even once they are
// within this many percentage points of each other.
const DefaultSkewTolerancePercent = 5.0

// DefaultRebalanceBatchLimit is Deps.RebalanceBatchLimit's own default: a
// fixed margin comfortably under parity.DefaultRemovedFilesMax (500) that
// stays safe regardless of the array's own size — unlike the percent
// rule, the count rule's threshold does not depend on anything about the
// array's current state, so a fixed conservative default can never be
// wrong.
const DefaultRebalanceBatchLimit = 200

// ErrRebalanceTrackedCountRequired is RunRebalance's refusal to start at
// all when Deps.TrackedFileCount is nil: sizing a batch against the
// threshold guard's percent rule needs the guard's own tracked file
// count, and there is no safe way to guess it (Deps.TrackedFileCount's
// own doc comment) — a nil dependency here is a caller that has not
// wired the real thing yet, not something RunRebalance can default
// around.
var ErrRebalanceTrackedCountRequired = errors.New("cache: rebalance requires Deps.TrackedFileCount")

// ErrRebalanceUnsafeBatchSize is returned before any os.Remove when a
// batch that already passed sizing can no longer be shown safe against
// the threshold guard's own rules — either because rebalanceBatchSize
// itself could not find a positive size that stays under both rules, or
// because the pre-delete re-check (RunRebalance's own "before deleting a
// batch's sources, re-check both rules fresh") found the ceiling dropped
// since the batch started. Either way, nothing in the batch has been
// deleted yet.
var ErrRebalanceUnsafeBatchSize = errors.New("cache: rebalance batch size is not safe against the threshold guard's current thresholds")

// RebalanceMove is one file PlanRebalance decided to move from its own
// most-full disk to its own least-full disk, for one share (doc 09 §3).
// SourceBranch and TargetBranch are share-scoped branch directories —
// the same "<disk>/<share>" shape Share.Branches carries — so
// filepath.Join(branch, RelPath) is the file's real path on each side and
// filepath.Dir(branch) is the branch's own data disk mount, matching
// parity.ManifestEntry.SourceDisk/TargetDisk's own shape.
type RebalanceMove struct {
	Share        string
	RelPath      string
	SourceBranch string
	TargetBranch string
	Size         int64
}

// RebalanceWarning is one condition PlanRebalance surfaces for the UI to
// show before a plan runs (doc 09 §3's "Path-preserving caveat") — never
// something PlanRebalance itself decides to act on.
type RebalanceWarning struct {
	Share  string
	Reason string
}

// RebalancePlan is PlanRebalance's whole output: a pure computation, no
// filesystem mutation (doc 09 §3). It is shown to the user before
// RunRebalance ever executes it — rebalancing is "not automatic" (doc 09
// §3) — and RunRebalance takes it as a value rather than recomputing it,
// so what runs is exactly what was shown.
type RebalancePlan struct {
	Moves    []RebalanceMove
	Warnings []RebalanceWarning
}

// RebalanceConfig tunes PlanRebalance's own target selection.
type RebalanceConfig struct {
	// SkewTolerancePercent overrides DefaultSkewTolerancePercent.
	SkewTolerancePercent float64
}

func (c RebalanceConfig) skewTolerancePercent() float64 {
	if c.SkewTolerancePercent > 0 {
		return c.SkewTolerancePercent
	}
	return DefaultSkewTolerancePercent
}

func (d Deps) rebalanceBatchLimit() int {
	if d.RebalanceBatchLimit != nil {
		if v := d.RebalanceBatchLimit(); v > 0 {
			return v
		}
	}
	return DefaultRebalanceBatchLimit
}

func (d Deps) rebalancePercentLimit() float64 {
	if d.RebalancePercentLimit != nil {
		if v := d.RebalancePercentLimit(); v > 0 && v < 100 {
			return v
		}
	}
	return parity.DefaultRemovedUpdatedPercent
}

// PlanRebalance computes, for every share with at least two branches, a
// plan that moves files from that share's own most-full disk to its own
// least-full disk until they are within cfg's skew tolerance or nothing
// more fits the share's own MinFreeSpace headroom (doc 09 §3). It never
// touches a filesystem beyond reading it: no copy, no delete, nothing
// that could lose data — that is entirely RunRebalance's job, against
// exactly the plan this returns.
func PlanRebalance(ctx context.Context, shares []Share, cfg RebalanceConfig, deps Deps) (RebalancePlan, error) {
	deps = deps.withDefaults()
	var plan RebalancePlan
	for _, s := range shares {
		if ctx.Err() != nil {
			return RebalancePlan{}, ctx.Err()
		}
		if len(s.Branches) < 2 {
			continue
		}
		moves, warnings, err := planShareRebalance(ctx, s, cfg, deps)
		if err != nil {
			return RebalancePlan{}, fmt.Errorf("cache: plan rebalance for share %q: %w", s.Name, err)
		}
		plan.Moves = append(plan.Moves, moves...)
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	return plan, nil
}

// branchState is planShareRebalance's own working state for one branch:
// its simulated usage (updated as moves are planned away from or onto
// it, without touching the filesystem) and the files still available to
// move away from it, largest first so each move reduces skew the most.
type branchState struct {
	branch string
	usage  DiskUsage
	files  []rebalanceCandidate
}

type rebalanceCandidate struct {
	rel  string
	size int64
}

func planShareRebalance(ctx context.Context, s Share, cfg RebalanceConfig, deps Deps) ([]RebalanceMove, []RebalanceWarning, error) {
	states := make([]*branchState, len(s.Branches))
	for i, b := range s.Branches {
		usage, err := deps.Usage(b)
		if err != nil {
			return nil, nil, fmt.Errorf("usage for %q: %w", b, err)
		}
		rels, err := enumerateFiles(b)
		if err != nil {
			return nil, nil, fmt.Errorf("enumerate %q: %w", b, err)
		}
		cands := make([]rebalanceCandidate, 0, len(rels))
		for _, rel := range rels {
			info, err := os.Lstat(filepath.Join(b, rel))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, nil, fmt.Errorf("stat %q: %w", filepath.Join(b, rel), err)
			}
			cands = append(cands, rebalanceCandidate{rel: rel, size: info.Size()})
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].size > cands[j].size })
		states[i] = &branchState{branch: b, usage: usage, files: cands}
	}

	var moves []RebalanceMove
	var warnings []RebalanceWarning
	warnedDirs := make(map[string]bool)

	for {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		mostIdx, leastIdx := 0, 0
		for i := 1; i < len(states); i++ {
			if states[i].usage.UsedPercent() > states[mostIdx].usage.UsedPercent() {
				mostIdx = i
			}
			if states[i].usage.UsedPercent() < states[leastIdx].usage.UsedPercent() {
				leastIdx = i
			}
		}
		most, least := states[mostIdx], states[leastIdx]
		if most.usage.UsedPercent()-least.usage.UsedPercent() <= cfg.skewTolerancePercent() {
			break
		}

		idx := pickMovableFile(most, least, s.MinFreeSpace)
		if idx < 0 {
			break
		}
		f := most.files[idx]
		most.files = append(most.files[:idx], most.files[idx+1:]...)

		if s.PathPreserving {
			dir := filepath.Dir(f.rel)
			key := s.Name + "/" + dir
			if !warnedDirs[key] {
				if _, err := os.Stat(filepath.Join(least.branch, dir)); err != nil {
					warnings = append(warnings, RebalanceWarning{
						Share: s.Name,
						Reason: fmt.Sprintf(
							"rebalancing will spread %s across more disks, which reduces the benefit of keeping folders together",
							filepath.Join(s.Name, dir),
						),
					})
					warnedDirs[key] = true
				}
			}
		}

		moves = append(moves, RebalanceMove{
			Share:        s.Name,
			RelPath:      f.rel,
			SourceBranch: most.branch,
			TargetBranch: least.branch,
			Size:         f.size,
		})

		most.usage.FreeBytes += f.size
		least.usage.FreeBytes -= f.size
	}

	return moves, warnings, nil
}

// pickMovableFile returns the index of the first (largest-first sorted)
// file on most that still fits on least once least's own MinFreeSpace
// headroom is kept (doc 09 §3's "or nothing more fits minfreespace"), and
// that would not leave least more full than most — a candidate that
// overshoots would invert the skew the caller is trying to reduce,
// letting the next iteration plan a reverse move that ping-pongs the
// same file back and forth through real copy/delete/guarded-sync work.
// Returns -1 when no candidate satisfies both.
func pickMovableFile(most, least *branchState, minFreeSpace int64) int {
	for i, f := range most.files {
		if least.usage.FreeBytes-minFreeSpace < f.size {
			continue
		}
		mostAfter := DiskUsage{TotalBytes: most.usage.TotalBytes, FreeBytes: most.usage.FreeBytes + f.size}
		leastAfter := DiskUsage{TotalBytes: least.usage.TotalBytes, FreeBytes: least.usage.FreeBytes - f.size}
		if leastAfter.UsedPercent() > mostAfter.UsedPercent() {
			continue
		}
		return i
	}
	return -1
}

// RebalancePhase is where one batch of a RunRebalance run currently is,
// mirroring RelocatePhase's own shape: copy and verify the batch, sync
// through the threshold guard (protecting the copies), delete the
// batch's sources, then sync again (protecting against the deletes) —
// Q14's order, applied per batch rather than once over the whole plan
// (see RunRebalance's own doc comment for why).
type RebalancePhase string

const (
	RebalancePhaseCopying   RebalancePhase = "copying"
	RebalancePhaseSyncing   RebalancePhase = "syncing"
	RebalancePhaseDeleting  RebalancePhase = "deleting"
	RebalancePhaseFinalSync RebalancePhase = "final_sync"
)

// RebalanceCheckpoint is RunRebalance's own resumable progress marker
// (Q29), checkpointed at batch boundaries: BatchStart and BatchEnd are
// plan.Moves indices bounding the batch currently in flight, so a crash
// resumes at that batch rather than restarting the whole plan or
// re-deleting anything a previous run's delete phase already finished.
// Manifest is set once a batch's own copy phase completes and carried
// unchanged through the rest of that batch's life, the same shape
// RelocateCheckpoint uses.
type RebalanceCheckpoint struct {
	BatchStart int                    `json:"batch_start"`
	BatchEnd   int                    `json:"batch_end"`
	Phase      RebalancePhase         `json:"phase"`
	Manifest   []parity.ManifestEntry `json:"manifest,omitempty"`
	// DeletedCount is how many of Manifest's entries the delete phase has
	// already finished (in order) within this batch — resuming the
	// delete phase skips straight to Manifest[DeletedCount:].
	DeletedCount int `json:"deleted_count,omitempty"`
}

func (h RunHooks) checkpointRebalance(cp RebalanceCheckpoint) error {
	if h.SaveCheckpoint == nil {
		return nil
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("cache: encode rebalance checkpoint: %w", err)
	}
	return h.SaveCheckpoint(data)
}

// RunRebalance executes plan (doc 09 §3, Q14, Q15) in successive
// batches, each running its own complete copy-verify / guarded-sync /
// delete / guarded-sync cycle before the next batch starts. It never runs
// one copy-all/sync/delete-all/sync-again pass over the whole plan: a
// plan can be larger than the threshold guard's own configured limits
// allow in one sync, and the guard's block lands on the trailing,
// post-delete sync — after sources are already gone, which is
// unrecoverable until a human confirms. Batching means every trailing
// sync this run ever makes is sized, in advance, to be one the guard's
// own rules cannot block.
//
// Deps.Sync and Deps.TrackedFileCount are both required: RunRebalance
// refuses immediately, before copying or deleting anything, when either
// is nil (ErrRebalanceTrackedCountRequired for the latter — see its own
// doc comment for why this one input has no safe default).
func RunRebalance(ctx context.Context, plan RebalancePlan, cfg Config, deps Deps, hooks RunHooks, initialCheckpoint []byte) (report Report, err error) {
	deps = deps.withDefaults()
	if deps.Sync == nil {
		return Report{}, errors.New("cache: run rebalance: Deps.Sync is required")
	}
	if deps.TrackedFileCount == nil {
		return Report{}, ErrRebalanceTrackedCountRequired
	}

	var cp RebalanceCheckpoint
	if len(initialCheckpoint) > 0 {
		if err := json.Unmarshal(initialCheckpoint, &cp); err != nil {
			return Report{}, fmt.Errorf("cache: decode rebalance checkpoint: %w", err)
		}
	}
	if cp.Phase == "" {
		cp.Phase = RebalancePhaseCopying
	}

	report = Report{StartedAt: deps.Now()}
	defer func() {
		if report.FinishedAt.IsZero() {
			report.FinishedAt = deps.Now()
		}
	}()

	total := len(plan.Moves)

	for cp.BatchStart < total {
		if cp.BatchEnd <= cp.BatchStart {
			size, serr := rebalanceBatchSize(ctx, deps, total-cp.BatchStart)
			if serr != nil {
				return report, serr
			}
			if size <= 0 {
				return report, ErrRebalanceUnsafeBatchSize
			}
			cp = RebalanceCheckpoint{BatchStart: cp.BatchStart, BatchEnd: cp.BatchStart + size, Phase: RebalancePhaseCopying}
			if err := hooks.checkpointRebalance(cp); err != nil {
				return report, err
			}
		}
		batch := plan.Moves[cp.BatchStart:cp.BatchEnd]

		if cp.Phase == RebalancePhaseCopying {
			manifest, interrupted, cerr := rebalanceCopyBatch(ctx, batch, cfg, deps, hooks, &report)
			if cerr != nil {
				return report, cerr
			}
			if interrupted {
				report.Interrupted = true
				return report, nil
			}
			cp = RebalanceCheckpoint{BatchStart: cp.BatchStart, BatchEnd: cp.BatchEnd, Phase: RebalancePhaseSyncing, Manifest: manifest}
			if err := hooks.checkpointRebalance(cp); err != nil {
				return report, err
			}
			if ctx.Err() != nil || hooks.stopRequested() {
				report.Interrupted = true
				return report, nil
			}
		}

		if cp.Phase == RebalancePhaseSyncing && len(cp.Manifest) == 0 {
			cp = RebalanceCheckpoint{BatchStart: cp.BatchStart, BatchEnd: cp.BatchEnd, Phase: RebalancePhaseDeleting}
		} else if cp.Phase == RebalancePhaseSyncing {
			if err := deps.Sync(ctx, cp.Manifest); err != nil {
				return report, fmt.Errorf("cache: rebalance: sync: %w", err)
			}
			cp = RebalanceCheckpoint{BatchStart: cp.BatchStart, BatchEnd: cp.BatchEnd, Phase: RebalancePhaseDeleting, Manifest: cp.Manifest}
			if err := hooks.checkpointRebalance(cp); err != nil {
				return report, err
			}
			if ctx.Err() != nil || hooks.stopRequested() {
				report.Interrupted = true
				return report, nil
			}
		}

		if cp.Phase == RebalancePhaseDeleting {
			// The pre-delete re-check only ever runs once per batch,
			// before this batch's first unlink — not on every resume
			// into an already-started delete phase, since deletion for
			// this batch has already begun and a later file's own
			// re-check (finishRebalanceDelete's open-handle check) is a
			// different guarantee than this one.
			if cp.DeletedCount == 0 && len(cp.Manifest) > 0 {
				safe, serr := rebalanceRecheckSafe(ctx, deps, len(cp.Manifest))
				if serr != nil {
					return report, serr
				}
				if !safe {
					return report, ErrRebalanceUnsafeBatchSize
				}
			}
			interrupted, derr := rebalanceDeleteBatch(ctx, deps, hooks, &report, cp.BatchStart, cp.BatchEnd, cp.Manifest, cp.DeletedCount)
			if derr != nil {
				return report, derr
			}
			if interrupted {
				report.Interrupted = true
				return report, nil
			}
			cp = RebalanceCheckpoint{BatchStart: cp.BatchStart, BatchEnd: cp.BatchEnd, Phase: RebalancePhaseFinalSync, Manifest: cp.Manifest}
			if err := hooks.checkpointRebalance(cp); err != nil {
				return report, err
			}
			if ctx.Err() != nil || hooks.stopRequested() {
				report.Interrupted = true
				return report, nil
			}
		}

		if cp.Phase == RebalancePhaseFinalSync && len(cp.Manifest) > 0 {
			if err := deps.Sync(ctx, cp.Manifest); err != nil {
				return report, fmt.Errorf("cache: rebalance: final sync: %w", err)
			}
		}

		if total > 0 {
			hooks.progress(cp.BatchEnd * 100 / total)
		}
		cp = RebalanceCheckpoint{BatchStart: cp.BatchEnd, Phase: RebalancePhaseCopying}
		if err := hooks.checkpointRebalance(cp); err != nil {
			return report, err
		}
		if ctx.Err() != nil || hooks.stopRequested() {
			report.Interrupted = true
			return report, nil
		}
	}

	report.FinishedAt = deps.Now()
	return report, nil
}

// rebalanceBatchSize decides how many of the plan's remaining moves the
// next batch may safely contain — the largest B such that B <=
// Deps.RebalanceBatchLimit() and B <= percentLimit*T/(100-percentLimit),
// where T is Deps.TrackedFileCount's own fresh, current reading and
// percentLimit is Deps.RebalancePercentLimit().
//
// The (100-percentLimit) denominator, rather than the plainer
// B<=percentLimit*T/100, accounts for what this batch's own copy phase
// is about to do to the guard's tracked count before its trailing,
// post-delete sync ever runs: once the batch's files are copied and
// protected by the first guarded sync, SnapRAID tracks both the still-
// present sources and their new copies, so the delete phase's own
// trailing sync sees a "before" count of T+B, not T. Solving
// B/(T+B)*100 <= percentLimit for B gives exactly this bound. T<=0 (an
// array with nothing tracked yet) cannot trip the percent rule at any
// batch size, mirroring guard.Evaluate's own "percent stays zero when
// totalBefore is zero" — so only the count rule constrains B then.
func rebalanceBatchSize(ctx context.Context, deps Deps, remaining int) (int, error) {
	if deps.TrackedFileCount == nil {
		return 0, ErrRebalanceTrackedCountRequired
	}
	tracked, err := deps.TrackedFileCount(ctx)
	if err != nil {
		return 0, fmt.Errorf("cache: rebalance: tracked file count: %w", err)
	}

	limit := deps.rebalanceBatchLimit()
	if tracked > 0 {
		percentLimit := deps.rebalancePercentLimit()
		if pctLimit := int(percentLimit * float64(tracked) / (100 - percentLimit)); pctLimit < limit {
			limit = pctLimit
		}
	}
	if limit > remaining {
		limit = remaining
	}
	return limit, nil
}

// rebalanceRecheckSafe re-verifies, immediately before a batch's first
// unlink, that removing batchSize sources cannot trip either guard rule
// against a fresh Deps.TrackedFileCount reading — never the sizing
// estimate rebalanceBatchSize made before this batch's own copy phase
// and first guarded sync ran. By this point that sync has already
// happened, so the fresh tracked count already reflects the "before" the
// guard's own next (delete-phase) Evaluate call will actually see —
// the plain percent formula applies directly, with no
// (100-percentLimit) adjustment needed.
func rebalanceRecheckSafe(ctx context.Context, deps Deps, batchSize int) (bool, error) {
	if deps.TrackedFileCount == nil {
		return false, ErrRebalanceTrackedCountRequired
	}
	if batchSize > deps.rebalanceBatchLimit() {
		return false, nil
	}
	tracked, err := deps.TrackedFileCount(ctx)
	if err != nil {
		return false, fmt.Errorf("cache: rebalance: tracked file count: %w", err)
	}
	if tracked <= 0 {
		return true, nil
	}
	percent := float64(batchSize) / float64(tracked) * 100
	return percent <= deps.rebalancePercentLimit(), nil
}

// rebalanceCopyBatch copies and verifies every file in batch (mover.go's
// own copyMoveFile, reused unchanged — it already writes a temp-suffixed
// copy, verifies, fsyncs and renames it into place regardless of which
// direction src and dst run) and returns the manifest the batch's own
// guarded syncs need. It never touches a source file.
func rebalanceCopyBatch(ctx context.Context, batch []RebalanceMove, cfg Config, deps Deps, hooks RunHooks, report *Report) (manifest []parity.ManifestEntry, interrupted bool, err error) {
	preCopyOpen, err := shareOpenChecker(ctx, deps.Open)
	if err != nil {
		return nil, false, fmt.Errorf("cache: rebalance: snapshot open files: %w", err)
	}

	for _, mv := range batch {
		if ctx.Err() != nil || hooks.stopRequested() {
			interrupted = true
			break
		}

		entry, me := rebalanceCopyItem(ctx, mv, cfg, deps, preCopyOpen)
		if entry != nil {
			report.add(*entry)
			hooks.logf("rebalance: %s %s/%s%s", entry.Result, mv.Share, mv.RelPath, entry.reasonSuffix())
		}
		if me != nil {
			manifest = append(manifest, *me)
		}
	}
	return manifest, interrupted, nil
}

// rebalanceCopyItem decides and, when eligible, executes one move's copy
// to its target branch. Exactly one of its two return values is
// non-nil, the same split relocateCopyItem uses: entry for a terminal
// outcome to report now, or manifestEntry for a file now safely and
// verifiably duplicated on its target branch, awaiting the sync and
// delete phases still to come. A target that already holds this exact
// file — from an earlier, interrupted run's own copy phase for this same
// batch — is recognized (isSamePendingCopy) and folded back into the
// manifest rather than copied again.
func rebalanceCopyItem(ctx context.Context, mv RebalanceMove, cfg Config, deps Deps, preCopyOpen OpenChecker) (entry *Entry, manifestEntry *parity.ManifestEntry) {
	src := filepath.Join(mv.SourceBranch, mv.RelPath)
	dst := filepath.Join(mv.TargetBranch, mv.RelPath)
	sourceDisk := filepath.Dir(mv.SourceBranch)
	targetDisk := filepath.Dir(mv.TargetBranch)

	srcInfo, statErr := os.Lstat(src)
	if statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultSkippedGone}, nil
		}
		return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultFailed, Err: statErr.Error()}, nil
	}
	if !srcInfo.Mode().IsRegular() {
		return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultSkippedNotRegular}, nil
	}

	if dstInfo, derr := os.Lstat(dst); derr == nil {
		same, checkErr := isSamePendingCopy(src, dst, srcInfo, dstInfo, cfg.VerifyChecksum)
		if checkErr != nil {
			return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultFailed, Err: checkErr.Error()}, nil
		}
		if !same {
			return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultConflict}, nil
		}
		return nil, &parity.ManifestEntry{RelPath: filepath.Join(mv.Share, mv.RelPath), Size: srcInfo.Size(), MTime: srcInfo.ModTime(), SourceDisk: sourceDisk, TargetDisk: targetDisk}
	} else if !errors.Is(derr, fs.ErrNotExist) {
		return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultFailed, Err: derr.Error()}, nil
	}

	open, oerr := preCopyOpen.IsOpen(ctx, src)
	if oerr != nil {
		return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultFailed, Err: oerr.Error()}, nil
	}
	if open {
		return &Entry{Share: mv.Share, Path: mv.RelPath, Bytes: srcInfo.Size(), Result: ResultSkippedOpen}, nil
	}

	if err := copyMoveFile(src, dst, srcInfo, cfg, deps); err != nil {
		if errors.Is(err, errTargetAppeared) {
			return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultConflict}, nil
		}
		return &Entry{Share: mv.Share, Path: mv.RelPath, Result: ResultFailed, Err: err.Error()}, nil
	}

	return nil, &parity.ManifestEntry{RelPath: filepath.Join(mv.Share, mv.RelPath), Size: srcInfo.Size(), MTime: srcInfo.ModTime(), SourceDisk: sourceDisk, TargetDisk: targetDisk}
}

// rebalanceDeleteBatch removes manifest's own sources, in order, starting
// from startIndex (RebalanceCheckpoint.DeletedCount on resume) — this
// only ever runs after the batch's own first guarded sync has already
// succeeded, so every source it touches here is already safely
// duplicated and verified on its target branch.
func rebalanceDeleteBatch(ctx context.Context, deps Deps, hooks RunHooks, report *Report, batchStart, batchEnd int, manifest []parity.ManifestEntry, startIndex int) (interrupted bool, err error) {
	for i := startIndex; i < len(manifest); i++ {
		if ctx.Err() != nil || hooks.stopRequested() {
			return true, nil
		}

		me := manifest[i]
		entry := finishRebalanceDelete(ctx, me, deps)
		report.add(entry)
		hooks.logf("rebalance: %s %s/%s%s", entry.Result, entry.Share, entry.Path, entry.reasonSuffix())

		cp := RebalanceCheckpoint{BatchStart: batchStart, BatchEnd: batchEnd, Phase: RebalancePhaseDeleting, Manifest: manifest, DeletedCount: i + 1}
		if err := hooks.checkpointRebalance(cp); err != nil {
			return false, fmt.Errorf("cache: save rebalance checkpoint: %w", err)
		}
	}
	return false, nil
}

// finishRebalanceDelete re-checks me's source for an open handle — the
// re-check-immediately-before-unlink step doc 09 §2 requires, applied
// here exactly as finishRelocateDelete applies it — and removes it only
// when it is clear. share and rel are recovered from me.RelPath (share-
// prefixed, disk-relative — ManifestEntry's own doc comment) purely for
// Entry's own reporting; the file's real path is
// filepath.Join(me.SourceDisk, me.RelPath) regardless of how many shares
// share a batch.
func finishRebalanceDelete(ctx context.Context, me parity.ManifestEntry, deps Deps) Entry {
	share, rel := splitManifestRelPath(me.RelPath)
	src := filepath.Join(me.SourceDisk, me.RelPath)

	open, err := deps.Open.IsOpen(ctx, src)
	if err != nil {
		return Entry{Share: share, Path: rel, Bytes: me.Size, Result: ResultMovedPendingDelete, Err: err.Error()}
	}
	if open {
		return Entry{Share: share, Path: rel, Bytes: me.Size, Result: ResultMovedPendingDelete}
	}
	if err := os.Remove(src); err != nil {
		return Entry{Share: share, Path: rel, Bytes: me.Size, Result: ResultFailed, Err: err.Error()}
	}
	return Entry{Share: share, Path: rel, Bytes: me.Size, Result: ResultMoved}
}

// splitManifestRelPath splits a ManifestEntry.RelPath ("<share>/<rel>",
// ManifestEntry's own doc comment) back into its share and share-relative
// parts, matching relocate.go's own strings.TrimPrefix(me.RelPath,
// share.Name+"/") — expressed without an already-known share name here,
// since one rebalance batch can span more than one share.
func splitManifestRelPath(relPath string) (share, rel string) {
	share, rel, ok := strings.Cut(relPath, "/")
	if !ok {
		return relPath, ""
	}
	return share, rel
}
