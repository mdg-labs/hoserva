//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern mount_lab_test.go uses. It proves this package's own piece
// of doc 02 §4 "Adding a disk" steps 5-6 against a real mergerfs mount:
// AppendDataDisk grows the branch list, and Remount brings the pool back
// up with it — with no separate step, and nothing computed, before the
// new disk's capacity is part of the pool.

package pool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// TestLabAppendDataDisk_RemountJoinsTheNewDiskWithNoRebuild grows a real
// catch-all mount by one disk and confirms two things a "rebuild" would
// contradict: the file written through the pool before the new disk
// existed is still there afterward, byte for byte, and a file placed
// directly on the new disk's own branch is visible through the pool the
// moment Remount returns — no separate computation step in between.
func TestLabAppendDataDisk_RemountJoinsTheNewDiskWithNoRebuild(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	runner := disk.CommandRunner{}
	mounter := Mounter{Runner: runner}

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	for _, d := range dataDisks {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("data disk %s not present — expected create-array.sh to have mounted it: %v", d, err)
		}
	}

	opts := Options{MinFreeSpace: "50M", Responsiveness: Responsive}
	catchAllWhere := filepath.Join(lab, "mnt", "pool-add-disk-test")
	mustMkdirAll(t, catchAllWhere)

	catchAll, err := CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAll.Where = catchAllWhere

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), catchAllWhere)
	})
	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("mounting catch-all: %v", err)
	}

	mustWriteFile(t, filepath.Join(catchAllWhere, "before-add.txt"), "written before the new disk existed")

	newDisk := filepath.Join(lab, "mnt", "pool-add-disk-new")
	createLoopDiskForPoolTest(t, lab, "pool-add-disk", newDisk)

	grown, err := AppendDataDisk(dataDisks, newDisk)
	if err != nil {
		t.Fatalf("AppendDataDisk: %v", err)
	}
	grownMount, err := CatchAllMount(grown, opts)
	if err != nil {
		t.Fatalf("CatchAllMount (grown): %v", err)
	}
	grownMount.Where = catchAllWhere

	if err := mounter.Remount(ctx, grownMount); err != nil {
		t.Fatalf("Remount: %v", err)
	}

	if got := mustReadFile(t, filepath.Join(catchAllWhere, "before-add.txt")); got != "written before the new disk existed" {
		t.Fatalf("after Remount, before-add.txt = %q, want the byte-identical original — no rebuild means existing content survives untouched", got)
	}

	// A file placed directly on the new disk's own branch is immediately
	// visible through the pool — proving the branch actually joined, with
	// nothing else required first.
	mustWriteFile(t, filepath.Join(newDisk, "on-new-disk.txt"), "lives on the newly added disk")
	if got := mustReadFile(t, filepath.Join(catchAllWhere, "on-new-disk.txt")); got != "lives on the newly added disk" {
		t.Fatalf("on-new-disk.txt not visible through the pool after Remount: got %q", got)
	}
}

// createLoopDiskForPoolTest truncates a fresh sparse image, attaches it,
// formats it XFS and mounts it at mountpoint — mirroring
// create-array.sh's own recipe (doc 06 §3) — detaching only this device,
// backed by the image this call created, once the test is done
// (CLAUDE.md: never losetup -D).
func createLoopDiskForPoolTest(t *testing.T, lab, name, mountpoint string) string {
	t.Helper()
	r := disk.CommandRunner{}
	ctx := context.Background()

	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}
	if _, err := r.Run(ctx, "truncate", "-s", "320M", img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}
	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev := trimNewline(out)

	if _, err := r.Run(ctx, "mkfs.xfs", "-q", dev); err != nil {
		t.Fatalf("mkfs.xfs %s: %v", dev, err)
	}
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if _, err := r.Run(ctx, "mount", dev, mountpoint); err != nil {
		t.Fatalf("mount %s %s: %v", dev, mountpoint, err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", mountpoint)
		_, _ = r.Run(context.Background(), "losetup", "-d", dev)
	})
	return dev
}

func trimNewline(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
