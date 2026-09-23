package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrEvacuationNoOtherBranch is PlanEvacuation's refusal for a share that
// has a branch on the disk being evacuated but no other branch to move
// that share's files onto — there is nowhere doc 09 §4 step 3's own
// enumeration could send them.
var ErrEvacuationNoOtherBranch = errors.New("cache: share has no other branch to evacuate onto")

// ErrEvacuationWontFit is PlanEvacuation's own pre-check refusal (doc 09
// §4 step 1): the remaining branches cannot hold everything currently on
// the disk being evacuated, even after each one's own MinFreeSpace
// headroom is kept. PlanEvacuation never mutates a filesystem (like
// PlanRebalance, its own doc comment), so this is returned before
// anything is copied. pool.EvacuationFits is the same pre-check's cheap,
// whole-disk-sum early filter, safe to run before any of this; this is
// the authoritative, per-file one — the real bin-packing this builds can
// still fail even when that coarser sum-only check passes.
var ErrEvacuationWontFit = errors.New("cache: remaining disks do not have room to evacuate this disk")

// ErrEvacuationUnsupportedEntry is PlanEvacuation's refusal for an entry
// the copy path never moves — a symlink, fifo, socket or device node, or
// a leftover partial copy. EvacuationPostCheck accepts nothing but empty
// directories, so planning past one would run every copy and sync only to
// fail the post-check, and fail every retry the same way.
var ErrEvacuationUnsupportedEntry = errors.New("cache: evacuated disk holds an entry evacuation cannot move")

// PlanEvacuation computes, for every share with a branch on disk, a plan
// that moves every file on that branch onto the share's other branches
// (doc 09 §4 steps 1 and 3: "mechanically a rebalance targeting one
// specific source disk"). Each file is placed on whichever eligible
// remaining branch currently has the most free space, respecting the
// share's own MinFreeSpace headroom — not mergerfs's own create policy:
// evacuation redistributes files that already exist, it does not create
// new ones, so there is nothing for a create policy to decide here
// either way. It returns the plan in RunRebalance's own RebalancePlan
// shape: RunRebalance already implements Q14/Q15's two-phase
// copy/verify/guarded-sync/delete/guarded-sync execution, batched
// against the threshold guard's own thresholds — this file adds nothing
// to that execution path, only a specialized planner and, in
// EvacuationPostCheck below, the evacuation-specific post-check
// RunRebalance itself has no reason to perform.
//
// A share with no branch on disk contributes nothing to the plan — there
// is nothing of that share's to move. A share whose only branch is on
// disk fails outright (ErrEvacuationNoOtherBranch): evacuation cannot
// proceed until that share's own layout is fixed some other way. Any
// file this plan cannot place anywhere, even respecting MinFreeSpace,
// fails the whole call (ErrEvacuationWontFit) before returning a plan at
// all — this doubles as step 1's own pre-check: an evacuation the
// remaining pool cannot hold is refused here, never discovered partway
// through a real copy.
func PlanEvacuation(ctx context.Context, disk string, shares []Share, deps Deps) (RebalancePlan, error) {
	deps = deps.withDefaults()
	var plan RebalancePlan
	// Keyed by disk mount: every share's branch on one disk draws on the
	// same filesystem's free space, so planned moves must be charged
	// against one shared figure, not one per share.
	diskUsage := make(map[string]*DiskUsage)
	for _, s := range shares {
		if ctx.Err() != nil {
			return RebalancePlan{}, ctx.Err()
		}
		moves, warnings, err := planShareEvacuation(ctx, disk, s, deps, diskUsage)
		if err != nil {
			return RebalancePlan{}, fmt.Errorf("cache: plan evacuation of %s for share %q: %w", disk, s.Name, err)
		}
		plan.Moves = append(plan.Moves, moves...)
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	return plan, nil
}

// planShareEvacuation plans one share's own share of disk's evacuation —
// reusing rebalance.go's own branchState/rebalanceCandidate shapes, since
// this is the same "usage per branch, largest-file-first candidates"
// bookkeeping planShareRebalance already uses, just walked once against
// a single, fixed source branch instead of iteratively against whichever
// pair of branches is currently most and least full.
func planShareEvacuation(ctx context.Context, disk string, s Share, deps Deps, diskUsage map[string]*DiskUsage) ([]RebalanceMove, []RebalanceWarning, error) {
	var sourceBranch string
	var remaining []string
	for _, b := range s.Branches {
		if filepath.Dir(b) == disk {
			sourceBranch = b
			continue
		}
		remaining = append(remaining, b)
	}
	if sourceBranch == "" {
		return nil, nil, nil
	}
	if len(remaining) == 0 {
		return nil, nil, fmt.Errorf("%w: share %q", ErrEvacuationNoOtherBranch, s.Name)
	}

	states := make([]*evacuationTarget, len(remaining))
	for i, b := range remaining {
		u, ok := diskUsage[filepath.Dir(b)]
		if !ok {
			usage, err := deps.Usage(b)
			if err != nil {
				return nil, nil, fmt.Errorf("usage for %q: %w", b, err)
			}
			u = &usage
			diskUsage[filepath.Dir(b)] = u
		}
		states[i] = &evacuationTarget{branch: b, usage: u}
	}

	if err := refuseUnsupportedEntries(sourceBranch); err != nil {
		return nil, nil, err
	}
	rels, err := enumerateFiles(sourceBranch)
	if err != nil {
		return nil, nil, fmt.Errorf("enumerate %q: %w", sourceBranch, err)
	}
	cands := make([]rebalanceCandidate, 0, len(rels))
	for _, rel := range rels {
		info, err := os.Lstat(filepath.Join(sourceBranch, rel))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, nil, fmt.Errorf("stat %q: %w", filepath.Join(sourceBranch, rel), err)
		}
		cands = append(cands, rebalanceCandidate{rel: rel, size: info.Size()})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].size > cands[j].size })

	var moves []RebalanceMove
	var warnings []RebalanceWarning
	warnedDirs := make(map[string]bool)

	for _, f := range cands {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}

		target := pickEvacuationTarget(states, f.size, s.MinFreeSpace)
		if target == nil {
			return nil, nil, fmt.Errorf("%w: share %q, %s (%d bytes)", ErrEvacuationWontFit, s.Name, f.rel, f.size)
		}

		if s.PathPreserving {
			dir := filepath.Dir(f.rel)
			key := s.Name + "/" + dir
			if !warnedDirs[key] {
				if _, err := os.Stat(filepath.Join(target.branch, dir)); err != nil {
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
			SourceBranch: sourceBranch,
			TargetBranch: target.branch,
			Size:         f.size,
		})
		target.usage.FreeBytes -= f.size
	}

	return moves, warnings, nil
}

// evacuationTarget is one remaining branch; usage is shared with every
// other branch on the same disk (PlanEvacuation's diskUsage).
type evacuationTarget struct {
	branch string
	usage  *DiskUsage
}

// refuseUnsupportedEntries fails on the first entry under root that
// enumerateFiles would skip — anything but a directory or a regular,
// non-temporary file — so the refusal comes before any copy, with its
// cause, instead of from EvacuationPostCheck after the whole run.
func refuseUnsupportedEntries(root string) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() || strings.Contains(d.Name(), tempSuffix) {
			return fmt.Errorf("%w: %s", ErrEvacuationUnsupportedEntry, path)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// pickEvacuationTarget returns the remaining branch with the most free
// space (after minFreeSpace headroom) that can still hold size — the
// same most-free-space-first choice pickMovableFile's own least-full
// side makes, reused here because it happens to also spread an
// evacuated disk's files evenly across whatever remains, rather than
// piling them all onto whichever branch happens to come first. Unlike
// pickMovableFile, there is no skew-inversion check: evacuation is not
// balancing anything, only placing every file somewhere that fits.
// Returns nil when no remaining branch has room — the caller turns that
// into ErrEvacuationWontFit.
func pickEvacuationTarget(states []*evacuationTarget, size, minFreeSpace int64) *evacuationTarget {
	var best *evacuationTarget
	for _, st := range states {
		if st.usage.FreeBytes-minFreeSpace < size {
			continue
		}
		if best == nil || st.usage.FreeBytes > best.usage.FreeBytes {
			best = st
		}
	}
	return best
}

// ErrEvacuationNotEmpty is EvacuationPostCheck's own refusal (doc 09 §4
// step 6): something other than an empty directory remains under one of
// disk's own share branches after a caller's RunRebalance call for
// PlanEvacuation's plan reported no interruption. This means either a
// file was skipped (still open, in conflict, or created on disk after
// the plan was built) or that run never reached this branch's own delete
// phase — either way, disk is not actually safe to remove yet. A caller
// must never proceed past ErrEvacuationNotEmpty to doc 09 §4 steps 7-9
// (branch-list removal, SnapRAID removal, unmount): doing so would
// remove disk from the array's own config while it still holds data with
// no copy anywhere else — exactly the corruption this whole design
// exists to prevent.
var ErrEvacuationNotEmpty = errors.New("cache: evacuated disk still has content besides empty directories")

// EvacuationPostCheck implements doc 09 §4 step 6's own post-check:
// every share's own branch on disk must contain nothing but empty
// directories once a caller's RunRebalance call for PlanEvacuation's own
// plan has finished without interruption. It only ever reads the
// filesystem — it never deletes anything itself.
//
// This checks the branches PlanEvacuation itself enumerated files from
// (each share's own "<disk>/<share>" directory) — the files doc 09 §4's
// own "sources" refers to — not disk's mount point as a whole: SnapRAID's
// own bookkeeping (its content/parity files, when disk is one of the
// ones doc 02 §2's own placement chose to hold a copy) legitimately
// lives directly on a data disk's own root, outside any share. Step 8's
// own "remove from the SnapRAID data list" is what retires those, not
// this check.
func EvacuationPostCheck(disk string, shares []Share) error {
	for _, s := range shares {
		for _, b := range s.Branches {
			if filepath.Dir(b) != disk {
				continue
			}
			if err := checkOnlyEmptyDirs(b); err != nil {
				return fmt.Errorf("%w: %s: %v", ErrEvacuationNotEmpty, b, err)
			}
		}
	}
	return nil
}

// checkOnlyEmptyDirs walks root and fails on the first non-directory
// entry it finds — a bare directory tree, with nothing left under it, is
// the only shape step 6's post-check accepts. A root that does not exist
// at all counts as empty: a share that was never present on disk in the
// first place has nothing left to check.
func checkOnlyEmptyDirs(root string) error {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var bad error
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		bad = fmt.Errorf("%s is not an empty directory", path)
		return filepath.SkipAll
	})
	if bad != nil {
		return bad
	}
	return err
}
