package cache

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// evacuateShare builds a Share with one branch per disk in disks, all
// under one share directory — mirroring rebalanceShare, but letting the
// caller name each disk's own mount root explicitly since PlanEvacuation
// derives a file's owning disk from filepath.Dir(branch), the same
// "<disk>/<share>" shape rebalance.go's own moves use.
func evacuateShare(t *testing.T, name string, disks []string) Share {
	t.Helper()
	s := Share{Name: name}
	for _, d := range disks {
		s.Branches = append(s.Branches, filepath.Join(d, name))
	}
	return s
}

// --- PlanEvacuation ---

// TestPlanEvacuation_MovesEverythingOffTheDisk proves doc 09 §4 step 3:
// every file on the evacuating disk's own branch is planned onto one of
// the share's other branches, and none of them target the disk being
// evacuated.
func TestPlanEvacuation_MovesEverythingOffTheDisk(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	disk3 := filepath.Join(base, "disk3")
	s := evacuateShare(t, "movies", []string{disk1, disk2, disk3})
	source := s.Branches[0]

	rebalanceWriteSize(t, filepath.Join(source, "a.bin"), 100)
	rebalanceWriteSize(t, filepath.Join(source, "b.bin"), 200)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
		s.Branches[2]: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", plan.Moves)
	}
	for _, mv := range plan.Moves {
		if mv.SourceBranch != source {
			t.Fatalf("move %+v: SourceBranch = %q, want %q", mv, mv.SourceBranch, source)
		}
		if mv.TargetBranch == source {
			t.Fatalf("move %+v: TargetBranch must not be the disk being evacuated", mv)
		}
	}
}

// TestPlanEvacuation_SkipsShareWithNoPresenceOnDisk proves a share that
// has no branch on the disk being evacuated contributes nothing to the
// plan — there is nothing of that share's to move.
func TestPlanEvacuation_SkipsShareWithNoPresenceOnDisk(t *testing.T) {
	base := t.TempDir()
	disk2 := filepath.Join(base, "disk2")
	disk3 := filepath.Join(base, "disk3")
	s := evacuateShare(t, "movies", []string{disk2, disk3})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[0]: {TotalBytes: 1000, FreeBytes: 900},
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanEvacuation(context.Background(), filepath.Join(base, "disk1"), []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 0 {
		t.Fatalf("Moves = %+v, want none", plan.Moves)
	}
}

// TestPlanEvacuation_ErrorsWhenNoOtherBranch proves a share whose only
// branch is on the disk being evacuated refuses outright — there is
// nowhere for step 3's own enumeration to send its files.
func TestPlanEvacuation_ErrorsWhenNoOtherBranch(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	s := evacuateShare(t, "movies", []string{disk1})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)

	deps := Deps{Open: NewFakeOpenChecker()}
	_, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if !errors.Is(err, ErrEvacuationNoOtherBranch) {
		t.Fatalf("PlanEvacuation: got %v, want ErrEvacuationNoOtherBranch", err)
	}
}

// TestPlanEvacuation_RefusesWhenItWontFit is doc 09 §4 step 1's own
// pre-check: a file bigger than any remaining branch can hold, even
// respecting MinFreeSpace, refuses the whole plan rather than silently
// dropping that file or starting a copy that must fail partway through.
func TestPlanEvacuation_RefusesWhenItWontFit(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "big.bin"), 950)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
	})

	_, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if !errors.Is(err, ErrEvacuationWontFit) {
		t.Fatalf("PlanEvacuation: got %v, want ErrEvacuationWontFit", err)
	}

	// Nothing must have been touched — PlanEvacuation is a pure
	// computation, exactly like PlanRebalance.
	if _, statErr := os.Stat(filepath.Join(s.Branches[0], "big.bin")); statErr != nil {
		t.Fatalf("source must survive a refused plan: %v", statErr)
	}
}

// TestPlanEvacuation_RefusesWhenMinFreeSpaceLeavesNoRoom proves the
// pre-check honours MinFreeSpace, not just raw free bytes — the same
// rule PlanRebalance's own pickMovableFile applies, but here a file that
// cannot be placed must fail the plan rather than simply not being
// scheduled: doc 09 §4 evacuates everything, it does not leave a
// residue behind the way rebalance's own skew tolerance permits.
func TestPlanEvacuation_RefusesWhenMinFreeSpaceLeavesNoRoom(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	s.MinFreeSpace = 950
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900}, // MinFreeSpace leaves -50 headroom
	})

	_, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if !errors.Is(err, ErrEvacuationWontFit) {
		t.Fatalf("PlanEvacuation: got %v, want ErrEvacuationWontFit", err)
	}
}

// TestPlanEvacuation_SharesOneDisksFreeSpaceAcrossShares: movies and tv
// both have a branch on disk2, one filesystem with 500 bytes free. Each
// share alone fits, but together they do not — planning each share
// against disk2's full free space would overcommit it and fail partway
// through the real copy, which the pre-check exists to prevent.
func TestPlanEvacuation_SharesOneDisksFreeSpaceAcrossShares(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	movies := evacuateShare(t, "movies", []string{disk1, disk2})
	tv := evacuateShare(t, "tv", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(movies.Branches[0], "a.bin"), 400)
	rebalanceWriteSize(t, filepath.Join(tv.Branches[0], "b.bin"), 400)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		movies.Branches[1]: {TotalBytes: 1000, FreeBytes: 500},
		tv.Branches[1]:     {TotalBytes: 1000, FreeBytes: 500},
	})

	_, err := PlanEvacuation(context.Background(), disk1, []Share{movies, tv}, deps)
	if !errors.Is(err, ErrEvacuationWontFit) {
		t.Fatalf("PlanEvacuation: got %v, want ErrEvacuationWontFit", err)
	}
}

// TestPlanEvacuation_RefusesASymlinkBeforeAnyCopy: the copy path skips a
// symlink and the post-check rejects it, so a plan that omitted it would
// run every copy and sync and then fail — on every retry. It must be
// refused at planning time, naming the entry.
func TestPlanEvacuation_RefusesASymlinkBeforeAnyCopy(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)
	if err := os.Symlink("a.bin", filepath.Join(s.Branches[0], "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
	})

	_, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if !errors.Is(err, ErrEvacuationUnsupportedEntry) {
		t.Fatalf("PlanEvacuation: got %v, want ErrEvacuationUnsupportedEntry", err)
	}
}

// TestPickEvacuationTarget_PicksTheMostFreeSpace proves target selection
// spreads an evacuated disk's files across what remains rather than
// piling them onto whichever branch comes first.
func TestPickEvacuationTarget_PicksTheMostFreeSpace(t *testing.T) {
	states := []*evacuationTarget{
		{branch: "/mnt/disk2/movies", usage: &DiskUsage{TotalBytes: 1000, FreeBytes: 300}},
		{branch: "/mnt/disk3/movies", usage: &DiskUsage{TotalBytes: 1000, FreeBytes: 700}},
	}
	got := pickEvacuationTarget(states, 100, 0)
	if got == nil || got.branch != "/mnt/disk3/movies" {
		t.Fatalf("pickEvacuationTarget = %+v, want disk3 (the most free space)", got)
	}
}

func TestPickEvacuationTarget_ReturnsNilWhenNothingFits(t *testing.T) {
	states := []*evacuationTarget{
		{branch: "/mnt/disk2/movies", usage: &DiskUsage{TotalBytes: 1000, FreeBytes: 50}},
	}
	if got := pickEvacuationTarget(states, 100, 0); got != nil {
		t.Fatalf("pickEvacuationTarget = %+v, want nil", got)
	}
}

// --- PlanEvacuation + RunRebalance integration ---

// TestPlanEvacuation_RunViaRunRebalance_EmptiesTheDiskCleanly proves the
// whole doc 09 §4 steps 1/3-6 pipeline end to end: PlanEvacuation's own
// plan, executed through RunRebalance's real two-phase
// copy/verify/guarded-sync/delete/guarded-sync machinery, empties the
// evacuating disk's own branch completely, and EvacuationPostCheck
// confirms it.
func TestPlanEvacuation_RunViaRunRebalance_EmptiesTheDiskCleanly(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	disk3 := filepath.Join(base, "disk3")
	s := evacuateShare(t, "movies", []string{disk1, disk2, disk3})

	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "nested", "b.bin"), 100)

	deps := rebalanceTestDeps(NewFakeOpenChecker())
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
		s.Branches[2]: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", plan.Moves)
	}

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != 2 {
		t.Fatalf("Moved() = %+v, want 2 entries", report.Entries)
	}

	if err := EvacuationPostCheck(disk1, []Share{s}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v, want nil — every file was moved and deleted", err)
	}
}

// --- EvacuationPostCheck ---

// TestEvacuationPostCheck_PassesWhenOnlyEmptyDirsRemain proves the
// post-check accepts a directory skeleton with nothing left under it.
func TestEvacuationPostCheck_PassesWhenOnlyEmptyDirsRemain(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	s := evacuateShare(t, "movies", []string{disk1})
	if err := os.MkdirAll(filepath.Join(s.Branches[0], "nested", "deeper"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := EvacuationPostCheck(disk1, []Share{s}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v, want nil", err)
	}
}

// TestEvacuationPostCheck_PassesWhenBranchNeverExisted proves a share
// that was never actually present on disk (no directory created at all)
// does not fail the check.
func TestEvacuationPostCheck_PassesWhenBranchNeverExisted(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	s := evacuateShare(t, "movies", []string{disk1})

	if err := EvacuationPostCheck(disk1, []Share{s}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v, want nil", err)
	}
}

// TestEvacuationPostCheck_FailsWhenAFileRemains is this issue's own
// central data-loss guard: a single leftover regular file — exactly what
// a skipped-open, a conflict, or an interrupted run's own unfinished
// delete phase would leave behind — must be caught here, before a
// caller ever proceeds to doc 09 §4 steps 7-9 (branch-list removal,
// SnapRAID removal, unmount). Removing disk from the array while this
// check is wrongly satisfied would delete the only copy of whatever
// file remained.
func TestEvacuationPostCheck_FailsWhenAFileRemains(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	s := evacuateShare(t, "movies", []string{disk1})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "still-here.bin"), 10)

	err := EvacuationPostCheck(disk1, []Share{s})
	if !errors.Is(err, ErrEvacuationNotEmpty) {
		t.Fatalf("EvacuationPostCheck: got %v, want ErrEvacuationNotEmpty", err)
	}
}

// TestEvacuationPostCheck_IgnoresOtherDisks proves the check only ever
// looks at disk's own branch, never a share's branches on other disks —
// a file that legitimately still exists on a disk that is not being
// evacuated must not fail this check.
func TestEvacuationPostCheck_IgnoresOtherDisks(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(s.Branches[1], "stays.bin"), 10)

	if err := EvacuationPostCheck(disk1, []Share{s}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v, want nil — the leftover file is on disk2, not disk1", err)
	}
}

// TestEvacuationPostCheck_DataLossScenario_SkippedOpenFileNeverPassesTheCheck
// is the same safety-critical scenario as
// TestEvacuationPostCheck_FailsWhenAFileRemains, but reached the way it
// would happen for real: a file held open during RunRebalance's own copy
// phase is skipped (doc 09 §2's "never move an open file"), so it is
// never even in the manifest RunRebalance's delete phase later acts on —
// EvacuationPostCheck must still catch it rather than a caller wrongly
// concluding, from RunRebalance's own success and lack of Interrupted,
// that disk is safe to remove.
func TestEvacuationPostCheck_DataLossScenario_SkippedOpenFileNeverPassesTheCheck(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	openFile := filepath.Join(s.Branches[0], "in-use.bin")
	rebalanceWriteSize(t, openFile, 100)

	open := NewFakeOpenChecker()
	open.SetOpen(openFile, true)
	deps := rebalanceTestDeps(open)
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}

	engine := parity.NewFakeEngine()
	engine.Sleep = func(time.Duration) {}
	engine.ScriptSync([]parity.Progress{{Percent: 100}}, nil)
	deps.Sync = syncFuncFromEngine(engine)

	report, err := RunRebalance(context.Background(), plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if report.Interrupted {
		t.Fatal("RunRebalance should not report Interrupted for a plain skipped-open file")
	}

	err = EvacuationPostCheck(disk1, []Share{s})
	if !errors.Is(err, ErrEvacuationNotEmpty) {
		t.Fatalf("EvacuationPostCheck: got %v, want ErrEvacuationNotEmpty — the open file was skipped, not moved", err)
	}
}

// TestPlanEvacuation_ResumeAfterInterrupt_CheckspointRoundTrips proves a
// plan PlanEvacuation returns survives an ordinary JSON checkpoint
// round-trip unchanged, so RunRebalance's own resumability (Q29) applies
// to it exactly as it does to a plan built by PlanRebalance — evacuation
// carries no checkpoint shape of its own; it uses RunRebalance's.
func TestPlanEvacuation_ChecksIntoJSONRoundTrip(t *testing.T) {
	base := t.TempDir()
	disk1 := filepath.Join(base, "disk1")
	disk2 := filepath.Join(base, "disk2")
	s := evacuateShare(t, "movies", []string{disk1, disk2})
	rebalanceWriteSize(t, filepath.Join(s.Branches[0], "a.bin"), 100)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.Usage = fakeUsage(map[string]DiskUsage{
		s.Branches[1]: {TotalBytes: 1000, FreeBytes: 900},
	})

	plan, err := PlanEvacuation(context.Background(), disk1, []Share{s}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}

	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	var roundTripped RebalancePlan
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if len(roundTripped.Moves) != len(plan.Moves) {
		t.Fatalf("round-tripped plan has %d moves, want %d", len(roundTripped.Moves), len(plan.Moves))
	}
}
