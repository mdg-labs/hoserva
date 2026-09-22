package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// rebalanceShare builds a Share with branchCount per-disk directories
// under one share, the "<disk>/<share>" shape both PlanRebalance and
// RunRebalance expect (mirroring relocateShare, mover_test.go's own
// newShare).
func rebalanceShare(t *testing.T, name string, branchCount int) Share {
	t.Helper()
	base := t.TempDir()
	s := Share{Name: name}
	for i := 0; i < branchCount; i++ {
		s.Branches = append(s.Branches, filepath.Join(base, "disks", fmt.Sprintf("disk%d", i+1), name))
	}
	return s
}

// fakeUsage scripts Deps.Usage from a fixed map, keyed by branch path —
// PlanRebalance's own target-selection input, without ever calling
// statfs.
func fakeUsage(usage map[string]DiskUsage) func(path string) (DiskUsage, error) {
	return func(path string) (DiskUsage, error) {
		if u, ok := usage[path]; ok {
			return u, nil
		}
		return DiskUsage{}, fmt.Errorf("fakeUsage: no usage scripted for %q", path)
	}
}

func rebalanceWriteSize(t *testing.T, path string, size int) {
	t.Helper()
	mustWrite(t, path, string(make([]byte, size)))
}

// --- PlanRebalance ---

// TestPlanRebalance_EvensOutSkewedShare is doc 09 §6's own "rebalance
// evens out a deliberately skewed pool", at the planning level: a share
// with one disk at 90% used and another at 10% used gets a plan that
// moves enough to bring them within the default skew tolerance.
func TestPlanRebalance_EvensOutSkewedShare(t *testing.T) {
	s := rebalanceShare(t, "movies", 2)
	full, empty := s.Branches[0], s.Branches[1]
	rebalanceWriteSize(t, filepath.Join(full, "big.bin"), 400)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		full:  {TotalBytes: 1000, FreeBytes: 100}, // 90% used
		empty: {TotalBytes: 1000, FreeBytes: 900}, // 10% used
	})

	plan, err := PlanRebalance(context.Background(), []Share{s}, RebalanceConfig{}, deps)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want exactly one move", plan.Moves)
	}
	mv := plan.Moves[0]
	if mv.RelPath != "big.bin" || mv.SourceBranch != full || mv.TargetBranch != empty || mv.Size != 400 {
		t.Fatalf("move = %+v, want big.bin from %q to %q, size 400", mv, full, empty)
	}
}

// TestPlanRebalance_StopsWhenNothingFitsMinFreeSpace proves "or nothing
// more fits minfreespace" (doc 09 §3): a least-full disk with no real
// headroom, once MinFreeSpace is respected, gets no moves at all rather
// than one that would breach it.
func TestPlanRebalance_StopsWhenNothingFitsMinFreeSpace(t *testing.T) {
	s := rebalanceShare(t, "movies", 2)
	s.MinFreeSpace = 950
	full, empty := s.Branches[0], s.Branches[1]
	rebalanceWriteSize(t, filepath.Join(full, "big.bin"), 400)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		full:  {TotalBytes: 1000, FreeBytes: 100},
		empty: {TotalBytes: 1000, FreeBytes: 900}, // MinFreeSpace leaves only -50 headroom
	})

	plan, err := PlanRebalance(context.Background(), []Share{s}, RebalanceConfig{}, deps)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("Moves = %+v, want none — nothing fits MinFreeSpace", plan.Moves)
	}
}

// TestPlanRebalance_WarnsPathPreservingSpread proves doc 09 §3's own UI
// warning: a path-preserving share whose plan would place a file on a
// branch lacking its parent directory gets a RebalanceWarning, without
// PlanRebalance refusing to plan the move itself.
func TestPlanRebalance_WarnsPathPreservingSpread(t *testing.T) {
	s := rebalanceShare(t, "media", 2)
	s.PathPreserving = true
	full, empty := s.Branches[0], s.Branches[1]
	rebalanceWriteSize(t, filepath.Join(full, "tv", "episode.mkv"), 400)
	// empty has no "tv" directory at all.

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		full:  {TotalBytes: 1000, FreeBytes: 100},
		empty: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanRebalance(context.Background(), []Share{s}, RebalanceConfig{}, deps)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want exactly one move", plan.Moves)
	}
	if len(plan.Warnings) != 1 {
		t.Fatalf("Warnings = %+v, want exactly one", plan.Warnings)
	}
	if plan.Warnings[0].Share != "media" {
		t.Errorf("Warnings[0].Share = %q, want %q", plan.Warnings[0].Share, "media")
	}
}

// TestPlanRebalance_SkipsShareWithFewerThanTwoBranches proves a share
// with a single branch — nothing to rebalance between — produces no
// moves and no error.
func TestPlanRebalance_SkipsShareWithFewerThanTwoBranches(t *testing.T) {
	s := rebalanceShare(t, "movies", 1)
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "big.bin"), 400)

	plan, err := PlanRebalance(context.Background(), []Share{s}, RebalanceConfig{}, Deps{Open: NewFakeOpenChecker()})
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) != 0 || len(plan.Warnings) != 0 {
		t.Fatalf("plan = %+v, want an empty plan", plan)
	}
}

// --- RunRebalance ---

// rebalancePlanMoves builds a plan of n moves directly (bypassing
// PlanRebalance) so RunRebalance's own batching and phase logic can be
// tested independently of target selection — each move's own source file
// is written for real, since RunRebalance's copy phase reads it.
func rebalancePlanMoves(t *testing.T, share string, sourceBranch, targetBranch string, n int) RebalancePlan {
	t.Helper()
	var moves []RebalanceMove
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("file%03d.bin", i)
		content := fmt.Sprintf("content-%03d", i)
		mustWrite(t, filepath.Join(sourceBranch, rel), content)
		moves = append(moves, RebalanceMove{
			Share:        share,
			RelPath:      rel,
			SourceBranch: sourceBranch,
			TargetBranch: targetBranch,
			Size:         int64(len(content)),
		})
	}
	return RebalancePlan{Moves: moves}
}

func rebalanceTestDeps(open *FakeOpenChecker) Deps {
	d := testDeps(open)
	d.TrackedFileCount = func(context.Context) (int, error) { return 100000, nil }
	return d
}

// TestRunRebalance_CopiesVerifiesSyncsDeletesSyncsAgain is the two-phase
// happy path (doc 09 §3, Q14, Q15): every file is copied and verified to
// its target branch, a guarded sync protects the copies, the sources are
// deleted, and a second guarded sync protects the deletion.
func TestRunRebalance_CopiesVerifiesSyncsDeletesSyncsAgain(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 3)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var syncCalls int
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != 3 {
		t.Fatalf("Moved() = %+v, want 3 entries", report.Entries)
	}
	if syncCalls != 2 {
		t.Fatalf("sync called %d times, want 2 (Q14: sync, delete, sync again)", syncCalls)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); !os.IsNotExist(err) {
			t.Fatalf("source %s should be gone: err=%v", mv.RelPath, err)
		}
		if _, err := os.Stat(filepath.Join(mv.TargetBranch, mv.RelPath)); err != nil {
			t.Fatalf("target %s should exist: %v", mv.RelPath, err)
		}
	}
}

// TestRunRebalance_ManifestUsesDiskRelativePaths proves each
// ManifestEntry RunRebalance hands to Sync matches DiffFile.RelPath's own
// shape (ManifestEntry's doc comment, Q15) — required for the guard's
// matchManifest to ever recognize the removal as accounted.
func TestRunRebalance_ManifestUsesDiskRelativePaths(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "docs")
	target := filepath.Join(base, "disk2", "docs")
	plan := rebalancePlanMoves(t, "docs", source, target, 1)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var gotManifest []parity.ManifestEntry
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		if gotManifest == nil {
			gotManifest = manifest
		}
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	if _, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil); err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}

	if len(gotManifest) != 1 {
		t.Fatalf("manifest = %+v, want one entry", gotManifest)
	}
	me := gotManifest[0]
	if me.RelPath != filepath.Join("docs", "file000.bin") {
		t.Errorf("RelPath = %q, want share-prefixed disk-relative path", me.RelPath)
	}
	if me.SourceDisk != filepath.Dir(source) {
		t.Errorf("SourceDisk = %q, want %q", me.SourceDisk, filepath.Dir(source))
	}
	if me.TargetDisk != filepath.Dir(target) {
		t.Errorf("TargetDisk = %q, want %q", me.TargetDisk, filepath.Dir(target))
	}
}

// TestRunRebalance_BatchesByCountLimit proves a plan larger than
// Deps.RebalanceBatchLimit runs as more than one complete batch cycle —
// never one copy-all/sync/delete-all/sync-again pass over the whole plan
// — with the percent rule left deliberately non-binding (a very high
// TrackedFileCount) so only the count ceiling shapes the batches.
func TestRunRebalance_BatchesByCountLimit(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 7)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var syncCalls int
	var maxManifestLen int
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.RebalanceBatchLimit = func() int { return 3 }
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		if len(manifest) > maxManifestLen {
			maxManifestLen = len(manifest)
		}
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != 7 {
		t.Fatalf("Moved() = %+v, want 7 entries", report.Entries)
	}
	// 7 files at a batch limit of 3 is three batches (3, 3, 1), each with
	// its own two syncs.
	if syncCalls != 6 {
		t.Fatalf("sync called %d times, want 6 (three batches of at most 3, two syncs each)", syncCalls)
	}
	if maxManifestLen > 3 {
		t.Fatalf("largest single sync manifest = %d entries, want <= 3 (the count ceiling)", maxManifestLen)
	}
}

// TestRunRebalance_BatchesByPercentLimit proves the percent rule alone —
// with the count ceiling left deliberately generous — still bounds batch
// size: a small TrackedFileCount forces batches well under the count
// limit so no batch's own trailing sync could ever exceed
// RebalancePercentLimit.
func TestRunRebalance_BatchesByPercentLimit(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 20)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var syncCalls int
	var maxManifestLen int
	deps := testDeps(NewFakeOpenChecker())
	// A tiny tracked array (30 files) with the guard's own default 10%
	// threshold: percentLimit*T/(100-percentLimit) = 10*30/90 = 3.33, so
	// no batch may contain more than 3 moves even though the count
	// ceiling (the package default, 200) would happily allow all 20 at
	// once.
	deps.TrackedFileCount = func(context.Context) (int, error) { return 30, nil }
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		if len(manifest) > maxManifestLen {
			maxManifestLen = len(manifest)
		}
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != 20 {
		t.Fatalf("Moved() = %+v, want 20 entries", report.Entries)
	}
	if maxManifestLen > 3 {
		t.Fatalf("largest single sync manifest = %d entries, want <= 3 (the percent ceiling for a 30-file tracked array)", maxManifestLen)
	}
	if syncCalls < 2 {
		t.Fatalf("sync called %d times, want multiple batches worth of syncs", syncCalls)
	}
}

// TestRunRebalance_RefusesWithoutTrackedFileCount_NoCopyNoDelete proves
// Deps.TrackedFileCount's own contract: a nil value makes RunRebalance
// refuse outright, before touching anything — no filesystem-walk
// fallback, no numeric default, since guessing this exact number is what
// caused three prior, rejected designs to oversize a batch relative to
// the guard's own real threshold.
func TestRunRebalance_RefusesWithoutTrackedFileCount_NoCopyNoDelete(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 2)

	deps := testDeps(NewFakeOpenChecker())
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		t.Fatal("sync must never be called when Deps.TrackedFileCount is nil")
		return nil
	}

	_, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if !errors.Is(err, ErrRebalanceTrackedCountRequired) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRebalanceTrackedCountRequired)", err)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); err != nil {
			t.Fatalf("source %s must survive: %v", mv.RelPath, err)
		}
		if _, err := os.Stat(filepath.Join(mv.TargetBranch, mv.RelPath)); !os.IsNotExist(err) {
			t.Fatalf("target %s must not exist: err=%v", mv.RelPath, err)
		}
	}
}

func TestRunRebalance_RequiresSync(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 1)

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	_, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err == nil {
		t.Fatal("expected an error when Deps.Sync is nil")
	}
}

// TestRunRebalance_PreDeleteRecheck_CatchesDroppedCountCeiling proves
// RunRebalance re-checks the count rule fresh immediately before a
// batch's first unlink: Deps.RebalanceBatchLimit reports a generous
// ceiling for the batch's own sizing call, then a ceiling too low for
// that same batch by the time the pre-delete re-check calls it again —
// nothing must be deleted.
func TestRunRebalance_PreDeleteRecheck_CatchesDroppedCountCeiling(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 5)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var limitCalls int
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.RebalanceBatchLimit = func() int {
		limitCalls++
		if limitCalls == 1 {
			return 5 // sizing: the whole plan fits in one batch
		}
		return 2 // recheck: the ceiling dropped below this batch's own size
	}
	var syncCalls int
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	_, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if !errors.Is(err, ErrRebalanceUnsafeBatchSize) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRebalanceUnsafeBatchSize)", err)
	}
	if syncCalls != 1 {
		t.Fatalf("sync called %d times, want exactly 1 (the copy-protecting sync only, never the trailing one)", syncCalls)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); err != nil {
			t.Fatalf("source %s must survive an unsafe-batch refusal: %v", mv.RelPath, err)
		}
	}
}

// TestRunRebalance_PreDeleteRecheck_CatchesDroppedPercentCeiling is the
// same guarantee against the percent rule: Deps.TrackedFileCount reports
// a large array for the batch's own sizing call, then a much smaller one
// by the time the pre-delete re-check calls it again — the batch that
// was safe against the first reading is not safe against the second, and
// nothing must be deleted.
func TestRunRebalance_PreDeleteRecheck_CatchesDroppedPercentCeiling(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 50)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var trackedCalls int
	deps := testDeps(NewFakeOpenChecker())
	deps.TrackedFileCount = func(context.Context) (int, error) {
		trackedCalls++
		if trackedCalls == 1 {
			return 1000, nil // sizing: percentLimit*1000/90 = 111, so all 50 fit in one batch
		}
		return 10, nil // recheck: 50/10*100 = 500% > the 10% default
	}
	var syncCalls int
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	_, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if !errors.Is(err, ErrRebalanceUnsafeBatchSize) {
		t.Fatalf("err = %v, want errors.Is(err, ErrRebalanceUnsafeBatchSize)", err)
	}
	if syncCalls != 1 {
		t.Fatalf("sync called %d times, want exactly 1", syncCalls)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); err != nil {
			t.Fatalf("source %s must survive an unsafe-batch refusal: %v", mv.RelPath, err)
		}
	}
}

// TestRunRebalance_GuardBlocked_LeavesSourcesUntouched proves a blocked
// sync stops the batch before anything is deleted — the same guarantee
// TestRelocateToCache_GuardBlocked_LeavesArrayUntouched proves for
// RelocateToCache.
func TestRunRebalance_GuardBlocked_LeavesSourcesUntouched(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 1)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerRemovedCount}})

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = syncFuncFromEngine(engine)

	_, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err == nil {
		t.Fatal("expected an error from a blocked sync")
	}
	if !errors.Is(err, parity.ErrGuardBlocked) {
		t.Fatalf("err = %v, want errors.Is(err, parity.ErrGuardBlocked)", err)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); err != nil {
			t.Fatalf("source %s must survive a blocked sync: %v", mv.RelPath, err)
		}
	}
}

// TestRunRebalance_OpenAtDeleteTime_LeavesSourceInPlace proves the
// per-file re-check-immediately-before-unlink step (doc 09 §2, applied
// here via finishRebalanceDelete): a file that became open again between
// the copy phase and the delete phase is left in place.
func TestRunRebalance_OpenAtDeleteTime_LeavesSourceInPlace(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 1)
	src := filepath.Join(source, plan.Moves[0].RelPath)

	open := NewFakeOpenChecker()
	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	deps := rebalanceTestDeps(open)
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		open.SetOpen(src, true)
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("expected no completed move, got %+v", report.Entries)
	}
	pending := report.byResult(ResultMovedPendingDelete)
	if len(pending) != 1 {
		t.Fatalf("expected one moved_pending_delete entry, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source held open at delete time must survive: %v", err)
	}
}

// TestRunRebalance_ResumeAfterAbort_DeletePhase_CompletesCleanly proves a
// crash mid-delete-phase resumes at exactly DeletedCount, never
// re-deleting anything already gone, and completes the batch (including
// its trailing sync) once resumed.
func TestRunRebalance_ResumeAfterAbort_DeletePhase_CompletesCleanly(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 5)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = syncFuncFromEngine(engine)

	ctx, cancel := context.WithCancel(context.Background())
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RebalanceCheckpoint
			if err := json.Unmarshal(data, &cp); err != nil {
				t.Fatalf("unmarshal checkpoint: %v", err)
			}
			if cp.Phase == RebalancePhaseDeleting && cp.DeletedCount == 2 {
				cancel()
			}
			return nil
		},
	}

	report, err := RunRebalance(ctx, plan, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected the run to report Interrupted")
	}
	if got := len(report.byResult(ResultMoved)); got != 2 {
		t.Fatalf("expected exactly 2 sources deleted before the interrupt, got %d: %+v", got, report.Entries)
	}
	for i, mv := range plan.Moves {
		_, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath))
		if i < 2 {
			if !os.IsNotExist(err) {
				t.Fatalf("source %s should already be deleted: err=%v", mv.RelPath, err)
			}
		} else if err != nil {
			t.Fatalf("source %s must still exist before the resume: %v", mv.RelPath, err)
		}
	}

	report2, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RunRebalance: %v", err)
	}
	if report2.Interrupted {
		t.Fatal("resumed run should complete, not interrupt again")
	}
	if got := len(report2.byResult(ResultMoved)); got != 3 {
		t.Fatalf("expected the remaining 3 sources deleted on resume, got %d: %+v", got, report2.Entries)
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); !os.IsNotExist(err) {
			t.Fatalf("source %s should be gone after the resumed run: err=%v", mv.RelPath, err)
		}
		if _, err := os.Stat(filepath.Join(mv.TargetBranch, mv.RelPath)); err != nil {
			t.Fatalf("target %s should exist after the resumed run: %v", mv.RelPath, err)
		}
	}
}

// cancelAfterNOpen cancels its own context after its Nth IsOpen call —
// used to interrupt RunRebalance's copy phase partway through a batch,
// after some files have already been copied and verified but before the
// phase itself finishes.
type cancelAfterNOpen struct {
	*FakeOpenChecker
	n, calls int
	cancel   func()
}

func (c *cancelAfterNOpen) IsOpen(ctx context.Context, path string) (bool, error) {
	c.calls++
	open, err := c.FakeOpenChecker.IsOpen(ctx, path)
	if c.calls == c.n {
		c.cancel()
	}
	return open, err
}

// TestRunRebalance_InterruptedMidCopy_ResumesWithoutDuplication proves an
// interrupt during the copy phase — before it finishes, so no per-file
// checkpoint inside it is relied on — leaves the batch's already-copied
// file exactly as it is, and a resumed run's own copy phase recognizes
// it (isSamePendingCopy) rather than copying it again, going on to
// complete the batch normally.
func TestRunRebalance_InterruptedMidCopy_ResumesWithoutDuplication(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "disk1", "movies")
	target := filepath.Join(base, "disk2", "movies")
	plan := rebalancePlanMoves(t, "movies", source, target, 3)

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)

	var syncCalls int
	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Sync = func(ctx context.Context, manifest []parity.ManifestEntry) error {
		syncCalls++
		return syncFuncFromEngine(engine)(ctx, manifest)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelling := &cancelAfterNOpen{FakeOpenChecker: NewFakeOpenChecker(), n: 1, cancel: cancel}
	deps.Open = cancelling

	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			return nil
		},
	}

	report, err := RunRebalance(ctx, plan, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if !report.Interrupted {
		t.Fatal("expected the run to report Interrupted")
	}
	if syncCalls != 0 {
		t.Fatal("sync must never run before the copy phase finishes")
	}
	firstTarget := filepath.Join(plan.Moves[0].TargetBranch, plan.Moves[0].RelPath)
	firstInfoBefore, err := os.Stat(firstTarget)
	if err != nil {
		t.Fatalf("first file must already be copied and verified before the interrupt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plan.Moves[1].TargetBranch, plan.Moves[1].RelPath)); !os.IsNotExist(err) {
		t.Fatalf("second file must not have been reached yet: err=%v", err)
	}

	deps.Open = NewFakeOpenChecker()
	report2, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RunRebalance: %v", err)
	}
	if len(report2.Moved()) != 3 {
		t.Fatalf("expected all three moves to complete, got %+v", report2.Entries)
	}
	firstInfoAfter, err := os.Stat(firstTarget)
	if err != nil {
		t.Fatalf("first file must still exist after the resumed run: %v", err)
	}
	if !firstInfoAfter.ModTime().Equal(firstInfoBefore.ModTime()) {
		t.Fatalf("first file's mtime changed (%v -> %v) — it must have been recognized as already copied, not copied again", firstInfoBefore.ModTime(), firstInfoAfter.ModTime())
	}
	for _, mv := range plan.Moves {
		if _, err := os.Stat(filepath.Join(mv.SourceBranch, mv.RelPath)); !os.IsNotExist(err) {
			t.Fatalf("source %s should be gone: err=%v", mv.RelPath, err)
		}
	}
}
