//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host, the same way mover_lab_test.go and relocate_lab_test.go do.
// It proves what the unit tests over a temp directory cannot: that a
// mover run and a share relocation carry every entry type — symlinks,
// FIFOs, character and block device nodes, a sparse file and a hard-linked
// pair — across real XFS filesystems and through the real array-only
// mergerfs mount, keeping owner, mode, timestamps, xattrs and holes, and
// that a socket is left in place and reported (doc 09 §2, #558).

package cache

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/parity"
)

const (
	labOwnerUID = 1234
	labOwnerGID = 5678
	labXattr    = "user.hoserva_entry_test"
)

var labEntries = append(slices.Clone(movedEntries), "app/chardev", "app/blockdev")

// labDecorateTree adds what only a real root-in-a-lab can make — device
// nodes, files owned by someone else, an xattr — to a tree built by
// buildEntryTree, then gives every entry the old mtime the grace period
// needs.
func labDecorateTree(t *testing.T, tree entryTree) {
	t.Helper()
	if err := unix.Mknod(tree.path("app/chardev"), unix.S_IFCHR|0o660, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatalf("mknod char device: %v", err)
	}
	if err := unix.Mknod(tree.path("app/blockdev"), unix.S_IFBLK|0o640, int(unix.Mkdev(7, 254))); err != nil {
		t.Fatalf("mknod block device: %v", err)
	}
	for _, rel := range tree.entries(t) {
		if err := os.Lchown(tree.path(rel), labOwnerUID, labOwnerGID); err != nil {
			t.Fatalf("lchown %s: %v", rel, err)
		}
	}
	for _, rel := range []string{"plain.txt", "vm.img"} {
		if err := unix.Setxattr(tree.path(rel), labXattr, []byte("kept:"+rel), 0); err != nil {
			t.Fatalf("setxattr %s: %v", rel, err)
		}
	}
	old := unix.NsecToTimespec(time.Now().Add(-entryMTimeAgo).Truncate(time.Second).UnixNano())
	for _, rel := range tree.entries(t) {
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, tree.path(rel), []unix.Timespec{old, old}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			t.Fatalf("utimes %s: %v", rel, err)
		}
	}
}

func labAssertCarried(t *testing.T, snap entrySnapshot, dstRoot string) {
	t.Helper()
	snap.assertOn(t, dstRoot)
	for rel, item := range snap {
		if item.info.Mode()&os.ModeDevice == 0 {
			continue
		}
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dstRoot, rel), &st); err != nil {
			t.Fatal(err)
		}
		if uint64(st.Rdev) != item.rdev {
			t.Errorf("%s: device number %d, want %d", rel, st.Rdev, item.rdev)
		}
	}
	for _, rel := range []string{"plain.txt", "vm.img"} {
		got, err := getXattr(filepath.Join(dstRoot, rel), labXattr)
		if err != nil || string(got) != "kept:"+rel {
			t.Errorf("%s: xattr = %q (%v), want %q", rel, got, err, "kept:"+rel)
		}
	}
}

func TestLabMover_MovesEveryEntryTypeThroughMergerfs(t *testing.T) {
	top := bringUpLabMoverTopology(t, "entrymover")
	share := top.cacheShare()
	tree := buildEntryTree(t, share.CachePath)
	labDecorateTree(t, tree)
	snap := snapshotEntries(t, tree, labEntries)

	report, err := RelocateToArray(context.Background(), share, Config{VerifyChecksum: true}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	labAssertCarried(t, snap, share.ArrayPath)
	assertGoneOrLeft(t, share.CachePath, labEntries, false)
	assertGoneOrLeft(t, share.CachePath, []string{"app/runtime.sock"}, true)
	if report.Incomplete() {
		t.Fatalf("only a socket was left behind: %+v", report.LeftBehind())
	}
	if e, _ := resultFor(report, "app/runtime.sock"); e.Result != ResultSkippedSocket {
		t.Fatalf("socket entry = %+v", e)
	}
	landed := 0
	for _, d := range top.dataDisks {
		if _, err := os.Lstat(filepath.Join(d, share.Name, "app/rel-link")); err == nil {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("the symlink landed on %d data disks, want the one mergerfs chose", landed)
	}
}

func TestLabRelocateToCache_CarriesEveryEntryType(t *testing.T) {
	top := bringUpLabMoverTopology(t, "entrytocache")
	share := top.cacheShare()
	tree := buildEntryTree(t, share.Branches[0])
	labDecorateTree(t, tree)
	snap := snapshotEntries(t, tree, labEntries)

	deps := Deps{Sync: func(context.Context, []parity.ManifestEntry) error { return nil }}
	report, err := RelocateToCache(context.Background(), share, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToCache: %v", err)
	}
	labAssertCarried(t, snap, share.CachePath)
	assertGoneOrLeft(t, share.Branches[0], labEntries, false)
	assertGoneOrLeft(t, share.Branches[0], []string{"app/runtime.sock"}, true)
	if report.Incomplete() {
		t.Fatalf("only a socket was left behind: %+v", report.LeftBehind())
	}
}

func TestLabEvacuation_CarriesEveryEntryType(t *testing.T) {
	lab := rebalanceLabDir(t)
	d1 := filepath.Join(lab, "mnt", "disk-p558-a1")
	d2 := filepath.Join(lab, "mnt", "disk-p558-a2")
	rebalanceCreateAndMountLoopDisk(t, lab, "p558-a1", d1)
	rebalanceCreateAndMountLoopDisk(t, lab, "p558-a2", d2)
	share := Share{Name: "entryevac", Branches: []string{filepath.Join(d1, "entryevac"), filepath.Join(d2, "entryevac")}}

	tree := buildEntryTree(t, share.Branches[0])
	if err := os.Remove(tree.path("app/runtime.sock")); err != nil {
		t.Fatal(err)
	}
	labDecorateTree(t, tree)
	evacuated := labEntries
	snap := snapshotEntries(t, tree, evacuated)

	deps := Deps{Open: NewFakeOpenChecker()}
	deps.TrackedFileCount = func(context.Context) (int, error) { return 100000, nil }
	deps.Sync = func(context.Context, []parity.ManifestEntry) error { return nil }
	plan, err := PlanEvacuation(context.Background(), d1, []Share{share}, deps)
	if err != nil {
		t.Fatalf("PlanEvacuation: %v", err)
	}
	report, err := RunRebalance(context.Background(), plan, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RunRebalance: %v", err)
	}
	if len(report.Moved()) != len(evacuated) {
		t.Fatalf("Moved() = %d, want %d: %+v", len(report.Moved()), len(evacuated), report.Entries)
	}
	labAssertCarried(t, snap, share.Branches[1])
	if err := EvacuationPostCheck(d1, []Share{share}); err != nil {
		t.Fatalf("EvacuationPostCheck: %v", err)
	}
}
