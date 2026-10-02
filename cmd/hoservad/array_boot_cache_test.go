package main

import (
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestNewArraySequence_CacheOnBootDiskPartitionIsNotAWrongFilesystem is
// the gate's side of a cache on a boot-disk partition: the cache row
// carries the boot disk's own identity, and the boot disk's udev-cached
// filesystem is its first partition's (here the EFI system partition), not
// the cache partition's. Comparing the cache partition's recorded UUID
// against that would report the slot wrong_filesystem and keep the array
// from ever starting, so the gate compares identity only for a partition.
func TestNewArraySequence_CacheOnBootDiskPartitionIsNotAWrongFilesystem(t *testing.T) {
	ctx, h, arrays, shares, provider, runner := newArrayTestEnv(t)
	disks := sampleArrayDisks()
	disks = append(disks, store.ArrayDisk{
		Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme0n1p3", Filesystem: "ext4", FSUUID: "uuid-cache",
		Serial: "S4EWNX0M123456X", ByIDName: "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X-part3", Mountpoint: "/mnt/cache",
	})
	if err := arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	for _, d := range disks {
		if d.Role == store.ArrayRoleCache {
			continue
		}
		provider.AddDisk(d.Device, disk.Disk{WWN: d.WWN, Serial: d.Serial, ByIDName: d.ByIDName, FSUUID: d.FSUUID})
	}
	provider.AddDisk("/dev/nvme0n1", disk.Disk{
		Boot: true, Serial: "S4EWNX0M123456X", ByIDName: "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X", FSUUID: "ABCD-1234",
	})

	seq, err := newArraySequence(ctx, h.Scheduler, arrays, shares, provider, runner, nil)
	if err != nil {
		t.Fatalf("newArraySequence: %v", err)
	}
	gate, ok := storageGateOf(seq.Gate)
	if !ok {
		t.Fatal("seq.Gate is not the expected wrapper")
	}
	if !gate.Ready() {
		t.Fatalf("gate not ready: missing %+v, wrong filesystem %+v", gate.Missing(), gate.WrongFilesystem())
	}
}
