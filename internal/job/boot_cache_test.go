package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	bootCacheByIDPrefix = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	bootCacheByID       = bootCacheByIDPrefix + "-part3"
	bootCachePath       = "/dev/disk/by-id/" + bootCacheByID
	bootCachePartDev    = "/dev/nvme0n1p3"
	bootCachePartUUID   = "5b3d9e0a-03"
)

func sharedNVMeFixture(dataDisks int) (*disk.FakeProvider, *disk.FakeRunner, DiskFormatParams) {
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/nvme0n1", disk.Disk{
		Size: 1 * disk.TB, Boot: true, Serial: "S4EWNX0M123456X", ByIDName: bootCacheByIDPrefix,
		CachePartitions: []disk.CachePartition{{
			Device: bootCachePartDev, Size: 900 * disk.GB, ByIDName: bootCacheByID, PartUUID: bootCachePartUUID,
			Reason: disk.ReasonSpareBootPartition,
		}},
	})
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "0xp", ByIDName: "wwn-0xp"})
	runner := disk.NewFakeRunner()
	scriptFilesystemUUID(runner, "/dev/disk/by-id/wwn-0xp", "uuid-parity1")
	scriptFilesystemUUID(runner, bootCachePath, "uuid-cache")

	cache := disk.AssignedDisk{
		Device: bootCachePartDev, Filesystem: disk.EXT4, Serial: "S4EWNX0M123456X",
		ByIDName: bootCacheByID, PartUUID: bootCachePartUUID,
	}
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS, WWN: "0xp", ByIDName: "wwn-0xp"}},
		Cache:  &cache,
	}
	sizes := map[string]int64{"/dev/sda": 8 * disk.TB, bootCachePartDev: 900 * disk.GB}
	for i := 1; i <= dataDisks; i++ {
		dev := "/dev/sd" + string(rune('a'+i))
		wwn := "0xd" + string(rune('0'+i))
		p.AddDisk(dev, disk.Disk{Size: 4 * disk.TB, WWN: wwn, ByIDName: "wwn-" + wwn})
		scriptFilesystemUUID(runner, "/dev/disk/by-id/wwn-"+wwn, "uuid-disk"+string(rune('0'+i)))
		plan.Data = append(plan.Data, disk.AssignedDisk{Device: dev, Filesystem: disk.XFS, WWN: wwn, ByIDName: "wwn-" + wwn})
		sizes[dev] = 4 * disk.TB
	}
	return p, runner, DiskFormatParams{
		Confirmation: plan.Confirmation(), Parity: plan.Parity, Data: plan.Data, Cache: plan.Cache, Sizes: sizes,
	}
}

func registerSharedNVMeDiskFormat(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner, probe disk.BlankProber) (*store.ArrayStore, string, *disk.FakeMounter) {
	t.Helper()
	arrayStore := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(DiskFormatDeps{
		Provider: p, Runner: r, Probe: probe, Store: arrayStore, Generator: config.NewGenerator(genRoot), Mounter: mounter,
		Now: func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) },
	}))
	return arrayStore, genRoot, mounter
}

func TestRunDiskFormat_CacheOnBootDiskPartition_FormatsOnlyThePartitionAndPersistsItsIdentity(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p, runner, params := sharedNVMeFixture(2)
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank(bootCachePath)
	st, genRoot, _ := registerSharedNVMeDiskFormat(t, s, p, runner, probe)

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	want := map[string]bool{
		"/dev/disk/by-id/wwn-0xp": true, "/dev/disk/by-id/wwn-0xd1": true, "/dev/disk/by-id/wwn-0xd2": true, bootCachePath: true,
	}
	for _, c := range p.FormatCalls() {
		if !want[c] {
			t.Fatalf("formatted %s, which is not an assigned disk or the spare partition", c)
		}
	}
	if fs, ok := p.FormattedAs(bootCachePartDev); !ok || fs != disk.EXT4 {
		t.Fatalf("FormattedAs(%s) = %v, %v, want ext4", bootCachePartDev, fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/nvme0n1"); ok {
		t.Fatal("the whole boot disk was formatted")
	}

	_, rows, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	var cache *store.ArrayDisk
	for i := range rows {
		if rows[i].Role == store.ArrayRoleCache {
			cache = &rows[i]
		}
	}
	if cache == nil {
		t.Fatalf("no cache row in %+v", rows)
	}
	if cache.Device != bootCachePartDev || cache.ByIDName != bootCacheByID || cache.Serial != "S4EWNX0M123456X" ||
		cache.FSUUID != "uuid-cache" || cache.Mountpoint != "/mnt/cache" || cache.Filesystem != "ext4" {
		t.Fatalf("cache row = %+v", *cache)
	}

	conf, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	for _, line := range []string{
		"content /var/lib/hoserva/snapraid.content\n", "content /mnt/disk1/snapraid.content\n", "content /mnt/disk2/snapraid.content\n",
	} {
		if !strings.Contains(string(conf), line) {
			t.Fatalf("snapraid.conf missing %q:\n%s", line, conf)
		}
	}
	if strings.Contains(string(conf), "/mnt/cache") {
		t.Fatalf("snapraid.conf places a content copy on the cache, which shares the boot disk:\n%s", conf)
	}
}

// TestRunDiskFormat_CacheOnBootDiskRefusesAPlanQ18CannotPlace is Q18 for
// the shared layout: parity, one data disk and a cache on the boot disk is
// two distinct content-file devices. The refusal comes from the preview,
// before any mkfs, so nothing is erased and no topology is written.
func TestRunDiskFormat_CacheOnBootDiskRefusesAPlanQ18CannotPlace(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p, runner, params := sharedNVMeFixture(1)
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank(bootCachePath)
	st, genRoot, mounter := registerSharedNVMeDiskFormat(t, s, p, runner, probe)

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || !strings.Contains(finished.ErrorMessage, "Q18") {
		t.Fatalf("status = %s (%q), want failed with the Q18 content-placement refusal", finished.Status, finished.ErrorMessage)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v before refusing the layout", calls)
	}
	if got := probe.Probed(); len(got) != 0 {
		t.Fatalf("probed %v before refusing the layout", got)
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestRunDiskFormat_CacheOnBootDiskWithASignatureErasesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p, runner, params := sharedNVMeFixture(2)
	probe := disk.NewFakeBlankProber()
	probe.ScriptFound(bootCachePath)
	st, genRoot, mounter := registerSharedNVMeDiskFormat(t, s, p, runner, probe)

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed || !strings.Contains(finished.ErrorMessage, "not confirmed blank") {
		t.Fatalf("status = %s (%q), want failed: spare partition not confirmed blank", finished.Status, finished.ErrorMessage)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v although the partition carries a signature — the parity and data disks must be left alone too", calls)
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestRunDiskFormat_CacheOnBootDiskWithoutAProbeErasesNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p, runner, params := sharedNVMeFixture(2)
	st, genRoot, mounter := registerSharedNVMeDiskFormat(t, s, p, runner, nil)

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%q), want failed — the default probe runs blkid, which the fake runner reports as found", finished.Status, finished.ErrorMessage)
	}
	if calls := p.FormatCalls(); len(calls) != 0 {
		t.Fatalf("formatted %v", calls)
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestLayoutFromStore_CacheOnBootDeviceOnlyForAPartitionRow(t *testing.T) {
	rows := func(cache store.ArrayDisk) []store.ArrayDisk {
		return []store.ArrayDisk{
			{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Mountpoint: "/mnt/parity1"},
			{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Mountpoint: "/mnt/disk1"},
			cache,
		}
	}
	shared := layoutFromStore(rows(store.ArrayDisk{Role: store.ArrayRoleCache, RoleIndex: 1, Device: bootCachePartDev, ByIDName: bootCacheByID, Mountpoint: "/mnt/cache"}))
	if !shared.CacheOnBootDevice {
		t.Fatal("a cache row that is a partition must set CacheOnBootDevice")
	}
	if _, err := shared.Render(); err == nil {
		t.Fatal("Render accepted parity + one data disk + a cache on the boot disk (two distinct content devices)")
	}
	own := layoutFromStore(rows(store.ArrayDisk{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme1n1", ByIDName: "nvme-Other_Disk_SERIAL1", Mountpoint: "/mnt/cache"}))
	if own.CacheOnBootDevice {
		t.Fatal("a whole-disk cache must not set CacheOnBootDevice")
	}
	if _, err := own.Render(); err != nil {
		t.Fatalf("Render with a cache on its own disk: %v", err)
	}
}

func TestSnapraidLayout_CacheOnBootDeviceOnlyForAPartitionPlan(t *testing.T) {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: "/dev/sda"}},
		Data:   []disk.AssignedDisk{{Device: "/dev/sdb"}},
	}
	plan.Cache = &disk.AssignedDisk{Device: bootCachePartDev, ByIDName: bootCacheByID}
	if !snapraidLayout(plan).CacheOnBootDevice {
		t.Fatal("a partition cache must set CacheOnBootDevice")
	}
	plan.Cache = &disk.AssignedDisk{Device: "/dev/nvme1n1", ByIDName: "nvme-Other_Disk_SERIAL1"}
	if snapraidLayout(plan).CacheOnBootDevice {
		t.Fatal("a whole-disk cache must not set CacheOnBootDevice")
	}
}
