//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It proves what a fake Sync cannot: that the removals an
// evacuation or a relocation to cache makes of symlinks, hard-linked names,
// FIFOs and device nodes are all accounted for by the relocation manifest in
// a real SnapraidEngine's threshold guard (#558). The guard here allows one
// unaccounted removal, so any entry type whose move SnapRAID reports as a
// removal that Q14's two-phase confirmation does not recognise blocks the
// final sync.

package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// labGuardEntries creates, under branch, a regular file with a hard-linked
// second name, relative and absolute and dangling symlinks, a FIFO and a
// device node — every entry type a data disk can hold besides a socket.
func labGuardEntries(t *testing.T, branch string) []string {
	t.Helper()
	rebalanceLabWriteFile(t, filepath.Join(branch, "a.bin"), "alpha")
	if err := os.Link(filepath.Join(branch, "a.bin"), filepath.Join(branch, "a-hardlink.bin")); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"rel-link":      "a.bin",
		"abs-link":      filepath.Join(branch, "a.bin"),
		"dangling-link": "missing",
	} {
		if err := os.Symlink(target, filepath.Join(branch, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(branch, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mknod(filepath.Join(branch, "chardev"), unix.S_IFCHR|0o660, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatal(err)
	}
	return []string{"a.bin", "a-hardlink.bin", "rel-link", "abs-link", "dangling-link", "pipe", "chardev"}
}

func labGuardLoopDisks(t *testing.T, tag string) (lab string, mounts []string) {
	t.Helper()
	lab = rebalanceLabDir(t)
	for _, n := range []string{"1", "2"} {
		m := filepath.Join(lab, "mnt", "disk-"+tag+"-"+n)
		rebalanceCreateAndMountLoopDisk(t, lab, tag+"-"+n, m)
		mounts = append(mounts, m)
	}
	return lab, mounts
}

func TestLabEvacuation_EveryEntryTypeIsAccountedByTheStrictGuard(t *testing.T) {
	ctx := context.Background()
	lab, mounts := labGuardLoopDisks(t, "g558e")
	d1, d2 := mounts[0], mounts[1]
	engine := rebalanceLabEngineWithMounts(t, lab, "g558e-test", "g558e", mounts,
		parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 1, RemovedUpdatedPercent: 100}})
	branches := []string{filepath.Join(d1, "s"), filepath.Join(d2, "s")}
	for i := 0; i < 16; i++ {
		rebalanceLabWriteFile(t, filepath.Join(branches[1], fmt.Sprintf("keep%d.bin", i)), "keep")
	}
	entries := labGuardEntries(t, branches[0])
	rebalanceLabSyncOnce(t, ctx, engine)

	share := Share{Name: "s", Branches: branches}
	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	deps.RebalancePercentLimit = func() float64 { return 99 }
	plan, err := PlanEvacuation(ctx, d1, []Share{share}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	deps.Sync = evacuationLabSyncFunc(engine, map[string]bool{filepath.Clean(d1): true})
	report, err := RunRebalance(ctx, plan, Config{}, deps, RunHooks{}, nil)
	var blocked *parity.GuardBlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("the guard blocked the evacuation: RemovedCount=%d triggers=%v accounted=%d of %d entries",
			blocked.Result.RemovedCount, blocked.Result.Triggers, len(blocked.Result.AccountedRemovals), len(entries))
	}
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != len(entries) {
		t.Fatalf("Moved() = %d, want %d: %+v", len(report.Moved()), len(entries), report.Entries)
	}
	if err := EvacuationPostCheck(d1, []Share{share}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v", err)
	}
	for _, rel := range entries {
		if _, err := os.Lstat(filepath.Join(branches[1], rel)); err != nil {
			t.Errorf("%s did not reach the target: %v", rel, err)
		}
	}
}

func TestLabRelocateToCache_EveryEntryTypeIsAccountedByTheStrictGuard(t *testing.T) {
	ctx := context.Background()
	lab, mounts := labGuardLoopDisks(t, "g558c")
	d1, d2 := mounts[0], mounts[1]
	engine := rebalanceLabEngineWithMounts(t, lab, "g558c-test", "g558c", mounts,
		parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 1, RemovedUpdatedPercent: 100}})
	branches := []string{filepath.Join(d1, "s"), filepath.Join(d2, "s")}
	for i := 0; i < 16; i++ {
		rebalanceLabWriteFile(t, filepath.Join(d1, "other", fmt.Sprintf("keep%d.bin", i)), "keep")
	}
	rebalanceLabWriteFile(t, filepath.Join(branches[1], "moves.bin"), "moves")
	rebalanceLabWriteFile(t, filepath.Join(d2, "other", "keep.bin"), "keep")
	entries := labGuardEntries(t, branches[0])
	rebalanceLabSyncOnce(t, ctx, engine)

	cachePath := filepath.Join(lab, "mnt/cache", "g558c-s")
	share := Share{Name: "s", CachePath: filepath.Join(cachePath, "s"), Branches: branches}
	if err := os.MkdirAll(share.CachePath, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cachePath) })

	deps := Deps{Open: NewFakeOpenChecker(), Sync: rebalanceLabSyncFunc(engine)}
	report, err := RelocateToCache(ctx, share, Config{}, deps, RunHooks{}, nil)
	var blocked *parity.GuardBlockedError
	if errors.As(err, &blocked) {
		t.Fatalf("the guard blocked the relocation: RemovedCount=%d triggers=%v accounted=%d of %d entries, zero-files disks %+v, per disk %+v",
			blocked.Result.RemovedCount, blocked.Result.Triggers, len(blocked.Result.AccountedRemovals), len(entries), blocked.Result.ZeroFilesDisks, blocked.Result.Diff.PerDisk)
	}
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	if report.Incomplete() {
		t.Fatalf("relocation incomplete: %+v", report.LeftBehind())
	}
	for _, rel := range entries {
		if _, err := os.Lstat(filepath.Join(share.CachePath, rel)); err != nil {
			t.Errorf("%s did not reach the cache: %v", rel, err)
		}
		if _, err := os.Lstat(filepath.Join(branches[0], rel)); !os.IsNotExist(err) {
			t.Errorf("%s is still on the array: %v", rel, err)
		}
	}
}
