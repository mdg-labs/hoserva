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

// bootSysBlock builds a synthetic /sys/class/block tree under t.TempDir()
// and returns its root, for exercising BootDevices' sysfs walk without
// touching a real system (CLAUDE.md: real disks are off-limits).
func bootSysBlock(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustMkdirAll(t, dir)
	return dir
}

func TestBootDevices_ResolvesRootPartitionToWholeDisk(t *testing.T) {
	sysBlock := bootSysBlock(t)
	mustMkdirAll(t, filepath.Join(sysBlock, "nvme0n1p2"))
	mustWriteFile(t, filepath.Join(sysBlock, "nvme0n1p2", "partition"), "2\n")

	mounts := []MountEntry{
		{Device: "proc", MountPoint: "/proc", FSType: "proc"},
		{Device: "/dev/nvme0n1p2", MountPoint: "/", FSType: "ext4"},
		{Device: "/dev/nvme0n1p1", MountPoint: "/boot/efi", FSType: "vfat"},
	}
	got, err := BootDevices(mounts, sysBlock)
	if err != nil {
		t.Fatalf("BootDevices: %v", err)
	}
	if len(got) != 1 || got[0] != "/dev/nvme0n1" {
		t.Fatalf("BootDevices: got %v, want [/dev/nvme0n1]", got)
	}
}

func TestBootDevices_NoRootMount(t *testing.T) {
	mounts := []MountEntry{
		{Device: "proc", MountPoint: "/proc", FSType: "proc"},
	}
	got, err := BootDevices(mounts, bootSysBlock(t))
	if err != nil {
		t.Fatalf("BootDevices: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BootDevices: got %v, want none — no root mount present", got)
	}
}

// TestBootDevices_ResolvesDeviceMapperRootToItsPhysicalSlave is the
// device-mapper half of this issue's own concern: a root mount on an LVM
// or LUKS volume names a virtual dm-N device, resolved here by its own
// sysfs "dm/name" file (never a /dev/mapper symlink read, so this stays
// fixture-testable) down to the real physical disk backing it.
func TestBootDevices_ResolvesDeviceMapperRootToItsPhysicalSlave(t *testing.T) {
	sysBlock := bootSysBlock(t)
	mustMkdirAll(t, filepath.Join(sysBlock, "dm-0", "dm"))
	mustWriteFile(t, filepath.Join(sysBlock, "dm-0", "dm", "name"), "vg-root\n")
	mustMkdirAll(t, filepath.Join(sysBlock, "dm-0", "slaves"))
	mustSymlink(t, "../../sda1", filepath.Join(sysBlock, "dm-0", "slaves", "sda1"))
	mustMkdirAll(t, filepath.Join(sysBlock, "sda1"))
	mustWriteFile(t, filepath.Join(sysBlock, "sda1", "partition"), "1\n")

	mounts := []MountEntry{
		{Device: "/dev/mapper/vg-root", MountPoint: "/", FSType: "ext4"},
	}
	got, err := BootDevices(mounts, sysBlock)
	if err != nil {
		t.Fatalf("BootDevices: %v", err)
	}
	if len(got) != 1 || got[0] != "/dev/sda" {
		t.Fatalf("BootDevices: got %v, want [/dev/sda]", got)
	}
}

// TestBootDevices_ResolvesMDRaidRootToEveryPhysicalMember proves a
// virtual root backed by more than one physical disk (a software-RAID
// root) reports every one of them, not just one — the exact gap this
// issue calls out against a single-string return.
func TestBootDevices_ResolvesMDRaidRootToEveryPhysicalMember(t *testing.T) {
	sysBlock := bootSysBlock(t)
	mustMkdirAll(t, filepath.Join(sysBlock, "md0", "slaves"))
	mustSymlink(t, "../../sda1", filepath.Join(sysBlock, "md0", "slaves", "sda1"))
	mustSymlink(t, "../../sdb1", filepath.Join(sysBlock, "md0", "slaves", "sdb1"))
	mustMkdirAll(t, filepath.Join(sysBlock, "sda1"))
	mustWriteFile(t, filepath.Join(sysBlock, "sda1", "partition"), "1\n")
	mustMkdirAll(t, filepath.Join(sysBlock, "sdb1"))
	mustWriteFile(t, filepath.Join(sysBlock, "sdb1", "partition"), "1\n")

	mounts := []MountEntry{
		{Device: "/dev/md0", MountPoint: "/", FSType: "ext4"},
	}
	got, err := BootDevices(mounts, sysBlock)
	if err != nil {
		t.Fatalf("BootDevices: %v", err)
	}
	want := []string{"/dev/sda", "/dev/sdb"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("BootDevices: got %v, want %v", got, want)
	}
}

// TestBootDevices_FailsClosedOnUnresolvableRoot is the safety property
// this issue exists for: a root device BootDevices cannot resolve to any
// real physical disk under sysBlockDir (the kernel's "/dev/root" alias,
// unhandled here on purpose — see boot.go) must be reported as an error,
// never silently treated as "no boot device", which would let a
// destructive caller downstream format the real boot disk.
func TestBootDevices_FailsClosedOnUnresolvableRoot(t *testing.T) {
	mounts := []MountEntry{
		{Device: "/dev/root", MountPoint: "/", FSType: "ext4"},
	}
	if _, err := BootDevices(mounts, bootSysBlock(t)); err == nil {
		t.Fatal("BootDevices: expected an error for an unresolvable root device, got nil")
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
