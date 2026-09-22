//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host — same pattern as snapraid_lab_test.go and
// journal_lab_test.go: built with `go test -tags lab -c` from the host
// (compiling touches no device) and run with `docker compose exec -T lab
// <binary>` inside the lab container.
//
// This is this issue's own acceptance criterion, written and run failing
// before the guard existed: fill the array, unmount a disk, run diff,
// and assert the sync is blocked (doc 06 §3's own recipe: "Simulate the
// unmounted-disk case the threshold guard must catch"). Against Sync as
// #28 left it — no guard at all — this scenario would have synced real
// parity over a disk that just went blind, destroying the one thing that
// could have recovered it; that is the exact data-loss path this test
// exists to close off, for real, against a real snapraid binary and a
// real unmount.

package parity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestLabGuard_BlocksSyncWhenDiskUnmounts fills a real array, unmounts one
// data disk's own loop-backed filesystem (never `losetup -D`, never a
// device this lab does not own — assertOwnLoop confirms that before
// touching anything), and confirms a real `snapraid diff` shows that
// disk dropping to zero files and a real Sync call refuses to run
// `snapraid sync` at all.
func TestLabGuard_BlocksSyncWhenDiskUnmounts(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := labEngine(t, lab)

	// Fill the array, including the disk this test is about to unmount,
	// and sync once so its files are part of the array's own synced
	// state — status/diff's own "before" snapshot — not merely present
	// on disk with nothing to lose.
	writeFile(t, filepath.Join(mounts[0], "movies/keep1.bin"), 200_000)
	writeFile(t, filepath.Join(mounts[1], "movies/keep2.bin"), 200_000)
	target := filepath.Join(mounts[2], "photos/guard-target.bin")
	writeFile(t, target, 200_000)
	syncOnce(t, ctx, engine)

	before, err := engine.Status(ctx)
	if err != nil {
		t.Fatalf("Status before unmount: %v", err)
	}
	if before.Freshness != FreshnessGreen {
		t.Fatalf("Status before unmount: Freshness = %v, want FreshnessGreen after a clean sync", before.Freshness)
	}

	disk3 := mounts[2]
	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", disk3).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", disk3, err)
	}
	dev := strings.TrimSpace(string(devOut))
	img := filepath.Join(lab, "img", "disk3.img")
	assertOwnLoop(t, dev, img)

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", disk3).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", disk3, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("mount", dev, disk3).CombinedOutput(); err != nil {
			t.Errorf("remount %s at %s: %v: %s", dev, disk3, err, out)
		}
	})

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after unmount: %v", err)
	}
	dd, ok := diff.PerDisk[filepath.Clean(disk3)]
	if !ok {
		t.Fatalf("Diff after unmount: PerDisk missing %s (have %v)", disk3, diff.PerDisk)
	}
	if dd.FilesBefore == 0 || dd.FilesAfter != 0 {
		t.Fatalf("Diff after unmount: PerDisk[%s] = %+v, want FilesBefore>0 and FilesAfter=0", disk3, dd)
	}

	ch, err := engine.Sync(ctx, SyncOpts{})
	if ch != nil {
		t.Fatal("Sync after unmount: got a non-nil progress channel — a real sync must not have started")
	}
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync after unmount: err = %v, want a *GuardBlockedError", err)
	}
	if !blocked.Result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Sync after unmount: GuardBlockedError.Result.Triggers = %v, want TriggerZeroFiles", blocked.Result.Triggers)
	}
	// The caller-facing decision a notification dispatches from (doc 02
	// §2's "a high-priority notification fires through every configured
	// channel"): this package's own scope ends at exposing that decision
	// queryably — see this issue's own report for why dispatch itself is
	// out of scope here.
	if !blocked.Result.Blocked {
		t.Fatal("Sync after unmount: GuardBlockedError.Result.Blocked = false, want true")
	}

	// Confirm the block was real, not merely reported: a real sync would
	// have committed disk3's now-empty state to the content file, so a
	// fresh diff's own "before" snapshot for disk3 would already read
	// zero. It still reading the original nonzero count proves
	// `snapraid sync` itself was never invoked.
	again, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after blocked sync: %v", err)
	}
	stillBefore, ok := again.PerDisk[filepath.Clean(disk3)]
	if !ok || stillBefore.FilesBefore != dd.FilesBefore {
		t.Fatalf("Diff after blocked sync: PerDisk[%s].FilesBefore = %+v, want unchanged at %d — a real sync must not have run", disk3, stillBefore, dd.FilesBefore)
	}
}

// TestLabGuard_TwoPhaseRelocationTrailingSyncAccountedAcrossTwoSyncs is
// this issue's (#248) own central acceptance criterion, proven against a
// real snapraid binary rather than a hand-built diff: a Q14 two-phase
// relocation (copy+verify, sync #1, delete, sync #2) whose trailing sync
// removes files the manifest already accounts for must not block, even
// though the addition and the removal are recorded by two entirely
// separate, real `snapraid sync` invocations — sync #1's own diff, not
// sync #2's, is what actually shows the addition.
//
// RemovedFilesMax is lowered enough (2, versus the two files this test
// relocates) that, run against the pre-#248 matchManifest — which only
// ever looked for the addition in the *same* diff as the removal —
// sync #2 would have blocked on both files as fully unaccounted; this
// test's own second Sync call succeeding end to end is what confirms the
// gap is actually closed, not merely that the case compiles.
func TestLabGuard_TwoPhaseRelocationTrailingSyncAccountedAcrossTwoSyncs(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := labEngine(t, lab)
	// Isolates this test's own guard decision from the default lab
	// override's 100000-file headroom (labEngine's own doc comment):
	// RemovedUpdatedPercent stays at labEngine's 100% so only the
	// removed-count trigger is in play, at a threshold low enough that 2
	// unaccounted removals genuinely trips it.
	engine.Guard.Config.RemovedFilesMax = 1

	sourceDisk := mounts[0]
	targetDisk := mounts[1]
	relA := "relocate-p248/a.bin"
	relB := "relocate-p248/b.bin"
	pathA := filepath.Join(sourceDisk, relA)
	pathB := filepath.Join(sourceDisk, relB)

	// Establish the pre-relocation baseline: both files already exist on
	// their source disk and are already part of the synced array — what
	// a relocation actually moves, not files nobody has covered by
	// parity yet.
	dataA := writeFile(t, pathA, 150_000)
	dataB := writeFile(t, pathB, 150_000)
	hashA, hashB := sha256Hex(dataA), sha256Hex(dataB)
	syncOnce(t, ctx, engine)

	// Copy + verify: place the same bytes on the target disk, at the
	// same relative path (Q14, doc 09 §3), and confirm the copy is
	// byte-identical before anything is ever synced or deleted.
	targetPathA := filepath.Join(targetDisk, relA)
	targetPathB := filepath.Join(targetDisk, relB)
	if err := os.MkdirAll(filepath.Dir(targetPathA), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(targetPathA), err)
	}
	if err := os.WriteFile(targetPathA, dataA, 0o644); err != nil {
		t.Fatalf("copying %s to %s: %v", pathA, targetPathA, err)
	}
	if err := os.WriteFile(targetPathB, dataB, 0o644); err != nil {
		t.Fatalf("copying %s to %s: %v", pathB, targetPathB, err)
	}
	copiedA, err := os.ReadFile(targetPathA)
	if err != nil || sha256Hex(copiedA) != hashA {
		t.Fatalf("verify copy of %s: err=%v, hash mismatch", relA, err)
	}
	copiedB, err := os.ReadFile(targetPathB)
	if err != nil || sha256Hex(copiedB) != hashB {
		t.Fatalf("verify copy of %s: err=%v, hash mismatch", relB, err)
	}

	manifest := []ManifestEntry{
		{RelPath: relA, SourceDisk: sourceDisk, TargetDisk: targetDisk},
		{RelPath: relB, SourceDisk: sourceDisk, TargetDisk: targetDisk},
	}

	// Sync #1: commits the copies' addition on the target disk. Nothing
	// has been removed from the source yet — this is the diff that
	// actually shows the addition, and it is not the diff the trailing
	// sync below will evaluate.
	ch1, err := engine.Sync(ctx, SyncOpts{Manifest: manifest})
	if err != nil {
		t.Fatalf("Sync #1 (commits the copy): %v", err)
	}
	final1 := drainReal(t, ch1)
	if final1.Err != nil {
		t.Fatalf("Sync #1 failed: %v", final1.Err)
	}

	// Delete: the copy is now covered by parity, so the source files can
	// safely go.
	if err := os.Remove(pathA); err != nil {
		t.Fatalf("removing %s: %v", pathA, err)
	}
	if err := os.Remove(pathB); err != nil {
		t.Fatalf("removing %s: %v", pathB, err)
	}

	// Confirm what this test is actually proving: the trailing diff shows
	// only the removal, with no reappearance of either file on the target
	// disk in *this* diff — the addition was already committed by sync #1.
	trailing, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff before sync #2: %v", err)
	}
	if trailing.Removed != 2 {
		t.Fatalf("Diff before sync #2: Removed = %d, want 2", trailing.Removed)
	}
	for _, f := range trailing.AddedFiles {
		if f.Disk == filepath.Clean(targetDisk) && (f.RelPath == relA || f.RelPath == relB) {
			t.Fatalf("Diff before sync #2 already shows %s added on the target disk — this test needs the addition absent from the trailing diff to prove anything", f.RelPath)
		}
	}

	// Sync #2: the trailing sync. Pre-#248, matchManifest could never
	// account either removal here (the addition is in sync #1's diff, not
	// this one), so both would count fully against RemovedFilesMax=1 and
	// this call would return a *GuardBlockedError instead of running a
	// real sync at all.
	ch2, err := engine.Sync(ctx, SyncOpts{Manifest: manifest})
	if err != nil {
		var blocked *GuardBlockedError
		if errors.As(err, &blocked) {
			t.Fatalf("Sync #2 (trailing) blocked: %s — the manifest-recorded removal was not accounted", blocked.Result.summary())
		}
		t.Fatalf("Sync #2 (trailing): %v", err)
	}
	final2 := drainReal(t, ch2)
	if final2.Err != nil {
		t.Fatalf("Sync #2 (trailing) failed: %v", final2.Err)
	}

	// Confirm the array is now fully synced with nothing pending — both
	// syncs actually ran against the real binary, not merely returned
	// without error.
	afterSync, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after sync #2: %v", err)
	}
	if afterSync.Added != 0 || afterSync.Removed != 0 || afterSync.Updated != 0 {
		t.Fatalf("Diff after sync #2 reported pending changes: %+v, want none", afterSync)
	}
}
