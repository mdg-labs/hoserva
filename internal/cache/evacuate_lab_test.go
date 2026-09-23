//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern rebalance_lab_test.go and relocate_lab_test.go use in
// this package — and it reuses their own lab helpers directly (same
// package, same build tag) rather than duplicating them. It carries this
// issue's own central lab acceptance criterion (doc 09 §6's evacuation
// entries, doc 09 §4): a real evacuation — real files, a real
// SnapraidEngine, Q14's two-phase copy/verify/guarded-sync/delete/
// guarded-sync order — empties a disk completely, doc 09 §4 step 6's own
// post-check confirms it, and the guard's zero-files exemption (Q15)
// applies to the disk being evacuated and only to it: a real, unrelated
// disk dropping to zero files in the very same test still trips the
// same, unmodified rule.
//
// internal/pool's own remove_disk_lab_test.go covers doc 09 §4 steps 2
// and 7 (the "removing" state's NC branches, and RemoveDataDisk +
// Remount without the disk) — this file does not repeat that; together
// the two satisfy the issue's "full evacuation and clean remount without
// the disk" acceptance criterion within each package's own scope.
//
// It uses its own dedicated loop disks, never disk1-3 of `make lab-up`'s
// own standing array — the same reason rebalance_lab_test.go does:
// this test's own tracked-file-count and zero-files assertions have to
// be exact against a content file only this test has ever synced.

package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// evacuationLabSyncFunc is rebalanceLabSyncFunc, generalized to also
// carry removingDisks into every call's SyncOpts.RemovingDisks (doc 09
// §4 step 2, Q15) — an ordinary rebalance never needs this, since it
// never empties a disk on purpose, but an evacuation's own trailing sync
// (the one that actually empties the disk being evacuated) does.
func evacuationLabSyncFunc(engine *parity.SnapraidEngine, removingDisks map[string]bool) SyncFunc {
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		ch, err := engine.Sync(ctx, parity.SyncOpts{Manifest: manifest, RemovingDisks: removingDisks})
		if err != nil {
			return err
		}
		var last parity.Progress
		for p := range ch {
			last = p
		}
		return last.Err
	}
}

// TestLabEvacuation_FullEvacuationAndCleanRemount is this issue's own
// lab acceptance criterion. PlanEvacuation and RunRebalance move every
// file off disk1 onto disk2/disk3 for real, protected by a real,
// threshold-guarded SnapRAID sync; EvacuationPostCheck confirms disk1
// holds nothing but empty directories afterward; and disk1 dropping to
// zero tracked files does not trip the guard's zero-files rule — while a
// second, genuinely unrelated disk dropping to zero files in the very
// same test still does, proving the exemption is scoped to the disk
// actually being evacuated, not the rule going quiet altogether.
func TestLabEvacuation_FullEvacuationAndCleanRemount(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	d1 := filepath.Join(lab, "mnt", "disk-p56-a1")
	d2 := filepath.Join(lab, "mnt", "disk-p56-a2")
	d3 := filepath.Join(lab, "mnt", "disk-p56-a3")
	rebalanceCreateAndMountLoopDisk(t, lab, "p56-a1", d1)
	rebalanceCreateAndMountLoopDisk(t, lab, "p56-a2", d2)
	rebalanceCreateAndMountLoopDisk(t, lab, "p56-a3", d3)

	mounts := []string{d1, d2, d3}
	engine := rebalanceLabEngineWithMounts(t, lab, "p56-a-test", "p56-a", mounts, rebalanceGenerousGuard())

	shareName := "evacshare"
	branches := make([]string, len(mounts))
	for i, m := range mounts {
		branches[i] = filepath.Join(m, shareName)
	}

	rebalanceLabWriteFile(t, filepath.Join(branches[0], "a.bin"), "alpha content")
	rebalanceLabWriteFile(t, filepath.Join(branches[0], "nested", "b.bin"), "beta content, a bit longer than alpha")
	rebalanceLabWriteFile(t, filepath.Join(branches[1], "stays-on-2.bin"), "already on disk2")
	rebalanceLabWriteFile(t, filepath.Join(branches[2], "stays-on-3.bin"), "already on disk3")

	rebalanceLabSyncOnce(t, ctx, engine)

	before, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before evacuation: %v", err)
	}
	if before.Added != 0 || before.Removed != 0 || before.Updated != 0 {
		t.Fatalf("Diff before evacuation reported pending changes: %+v, want none", before)
	}
	d1Before, ok := before.PerDisk[filepath.Clean(d1)]
	if !ok || d1Before.FilesBefore != 2 {
		t.Fatalf("disk1's own tracked count before evacuation = %+v, want FilesBefore=2 — this test's own zero-files exemption claim needs it to have actually held files", d1Before)
	}

	share := Share{Name: shareName, Branches: branches}

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	// This test is about evacuation mechanics and the guard's own
	// zero-files exemption, not batch sizing against the guard's other
	// two rules — match rebalanceGenerousGuard's own thresholds above so
	// this tiny array's own handful of tracked files can never make
	// rebalanceBatchSize reject this plan.
	deps.RebalancePercentLimit = func() float64 { return 99 }

	plan, err := PlanEvacuation(ctx, d1, []Share{share}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", plan.Moves)
	}
	for _, mv := range plan.Moves {
		if mv.SourceBranch != branches[0] {
			t.Fatalf("move %+v: SourceBranch = %q, want %q", mv, mv.SourceBranch, branches[0])
		}
	}

	removingDisks := map[string]bool{filepath.Clean(d1): true}
	deps.Sync = evacuationLabSyncFunc(engine, removingDisks)

	report, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance (evacuation): %v", err)
	}
	if len(report.Moved()) != 2 {
		t.Fatalf("Moved() = %+v, want 2 entries", report.Entries)
	}

	// doc 09 §4 step 6's own post-check: nothing but empty directories
	// left under disk1's own branch.
	if err := EvacuationPostCheck(d1, []Share{share}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v, want nil", err)
	}

	after, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after evacuation: %v", err)
	}
	if after.Added != 0 || after.Removed != 0 || after.Updated != 0 {
		t.Fatalf("Diff after evacuation reported pending changes: %+v, want none — the trailing sync must have covered everything", after)
	}
	d1Diff, ok := after.PerDisk[filepath.Clean(d1)]
	if !ok {
		t.Fatalf("Diff after evacuation has no entry for %s", d1)
	}
	if d1Diff.FilesAfter != 0 {
		t.Fatalf("disk1's own FilesAfter = %d, want 0 — every file must have moved off it", d1Diff.FilesAfter)
	}

	// The exemption is scoped to disk1, not the rule going quiet: a
	// genuinely unrelated disk (disk3) dropping to zero tracked files in
	// this same test, in the same window, still trips the same,
	// unmodified guard.
	entries, err := os.ReadDir(branches[2])
	if err != nil {
		t.Fatalf("reading disk3's own share branch: %v", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(branches[2], e.Name())); err != nil {
			t.Fatalf("removing disk3's own %s: %v", e.Name(), err)
		}
	}

	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if ch != nil {
		t.Fatal("Sync after emptying disk3: got a non-nil progress channel — a real sync must not have started")
	}
	var blocked *parity.GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync after emptying disk3: err = %v, want a *GuardBlockedError", err)
	}
	var trippedZeroFiles bool
	for _, trig := range blocked.Result.Triggers {
		if trig == parity.TriggerZeroFiles {
			trippedZeroFiles = true
		}
	}
	if !trippedZeroFiles {
		t.Fatalf("Sync after emptying disk3: Triggers = %v, want TriggerZeroFiles — disk3 is not in this sync's RemovingDisks", blocked.Result.Triggers)
	}
}
