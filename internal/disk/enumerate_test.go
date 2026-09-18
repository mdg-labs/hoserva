package disk

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// newTestLister builds a synthetic /sys/class/block + /dev/disk/by-id +
// /proc/mounts tree under t.TempDir() so List can be tested without
// touching any real device (CLAUDE.md).
func newTestLister(t *testing.T) *Lister {
	t.Helper()
	root := t.TempDir()

	sysBlock := filepath.Join(root, "sys-class-block")
	byID := filepath.Join(root, "by-id")
	mustMkdirAll(t, sysBlock)
	mustMkdirAll(t, byID)

	// sda: a whole disk with a strong (wwn) identity, plus its partition
	// sda1, which List must not enumerate as its own disk.
	mustMkdirAll(t, filepath.Join(sysBlock, "sda", "device"))
	mustWriteFile(t, filepath.Join(sysBlock, "sda", "size"), "15628053168\n")
	mustWriteFile(t, filepath.Join(sysBlock, "sda", "device", "model"), "WDC WD80EFZX-68UW8N0\n")
	mustMkdirAll(t, filepath.Join(sysBlock, "sda1"))
	mustWriteFile(t, filepath.Join(sysBlock, "sda1", "size"), "15626006016\n")
	mustWriteFile(t, filepath.Join(sysBlock, "sda1", "partition"), "1\n")

	// sdb: a whole disk with only a usb- by-id link (weak identity).
	mustMkdirAll(t, filepath.Join(sysBlock, "sdb", "device"))
	mustWriteFile(t, filepath.Join(sysBlock, "sdb", "size"), "7814037168\n")
	mustWriteFile(t, filepath.Join(sysBlock, "sdb", "device", "model"), "External USB 3.0\n")

	// loop0: must be skipped entirely.
	mustMkdirAll(t, filepath.Join(sysBlock, "loop0"))
	mustWriteFile(t, filepath.Join(sysBlock, "loop0", "size"), "2048\n")

	mustSymlink(t, "../../sda", filepath.Join(byID, "wwn-0x5000cca0b1c2d3e4"))
	mustSymlink(t, "../../sda", filepath.Join(byID, "ata-WDC_WD80EFZX-68UW8N0_VGH0A1B2"))
	mustSymlink(t, "../../sda1", filepath.Join(byID, "ata-WDC_WD80EFZX-68UW8N0_VGH0A1B2-part1"))
	mustSymlink(t, "../../sdb", filepath.Join(byID, "usb-WD_easystore_25FB_575836314141304A4A3236-0:0"))

	mounts := filepath.Join(root, "mounts")
	mustWriteFile(t, mounts, "proc /proc proc rw 0 0\n/dev/sda1 / xfs rw,relatime 0 0\n")

	return &Lister{SysBlockDir: sysBlock, ByIDDir: byID, ProcMounts: mounts}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink(%s -> %s): %v", link, target, err)
	}
}

func TestLister_List(t *testing.T) {
	l := newTestLister(t)

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: got %d disks, want 2 (sda, sdb; sda1 and loop0 excluded): %+v", len(got), got)
	}

	sda := got[0]
	if sda.Device != "/dev/sda" {
		t.Fatalf("List[0].Device = %q, want /dev/sda", sda.Device)
	}
	if sda.Size != 15628053168*512 {
		t.Errorf("sda.Size = %d, want %d", sda.Size, int64(15628053168)*512)
	}
	if sda.Model != "WDC WD80EFZX-68UW8N0" {
		t.Errorf("sda.Model = %q", sda.Model)
	}
	if sda.WWN != "0x5000cca0b1c2d3e4" {
		t.Errorf("sda.WWN = %q, want 0x5000cca0b1c2d3e4", sda.WWN)
	}
	if sda.Serial != "VGH0A1B2" {
		t.Errorf("sda.Serial = %q, want VGH0A1B2", sda.Serial)
	}
	if sda.WeakIdentity {
		t.Error("sda.WeakIdentity = true, want false (it has a wwn- link)")
	}
	if !sda.Boot {
		t.Error("sda.Boot = false, want true (its partition sda1 backs /)")
	}

	sdb := got[1]
	if sdb.Device != "/dev/sdb" {
		t.Fatalf("List[1].Device = %q, want /dev/sdb", sdb.Device)
	}
	if !sdb.WeakIdentity {
		t.Error("sdb.WeakIdentity = false, want true (usb- only)")
	}
	if sdb.Boot {
		t.Error("sdb.Boot = true, want false")
	}
}

func TestLister_List_ContextCancelled(t *testing.T) {
	l := newTestLister(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.List(ctx); err == nil {
		t.Fatal("List with a cancelled context: got nil error")
	}
}
