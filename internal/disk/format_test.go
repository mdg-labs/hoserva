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

// TestFormatAssigned_FormatsTheIdentityWhereverItMoved is this issue's own
// central safety property: a plan built against /dev/sdb's WWN, run after
// that WWN has since moved to /dev/sdc (a controller reorder or a swapped
// cable between discovery and execution), still formats the confirmed
// disk — wherever it now is — through its stable by-id path, and never
// touches whatever now happens to sit at /dev/sdb.
func TestFormatAssigned_FormatsTheIdentityWhereverItMoved(t *testing.T) {
	p := NewFakeProvider()
	// The confirmed disk, now at a different path.
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB, WWN: "0xabc123", ByIDName: "wwn-0xabc123"})
	// A different disk has since taken over the old path — the identity
	// check must never mistake it for the one that was confirmed.
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123", ByIDName: "wwn-0xabc123"}},
	}
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})

	if err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS); err != nil {
		t.Fatalf("FormatAssigned: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdc): got (%v, %v), want (xfs, true) — the confirmed disk, wherever it moved", fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdb, the different disk that now sits at the confirmed path")
	}
}

// TestFormatAssigned_ClosesTheRaceBetweenIdentityCheckAndFormat is this
// issue's central safety-critical property: a udev reassignment landing
// in the window between FormatAssigned's own identity check (which reads
// p.List) and the Format call that follows it must not change which
// physical disk gets formatted. AfterList fires the reassignment exactly
// in that window — the same window a real udev event could land in
// between the check and the moment mkfs actually opens a device — proving
// the fix binds to the identity itself, not to whatever path a check
// happened to observe moments earlier.
func TestFormatAssigned_ClosesTheRaceBetweenIdentityCheckAndFormat(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB, WWN: "0xabc123", ByIDName: "wwn-0xabc123"})
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123", ByIDName: "wwn-0xabc123"}},
	}

	raced := false
	p.AfterList = func() {
		if raced {
			return
		}
		raced = true
		// The confirmed disk is renumbered to /dev/sdc, and a brand new,
		// unrelated disk takes over /dev/sdb — all after the identity
		// check already read the inventory, before Format runs.
		p.Reassign("/dev/sdb", "/dev/sdc")
		p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	}

	if err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS); err != nil {
		t.Fatalf("FormatAssigned: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdc): got (%v, %v), want (xfs, true) — the originally confirmed disk, wherever it raced to", fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdb — the unrelated disk that raced into the confirmed path mid-call")
	}
}

// TestFormatAssigned_RefusesWhenIdentityDisappears mirrors the same
// property when no disk at all currently matches the confirmed identity
// — a disk pulled between discovery and execution.
func TestFormatAssigned_RefusesWhenIdentityDisappears(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}},
	}

	err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS)
	if !errors.Is(err, ErrDiskIdentityChanged) {
		t.Fatalf("FormatAssigned: got %v, want ErrDiskIdentityChanged", err)
	}
}

// TestFormatAssigned_RefusesTheBootDevice is this issue's own boot-disk
// safety property: even once a disk's identity is known and its format
// target resolves to a /dev/disk/by-id path, the boot-disk refusal must
// still fire — a by-id path never compares equal to the /dev/sdX path a
// naive boot check would know the disk by, so the refusal has to happen
// against the matched disk's own Boot flag, not a path comparison.
// FakeProvider models no boot guard at all (fake.go), so this runs
// against the real LinuxProvider and its synthetic sysfs tree, where
// /dev/sda is the boot disk.
func TestFormatAssigned_RefusesTheBootDevice(t *testing.T) {
	p, runner := newTestProvider(t)
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS, WWN: "0x5000cca0b1c2d3e4", ByIDName: "wwn-0x5000cca0b1c2d3e4"}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}

	if err := FormatAssigned(context.Background(), p, plan, "/dev/sda", XFS); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("FormatAssigned(boot device): got %v, want ErrBootDevice", err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("FormatAssigned(boot device) ran a command: %+v, want none", calls)
	}
}

// TestFormatAssigned_RefusesWhenIdentityMovedWithoutAByIDLink covers the
// identity-known-but-no-by-id-link case: there is nothing to bind a path
// to, so a disk whose identity has moved off dev must still be refused
// rather than formatted through the stale, unverified dev.
func TestFormatAssigned_RefusesWhenIdentityMovedWithoutAByIDLink(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	// The confirmed identity now lives at /dev/sdc, but with no by-id
	// link recorded.
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB, WWN: "0xabc123"})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}},
	}

	err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS)
	if !errors.Is(err, ErrDiskIdentityChanged) {
		t.Fatalf("FormatAssigned: got %v, want ErrDiskIdentityChanged", err)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdb, the stale path, despite the identity having moved")
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("FormatAssigned formatted /dev/sdc, which has no by-id link to safely bind to")
	}
}

// TestFormatAssigned_ProceedsWhenIdentityStillMatches is the non-drift
// case: the disk confirmed by WWN is still found at the same path, so
// formatting proceeds exactly as it would have before identity tracking
// existed.
func TestFormatAssigned_ProceedsWhenIdentityStillMatches(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB, WWN: "0xabc123"})
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}},
	}
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})

	if err := FormatAssigned(context.Background(), p, plan, "/dev/sdb", XFS); err != nil {
		t.Fatalf("FormatAssigned: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sdb"); !ok || fs != XFS {
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

// TestFormatPlan_FailingAdoptCheckNeverFormatsAnEarlierDisk is the
// two-phase property this issue calls for: AdoptCheck's own guarantee is
// read-only, but formatCommand isn't — parity disks are never adopted
// (Validate) and so are always processed first in assignedDisks() order.
// A later data disk's failing AdoptCheck must be discovered before that
// earlier parity disk is formatted, not after.
func TestFormatPlan_FailingAdoptCheckNeverFormatsAnEarlierDisk(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 8 * TB})
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB})
	r := NewFakeRunner()
	r.Script("xfs_repair", []string{"-n", "/dev/sdc"}, nil, errors.New("filesystem corrupt"))

	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data: []AssignedDisk{
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sdc", Filesystem: XFS, Adopt: true},
		},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB, "/dev/sdc": 4 * TB}

	if err := FormatPlan(context.Background(), p, r, plan, sizes, plan.Confirmation()); err == nil {
		t.Fatal("FormatPlan: expected an error from the failing AdoptCheck")
	}

	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("FormatPlan formatted the parity disk before a later disk's AdoptCheck failed")
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatPlan formatted a data disk before a later disk's AdoptCheck failed")
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
