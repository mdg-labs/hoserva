//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), like
// datadisks_lab_test.go. It builds btrfs and ext4 images as a power cut leaves
// them: mounted with a long commit interval, written and fsync'd, and the image
// copied while still mounted, so the changes are only in the filesystem's log
// (btrfs log_root, ext4 needs_recovery). A mount that replays that log writes to
// the disk, and one that does not shows files missing; the scan has to refuse
// such a disk and leave every byte of it alone.

package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// crashImage returns an image of a filesystem of fsType as it is after a power
// cut with 20 fsync'd files in "share" that its log has not yet applied.
func crashImage(t *testing.T, name, fsType string, mkfs []string) string {
	t.Helper()
	ctx := context.Background()
	r := disk.CommandRunner{}
	lab := labDir(t)
	img := filepath.Join(lab, "crash-"+name+".img")
	snap := filepath.Join(lab, "crash-"+name+"-snap.img")
	t.Cleanup(func() { _ = os.Remove(img); _ = os.Remove(snap) })
	if out, err := r.Run(ctx, "truncate", "-s", "400M", img); err != nil {
		t.Fatalf("truncate: %v %s", err, out)
	}
	dev := attachLoop(ctx, t, r, img, 0, 0)
	if out, err := r.Run(ctx, mkfs[0], append(mkfs[1:], dev)...); err != nil {
		t.Fatalf("%v: %v %s", mkfs, err, out)
	}
	rw := filepath.Join(lab, "crash-"+name+"-rw")
	if err := os.MkdirAll(rw, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(rw) })
	if out, err := r.Run(ctx, "mount", "-t", fsType, "-o", "commit=300", dev, rw); err != nil {
		t.Fatalf("mounting %s read-write: %v %s", fsType, err, out)
	}
	mounted := true
	unmount := func() {
		if !mounted {
			return
		}
		if _, err := r.Run(context.Background(), "umount", rw); err == nil {
			mounted = false
		}
	}
	t.Cleanup(unmount)
	if err := os.MkdirAll(filepath.Join(rw, "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, "sync"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		f, err := os.Create(filepath.Join(rw, "share", "file-"+strings.Repeat("x", i+1)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(strings.Repeat("data", 1000+i)); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	d, err := os.Open(filepath.Join(rw, "share"))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if out, err := r.Run(ctx, "cp", "--sparse=always", img, snap); err != nil {
		t.Fatalf("cp: %v %s", err, out)
	}
	unmount()
	if mounted {
		t.Fatalf("could not unmount %s", rw)
	}
	return snap
}

// assertCrashImage fails unless dev's superblock itself, read with the tools'
// own output and not through the scan's log inspection, shows a log that was
// never applied; a crash image that does not is not one, and the tests built on
// it would pass vacuously.
func assertCrashImage(t *testing.T, dev string, fs disk.FilesystemType) {
	t.Helper()
	r := disk.CommandRunner{}
	switch fs {
	case disk.BTRFS:
		out, err := r.Run(context.Background(), "btrfs", "inspect-internal", "dump-super", dev)
		if err != nil {
			t.Fatalf("dump-super %s: %v", dev, err)
		}
		for _, l := range strings.Split(string(out), "\n") {
			if f := strings.Fields(l); len(f) == 2 && f[0] == "log_root" && f[1] != "0" {
				return
			}
		}
		t.Fatalf("the btrfs crash image on %s has log_root 0", dev)
	default:
		out, err := r.Run(context.Background(), "dumpe2fs", "-h", dev)
		if err != nil {
			t.Fatalf("dumpe2fs %s: %v", dev, err)
		}
		if !strings.Contains(string(out), "needs_recovery") {
			t.Fatalf("the ext4 crash image on %s does not need recovery", dev)
		}
	}
}

// swapSlot makes the fixture's slot a bare filesystem on the loop device dev, as
// the inventory would report it.
func swapSlot(t *testing.T, a *labArray, slot, dev string) {
	t.Helper()
	s := a.slot(slot)
	props := probe(t, a.runner, dev)
	s.whole, s.part, s.fs, s.uuid = dev, dev, props["TYPE"], props["UUID"]
	a.disks = disk.NewFakeProvider()
	for _, o := range a.slots {
		a.disks.AddDisk(o.whole, disk.Disk{Serial: o.id, Size: o.size, Filesystem: o.fs, FSDevice: o.part, FSUUID: o.uuid})
	}
}

// plainReadOnlyChangesTheDisk is the control arm: a plain `-o ro` mount of a
// fresh crash image writes to it, so the whole-device hash the scan tests
// compare is able to see a read-only mount write.
func plainReadOnlyChangesTheDisk(t *testing.T, name, fsType string, mkfs []string) {
	t.Helper()
	ctx := context.Background()
	r := disk.CommandRunner{}
	dev := attachLoop(ctx, t, r, crashImage(t, name, fsType, mkfs), 0, 0)
	before := sha256File(t, dev)
	where := filepath.Join(labDir(t), "crash-"+name+"-control")
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(where) })
	if out, err := r.Run(ctx, "mount", "-t", fsType, "-o", "ro", dev, where); err != nil {
		t.Fatalf("control: mounting read-only: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = r.Run(context.Background(), "umount", where) })
	if _, err := r.Run(ctx, "umount", where); err != nil {
		t.Fatalf("control: unmounting: %v", err)
	}
	if after := sha256File(t, dev); after == before {
		t.Errorf("control: a plain read-only %s mount of a crash image left the device's sha256 unchanged (%s): this harness would not have seen a scan write to a source disk", fsType, after)
	}
}

// scanOfACrashedDisk scans the fixture with slot replaced by a crash image of
// fsType, and shows the disk refused for its log, never mounted by the scan or
// by the DiskReader, and every device byte-identical afterwards.
func scanOfACrashedDisk(t *testing.T, slot, fsType string, fs disk.FilesystemType, mkfs []string, wantInRow string) {
	t.Helper()
	ctx := context.Background()
	a := attachFixture(t, "unraid-btrfs-and-ext4-disks")
	dev := attachLoop(ctx, t, a.runner, crashImage(t, slot+"-scan", fsType, mkfs), 0, 0)
	assertCrashImage(t, dev, fs)
	swapSlot(t, a, slot, dev)
	if got := a.slot(slot).fs; got != fsType {
		t.Fatalf("%s reports %q, want %s", slot, got, fsType)
	}
	if err := disk.AdoptCheck(ctx, a.runner, dev, fs); err != nil {
		t.Fatalf("the crash image fails %s's own check (%v), so it does not show the log being missed by it", fs, err)
	}
	before := a.hashes()

	p := &readOnlyProbe{t: t}
	s := a.scanner(t, p)
	r, err := a.scan(t, s, ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	row := findRow(t, r, CheckIntegrity, slot)
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, slot+" is not adopted") || !strings.Contains(row.Detail, wantInRow) || !strings.Contains(row.Detail, "stop the array cleanly") {
		t.Errorf("%s integrity row = %+v, want it refused by name for its log", slot, row)
	}
	for _, m := range p.devices {
		if m == dev {
			t.Errorf("the scan mounted %s, a %s disk with a pending log", slot, fsType)
		}
	}
	if len(p.devices) == 0 {
		t.Errorf("the scan went on to no other disk")
	}
	if r.Baseline != nil {
		for _, d := range r.Baseline.Disks {
			if d.Slot == slot {
				t.Errorf("%s is in the baseline although it was refused", slot)
			}
		}
	}
	a.assertUnchanged(before, "after the scan")
	assertNoMountUnder(t, s.Dir)

	list, err := a.disks.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range list {
		if d.FSDevice != dev {
			continue
		}
		names, err := s.Dirs.TopLevelDirs(ctx, d)
		if err == nil {
			t.Errorf("the DiskReader read %s, listing %v", dev, names)
		} else if !strings.Contains(err.Error(), wantInRow) || !strings.Contains(err.Error(), "stop the array cleanly") {
			t.Errorf("the DiskReader refused %s for some other reason than its log: %v, want it to name %q and the clean-stop advice", dev, err, wantInRow)
		}
	}
	for _, m := range p.devices {
		if m == dev {
			t.Errorf("the DiskReader mounted %s, a %s disk with a pending log", dev, fsType)
		}
	}
	a.assertUnchanged(before, "after the DiskReader")
	assertNoMountUnder(t, s.Dir)
}

func TestLabCrashLog_ABtrfsDiskWithAPendingTreeLogIsRefusedAndNeverWritten(t *testing.T) {
	mkfs := []string{"mkfs.btrfs", "-f"}
	scanOfACrashedDisk(t, "disk3", "btrfs", disk.BTRFS, mkfs, "log_root")
	plainReadOnlyChangesTheDisk(t, "btrfs-control", "btrfs", mkfs)
}

func TestLabCrashLog_AnExt4DiskWithAnUnreplayedJournalIsRefusedAndNeverWritten(t *testing.T) {
	mkfs := []string{"mkfs.ext4", "-F", "-q"}
	scanOfACrashedDisk(t, "disk4", "ext4", disk.EXT4, mkfs, "needs_recovery")
	plainReadOnlyChangesTheDisk(t, "ext4-control", "ext4", mkfs)
}

// The read-only mount itself, which the DiskReader reaches for a pool device
// whose log was checked clean, writes nothing to a btrfs crash image: this is
// the mount option's own test, apart from the refusal that comes before it.
func TestLabCrashLog_TheScansBtrfsMountNeverReplaysATreeLog(t *testing.T) {
	ctx := context.Background()
	r := disk.CommandRunner{}
	dev := attachLoop(ctx, t, r, crashImage(t, "btrfs-mount", "btrfs", []string{"mkfs.btrfs", "-f"}), 0, 0)
	assertCrashImage(t, dev, disk.BTRFS)
	before := sha256File(t, dev)
	where := filepath.Join(labDir(t), "crash-btrfs-mount-mnt", mountsDir, dataMount)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(labDir(t), "crash-btrfs-mount-mnt")) })
	d := disk.Disk{Device: dev, FSDevice: dev, FSUUID: probe(t, r, dev)["UUID"]}
	if err := withDisk(ctx, disk.KernelReadOnlyMounter{Runner: r}, d, disk.BTRFS, where, func(root string) error {
		_, err := os.ReadDir(filepath.Join(root, "share"))
		return err
	}); err != nil {
		t.Fatalf("withDisk: %v", err)
	}
	if after := sha256File(t, dev); after != before {
		t.Errorf("the scan's btrfs mount changed the device: sha256 %s, was %s", after, before)
	}
}
