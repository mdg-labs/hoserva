package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestMockGetPool_MountedFollowsTheArrayStopAndStart(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	poolMounted := func() bool {
		t.Helper()
		pool, err := h.GetPool(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return pool.Mounted
	}

	if !poolMounted() {
		t.Fatal("GET /pool with the array running: mounted = false, want true")
	}
	if _, err := h.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if poolMounted() {
		t.Fatal("GET /pool with the array stopped: mounted = true, want false")
	}
	if res := testDestination(t, h, "pool"); res.Success {
		t.Fatal("backup connection test of the pool with the array stopped succeeded, want the not-mounted refusal")
	}
	if _, err := h.StartArray(ctx); err != nil {
		t.Fatal(err)
	}
	if !poolMounted() {
		t.Fatal("GET /pool after the array restarted: mounted = false, want true")
	}
}

func TestMockGetPool_FreshInstallReportsTheUnmountedPool(t *testing.T) {
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := h.GetPool(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pool.Mounted {
		t.Fatal("GET /pool in the fresh-install scenario: mounted = true, want false")
	}
}

func mockCreateArrayRequest(cacheDevice string, cacheRole apiv1.ArrayDiskRole) *apiv1.CreateArrayRequest {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sdc", Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
	}
	disks := []apiv1.ArrayDiskAssignment{
		{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleParity},
		{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleData},
		{Device: cacheDevice, Role: cacheRole},
	}
	switch cacheRole {
	case apiv1.ArrayDiskRoleCache:
		plan.Cache = &disk.AssignedDisk{Device: cacheDevice, Filesystem: disk.XFS}
	case apiv1.ArrayDiskRoleData:
		plan.Data = append(plan.Data, disk.AssignedDisk{Device: cacheDevice, Filesystem: disk.XFS})
	default:
		plan.Parity = append(plan.Parity, disk.AssignedDisk{Device: cacheDevice, Filesystem: disk.XFS})
	}
	return &apiv1.CreateArrayRequest{Disks: disks, Confirmation: plan.Confirmation()}
}

// TestMockSharedNVMe_FreshInstallOffersTheSparePartitionAsACache is the
// shared-NVMe scenario (doc 01 §6): the inventory lists the boot NVMe with
// its one spare partition, the mock accepts it for the cache role only, and
// refuses it the way production's handler does for data and parity.
func TestMockSharedNVMe_FreshInstallOffersTheSparePartitionAsACache(t *testing.T) {
	h, err := newHandler("fresh-install")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	listed, err := h.ListDisks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var boot *apiv1.DiskInventoryEntry
	for i := range listed.Disks {
		if listed.Disks[i].Boot {
			boot = &listed.Disks[i]
		}
	}
	if boot == nil || len(boot.CachePartitions) != 1 || boot.CachePartitions[0].Device != "/dev/nvme0n1p3" {
		t.Fatalf("fresh-install boot disk = %+v, want one cache partition /dev/nvme0n1p3", boot)
	}

	if _, err := h.CreateArray(ctx, mockCreateArrayRequest("/dev/nvme0n1p3", apiv1.ArrayDiskRoleCache)); err != nil {
		t.Fatalf("CreateArray(cache on the spare partition): %v", err)
	}
	for _, role := range []apiv1.ArrayDiskRole{apiv1.ArrayDiskRoleData, apiv1.ArrayDiskRoleParity} {
		_, err := h.CreateArray(ctx, mockCreateArrayRequest("/dev/nvme0n1p3", role))
		var me *mockError
		if !errors.As(err, &me) || me.code != "boot_partition_cache_only" || me.statusCode != 400 {
			t.Fatalf("CreateArray(%s on the spare partition) = %v, want 400 boot_partition_cache_only", role, err)
		}
	}
	for _, role := range []apiv1.ArrayDiskRole{apiv1.ArrayDiskRoleData, apiv1.ArrayDiskRoleCache} {
		_, err := h.CreateArray(ctx, mockCreateArrayRequest("/dev/nvme0n1", role))
		var me *mockError
		if !errors.As(err, &me) || me.code != "invalid_plan" || me.statusCode != 400 || !strings.Contains(me.message, "refusing to assign the boot device") {
			t.Fatalf("CreateArray(%s on the whole boot disk) = %v, want 400 invalid_plan refusing the boot device", role, err)
		}
	}
	_, err = h.CreateArray(ctx, mockCreateArrayRequest("/dev/nvme0n1p2", apiv1.ArrayDiskRoleCache))
	var me *mockError
	if !errors.As(err, &me) || me.code != "unmanaged_device" {
		t.Fatalf("CreateArray(cache on the root partition) = %v, want unmanaged_device", err)
	}
}
