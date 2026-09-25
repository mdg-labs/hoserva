//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container — the
// same pattern share_relocation_lab_test.go uses in this package, and it
// reuses that file's own lab helpers directly (same package, same build
// tag) rather than duplicating them.
//
// It is this round's own close for the finding that blocked #274's first
// attempt: internal/cache's own evacuation lab tests
// (evacuate_lab_test.go, evacuation_interrupt_lab_test.go) prove
// cache.RunRebalance's own mechanics and the threshold guard's zero-files
// exemption (doc 09 §4 step 2, Q15) against a hand-rolled sync closure
// (evacuationLabSyncFunc) that carries removingDisks directly — never
// through production's own wiring, so a regression there (RunEvacuation's
// own sync never actually carrying RemovingDisks, the bug this round
// fixes) could pass every one of those tests while still blocking every
// real evacuation's own final sync after its sources were already
// deleted. This file proves the same full evacuation end to end through
// exactly what main.go registers —
// registry.Register(job.TypeEvacuation, true,
// job.RunEvacuation(job.EvacuationDeps{Sync:
// evacuationSyncFunc(parityEngine), TrackedFileCount:
// rebalanceTrackedFileCount(parityEngine), Shares:
// rebalanceSharesFromStore(shareStore, arrayStore)})) — against a real
// parity.SnapraidEngine and a real `snapraid sync`.
//
// It uses its own dedicated loop disks, never disk1-3 of `make lab-up`'s
// own standing array — the same reason share_relocation_lab_test.go and
// internal/cache's own rebalance/evacuate lab tests do: this test's own
// tracked-file-count and zero-files assertions have to be exact against a
// content file only this test has ever synced.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestLabEvacuation_FullEvacuation_RunsThroughRealRegistryAndSync proves
// a full evacuation — every file moved off a real data disk, protected by
// real, threshold-guarded `snapraid sync` calls, ending with the disk at
// zero tracked files — completes through exactly the registration
// main.go performs, never tripping the guard's own zero-files rule.
func TestLabEvacuation_FullEvacuation_RunsThroughRealRegistryAndSync(t *testing.T) {
	lab := shareRelocLabDir(t)
	ctx := context.Background()

	dataMounts := []string{filepath.Join(lab, "mnt/disk1"), filepath.Join(lab, "mnt/disk2"), filepath.Join(lab, "mnt/disk3")}
	shareName := "p274evacshare"

	engine := shareRelocLabEngine(t, lab, "p274-evac-test", "p274-evac", dataMounts, parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100})

	shareDir := filepath.Join(dataMounts[0], shareName)
	src := filepath.Join(shareDir, "movie.bin")
	shareRelocLabWriteFile(t, src, 300_000)

	// A stable file elsewhere so disk1 dropping to zero files is the only
	// disk this test's own diff ever sees empty — the case the guard's
	// own zero-files exemption has to actually cover for this evacuation
	// to succeed at all.
	stableElsewhere := filepath.Join(dataMounts[1], "other", "stays.bin")
	shareRelocLabWriteFile(t, stableElsewhere, 20_000)

	// job.RunEvacuation goes through cache.RunRebalance exactly as
	// production wires it: no job.EvacuationDeps knob overrides
	// cache.Deps.RebalancePercentLimit, so batch sizing uses
	// parity.DefaultRemovedUpdatedPercent (10%) against a fresh, real
	// tracked-file count — unlike internal/cache's own evacuate_lab_test.go,
	// which calls cache.RunRebalance directly and widens that limit for
	// its own tiny fixture. Moving even this test's single file needs
	// enough other tracked files that 10% of them is still >= 1
	// (rebalanceBatchSize's own doc comment), so pad disk3 with filler
	// files nothing here ever touches.
	for i := 0; i < 20; i++ {
		shareRelocLabWriteFile(t, filepath.Join(dataMounts[2], "filler", fmt.Sprintf("f%02d.bin", i)), 1_000)
	}

	shareRelocLabSyncOnce(t, ctx, engine)

	before, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before evacuation: %v", err)
	}
	d1Before, ok := before.PerDisk[filepath.Clean(dataMounts[0])]
	if !ok || d1Before.FilesBefore != 1 {
		t.Fatalf("disk1's own tracked count before evacuation = %+v, want FilesBefore=1", d1Before)
	}

	shares, arrays := shareRelocLabStores(t, filepath.Join(lab, "mnt/cache"), dataMounts, shareName)
	rebalanceShares := rebalanceSharesFromStore(shares, arrays)

	registry := job.NewRegistry()
	registry.Register(job.TypeEvacuation, true, job.RunEvacuation(job.EvacuationDeps{
		Sync:             evacuationSyncFunc(engine),
		TrackedFileCount: rebalanceTrackedFileCount(engine),
		Shares:           rebalanceShares,
		Store:            arrays,
		// No live pool is mounted by this test (its own file header: it
		// covers the guard/sync wiring, not doc 09 §4 step 2's live
		// no-create switch — evacuation_removal_state_lab_test.go covers
		// that).
		ArrayReady: func(context.Context) error { return nil },
	}))
	scheduler := shareRelocLabScheduler(t, registry)

	// planDiskEvacuation's own production call
	// (internal/api/rebalance_handler.go): compute the plan fresh from the
	// same shares this registration itself resolves, never a hand-built
	// one.
	shareList, err := rebalanceShares(ctx)
	if err != nil {
		t.Fatalf("rebalanceSharesFromStore: %v", err)
	}
	plan, err := cache.PlanEvacuation(ctx, dataMounts[0], shareList, cache.Deps{})
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want 1", plan.Moves)
	}

	params, err := json.Marshal(job.EvacuationParams{Mountpoint: dataMounts[0], Plan: plan})
	if err != nil {
		t.Fatalf("marshaling EvacuationParams: %v", err)
	}
	j, err := scheduler.Submit(ctx, job.TypeEvacuation, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeEvacuation): %v", err)
	}
	finished, err := scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded — a production-wired evacuation must reach a real, threshold-guarded final sync without tripping the zero-files rule on the disk it is deliberately emptying", finished.Status, finished.ErrorMessage)
	}

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("array original should be gone after a completed evacuation: err=%v", err)
	}
	entries, err := os.ReadDir(shareDir)
	if err != nil {
		t.Fatalf("reading evacuated share branch: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("evacuated share branch still has entries = %+v, want empty (EvacuationPostCheck should have run and passed)", entries)
	}

	after, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after evacuation: %v", err)
	}
	if after.Added != 0 || after.Removed != 0 || after.Updated != 0 {
		t.Fatalf("Diff after evacuation reported pending changes: %+v, want none — the job's own final sync must already have covered them", after)
	}
	d1After, ok := after.PerDisk[filepath.Clean(dataMounts[0])]
	if !ok || d1After.FilesAfter != 0 {
		t.Fatalf("disk1's own FilesAfter after evacuation = %+v, want 0", d1After)
	}
}

// TestLabEvacuation_FailedRun_ClearsRemovingDisksExemption is this fix
// round's own lab acceptance test for the finding it closes: a
// job.TypeEvacuation registered exactly the way main.go registers it —
// including Manifest: parityEngine.Relocation, the field the first
// version of this fix round's wiring left the exemption in forever once
// the run failed — must clear the whole persisted relocation state, both
// the manifest and the removing-disks exemption, once the run ends unable
// to resume, against a real parity.RelocationManifestStore and a real
// job.TypeSync/RunSync reading it back exactly as production wires it
// (parity_run.go's own relocationManifestSource), not a fake. Clearing
// only the exemption and leaving the manifest, as an earlier round of
// this fix did, is not enough: parity.matchManifest (internal/parity/
// guard.go) accounts a manifest entry whenever its SourceDisk/RelPath
// pair shows up as *any* removal in a later diff, not only this run's own
// delete, so a stale entry could exempt a later, unrelated removal of the
// same path from the guard's own thresholds.
//
// It reproduces the finding's own lab scenario: an evacuation whose
// pre-delete sync fails (standing in for a real threshold-guard block —
// RunEvacuation cannot tell the two apart, and must not care) leaves the
// source untouched, but must not leave the disk permanently exempt from
// the guard's zero-files rule, nor a stale manifest entry able to exempt
// an unrelated removal. Disk1 then legitimately empties through
// unrelated, ordinary use (never through this stuck job), and a later
// confirmed sync must trip the guard rather than silently going through
// — the exact loss of parity coverage the finding's own lab reproduction
// measured.
func TestLabEvacuation_FailedRun_ClearsRemovingDisksExemption(t *testing.T) {
	lab := shareRelocLabDir(t)
	ctx := context.Background()

	dataMounts := []string{filepath.Join(lab, "mnt/disk1"), filepath.Join(lab, "mnt/disk2"), filepath.Join(lab, "mnt/disk3")}
	shareName := "p274clearshare"

	engine := shareRelocLabEngine(t, lab, "p274-clear-test", "p274-clear", dataMounts, parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100})

	shareDir := filepath.Join(dataMounts[0], shareName)
	src := filepath.Join(shareDir, "movie.bin")
	shareRelocLabWriteFile(t, src, 300_000)

	// Padding on disk3, unrelated to the disk being evacuated, so
	// rebalanceBatchSize's own 10%-of-tracked-files limit still allows
	// this single move — the same reason
	// TestLabEvacuation_FullEvacuation_RunsThroughRealRegistryAndSync
	// pads it.
	for i := 0; i < 20; i++ {
		shareRelocLabWriteFile(t, filepath.Join(dataMounts[2], "filler", fmt.Sprintf("f%02d.bin", i)), 1_000)
	}

	shareRelocLabSyncOnce(t, ctx, engine)

	registry := job.NewRegistry()
	scheduler, db := newRegistryTestSchedulerAndDB(t, registry)
	// The single database main.go shares between the job store and
	// parityEngine.Relocation (main.go's own registration block) — not
	// shareRelocLabStores' own separate database, which only ever holds
	// array/share topology, never the relocation manifest.
	engine.Relocation = parity.NewRelocationManifestStore(db)

	shares, arrays := shareRelocLabStores(t, filepath.Join(lab, "mnt/cache"), dataMounts, shareName)
	rebalanceShares := rebalanceSharesFromStore(shares, arrays)

	shareList, err := rebalanceShares(ctx)
	if err != nil {
		t.Fatalf("rebalanceSharesFromStore: %v", err)
	}
	plan, err := cache.PlanEvacuation(ctx, dataMounts[0], shareList, cache.Deps{})
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("Moves = %+v, want 1", plan.Moves)
	}

	// Stand in for a real threshold-guard block on the evacuation's own
	// pre-delete sync: RunEvacuation reaches the exact same non-resumable
	// ending (ErrJobNotInterrupted — "only an interrupted job can be
	// resumed") whatever the sync's own error is, so this is a
	// deterministic substitute for tuning a real guard trip, not a
	// weakening of the scenario.
	var syncCalls int
	failingSync := func(context.Context, []parity.ManifestEntry, map[string]bool) error {
		syncCalls++
		return fmt.Errorf("synthetic sync failure standing in for a threshold-guard block")
	}
	registry.Register(job.TypeEvacuation, true, job.RunEvacuation(job.EvacuationDeps{
		Sync:             failingSync,
		TrackedFileCount: rebalanceTrackedFileCount(engine),
		Shares:           rebalanceShares,
		Manifest:         engine.Relocation,
		Store:            arrays,
		ArrayReady:       func(context.Context) error { return nil },
	}))
	registry.Register(job.TypeSync, false, job.RunSync(engine))

	params, err := json.Marshal(job.EvacuationParams{Mountpoint: dataMounts[0], Plan: plan})
	if err != nil {
		t.Fatalf("marshaling EvacuationParams: %v", err)
	}
	j, err := scheduler.Submit(ctx, job.TypeEvacuation, nil, params)
	if err != nil {
		t.Fatalf("Submit(TypeEvacuation): %v", err)
	}
	finished, err := scheduler.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if syncCalls == 0 {
		t.Fatal("sync was never called — the batch's own manifest should have been persisted and its pre-delete sync attempted before this failure")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a failed sync — an evacuation must never delete a source before the sync covering its copy: %v", err)
	}

	manifest, removingDisks, err := engine.Relocation.Current(ctx)
	if err != nil {
		t.Fatalf("reading relocation manifest after the failed run: %v", err)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("after the failed (non-resumable) run, store = (manifest=%+v, removingDisks=%+v), want both cleared", manifest, removingDisks)
	}
	// Unlike the guard's own exemption above, a plain failure (as opposed
	// to an explicit Cancel) must leave the disk's own removal_state
	// exactly as it was — still "evacuating" — so a later resume or
	// retry still finds it no-create (#359, doc 09 §4 step 2).
	if _, state, err := arrays.RemovingDisk(ctx); err != nil {
		t.Fatalf("RemovingDisk: %v", err)
	} else if state != store.RemovalStateEvacuating {
		t.Fatalf("removal_state after a failed (non-cancelled) run = %q, want still %q", state, store.RemovalStateEvacuating)
	}

	// Disk1 now legitimately empties through unrelated, ordinary use —
	// never through the stuck evacuation job — the same way the finding's
	// own lab reproduction has the operator "keep using disk1" after the
	// evacuation gave up.
	if err := os.Remove(src); err != nil {
		t.Fatalf("removing disk1's own remaining file: %v", err)
	}

	syncJob, err := scheduler.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(TypeSync): %v", err)
	}
	syncFinished, err := scheduler.Await(ctx, syncJob.ID)
	if err != nil {
		t.Fatalf("Await(sync): %v", err)
	}
	if syncFinished.Status != job.StatusFailed {
		t.Fatalf("status = %s (%s), want failed — a stale removing-disks exemption would let disk1's own zero-files drop sync through silently, losing parity coverage for it (the finding this test closes)", syncFinished.Status, syncFinished.ErrorMessage)
	}
	if !strings.Contains(syncFinished.ErrorMessage, "zero-files") {
		t.Fatalf("ErrorMessage = %q, want a zero-files threshold-guard block", syncFinished.ErrorMessage)
	}
}
