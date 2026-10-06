package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/web/fixtures"
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

// mockPoolMembers is the entries of a getPool answer that hold an array role.
func mockPoolMembers(pool *apiv1.PoolStatus) []apiv1.PoolDiskEntry {
	var out []apiv1.PoolDiskEntry
	for _, e := range pool.Disks {
		if e.Role != apiv1.PoolDiskEntryRoleUnassigned {
			out = append(out, e)
		}
	}
	return out
}

// Production lists every present disk that is not an array member as
// unassigned, whether or not an array exists yet, and never the boot disk: a
// fresh install offers its two data disks, the USB disk and the Unraid stick
// (flagged, so the pool page's pickers leave it out).
func TestMockGetPool_FreshInstallListsTheDisksAsUnassigned(t *testing.T) {
	for _, scenario := range []string{"fresh-install", "migration-pending"} {
		t.Run(scenario, func(t *testing.T) {
			h, err := newHandler(scenario)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := h.GetPool(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := mockPoolMembers(pool); len(got) != 0 {
				t.Fatalf("members with no array = %+v", got)
			}
			listed := map[string]apiv1.PoolDiskEntry{}
			for _, e := range pool.Disks {
				listed[e.Device] = e
			}
			for _, want := range []string{"/dev/sdb", "/dev/sdc", mockExternalDevice, mockFlashDevice} {
				if _, ok := listed[want]; !ok {
					t.Errorf("%s is not listed as unassigned: %+v", want, pool.Disks)
				}
			}
			if _, ok := listed["/dev/nvme0n1"]; ok {
				t.Error("the boot disk is listed")
			}
			if !listed[mockFlashDevice].UnraidStick.Or(false) {
				t.Errorf("the stick %+v is not flagged unraidStick", listed[mockFlashDevice])
			}
			if listed["/dev/sdc"].UnraidStick.IsSet() {
				t.Errorf("a data disk carries unraidStick: %+v", listed["/dev/sdc"])
			}
		})
	}
}

// A disk is either an array member or unassigned, never both: an inventory disk
// its stored array matches is listed as a member, and every other non-boot one
// as unassigned.
func TestMockGetPool_UnassignedDisksAreTheInventoryOutsideTheArray(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range fixtures.Scenarios {
		if scenario == "migration-pending" {
			continue
		}
		t.Run(scenario, func(t *testing.T) {
			h, err := newHandler(scenario)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := h.GetPool(ctx)
			if err != nil {
				t.Fatal(err)
			}
			members := map[string]bool{}
			for _, e := range mockPoolMembers(pool) {
				members[e.Device] = true
			}
			unassigned := map[string]bool{}
			for _, e := range pool.Disks {
				if e.Role != apiv1.PoolDiskEntryRoleUnassigned {
					continue
				}
				if members[e.Device] || unassigned[e.Device] {
					t.Errorf("%s is listed twice", e.Device)
				}
				unassigned[e.Device] = true
			}
			stored := map[string]bool{}
			for _, d := range mockArrayDisks(scenario) {
				stored[d.Serial] = true
			}
			for _, d := range mockDiskInventory(scenario) {
				switch {
				case d.Boot:
					if unassigned[d.Device] || members[d.Device] {
						t.Errorf("the boot disk %s is listed", d.Device)
					}
				case stored[d.Serial.Or("")]:
					if !members[d.Device] || unassigned[d.Device] {
						t.Errorf("%s is in the array but is not listed as its member", d.Device)
					}
				default:
					if !unassigned[d.Device] {
						t.Errorf("%s is outside the array but is not listed as unassigned", d.Device)
					}
				}
			}
		})
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

func TestMockStartScrub_AcceptsAllBlocksAndRefusesWhatProductionRefuses(t *testing.T) {
	ctx := context.Background()
	req := &apiv1.StartScrubRequest{Percent: apiv1.NewOptInt32(100), AllBlocks: apiv1.NewOptBool(true)}

	h, client := newTestHandlerClient(t, "healthy")
	j, err := client.StartScrub(ctx, req)
	if err != nil {
		t.Fatalf("StartScrub with allBlocks: %v", err)
	}
	if j.Type != apiv1.JobTypeScrub || j.Class != apiv1.JobClassParity {
		t.Fatalf("job = %+v, want a parity-class scrub", j)
	}

	h.migration.imported.Store(true)
	if _, err := client.StartScrub(ctx, req); errorCode(t, err) != "migration_in_progress" {
		t.Fatalf("StartScrub with allBlocks while the migration is pending = %v, want migration_in_progress", err)
	}
	h.migration.imported.Store(false)

	if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if _, err := client.StartScrub(ctx, req); errorCode(t, err) != "maintenance_mode" {
		t.Fatalf("StartScrub with allBlocks in maintenance mode = %v, want maintenance_mode", err)
	}
}

// A cache disk is one fact: wherever a scenario reports the array, the cache
// disk is in every report or in none — the stored array, the pool page and
// the disk inventory — and healthy alone has one.
func TestMockCacheDiskIsConsistentAcrossReports(t *testing.T) {
	scenarios := []string{"healthy", "degraded", "rebuilding", "sync-blocked", "fresh-install", "migration-pending"}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			stored := false
			for _, d := range mockArrayDisks(scenario) {
				if d.Role == store.ArrayRoleCache {
					stored = true
					if d.Mountpoint != "/mnt/cache" || d.Device != mockCacheDevice {
						t.Errorf("stored cache disk = %+v, want %s at /mnt/cache", d, mockCacheDevice)
					}
				}
			}
			pooled := false
			for _, d := range mockPoolStatus(scenario).Disks {
				if d.Role == apiv1.PoolDiskEntryRoleCache {
					pooled = true
					if d.MountPoint != "/mnt/cache" || d.Device != mockCacheDevice {
						t.Errorf("pool cache disk = %+v, want %s at /mnt/cache", d, mockCacheDevice)
					}
				}
			}
			listed := false
			for _, d := range mockDiskInventory(scenario) {
				if d.Device == mockCacheDevice {
					listed = true
					if d.Serial.Or("") != mockCacheSerial {
						t.Errorf("inventory serial = %q, want %q", d.Serial.Or(""), mockCacheSerial)
					}
				}
			}
			want := scenario == "healthy"
			if stored != want || pooled != want || listed != want {
				t.Fatalf("cache disk in stored array = %v, pool = %v, inventory = %v; want all %v", stored, pooled, listed, want)
			}
		})
	}
}

// Real device paths are unique, and the disks page keys its rows by device:
// no scenario's inventory may list one path twice, and the USB disk must be
// the one the external-disk endpoints report.
func TestMockDiskInventory_DevicePathsAreUnique(t *testing.T) {
	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario, func(t *testing.T) {
			seen := map[string]string{}
			for _, d := range mockDiskInventory(scenario) {
				if prev, dup := seen[d.Device]; dup {
					t.Errorf("%s listed twice: serial %q and %q", d.Device, prev, d.Serial.Or(""))
				}
				seen[d.Device] = d.Serial.Or("")
			}
			if _, ok := seen[mockExternalDevice]; !ok {
				t.Errorf("the USB disk %s is not in the inventory", mockExternalDevice)
			}
		})
	}
}

// The stick GetMigration offers to read from must be in the disk list, as
// production lists any attached Unraid stick: a vfat filesystem labelled
// UNRAID, not the boot disk and not in the array.
func TestMockDiskInventory_ListsTheOfferedUnraidStick(t *testing.T) {
	ctx := context.Background()
	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario, func(t *testing.T) {
			h, err := newHandler(scenario)
			if err != nil {
				t.Fatal(err)
			}
			m, err := h.GetMigration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.FlashDevices) != 1 {
				t.Fatalf("GetMigration offers %+v, want the one stick", m.FlashDevices)
			}
			offered := m.FlashDevices[0]
			listed, err := h.ListDisks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var stick *apiv1.DiskInventoryEntry
			for i := range listed.Disks {
				if listed.Disks[i].Device == offered.Device {
					stick = &listed.Disks[i]
				}
			}
			if stick == nil {
				t.Fatalf("ListDisks does not list %s, which GetMigration offers", offered.Device)
			}
			if stick.Boot || stick.Filesystem.Or("") != "vfat" || stick.Label.Or("") != "UNRAID" ||
				stick.SizeBytes != offered.Size || stick.Model != offered.Model || stick.Serial != offered.Serial {
				t.Errorf("listed stick = %+v, want vfat UNRAID, not boot, matching the offer %+v", *stick, offered)
			}
			if !stick.UnraidStick.Or(false) {
				t.Errorf("listed stick has unraidStick %v, want true", stick.UnraidStick)
			}
			if !stick.ContainsData.Or(false) {
				t.Errorf("listed stick has containsData %v, want true: production reports it for any disk with a filesystem", stick.ContainsData)
			}
			for _, d := range mockArrayDisks(scenario) {
				if d.Device == offered.Device {
					t.Errorf("the stick %s is also in the array", offered.Device)
				}
			}

			prod := newContractProductionHandler(t, scenario)
			got, err := prod.ListDisks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range got.Disks {
				if d.Device != offered.Device {
					continue
				}
				if d.Boot != stick.Boot || d.SizeBytes != stick.SizeBytes || d.Model != stick.Model || d.Serial != stick.Serial ||
					d.Filesystem != stick.Filesystem || d.Label != stick.Label || d.UnraidStick != stick.UnraidStick {
					t.Errorf("production lists the stick as %+v, the mock as %+v", d, *stick)
				}
				return
			}
			t.Errorf("production does not list %s", offered.Device)
		})
	}
}
