package disk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWholeDiskDevice(t *testing.T) {
	cases := map[string]string{
		"/dev/sda":        "/dev/sda",
		"/dev/sda2":       "/dev/sda",
		"/dev/vda1":       "/dev/vda",
		"/dev/nvme0n1":    "/dev/nvme0n1",
		"/dev/nvme0n1p2":  "/dev/nvme0n1",
		"/dev/mmcblk0":    "/dev/mmcblk0",
		"/dev/mmcblk0p1":  "/dev/mmcblk0",
		"/dev/nvme10n1p1": "/dev/nvme10n1",
	}
	for in, want := range cases {
		if got := WholeDiskDevice(in); got != want {
			t.Errorf("WholeDiskDevice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBootDevice_ResolvesRootPartitionToWholeDisk(t *testing.T) {
	mounts := []MountEntry{
		{Device: "proc", MountPoint: "/proc", FSType: "proc"},
		{Device: "/dev/nvme0n1p2", MountPoint: "/", FSType: "ext4"},
		{Device: "/dev/nvme0n1p1", MountPoint: "/boot/efi", FSType: "vfat"},
	}
	got, ok := BootDevice(mounts)
	if !ok {
		t.Fatal("BootDevice: no boot device found")
	}
	if got != "/dev/nvme0n1" {
		t.Fatalf("BootDevice: got %q, want /dev/nvme0n1", got)
	}
}

func TestBootDevice_NoRootMount(t *testing.T) {
	mounts := []MountEntry{
		{Device: "proc", MountPoint: "/proc", FSType: "proc"},
	}
	if _, ok := BootDevice(mounts); ok {
		t.Fatal("BootDevice: reported a boot device with no root mount present")
	}
}

func TestReadProcMounts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mounts")
	content := "proc /proc proc rw,nosuid,nodev,noexec 0 0\n/dev/sda2 / xfs rw,relatime 0 0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := ReadProcMounts(path)
	if err != nil {
		t.Fatalf("ReadProcMounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadProcMounts: got %d entries, want 2: %+v", len(got), got)
	}
	if got[1].Device != "/dev/sda2" || got[1].MountPoint != "/" || got[1].FSType != "xfs" {
		t.Fatalf("ReadProcMounts: got %+v", got[1])
	}
}
