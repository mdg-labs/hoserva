//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It proves that a directory of a target disk turned into a
// symlink to somewhere else on the host makes the mover, a share
// relocation and the shared copy routine fail the entry and keep its
// source, with nothing written outside the disk, as root, across real XFS
// filesystems and through the real array-only mergerfs mount (#656).

package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/beneath"
	"github.com/mdg-labs/hoserva/internal/parity"
)

func labOutsideDir(t *testing.T, name string) string {
	t.Helper()
	outside := filepath.Join(labDir(t), "outside-"+name)
	mustMkdirAll(t, outside)
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	return outside
}

func labSymlinkDocumentsOnEveryDisk(t *testing.T, top labMoverTopology, outside string) {
	t.Helper()
	for _, d := range top.dataDisks {
		link := filepath.Join(d, top.shareName(), "documents")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
	}
}

func labAssertKeptAndNothingOutside(t *testing.T, report Report, src, outside string) {
	t.Helper()
	e, ok := resultFor(report, "documents/Reports/Q3.txt")
	if !ok || e.Result != ResultFailed || !strings.Contains(e.Err, beneath.ErrSymlink.Error()) {
		t.Fatalf("entry = %+v, want failed naming the symlink", e)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatalf("the source was touched: %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("%d entries were written outside the disks", len(entries))
	}
}

func TestLabMover_ASymlinkedTargetDirectoryWritesNothingOutsideTheDisk(t *testing.T) {
	top := bringUpLabMoverTopology(t, "symlinkmover")
	share := top.cacheShare()
	labBuildShareTree(t, share.CachePath)
	outside := labOutsideDir(t, "mover")
	labSymlinkDocumentsOnEveryDisk(t, top, outside)

	report, err := Run(context.Background(), []Share{share}, Config{SkipGracePeriod: true, VerifyChecksum: true}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	labAssertKeptAndNothingOutside(t, report, filepath.Join(share.CachePath, "documents/Reports/Q3.txt"), outside)
}

func TestLabRelocateToArray_ASymlinkedTargetDirectoryWritesNothingOutsideTheDisk(t *testing.T) {
	top := bringUpLabMoverTopology(t, "symlinkreloc")
	share := top.cacheShare()
	labBuildShareTree(t, share.CachePath)
	outside := labOutsideDir(t, "reloc")
	labSymlinkDocumentsOnEveryDisk(t, top, outside)

	report, err := RelocateToArray(context.Background(), share, Config{VerifyChecksum: true}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	labAssertKeptAndNothingOutside(t, report, filepath.Join(share.CachePath, "documents/Reports/Q3.txt"), outside)
}

func TestLabRelocateToCache_ASymlinkedTargetDirectoryWritesNothingOutsideTheDisk(t *testing.T) {
	top := bringUpLabMoverTopology(t, "symlinkcache")
	share := top.cacheShare()
	labBuildShareTree(t, share.Branches[0])
	outside := labOutsideDir(t, "cache")
	link := filepath.Join(share.CachePath, "documents")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })

	deps := Deps{Sync: func(context.Context, []parity.ManifestEntry) error { return nil }}
	report, err := RelocateToCache(context.Background(), share, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	labAssertKeptAndNothingOutside(t, report, filepath.Join(share.Branches[0], "documents/Reports/Q3.txt"), outside)
}

// A directory of the target disk swapped for a symlink after the copy has
// resolved it and before it creates the temp file — the moment a share
// user can pick — still creates nothing outside the disk, as root.
func TestLabCopyMoveFile_ADirectorySwappedMidCopyWritesNothingOutsideTheDisk(t *testing.T) {
	top := bringUpLabMoverTopology(t, "symlinkrace")
	srcRoot := filepath.Join(top.cachePath, top.shareName())
	labBuildShareTree(t, srcRoot)
	dstRoot := top.dataDisks[0]
	target := filepath.Join(dstRoot, top.shareName(), "documents")
	mustMkdirAll(t, filepath.Join(target, "Reports"))
	outside := labOutsideDir(t, "race")
	t.Cleanup(func() {
		_ = os.Remove(target)
		_ = os.RemoveAll(target + ".moved")
	})

	src := filepath.Join(srcRoot, "documents/Reports/Q3.txt")
	info, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{UUID: swapForSymlink(t, target, outside)}.withDefaults()

	err = copyMoveFile(src, filepath.Join(target, "Reports/Q3.txt"), dstRoot, info, Config{VerifyChecksum: true}, deps)
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("copyMoveFile = %v, want beneath.ErrSymlink", err)
	}
	assertNothingEscaped(t, src, outside, filepath.Join(target+".moved", "Reports"))
}
