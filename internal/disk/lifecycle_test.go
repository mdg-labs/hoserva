package disk

import (
	"context"
	"errors"
	"testing"
)

func TestNextDataMountpoint_FirstDiskIsOne(t *testing.T) {
	if got := NextDataMountpoint(nil); got != "/mnt/disk1" {
		t.Fatalf("NextDataMountpoint(nil): got %q, want /mnt/disk1", got)
	}
}

func TestNextDataMountpoint_SkipsExistingSlots(t *testing.T) {
	used := []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"}
	if got := NextDataMountpoint(used); got != "/mnt/disk4" {
		t.Fatalf("NextDataMountpoint(%v): got %q, want /mnt/disk4", used, got)
	}
}

// TestNextDataMountpoint_ReusesAFreedSlot is doc 02 §4's own "next free"
// wording: a disk removed earlier leaves a gap that the next addition
// fills, rather than the assignment always growing past the highest N
// ever used.
func TestNextDataMountpoint_ReusesAFreedSlot(t *testing.T) {
	used := []string{"/mnt/disk1", "/mnt/disk3"}
	if got := NextDataMountpoint(used); got != "/mnt/disk2" {
		t.Fatalf("NextDataMountpoint(%v): got %q, want /mnt/disk2", used, got)
	}
}

func TestNextDataMountpoint_IgnoresNonDataMountpoints(t *testing.T) {
	used := []string{"/mnt/parity1", "/mnt/cache", "/mnt/disk1"}
	if got := NextDataMountpoint(used); got != "/mnt/disk2" {
		t.Fatalf("NextDataMountpoint(%v): got %q, want /mnt/disk2", used, got)
	}
}

func TestFormatForAddition_FormatsAFreshDisk(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS}
	if err := FormatForAddition(context.Background(), p, NewFakeRunner(), a); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	fs, ok := p.FormattedAs("/dev/sdb")
	if !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdb): got (%v, %v), want (xfs, true)", fs, ok)
	}
}

func TestFormatForAddition_AdoptedDiskIsCheckedNotFormatted(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	r := NewFakeRunner()
	r.Script("xfs_repair", []string{"-n", "/dev/sdb"}, nil, nil)

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, Adopt: true}
	if err := FormatForAddition(context.Background(), p, r, a); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatForAddition formatted an adopted disk, not just checked it")
	}
	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "xfs_repair" {
		t.Fatalf("Calls: got %+v, want one xfs_repair call", calls)
	}
}

// TestFormatForAddition_AdoptedDiskIsCheckedThroughItsIdentityPath is this
// issue's own property for the adopt-check branch: when the disk's
// identity is known, AdoptCheck runs against its by-id path, not the
// plain device path — the same binding FormatForAddition's format branch
// gets — so a udev reassignment before AdoptCheck's own read-only check
// runs still checks the right physical disk.
func TestFormatForAddition_AdoptedDiskIsCheckedThroughItsIdentityPath(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB, WWN: "0xabc123", ByIDName: "wwn-0xabc123"})
	r := NewFakeRunner()
	r.Script("xfs_repair", []string{"-n", "/dev/disk/by-id/wwn-0xabc123"}, nil, nil)

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, Adopt: true, WWN: "0xabc123", ByIDName: "wwn-0xabc123"}
	if err := FormatForAddition(context.Background(), p, r, a); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "xfs_repair" || !equalArgs(calls[0].Args, []string{"-n", "/dev/disk/by-id/wwn-0xabc123"}) {
		t.Fatalf("Calls: got %+v, want one xfs_repair call against the by-id path", calls)
	}
}

// TestFormatForAddition_FormatsTheIdentityWhereverItMoved mirrors
// FormatAssigned's own central safety property (format_test.go) for the
// single-disk addition/replacement path: a device confirmed by WWN is
// still formatted, through its stable by-id path, once that WWN has
// moved to a different path — never whatever now sits at the old one.
func TestFormatForAddition_FormatsTheIdentityWhereverItMoved(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB, WWN: "0xabc123", ByIDName: "wwn-0xabc123"})
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB}) // a different disk now sits at the confirmed path

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123", ByIDName: "wwn-0xabc123"}
	if err := FormatForAddition(context.Background(), p, NewFakeRunner(), a); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdc): got (%v, %v), want (xfs, true) — the confirmed disk, wherever it moved", fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatForAddition formatted /dev/sdb, the different disk that now sits at the confirmed path")
	}
}

// TestFormatForAddition_ClosesTheRaceBetweenIdentityCheckAndFormat mirrors
// FormatAssigned's own race-closing test (format_test.go) for the
// single-disk addition/replacement path.
func TestFormatForAddition_ClosesTheRaceBetweenIdentityCheckAndFormat(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdb", Disk{Size: 4 * TB, WWN: "0xabc123", ByIDName: "wwn-0xabc123"})

	p.AfterList = func() {
		p.Reassign("/dev/sdb", "/dev/sdc")
		p.AddDisk("/dev/sdb", Disk{Size: 4 * TB})
	}

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123", ByIDName: "wwn-0xabc123"}
	if err := FormatForAddition(context.Background(), p, NewFakeRunner(), a); err != nil {
		t.Fatalf("FormatForAddition: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sdc"); !ok || fs != XFS {
		t.Fatalf("FormattedAs(/dev/sdc): got (%v, %v), want (xfs, true) — the originally confirmed disk, wherever it raced to", fs, ok)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatForAddition formatted /dev/sdb — the unrelated disk that raced into the confirmed path mid-call")
	}
}

func TestFormatForAddition_RefusesWhenIdentityDisappears(t *testing.T) {
	p := NewFakeProvider()
	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}
	err := FormatForAddition(context.Background(), p, NewFakeRunner(), a)
	if !errors.Is(err, ErrDiskIdentityChanged) {
		t.Fatalf("FormatForAddition: got %v, want ErrDiskIdentityChanged", err)
	}
}

// TestFormatForAddition_RefusesWhenIdentityMovedWithoutAByIDLink mirrors
// FormatAssigned's own property (format_test.go) for the single-disk
// addition/replacement path: identity known, no by-id link to bind a
// path to, and the identity has moved off a.Device — there is nothing
// safe to format, so this must refuse rather than trust the stale
// a.Device unverified.
func TestFormatForAddition_RefusesWhenIdentityMovedWithoutAByIDLink(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB, WWN: "0xabc123"})

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}
	err := FormatForAddition(context.Background(), p, NewFakeRunner(), a)
	if !errors.Is(err, ErrDiskIdentityChanged) {
		t.Fatalf("FormatForAddition: got %v, want ErrDiskIdentityChanged", err)
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("FormatForAddition formatted /dev/sdc, which has no by-id link to safely bind to")
	}
}

// TestFormatForAddition_RefusesTheBootDevice mirrors FormatAssigned's own
// boot-disk property (format_test.go) for the single-disk
// addition/replacement path. FakeProvider models no boot guard at all
// (fake.go), so this runs against the real LinuxProvider and its
// synthetic sysfs tree, where /dev/sda is the boot disk.
func TestFormatForAddition_RefusesTheBootDevice(t *testing.T) {
	p, runner := newTestProvider(t)
	a := DiskAddition{Device: "/dev/sda", Filesystem: XFS, WWN: "0x5000cca0b1c2d3e4", ByIDName: "wwn-0x5000cca0b1c2d3e4"}

	if err := FormatForAddition(context.Background(), p, runner, a); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("FormatForAddition(boot device): got %v, want ErrBootDevice", err)
	}
	if calls := runner.Calls(); len(calls) != 0 {
		t.Fatalf("FormatForAddition(boot device) ran a command: %+v, want none", calls)
	}
}

func TestDataDiskMountUnit_BuildsExpectedUnit(t *testing.T) {
	u := DataDiskMountUnit("/mnt/disk4", "1234-5678", XFS)
	if u.Where != "/mnt/disk4" || u.UUID != "1234-5678" || u.Filesystem != XFS {
		t.Fatalf("DataDiskMountUnit: got %+v", u)
	}
	if u.Description == "" {
		t.Fatal("DataDiskMountUnit: Description is empty")
	}
}
