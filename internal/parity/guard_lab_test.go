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
