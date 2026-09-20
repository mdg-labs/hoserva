package store

import (
	"context"
	"errors"
	"testing"
)

func TestExternalStore_PutGetListAndBackupFlag(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t).External()

	d := ExternalDisk{
		Label:             "backup",
		Device:            "/dev/sde",
		Filesystem:        "xfs",
		FSUUID:            "uuid-ext",
		Serial:            "EXT1",
		Mountpoint:        "/mnt/disks/backup",
		BackupDestination: true,
	}
	if err := st.PutExternalDisk(ctx, d); err != nil {
		t.Fatalf("PutExternalDisk: %v", err)
	}

	got, err := st.GetExternalDisk(ctx, "backup")
	if err != nil {
		t.Fatalf("GetExternalDisk: %v", err)
	}
	if got.Device != "/dev/sde" || got.FSUUID != "uuid-ext" || !got.BackupDestination || got.Mountpoint != "/mnt/disks/backup" {
		t.Fatalf("got %+v", got)
	}

	list, err := st.ListExternalDisks(ctx)
	if err != nil {
		t.Fatalf("ListExternalDisks: %v", err)
	}
	if len(list) != 1 || list[0].Label != "backup" {
		t.Fatalf("list = %+v", list)
	}

	if err := st.SetBackupDestination(ctx, "backup", false); err != nil {
		t.Fatalf("SetBackupDestination: %v", err)
	}
	got, err = st.GetExternalDisk(ctx, "backup")
	if err != nil {
		t.Fatalf("GetExternalDisk after flag: %v", err)
	}
	if got.BackupDestination {
		t.Fatal("backup destination flag was not cleared")
	}

	if err := st.PutExternalDisk(ctx, d); !errors.Is(err, ErrExternalExists) {
		t.Fatalf("second Put = %v, want ErrExternalExists", err)
	}
}

func TestArrayStore_DoesNotStoreExternalRole(t *testing.T) {
	ctx := context.Background()
	arrays := migratedArrayDB(t)
	disks := []ArrayDisk{
		{Role: "external", RoleIndex: 1, Device: "/dev/sde", Filesystem: "xfs", FSUUID: "uuid-ext", Mountpoint: "/mnt/disks/backup"},
	}
	err := arrays.PutArray(ctx, ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G"}, disks)
	if err == nil {
		t.Fatal("PutArray accepted role=external — array_disks CHECK must refuse it")
	}
}
