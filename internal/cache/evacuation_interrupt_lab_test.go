//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host — the same build/run pattern evacuate_lab_test.go and
// rebalance_lab_test.go use in this package, and it reuses their own lab
// helpers directly (same package, same build tag) rather than duplicating
// them. It carries this issue's own safety-critical lab acceptance
// criterion (#274): an evacuation interrupted at the copy/sync checkpoint
// boundary — a daemon restart or maintenance-mode stop landing right
// after the copy phase makes its manifest durable, before RunRebalance
// ever calls Sync — must never delete a source before the sync that
// covers its copy, mirroring rebalance_lab_test.go's own
// TestLabRebalance_InterruptedBeforeSync_OtherDiskReconstructsFully for
// evacuation specifically (doc 09 §4 steps 3-5), including the
// doc 09 §4 step 6 post-check correctly refusing the disk as not yet
// empty until the interrupted run actually resumes and finishes.
//
// It uses its own dedicated loop disks, never disk1-3 of `make lab-up`'s
// own standing array — the same reason evacuate_lab_test.go does: this
// test's own tracked-file-count assertions have to be exact against a
// content file only this test has ever synced.

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func TestLabEvacuation_InterruptedBeforeSync_SourceSurvives(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	d1 := filepath.Join(lab, "mnt", "disk-p56-c1")
	d2 := filepath.Join(lab, "mnt", "disk-p56-c2")
	rebalanceCreateAndMountLoopDisk(t, lab, "p56-c1", d1)
	rebalanceCreateAndMountLoopDisk(t, lab, "p56-c2", d2)

	mounts := []string{d1, d2}
	engine := rebalanceLabEngineWithMounts(t, lab, "p56-c-test", "p56-c", mounts, rebalanceGenerousGuard())

	shareName := "evacshare"
	branches := make([]string, len(mounts))
	for i, m := range mounts {
		branches[i] = filepath.Join(m, shareName)
	}

	rel := "movie.bin"
	content := strings.Repeat("m", 300_000)
	rebalanceLabWriteFile(t, filepath.Join(branches[0], rel), content)

	rebalanceLabSyncOnce(t, ctx, engine)

	share := Share{Name: shareName, Branches: branches}
	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	// This test is about the interrupt/resume guarantee, not batch sizing
	// against the guard — match the engine's own generous Guard.Config
	// above so this tiny array's own handful of tracked files can never
	// make rebalanceBatchSize reject this plan's single move.
	deps.RebalancePercentLimit = func() float64 { return 99 }

	plan, err := PlanEvacuation(ctx, d1, []Share{share}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want 1", plan.Moves)
	}

	syncCalled := false
	deps.Sync = func(context.Context, []parity.ManifestEntry) error {
		syncCalled = true
		return fmt.Errorf("sync must never be called before this test's own interrupt point")
	}

	evacCtx, cancel := context.WithCancel(ctx)
	var lastCheckpoint []byte
	hooks := RunHooks{
		SaveCheckpoint: func(data []byte) error {
			lastCheckpoint = append([]byte(nil), data...)
			var cp RebalanceCheckpoint
			if uerr := json.Unmarshal(data, &cp); uerr != nil {
				t.Fatalf("unmarshal checkpoint: %v", uerr)
			}
			if cp.Phase == RebalancePhaseSyncing {
				cancel()
			}
			return nil
		},
	}

	report, err := RunRebalance(evacCtx, plan, Config{}, deps, hooks, nil)
	if err != nil {
		t.Fatalf("RunRebalance (evacuation, interrupted): %v", err)
	}
	if !report.Interrupted {
		t.Fatalf("expected the run to report Interrupted, got %+v", report)
	}
	if syncCalled {
		t.Fatal("sync must never have been called before the interrupt")
	}
	if len(lastCheckpoint) == 0 {
		t.Fatal("expected a saved checkpoint at the copy/sync boundary")
	}

	if _, err := os.Stat(filepath.Join(branches[0], rel)); err != nil {
		t.Fatalf("source must survive an interrupt before the sync covering its copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(branches[1], rel)); err != nil {
		t.Fatalf("verified target copy must exist after the interrupt: %v", err)
	}

	stillClean, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the interrupted evacuation: %v", err)
	}
	if stillClean.Added != 0 || stillClean.Removed != 0 || stillClean.Updated != 0 {
		t.Fatalf("Diff after the interrupted evacuation reported changes: %+v, want none — nothing was ever synced", stillClean)
	}

	// doc 09 §4 step 6's own post-check must still refuse disk1 as safe
	// to remove: the interrupted run never reached its own delete phase,
	// so disk1 still holds its (already-copied-elsewhere) source file.
	if err := EvacuationPostCheck(d1, []Share{share}); !errors.Is(err, ErrEvacuationNotEmpty) {
		t.Fatalf("EvacuationPostCheck after the interrupted evacuation = %v, want ErrEvacuationNotEmpty — the disk must not be reported safe to remove yet", err)
	}

	// Resume to completion once the interrupt is lifted — the trailing
	// sync that actually empties disk1 needs the same zero-files
	// exemption (Q15) evacuate_lab_test.go's own happy-path test uses.
	removingDisks := map[string]bool{filepath.Clean(d1): true}
	deps.Sync = evacuationLabSyncFunc(engine, removingDisks)
	resumed, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, lastCheckpoint)
	if err != nil {
		t.Fatalf("resumed RunRebalance: %v", err)
	}
	if len(resumed.Moved()) != 1 {
		t.Fatalf("expected the resumed evacuation to complete, got %+v", resumed.Entries)
	}
	if _, err := os.Stat(filepath.Join(branches[0], rel)); !os.IsNotExist(err) {
		t.Fatalf("source should be gone after the resumed evacuation: err=%v", err)
	}

	if err := EvacuationPostCheck(d1, []Share{share}); err != nil {
		t.Fatalf("EvacuationPostCheck after the resumed evacuation completes: %v, want nil", err)
	}
}
