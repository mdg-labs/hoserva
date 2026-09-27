package job

import (
	"context"
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
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", nil)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the disk's filesystem is unchanged, it has not failed", err)
	}
}

// TestConfirmReplacementTargetAbsent_SameSerialUnknownFilesystemNoProberStillRefuses
// is the data-loss case #398's own probe exists to resolve, with no
// prober configured at all (an older build, or a replace path this test
// exercises without one): disk.FSUUIDMismatch never reports a mismatch
// for an unknown value on either side, and probe == nil never allows the
// exception — this must refuse exactly as it did before #398, never
// silently treat "unreadable" as either "confirmed different" or
// "confirmed blank".
func TestConfirmReplacementTargetAbsent_SameSerialUnknownFilesystemNoProberStillRefuses(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", nil)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — an unreadable filesystem UUID with no prober must never be read as confirmed blank", err)
	}
}

// TestConfirmReplacementTargetAbsent_ProbeErrorStillRefuses is #398's own
// data-loss test: the target device carries old's own identity, its own
// filesystem UUID was not positively read, but the probe itself errors
// (an ambiguous low-level result, or any other probe failure) — this
// must refuse exactly like every other inconclusive outcome, never treat
// a probe failure as "safe to assume blank".
func TestConfirmReplacementTargetAbsent_ProbeErrorStillRefuses(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	probe := disk.NewFakeBlankProber()
	probe.ScriptError("/dev/sdb", errors.New("blkid: ambiguous"))
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", probe)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — a probe error is never a positive 'blank' result", err)
	}
}

// TestConfirmReplacementTargetAbsent_ProbeFoundFilesystemStillRefuses
// covers the probe positively finding a filesystem (or a partition
// table) despite the udev-cached FSUUID reading empty — the probe's own
// "found something" answer, never treated as blank.
func TestConfirmReplacementTargetAbsent_ProbeFoundFilesystemStillRefuses(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	probe := disk.NewFakeBlankProber()
	probe.ScriptFound("/dev/sdb")
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", probe)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the probe found a signature, this disk is not blank", err)
	}
}

// TestConfirmReplacementTargetAbsent_ProbedBlankAllowsTheExactTarget is
// #398's own fix: the target device carries old's own identity, its own
// filesystem UUID was not positively read, old's own slot has a recorded
// filesystem UUID (it was actually formatted before), and the probe
// positively confirms no signature at all — the literal #388 scenario, a
// same-serial disk that is genuinely blank.
func TestConfirmReplacementTargetAbsent_ProbedBlankAllowsTheExactTarget(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank("/dev/sdb")
	if err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", probe); err != nil {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want nil — the probe positively confirmed no signature at all", err)
	}
	probed := probe.Probed()
	if len(probed) != 1 || probed[0] != "/dev/sdb" {
		t.Fatalf("probe.Probed() = %v, want exactly one call against /dev/sdb — the one device this replace names, never any other", probed)
	}
}

// TestConfirmReplacementTargetAbsent_ProbedBlankWithNoRecordedFSUUIDStillRefuses
// is #398's own narrower guard: old's own slot was never actually
// formatted (no recorded FSUUID at all — a slot mid-setup, not a failed
// disk), so a blank present-by-identity disk there is not the #388/#398
// scenario at all; the probe must never even run.
func TestConfirmReplacementTargetAbsent_ProbedBlankWithNoRecordedFSUUIDStillRefuses(t *testing.T) {
	old := oldSlotDisk()
	old.FSUUID = ""
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: ""}}
	probe := disk.NewFakeBlankProber()
	probe.ScriptBlank("/dev/sdb")
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", old, listed, "/dev/sdb", probe)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the slot was never formatted, there is nothing to positively confirm blank against", err)
	}
	if probed := probe.Probed(); len(probed) != 0 {
		t.Fatalf("probe.Probed() = %v, want no calls — a slot with no recorded FSUUID never needs the probe", probed)
	}
}

// TestConfirmReplacementTargetAbsent_WrongFilesystemAllowsTheExactTarget
// is #388's own fix: a disk matched by identity but positively read to
// carry a different filesystem than the slot's own recorded one is not
// old's own disk continuing to serve — it is a replacement disk that was
// already formatted with something else. The exception applies only to
// targetDevice, the disk the caller is actually about to format, and
// never even calls the probe: the filesystem was already positively
// read, nothing is unknown to probe for.
func TestConfirmReplacementTargetAbsent_WrongFilesystemAllowsTheExactTarget(t *testing.T) {
	listed := []disk.Disk{{Device: "/dev/sdb", WWN: "wwn-d1", FSUUID: "uuid-different"}}
	probe := disk.NewFakeBlankProber()
	if err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdb", probe); err != nil {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want nil — the present disk's filesystem was positively read and differs from the slot's own", err)
	}
	if probed := probe.Probed(); len(probed) != 0 {
		t.Fatalf("probe.Probed() = %v, want no calls — the filesystem was already positively read, the probe is never needed", probed)
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
	err := ConfirmReplacementTargetAbsent(context.Background(), "/mnt/disk1", oldSlotDisk(), listed, "/dev/sdz", nil)
	if !errors.Is(err, ErrReplacementSlotDiskPresent) {
		t.Fatalf("ConfirmReplacementTargetAbsent = %v, want ErrReplacementSlotDiskPresent — the identity-matching disk is not the caller's own replacement target", err)
	}
}
