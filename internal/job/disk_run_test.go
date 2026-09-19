package job

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func validFormatParams(parity, data string, sizes map[string]int64) DiskFormatParams {
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
	}
	return DiskFormatParams{
		Confirmation: plan.Confirmation(),
		Parity:       plan.Parity,
		Data:         plan.Data,
		Sizes:        sizes,
	}
}

func validFormatParamsWithCache(parity, data, cache string, sizes map[string]int64) DiskFormatParams {
	c := disk.AssignedDisk{Device: cache, Filesystem: disk.XFS}
	plan := disk.TopologyPlan{
		Parity: []disk.AssignedDisk{{Device: parity, Filesystem: disk.XFS}},
		Data:   []disk.AssignedDisk{{Device: data, Filesystem: disk.XFS}},
		Cache:  &c,
	}
	return DiskFormatParams{
		Confirmation: plan.Confirmation(),
		Parity:       plan.Parity,
		Data:         plan.Data,
		Cache:        plan.Cache,
		Sizes:        sizes,
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
	}
}

func scriptFilesystemUUID(r *disk.FakeRunner, dev, uuid string) {
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", dev}, []byte(uuid+"\n"), nil)
}

func registerDiskFormat(t *testing.T, s *Scheduler, p disk.Provider, r disk.Runner) (*store.ArrayStore, string, *disk.FakeMounter) {
	t.Helper()
	arrayStore := store.NewArrayStore(newTestDB(t))
	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()
	s.registry.Register(TypeDiskFormat, false, RunDiskFormat(DiskFormatDeps{
		Provider:  p,
		Runner:    r,
		Store:     arrayStore,
		Generator: config.NewGenerator(genRoot),
		Mounter:   mounter,
		Now:       func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) },
	}))
	return arrayStore, genRoot, mounter
}

func assertNoArrayTopology(t *testing.T, st *store.ArrayStore, genRoot string, mounter *disk.FakeMounter) {
	t.Helper()
	exists, err := st.Exists(context.Background())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("SQLite has array topology after a failed FormatPlan")
	}
	if _, err := os.Stat(filepath.Join(genRoot, "snapraid.conf")); !os.IsNotExist(err) {
		t.Fatalf("snapraid.conf written after a failed FormatPlan: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(genRoot, "systemd", "system")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".mount") {
				t.Fatalf("mount unit %s written after a failed FormatPlan", e.Name())
			}
		}
	}
	if len(mounter.Mounts) != 0 {
		t.Fatalf("mounted %d units after a failed FormatPlan: %+v", len(mounter.Mounts), mounter.Mounts)
	}
}

func TestRunDiskFormat_WrongConfirmationFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())

	params := validFormatParams("/dev/sda", "/dev/sdb", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB})
	params.Confirmation = "erase /dev/sda, /dev/sdb"

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "typed confirmation") {
		t.Fatalf("ErrorMessage = %q, want confirmation mismatch", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("wrong confirmation formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("wrong confirmation formatted /dev/sdb")
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestSubmit_RejectsDiskFormatWithoutConfirmation(t *testing.T) {
	s := newTestScheduler(t)
	registerDiskFormat(t, s, disk.NewFakeProvider(), disk.NewFakeRunner())
	for _, params := range [][]byte{
		nil,
		[]byte("null"),
		[]byte(""),
		mustJSON(t, DiskFormatParams{Parity: []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}}}),
	} {
		_, err := s.Submit(context.Background(), TypeDiskFormat, nil, params)
		if err == nil {
			t.Fatalf("Submit(disk_format, params=%q) = nil error, want rejection", params)
		}
	}
}

func TestRunDiskFormat_UnmanagedPathFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())

	params := validFormatParams("/dev/sda", "/dev/sda1", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sda1": 4 * disk.TB})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !errors.Is(errors.Unwrap(errors.New(finished.ErrorMessage)), disk.ErrUnmanagedDevice) &&
		!strings.Contains(finished.ErrorMessage, "not a loop device") {
		t.Fatalf("ErrorMessage = %q, want unmanaged-device refusal", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("unmanaged path in the plan formatted /dev/sda")
	}
	if _, ok := p.FormattedAs("/dev/sda1"); ok {
		t.Fatal("unmanaged path formatted /dev/sda1")
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestRunDiskFormat_ValidateFailuresFormatNothing(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		params DiskFormatParams
		want   string
	}{
		{
			name: "no parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sdb",
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sdb": 4 * disk.TB},
			},
			want: "at least one parity",
		},
		{
			name: "three parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb, /dev/sdc, /dev/sdd",
				Parity: []disk.AssignedDisk{
					{Device: "/dev/sda", Filesystem: disk.XFS},
					{Device: "/dev/sdb", Filesystem: disk.XFS},
					{Device: "/dev/sdc", Filesystem: disk.XFS},
				},
				Data:  []disk.AssignedDisk{{Device: "/dev/sdd", Filesystem: disk.XFS}},
				Sizes: map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 8 * disk.TB, "/dev/sdc": 8 * disk.TB, "/dev/sdd": 4 * disk.TB},
			},
			want: "at most two parity",
		},
		{
			name: "parity not xfs",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.EXT4}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB},
			},
			want: "always formatted XFS",
		},
		{
			name: "weak identity parity",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS, WeakIdentity: true}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB},
			},
			want: "weak-identity",
		},
		{
			name: "parity smaller than data",
			params: DiskFormatParams{
				Confirmation: "ERASE /dev/sda, /dev/sdb",
				Parity:       []disk.AssignedDisk{{Device: "/dev/sda", Filesystem: disk.XFS}},
				Data:         []disk.AssignedDisk{{Device: "/dev/sdb", Filesystem: disk.XFS}},
				Sizes:        map[string]int64{"/dev/sda": 4 * disk.TB, "/dev/sdb": 8 * disk.TB},
			},
			want: "at least as large",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestScheduler(t)
			p := disk.NewFakeProvider()
			p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdb", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdc", disk.Disk{Size: 8 * disk.TB})
			p.AddDisk("/dev/sdd", disk.Disk{Size: 8 * disk.TB})
			st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())

			j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, tc.params))
			if err != nil {
				t.Fatalf("Submit: %v", err)
			}
			finished := await(t, s, j.ID)
			if finished.Status != StatusFailed {
				t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
			}
			if !strings.Contains(finished.ErrorMessage, tc.want) {
				t.Fatalf("ErrorMessage = %q, want substring %q", finished.ErrorMessage, tc.want)
			}
			for _, dev := range []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/dev/sdd"} {
				if _, ok := p.FormattedAs(dev); ok {
					t.Fatalf("formatted %s despite Validate failure %q", dev, tc.name)
				}
			}
			assertNoArrayTopology(t, st, genRoot, mounter)
		})
	}
}

func TestRunDiskFormat_SuccessPersistsTopologyAndGeneratesFromSQLite(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, WWN: "wwn-parity", Serial: "PARITY1", ByIDName: "wwn-wwn-parity"})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB, Serial: "DATA1"})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	runner := disk.NewFakeRunner()
	scriptFilesystemUUID(runner, "/dev/disk/by-id/wwn-wwn-parity", "uuid-parity1")
	scriptFilesystemUUID(runner, "/dev/sdb", "uuid-disk1")
	scriptFilesystemUUID(runner, "/dev/sdc", "uuid-cache")
	st, genRoot, mounter := registerDiskFormat(t, s, p, runner)

	params := validFormatParamsWithCache("/dev/sda", "/dev/sdb", "/dev/sdc", map[string]int64{
		"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB,
	})
	params.Parity[0].WWN = "wwn-parity"
	params.Parity[0].Serial = "PARITY1"
	params.Parity[0].ByIDName = "wwn-wwn-parity"
	params.Data[0].Serial = "DATA1"

	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
	if fs, ok := p.FormattedAs("/dev/sda"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sda) = (%v, %v), want xfs", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdb"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sdb) = (%v, %v), want xfs", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs(/dev/sdc) = (%v, %v), want xfs", fs, ok)
	}

	settings, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if settings.CreatePolicy != "mfs" || settings.MinFreeSpace != "20G" {
		t.Fatalf("settings = %+v, want create_policy=mfs min_free_space=20G", settings)
	}
	if len(disks) != 3 {
		t.Fatalf("len(disks) = %d, want 3: %+v", len(disks), disks)
	}
	byRole := map[string]store.ArrayDisk{}
	for _, d := range disks {
		byRole[d.Role] = d
		if d.FSUUID == "" || d.Mountpoint == "" {
			t.Fatalf("disk %+v missing uuid or mountpoint", d)
		}
	}
	if byRole[store.ArrayRoleParity].FSUUID != "uuid-parity1" || byRole[store.ArrayRoleParity].Mountpoint != "/mnt/parity1" {
		t.Fatalf("parity = %+v", byRole[store.ArrayRoleParity])
	}
	if byRole[store.ArrayRoleParity].WWN != "wwn-parity" || byRole[store.ArrayRoleParity].ByIDName != "wwn-wwn-parity" {
		t.Fatalf("parity identity = %+v", byRole[store.ArrayRoleParity])
	}
	if byRole[store.ArrayRoleData].FSUUID != "uuid-disk1" || byRole[store.ArrayRoleData].Mountpoint != "/mnt/disk1" {
		t.Fatalf("data = %+v", byRole[store.ArrayRoleData])
	}
	if byRole[store.ArrayRoleCache].FSUUID != "uuid-cache" || byRole[store.ArrayRoleCache].Mountpoint != "/mnt/cache" {
		t.Fatalf("cache = %+v", byRole[store.ArrayRoleCache])
	}

	diskUnit, err := os.ReadFile(filepath.Join(genRoot, "systemd", "system", "mnt-disk1.mount"))
	if err != nil {
		t.Fatalf("reading data disk unit: %v", err)
	}
	if !strings.Contains(string(diskUnit), "What=/dev/disk/by-uuid/uuid-disk1") {
		t.Fatalf("data disk unit not mounted by UUID:\n%s", diskUnit)
	}
	if strings.Contains(string(diskUnit), "/dev/sdb") {
		t.Fatalf("data disk unit names the transient device path:\n%s", diskUnit)
	}
	parityUnit, err := os.ReadFile(filepath.Join(genRoot, "systemd", "system", "mnt-parity1.mount"))
	if err != nil {
		t.Fatalf("reading parity disk unit: %v", err)
	}
	if !strings.Contains(string(parityUnit), "What=/dev/disk/by-uuid/uuid-parity1") {
		t.Fatalf("parity disk unit not mounted by UUID:\n%s", parityUnit)
	}

	conf, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	got := string(conf)
	for _, want := range []string{"/mnt/parity1", "/mnt/disk1", "data d1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("snapraid.conf missing %q:\n%s", want, got)
		}
	}
	for _, refuse := range []string{"/dev/sda", "/dev/sdb", "/dev/sdc", "/mnt/disk2"} {
		if strings.Contains(got, refuse) {
			t.Fatalf("snapraid.conf names %q, which is not a plan mount:\n%s", refuse, got)
		}
	}

	poolUnit, err := os.ReadFile(filepath.Join(genRoot, "systemd", "system", "mnt-user.mount"))
	if err != nil {
		t.Fatalf("catch-all pool unit: %v", err)
	}
	if !strings.Contains(string(poolUnit), "category.create=mfs") {
		t.Fatalf("catch-all pool unit missing selected create policy:\n%s", poolUnit)
	}

	if len(mounter.Mounts) != 3 {
		t.Fatalf("mounted %d units, want 3: %+v", len(mounter.Mounts), mounter.Mounts)
	}
	for _, u := range mounter.Mounts {
		if u.UUID == "" || strings.HasPrefix(u.UUID, "/dev/") {
			t.Fatalf("mounted by something other than filesystem UUID: %+v", u)
		}
	}
}

func TestRunDiskFormat_PartialFormatFailureWritesNoTopology(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	p.FailAfter("/dev/sdb", 0)
	st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())

	params := validFormatParamsWithCache("/dev/sda", "/dev/sdb", "/dev/sdc", map[string]int64{
		"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB,
	})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}

func TestRunDiskFormat_ExistingArrayFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sdc", disk.Disk{Size: 4 * disk.TB})
	st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())
	if err := st.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mspmfs",
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, []store.ArrayDisk{{
		Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda",
		Filesystem: "xfs", FSUUID: "existing-parity", Mountpoint: "/mnt/parity1",
	}, {
		Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb",
		Filesystem: "xfs", FSUUID: "existing-data", Mountpoint: "/mnt/disk1",
	}}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	params := validFormatParamsWithCache("/dev/sda", "/dev/sdb", "/dev/sdc", map[string]int64{
		"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB, "/dev/sdc": 4 * disk.TB,
	})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "already exists") {
		t.Fatalf("ErrorMessage = %q, want already exists", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("existing array formatted /dev/sda")
	}
	if _, err := os.Stat(filepath.Join(genRoot, "snapraid.conf")); !os.IsNotExist(err) {
		t.Fatal("existing array regenerated snapraid.conf")
	}
	if len(mounter.Mounts) != 0 {
		t.Fatalf("existing array mounted units: %+v", mounter.Mounts)
	}
}

func TestRunDiskFormat_TooFewDisksForContentFilesFormatsNothing(t *testing.T) {
	ctx := context.Background()
	s := newTestScheduler(t)
	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	st, genRoot, mounter := registerDiskFormat(t, s, p, disk.NewFakeRunner())

	params := validFormatParams("/dev/sda", "/dev/sdb", map[string]int64{"/dev/sda": 8 * disk.TB, "/dev/sdb": 4 * disk.TB})
	j, err := s.Submit(ctx, TypeDiskFormat, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	finished := await(t, s, j.ID)
	if finished.Status != StatusFailed {
		t.Fatalf("status = %s (%s), want failed", finished.Status, finished.ErrorMessage)
	}
	if !strings.Contains(finished.ErrorMessage, "content") {
		t.Fatalf("ErrorMessage = %q, want content-placement refusal", finished.ErrorMessage)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("under-specified array formatted /dev/sda")
	}
	assertNoArrayTopology(t, st, genRoot, mounter)
}
