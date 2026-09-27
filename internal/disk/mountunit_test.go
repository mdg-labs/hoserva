package disk

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUnitFileName(t *testing.T) {
	cases := map[string]string{
		"/mnt/disk1":   "mnt-disk1.mount",
		"/mnt/parity1": "mnt-parity1.mount",
		"/mnt/cache":   "mnt-cache.mount",
	}
	for where, want := range cases {
		if got := UnitFileName(where); got != want {
			t.Fatalf("UnitFileName(%s): got %q, want %q", where, got, want)
		}
	}
}

func TestMountUnit_Render(t *testing.T) {
	u := MountUnit{
		Where:       "/mnt/disk1",
		UUID:        "1234-5678",
		Filesystem:  XFS,
		Description: "Hoserva data disk 1",
	}
	got := u.Render()

	for _, want := range []string{
		"Description=Hoserva data disk 1",
		"ConditionPathExists=!" + StorageStoppedFlagPath,
		"What=/dev/disk/by-uuid/1234-5678",
		"Where=/mnt/disk1",
		"Type=xfs",
		"Options=defaults,nofail",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

// TestMountUnit_Render_ConditionSkipsAnExternalStartWhileArrayStopped is
// #387's own regression (L3 nightly run 36258823325): nfs-utils' own
// systemd integration derives a RequiresMountsFor= directly on
// nfs-server.service for every NFS-exported path, entirely outside any
// unit Hoserva writes, so `systemctl start nfs-kernel-server` remounted a
// physical data disk during `array stop` even after hoserva-storage
// .target's own Wants= on it was removed. ConditionPathExists= is what
// closes that: unlike a Requires=/Wants= edge on this unit, it is
// evaluated only when this unit's own start job actually runs, so an
// external RequiresMountsFor= reaching this unit finds nothing to
// mount — a skip, not a failure — while StorageStoppedFlagPath exists.
func TestMountUnit_Render_ConditionSkipsAnExternalStartWhileArrayStopped(t *testing.T) {
	u := MountUnit{Where: "/mnt/disk1", UUID: "1234-5678", Filesystem: XFS, Description: "Hoserva data disk 1"}
	got := u.Render()
	wantLine := "ConditionPathExists=!" + StorageStoppedFlagPath
	found := false
	for _, line := range strings.Split(got, "\n") {
		if line == wantLine {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Render() = %q, want a line exactly %q", got, wantLine)
	}
}

// x-systemd.device-timeout= is documented to apply only to an /etc/fstab
// entry and to be silently ignored in a native unit's own Options= — this
// asserts Render never emits it, so a regression can't quietly bring back
// an option that does nothing here.
func TestMountUnit_Render_NeverEmitsTheInertFstabOnlyTimeoutOption(t *testing.T) {
	u := MountUnit{Where: "/mnt/disk1", UUID: "1234-5678", Filesystem: XFS, Description: "Hoserva data disk 1"}
	if got := u.Render(); strings.Contains(got, "device-timeout") {
		t.Fatalf("Render() = %q, must not contain x-systemd.device-timeout (ignored outside /etc/fstab)", got)
	}
}

func TestMountPlan_AssignsStandardMountpoints(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data: []AssignedDisk{
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sdc", Filesystem: EXT4},
		},
		Cache: &AssignedDisk{Device: "/dev/sdd", Filesystem: XFS},
	}
	uuids := map[string]string{
		"/dev/sda": "uuid-parity1",
		"/dev/sdb": "uuid-disk1",
		"/dev/sdc": "uuid-disk2",
		"/dev/sdd": "uuid-cache",
	}

	units, err := MountPlan(plan, uuids)
	if err != nil {
		t.Fatalf("MountPlan: %v", err)
	}

	want := map[string]struct {
		uuid string
		fs   FilesystemType
	}{
		"/mnt/disk1":   {"uuid-disk1", XFS},
		"/mnt/disk2":   {"uuid-disk2", EXT4},
		"/mnt/parity1": {"uuid-parity1", XFS},
		"/mnt/cache":   {"uuid-cache", XFS},
	}
	if len(units) != len(want) {
		t.Fatalf("MountPlan: got %d units, want %d: %+v", len(units), len(want), units)
	}
	for _, u := range units {
		w, ok := want[u.Where]
		if !ok {
			t.Fatalf("MountPlan: unexpected mountpoint %s", u.Where)
		}
		if u.UUID != w.uuid || u.Filesystem != w.fs {
			t.Fatalf("MountPlan[%s]: got (%s, %s), want (%s, %s)", u.Where, u.UUID, u.Filesystem, w.uuid, w.fs)
		}
	}
}

func TestMountPlan_ErrorsOnMissingUUID(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	if _, err := MountPlan(plan, map[string]string{"/dev/sda": "uuid-parity1"}); err == nil {
		t.Fatal("MountPlan: got nil error for a disk missing its UUID")
	}
}

func TestFilesystemUUID(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte("1234-5678\n"), nil)

	got, err := FilesystemUUID(context.Background(), r, "/dev/sdb")
	if err != nil {
		t.Fatalf("FilesystemUUID: %v", err)
	}
	if got != "1234-5678" {
		t.Fatalf("FilesystemUUID: got %q, want %q", got, "1234-5678")
	}
}

func TestFilesystemUUID_EmptyOutputIsAnError(t *testing.T) {
	r := NewFakeRunner()
	r.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sdb"}, []byte(""), nil)

	if _, err := FilesystemUUID(context.Background(), r, "/dev/sdb"); err == nil {
		t.Fatal("FilesystemUUID: got nil error for empty blkid output")
	}
}

func TestFilesystemUUID_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FilesystemUUID(ctx, NewFakeRunner(), "/dev/sdb"); !errors.Is(err, context.Canceled) {
		t.Fatalf("FilesystemUUID: got %v, want context.Canceled", err)
	}
}
