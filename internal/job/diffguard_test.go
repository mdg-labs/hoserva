package job

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func TestEngineDiffGuard_BlockedDiffDoesNotNeedSync(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{
		Removed: 600,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 400},
		},
	})
	g := EngineDiffGuard{Engine: eng, Guard: parity.Guard{}}
	blocked, err := g.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !blocked {
		t.Fatal("Evaluate blocked = false, want true for 600 removals")
	}
}

func TestEngineDiffGuard_SmallDiffIsNotBlocked(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{
		Removed: 1,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
		},
	})
	g := EngineDiffGuard{Engine: eng, Guard: parity.Guard{}}
	blocked, err := g.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if blocked {
		t.Fatal("Evaluate blocked = true, want false for one removal")
	}
}

// twoPhaseDiff is the Q14 two-phase relocation's trailing-sync diff shape
// this issue is about: three files removed from their source disk, with no
// matching addition in the same diff — the addition was already committed
// by an earlier sync's own diff, not this one (#248).
func twoPhaseDiff() parity.DiffReport {
	return parity.DiffReport{
		Removed: 3,
		RemovedFiles: []parity.DiffFile{
			{Disk: "/mnt/disk1", RelPath: "movies/f1.bin"},
			{Disk: "/mnt/disk1", RelPath: "movies/f2.bin"},
			{Disk: "/mnt/disk1", RelPath: "movies/f3.bin"},
		},
	}
}

func twoPhaseManifest() []parity.ManifestEntry {
	return []parity.ManifestEntry{
		{RelPath: "movies/f1.bin", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		{RelPath: "movies/f2.bin", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
		{RelPath: "movies/f3.bin", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"},
	}
}

// TestEngineDiffGuard_Evaluate_ManifestAccountedTwoPhaseRelocationDoesNotBlock
// is this issue's central reproduction: without consulting the engine's
// persisted relocation manifest, a Q14 two-phase relocation's trailing-sync
// removal trips the guard here even though the real SnapraidEngine.Sync
// (#248) and the diff-preview endpoint (#252) would both confirm it via
// `snapraid list` and let the sync proceed. RemovedFilesMax is set low
// enough that all 3 removals would block without the exemption.
func TestEngineDiffGuard_Evaluate_ManifestAccountedTwoPhaseRelocationDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())
	eng.ScriptRelocationManifest(twoPhaseManifest(), nil, nil)
	eng.SetList(parity.ListReport{
		DataMounts: map[string]string{"d2": "/mnt/disk2"},
		Files: []parity.ListFile{
			{Disk: "d2", RelPath: "movies/f1.bin"},
			{Disk: "d2", RelPath: "movies/f2.bin"},
			{Disk: "d2", RelPath: "movies/f3.bin"},
		},
	})

	guard := EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}}
	blocked, err := guard.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if blocked {
		t.Fatal("Evaluate = blocked, want unblocked — a real Sync call would confirm all 3 removals already landed on their target disk")
	}
}

// TestEngineDiffGuard_Evaluate_UnaccountedRemovalStillBlocks confirms the
// pre-check still blocks a genuinely unaccounted removal — no relocation
// manifest at all, exactly today's behaviour before this issue's fix.
func TestEngineDiffGuard_Evaluate_UnaccountedRemovalStillBlocks(t *testing.T) {
	ctx := context.Background()
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())

	guard := EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}}
	blocked, err := guard.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !blocked {
		t.Fatal("Evaluate = unblocked, want blocked — no manifest accounts for these removals")
	}
}

// TestEngineDiffGuard_Evaluate_StaleManifestStillBlocks is the other half
// of #252's own worked example, one layer up: a manifest entry naming a
// target disk `snapraid list` never actually shows the file on must not be
// trusted — this must not create an under-blocking path (#253's own
// constraint).
func TestEngineDiffGuard_Evaluate_StaleManifestStillBlocks(t *testing.T) {
	ctx := context.Background()
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())
	eng.ScriptRelocationManifest(twoPhaseManifest(), nil, nil)
	eng.SetList(parity.ListReport{DataMounts: map[string]string{"d2": "/mnt/disk2"}})

	guard := EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}}
	blocked, err := guard.Evaluate(ctx)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !blocked {
		t.Fatal("Evaluate = unblocked, want blocked — none of the 3 removals were ever confirmed on their target disk")
	}
}

// TestEngineDiffGuard_Evaluate_RelocationManifestLoadErrorFailsClosed is
// this issue's own fail-closed scenario, mirroring RunSync's
// TestRunSync_RelocationManifestLoadErrorFailsClosed: a transient failure
// reading the persisted manifest must stop the pre-check with an error, not
// silently fall back to an unaccounted (or worse, wrongly-accounted)
// evaluation.
func TestEngineDiffGuard_Evaluate_RelocationManifestLoadErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())
	eng.ScriptRelocationManifest(nil, nil, errors.New("relocation manifest store: boom"))

	guard := EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}}
	_, err := guard.Evaluate(ctx)
	if err == nil {
		t.Fatal("Evaluate = nil error, want the relocation manifest load failure to fail the pre-check closed")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want it to surface the relocation manifest load failure", err)
	}
}

// TestMaintenanceChain_ManifestAccountedTwoPhaseRelocationReachesSync is
// this issue's acceptance test at the chain level: the scheduled
// maintenance chain, wired with the real EngineDiffGuard (not a fake DiffGuard),
// must actually submit and run StepSync for a manifest-accounted two-phase
// relocation's trailing-sync removal instead of stopping at diff_guard.
func TestMaintenanceChain_ManifestAccountedTwoPhaseRelocationReachesSync(t *testing.T) {
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())
	eng.ScriptRelocationManifest(twoPhaseManifest(), nil, nil)
	eng.SetList(parity.ListReport{
		DataMounts: map[string]string{"d2": "/mnt/disk2"},
		Files: []parity.ListFile{
			{Disk: "d2", RelPath: "movies/f1.bin"},
			{Disk: "d2", RelPath: "movies/f2.bin"},
			{Disk: "d2", RelPath: "movies/f3.bin"},
		},
	})

	rec := &stepRecorder{}
	registerRecording(s, TypeSync, rec, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}},
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Blocked {
		t.Fatal("result.Blocked = true, want false — the manifest already accounts for this removal")
	}
	got := rec.get()
	if len(got) != 1 || got[0] != "sync" {
		t.Fatalf("recorded steps = %v, want [sync] — the chain must submit StepSync for an accounted removal", got)
	}
}

// TestMaintenanceChain_UnaccountedRemovalStillStopsChain is this issue's
// negative counterpart: an unaccounted removal must still stop the chain
// before StepSync, exactly as before this fix — this must not become an
// under-blocking path.
func TestMaintenanceChain_UnaccountedRemovalStillStopsChain(t *testing.T) {
	s := newTestScheduler(t)
	eng := newRecordingEngine()
	eng.SetDiff(twoPhaseDiff())

	rec := &stepRecorder{}
	registerRecording(s, TypeSync, rec, "sync")

	chain := &MaintenanceChain{
		Scheduler: s,
		Guard:     EngineDiffGuard{Engine: eng, Guard: parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 2}}},
		Weekly:    false,
	}

	result, err := chain.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Blocked {
		t.Fatal("result.Blocked = false, want true — no manifest accounts for these removals")
	}
	if got := rec.get(); len(got) != 0 {
		t.Fatalf("recorded steps = %v, want none — sync must not run past a blocked diff_guard", got)
	}
}
