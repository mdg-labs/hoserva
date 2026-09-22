package parity

import (
	"fmt"
	"testing"
)

// diffWithPerDisk is a small builder for hand-constructed DiffReports —
// the guard's own unit tests exercise Guard.Evaluate directly, decoupled
// from ParseDiff/ParseStatus (those are covered separately in
// diff_parse_test.go and status_parse_test.go).
func diffWithPerDisk(perDisk map[string]DiskDiff, removed, updated int) DiffReport {
	return DiffReport{Removed: removed, Updated: updated, PerDisk: perDisk}
}

// TestGuard_BlocksOnRemovedCount is this issue's central data-loss
// reproduction for the removed-count trigger (doc 02 §2): a disaster that
// removes more than the default 500 files must block, even though no
// disk was emptied and the percentage stayed low.
func TestGuard_BlocksOnRemovedCount(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 100000, FilesAfter: 99499},
	}, 501, 0)

	result := Guard{}.Evaluate(diff, nil, nil)

	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true for 501 removed files (default max 500)")
	}
	if !result.hasTrigger(TriggerRemovedCount) {
		t.Fatalf("Evaluate: Triggers = %v, want to include TriggerRemovedCount", result.Triggers)
	}
	if result.RemovedCount != 501 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 501", result.RemovedCount)
	}
}

func TestGuard_AllowsRemovedCountAtThreshold(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 100000, FilesAfter: 99500},
	}, 500, 0)

	result := Guard{}.Evaluate(diff, nil, nil)

	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true at exactly the default threshold (500), want false: %+v", result)
	}
}

// TestGuard_BlocksOnRemovedUpdatedPercent reproduces a disaster shaped
// like ransomware rewriting a large share in place: few outright removals,
// but a large fraction of the array updated.
func TestGuard_BlocksOnRemovedUpdatedPercent(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 1000},
	}, 0, 150) // 150/1000 = 15% > default 10%

	result := Guard{}.Evaluate(diff, nil, nil)

	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true for 15% removed+updated (default max 10%)")
	}
	if !result.hasTrigger(TriggerRemovedUpdatedPercent) {
		t.Fatalf("Evaluate: Triggers = %v, want to include TriggerRemovedUpdatedPercent", result.Triggers)
	}
}

func TestGuard_AllowsRemovedUpdatedPercentAtThreshold(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 1000},
	}, 0, 100) // exactly 10%

	result := Guard{}.Evaluate(diff, nil, nil)

	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true at exactly the default percent threshold (10%%), want false: %+v", result)
	}
}

// TestGuard_BlocksOnZeroFiles is doc 06 §3's own scenario: a disk
// unmounts, and the next diff reports it holding zero files where it had
// some — exactly what the lab test (guard_lab_test.go) drives for real.
func TestGuard_BlocksOnZeroFiles(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 5000, FilesAfter: 5000},
		"/mnt/disk2": {FilesBefore: 3000, FilesAfter: 0},
	}, 3000, 0)

	result := Guard{}.Evaluate(diff, nil, nil)

	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true when a data disk drops from nonzero to zero files")
	}
	if !result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Evaluate: Triggers = %v, want to include TriggerZeroFiles", result.Triggers)
	}
	if len(result.ZeroFilesDisks) != 1 || result.ZeroFilesDisks[0].Disk != "/mnt/disk2" {
		t.Fatalf("Evaluate: ZeroFilesDisks = %+v, want just /mnt/disk2", result.ZeroFilesDisks)
	}
	// disk2's 3000 removals also exceed the default count/percent
	// thresholds on their own — TriggerRemovedCount is expected too, and
	// asserting that guards against a change that made the triggers
	// mutually exclusive instead of independent (doc 02 §2 lists three
	// separate rules, "or").
	if !result.hasTrigger(TriggerRemovedCount) {
		t.Fatalf("Evaluate: Triggers = %v, want TriggerRemovedCount alongside TriggerZeroFiles", result.Triggers)
	}
}

// TestGuard_ZeroFilesExemptForRemovingDisk is doc 09 §4 / Q15's own
// exemption: a disk deliberately being evacuated is allowed to reach
// zero files without tripping the zero-files rule — but an unrelated
// disk emptying in the same diff still trips it.
func TestGuard_ZeroFilesExemptForRemovingDisk(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 200, FilesAfter: 0},
		// disk2 pads the array's total file count so 200 removals stay
		// under the default 10% threshold too — this test isolates the
		// zero-files exemption, not the percent trigger.
		"/mnt/disk2": {FilesBefore: 9800, FilesAfter: 9800},
	}, 200, 0)

	result := Guard{}.Evaluate(diff, nil, map[string]bool{"/mnt/disk1": true})

	if result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Evaluate: TriggerZeroFiles fired for a disk in RemovingDisks: %+v", result)
	}
	// 200 removed still exceeds neither default threshold on its own
	// (500 files / a small percent here), so the whole result must be
	// unblocked, not just the one trigger suppressed.
	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true for an evacuating disk's own expected removals: %+v", result)
	}
}

func TestGuard_ZeroFilesNotExemptForUnrelatedDisk(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 200, FilesAfter: 0}, // removing
		"/mnt/disk2": {FilesBefore: 50, FilesAfter: 0},  // not removing — disaster
	}, 250, 0)

	result := Guard{}.Evaluate(diff, nil, map[string]bool{"/mnt/disk1": true})

	if !result.hasTrigger(TriggerZeroFiles) {
		t.Fatal("Evaluate: TriggerZeroFiles did not fire for disk2, which is not in RemovingDisks")
	}
	if len(result.ZeroFilesDisks) != 1 || result.ZeroFilesDisks[0].Disk != "/mnt/disk2" {
		t.Fatalf("Evaluate: ZeroFilesDisks = %+v, want just /mnt/disk2", result.ZeroFilesDisks)
	}
}

// TestGuard_AccountedRemovalsExcludedFromThreshold is Q15's own worked
// example: an evacuation's own manifest keeps its accounted removals out
// of the threshold count, while an unrelated removal in the same diff
// still counts fully.
func TestGuard_AccountedRemovalsExcludedFromThreshold(t *testing.T) {
	diff := DiffReport{
		Removed: 5,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 10000, FilesAfter: 9998}, // 3 moved off, 2 genuinely deleted
			"/mnt/disk2": {FilesBefore: 10000, FilesAfter: 10003},
		},
		RemovedFiles: []DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/b.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/c.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unrelated-delete-1.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unrelated-delete-2.mkv"},
		},
		AddedFiles: []DiffFile{
			{Disk: "/mnt/disk2", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk2", RelPath: "movies/b.mkv"},
			{Disk: "/mnt/disk2", RelPath: "movies/c.mkv"},
		},
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		{RelPath: "movies/b.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		{RelPath: "movies/c.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}

	result := Guard{Config: GuardConfig{RemovedFilesMax: 1}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 3 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want 3 matched entries", result.AccountedRemovals)
	}
	if result.Diff.MovedByHoserva != 3 {
		t.Fatalf("Evaluate: Diff.MovedByHoserva = %d, want 3", result.Diff.MovedByHoserva)
	}
	// RemovedFilesMax=1 and 2 unaccounted removals remain (5 removed - 3
	// accounted) — the guard must still block on those, since Q15 only
	// exempts removals a manifest actually matches, never a whole diff.
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true — the unrelated removals must still count")
	}
	if result.RemovedCount != 2 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 2 (5 removed - 3 accounted)", result.RemovedCount)
	}
}

// TestGuard_ManifestEntryWithoutReappearanceNotAccounted checks the other
// half of Q15's own rule: a manifest entry only counts as accounted when
// the file actually reappears on its recorded target disk in *this*
// diff — a manifest entry alone, or one whose copy failed and never
// landed, must not silently exempt a genuine loss.
func TestGuard_ManifestEntryWithoutReappearanceNotAccounted(t *testing.T) {
	diff := DiffReport{
		Removed: 1,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		// AddedFiles deliberately empty: the manifest claims a.mkv moved
		// to disk2, but it never actually appeared there in this diff.
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}

	result := Guard{Config: GuardConfig{RemovedFilesMax: 0}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 0 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want none — the file never reappeared on its target disk", result.AccountedRemovals)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 1 (unaccounted)", result.RemovedCount)
	}
}

// TestGuard_TargetConfirmedAccountsTrailingSyncRemoval is this issue's
// (#248) own reproduction: a Q14 two-phase relocation's trailing sync
// shows only the removal — the addition was already committed by an
// earlier sync, so it never reappears in AddedFiles here — yet the
// manifest entry must still be accounted when TargetConfirmed says a
// real `snapraid list` already found the file on its target disk.
func TestGuard_TargetConfirmedAccountsTrailingSyncRemoval(t *testing.T) {
	diff := DiffReport{
		Removed: 1,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		// AddedFiles deliberately empty: unlike the same-diff case, a
		// trailing sync's diff never shows the addition — it was already
		// committed by the sync before this one.
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2", TargetConfirmed: true},
	}

	result := Guard{Config: GuardConfig{RemovedFilesMax: 0}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 1 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want 1 — TargetConfirmed must account it even without a same-diff reappearance", result.AccountedRemovals)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 0 (the one removal is accounted)", result.RemovedCount)
	}
	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true, want false: %+v", result)
	}
}

// TestGuard_TargetConfirmedDoesNotMaskAnUnrelatedRemoval proves
// TargetConfirmed only exempts the exact (disk, path) it names — an
// unrelated real removal at the same source disk, not covered by any
// manifest entry, still counts fully and still blocks.
func TestGuard_TargetConfirmedDoesNotMaskAnUnrelatedRemoval(t *testing.T) {
	diff := DiffReport{
		Removed: 3,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 97},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
		RemovedFiles: []DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unexpected-delete-1.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unexpected-delete-2.mkv"},
		},
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2", TargetConfirmed: true},
	}

	// RemovedFilesMax's own zero value falls back to the package default
	// (500, GuardConfig.removedFilesMax) — 1 is the lowest threshold that
	// still actually blocks, so 2 unaccounted removals must trip it.
	result := Guard{Config: GuardConfig{RemovedFilesMax: 1}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 1 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want exactly 1 (movies/a.mkv only)", result.AccountedRemovals)
	}
	if result.RemovedCount != 2 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 2 — the two unrelated deletions must still count", result.RemovedCount)
	}
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true — unaccounted removals alongside a TargetConfirmed one must still block")
	}
}

// TestGuard_TargetConfirmedOnWrongEntryStillBlocks reproduces this issue's
// own "stale/incorrect manifest entry" acceptance criterion: TargetConfirmed
// set true on an entry naming the wrong path must not exempt a real
// removal it doesn't actually name — matchManifest matches by (disk, path)
// key, and TargetConfirmed on one entry never leaks into any other.
func TestGuard_TargetConfirmedOnWrongEntryStillBlocks(t *testing.T) {
	diff := DiffReport{
		Removed: 1,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/unexpected-delete.mkv"}},
	}
	// This entry's own path was genuinely confirmed on disk2 (some other,
	// unrelated relocation), but it names a different RelPath than the
	// one actually removed above — a stale/incorrect manifest entry must
	// never account for a removal it doesn't name.
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2", TargetConfirmed: true},
	}

	// RemovedFilesMax's own zero value falls back to the package default
	// (500) — 0 removed files max would too, so this uses the percent
	// trigger instead: 1 removed against a before-count of 200 files is
	// 0.5%, well over a RemovedUpdatedPercent lowered to 0.1%.
	result := Guard{Config: GuardConfig{RemovedUpdatedPercent: 0.1}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 0 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want none — the confirmed entry names a different path", result.AccountedRemovals)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 1 (unaccounted)", result.RemovedCount)
	}
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true")
	}
}

// TestGuard_CacheTargetRemovalAccounted is this issue's (#240) own
// reproduction: a cache.RelocateToCache manifest entry's TargetDisk is the
// cache mount, which is never a SnapRAID data disk and so can never appear
// in diff.PerDisk, diff.AddedFiles, or a `snapraid list` — matching on a
// same-diff reappearance or TargetConfirmed is structurally impossible for
// it, regardless of sync ordering. The removal must still be accounted once
// it shows up on its own SourceDisk in the diff, the same way an array-to-
// array relocation already is.
func TestGuard_CacheTargetRemovalAccounted(t *testing.T) {
	diff := DiffReport{
		Removed: 1,
		PerDisk: map[string]DiskDiff{
			// /mnt/cache deliberately absent: it is never a SnapRAID data
			// disk, so it never appears here.
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
		},
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		// AddedFiles deliberately empty: cache is never diffed by SnapRAID,
		// so the target side of an array→cache relocation can never appear
		// here, in any diff.
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"},
	}

	result := Guard{Config: GuardConfig{RemovedFilesMax: 0}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 1 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want 1 — a cache-target removal must be accounted from the source-side removal alone", result.AccountedRemovals)
	}
	if result.RemovedCount != 0 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 0 (the one removal is accounted)", result.RemovedCount)
	}
	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true, want false: %+v", result)
	}
}

// TestGuard_CacheTargetDoesNotMaskUnrelatedRemoval proves the cache-target
// accounting only exempts the exact (disk, path) a manifest entry names —
// an unrelated real removal on the same source disk, not covered by any
// manifest entry, still counts fully and still blocks, exactly as an
// unaccounted array-to-array removal already does.
func TestGuard_CacheTargetDoesNotMaskUnrelatedRemoval(t *testing.T) {
	diff := DiffReport{
		Removed: 3,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 97},
		},
		RemovedFiles: []DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unexpected-delete-1.mkv"},
			{Disk: "/mnt/disk1", RelPath: "movies/unexpected-delete-2.mkv"},
		},
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"},
	}

	// RemovedFilesMax's own zero value falls back to the package default
	// (500, GuardConfig.removedFilesMax) — 1 is the lowest threshold that
	// still actually blocks, so 2 unaccounted removals must trip it.
	result := Guard{Config: GuardConfig{RemovedFilesMax: 1}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 1 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want exactly 1 (movies/a.mkv only)", result.AccountedRemovals)
	}
	if result.RemovedCount != 2 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 2 — the two unrelated deletions must still count", result.RemovedCount)
	}
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true — unaccounted removals alongside a cache-target one must still block")
	}
}

// TestGuard_DataDiskTargetStillRequiresStrictMatch proves the looser
// cache-target rule above never leaks to a manifest entry whose TargetDisk
// genuinely is a SnapRAID-tracked data disk (diff.PerDisk): such an entry
// still needs a same-diff reappearance or TargetConfirmed, exactly as
// before #240 — an entry cannot borrow the cache rule just by pointing at
// a data disk that happens not to have gained the file in this diff.
func TestGuard_DataDiskTargetStillRequiresStrictMatch(t *testing.T) {
	diff := DiffReport{
		Removed: 1,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		// AddedFiles deliberately empty and TargetConfirmed deliberately
		// unset: disk2 is a real, tracked data disk, so the strict rule —
		// not the cache-target one — must apply to it.
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}

	// RemovedFilesMax's own zero value falls back to the package default
	// (500) — 1 removed file never trips that on its own, so this uses the
	// percent trigger instead, the same way TestGuard_
	// TargetConfirmedOnWrongEntryStillBlocks does: 1 removed against a
	// before-count of 200 files is 0.5%, well over a RemovedUpdatedPercent
	// lowered to 0.1%.
	result := Guard{Config: GuardConfig{RemovedUpdatedPercent: 0.1}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 0 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want none — a tracked data-disk target still needs a same-diff reappearance or TargetConfirmed", result.AccountedRemovals)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 1 (unaccounted)", result.RemovedCount)
	}
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true")
	}
}

// TestGuard_EmptyPerDiskFallsBackToStrictMatch proves the cache-target rule
// never activates on a diff that never says which disks it tracks in the
// first place: with diff.PerDisk empty, matchManifest cannot tell TargetDisk
// apart from a genuine, currently-untracked data disk, so it must fall back
// to the strict same-diff-or-confirmed rule rather than assume TargetDisk is
// a cache mount. A real diff from BuildDiffReport always populates PerDisk
// (Q19: at least one data disk) — an empty PerDisk only ever happens in a
// hand-built DiffReport, the shape EngineDiffGuard's own tests
// (internal/job) use.
func TestGuard_EmptyPerDiskFallsBackToStrictMatch(t *testing.T) {
	diff := DiffReport{
		Removed:      1,
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		// PerDisk deliberately left unset.
	}
	manifest := []ManifestEntry{
		{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}

	result := Guard{Config: GuardConfig{RemovedFilesMax: 0}}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 0 {
		t.Fatalf("Evaluate: AccountedRemovals = %+v, want none — an empty diff.PerDisk must not be read as \"TargetDisk is non-data\"", result.AccountedRemovals)
	}
	if result.RemovedCount != 1 {
		t.Fatalf("Evaluate: RemovedCount = %d, want 1 (unaccounted)", result.RemovedCount)
	}
}

// TestManifestNeedsTargetConfirmation exercises the gate
// SnapraidEngine.Sync calls before ever paying for a real `snapraid list`
// (#248): it must fire only when a manifest names a removal this diff
// shows but whose target-side addition it does not, and never fire for an
// empty manifest, an already same-diff-matched entry, or an entry already
// marked TargetConfirmed.
func TestManifestNeedsTargetConfirmation(t *testing.T) {
	trailingDiff := DiffReport{
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
	}
	sameDiff := DiffReport{
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		AddedFiles:   []DiffFile{{Disk: "/mnt/disk2", RelPath: "movies/a.mkv"}},
	}
	entry := ManifestEntry{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}

	if manifestNeedsTargetConfirmation(trailingDiff, nil) {
		t.Error("manifestNeedsTargetConfirmation: true for an empty manifest, want false")
	}
	if !manifestNeedsTargetConfirmation(trailingDiff, []ManifestEntry{entry}) {
		t.Error("manifestNeedsTargetConfirmation: false for a trailing-sync removal with no same-diff addition, want true")
	}
	if manifestNeedsTargetConfirmation(sameDiff, []ManifestEntry{entry}) {
		t.Error("manifestNeedsTargetConfirmation: true when matchManifest can already account it via the same diff, want false")
	}
	already := entry
	already.TargetConfirmed = true
	if manifestNeedsTargetConfirmation(trailingDiff, []ManifestEntry{already}) {
		t.Error("manifestNeedsTargetConfirmation: true for an entry already TargetConfirmed, want false")
	}

	// A non-empty diff.PerDisk that does not list TargetDisk means
	// matchManifest already accounts the entry on its own (#240's
	// cache-target rule) — paying for a `snapraid list` here could never
	// confirm a cache target anyway (#256), so this must not fire even
	// though the removal is a trailing-sync one with no same-diff addition.
	cacheTargetDiff := DiffReport{
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
		},
	}
	cacheEntry := ManifestEntry{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/cache"}
	if manifestNeedsTargetConfirmation(cacheTargetDiff, []ManifestEntry{cacheEntry}) {
		t.Error("manifestNeedsTargetConfirmation: true for a cache-target entry confirmed non-data by diff.PerDisk, want false")
	}

	// The same shape, but TargetDisk genuinely is a tracked data disk
	// (present in diff.PerDisk): the strict rule must still apply exactly
	// as before #256.
	dataTargetDiff := DiffReport{
		RemovedFiles: []DiffFile{{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"}},
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100, FilesAfter: 99},
			"/mnt/disk2": {FilesBefore: 100, FilesAfter: 100},
		},
	}
	if !manifestNeedsTargetConfirmation(dataTargetDiff, []ManifestEntry{entry}) {
		t.Error("manifestNeedsTargetConfirmation: false for a trailing-sync removal whose TargetDisk is a tracked data disk, want true")
	}
}

// TestGuard_DuplicateManifestEntriesDoNotMaskUnrelatedRemovals reproduces
// this issue's own rejected-commit finding: a manifest that records the
// same real removal many times (a retry/resume loop appending an entry
// per attempt, say — nothing in the types forbids it) must never let those
// duplicates subtract more than the one real removal they actually match,
// even when the diff also carries a large, unrelated removal on another
// disk. The exact numbers reproduce the rejected commit's own probe: 700
// duplicate entries for one real move, alongside 600 unrelated deletions —
// which, under the old per-manifest-entry subtraction, drove RemovedCount
// to 0 and let a 600-file disaster sync straight over the parity that
// could have recovered it.
func TestGuard_DuplicateManifestEntriesDoNotMaskUnrelatedRemovals(t *testing.T) {
	const unrelatedRemovals = 600
	const duplicateManifestEntries = 700

	removedFiles := make([]DiffFile, 0, unrelatedRemovals+1)
	removedFiles = append(removedFiles, DiffFile{Disk: "/mnt/disk1", RelPath: "movies/a.mkv"})
	for i := 0; i < unrelatedRemovals; i++ {
		removedFiles = append(removedFiles, DiffFile{
			Disk:    "/mnt/disk2",
			RelPath: fmt.Sprintf("shows/unrelated-delete-%d.mkv", i),
		})
	}

	diff := DiffReport{
		Removed: len(removedFiles),
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 100000, FilesAfter: 99999},
			"/mnt/disk2": {FilesBefore: 100000, FilesAfter: 100000 - unrelatedRemovals},
			"/mnt/disk3": {FilesBefore: 100000, FilesAfter: 100001},
		},
		RemovedFiles: removedFiles,
		AddedFiles: []DiffFile{
			{Disk: "/mnt/disk3", RelPath: "movies/a.mkv"},
		},
	}

	manifest := make([]ManifestEntry, 0, duplicateManifestEntries)
	for i := 0; i < duplicateManifestEntries; i++ {
		manifest = append(manifest, ManifestEntry{
			RelPath:    "movies/a.mkv",
			SourceDisk: "/mnt/disk1",
			TargetDisk: "/mnt/disk3",
		})
	}

	result := Guard{}.Evaluate(diff, manifest, nil)

	if len(result.AccountedRemovals) != 1 {
		t.Fatalf("Evaluate: AccountedRemovals = %d entries, want exactly 1 — the one real removal, deduplicated", len(result.AccountedRemovals))
	}
	if result.Diff.MovedByHoserva != 1 {
		t.Fatalf("Evaluate: Diff.MovedByHoserva = %d, want 1", result.Diff.MovedByHoserva)
	}
	if result.RemovedCount != unrelatedRemovals {
		t.Fatalf("Evaluate: RemovedCount = %d, want %d (the 700 duplicate manifest entries must never subtract more than the 1 real removal they match)", result.RemovedCount, unrelatedRemovals)
	}
	if !result.Blocked {
		t.Fatal("Evaluate: Blocked = false, want true — 600 unrelated removals must not be masked by 700 duplicate manifest entries")
	}
	if !result.hasTrigger(TriggerRemovedCount) {
		t.Fatalf("Evaluate: Triggers = %v, want to include TriggerRemovedCount", result.Triggers)
	}
}

func TestGuard_NoTriggersWhenNothingUnusualChanged(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 1005},
	}, 0, 3)

	result := Guard{}.Evaluate(diff, nil, nil)

	if result.Blocked {
		t.Fatalf("Evaluate: Blocked = true for an ordinary diff: %+v", result)
	}
	if len(result.Triggers) != 0 {
		t.Fatalf("Evaluate: Triggers = %v, want none", result.Triggers)
	}
}

// TestGuardConfig_RaisesButNeverDisables confirms a caller can raise
// either threshold arbitrarily high, but that doing so still leaves the
// guard evaluating and blocking correctly at whatever bar it was given —
// there is no configuration value (e.g. 0, negative) that disables the
// rule outright; GuardConfig's own zero value instead falls back to the
// package default (Q16).
func TestGuardConfig_RaisesButNeverDisables(t *testing.T) {
	diff := diffWithPerDisk(map[string]DiskDiff{
		"/mnt/disk1": {FilesBefore: 1000000, FilesAfter: 990000},
	}, 10000, 0)

	raised := Guard{Config: GuardConfig{RemovedFilesMax: 20000}}.Evaluate(diff, nil, nil)
	if raised.Blocked {
		t.Fatalf("Evaluate: Blocked = true after raising the bar above the actual removed count: %+v", raised)
	}

	zeroValue := Guard{Config: GuardConfig{RemovedFilesMax: 0}}.Evaluate(diff, nil, nil)
	if !zeroValue.Blocked {
		t.Fatal("Evaluate: Blocked = false with GuardConfig{} (zero value) — it must fall back to DefaultRemovedFilesMax, not disable the check")
	}
}
