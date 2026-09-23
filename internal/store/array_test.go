package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func migratedArrayDB(t *testing.T) *ArrayStore {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("loading migrations: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "array-test.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	return NewArrayStore(db)
}

func TestArrayStore_PutGetAndRefuseOverwrite(t *testing.T) {
	ctx := context.Background()
	st := migratedArrayDB(t)

	exists, err := st.Exists(ctx)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists {
		t.Fatal("fresh database already has array topology")
	}
	if _, _, err := st.GetArray(ctx); !errors.Is(err, ErrNoArray) {
		t.Fatalf("GetArray = %v, want ErrNoArray", err)
	}

	settings := ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}
	disks := []ArrayDisk{
		{Role: ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Mountpoint: "/mnt/parity1", Size: 8 << 40, SizeSet: true},
		{Role: ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", Serial: "DATA1", Mountpoint: "/mnt/disk1", Size: 4 << 40, SizeSet: true},
		{Role: ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "uuid-c", Mountpoint: "/mnt/cache", Size: 1 << 40, SizeSet: true},
	}
	if err := st.PutArray(ctx, settings, disks); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	gotSettings, gotDisks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	if gotSettings.CreatePolicy != "mfs" || gotSettings.MinFreeSpace != "20G" {
		t.Fatalf("settings = %+v", gotSettings)
	}
	if len(gotDisks) != 3 {
		t.Fatalf("len(disks) = %d, want 3", len(gotDisks))
	}
	if gotDisks[0].Role != ArrayRoleParity || gotDisks[0].WWN != "wwn-p" || gotDisks[0].FSUUID != "uuid-p" {
		t.Fatalf("parity = %+v", gotDisks[0])
	}
	if !gotDisks[0].SizeSet || gotDisks[0].Size != 8<<40 {
		t.Fatalf("parity size = %d set=%v, want 8TiB set", gotDisks[0].Size, gotDisks[0].SizeSet)
	}
	if gotDisks[1].Role != ArrayRoleData || gotDisks[1].Serial != "DATA1" {
		t.Fatalf("data = %+v", gotDisks[1])
	}
	if !gotDisks[1].SizeSet || gotDisks[1].Size != 4<<40 {
		t.Fatalf("data size = %d set=%v, want 4TiB set", gotDisks[1].Size, gotDisks[1].SizeSet)
	}
	if gotDisks[2].Role != ArrayRoleCache || gotDisks[2].Filesystem != "ext4" {
		t.Fatalf("cache = %+v", gotDisks[2])
	}

	if err := st.PutArray(ctx, settings, disks); !errors.Is(err, ErrArrayExists) {
		t.Fatalf("second PutArray = %v, want ErrArrayExists", err)
	}
}
