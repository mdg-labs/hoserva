package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

const (
	nvmeBoot       = "/dev/nvme0n1"
	nvmeRootPart   = "/dev/nvme0n1p2"
	nvmeSparePart  = "/dev/nvme0n1p3"
	nvmeByIDPrefix = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	nvmeSpareByID  = nvmeByIDPrefix + "-part3"
	nvmeSparePath  = "/dev/disk/by-id/" + nvmeSpareByID
)

// newSharedNVMeHandler is the layout this issue adds: a boot NVMe with a
// spare partition, a parity disk and two data disks, behind the real
// RunDiskFormat job.
func newSharedNVMeHandler(t *testing.T, probe disk.BlankProber) (*api.Handler, *job.Scheduler, *disk.FakeProvider) {
	t.Helper()
	h, s, r := newTestHandler(t)
	p := disk.NewFakeProvider()
	p.AddDisk(nvmeBoot, disk.Disk{
		Size: 1 * disk.TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: nvmeByIDPrefix,
		CachePartitions: []disk.CachePartition{{
			Device: nvmeSparePart, Size: 900 * disk.GB, ByIDName: nvmeSpareByID, PartUUID: "5b3d9e0a-03",
			Reason: disk.ReasonSpareBootPartition,
		}},
	})
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	h.Disks = p

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "array.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening array test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	migrator := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := migrator.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	fakeRun := disk.NewFakeRunner()
	for dev, uuid := range map[string]string{"/dev/sda": "uuid-sda", "/dev/sdb": "uuid-sdb", "/dev/sdc": "uuid-sdc", nvmeSparePath: "uuid-spare"} {
		fakeRun.Script("blkid", []string{"-s", "UUID", "-o", "value", dev}, []byte(uuid+"\n"), nil)
	}
	r.Register(job.TypeDiskFormat, false, job.RunDiskFormat(job.DiskFormatDeps{
		Provider: p, Runner: fakeRun, Probe: probe, Store: store.NewArrayStore(db),
		Generator: config.NewGenerator(t.TempDir()), Mounter: disk.NewFakeMounter(),
		Now: func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) },
	}))
	return h, s, p
}

func sharedNVMeRequest(cache string, cacheRole apiv1.ArrayDiskRole) *apiv1.CreateArrayRequest {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}, {Device: "/dev/sdc", Filesystem: disk.XFS}},
		Cache:  &disk.AssignedDisk{Device: cache, Filesystem: disk.XFS},
	}
	return createArrayReq(plan.Confirmation(),
		xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
		xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
		xfsAssignment("/dev/sdc", apiv1.ArrayDiskRoleData),
		xfsAssignment(cache, cacheRole),
	)
}

func TestHandler_CreateArray_CacheOnTheBootDisksSparePartition(t *testing.T) {
	ctx := context.Background()
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank(nvmeSparePath)
	h, s, p := newSharedNVMeHandler(t, probe)

	got, err := h.CreateArray(ctx, sharedNVMeRequest(nvmeSparePart, apiv1.ArrayDiskRoleCache))
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	finished := awaitJob(t, s, got.ID.String())
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if fs, ok := p.FormattedAs(nvmeSparePart); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(%s) = %v, %v, want xfs", nvmeSparePart, fs, ok)
	}
	if _, ok := p.FormattedAs(nvmeBoot); ok {
		t.Fatal("the boot disk itself was formatted")
	}
	if calls := p.FormatCalls(); !slices.Contains(calls, nvmeSparePath) {
		t.Fatalf("FormatCalls = %v, want the spare partition's by-id path among them", calls)
	}
}

func TestHandler_CreateArray_BootDiskPartitionRefusalsFormatNothing(t *testing.T) {
	cases := []struct {
		name     string
		device   string
		role     apiv1.ArrayDiskRole
		wantCode string
		probe    func(*disk.FakeBlankProber)
	}{
		{"spare partition as data", nvmeSparePart, apiv1.ArrayDiskRoleData, "boot_partition_cache_only", nil},
		{"spare partition as parity", nvmeSparePart, apiv1.ArrayDiskRoleParity, "boot_partition_cache_only", nil},
		{"root partition as data", nvmeRootPart, apiv1.ArrayDiskRoleData, "boot_partition_cache_only", nil},
		{"root partition as cache", nvmeRootPart, apiv1.ArrayDiskRoleCache, "unmanaged_device", nil},
		{"the whole boot disk as cache", nvmeBoot, apiv1.ArrayDiskRoleCache, "invalid_plan", nil},
		{"spare partition carrying a signature", nvmeSparePart, apiv1.ArrayDiskRoleCache, "", func(f *disk.FakeBlankProber) { f.ScriptFound(nvmeSparePath) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			probe := disk.NewFakeBlankProber()
			if tc.probe != nil {
				tc.probe(probe)
			} else {
				probe.ScriptBlank(nvmeSparePath)
			}
			h, s, p := newSharedNVMeHandler(t, probe)
			req := sharedNVMeRequest(tc.device, tc.role)
			if tc.role != apiv1.ArrayDiskRoleCache {
				plan := disk.TopologyPlan{
					Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
					Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}, {Device: "/dev/sdc", Filesystem: disk.XFS}},
				}
				if tc.role == apiv1.ArrayDiskRoleData {
					plan.Data = append(plan.Data, disk.AssignedDisk{Device: tc.device, Filesystem: disk.XFS})
				} else {
					plan.Parity = append(plan.Parity, disk.AssignedDisk{Device: tc.device, Filesystem: disk.XFS})
				}
				req = createArrayReq(plan.Confirmation(),
					xfsAssignment("/dev/sda", apiv1.ArrayDiskRoleParity),
					xfsAssignment("/dev/sdb", apiv1.ArrayDiskRoleData),
					xfsAssignment("/dev/sdc", apiv1.ArrayDiskRoleData),
					xfsAssignment(tc.device, tc.role),
				)
			}

			got, err := h.CreateArray(ctx, req)
			if tc.wantCode != "" {
				status := apiError(t, h, err)
				if status.StatusCode != 400 || status.Response.Code != tc.wantCode {
					t.Fatalf("CreateArray = %+v, want 400 %s", status, tc.wantCode)
				}
			} else {
				if err != nil {
					t.Fatalf("CreateArray: %v", err)
				}
				finished := awaitJob(t, s, got.ID.String())
				if finished.Status != job.StatusFailed {
					t.Fatalf("job status = %s, want failed — the partition carries a signature", finished.Status)
				}
			}
			if calls := p.FormatCalls(); len(calls) != 0 {
				t.Fatalf("a refused request formatted %v", calls)
			}
		})
	}
}

func TestHandler_CreateArray_AdoptingABootDiskPartitionIsRefused(t *testing.T) {
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank(nvmeSparePath)
	h, _, p := newSharedNVMeHandler(t, probe)
	req := sharedNVMeRequest(nvmeSparePart, apiv1.ArrayDiskRoleCache)
	req.Disks[3].Adopt = apiv1.NewOptBool(true)
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}, {Device: "/dev/sdc", Filesystem: disk.XFS}},
		Cache:  &disk.AssignedDisk{Device: nvmeSparePart, Filesystem: disk.XFS, Adopt: true},
	}
	req.Confirmation = plan.Confirmation()

	_, err := h.CreateArray(context.Background(), req)
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_plan" {
		t.Fatalf("CreateArray(adopt partition) = %+v, want 400 invalid_plan", status)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v", calls)
	}
}

func TestHandler_PlanDiskAdd_RefusesABootDiskPartition(t *testing.T) {
	h, _, p, st, _, _ := newDiskLifecycleHandler(t)
	seedHandlerArray(t, st, p)
	p.AddDisk(nvmeBoot, disk.Disk{
		Size: 1 * disk.TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: nvmeByIDPrefix,
		CachePartitions: []disk.CachePartition{{Device: nvmeSparePart, Size: 900 * disk.GB, ByIDName: nvmeSpareByID, PartUUID: "5b3d9e0a-03", Reason: disk.ReasonSpareBootPartition}},
	})
	for _, dev := range []string{nvmeSparePart, nvmeRootPart} {
		_, err := h.PlanDiskAdd(context.Background(), &apiv1.AddDiskPlanRequest{Device: dev})
		status := apiError(t, h, err)
		if status.StatusCode != 400 || status.Response.Code != "boot_partition_cache_only" {
			t.Fatalf("PlanDiskAdd(%s) = %+v, want 400 boot_partition_cache_only", dev, status)
		}
	}
}

func TestHandler_ListDisks_ReportsTheBootDisksSparePartitions(t *testing.T) {
	h, _, _ := newSharedNVMeHandler(t, disk.NewFakeBlankProber())
	got, err := h.ListDisks(context.Background())
	if err != nil {
		t.Fatalf("ListDisks: %v", err)
	}
	var boot *apiv1.DiskInventoryEntry
	for i := range got.Disks {
		if got.Disks[i].Device == nvmeBoot {
			boot = &got.Disks[i]
		}
		if got.Disks[i].Device != nvmeBoot && len(got.Disks[i].CachePartitions) != 0 {
			t.Fatalf("%s reports cache partitions", got.Disks[i].Device)
		}
	}
	if boot == nil || !boot.Boot || len(boot.CachePartitions) != 1 {
		t.Fatalf("boot disk entry = %+v, want one cache partition", boot)
	}
	c := boot.CachePartitions[0]
	if c.Device != nvmeSparePart || c.SizeBytes != 900*disk.GB || c.ByIdName.Or("") != nvmeSpareByID || c.PartUuid.Or("") != "5b3d9e0a-03" ||
		c.Reason != apiv1.CachePartitionReasonSpareBootPartition {
		t.Fatalf("cache partition = %+v", c)
	}
}

func poolWithCachePartition(t *testing.T, bootPresent bool) *apiv1.PoolStatus {
	t.Helper()
	ctx := context.Background()
	h, _, _ := newTestHandler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, WWN: "wwn-sdb"})
	if bootPresent {
		p.AddDisk(nvmeBoot, disk.Disk{Size: 1 * disk.TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: nvmeByIDPrefix, FSUUID: "ABCD-1234"})
	}
	h.Disks = p
	arrayStore := store.NewArrayStore(newArrayStoreDB(t))
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-sdb", WWN: "wwn-sdb", Mountpoint: t.TempDir()},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: nvmeSparePart, Filesystem: "ext4", FSUUID: "uuid-cache", Serial: "S4EWNX0M123456X", ByIDName: nvmeSpareByID, Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	h.ArrayStore = arrayStore
	got, err := h.GetPool(ctx)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	return got
}

// TestHandler_GetPool_CacheOnTheBootDiskIsActiveWhileTheBootDiskIsPresent
// keeps the pool page from reporting a healthy cache as missing: GetPool
// leaves the boot disk out of its identity matching, and a cache that is
// a partition of it is bound to that disk's identity.
func TestHandler_GetPool_CacheOnTheBootDiskIsActiveWhileTheBootDiskIsPresent(t *testing.T) {
	got := poolWithCachePartition(t, true)
	var cache *apiv1.PoolDiskEntry
	for i := range got.Disks {
		if got.Disks[i].Role == apiv1.PoolDiskEntryRoleCache {
			cache = &got.Disks[i]
		}
		if got.Disks[i].Device == nvmeBoot {
			t.Fatalf("the boot disk itself is listed as a pool disk: %+v", got.Disks[i])
		}
	}
	if cache == nil || cache.Device != nvmeSparePart || cache.MountPoint != "/mnt/cache" || cache.State != apiv1.DiskStateActive {
		t.Fatalf("cache entry = %+v, want active at %s", cache, nvmeSparePart)
	}
}

func TestHandler_GetPool_CacheOnTheBootDiskIsMissingWhenTheBootDiskIsGone(t *testing.T) {
	got := poolWithCachePartition(t, false)
	for _, e := range got.Disks {
		if e.Role == apiv1.PoolDiskEntryRoleCache {
			if e.State != apiv1.DiskStateMissing {
				t.Fatalf("cache entry state = %s, want missing when no disk carries its identity", e.State)
			}
			return
		}
	}
	t.Fatal("no cache entry")
}
