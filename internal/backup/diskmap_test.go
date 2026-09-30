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
