package backup

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func recordedDisk(role string, index int, uuid, wwn string) store.ArrayDisk {
	return store.ArrayDisk{Role: role, RoleIndex: index, Device: "/dev/old-" + uuid, Filesystem: "xfs", FSUUID: uuid, WWN: wwn, Serial: "ser-" + uuid, Mountpoint: "/mnt/" + role + string(rune('0'+index))}
}

func stateOf(t *testing.T, mapped []MappedDisk, role string, index int) MappedDisk {
	t.Helper()
	for _, m := range mapped {
		if m.Recorded.Role == role && m.Recorded.RoleIndex == index {
			return m
		}
	}
	t.Fatalf("no %s %d in the mapping", role, index)
	return MappedDisk{}
}

func TestMapArrayDisks_StatesAndTheDeviceEachMatchedDiskIsOn(t *testing.T) {
	recorded := []store.ArrayDisk{
		recordedDisk("parity", 1, "u-p1", "wwn-p1"),
		recordedDisk("data", 1, "u-d1", "wwn-d1"),
		recordedDisk("data", 2, "u-d2", "wwn-d2"),
		recordedDisk("data", 3, "u-d3", "wwn-d3"),
		recordedDisk("data", 4, "u-d4", "wwn-d4"),
	}
	mapped := MapArrayDisks(recorded, []disk.Disk{
		// matched, on another device than it was
		{Device: "/dev/sdx", WWN: "wwn-p1", Serial: "ser-u-p1", FSUUID: "u-p1"},
		// data 1: its identity carries another filesystem now
		{Device: "/dev/sdb", WWN: "wwn-d1", Serial: "ser-u-d1", FSUUID: "u-other"},
		// data 2's filesystem is on a disk with another identity
		{Device: "/dev/sdc", WWN: "wwn-new", Serial: "ser-new", FSUUID: "u-d2"},
		// data 3 has two claimants: a disk and its clone
		{Device: "/dev/sdd", WWN: "wwn-d3", Serial: "ser-u-d3", FSUUID: "u-d3"},
		{Device: "/dev/sde", WWN: "wwn-d3", Serial: "ser-u-d3", FSUUID: "u-d3"},
		// data 4 is not attached
		// the boot disk is never a candidate, even carrying a recorded identity
		{Device: "/dev/sdz", WWN: "wwn-d4", Serial: "ser-u-d4", FSUUID: "u-d4", Boot: true},
	})

	for _, tc := range []struct {
		role   string
		index  int
		state  string
		device string
	}{
		{"parity", 1, DiskMatched, "/dev/sdx"},
		{"data", 1, DiskReplaced, "/dev/sdb"},
		{"data", 2, DiskReplaced, "/dev/sdc"},
		{"data", 3, DiskAmbiguous, ""},
		{"data", 4, DiskAbsent, ""},
	} {
		m := stateOf(t, mapped, tc.role, tc.index)
		if m.State != tc.state {
			t.Errorf("%s: state = %s, want %s", m.Name(), m.State, tc.state)
		}
		switch {
		case tc.device == "" && m.Attached != nil:
			t.Errorf("%s: attached = %s, want none", m.Name(), m.Attached.Device)
		case tc.device != "" && (m.Attached == nil || m.Attached.Device != tc.device):
			t.Errorf("%s: attached = %v, want %s", m.Name(), m.Attached, tc.device)
		}
	}

	got := MatchedMapping(mapped)
	if len(got.Disks) != 1 || got.Disks[0] != (DiskMappingEntry{Role: "parity", RoleIndex: 1, Device: "/dev/sdx"}) {
		t.Errorf("MatchedMapping = %+v, want only parity 1 on /dev/sdx: a replaced, ambiguous or absent disk is never confirmed", got)
	}
}

// A disk with no by-id link at all, as in the loop-device lab, and a
// weak-identity USB disk are matched by filesystem UUID and size (Q21).
func TestMapArrayDisks_WeakIdentitiesMatchByFilesystemAndSize(t *testing.T) {
	rec := store.ArrayDisk{Role: "data", RoleIndex: 1, Device: "/dev/loop1", Filesystem: "xfs", FSUUID: "u-lab", WeakIdentity: true, Size: 1 << 30, SizeSet: true, Mountpoint: "/mnt/disk1"}
	for _, tc := range []struct {
		name  string
		d     disk.Disk
		state string
	}{
		{"same filesystem and size", disk.Disk{Device: "/dev/loop9", WeakIdentity: true, FSUUID: "u-lab", Size: 1 << 30}, DiskMatched},
		{"same filesystem, another size", disk.Disk{Device: "/dev/loop9", WeakIdentity: true, FSUUID: "u-lab", Size: 2 << 30}, DiskReplaced},
		{"a strong identity is not a weak one", disk.Disk{Device: "/dev/loop9", WWN: "wwn-x", FSUUID: "u-lab", Size: 1 << 30}, DiskReplaced},
		{"another filesystem", disk.Disk{Device: "/dev/loop9", WeakIdentity: true, FSUUID: "u-x", Size: 1 << 30}, DiskAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := MapArrayDisks([]store.ArrayDisk{rec}, []disk.Disk{tc.d})[0]
			if m.State != tc.state {
				t.Fatalf("state = %s, want %s", m.State, tc.state)
			}
		})
	}

	t.Run("a clone of a weak-identity disk makes both ambiguous", func(t *testing.T) {
		m := MapArrayDisks([]store.ArrayDisk{rec}, []disk.Disk{
			{Device: "/dev/loop8", WeakIdentity: true, FSUUID: "u-lab", Size: 1 << 30},
			{Device: "/dev/loop9", WeakIdentity: true, FSUUID: "u-lab", Size: 1 << 30},
		})[0]
		if m.State != DiskAmbiguous {
			t.Fatalf("state = %s, want ambiguous, never the first of them", m.State)
		}
	})
}

func TestCheckConfirmed_TheConfirmationMustBeExactlyTheCurrentMapping(t *testing.T) {
	recorded := []store.ArrayDisk{
		recordedDisk("parity", 1, "u-p1", "wwn-p1"),
		recordedDisk("data", 1, "u-d1", "wwn-d1"),
		recordedDisk("data", 2, "u-d2", "wwn-d2"),
	}
	mapped := MapArrayDisks(recorded, []disk.Disk{
		{Device: "/dev/sdb", WWN: "wwn-p1", Serial: "ser-u-p1", FSUUID: "u-p1"},
		{Device: "/dev/sdc", WWN: "wwn-d1", Serial: "ser-u-d1", FSUUID: "u-d1"},
	})
	good := DiskMapping{Disks: []DiskMappingEntry{
		{Role: "parity", RoleIndex: 1, Device: "/dev/sdb"},
		{Role: "data", RoleIndex: 1, Device: "/dev/sdc"},
	}}
	if err := checkConfirmed(mapped, good); err != nil {
		t.Fatalf("the current mapping was refused: %v", err)
	}

	for _, tc := range []struct {
		name  string
		m     DiskMapping
		wants string
	}{
		{"a matched disk left out", DiskMapping{Disks: good.Disks[:1]}, "was not confirmed"},
		{"a disk on another device", DiskMapping{Disks: []DiskMappingEntry{
			{Role: "parity", RoleIndex: 1, Device: "/dev/sdb"}, {Role: "data", RoleIndex: 1, Device: "/dev/sdd"}}}, "not the confirmed /dev/sdd"},
		{"an absent disk confirmed", DiskMapping{Disks: append([]DiskMappingEntry{{Role: "data", RoleIndex: 2, Device: "/dev/sdc"}}, good.Disks...)}, "but is absent"},
		{"a slot the archive does not record", DiskMapping{Disks: append([]DiskMappingEntry{{Role: "cache", RoleIndex: 1, Device: "/dev/sdd"}}, good.Disks...)}, "records no such disk"},
		{"a slot confirmed twice", DiskMapping{Disks: append([]DiskMappingEntry{good.Disks[0]}, good.Disks...)}, "confirmed twice"},
		{"nothing confirmed", DiskMapping{}, "was not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkConfirmed(mapped, tc.m)
			var stale *DiskMappingStaleError
			if !errors.As(err, &stale) || !strings.Contains(err.Error(), tc.wants) {
				t.Fatalf("checkConfirmed = %v, want a *DiskMappingStaleError containing %q", err, tc.wants)
			}
		})
	}

	if err := checkConfirmed(nil, DiskMapping{}); err != nil {
		t.Errorf("an archive that records no disks is confirmed by an empty mapping: %v", err)
	}
}

func TestBareMetalConfirm_ANilMappingIsRequiredNotEmpty(t *testing.T) {
	b := &BareMetal{recorded: []store.ArrayDisk{recordedDisk("data", 1, "u-d1", "wwn-d1")}}
	if _, err := b.Confirm(nil, nil); !errors.Is(err, ErrDiskMappingRequired) {
		t.Fatalf("Confirm(nil) = %v, want ErrDiskMappingRequired", err)
	}
	if _, err := b.Confirm(nil, &DiskMapping{}); err != nil {
		t.Fatalf("an empty mapping for an archive whose only disk is absent = %v, want confirmed", err)
	}
}

func TestParseDiskMapping(t *testing.T) {
	m, err := ParseDiskMapping(`{"disks":[{"role":"data","roleIndex":2,"device":"/dev/sdc"},{"role":"parity","roleIndex":1,"device":"/dev/sdb"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Disks) != 2 || m.Disks[0] != (DiskMappingEntry{Role: "data", RoleIndex: 2, Device: "/dev/sdc"}) || m.Disks[1].Role != "parity" {
		t.Fatalf("mapping = %+v", m)
	}
	if m, err := ParseDiskMapping(` {"disks":[]} `); err != nil || m.Disks == nil || len(m.Disks) != 0 {
		t.Errorf("an empty list = %+v, %v, want an empty confirmed mapping", m, err)
	}
	for name, doc := range map[string]string{
		"not json":             `x`,
		"empty":                ``,
		"no disks list":        `{}`,
		"a null list":          `{"disks":null}`,
		"an unknown field":     `{"disks":[],"x":1}`,
		"an unknown entry key": `{"disks":[{"role":"data","roleIndex":1,"device":"/dev/sdb","wwn":"x"}]}`,
		"a bad role":           `{"disks":[{"role":"spare","roleIndex":1,"device":"/dev/sdb"}]}`,
		"a role index of zero": `{"disks":[{"role":"data","roleIndex":0,"device":"/dev/sdb"}]}`,
		"no device":            `{"disks":[{"role":"data","roleIndex":1}]}`,
		"trailing content":     `{"disks":[]}{"disks":[]}`,
		"the wrong type":       `{"disks":[{"role":"data","roleIndex":"1","device":"/dev/sdb"}]}`,
	} {
		if _, err := ParseDiskMapping(doc); !errors.Is(err, ErrInvalidDiskMapping) {
			t.Errorf("%s: ParseDiskMapping(%q) = %v, want ErrInvalidDiskMapping", name, doc, err)
		}
	}
}

const (
	nvmeByID      = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	nvmeCacheByID = nvmeByID + "-part3"
	nvmeCacheUUID = "u-cache"
)

func recordedBootCache() store.ArrayDisk {
	return store.ArrayDisk{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme0n1p3", Filesystem: "ext4", FSUUID: nvmeCacheUUID,
		Serial: "S4EWNX0M123456X", ByIDName: nvmeCacheByID, Mountpoint: "/mnt/cache"}
}

func sharedNVMe(cache disk.BootPartition) disk.Disk {
	return disk.Disk{
		Device: "/dev/nvme0n1", Serial: "S4EWNX0M123456X", ByIDName: nvmeByID, Boot: true,
		Partitions: []disk.BootPartition{
			{Device: "/dev/nvme0n1p1", ByIDName: nvmeByID + "-part1", Filesystem: "vfat", FSUUID: "u-efi"},
			{Device: "/dev/nvme0n1p2", ByIDName: nvmeByID + "-part2", Filesystem: "ext4", FSUUID: "u-root"},
			cache,
		},
	}
}

func cachePart(fsType, uuid string) disk.BootPartition {
	return disk.BootPartition{Device: "/dev/nvme0n1p3", Size: 900 << 30, ByIDName: nvmeCacheByID, PartUUID: "5b3d9e0a-03", Filesystem: fsType, FSUUID: uuid}
}

func TestMapArrayDisks_ACacheOnABootDiskPartition(t *testing.T) {
	recorded := []store.ArrayDisk{recordedDisk("data", 1, "u-d1", "wwn-d1"), recordedBootCache()}
	data := disk.Disk{Device: "/dev/sdb", WWN: "wwn-d1", Serial: "ser-u-d1", FSUUID: "u-d1"}

	for _, tc := range []struct {
		name   string
		attach []disk.Disk
		state  string
		device string
	}{
		{"the same layout", []disk.Disk{data, sharedNVMe(cachePart("ext4", nvmeCacheUUID))}, DiskMatched, "/dev/nvme0n1p3"},
		{"the partition renumbered to another kernel name", []disk.Disk{data, sharedNVMe(disk.BootPartition{
			Device: "/dev/nvme0n1p7", ByIDName: nvmeCacheByID, Filesystem: "ext4", FSUUID: nvmeCacheUUID})}, DiskMatched, "/dev/nvme0n1p7"},
		{"another filesystem on the partition", []disk.Disk{data, sharedNVMe(cachePart("ext4", "u-other"))}, DiskReplaced, "/dev/nvme0n1p3"},
		{"no filesystem on the partition", []disk.Disk{data, sharedNVMe(cachePart("", ""))}, DiskReplaced, "/dev/nvme0n1p3"},
		{"no partition on the boot disk", []disk.Disk{data, func() disk.Disk {
			d := sharedNVMe(cachePart("", ""))
			d.Partitions = d.Partitions[:2]
			return d
		}()}, DiskAbsent, ""},
		{"no boot disk with partitions listed", []disk.Disk{data, {Device: "/dev/nvme0n1", Serial: "S4EWNX0M123456X", ByIDName: nvmeByID, Boot: true}}, DiskAbsent, ""},
		{"another boot disk", []disk.Disk{data, func() disk.Disk {
			d := sharedNVMe(cachePart("ext4", "u-other"))
			d.Serial = "OTHER"
			return d
		}()}, DiskAbsent, ""},
		{"another boot disk holding the filesystem", []disk.Disk{data, func() disk.Disk {
			d := sharedNVMe(cachePart("ext4", nvmeCacheUUID))
			d.Serial = "OTHER"
			return d
		}()}, DiskReplaced, "/dev/nvme0n1p3"},
		{"the filesystem on a partition of another by-id name", []disk.Disk{data, sharedNVMe(disk.BootPartition{
			Device: "/dev/nvme0n1p4", ByIDName: nvmeByID + "-part4", Filesystem: "ext4", FSUUID: nvmeCacheUUID})}, DiskReplaced, "/dev/nvme0n1p4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := stateOf(t, MapArrayDisks(recorded, tc.attach), store.ArrayRoleCache, 1)
			if m.State != tc.state {
				t.Fatalf("cache state = %s, want %s", m.State, tc.state)
			}
			switch {
			case tc.device == "" && m.Attached != nil:
				t.Fatalf("attached = %s, want none", m.Attached.Device)
			case tc.device != "" && (m.Attached == nil || m.Attached.Device != tc.device):
				t.Fatalf("attached = %v, want %s", m.Attached, tc.device)
			}
			if m.Attached != nil && m.Attached.Device == "/dev/nvme0n1" {
				t.Fatal("the whole boot disk was matched")
			}
		})
	}

	t.Run("a matched partition is confirmed on its own device", func(t *testing.T) {
		mapped := MapArrayDisks(recorded, []disk.Disk{data, sharedNVMe(cachePart("ext4", nvmeCacheUUID))})
		got := MatchedMapping(mapped)
		want := []DiskMappingEntry{{Role: "data", RoleIndex: 1, Device: "/dev/sdb"}, {Role: "cache", RoleIndex: 1, Device: "/dev/nvme0n1p3"}}
		if len(got.Disks) != 2 || got.Disks[0] != want[0] || got.Disks[1] != want[1] {
			t.Fatalf("MatchedMapping = %+v, want %+v", got, want)
		}
		if err := checkConfirmed(mapped, got); err != nil {
			t.Fatalf("checkConfirmed: %v", err)
		}
	})

	t.Run("a recorded filesystem UUID of none matches a partition that has one", func(t *testing.T) {
		rec := recordedBootCache()
		rec.FSUUID = ""
		m := MapArrayDisks([]store.ArrayDisk{rec}, []disk.Disk{sharedNVMe(cachePart("ext4", nvmeCacheUUID))})[0]
		if m.State != DiskMatched {
			t.Fatalf("state = %s, want matched", m.State)
		}
	})

	t.Run("a boot disk and its clone make the partition ambiguous", func(t *testing.T) {
		a := sharedNVMe(cachePart("ext4", nvmeCacheUUID))
		b := sharedNVMe(cachePart("ext4", nvmeCacheUUID))
		b.Device = "/dev/nvme1n1"
		m := MapArrayDisks([]store.ArrayDisk{recordedBootCache()}, []disk.Disk{a, b})[0]
		if m.State != DiskAmbiguous || m.Attached != nil {
			t.Fatalf("state = %s attached = %v, want ambiguous and none", m.State, m.Attached)
		}
	})
}

func TestMapArrayDisks_OnlyTheCacheSlotEverSitsOnTheBootDisk(t *testing.T) {
	boot := sharedNVMe(cachePart("ext4", nvmeCacheUUID))
	asData := recordedBootCache()
	asData.Role, asData.Mountpoint = store.ArrayRoleData, "/mnt/disk1"
	asParity := recordedBootCache()
	asParity.Role, asParity.Mountpoint = store.ArrayRoleParity, "/mnt/parity1"
	wholeBoot := store.ArrayDisk{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/nvme0n1", Filesystem: "ext4", FSUUID: "u-whole",
		Serial: "S4EWNX0M123456X", ByIDName: nvmeByID, Mountpoint: "/mnt/disk2"}
	wholeBootCache := store.ArrayDisk{Role: store.ArrayRoleCache, RoleIndex: 2, Device: "/dev/nvme0n1", Filesystem: "ext4", FSUUID: "u-whole2",
		Serial: "S4EWNX0M123456X", ByIDName: nvmeByID, Mountpoint: "/mnt/cache2"}

	mapped := MapArrayDisks([]store.ArrayDisk{asData, asParity, wholeBoot, wholeBootCache}, []disk.Disk{boot})
	for _, m := range mapped {
		if m.State != DiskAbsent || m.Attached != nil {
			t.Errorf("%s = %s attached %v, want absent: the boot disk and its partitions are never a data or parity disk, nor a whole-disk cache", m.Name(), m.State, m.Attached)
		}
	}
}

func TestMapArrayDisks_ACachePartitionIsNeverMatchedByAWholeDisk(t *testing.T) {
	whole := disk.Disk{Device: "/dev/nvme1n1", Serial: "S4EWNX0M123456X", ByIDName: nvmeByID, FSUUID: nvmeCacheUUID}
	m := MapArrayDisks([]store.ArrayDisk{recordedBootCache()}, []disk.Disk{whole})[0]
	if m.State != DiskAbsent || m.Attached != nil {
		t.Fatalf("state = %s attached %v, want absent: the whole disk is not the partition", m.State, m.Attached)
	}
}
