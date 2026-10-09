//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. A guarded sync runs `snapraid touch` first (Q17), which gives a
// file whose modification time has a zero sub-second part a non-zero one in
// the same second. A source on a data disk that was already synced, with such
// a time, is therefore touched by the very sync that sits between its copy
// and its delete; the delete's check that the source is still what was copied
// (#776) must keep accepting it, or a relocation, rebalance or evacuation
// entry would never complete.

package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// labWholeSecond sets path's modification time to an exact second and returns
// it.
func labWholeSecond(t *testing.T, path string) time.Time {
	t.Helper()
	mt := time.Unix(time.Now().Add(-72*time.Hour).Unix(), 0)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatalf("Chtimes %s: %v", path, err)
	}
	return mt
}

// labSyncThatMustTouch wraps sync so that, once it ran, every path in
// sources must have gained a sub-second part within the same second: the test
// is about that case and fails loudly if the sync stops producing it. A
// source already deleted, by a later sync that follows the delete phase, is
// not looked at again.
func labSyncThatMustTouch(t *testing.T, sync SyncFunc, sources map[string]time.Time) SyncFunc {
	t.Helper()
	checked := map[string]bool{}
	t.Cleanup(func() {
		if len(checked) != len(sources) {
			t.Errorf("the sync was checked for %d of %d sources", len(checked), len(sources))
		}
	})
	return func(ctx context.Context, manifest []parity.ManifestEntry) error {
		if err := sync(ctx, manifest); err != nil {
			return err
		}
		for path, whole := range sources {
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Errorf("after the sync: %v", err)
				continue
			}
			checked[path] = true
			if got := info.ModTime(); got.Unix() != whole.Unix() || got.Nanosecond() == 0 {
				t.Errorf("%s modification time after the sync = %v, want the same second with a sub-second part (was %v)", path, got, whole)
			}
		}
		return nil
	}
}

func TestLabRelocateToCache_ASourceTouchedByTheGuardedSyncIsStillDeleted(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	mounts := []string{
		filepath.Join(lab, "mnt", "disk-p776-r1"),
		filepath.Join(lab, "mnt", "disk-p776-r2"),
	}
	for i, m := range mounts {
		rebalanceCreateAndMountLoopDisk(t, lab, fmt.Sprintf("p776-r%d", i+1), m)
	}
	engine := rebalanceLabEngineWithMounts(t, lab, "p776-reloc-test", "p776-reloc", mounts, rebalanceGenerousGuard())

	shareName := "touchshare"
	branches := []string{filepath.Join(mounts[0], shareName), filepath.Join(mounts[1], shareName)}
	src := filepath.Join(branches[0], "whole.bin")
	rebalanceLabWriteFile(t, src, "a whole-second modification time")
	for i, m := range mounts {
		rebalanceLabWriteFile(t, filepath.Join(m, "other", "stays.bin"), fmt.Sprintf("outside the share, so disk %d keeps a file", i+1))
	}
	srcAt := labWholeSecond(t, src)
	rebalanceLabSyncOnce(t, ctx, engine)

	cachePath := filepath.Join(lab, "mnt", "cache", "p776-touchshare")
	if err := os.RemoveAll(cachePath); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, cachePath)
	t.Cleanup(func() { _ = os.RemoveAll(cachePath) })
	share := Share{Name: shareName, CachePath: cachePath, Branches: branches}

	deps := Deps{Open: NewFakeOpenChecker(), Sync: labSyncThatMustTouch(t, rebalanceLabSyncFunc(engine), map[string]time.Time{src: srcAt})}
	report, err := RelocateToCache(ctx, share, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	e, ok := resultFor(report, "whole.bin")
	if !ok || e.Result != ResultMoved {
		t.Fatalf("entry = %+v (found %v), want moved; report %+v", e, ok, report.Entries)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Errorf("the source is still there after the sync that covers its copy: %v", err)
	}
	assertFileContent(t, filepath.Join(cachePath, "whole.bin"), "a whole-second modification time")
}

func TestLabEvacuation_ASourceTouchedByTheGuardedSyncIsStillDeleted(t *testing.T) {
	lab := rebalanceLabDir(t)
	ctx := context.Background()

	mounts := []string{
		filepath.Join(lab, "mnt", "disk-p776-e1"),
		filepath.Join(lab, "mnt", "disk-p776-e2"),
		filepath.Join(lab, "mnt", "disk-p776-e3"),
	}
	for i, m := range mounts {
		rebalanceCreateAndMountLoopDisk(t, lab, fmt.Sprintf("p776-e%d", i+1), m)
	}
	engine := rebalanceLabEngineWithMounts(t, lab, "p776-evac-test", "p776-evac", mounts, rebalanceGenerousGuard())

	shareName := "touchevac"
	branches := make([]string, len(mounts))
	for i, m := range mounts {
		branches[i] = filepath.Join(m, shareName)
	}
	whole := filepath.Join(branches[0], "whole.bin")
	nested := filepath.Join(branches[0], "nested", "whole2.bin")
	rebalanceLabWriteFile(t, whole, "a whole-second modification time")
	rebalanceLabWriteFile(t, nested, "another one, nested")
	rebalanceLabWriteFile(t, filepath.Join(branches[1], "stays-on-2.bin"), "already on disk2")
	rebalanceLabWriteFile(t, filepath.Join(branches[2], "stays-on-3.bin"), "already on disk3")
	wholeAt := labWholeSecond(t, whole)
	nestedAt := labWholeSecond(t, nested)
	rebalanceLabSyncOnce(t, ctx, engine)

	share := Share{Name: shareName, Branches: branches}
	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = rebalanceLabTrackedFileCount(engine)
	deps.RebalancePercentLimit = func() float64 { return 99 }
	plan, err := PlanEvacuation(ctx, mounts[0], []Share{share}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	if len(plan.Moves) != 2 {
		t.Fatalf("Moves = %+v, want 2", plan.Moves)
	}
	deps.Sync = labSyncThatMustTouch(t, evacuationLabSyncFunc(engine, map[string]bool{filepath.Clean(mounts[0]): true}),
		map[string]time.Time{whole: wholeAt, nested: nestedAt})

	report, err := RunRebalance(ctx, plan, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance (evacuation): %v", err)
	}
	if len(report.Moved()) != 2 {
		t.Fatalf("Moved() = %d, want 2; report %+v", len(report.Moved()), report.Entries)
	}
	if err := EvacuationPostCheck(mounts[0], []Share{share}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v", err)
	}
	var moved int
	for _, mv := range plan.Moves {
		for _, b := range branches[1:] {
			if _, err := os.Stat(filepath.Join(b, mv.RelPath)); err == nil {
				moved++
			}
		}
	}
	if moved != 2 {
		t.Errorf("%d of 2 evacuated files are on a remaining disk", moved)
	}
}
