// The threshold guard (doc 02 §2) — the single most important safety
// feature in the product. `snapraid diff` reports what a sync is about to
// commit; a large removal or update count, or a data disk that dropped to
// zero files, is either an intentional cleanup or a disaster in progress
// (ransomware, a failing disk, a bad `rm -rf`, an unmounted share).
// Syncing over that destroys the parity that could have recovered it, so
// SnapraidEngine.Sync evaluates this package's Guard on a fresh diff
// before every real sync, unconditionally — see Sync's own doc comment
// for how that is structural, not a convention a caller could skip.
package parity

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// DefaultRemovedFilesMax and DefaultRemovedUpdatedPercent are Q16's own
// starting thresholds — guesses until the soak test's diff history exists
// to replace them (doc 02 §2, doc 13 Q16).
const (
	DefaultRemovedFilesMax       = 500
	DefaultRemovedUpdatedPercent = 10.0
)

// GuardConfig is the threshold guard's own configuration (doc 02 §2, Q16).
// A caller can raise either threshold; there is deliberately no field here
// that disables the guard's evaluation itself or skips the confirmation
// SyncOpts.Confirm carries — "configurable but cannot be disabled
// entirely; the minimum is a confirmation prompt" (doc 02 §2).
type GuardConfig struct {
	// RemovedFilesMax blocks when accounted-for removals (Removed minus
	// Q15's own manifest matches) exceed this count. Zero (the type's own
	// zero value) uses DefaultRemovedFilesMax.
	RemovedFilesMax int
	// RemovedUpdatedPercent blocks when (accounted-for removals + Updated)
	// exceed this percent of the array's total file count before the
	// diff. Zero uses DefaultRemovedUpdatedPercent.
	RemovedUpdatedPercent float64
}

func (c GuardConfig) removedFilesMax() int {
	if c.RemovedFilesMax > 0 {
		return c.RemovedFilesMax
	}
	return DefaultRemovedFilesMax
}

func (c GuardConfig) removedUpdatedPercent() float64 {
	if c.RemovedUpdatedPercent > 0 {
		return c.RemovedUpdatedPercent
	}
	return DefaultRemovedUpdatedPercent
}

// GuardTrigger identifies which of the guard's three rules (doc 02 §2)
// fired. More than one can fire on the same diff.
type GuardTrigger int

const (
	// TriggerRemovedCount is "removed files exceed N".
	TriggerRemovedCount GuardTrigger = iota
	// TriggerRemovedUpdatedPercent is "removed + updated files exceed X%
	// of the array".
	TriggerRemovedUpdatedPercent
	// TriggerZeroFiles is "a data disk reports zero files where it
	// previously had files" (the unmounted-disk case).
	TriggerZeroFiles
)

func (t GuardTrigger) String() string {
	switch t {
	case TriggerRemovedCount:
		return "removed-count"
	case TriggerRemovedUpdatedPercent:
		return "removed-updated-percent"
	case TriggerZeroFiles:
		return "zero-files"
	default:
		return "unknown"
	}
}

// ZeroFilesDisk is one disk TriggerZeroFiles fired for.
type ZeroFilesDisk struct {
	Disk        string
	FilesBefore int
}

// GuardResult is the guard's decision on one diff (doc 02 §2): what the
// dashboard's blocked-sync banner and a notification both render from.
type GuardResult struct {
	Blocked  bool
	Triggers []GuardTrigger

	// RemovedCount and RemovedUpdatedPercent are the values the two
	// count/percent triggers actually compared against their thresholds —
	// already net of AccountedRemovals (Q15).
	RemovedCount          int
	RemovedUpdatedPercent float64
	ZeroFilesDisks        []ZeroFilesDisk

	// AccountedRemovals is Q15's own "moved by Hoserva" group: manifest
	// entries whose removal was matched to a reappearance on their
	// recorded target disk in this diff. Shown as its own group, and
	// excluded from RemovedCount/RemovedUpdatedPercent above; every other
	// removal counts fully.
	AccountedRemovals []ManifestEntry

	// Diff is the diff this decision was made against, with
	// DiffReport.MovedByHoserva filled in to len(AccountedRemovals) —
	// BuildDiffReport itself always leaves it zero (diff_parse.go's own
	// doc comment: matching a manifest is the guard's job).
	Diff DiffReport
}

// hasTrigger reports whether t fired in this result.
func (r GuardResult) hasTrigger(t GuardTrigger) bool {
	for _, got := range r.Triggers {
		if got == t {
			return true
		}
	}
	return false
}

func (r GuardResult) summary() string {
	s := fmt.Sprintf("removed=%d (threshold-relevant), removed+updated=%.1f%%, triggers=%v", r.RemovedCount, r.RemovedUpdatedPercent, r.Triggers)
	if len(r.ZeroFilesDisks) > 0 {
		s += fmt.Sprintf(", zero-files disks=%v", r.ZeroFilesDisks)
	}
	return s
}

// ErrGuardBlocked is the sentinel a caller matches with errors.Is against
// whatever Sync returns when the threshold guard blocks a sync and the
// caller has not set SyncOpts.Confirm. The concrete error is always a
// *GuardBlockedError, carrying the full GuardResult a caller needs to
// dispatch a notification and render the dashboard's blocked-sync banner
// (doc 02 §2) without re-evaluating anything itself.
var ErrGuardBlocked = errors.New("parity: threshold guard blocked the sync")

// GuardBlockedError is returned by Sync in place of running `snapraid
// sync` at all: "the sync is held ... the sync does not proceed until a
// human decides" (doc 02 §2).
type GuardBlockedError struct {
	Result GuardResult
}

func (e *GuardBlockedError) Error() string {
	return fmt.Sprintf("parity: threshold guard blocked the sync: %s", e.Result.summary())
}

func (e *GuardBlockedError) Unwrap() error { return ErrGuardBlocked }

// Guard evaluates one diff against its own thresholds (doc 02 §2, Q16).
// Its zero value, Guard{}, is fully usable and applies the package's
// default thresholds.
type Guard struct {
	Config GuardConfig
}

// Evaluate is the guard's whole job: decide whether diff, accounting for
// manifest (Q15) and removingDisks (doc 09 §4's zero-files exemption),
// should block a sync.
func (g Guard) Evaluate(diff DiffReport, manifest []ManifestEntry, removingDisks map[string]bool) GuardResult {
	accounted, matchedRemovals := matchManifest(diff, manifest)
	diff.MovedByHoserva = len(matchedRemovals)

	// matchedRemovals is built as a subset of the diff's own distinct
	// removed-file keys (matchManifest never adds a key that isn't in
	// diff.RemovedFiles), so its size can never exceed the removal set
	// it's being subtracted from — no clamp needed, and none is added:
	// a negative result here would mean the diff itself is inconsistent
	// (Removed doesn't cover its own RemovedFiles), which must fail loud,
	// not be silently absorbed.
	removedCount := diff.Removed - len(matchedRemovals)

	var totalBefore int
	for _, dd := range diff.PerDisk {
		totalBefore += dd.FilesBefore
	}
	var percent float64
	if totalBefore > 0 {
		percent = float64(removedCount+diff.Updated) / float64(totalBefore) * 100
	}

	var triggers []GuardTrigger
	if removedCount > g.Config.removedFilesMax() {
		triggers = append(triggers, TriggerRemovedCount)
	}
	if percent > g.Config.removedUpdatedPercent() {
		triggers = append(triggers, TriggerRemovedUpdatedPercent)
	}

	var zeroDisks []ZeroFilesDisk
	for disk, dd := range diff.PerDisk {
		if removingDisks[disk] {
			continue
		}
		if dd.FilesBefore > 0 && dd.FilesAfter == 0 {
			zeroDisks = append(zeroDisks, ZeroFilesDisk{Disk: disk, FilesBefore: dd.FilesBefore})
		}
	}
	if len(zeroDisks) > 0 {
		triggers = append(triggers, TriggerZeroFiles)
	}

	return GuardResult{
		Blocked:               len(triggers) > 0,
		Triggers:              triggers,
		RemovedCount:          removedCount,
		RemovedUpdatedPercent: percent,
		ZeroFilesDisks:        zeroDisks,
		AccountedRemovals:     accounted,
		Diff:                  diff,
	}
}

// fileKey identifies one file on one disk, for matching a removal against
// a reappearance elsewhere in the same diff.
type fileKey struct{ disk, relPath string }

// diffFileKeySets projects diff's RemovedFiles/AddedFiles into the same
// fileKey shape matchManifest and manifestNeedsTargetConfirmation both
// match a ManifestEntry's SourceDisk/TargetDisk against, so the two never
// risk looking at the diff two different ways.
func diffFileKeySets(diff DiffReport) (removed, added map[fileKey]bool) {
	removed = make(map[fileKey]bool, len(diff.RemovedFiles))
	for _, f := range diff.RemovedFiles {
		removed[fileKey{f.Disk, f.RelPath}] = true
	}
	added = make(map[fileKey]bool, len(diff.AddedFiles))
	for _, f := range diff.AddedFiles {
		added[fileKey{f.Disk, f.RelPath}] = true
	}
	return removed, added
}

// matchManifest is Q15's own rule: a manifest entry is accounted when its
// RelPath was removed from SourceDisk in this diff *and* the same RelPath
// either appears (added or copied) on TargetDisk within that same diff, or
// was already confirmed there by an earlier sync (ManifestEntry.
// TargetConfirmed) — Q14's mandated two-phase order (copy+verify, sync,
// delete, sync) means a relocation's own addition and its eventual
// trailing-sync removal are structurally never in the same diff, so the
// same-diff check alone can never account for one (#248).
// TargetConfirmed is never taken on trust from the manifest itself: it is
// set only by SnapraidEngine.Sync, and only after a real `snapraid list`
// showed the file genuinely already tracked on TargetDisk — the same
// grounding in real, observed state the same-diff check already had.
//
// Neither half of that rule can ever be satisfied when TargetDisk is not a
// SnapRAID-tracked data disk at all — a cache.RelocateToCache manifest
// entry's TargetDisk is the cache mount, which never appears in a SnapRAID
// diff's AddedFiles and is never something `snapraid list` tracks for
// ConfirmManifestTargets to confirm (#240). diff.PerDisk is built directly
// from the diff's own "data:" config echo (BuildDiffReport), so whenever it
// is populated it is exactly the set of disks SnapRAID currently tracks for
// this diff — a real, structurally grounded fact the manifest itself has no
// say over, and (Q19 requiring at least one data disk) always non-empty for
// a diff BuildDiffReport actually produced. When diff.PerDisk is non-empty
// and TargetDisk is not one of its keys, the entry is accounted as soon as
// its source-side removal appears in the diff, the same "removed and the
// manifest says why" grounding the data-disk case gets from a real
// reappearance or a real `snapraid list`. When TargetDisk *is* a tracked
// data disk, or diff.PerDisk is empty (never true for a real diff, but true
// of a hand-built DiffReport a test constructs without it), the strict
// same-diff-or-confirmed rule above still applies unchanged — an entry
// cannot claim the looser cache rule just by pointing at an unrelated data
// disk that happens not to have gained the file, or by relying on a diff
// that never says which disks it tracks in the first place.
//
// It returns both the matched manifest entries (for GuardResult's own
// display group) and matchedRemovals — the *distinct* removal identities
// (disk, path) those entries matched, deduplicated. matchedRemovals is
// always a subset of the diff's own removed-file keys, by construction:
// nothing is ever added to it unless it was already in removed below. This
// is what makes Evaluate's subtraction safe against a manifest recording
// the same real removal more than once (a retry/resume loop appending an
// entry per attempt, say) — each real removal can only ever be subtracted
// once no matter how many manifest entries name it.
func matchManifest(diff DiffReport, manifest []ManifestEntry) (accounted []ManifestEntry, matchedRemovals map[fileKey]struct{}) {
	if len(manifest) == 0 {
		return nil, nil
	}

	removed, added := diffFileKeySets(diff)

	matchedRemovals = make(map[fileKey]struct{})
	for _, m := range manifest {
		key := fileKey{m.SourceDisk, m.RelPath}
		if _, already := matchedRemovals[key]; already {
			continue
		}
		if !removed[key] {
			continue
		}
		_, targetIsDataDisk := diff.PerDisk[filepath.Clean(m.TargetDisk)]
		if len(diff.PerDisk) == 0 || targetIsDataDisk {
			if !added[fileKey{m.TargetDisk, m.RelPath}] && !m.TargetConfirmed {
				continue
			}
		}
		matchedRemovals[key] = struct{}{}
		accounted = append(accounted, m)
	}
	return accounted, matchedRemovals
}

// manifestNeedsTargetConfirmation reports whether manifest has an entry
// whose SourceDisk removal shows up in diff but whose TargetDisk addition
// does not — exactly the shape a Q14 two-phase relocation's trailing sync
// leaves matchManifest unable to account for on its own (#248).
// ConfirmManifestTargets calls this before paying for a real `snapraid
// list`: every ordinary, same-diff relocation — the plain mover, and every
// manifest matchManifest can already account for — costs exactly what it
// always did, no extra invocation.
//
// It also skips an entry whose TargetDisk is confirmed non-data by a
// non-empty diff.PerDisk (an array→cache relocation, #256): matchManifest
// already accounts such an entry from diff.PerDisk alone, regardless of
// TargetConfirmed, so a `snapraid list` could never change the outcome —
// and it could never confirm a cache target anyway, since List only
// reports tracked data disks. An empty diff.PerDisk still falls back to
// the strict rule below, the same way matchManifest does: it cannot tell
// TargetDisk apart from a genuine, currently-untracked data disk.
func manifestNeedsTargetConfirmation(diff DiffReport, manifest []ManifestEntry) bool {
	if len(manifest) == 0 {
		return false
	}
	removed, added := diffFileKeySets(diff)
	for _, m := range manifest {
		if m.TargetConfirmed {
			continue
		}
		if len(diff.PerDisk) > 0 {
			if _, targetIsDataDisk := diff.PerDisk[filepath.Clean(m.TargetDisk)]; !targetIsDataDisk {
				continue
			}
		}
		key := fileKey{m.SourceDisk, m.RelPath}
		if removed[key] && !added[fileKey{m.TargetDisk, m.RelPath}] {
			return true
		}
	}
	return false
}

// ConfirmManifestTargets applies Q14/#248's own trailing-sync exemption to
// manifest before a caller evaluates Guard.Evaluate against diff: when
// manifestNeedsTargetConfirmation finds an entry the same-diff check in
// matchManifest cannot account for, this runs a real `snapraid list`
// through lister (List's own doc comment: tracked state only, never a live
// directory walk) and marks ManifestEntry.TargetConfirmed on every entry
// whose file is already tracked on its own TargetDisk — the two-phase
// relocation case where the addition was recorded by an earlier sync's own
// diff, not this one. An ordinary, same-diff relocation never pays for the
// extra `snapraid list` call: manifestNeedsTargetConfirmation returns
// false and manifest comes back unmodified.
//
// SnapraidEngine.Sync and the diff-preview endpoint (RunParityDiff,
// internal/api/parity_handler.go, #252) both call this before evaluating
// the guard, so the two can never reach a different verdict on the same
// manifest/diff state. lister is typed as Engine, not *SnapraidEngine, so
// any caller already holding a parity.Engine — including one outside this
// package, e.g. internal/job — can call this without a type assertion. The
// manifest passed in is never mutated or written back anywhere — this
// returns a fresh slice for the caller's own guard evaluation.
func ConfirmManifestTargets(ctx context.Context, lister Engine, diff DiffReport, manifest []ManifestEntry) ([]ManifestEntry, error) {
	if !manifestNeedsTargetConfirmation(diff, manifest) {
		return manifest, nil
	}
	list, err := lister.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("parity: confirming relocation manifest targets: %w", err)
	}
	tracked := make(map[fileKey]bool, len(list.Files))
	for _, f := range list.Files {
		mount, ok := list.DataMounts[f.Disk]
		if !ok {
			continue
		}
		tracked[fileKey{filepath.Clean(mount), f.RelPath}] = true
	}

	confirmed := make([]ManifestEntry, len(manifest))
	for i, m := range manifest {
		confirmed[i] = m
		if tracked[fileKey{m.TargetDisk, m.RelPath}] {
			confirmed[i].TargetConfirmed = true
		}
	}
	return confirmed, nil
}

// anyDiskEmptied reports whether diff would leave any disk (including one
// in RemovingDisks — that exemption only affects the guard's own
// zero-files rule above, not SnapRAID's own separate, unconditional
// refusal to sync an emptied disk) with zero files where it previously had
// some. syncArgv's own doc comment covers why Sync needs this regardless
// of what the guard itself decided.
func anyDiskEmptied(diff DiffReport) bool {
	for _, dd := range diff.PerDisk {
		if dd.FilesBefore > 0 && dd.FilesAfter == 0 {
			return true
		}
	}
	return false
}
