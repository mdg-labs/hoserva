package disk

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLooksLikeUnraidLabel(t *testing.T) {
	yes := []string{"disk1", "disk12", "DISK1", "parity", "parity2", "cache", "cache2", "Parity"}
	for _, label := range yes {
		if !LooksLikeUnraidLabel(label) {
			t.Errorf("LooksLikeUnraidLabel(%q) = false, want true", label)
		}
	}
	no := []string{"", "media", "data", "parity-2", "disk", "unraid", "sda"}
	for _, label := range no {
		if LooksLikeUnraidLabel(label) {
			t.Errorf("LooksLikeUnraidLabel(%q) = true, want false", label)
		}
	}
}

func TestLister_List_ReadsUdevFilesystemWithoutOpeningDevice(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda", "dev"), "8:0\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda1", "dev"), "8:1\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdb", "dev"), "8:16\n")
	mustWriteFile(t, filepath.Join(udev, "b8:1"), "I:1\nE:ID_FS_TYPE=xfs\nE:ID_FS_LABEL=disk1\nE:ID_FS_UUID=uuid-disk1\n")
	mustWriteFile(t, filepath.Join(udev, "b8:16"), "I:2\nE:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=backup\nE:ID_FS_UUID=uuid-backup\n")
	l.UdevDataDir = udev

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List: got %d disks, want 2", len(got))
	}

	sda := got[0]
	if sda.Filesystem != "xfs" || sda.Label != "disk1" || sda.FSUUID != "uuid-disk1" {
		t.Fatalf("sda filesystem/label/uuid = %q/%q/%q, want xfs/disk1/uuid-disk1 (from partition udev data)", sda.Filesystem, sda.Label, sda.FSUUID)
	}
	if !sda.ContainsData {
		t.Fatal("sda.ContainsData = false, want true")
	}
	if !sda.LooksLikeUnraid {
		t.Fatal("sda.LooksLikeUnraid = false, want true (label disk1)")
	}

	sdb := got[1]
	if sdb.Filesystem != "ext4" || sdb.Label != "backup" || sdb.FSUUID != "uuid-backup" {
		t.Fatalf("sdb filesystem/label/uuid = %q/%q/%q, want ext4/backup/uuid-backup", sdb.Filesystem, sdb.Label, sdb.FSUUID)
	}
	if sdb.LooksLikeUnraid {
		t.Fatal("sdb.LooksLikeUnraid = true, want false (label backup)")
	}
}
