package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	uuidData1  = "11111111-1111-4111-8111-111111111111"
	uuidData2  = "22222222-2222-4222-8222-222222222222"
	uuidParity = "33333333-3333-4333-8333-333333333333"
)

// unraidDisks is an Unraid array as udev lists it on the new machine: two data
// disks, a parity disk that is larger and reports XFS, and a cache SSD, each
// with a by-id link for its first partition.
func unraidDisks() []Disk {
	d := func(dev, serial string, size int64, fs, uuid string) Disk {
		byID := "ata-EX_" + serial
		return Disk{Device: dev, Serial: serial, Size: size, Filesystem: fs, FSDevice: dev + "1", FSUUID: uuid, ByIDName: byID, FSByIDName: byID + "-part1"}
	}
	return []Disk{
		d("/dev/sdb", "PAR1", 8*TB, "xfs", uuidParity),
		d("/dev/sdc", "DAT1", 4*TB, "xfs", uuidData1),
		d("/dev/sdd", "DAT2", 2*TB, "ext4", uuidData2),
		d("/dev/nvme0n1", "CAC1", 500*GB, "btrfs", "44444444-4444-4444-8444-444444444444"),
	}
}

func assignAll() []AdoptionAssignment {
	return []AdoptionAssignment{
		{Role: AdoptParity, Serial: "PAR1"},
		{Role: AdoptData, Serial: "DAT1"},
		{Role: AdoptData, Serial: "DAT2"},
		{Role: AdoptCache, Serial: "CAC1"},
	}
}

func TestResolveAdoption_BuildsTheAdoptionPlan(t *testing.T) {
	plan, err := ResolveAdoption(unraidDisks(), assignAll())
	if err != nil {
		t.Fatalf("ResolveAdoption: %v", err)
	}
	if len(plan.Data) != 2 || len(plan.Parity) != 1 || plan.Cache == nil {
		t.Fatalf("plan = %+v, want two data disks, one parity and a cache", plan)
	}
	d1 := plan.Data[0]
	if !d1.Adopt || d1.Filesystem != XFS || d1.FSUUID != uuidData1 || d1.FSDevice != "/dev/sdc1" || d1.MountSource != "/dev/disk/by-id/ata-EX_DAT1-part1" {
		t.Errorf("data disk 1 = %+v", d1)
	}
	if plan.Data[1].Filesystem != EXT4 {
		t.Errorf("data disk 2 = %+v, want ext4", plan.Data[1])
	}
	if plan.Parity[0].Filesystem != XFS || plan.Parity[0].Adopt {
		t.Errorf("the recorded parity disk = %+v, want it recorded and never adopted", plan.Parity[0])
	}

	units := plan.MountUnits()
	if len(units) != 2 {
		t.Fatalf("units = %+v, want one per data disk and none for parity or cache", units)
	}
	for i, u := range units {
		if !u.ReadOnly || u.What == "" || u.Where != []string{"/mnt/disk1", "/mnt/disk2"}[i] {
			t.Errorf("unit %d = %+v, want a read-only unit bound to the disk's own device", i, u)
		}
	}
	rendered := units[0].Render("/state/array-stopped")
	for _, want := range []string{"What=/dev/disk/by-id/ata-EX_DAT1-part1", "Options=ro,norecovery,noatime,nodiratime,nosuid,nodev,noexec,nofail"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("unit 1:\n%s\nwant %q", rendered, want)
		}
	}
	if strings.Contains(rendered, "by-uuid") || strings.Contains(rendered, "defaults") {
		t.Errorf("unit 1 is not read-only or mounts by UUID:\n%s", rendered)
	}
	if got := units[1].Render("/x"); !strings.Contains(got, "Options=ro,noload,") {
		t.Errorf("an ext4 unit = %s, want ro,noload", got)
	}
}

// The one-data-disk array of doc 05 §3: Unraid's single parity is a copy of the
// data disk, so the parity partition carries the data disk's UUID.
func TestResolveAdoption_OneDataDiskWhoseParityCarriesItsUUID(t *testing.T) {
	disks := unraidDisks()[:2]
	disks[0].FSUUID = disks[1].FSUUID
	assign := []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptData, Serial: "DAT1"}}

	plan, err := ResolveAdoption(disks, assign)
	if err != nil {
		t.Fatalf("ResolveAdoption: %v", err)
	}
	units := plan.MountUnits()
	if len(units) != 1 || units[0].What != "/dev/disk/by-id/ata-EX_DAT1-part1" {
		t.Fatalf("units = %+v, want the one data disk bound to its own by-id link", units)
	}
	if err := plan.CheckData(context.Background(), failIfRun{t}); err != nil {
		t.Fatal(err)
	}

	// Without a link nothing but the UUID tells the two apart: refused, never
	// mounted by a UUID the parity copy shares.
	noLink := unraidDisks()[:2]
	noLink[0].FSUUID = noLink[1].FSUUID
	noLink[1].FSByIDName = ""
	if _, err := ResolveAdoption(noLink, assign); !errors.Is(err, ErrAdoptUUIDShared) {
		t.Errorf("a data disk with no by-id link and a shared UUID = %v, want ErrAdoptUUIDShared", err)
	}
}

// failIfRun is a Runner that records the one check that may run and fails any
// other command: parity is never opened.
type failIfRun struct{ t *testing.T }

func (f failIfRun) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	if cmd != "xfs_repair -n /dev/disk/by-id/ata-EX_DAT1-part1" {
		f.t.Errorf("unexpected command: %s", cmd)
	}
	return nil, nil
}

func TestResolveAdoption_RefusesWhatIsNotAdoptable(t *testing.T) {
	twoSameUUID := unraidDisks()
	twoSameUUID[2].FSUUID = twoSameUUID[1].FSUUID
	stick := func(disks []Disk) []Disk {
		return append(disks, Disk{Device: "/dev/sde", Serial: "STICK1", Size: 16 * GB, Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234", FSDevice: "/dev/sde1"})
	}
	bootDisk := func(disks []Disk) []Disk {
		return append(disks, Disk{Device: "/dev/sdf", Serial: "BOOT1", Size: 10 * TB, Boot: true, Filesystem: "ext4", FSUUID: "55555555-5555-4555-8555-555555555555", FSDevice: "/dev/sdf1"})
	}
	internalBoot := func(disks []Disk) []Disk {
		return append(disks, Disk{Device: "/dev/nvme1n1", Serial: "IBOOT", Size: 10 * TB, UnraidBoot: true, Filesystem: "zfs_member", FSDevice: "/dev/nvme1n1p3"})
	}
	weakParity := unraidDisks()
	weakParity[0].WeakIdentity = true
	smallParity := unraidDisks()
	smallParity[0].Size = 3 * TB
	failed := unraidDisks()
	failed[1].Failed = true
	zfs := unraidDisks()
	zfs[1].Filesystem = "zfs_member"
	noNode := unraidDisks()
	noNode[1].FSDevice = ""
	dup := append(assignAll(), AdoptionAssignment{Role: AdoptIgnore, Serial: "DAT1"})

	tests := []struct {
		name    string
		disks   []Disk
		assign  []AdoptionAssignment
		wantErr error
	}{
		{"an unknown serial", unraidDisks(), []AdoptionAssignment{{Role: AdoptData, Serial: "NOPE"}}, ErrAdoptDiskMissing},
		{"neither serial nor WWN", unraidDisks(), []AdoptionAssignment{{Role: AdoptData}}, ErrAdoptDiskMissing},
		{"an ignored disk that is gone", unraidDisks(), append(assignAll(), AdoptionAssignment{Role: AdoptIgnore, Serial: "GONE"}), ErrAdoptDiskMissing},
		{"two disks with one serial", append(unraidDisks(), Disk{Device: "/dev/sdg", Serial: "DAT1", Size: TB}), assignAll(), ErrAdoptDiskAmbiguous},
		{"a disk with two roles", unraidDisks(), dup, ErrDeviceAssignedTwice},
		{"the USB stick as data", stick(unraidDisks()), append(assignAll(), AdoptionAssignment{Role: AdoptData, Serial: "STICK1"}), ErrUnraidStick},
		{"the USB stick as parity", stick(unraidDisks()), append(assignAll(), AdoptionAssignment{Role: AdoptParity, Serial: "STICK1"}), ErrUnraidStick},
		{"the USB stick as cache", stick(unraidDisks()), append(assignAll(), AdoptionAssignment{Role: AdoptCache, Serial: "STICK1"}), ErrUnraidStick},
		{"the boot disk as parity", bootDisk(unraidDisks()), []AdoptionAssignment{{Role: AdoptParity, Serial: "BOOT1"}, {Role: AdoptData, Serial: "DAT1"}}, ErrAdoptBootDisk},
		{"the boot disk as data", bootDisk(unraidDisks()), []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptData, Serial: "BOOT1"}}, ErrAdoptBootDisk},
		{"the whole boot disk as cache", bootDisk(unraidDisks()), append(assignAll()[:3], AdoptionAssignment{Role: AdoptCache, Serial: "BOOT1"}), ErrAdoptBootDisk},
		{"an Unraid internal boot device as data", internalBoot(unraidDisks()), []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptData, Serial: "IBOOT"}}, ErrAdoptUnraidBoot},
		{"an Unraid internal boot device as cache", internalBoot(unraidDisks()), []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptData, Serial: "DAT1"}, {Role: AdoptCache, Serial: "IBOOT"}}, ErrAdoptUnraidBoot},
		{"a weak-identity disk as parity", weakParity, assignAll(), ErrWeakIdentityParity},
		{"parity smaller than the largest data disk", smallParity, assignAll(), ErrParityTooSmall},
		{"no parity", unraidDisks(), assignAll()[1:3], ErrNoParityDisks},
		{"three parity disks", unraidDisks(), []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptParity, Serial: "DAT1"}, {Role: AdoptParity, Serial: "DAT2"}, {Role: AdoptParity, Serial: "CAC1"}}, ErrTooManyParityDisks},
		{"no data disk", unraidDisks(), assignAll()[:1], ErrNoDataDisks},
		{"a failed disk", failed, assignAll(), ErrAdoptDiskFailed},
		{"a ZFS data disk", zfs, assignAll(), ErrAdoptNoFilesystem},
		{"a data disk with no filesystem node", noNode, assignAll(), ErrAdoptNoFilesystem},
		{"two data disks with one UUID", twoSameUUID, assignAll(), ErrAdoptUUIDShared},
		{"a cache with neither serial nor a partition", unraidDisks(), []AdoptionAssignment{{Role: AdoptParity, Serial: "PAR1"}, {Role: AdoptData, Serial: "DAT1"}, {Role: AdoptCache, ByIDName: "x"}}, ErrAdoptNoCacheBinding},
		{"an unknown role", unraidDisks(), []AdoptionAssignment{{Role: "boot", Serial: "DAT1"}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveAdoption(tc.disks, tc.assign)
			switch {
			case err == nil:
				t.Fatal("ResolveAdoption accepted it")
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("ResolveAdoption = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestResolveAdoption_ACacheOnASparePartitionOfTheBootDisk(t *testing.T) {
	disks := unraidDisks()
	disks = append(disks, Disk{
		Device: "/dev/nvme1n1", Serial: "NVME-BOOT", WWN: "eui.0001", Size: 1 * TB, Boot: true, ByIDName: "nvme-eui.0001",
		CachePartitions: []CachePartition{{Device: "/dev/nvme1n1p3", Size: 400 * GB, ByIDName: "nvme-eui.0001-part3", PartUUID: "aaaa-bbbb", Reason: ReasonSpareBootPartition}},
	})
	assign := append(assignAll()[:3], AdoptionAssignment{Role: AdoptCache, ByIDName: "nvme-eui.0001-part3", PartUUID: "AAAA-BBBB"})
	plan, err := ResolveAdoption(disks, assign)
	if err != nil {
		t.Fatalf("ResolveAdoption: %v", err)
	}
	c := plan.Cache
	if c == nil || c.Device != "/dev/nvme1n1p3" || c.PartUUID != "aaaa-bbbb" || c.ByIDName != "nvme-eui.0001-part3" || c.Serial != "NVME-BOOT" || c.Size != 400*GB {
		t.Fatalf("cache = %+v, want the partition with its parent's identity and its own PARTUUID", c)
	}
	for _, bad := range []AdoptionAssignment{
		{Role: AdoptCache, ByIDName: "nvme-eui.0001-part3", PartUUID: "other"},
		{Role: AdoptCache, ByIDName: "nvme-eui.0001-part9", PartUUID: "aaaa-bbbb"},
		{Role: AdoptCache, ByIDName: "nvme-eui.0001-part3"},
		{Role: AdoptCache, ByIDName: "nvme-eui.0001-part3", PartUUID: "aaaa-bbbb", Serial: "NVME-BOOT"},
	} {
		if _, err := ResolveAdoption(disks, append(assignAll()[:3], bad)); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}

// An Unraid boot + data device is one disk that holds Unraid's boot pool in
// partitions 1 to 3 and its cache in partition 4: the cache role records the
// partition by its own identity and never the disk.
func TestResolveAdoption_TheCacheOfAnUnraidBootAndDataDeviceIsItsDataPartitionOnly(t *testing.T) {
	shared := func(p *BootPartition) []Disk {
		return append(unraidDisks()[:3], Disk{
			Device: "/dev/nvme1n1", Serial: "IBOOT", WWN: "eui.0002", Size: 1 * TB, ByIDName: "nvme-eui.0002",
			UnraidBoot: true, Filesystem: "zfs_member", FSDevice: "/dev/nvme1n1p3", UnraidDataPartition: p,
		})
	}
	part4 := &BootPartition{Device: "/dev/nvme1n1p4", Size: 400 * GB, ByIDName: "nvme-eui.0002-part4", PartUUID: "cccc-dddd", Filesystem: "xfs", FSUUID: "66666666-6666-4666-8666-666666666666"}
	assign := append(assignAll()[:3], AdoptionAssignment{Role: AdoptCache, WWN: "eui.0002"})

	plan, err := ResolveAdoption(shared(part4), assign)
	if err != nil {
		t.Fatalf("ResolveAdoption: %v", err)
	}
	c := plan.Cache
	if c == nil || c.Device != "/dev/nvme1n1p4" || c.ByIDName != "nvme-eui.0002-part4" || c.PartUUID != "cccc-dddd" || c.WWN != "eui.0002" || c.Serial != "IBOOT" || c.Size != 400*GB {
		t.Fatalf("cache = %+v, want partition 4 with its own by-id link and PARTUUID under the disk's identity", c)
	}
	if !IsPartition(c.Device, c.ByIDName) {
		t.Errorf("cache %q is not a partition: #300 would format the whole disk", c.Device)
	}
	fresh, err := ResolveAdoption(shared(part4), assign)
	if err != nil {
		t.Fatalf("ResolveAdoption again: %v", err)
	}
	if err := plan.Matches(fresh); err != nil {
		t.Errorf("the same partition does not match itself: %v", err)
	}
	moved := *part4
	moved.PartUUID = "eeee-ffff"
	if fresh, err = ResolveAdoption(shared(&moved), assign); err != nil {
		t.Fatalf("ResolveAdoption repartitioned: %v", err)
	}
	if err := plan.Matches(fresh); !errors.Is(err, ErrAdoptDiskChanged) {
		t.Errorf("a repartitioned device matches: %v", err)
	}

	noByID, noPartUUID, noSize := *part4, *part4, *part4
	noByID.ByIDName, noPartUUID.PartUUID, noSize.Size = "", "", 0
	for _, tc := range []struct {
		name  string
		disks []Disk
		a     []AdoptionAssignment
	}{
		{"no data partition", shared(nil), assign},
		{"a data partition with no by-id link", shared(&noByID), assign},
		{"a data partition with no PARTUUID", shared(&noPartUUID), assign},
		{"a data partition with no size", shared(&noSize), assign},
		{"as data", shared(part4), append(assignAll()[:2], AdoptionAssignment{Role: AdoptData, WWN: "eui.0002"})},
		{"as parity", shared(part4), append(assignAll()[1:3], AdoptionAssignment{Role: AdoptParity, WWN: "eui.0002"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ResolveAdoption(tc.disks, tc.a); !errors.Is(err, ErrAdoptUnraidBoot) {
				t.Fatalf("ResolveAdoption = %v, want ErrAdoptUnraidBoot", err)
			}
		})
	}

	withCache := append(shared(part4), unraidDisks()[3])
	if _, err := ResolveAdoption(withCache, append(assign, AdoptionAssignment{Role: AdoptCache, Serial: "CAC1"})); !errors.Is(err, ErrTooManyCacheDisks) {
		t.Errorf("a whole-disk cache beside the shared disk's: %v, want ErrTooManyCacheDisks", err)
	}
	if _, err := ResolveAdoption(withCache, append(assignAll(), AdoptionAssignment{Role: AdoptCache, WWN: "eui.0002"})); !errors.Is(err, ErrTooManyCacheDisks) {
		t.Errorf("the shared disk's cache beside a whole-disk one: %v, want ErrTooManyCacheDisks", err)
	}
}

func TestResolveAdoption_RefusesASecondCache(t *testing.T) {
	second := Disk{Device: "/dev/nvme2n1", Serial: "CAC2", Size: 500 * GB, Filesystem: "xfs", FSDevice: "/dev/nvme2n1p1", FSUUID: "55555555-5555-4555-8555-555555555555", ByIDName: "ata-EX_CAC2", FSByIDName: "ata-EX_CAC2-part1"}
	boot := Disk{
		Device: "/dev/nvme1n1", Serial: "NVME-BOOT", WWN: "eui.0001", Size: 1 * TB, Boot: true, ByIDName: "nvme-eui.0001",
		CachePartitions: []CachePartition{{Device: "/dev/nvme1n1p3", Size: 400 * GB, ByIDName: "nvme-eui.0001-part3", PartUUID: "aaaa-bbbb", Reason: ReasonSpareBootPartition}},
	}
	disks := append(unraidDisks(), second, boot)
	part := AdoptionAssignment{Role: AdoptCache, ByIDName: "nvme-eui.0001-part3", PartUUID: "aaaa-bbbb"}
	for _, tc := range []struct {
		name string
		a    []AdoptionAssignment
	}{
		{"two disks by serial", append(assignAll(), AdoptionAssignment{Role: AdoptCache, Serial: "CAC2"})},
		{"two disks, the second first", append(assignAll()[:3], AdoptionAssignment{Role: AdoptCache, Serial: "CAC2"}, AdoptionAssignment{Role: AdoptCache, Serial: "CAC1"})},
		{"a partition then a disk", append(assignAll()[:3], part, AdoptionAssignment{Role: AdoptCache, Serial: "CAC1"})},
		{"a disk then a partition", append(assignAll(), part)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := ResolveAdoption(disks, tc.a)
			if !errors.Is(err, ErrTooManyCacheDisks) {
				t.Fatalf("ResolveAdoption = %v, want ErrTooManyCacheDisks", err)
			}
			if plan.Cache != nil {
				t.Errorf("a refused plan carries cache %+v", plan.Cache)
			}
		})
	}
}

func TestAdoptionPlan_MatchesRefusesADiskThatChanged(t *testing.T) {
	plan, err := ResolveAdoption(unraidDisks(), assignAll())
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Matches(plan); err != nil {
		t.Fatalf("a plan does not match itself: %v", err)
	}
	// A device node that renumbered is not a change.
	renumbered := unraidDisks()
	renumbered[1].Device, renumbered[1].FSDevice = "/dev/sdx", "/dev/sdx1"
	if fresh, err := ResolveAdoption(renumbered, assignAll()); err != nil || plan.Matches(fresh) != nil {
		t.Errorf("a renumbered /dev name is not a different disk: %v %v", err, plan.Matches(fresh))
	}
	for name, mutate := range map[string]func([]Disk){
		"a new filesystem UUID": func(d []Disk) { d[1].FSUUID = "99999999-9999-4999-8999-999999999999" },
		"a different size":      func(d []Disk) { d[1].Size++ },
		"another filesystem":    func(d []Disk) { d[2].Filesystem = "xfs" },
		"another by-id link":    func(d []Disk) { d[1].FSByIDName = "ata-EX_OTHER-part1" },
		"another parity size":   func(d []Disk) { d[0].Size += 512 },
		"another WWN":           func(d []Disk) { d[1].WWN = "0xdead" },
	} {
		changed := unraidDisks()
		mutate(changed)
		fresh, err := ResolveAdoption(changed, assignAll())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := plan.Matches(fresh); !errors.Is(err, ErrAdoptDiskChanged) {
			t.Errorf("%s: Matches = %v, want ErrAdoptDiskChanged", name, err)
		}
	}
	fewer, err := ResolveAdoption(unraidDisks(), assignAll()[:3])
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Matches(fewer); !errors.Is(err, ErrAdoptDiskChanged) {
		t.Errorf("a plan with no cache matches one with = %v", err)
	}
}

func TestAdoptionPlan_CheckDataRunsTheReadOnlyCheckOnDataDisksOnly(t *testing.T) {
	plan, err := ResolveAdoption(unraidDisks(), assignAll())
	if err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	if err := plan.CheckData(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range r.Calls() {
		got = append(got, c.Name+" "+strings.Join(c.Args, " "))
	}
	want := []string{"xfs_repair -n /dev/disk/by-id/ata-EX_DAT1-part1", "e2fsck -n /dev/disk/by-id/ata-EX_DAT2-part1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands = %v, want %v: never parity, cache or a whole disk", got, want)
	}

	bad := NewFakeRunner()
	bad.Script("e2fsck", []string{"-n", "/dev/disk/by-id/ata-EX_DAT2-part1"}, nil, errors.New("exit status 4"))
	if err := plan.CheckData(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Errorf("CheckData with a failing check = %v, want a refusal", err)
	}
}

func TestAdoptionPlan_ValidateRefusesWhatResolveWouldNeverBuild(t *testing.T) {
	plan, err := ResolveAdoption(unraidDisks(), assignAll())
	if err != nil {
		t.Fatal(err)
	}
	dup := plan
	dup.Data = append([]AdoptedDisk(nil), plan.Data...)
	dup.Data[1].FSUUID = dup.Data[0].FSUUID
	if err := dup.Validate(); !errors.Is(err, ErrAdoptUUIDShared) {
		t.Errorf("two data disks with one UUID = %v", err)
	}
	notAdopted := plan
	notAdopted.Data = append([]AdoptedDisk(nil), plan.Data...)
	notAdopted.Data[0].Adopt = false
	if err := notAdopted.Validate(); err == nil {
		t.Error("a data disk that is not adopted was accepted: it would be formatted")
	}
	twice := plan
	twice.Cache = &RecordedDisk{AssignedDisk: plan.Parity[0].AssignedDisk, Size: plan.Parity[0].Size}
	if err := twice.Validate(); !errors.Is(err, ErrDeviceAssignedTwice) {
		t.Errorf("a device as parity and cache = %v", err)
	}
}

func TestMountUnit_RenderIsReadOnlyForAnUnknownFilesystemToo(t *testing.T) {
	u := MountUnit{Where: "/mnt/disk1", UUID: "u", Filesystem: "ntfs", ReadOnly: true, What: "/dev/x"}
	if got := u.Render("/f"); !strings.Contains(got, "Options=ro,nofail") || strings.Contains(got, "defaults") {
		t.Errorf("a read-only unit of an unknown filesystem rendered read-write:\n%s", got)
	}
	if _, ok := ReadOnlyOptions("ntfs"); ok {
		t.Error("ReadOnlyOptions knows ntfs")
	}
	for fs, want := range map[FilesystemType]string{XFS: "ro,norecovery,", EXT4: "ro,noload,", BTRFS: "ro,rescue=nologreplay,"} {
		if o, ok := ReadOnlyOptions(fs); !ok || !strings.HasPrefix(o, want) {
			t.Errorf("ReadOnlyOptions(%s) = %q, %v, want %s…", fs, o, ok, want)
		}
	}
}

func TestDirectMounter_ReadOnlyUnitBoundToADeviceChecksSourceAndOptions(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "sdc1")
	other := filepath.Join(dir, "sdb1")
	link := filepath.Join(dir, "by-id-part1")
	for _, p := range []string{dev, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(dev, link); err != nil {
		t.Fatal(err)
	}
	where := filepath.Join(dir, "disk1")
	unit := MountUnit{Where: where, UUID: uuidData1, Filesystem: XFS, ReadOnly: true, What: link}
	mountArgs := []string{"-t", "xfs", "-o", "ro,norecovery,noatime,nodiratime,nosuid,nodev,noexec", link, where}

	good := NewFakeRunner()
	good.Script("mount", mountArgs, nil, nil)
	good.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte(uuidData1+"\n"), nil)
	good.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte(dev+"\n"), nil)
	good.Script("findmnt", []string{"-n", "-o", "OPTIONS", where}, []byte("ro,nosuid,nodev,noexec,noatime,norecovery\n"), nil)
	if err := (DirectMounter{Runner: good}).Mount(context.Background(), unit); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	for _, c := range good.Calls() {
		if c.Name == "mount" && (strings.Contains(strings.Join(c.Args, " "), "-U") || strings.Contains(strings.Join(c.Args, " "), "nofail")) {
			t.Errorf("a read-only device mount was %v: it must name the device and carry no nofail, which would turn a missing device into success", c.Args)
		}
	}

	// The same UUID on another device is not this disk.
	wrong := NewFakeRunner()
	wrong.Script("mount", mountArgs, nil, nil)
	wrong.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte(uuidData1+"\n"), nil)
	wrong.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte(other+"\n"), nil)
	wrong.Script("findmnt", []string{"-n", "-o", "OPTIONS", where}, []byte("ro\n"), nil)
	if err := (DirectMounter{Runner: wrong}).Mount(context.Background(), unit); err == nil || !strings.Contains(err.Error(), "mounted from") {
		t.Errorf("a mount whose source is another device with the same UUID = %v, want a refusal", err)
	}

	rw := NewFakeRunner()
	rw.Script("mount", mountArgs, nil, nil)
	rw.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte(uuidData1+"\n"), nil)
	rw.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte(dev+"\n"), nil)
	rw.Script("findmnt", []string{"-n", "-o", "OPTIONS", where}, []byte("rw,relatime\n"), nil)
	if err := (DirectMounter{Runner: rw}).Mount(context.Background(), unit); err == nil || !strings.Contains(err.Error(), "read-write") {
		t.Errorf("a read-only unit that came up read-write = %v, want a refusal", err)
	}

	// A mount that exits non-zero must not be excused by a mountpoint that
	// already holds the UUID unless it is the right device and read-only.
	retry := NewFakeRunner()
	retry.Script("mount", mountArgs, nil, errors.New("busy"))
	retry.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte(uuidData1+"\n"), nil)
	retry.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte(other+"\n"), nil)
	if err := (DirectMounter{Runner: retry}).Mount(context.Background(), unit); err == nil {
		t.Error("a failed mount over a mountpoint holding the same UUID from another device was excused")
	}
}

func TestConfirmMountedSource_DropsABtrfsSubvolumeSuffix(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "sdc1")
	if err := os.WriteFile(dev, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	r.Script("findmnt", []string{"-n", "-o", "SOURCE", "/mnt/disk1"}, []byte(dev+"[/sub]\n"), nil)
	if err := ConfirmMountedSource(context.Background(), r, "/mnt/disk1", dev); err != nil {
		t.Errorf("ConfirmMountedSource = %v", err)
	}
	if err := ConfirmMountedSource(context.Background(), r, "/mnt/disk1", filepath.Join(dir, "missing")); err == nil {
		t.Error("a device that does not resolve was accepted")
	}
}

// The array setup refuses the Unraid stick in every role, by identity or by
// device name, before a single format.
func TestFormatPlan_RefusesTheUnraidStickInEveryRole(t *testing.T) {
	stick := Disk{Serial: "STICK1", Size: 16 * GB, Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234", ByIDName: "usb-STICK_STICK1", WWN: ""}
	for _, role := range []string{"data", "parity", "cache"} {
		for _, withIdentity := range []bool{true, false} {
			p := NewFakeProvider()
			p.AddDisk("/dev/sdb", Disk{Serial: "D1", Size: 4 * TB, ByIDName: "ata-X_D1"})
			p.AddDisk("/dev/sdc", Disk{Serial: "P1", Size: 8 * TB, ByIDName: "ata-X_P1"})
			s := stick
			if !withIdentity {
				s.Serial, s.ByIDName = "", ""
			}
			p.AddDisk("/dev/sdd", s)
			a := AssignedDisk{Device: "/dev/sdd", Filesystem: XFS, Serial: s.Serial, ByIDName: s.ByIDName}
			plan := TopologyPlan{
				Parity: []AssignedDisk{{Device: "/dev/sdc", Filesystem: XFS, Serial: "P1", ByIDName: "ata-X_P1"}},
				Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, Serial: "D1", ByIDName: "ata-X_D1"}},
			}
			switch role {
			case "data":
				plan.Data = append(plan.Data, a)
			case "parity":
				plan.Parity = append(plan.Parity, a)
			case "cache":
				plan.Cache = &a
			}
			sizes := map[string]int64{"/dev/sdb": 4 * TB, "/dev/sdc": 8 * TB, "/dev/sdd": 16 * GB}
			if role == "parity" {
				sizes["/dev/sdd"] = 8 * TB
			}
			err := FormatPlan(context.Background(), p, NewFakeRunner(), plan, sizes, plan.Confirmation())
			if !errors.Is(err, ErrUnraidStick) {
				t.Errorf("%s, identity %v: FormatPlan = %v, want ErrUnraidStick", role, withIdentity, err)
			}
			if calls := p.FormatCalls(); len(calls) != 0 {
				t.Errorf("%s, identity %v: formatted %v before refusing the stick", role, withIdentity, calls)
			}
		}
	}
}

func TestLister_List_RecordsTheByIDLinkOfTheDisksOwnFilesystemDevice(t *testing.T) {
	l := newTestLister(t)
	udev := filepath.Join(t.TempDir(), "udev")
	mustMkdirAll(t, udev)
	l.UdevDataDir = udev
	mustWriteFile(t, filepath.Join(l.SysBlockDir, "sda1", "dev"), "8:1\n")
	mustWriteFile(t, filepath.Join(udev, "b8:1"), "I:1\nE:ID_FS_TYPE=xfs\nE:ID_FS_UUID="+uuidData1+"\n")
	byID := l.ByIDDir

	got, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// sda is identified by its wwn- link and only an ata- link names sda1: not
	// the disk's own identity family, so no binding is offered.
	if sda := got[0]; sda.FSDevice != "/dev/sda1" || sda.FSByIDName != "" {
		t.Errorf("sda = %+v, want no FSByIDName while only another link family names its partition", sda)
	}

	mustSymlink(t, "../../sda1", filepath.Join(byID, "wwn-0x5000cca0b1c2d3e4-part1"))
	got, err = l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sda := got[0]; sda.FSByIDName != "wwn-0x5000cca0b1c2d3e4-part1" {
		t.Errorf("sda.FSByIDName = %q, want the partition link of the wwn- identity", sda.FSByIDName)
	}
	if sdb := got[1]; sdb.FSByIDName != "" {
		t.Errorf("sdb.FSByIDName = %q, want none for a disk with no filesystem", sdb.FSByIDName)
	}
}

func TestOwnFSByIDName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
		disk  string
		want  string
	}{
		{"a partition link of the identity", []string{"ata-X_1-part1", "wwn-0x1-part1"}, "wwn-0x1", "wwn-0x1-part1"},
		{"the whole disk itself", []string{"ata-X_1"}, "ata-X_1", "ata-X_1"},
		{"another family only", []string{"ata-X_1-part1"}, "wwn-0x1", ""},
		{"a longer name that only starts the same", []string{"ata-X_12-part1"}, "ata-X_1", ""},
		{"no identity", []string{"ata-X_1-part1"}, "", ""},
		{"no links", nil, "ata-X_1", ""},
	} {
		if got := ownFSByIDName(tc.names, tc.disk); got != tc.want {
			t.Errorf("%s: ownFSByIDName(%v, %q) = %q, want %q", tc.name, tc.names, tc.disk, got, tc.want)
		}
	}
}
