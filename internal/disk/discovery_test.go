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

// TestLister_List_FilesystemOnLaterPartition covers a Windows GPT layout:
// partition 1 is an unformatted Microsoft Reserved partition, partition 2
// holds NTFS. Discovery must still report the NTFS filesystem and
// ContainsData so add/replace/setup plans warn before erase (doc 02 §4).
func TestLister_List_FilesystemOnLaterPartition(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)

	// sdc: whole disk with no filesystem of its own; sdc1 empty (MSR);
	// sdc2 carries NTFS. By-id link so List can resolve identity.
	mustMkdirAll(t, filepath.Join(l.SysBlockDir, "sdc", "device"))
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc", "size"), "1953525168\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc", "device", "model"), "Windows GPT Disk\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc", "dev"), "8:32\n")
	mustMkdirAll(t, filepath.Join(l.SysBlockDir, "sdc1"))
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc1", "size"), "32768\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc1", "partition"), "1\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc1", "dev"), "8:33\n")
	mustMkdirAll(t, filepath.Join(l.SysBlockDir, "sdc2"))
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc2", "size"), "1953492032\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc2", "partition"), "2\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdc2", "dev"), "8:34\n")
	mustSymlink(t, "../../sdc", filepath.Join(l.ByIDDir, "ata-Windows_GPT_Disk_WIN001"))

	// Whole disk and partition 1 have no ID_FS_*; only partition 2 does.
	mustWriteFile(t, filepath.Join(udev, "b8:34"), "I:3\nE:ID_FS_TYPE=ntfs\nE:ID_FS_LABEL=Data\nE:ID_FS_UUID=uuid-ntfs\n")
	l.UdevDataDir = udev

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sdc *Disk
	for i := range got {
		if got[i].Device == "/dev/sdc" {
			sdc = &got[i]
			break
		}
	}
	if sdc == nil {
		t.Fatalf("List: /dev/sdc missing: %+v", got)
	}
	if sdc.Filesystem != "ntfs" || sdc.Label != "Data" || sdc.FSUUID != "uuid-ntfs" {
		t.Fatalf("sdc filesystem/label/uuid = %q/%q/%q, want ntfs/Data/uuid-ntfs (from partition 2)", sdc.Filesystem, sdc.Label, sdc.FSUUID)
	}
	if !sdc.ContainsData {
		t.Fatal("sdc.ContainsData = false, want true (NTFS on partition 2)")
	}
}
