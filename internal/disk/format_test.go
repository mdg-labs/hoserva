package disk

import (
	"context"
	"errors"
	"testing"
)

func TestFormatAssigned_FormatsAnAssignedDevice(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})

	if err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS); err != nil {
		t.Fatalf("FormatAssigned: %v", err)
	}
	fs, ok := p.FormattedAs("/dev/sdb")
	if !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdb): got (%v, %v), want (xfs, true)", fs, ok)
	}
}

// TestFormatAssigned_RefusesADeviceNotInThePlan is this issue's central
// safety-critical property (doc 03 §3.1 step 6, CLAUDE.md safety rules):
// a device that exists and is even known to the provider, but was never
// explicitly assigned a role in this plan, must never be formatted.
func TestFormatAssigned_RefusesADeviceNotInThePlan(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB}) // parity, assigned
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB}) // data, assigned
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB}) // present on the system, NOT assigned
	p.AddDisk("/dev/sdd", Disk{Size: 4 * TB}) // present on the system, NOT assigned

	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}

	for _, dev := range []string{"/dev/sdc", "/dev/sdd", "/dev/nonexistent"} {
		if err := FormatAssigned(context.Background(), p, plan, dev, XFS); !errors.Is(err, ErrDiskNotAssigned) {
			t.Fatalf("FormatAssigned(%s): got %v, want ErrDiskNotAssigned", dev, err)
		}
	}

	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdc despite it not being in the plan")
	}
	if _, ok := p.FormattedAs("/dev/sdd"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdd despite it not being in the plan")
	}
}

func TestFormatPlan_FormatsEveryNonAdoptedDiskAndChecksAdoptedOnes(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB})
	r := NewFakeRunner()
	r.Script("xfs_repair", []string{"-n", "/dev/sdc"}, nil, nil)

	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data: []AssignedDisk{
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sdc", Filesystem: XFS, Adopt: true},
		},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB, "/dev/sdc": 4 * TB}

	if err := FormatPlan(context.Background(), p, r, plan, sizes, plan.Confirmation()); err != nil {
		t.Fatalf("FormatPlan: %v", err)
	}

	if fs, ok := p.FormattedAs("/dev/sda"); !ok || fs != XFS {
		t.Fatalf("parity disk not formatted: (%v, %v)", fs, ok)
	}
	if fs, ok := p.FormattedAs("/dev/sdb"); !ok || fs != XFS {
		t.Fatalf("data disk not formatted: (%v, %v)", fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("adopted disk was formatted, not just checked")
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "xfs_repair" {
		t.Fatalf("Calls: got %+v, want one xfs_repair call", calls)
	}
}

func TestFormatPlan_RefusesWrongConfirmation(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB}

	if err := FormatPlan(context.Background(), p, NewFakeRunner(), plan, sizes, "yes"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("FormatPlan: got %v, want ErrConfirmationMismatch", err)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("FormatPlan formatted a disk despite a confirmation mismatch")
	}
}

func TestFormatPlan_RefusesAnInvalidPlanBeforeFormattingAnything(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 4 * TB})
	p.AddDisk("/dev/sdb", Disk{Size: 8 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}}, // smaller than the data disk
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 4 * TB, "/dev/sdb": 8 * TB}

	if err := FormatPlan(context.Background(), p, NewFakeRunner(), plan, sizes, plan.Confirmation()); !errors.Is(err, ErrParityTooSmall) {
		t.Fatalf("FormatPlan: got %v, want ErrParityTooSmall", err)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatPlan formatted the data disk despite the plan being invalid")
	}
}

func TestAdoptCheck_RefusesADiskThatFailsItsCheck(t *testing.T) {
	r := NewFakeRunner()
	r.Script("e2fsck", []string{"-n", "/dev/sdb"}, []byte("errors left uncorrected"), errors.New("exit status 4"))

	if err := AdoptCheck(context.Background(), r, "/dev/sdb", EXT4); err == nil {
		t.Fatal("AdoptCheck: got nil error for a failing check")
	}
}

func TestAdoptCheck_ArgvPerFilesystem(t *testing.T) {
	cases := []struct {
		fs   FilesystemType
		name string
		args []string
	}{
		{XFS, "xfs_repair", []string{"-n", "/dev/sdb"}},
		{EXT4, "e2fsck", []string{"-n", "/dev/sdb"}},
		{BTRFS, "btrfs", []string{"check", "--readonly", "/dev/sdb"}},
	}
	for _, c := range cases {
		r := NewFakeRunner()
		r.Script(c.name, c.args, nil, nil)
		if err := AdoptCheck(context.Background(), r, "/dev/sdb", c.fs); err != nil {
			t.Fatalf("AdoptCheck(%s): %v", c.fs, err)
		}
		calls := r.Calls()
		if len(calls) != 1 || calls[0].Name != c.name || !equalArgs(calls[0].Args, c.args) {
			t.Fatalf("AdoptCheck(%s) calls: got %+v, want [%s %v]", c.fs, calls, c.name, c.args)
		}
	}
}
