package job

import (
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestDataDiskLabelForMountpoint_AgreesWithRenderAcrossARoleIndexGap is
// #360's own acceptance criterion: the "dN" label this returns for a
// display preview must be the same label parity.Layout.Render actually
// puts in snapraid.conf for that disk, even once an earlier removal
// (#358) has left role_index 2 missing. Position-based counting ("dN" by
// the i'th data row seen) would instead call /mnt/disk3 "d2" here.
func TestDataDiskLabelForMountpoint_AgreesWithRenderAcrossARoleIndexGap(t *testing.T) {
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 3, Mountpoint: "/mnt/disk3"},
	}

	got, err := DataDiskLabelForMountpoint(disks, "/mnt/disk3")
	if err != nil {
		t.Fatalf("DataDiskLabelForMountpoint: %v", err)
	}
	if got != "d3" {
		t.Fatalf("DataDiskLabelForMountpoint(/mnt/disk3) = %q, want d3", got)
	}

	rendered, err := layoutFromStore(disks).Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered, "data "+got+" /mnt/disk3/\n") {
		t.Fatalf("Render() = %q, does not name /mnt/disk3 as %s the way DataDiskLabelForMountpoint resolved it", rendered, got)
	}
}

func TestDataDiskLabelForMountpoint_NoDiskAtMountpoint(t *testing.T) {
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
	}
	if _, err := DataDiskLabelForMountpoint(disks, "/mnt/disk9"); err == nil {
		t.Fatal("DataDiskLabelForMountpoint: got nil error, want one for a mountpoint with no data disk")
	}
}

// oldSlotDisk is the store.ArrayDisk every
// TestConfirmReplacementTargetAbsent_* test below refuses or allows a
// replace against: /mnt/disk1, WWN wwn-d1, formatted xfs with filesystem
// UUID uuid-d1 (#388).
func oldSlotDisk() store.ArrayDisk {
	return store.ArrayDisk{
		Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb",
		Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Mountpoint: "/mnt/disk1",
	}
}

// TestConfirmReplacementTargetAbsent_SameSerialSameFilesystemStillRefuses
// is the data-loss case #388's relaxation must never open: the slot's own
// disk is still attached, unchanged, at the exact device the caller wants
// to format — matching serial and matching filesystem UUID both. This
// must refuse exactly as it always has, whether or not that device is
// also the caller's own replacement target: a disk that has not actually
// failed goes through the upgrade flow, never replace (#289).
func TestConfirmReplacementTargetAbsent_SameSerialSameFilesystemStillRefuses(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: "uuid-d1"}}
	err := ConfirmReplacementTargetAbsent("/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb")
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the disk's filesystem is unchanged, it has not failed", err)
	}
}

// TestConfirmReplacementTargetAbsent_SameSerialUnknownFilesystemStillRefuses
// is the other data-loss case: the present disk's filesystem UUID could
// not be read at all (empty) — disk.FSUUIDMismatch never reports a
// mismatch for an unknown value on either side, so this must refuse too,
// never silently treat "unreadable" as "confirmed different".
func TestConfirmReplacementTargetAbsent_SameSerialUnknownFilesystemStillRefuses(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	err := ConfirmReplacementTargetAbsent("/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb")
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — an unreadable filesystem UUID must never be read as a confirmed mismatch", err)
	}
}

// TestConfirmReplacementTargetAbsent_WrongFilesystemAllowsTheExactTarget
// is #388's own fix: a disk matched by identity but positively read to
// carry a different filesystem than the slot's own recorded one is not
// old's own disk continuing to serve — it is the "blank disk carrying the
// original disk's serial" scenario replace exists to recover from. The
// exception applies only to targetDevice, the disk the caller is actually
// about to format.
func TestConfirmReplacementTargetAbsent_WrongFilesystemAllowsTheExactTarget(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: "uuid-blank"}}
	if err := ConfirmReplacementTargetAbsent("/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb"); err != nil {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want nil — the present disk's filesystem was positively read and differs from the slot's own", err)
	}
}

// TestConfirmReplacementTargetAbsent_WrongFilesystemOnADifferentDiskStillRefuses
// guards the exception's own scope: a second, untouched disk elsewhere in
// the inventory still carrying old's identity — never the caller's own
// replacement target — must still refuse unconditionally, mismatched
// filesystem or not, since its presence means old's own data is not
// confirmed gone. Formatting some other device while this one still
// exists would abandon it, not replace it.
func TestConfirmReplacementTargetAbsent_WrongFilesystemOnADifferentDiskStillRefuses(t *testing.T) {
	listed := []disk.Disk{
		{Device: "/dev/sdx", WWN: "wwn-d1", FSUUID: "uuid-blank"},
		{Device: "/dev/sdz", WWN: "wwn-new"},
	}
	err := ConfirmReplacementTargetAbsent("/mnt/disk1", oldSlotDisk(), listed, "/dev/sdz")
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the identity-matching disk is not the caller's own replacement target", err)
	}
}
