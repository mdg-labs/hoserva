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
