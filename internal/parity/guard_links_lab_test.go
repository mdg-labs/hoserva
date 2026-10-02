//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: built with `go test -tags lab -c` and run inside the lab
// container, like guard_lab_test.go. It drives a real snapraid 12.4-1
// against disks that hold symlinks and hardlinked names, which
// `snapraid status` does not count but `snapraid diff` reports as removals
// and additions like files (doc 02 §2).
//
// Each test brings its own two loop disks and its own SnapRAID fileset, as
// lifecycle_lab_test.go does, and never touches disk1-3 of the standing
// array: SnapRAID parity is positional across disks, so a disk emptied or
// thinned there changes where the files of the tests that run afterward
// land in parity, and one of them (a scrub's corruption next to a fix's
// sync) fails on that alone.

package parity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// trackedEntries counts the names under root SnapRAID tracks as a file or
// a link — regular files and symlinks, hardlinked names included — by
// walking the directory itself, so it is an oracle independent of every
// SnapRAID report the guard reads.
func trackedEntries(t *testing.T, root string) int {
	t.Helper()
	var n int
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return n
}

// linksArray mounts two fresh loop disks at lab/mnt/<name>-d1 and -d2 and
// returns an engine over a SnapRAID fileset of its own named <name>.
func linksArray(t *testing.T, lab, name string) (*SnapraidEngine, []string) {
	t.Helper()
	mounts := []string{
		filepath.Join(lab, "mnt", name+"-d1"),
		filepath.Join(lab, "mnt", name+"-d2"),
	}
	for i, m := range mounts {
		createAndMountLoopDisk(t, lab, fmt.Sprintf("%s-d%d", name, i+1), m)
	}
	return labEngineWithMounts(t, lab, name+"-work", name, mounts), mounts
}

func mustSymlink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Fatalf("symlink %s: %v", name, err)
	}
}

// fillWithLinks writes 4 regular files, 2 symlinks (one dangling) and one
// hardlinked name under dir/links563.
func fillWithLinks(t *testing.T, dir string) {
	t.Helper()
	base := filepath.Join(dir, "links563")
	for _, n := range []string{"fa.bin", "fb.bin", "fc.bin", "fd.bin"} {
		writeFile(t, filepath.Join(base, n), 150_000)
	}
	mustSymlink(t, "fa.bin", filepath.Join(base, "rel-link"))
	mustSymlink(t, "/nonexistent/target", filepath.Join(base, "dangling"))
	if err := os.Link(filepath.Join(base, "fb.bin"), filepath.Join(base, "hard-fb")); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
}

func removeAllUnder(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatalf("removing %s: %v", e.Name(), err)
		}
	}
}

// TestLabGuard_BlocksSyncWhenADiskWithSymlinksUnmounts is the unmounted-
// disk scenario of TestLabGuard_BlocksSyncWhenDiskUnmounts on a disk that
// also holds symlinks and a hardlinked name. `disk_file_count` leaves the
// links out while the diff counts all of them as removed, so a projection
// built on status alone lands below zero and the zero-files rule never
// fired.
func TestLabGuard_BlocksSyncWhenADiskWithSymlinksUnmounts(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := linksArray(t, lab, "links563a")

	disk1 := mounts[0]
	writeFile(t, filepath.Join(mounts[1], "links563/other.bin"), 150_000)
	fillWithLinks(t, disk1)
	syncOnce(t, ctx, engine)
	entries := trackedEntries(t, disk1)

	devOut, err := exec.Command("findmnt", "-n", "-o", "SOURCE", "--target", disk1).Output()
	if err != nil {
		t.Fatalf("findmnt --target %s: %v", disk1, err)
	}
	dev := strings.TrimSpace(string(devOut))
	assertOwnLoop(t, dev, filepath.Join(lab, "img", "links563a-d1.img"))

	if out, err := exec.Command("sync").CombinedOutput(); err != nil {
		t.Fatalf("sync: %v: %s", err, out)
	}
	if out, err := exec.Command("umount", disk1).CombinedOutput(); err != nil {
		t.Fatalf("umount %s: %v: %s", disk1, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("mount", dev, disk1).CombinedOutput(); err != nil {
			t.Errorf("remount %s at %s: %v: %s", dev, disk1, err, out)
		}
	})

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after unmount: %v", err)
	}
	dd, ok := diff.PerDisk[filepath.Clean(disk1)]
	if !ok {
		t.Fatalf("PerDisk missing %s (have %v)", disk1, diff.PerDisk)
	}
	if dd.FilesBefore != entries || dd.FilesAfter != 0 {
		t.Fatalf("PerDisk[%s] = %+v, want {FilesBefore:%d FilesAfter:0} — the disk's %d files and links, all removed", disk1, dd, entries, entries)
	}

	ch, err := engine.Sync(ctx, SyncOpts{})
	if ch != nil {
		t.Fatal("Sync after unmount: got a progress channel — a real sync must not have started")
	}
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync after unmount: err = %v, want a *GuardBlockedError", err)
	}
	if !blocked.Result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Triggers = %v, want zero-files", blocked.Result.Triggers)
	}
	named := false
	for _, z := range blocked.Result.ZeroFilesDisks {
		named = named || z.Disk == filepath.Clean(disk1)
	}
	if !named {
		t.Fatalf("ZeroFilesDisks = %+v, want it to name %s", blocked.Result.ZeroFilesDisks, disk1)
	}
}

// TestLabGuard_ProjectionCountsLinksOnBothSides removes the links and one
// file from a disk that keeps regular files: FilesBefore and FilesAfter
// must equal what is actually on the disk before and after, and the guard
// must not treat the disk as emptied.
func TestLabGuard_ProjectionCountsLinksOnBothSides(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := linksArray(t, lab, "links563b")

	disk1 := mounts[0]
	fillWithLinks(t, disk1)
	writeFile(t, filepath.Join(mounts[1], "links563/other.bin"), 150_000)
	syncOnce(t, ctx, engine)
	before := trackedEntries(t, disk1)

	base := filepath.Join(disk1, "links563")
	for _, n := range []string{"rel-link", "dangling", "hard-fb", "fd.bin"} {
		if err := os.Remove(filepath.Join(base, n)); err != nil {
			t.Fatalf("removing %s: %v", n, err)
		}
	}
	after := trackedEntries(t, disk1)

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	dd := diff.PerDisk[filepath.Clean(disk1)]
	if dd.FilesBefore != before || dd.FilesAfter != after {
		t.Fatalf("PerDisk[%s] = %+v, want {FilesBefore:%d FilesAfter:%d} as counted on the disk", disk1, dd, before, after)
	}
	if res := engine.Guard.Evaluate(diff, nil, nil); res.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("guard fired zero-files for a disk that keeps %d files", after)
	}
	syncOnce(t, ctx, engine)
}

// TestLabGuard_EvacuatedDiskEmptiedOfSymlinksSyncsWithForceEmpty empties a
// disk that holds symlinks and a hardlinked name, as an evacuation does,
// and syncs it as the disk in removal: the guard exempts it and SnapRAID
// refuses an emptied disk without --force-empty, so Sync must pass it.
func TestLabGuard_EvacuatedDiskEmptiedOfSymlinksSyncsWithForceEmpty(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	engine, mounts := linksArray(t, lab, "links563c")
	// The removed-percent rule divides by regular files, which the evacuated
	// disk's removed links outnumber here; this test is about zero-files.
	engine.Guard.Config.RemovedUpdatedPercent = 100000

	disk2 := mounts[1]
	writeFile(t, filepath.Join(mounts[0], "links563/other.bin"), 150_000)
	fillWithLinks(t, disk2)
	syncOnce(t, ctx, engine)
	removeAllUnder(t, disk2)

	diff, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if dd := diff.PerDisk[filepath.Clean(disk2)]; dd.FilesBefore == 0 || dd.FilesAfter != 0 {
		t.Fatalf("PerDisk[%s] = %+v, want FilesBefore > 0 and FilesAfter 0", disk2, dd)
	}

	if _, err := engine.Sync(ctx, SyncOpts{}); !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("Sync of an emptied disk that is not being removed: err = %v, want ErrGuardBlocked", err)
	}

	ch, err := engine.Sync(ctx, SyncOpts{RemovingDisks: map[string]bool{filepath.Clean(disk2): true}})
	if err != nil {
		t.Fatalf("Sync of the disk in removal: %v", err)
	}
	if final := drainReal(t, ch); final.Err != nil {
		t.Fatalf("Sync of the disk in removal failed: %v", final.Err)
	}

	again, err := engine.Diff(ctx)
	if err != nil {
		t.Fatalf("Diff after the sync: %v", err)
	}
	if dd := again.PerDisk[filepath.Clean(disk2)]; dd.FilesBefore != 0 || dd.FilesAfter != 0 {
		t.Fatalf("PerDisk[%s] after the sync = %+v, want {0 0}: SnapRAID tracks nothing there", disk2, dd)
	}
}
