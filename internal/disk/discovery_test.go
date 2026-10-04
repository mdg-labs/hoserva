package disk

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestLooksLikeUnraidLayout(t *testing.T) {
	for name, tc := range map[string]struct {
		table string
		start int64
		fs    string
		want  bool
	}{
		"MBR, sector 64, XFS":             {"dos", 64, "xfs", true},
		"GPT, sector 64, XFS":             {"gpt", 64, "xfs", true},
		"GPT, sector 64, btrfs":           {"gpt", 64, "btrfs", true},
		"MBR, sector 64, ext4":            {"dos", 64, "ext4", true},
		"the usual 2048-sector alignment": {"gpt", 2048, "xfs", false},
		"MBR, sector 63":                  {"dos", 63, "xfs", false},
		"sector 64 holding NTFS":          {"dos", 64, "ntfs", false},
		"sector 64 holding ZFS":           {"gpt", 64, "zfs_member", false},
		"sector 64 with no filesystem":    {"gpt", 64, "", false},
		"no partition table":              {"", 64, "xfs", false},
		"no partition 1":                  {"gpt", 0, "xfs", false},
	} {
		if got := LooksLikeUnraidLayout(tc.table, tc.start, tc.fs); got != tc.want {
			t.Errorf("%s: LooksLikeUnraidLayout(%q, %d, %q) = %v, want %v", name, tc.table, tc.start, tc.fs, got, tc.want)
		}
	}
}

func internalBootPartitions() []PartitionEntry {
	return []PartitionEntry{
		{1, "BIOS Boot Partition", "21686148-6449-6e6f-744e-656564454649"},
		{2, "EFI System Partition", "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"},
		{3, "Unraid Boot Partition", "0fc63daf-8483-4772-8e79-3d69d8477de4"},
		{4, "", "0fc63daf-8483-4772-8e79-3d69d8477de4"},
	}
}

func TestIsUnraidInternalBoot(t *testing.T) {
	if !IsUnraidInternalBoot(internalBootPartitions()) {
		t.Fatal("the mkbootable layout is not recognised")
	}
	named := internalBootPartitions()
	named[3].Name = "cache data"
	if !IsUnraidInternalBoot(named) {
		t.Error("partition 4 may carry any name")
	}
	mutate := map[string]func([]PartitionEntry) []PartitionEntry{
		"partition 3 renamed":     func(p []PartitionEntry) []PartitionEntry { p[2].Name = "Boot"; return p },
		"partition 1 retyped":     func(p []PartitionEntry) []PartitionEntry { p[0].Type = p[3].Type; return p },
		"partition 4 retyped":     func(p []PartitionEntry) []PartitionEntry { p[3].Type = p[1].Type; return p },
		"no partition 4":          func(p []PartitionEntry) []PartitionEntry { return p[:3] },
		"a gap at partition 2":    func(p []PartitionEntry) []PartitionEntry { return append(p[:1], p[2:]...) },
		"an Unraid array disk":    func(p []PartitionEntry) []PartitionEntry { return []PartitionEntry{{1, "", p[3].Type}} },
		"no partitions at all":    func(p []PartitionEntry) []PartitionEntry { return nil },
		"a type GUID in capitals": nil,
	}
	delete(mutate, "a type GUID in capitals")
	for name, m := range mutate {
		if IsUnraidInternalBoot(m(internalBootPartitions())) {
			t.Errorf("%s: recognised as an Unraid internal boot device", name)
		}
	}
	upper := internalBootPartitions()
	upper[1].Type = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	if !IsUnraidInternalBoot(upper) {
		t.Error("udev's type GUIDs are compared without regard to case")
	}
}

// An internal boot device lists as a vfat disk labelled EFI, so the layout is
// read from the partition names and types in udev's database.
func TestLister_List_RecognisesAnUnraidInternalBootDevice(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda", "dev"), "8:0\n")
	mustWriteFile(t, filepath.Join(udev, "b8:0"), "I:1\nE:ID_PART_TABLE_TYPE=gpt\n")
	for i, p := range internalBootPartitions() {
		part := fmt.Sprintf("sda%d", i+1)
		mustMkdirAll(t, filepath.Join(l.SysBlockDir, part))
		mustWriteFile(t, filepath.Join(l.SysBlockDir, part, "dev"), fmt.Sprintf("8:%d\n", i+1))
		mustWriteFile(t, filepath.Join(l.SysBlockDir, part, "partition"), fmt.Sprintf("%d\n", i+1))
		body := fmt.Sprintf("I:1\nE:ID_PART_ENTRY_TYPE=%s\n", p.Type)
		if p.Name != "" {
			body += "E:ID_PART_ENTRY_NAME=" + strings.ReplaceAll(p.Name, " ", "\\x20") + "\n"
		}
		switch i {
		case 1:
			body += "E:ID_FS_TYPE=vfat\nE:ID_FS_LABEL=EFI\nE:ID_FS_UUID=ABCD-1234\n"
		case 2:
			body += "E:ID_FS_TYPE=zfs_member\nE:ID_FS_LABEL=flash\nE:ID_FS_UUID=123456789\n"
		}
		mustWriteFile(t, filepath.Join(udev, fmt.Sprintf("b8:%d", i+1)), body)
	}
	l.UdevDataDir = udev

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].Device != "/dev/sda" || !got[0].UnraidBoot || got[1].UnraidBoot {
		t.Fatalf("List = %+v, want only /dev/sda recognised as an Unraid internal boot device", got)
	}
	if got[0].LooksLikeUnraid {
		t.Error("an internal boot device has no partition at sector 64 holding XFS, btrfs or ext4")
	}

	mustWriteFile(t, filepath.Join(udev, "b8:3"), "I:1\nE:ID_PART_ENTRY_TYPE=0fc63daf-8483-4772-8e79-3d69d8477de4\nE:ID_PART_ENTRY_NAME=Boot\nE:ID_FS_TYPE=zfs_member\n")
	got, err = l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got[0].UnraidBoot {
		t.Error("a device whose partition 3 is named otherwise is classed as Unraid's boot device: its ZFS member would escape the ZFS refusal")
	}
}

func TestLister_List_ReadsUdevFilesystemWithoutOpeningDevice(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda", "dev"), "8:0\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda1", "dev"), "8:1\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda1", "partition"), "1\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda1", "start"), "64\n")
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sdb", "dev"), "8:16\n")
	mustWriteFile(t, filepath.Join(udev, "b8:0"), "I:3\nE:ID_PART_TABLE_TYPE=dos\n")
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
	if sda.FSDevice != "/dev/sda1" {
		t.Fatalf("sda.FSDevice = %q, want /dev/sda1: the filesystem is on the partition, so that is what a mount names", sda.FSDevice)
	}
	if !sda.ContainsData {
		t.Fatal("sda.ContainsData = false, want true")
	}
	if !sda.LooksLikeUnraid {
		t.Fatal("sda.LooksLikeUnraid = false, want true (partition 1 at sector 64 holding XFS)")
	}

	sdb := got[1]
	if sdb.Filesystem != "ext4" || sdb.Label != "backup" || sdb.FSUUID != "uuid-backup" {
		t.Fatalf("sdb filesystem/label/uuid = %q/%q/%q, want ext4/backup/uuid-backup", sdb.Filesystem, sdb.Label, sdb.FSUUID)
	}
	if sdb.LooksLikeUnraid {
		t.Fatal("sdb.LooksLikeUnraid = true, want false (no partition table)")
	}
	if sdb.FSDevice != "/dev/sdb" {
		t.Fatalf("sdb.FSDevice = %q, want /dev/sdb: the filesystem is on the whole disk", sdb.FSDevice)
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
	if sdc.FSDevice != "/dev/sdc2" {
		t.Fatalf("sdc.FSDevice = %q, want /dev/sdc2", sdc.FSDevice)
	}
}

// TestLister_List_FSDeviceIsEmptyWithoutAFilesystem: a disk udev knows no
// filesystem for names no node to mount, so a caller cannot fall back to the
// whole disk by reading it.
func TestLister_List_FSDeviceIsEmptyWithoutAFilesystem(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)
	l.UdevDataDir = udev

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, d := range got {
		if d.FSDevice != "" || d.Filesystem != "" {
			t.Errorf("%s: FSDevice %q, Filesystem %q, want neither with no udev data", d.Device, d.FSDevice, d.Filesystem)
		}
	}
}
