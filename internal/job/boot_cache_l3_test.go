//go:build l3

// This file runs only inside the L3 VM harness (doc 06 §4), never on the
// host or in the loop-device lab. scripts/vm/boot-cache-check.sh builds it
// with `go test -tags l3 -c` on the host (compiling touches no device),
// boots a VM in the shared-nvme topology (create-vm.sh: the OS disk
// carries an unused second partition and there is no cache disk), copies
// the binary in and runs it there with sudo.
//
// It creates an array through the real disk_format job — real
// LinuxProvider, real blkid probe, real mkfs, real systemd mount units
// under /etc — with the cache on the OS disk's spare partition, and checks
// that the root filesystem and the OS disk's partition table are exactly
// as they were.

package job

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func l3Output(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func TestL3BootCache_ArrayWithTheCacheOnTheBootDisksSparePartition(t *testing.T) {
	requireL3(t)
	requireRoot(t)
	ctx := context.Background()
	r := disk.CommandRunner{}
	provider := disk.NewLinuxProvider()

	listed, err := provider.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var boot disk.Disk
	for _, d := range listed {
		if d.Boot {
			boot = d
		}
	}
	if boot.Device == "" {
		t.Fatalf("no boot disk in %+v", listed)
	}
	if len(boot.CachePartitions) != 1 {
		t.Fatalf("boot disk %s reports cache partitions %+v, want exactly the spare partition create-vm.sh's shared-nvme topology adds", boot.Device, boot.CachePartitions)
	}
	spare := boot.CachePartitions[0]

	byIDPrefix := func(prefix string) disk.Disk {
		for _, d := range listed {
			if !d.Boot && strings.HasPrefix(d.ByIDName, prefix) {
				return d
			}
		}
		t.Fatalf("no array disk with a by-id name starting %q in %+v", prefix, listed)
		return disk.Disk{}
	}
	assigned := func(d disk.Disk, fs disk.FilesystemType) disk.AssignedDisk {
		return disk.AssignedDisk{
			Device: d.Device, Filesystem: fs, WWN: d.WWN, Serial: d.Serial, WeakIdentity: d.WeakIdentity, ByIDName: d.ByIDName,
		}
	}
	parity, data1, data2 := byIDPrefix("virtio-parity1-"), byIDPrefix("virtio-disk1-"), byIDPrefix("virtio-disk2-")
	cache := disk.AssignedDisk{
		Device: spare.Device, Filesystem: disk.XFS, WWN: boot.WWN, Serial: boot.Serial, WeakIdentity: boot.WeakIdentity,
		ByIDName: spare.ByIDName, PartUUID: spare.PartUUID,
	}
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{assigned(parity, disk.XFS)},
		Data:   []disk.AssignedDisk{assigned(data1, disk.XFS), assigned(data2, disk.XFS)},
		Cache:  &cache,
	}
	sizes := map[string]int64{parity.Device: parity.Size, data1.Device: data1.Size, data2.Device: data2.Size, spare.Device: spare.Size}

	rootSource := l3Output(t, "findmnt", "-n", "-o", "SOURCE", "/")
	tableBefore := l3Output(t, "sfdisk", "-d", boot.Device)
	const marker = "/root/hoserva-l3-bootcache-marker"
	if err := os.WriteFile(marker, []byte("root filesystem content"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", marker, err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })

	s := newTestScheduler(t)
	arrays := store.NewArrayStore(newTestDB(t))
	mounter := disk.GuardedMounter{Mounter: disk.SystemdMounter{Runner: r}, Runner: r}
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(DiskFormatDeps{
		Provider: provider, Runner: r, Store: arrays, Generator: config.NewGenerator("/etc"), Mounter: mounter,
	}))
	for _, where := range []string{"/mnt/cache", "/mnt/disk1", "/mnt/disk2", "/mnt/parity1"} {
		where := where
		t.Cleanup(func() { _, _ = r.Run(context.Background(), "systemctl", "stop", disk.UnitFileName(where)) })
	}

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, DiskFormatParams{
		Confirmation: plan.Confirmation(), Parity: plan.Parity, Data: plan.Data, Cache: plan.Cache, Sizes: sizes,
	}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	awaitCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	finished, err := s.Await(awaitCtx, j.ID)
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != StatusSucceeded {
		t.Fatalf("disk_format job status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	// The cache is the spare partition, mounted at /mnt/cache.
	if got := l3Output(t, "findmnt", "-n", "-o", "SOURCE", "/mnt/cache"); got != spare.Device {
		t.Fatalf("/mnt/cache is backed by %q, want the spare partition %s", got, spare.Device)
	}
	if err := os.WriteFile("/mnt/cache/appdata-probe", []byte("cache content"), 0o644); err != nil {
		t.Fatalf("writing to the mounted cache: %v", err)
	}
	if got := l3Output(t, "blkid", "-s", "TYPE", "-o", "value", spare.Device); got != "xfs" {
		t.Fatalf("spare partition filesystem = %q, want xfs", got)
	}

	// The root filesystem and the OS disk's partition table are untouched.
	if got := l3Output(t, "findmnt", "-n", "-o", "SOURCE", "/"); got != rootSource {
		t.Fatalf("root is now %q, was %q", got, rootSource)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "root filesystem content" {
		t.Fatalf("root marker = %q, %v", got, err)
	}
	if got := l3Output(t, "sfdisk", "-d", boot.Device); got != tableBefore {
		t.Fatalf("the OS disk's partition table changed:\nbefore:\n%s\nafter:\n%s", tableBefore, got)
	}
	if out, _ := exec.Command("blkid", "-p", "-s", "TYPE", "-o", "value", boot.Device).Output(); strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the whole OS disk %s now carries a filesystem signature %q", boot.Device, out)
	}

	// snapraid.conf keeps the three content copies on three distinct
	// devices: the boot copy and the two data disks, none on the cache.
	conf := l3MustReadFile(t, "/etc/snapraid.conf")
	for _, line := range []string{"content /var/lib/hoserva/snapraid.content\n", "content /mnt/disk1/snapraid.content\n", "content /mnt/disk2/snapraid.content\n"} {
		if !strings.Contains(conf, line) {
			t.Fatalf("snapraid.conf missing %q:\n%s", line, conf)
		}
	}
	if strings.Contains(conf, "/mnt/cache") {
		t.Fatalf("snapraid.conf places a content copy on the cache, which shares the boot disk:\n%s", conf)
	}

	// The immutable-mountpoint guard (doc 02 §1) covers /mnt/cache: once
	// unmounted, the directory on the root filesystem is empty and immutable.
	if _, err := r.Run(ctx, "systemctl", "stop", disk.UnitFileName("/mnt/cache")); err != nil {
		t.Fatalf("stopping the cache mount unit: %v", err)
	}
	if fields := strings.Fields(l3Output(t, "lsattr", "-d", "/mnt/cache")); len(fields) < 2 || !strings.Contains(fields[0], "i") {
		t.Fatalf("lsattr -d /mnt/cache = %v, want the immutable flag on the unmounted mountpoint", fields)
	}
	if err := os.WriteFile("/mnt/cache/stray", []byte("x"), 0o644); err == nil {
		t.Fatal("a file was written into the unmounted /mnt/cache, i.e. onto the boot disk's root filesystem")
	}
}
