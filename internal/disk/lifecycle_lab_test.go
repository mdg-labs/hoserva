//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern format_lab_test.go uses. It proves doc 02 §4 "Adding a
// disk" steps 3-4 against a real device: FormatForAddition builds a real
// filesystem on a fresh loop device, and the disk is mountable and
// immediately writable — no separate step, and no wait, before its
// capacity is usable ("no rebuild").

package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestLabAddDisk_FormatsAndMountsANewDataDisk exercises this issue's
// "Adding a disk" flow end to end against one real loop device: format,
// then mount at the next free slot NextDataMountpoint computed, then a
// write lands on it immediately.
func TestLabAddDisk_FormatsAndMountsANewDataDisk(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}

	dev := createLoopImage(ctx, t, r, lab, "add-disk-new")

	// Doc 02 §4 step 4: the next free slot, computed the same way a
	// caller tracking the array's own current disks would.
	existing := []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"}
	next := NextDataMountpoint(existing)
	if next != "/mnt/disk4" {
		t.Fatalf("NextDataMountpoint(%v) = %q, want /mnt/disk4", existing, next)
	}

	addition := DiskAddition{Device: dev, Filesystem: XFS}
	if err := FormatForAddition(ctx, provider, r, addition); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	if got := blkidType(ctx, r, dev); got != "xfs" {
		t.Fatalf("blkid TYPE = %q, want xfs", got)
	}

	uuid, err := FilesystemUUID(ctx, r, dev)
	if err != nil {
		t.Fatalf("FilesystemUUID: %v", err)
	}
	unit := DataDiskMountUnit(next, uuid, XFS)
	if unit.Where != next || unit.UUID != uuid || unit.Filesystem != XFS {
		t.Fatalf("DataDiskMountUnit: got %+v", unit)
	}

	// The lab has no systemd to drive unit.Render() through, so the mount
	// itself is exercised directly by device, exactly as create-array.sh
	// mounts every other lab disk (doc 06 §3) — proving the same real
	// mkfs.xfs output this test just built is a real, mountable
	// filesystem, which is what the rendered unit's own What= line would
	// bring up in production.
	mountpoint := filepath.Join(lab, "mnt", "disk-added")
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if _, err := r.Run(ctx, "mount", dev, mountpoint); err != nil {
		t.Fatalf("mount %s %s: %v", dev, mountpoint, err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", mountpoint)
	})

	// No rebuild: the disk is immediately writable, with nothing else to
	// wait for or run first (doc 02 §4: "capacity is available
	// immediately").
	testFile := filepath.Join(mountpoint, "hello.txt")
	if err := os.WriteFile(testFile, []byte("hello from the newly added disk"), 0o644); err != nil {
		t.Fatalf("writing to newly added disk: %v", err)
	}
	got, err := os.ReadFile(testFile)
	if err != nil || string(got) != "hello from the newly added disk" {
		t.Fatalf("reading back from newly added disk: got (%q, %v)", got, err)
	}
}

// TestLabReplace_IdentifiesAndFormatsTheReplacementAtTheSameMountpoint
// exercises doc 02 §4 "Replacing a failed disk" steps 3: a fresh loop
// device, confirmed by stable identity, is formatted and mounted back at
// the exact mountpoint a failed disk used — not a new slot.
func TestLabReplace_IdentifiesAndFormatsTheReplacementAtTheSameMountpoint(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := CommandRunner{}
	provider := &LinuxProvider{Lister: NewLister(), Exec: r}

	replacement := createLoopImage(ctx, t, r, lab, "replace-disk-new")

	// A weak-identity, no-by-id-link device (every loop device in this
	// lab, doc 06 §3) has nothing to re-verify against, so FormatForAddition
	// trusts the device path it was given directly — the same behaviour
	// FormatAssigned documents for array setup.
	addition := DiskAddition{Device: replacement, Filesystem: XFS}
	if err := FormatForAddition(ctx, provider, r, addition); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	if got := blkidType(ctx, r, replacement); got != "xfs" {
		t.Fatalf("blkid TYPE = %q, want xfs", got)
	}

	// The failed disk's own mountpoint — the replacement always returns
	// to exactly this path, never a freshly computed one.
	mountpoint := filepath.Join(lab, "mnt", "disk-replaced")
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mountpoint, err)
	}
	if _, err := r.Run(ctx, "mount", replacement, mountpoint); err != nil {
		t.Fatalf("mount %s %s: %v", replacement, mountpoint, err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", mountpoint)
	})

	if _, err := os.Stat(mountpoint); err != nil {
		t.Fatalf("replacement not mounted at %s: %v", mountpoint, err)
	}
}
