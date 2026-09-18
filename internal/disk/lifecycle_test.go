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

// TestFormatForAddition_RefusesWhenIdentityMovedToADifferentPath mirrors
// FormatAssigned's own central safety property (format_test.go) for the
// single-disk addition/replacement path: a device confirmed by WWN must
// never be formatted once that WWN has moved to a different path.
func TestFormatForAddition_RefusesWhenIdentityMovedToADifferentPath(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sdc", Disk{Size: 4 * TB, WWN: "0xabc123"})

	a := DiskAddition{Device: "/dev/sdb", Filesystem: XFS, WWN: "0xabc123"}
	err := FormatForAddition(context.Background(), p, NewFakeRunner(), a)
	if !errors.Is(err, ErrDiskIdentityChanged) {
		t.Fatalf("FormatForAddition: got %v, want ErrDiskIdentityChanged", err)
	}
	if _, ok := p.FormattedAs("/dev/sdb"); ok {
		t.Fatal("FormatForAddition formatted /dev/sdb despite its identity having moved elsewhere")
	}
	if _, ok := p.FormattedAs("/dev/sdc"); ok {
		t.Fatal("FormatForAddition formatted the disk now holding the identity, without a fresh confirmation")
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

func TestDataDiskMountUnit_BuildsExpectedUnit(t *testing.T) {
	u := DataDiskMountUnit("/mnt/disk4", "1234-5678", XFS)
	if u.Where != "/mnt/disk4" || u.UUID != "1234-5678" || u.Filesystem != XFS {
		t.Fatalf("DataDiskMountUnit: got %+v", u)
	}
	if u.Description == "" {
		t.Fatal("DataDiskMountUnit: Description is empty")
	}
}
