//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern add_disk_lab_test.go and mount_lab_test.go use. It proves
// this package's own piece of doc 09 §4's disk-removal steps against a
// real mergerfs mount: putting a disk in the "removing" state (step 2)
// really does keep mergerfs from placing any new file on it, and
// RemoveDataDisk + Remount (step 7) brings the pool back up without it,
// with every file that was already elsewhere still there.
//
// It uses its own dedicated loop disks, never disk1-3 of `make lab-up`'s
// own standing array (add_disk_lab_test.go's own precedent): this test
// unmounts and remounts its own catch-all repeatedly with a shrinking
// branch list, and a standing array other lab test files also use is not
// this test's own to mutate that way.
//
// The evacuation itself — moving real files off a disk with a real
// SnapRAID sync (doc 09 §4 steps 1, 3-6) — is internal/cache's own lab
// acceptance test (TestLabEvacuation_FullEvacuationAndCleanRemount);
// this file covers this package's own steps 2 and 7 in isolation, the
// same way add_disk_lab_test.go covers doc 02 §4's own steps 5-6 without
// needing a mover or SnapRAID in the loop at all.

package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// TestLabRemoveDataDisk_RemovingStateBlocksNewPlacement_ThenCleanRemount
// is this issue's own lab acceptance criterion, pool-mechanics half:
// with the disk to be removed marked NC (doc 09 §4 step 2), new files
// written through the pool never land on it; RemoveDataDisk and Remount
// (step 7) then bring the pool back up without that disk at all, and
// every file written before — both while the disk was still RW and while
// it was NC — survives untouched.
func TestLabRemoveDataDisk_RemovingStateBlocksNewPlacement_ThenCleanRemount(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	runner := disk.CommandRunner{}
	mounter := Mounter{Runner: runner}

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk-p56-a1"),
		filepath.Join(lab, "mnt", "disk-p56-a2"),
		filepath.Join(lab, "mnt", "disk-p56-a3"),
	}
	for i, d := range dataDisks {
		createLoopDiskForPoolTest(t, lab, fmt.Sprintf("p56-a-remove-%d", i+1), d)
	}
	removing := dataDisks[2]

	opts := Options{MinFreeSpace: "10M", Responsiveness: Responsive}
	catchAllWhere := filepath.Join(lab, "mnt", "pool-remove-disk-test")
	mustMkdirAll(t, catchAllWhere)

	full, err := CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	full.Where = catchAllWhere

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), catchAllWhere)
	})
	if err := mounter.Mount(ctx, full); err != nil {
		t.Fatalf("mounting catch-all: %v", err)
	}

	// Written directly onto disk1's own branch, bypassing the union, so
	// this test's own "survives the remount" assertion does not depend
	// on which disk the create policy happens to pick for a brand-new
	// top-level path among three otherwise-identical, freshly formatted
	// disks — the file's own placement is not what this test is about.
	mustWriteFile(t, filepath.Join(dataDisks[0], "before-removing.txt"), "written while every disk was RW")

	// Put the third disk in the "removing" state (doc 09 §4 step 2):
	// every other data disk stays RW.
	removingMount, err := CatchAllMountRemoving(dataDisks, removing, opts)
	if err != nil {
		t.Fatalf("CatchAllMountRemoving: %v", err)
	}
	removingMount.Where = catchAllWhere
	if err := mounter.Remount(ctx, full, removingMount); err != nil {
		t.Fatalf("Remount into removing state: %v", err)
	}

	// Several new files through the pool, none of them naming an
	// existing path — under KeepFoldersTogether (the catch-all's own
	// fixed policy) that falls back to most-free-space among eligible
	// branches, so if NC were not actually excluding disk3 from create
	// eligibility, at least one of these would very likely land there.
	mustMkdirAll(t, filepath.Join(catchAllWhere, "while-removing"))
	for i := 0; i < 6; i++ {
		mustWriteFile(t, filepath.Join(catchAllWhere, "while-removing", fmt.Sprintf("file%d.txt", i)), "written while disk3 was NC")
	}

	entries, err := os.ReadDir(filepath.Join(removing, "while-removing"))
	if err == nil && len(entries) > 0 {
		t.Fatalf("disk3 (NC) received %d new files it should have been excluded from: %v", len(entries), entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading disk3's own while-removing directory: %v", err)
	}

	// doc 09 §4 step 7: remove from the branch list, remount without it.
	shrunk, err := RemoveDataDisk(dataDisks, removing)
	if err != nil {
		t.Fatalf("RemoveDataDisk: %v", err)
	}
	shrunkMount, err := CatchAllMount(shrunk, opts)
	if err != nil {
		t.Fatalf("CatchAllMount (shrunk): %v", err)
	}
	shrunkMount.Where = catchAllWhere

	if err := mounter.Remount(ctx, removingMount, shrunkMount); err != nil {
		t.Fatalf("Remount without the removed disk: %v", err)
	}

	if got := mustReadFile(t, filepath.Join(catchAllWhere, "before-removing.txt")); got != "written while every disk was RW" {
		t.Fatalf("before-removing.txt = %q after remount without disk3, want the original content untouched", got)
	}
	for i := 0; i < 6; i++ {
		path := filepath.Join(catchAllWhere, "while-removing", fmt.Sprintf("file%d.txt", i))
		if got := mustReadFile(t, path); got != "written while disk3 was NC" {
			t.Fatalf("%s = %q after remount without disk3, want its original content untouched", path, got)
		}
	}

	// Step 9 (doc 09 §4): only now is disk3 safe to unmount, physically
	// remove — this package has nothing further to do with it once it
	// is out of the branch list; its own block-device mount is
	// internal/disk's own concern, not this package's.
}
