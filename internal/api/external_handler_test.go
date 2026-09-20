package api_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func newExternalHandler(t *testing.T) (*api.Handler, *disk.FakeProvider, *disk.FakeMounter, *disk.FakeRunner) {
	t.Helper()
	h, _, _ := newTestHandler(t)

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "external.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening array test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	arrays := store.NewArrayStore(db)

	p := disk.NewFakeProvider()
	p.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	p.AddDisk("/dev/sdb", disk.Disk{Size: 4 * disk.TB})
	p.AddDisk("/dev/sde", disk.Disk{Size: 4 * disk.TB, Filesystem: "xfs", Label: "backup", FSUUID: "uuid-ext", Serial: "EXT1"})
	mounter := disk.NewFakeMounter()
	fakeRun := disk.NewFakeRunner()
	fakeRun.Script("blkid", []string{"-s", "UUID", "-o", "value", "/dev/sde"}, []byte("uuid-ext\n"), nil)

	h.Disks = p
	h.ArrayStore = arrays
	h.DiskMounter = mounter
	h.DiskRunner = fakeRun
	return h, p, mounter, fakeRun
}

func TestListExternalDisks_SkipsBootAndShowsLabelledInventory(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	got, err := h.ListExternalDisks(context.Background())
	if err != nil {
		t.Fatalf("ListExternalDisks: %v", err)
	}
	if len(got.Disks) != 1 || string(got.Disks[0].Label) != "backup" || got.Disks[0].Device != "/dev/sde" {
		t.Fatalf("disks = %+v, want the labelled USB disk only", got.Disks)
	}
	if got.Disks[0].Boot {
		t.Fatal("external list included a boot disk")
	}
	if got.Disks[0].ContainerPath != "/mnt/disks/backup" {
		t.Fatalf("containerPath = %q, want /mnt/disks/backup", got.Disks[0].ContainerPath)
	}
	if v, ok := got.Disks[0].FsUuid.Get(); !ok || v != "uuid-ext" {
		t.Fatalf("fsUuid = %v, want uuid-ext from inventory udev UUID", got.Disks[0].FsUuid)
	}
}

func TestRegisterExternalDisk_RefusesBootDevice(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	_, err := h.RegisterExternalDisk(context.Background(), &apiv1.RegisterExternalDiskRequest{
		Device: "/dev/sda",
		Label:  "boot",
	})
	if err == nil {
		t.Fatal("RegisterExternalDisk(boot): expected an error")
	}
	if ae := apiError(t, h, err); ae.Response.Code != "invalid_plan" {
		t.Fatalf("code = %q, want invalid_plan", ae.Response.Code)
	}
}

func TestRegisterMountEjectExternalDisk(t *testing.T) {
	h, p, mounter, _ := newExternalHandler(t)
	ctx := context.Background()
	got, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
		Device: "/dev/sde",
		Label:  "backup",
	})
	if err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	if got.MountPoint != "/mnt/disks/backup" || got.ContainerPath != "/mnt/disks/backup" {
		t.Fatalf("paths = %+v", got)
	}
	if v, ok := got.FsUuid.Get(); !ok || v != "uuid-ext" {
		t.Fatalf("fsUuid = %v, want uuid-ext", got.FsUuid)
	}

	mounted, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup"})
	if err != nil {
		t.Fatalf("MountExternalDisk: %v", err)
	}
	if len(mounter.Mounts) != 1 || mounter.Mounts[0].UUID != "uuid-ext" || mounter.Mounts[0].Where != "/mnt/disks/backup" {
		t.Fatalf("Mounts = %+v", mounter.Mounts)
	}
	_ = mounted

	if _, err := h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("EjectExternalDisk: %v", err)
	}
	if len(mounter.Unmounts) != 1 {
		t.Fatalf("Unmounts = %+v", mounter.Unmounts)
	}
	state, err := p.SpinState("/dev/sde")
	if err != nil || state != disk.Standby {
		t.Fatalf("SpinState = (%v, %v), want Standby", state, err)
	}
}

func TestMountExternalDisk_NothingAutoMountsOnList(t *testing.T) {
	h, _, mounter, _ := newExternalHandler(t)
	if _, err := h.ListExternalDisks(context.Background()); err != nil {
		t.Fatalf("ListExternalDisks: %v", err)
	}
	if len(mounter.Mounts) != 0 {
		t.Fatalf("ListExternalDisks mounted %+v", mounter.Mounts)
	}
}

func TestFormatExternalDisk_WrongConfirmationFormatsNothing(t *testing.T) {
	h, p, _, _ := newExternalHandler(t)
	ctx := context.Background()
	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	_, err := h.FormatExternalDisk(ctx, &apiv1.FormatExternalDiskRequest{Confirmation: "yes"}, apiv1.FormatExternalDiskParams{Label: "backup"})
	if err == nil {
		t.Fatal("FormatExternalDisk(wrong confirm): expected an error")
	}
	if ae := apiError(t, h, err); ae.Response.Code != "confirmation_required" {
		t.Fatalf("code = %q, want confirmation_required", ae.Response.Code)
	}
	if _, ok := p.FormattedAs("/dev/sde"); ok {
		t.Fatal("wrong confirmation formatted the disk")
	}
}

func TestFormatExternalDisk_MatchingConfirmation(t *testing.T) {
	h, p, _, runner := newExternalHandler(t)
	ctx := context.Background()
	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	plan := disk.ExternalFormatPlan(disk.AssignedDisk{Device: "/dev/sde", Filesystem: disk.XFS})
	got, err := h.FormatExternalDisk(ctx, &apiv1.FormatExternalDiskRequest{Confirmation: plan.Confirmation()}, apiv1.FormatExternalDiskParams{Label: "backup"})
	if err != nil {
		t.Fatalf("FormatExternalDisk: %v", err)
	}
	if fs, ok := p.FormattedAs("/dev/sde"); !ok || fs != disk.XFS {
		t.Fatalf("FormattedAs: got (%v, %v)", fs, ok)
	}
	if v, ok := got.FsUuid.Get(); !ok || v != "uuid-ext" {
		t.Fatalf("fsUuid after format = %v", got.FsUuid)
	}
	_ = runner
}

func TestUpdateExternalDisk_BackupDestination(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	ctx := context.Background()
	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(true))
	got, err := h.UpdateExternalDisk(ctx, req, apiv1.UpdateExternalDiskParams{Label: "backup"})
	if err != nil {
		t.Fatalf("UpdateExternalDisk: %v", err)
	}
	if !got.BackupDestination {
		t.Fatal("backupDestination not set")
	}
}

func TestUpdateExternalDisk_AutoRegistersFromFilesystemLabel(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(true))
	got, err := h.UpdateExternalDisk(context.Background(), req, apiv1.UpdateExternalDiskParams{Label: "backup"})
	if err != nil {
		t.Fatalf("UpdateExternalDisk: %v", err)
	}
	if !got.BackupDestination {
		t.Fatal("backupDestination not set on auto-registered disk")
	}
	if got.Device != "/dev/sde" {
		t.Fatalf("device = %q, want /dev/sde", got.Device)
	}
}

func TestRegisterExternalDisk_RefusesArrayDisk(t *testing.T) {
	h, _, _, _ := newExternalHandler(t)
	ctx := context.Background()
	if err := h.ArrayStore.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G"}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sde", Filesystem: "xfs", FSUUID: "uuid-d", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	_, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{Device: "/dev/sde", Label: "backup"})
	if err == nil {
		t.Fatal("RegisterExternalDisk(array disk): expected an error")
	}
	if !strings.Contains(err.Error(), "array") && apiError(t, h, err).Response.Code != "invalid_plan" {
		t.Fatalf("error = %v", err)
	}
}

func TestMountExternalDisk_AutoRegistersFromFilesystemLabel(t *testing.T) {
	h, _, mounter, _ := newExternalHandler(t)
	if _, err := h.MountExternalDisk(context.Background(), apiv1.MountExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("MountExternalDisk: %v", err)
	}
	if len(mounter.Mounts) != 1 || mounter.Mounts[0].Where != "/mnt/disks/backup" {
		t.Fatalf("Mounts = %+v", mounter.Mounts)
	}
}

func TestFormatExternalDisk_RefusesBootEvenIfRegisteredSomehow(t *testing.T) {
	h, p, _, runner := newExternalHandler(t)
	ctx := context.Background()
	ext := h.ArrayStore.External()
	if err := ext.PutExternalDisk(ctx, store.ExternalDisk{
		Label:      "bootdisk",
		Device:     "/dev/sda",
		Filesystem: "xfs",
		FSUUID:     "uuid-boot",
		Mountpoint: "/mnt/disks/bootdisk",
	}); err != nil {
		t.Fatalf("PutExternalDisk: %v", err)
	}
	plan := disk.ExternalFormatPlan(disk.AssignedDisk{Device: "/dev/sda", Filesystem: disk.XFS})
	_, err := h.FormatExternalDisk(ctx, &apiv1.FormatExternalDiskRequest{Confirmation: plan.Confirmation()}, apiv1.FormatExternalDiskParams{Label: "bootdisk"})
	if err == nil {
		t.Fatal("FormatExternalDisk(boot): expected an error")
	}
	if _, ok := p.FormattedAs("/dev/sda"); ok {
		t.Fatal("formatted the boot device")
	}
	_ = runner
}
