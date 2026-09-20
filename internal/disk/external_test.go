package disk

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateExternalLabel(t *testing.T) {
	if err := ValidateExternalLabel("backup"); err != nil {
		t.Fatalf("ValidateExternalLabel(backup): %v", err)
	}
	for _, label := range []string{"", "../etc", "a/b", " disks", ".", "..", "mnt/disks/x"} {
		if err := ValidateExternalLabel(label); !errors.Is(err, ErrInvalidExternalLabel) {
			t.Fatalf("ValidateExternalLabel(%q): got %v, want ErrInvalidExternalLabel", label, err)
		}
	}
}

func TestExternalMountPoint(t *testing.T) {
	got, err := ExternalMountPoint("backup")
	if err != nil {
		t.Fatalf("ExternalMountPoint: %v", err)
	}
	if got != "/mnt/disks/backup" {
		t.Fatalf("ExternalMountPoint = %q, want /mnt/disks/backup", got)
	}
}

func TestExternalFormatPlan_UsesArrayConfirmationShape(t *testing.T) {
	plan := ExternalFormatPlan(AssignedDisk{Device: "/dev/sde", Filesystem: XFS})
	if got := plan.Confirmation(); got != "ERASE /dev/sde" {
		t.Fatalf("Confirmation = %q, want ERASE /dev/sde", got)
	}
	if err := plan.CheckConfirmation("ERASE /dev/sde"); err != nil {
		t.Fatalf("CheckConfirmation: %v", err)
	}
	if err := plan.CheckConfirmation("yes"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("CheckConfirmation(yes): got %v, want ErrConfirmationMismatch", err)
	}
}

func TestFormatExternal_WrongConfirmationFormatsNothing(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sde", Disk{Size: 4 * TB})
	d := AssignedDisk{Device: "/dev/sde", Filesystem: XFS}
	if err := FormatExternal(context.Background(), p, NewFakeRunner(), d, "yes"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("FormatExternal: got %v, want ErrConfirmationMismatch", err)
	}
	if _, ok := p.FormattedAs("/dev/sde"); ok {
		t.Fatal("FormatExternal formatted the disk after a confirmation mismatch")
	}
}

func TestFormatExternal_FormatsOnMatchingConfirmation(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sde", Disk{Size: 4 * TB})
	d := AssignedDisk{Device: "/dev/sde", Filesystem: XFS}
	if err := FormatExternal(context.Background(), p, NewFakeRunner(), d, ExternalFormatPlan(d).Confirmation()); err != nil {
		t.Fatalf("FormatExternal: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sde"); !ok || fs != XFS {
		t.Fatalf("FormattedAs: got (%v, %v), want (xfs, true)", fs, ok)
	}
}

func TestFormatExternal_RefusesBootDevice(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: 4 * TB, Boot: true})
	d := AssignedDisk{Device: "/dev/sda", Filesystem: XFS}
	if err := FormatExternal(context.Background(), p, NewFakeRunner(), d, ExternalFormatPlan(d).Confirmation()); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("FormatExternal(boot): got %v, want ErrBootDevice", err)
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("FormatExternal formatted the boot device")
	}
}

func TestRefuseBootDevice(t *testing.T) {
	p := NewFakeProvider()
	p.AddDisk("/dev/sda", Disk{Size: TB, Boot: true})
	p.AddDisk("/dev/sdb", Disk{Size: TB})
	if err := RefuseBootDevice(context.Background(), p, "/dev/sda"); !errors.Is(err, ErrBootDevice) {
		t.Fatalf("RefuseBootDevice(boot): got %v, want ErrBootDevice", err)
	}
	if err := RefuseBootDevice(context.Background(), p, "/dev/sdb"); err != nil {
		t.Fatalf("RefuseBootDevice(data): %v", err)
	}
}

func TestMountExternal_ByUUID(t *testing.T) {
	m := NewFakeMounter()
	unit, err := ExternalMountUnit("backup", "uuid-ext", XFS)
	if err != nil {
		t.Fatalf("ExternalMountUnit: %v", err)
	}
	if err := MountExternal(context.Background(), m, unit); err != nil {
		t.Fatalf("MountExternal: %v", err)
	}
	if len(m.Mounts) != 1 || m.Mounts[0].UUID != "uuid-ext" || m.Mounts[0].Where != "/mnt/disks/backup" {
		t.Fatalf("Mounts = %+v", m.Mounts)
	}
}

func TestEjectExternal_UnmountsThenSpinsDown(t *testing.T) {
	m := NewFakeMounter()
	p := NewFakeProvider()
	p.AddDisk("/dev/sde", Disk{Size: TB})
	unit, err := ExternalMountUnit("backup", "uuid-ext", XFS)
	if err != nil {
		t.Fatalf("ExternalMountUnit: %v", err)
	}
	if err := EjectExternal(context.Background(), m, p, unit, "/dev/sde"); err != nil {
		t.Fatalf("EjectExternal: %v", err)
	}
	if len(m.Unmounts) != 1 || m.Unmounts[0].Where != "/mnt/disks/backup" {
		t.Fatalf("Unmounts = %+v", m.Unmounts)
	}
	state, err := p.SpinState("/dev/sde")
	if err != nil || state != Standby {
		t.Fatalf("SpinState = (%v, %v), want Standby", state, err)
	}
}

func TestEjectExternal_DoesNotSpinDownIfUnmountFails(t *testing.T) {
	m := NewFakeMounter()
	m.UnmountErr = errors.New("busy")
	p := NewFakeProvider()
	p.AddDisk("/dev/sde", Disk{Size: TB})
	unit, err := ExternalMountUnit("backup", "uuid-ext", XFS)
	if err != nil {
		t.Fatalf("ExternalMountUnit: %v", err)
	}
	if err := EjectExternal(context.Background(), m, p, unit, "/dev/sde"); err == nil {
		t.Fatal("EjectExternal: expected unmount error")
	}
	state, err := p.SpinState("/dev/sde")
	if err != nil || state != Active {
		t.Fatalf("SpinState after failed unmount = (%v, %v), want Active", state, err)
	}
}

func TestIsExternalMountpoint(t *testing.T) {
	if !IsExternalMountpoint("/mnt/disks/backup") {
		t.Fatal("IsExternalMountpoint(/mnt/disks/backup): want true")
	}
	if IsExternalMountpoint("/mnt/disk1") || IsExternalMountpoint("/mnt/user") || IsExternalMountpoint("/mnt/disks-other") {
		t.Fatal("IsExternalMountpoint accepted a pool or parity path")
	}
}

func TestDirectMounter_UnmountsByArgv(t *testing.T) {
	r := NewFakeRunner()
	m := DirectMounter{Runner: r}
	where := t.TempDir()
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Unmount(context.Background(), u); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "umount" {
		t.Fatalf("calls = %+v, want umount", calls)
	}
	if strings.Join(calls[0].Args, " ") != where {
		t.Fatalf("umount argv = %v, want the mountpoint only", calls[0].Args)
	}
}

func TestSystemdMounter_StopsUnitByFilename(t *testing.T) {
	r := NewFakeRunner()
	m := SystemdMounter{Runner: r}
	u := MountUnit{Where: "/mnt/disks/backup", UUID: "uuid-ext", Filesystem: XFS}
	if err := m.Unmount(context.Background(), u); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "systemctl" || strings.Join(calls[0].Args, " ") != "stop mnt-disks-backup.mount" {
		t.Fatalf("call = %+v, want systemctl stop mnt-disks-backup.mount", calls)
	}
}
