package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestWireBackup_ExternalDiskFlagReachesTheNextBackup goes through the
// handler wireBackup builds and the service newBackupService builds — what
// main.go runs — from PATCH /disks/external/{label} to the archive on the
// disk's mount. The mount table is a fake and the disk's mount root a
// directory this test owns: a unit test never writes under /mnt.
func TestWireBackup_ExternalDiskFlagReachesTheNextBackup(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "")
	extRoot := filepath.Join(t.TempDir(), "disks")
	mounted := map[string]bool{}
	rig.svc.ExternalRoot = extRoot
	rig.svc.ExternalMounted = func(_ context.Context, path string) (bool, error) { return mounted[path], nil }
	rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
	rig.svc.Log = func(string, ...any) {}

	provider := disk.NewFakeProvider()
	provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	provider.AddDisk("/dev/sde", disk.Disk{Size: 4 * disk.TB, Filesystem: "xfs", Label: "backup", FSUUID: "uuid-ext", Serial: "EXT1"})
	h := &api.Handler{ArrayStore: store.NewArrayStore(rig.db), Disks: provider, DiskRunner: disk.NewFakeRunner()}
	wireBackup(h, rig.svc)

	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(true))
	if _, err := h.UpdateExternalDisk(ctx, req, apiv1.UpdateExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("UpdateExternalDisk: %v", err)
	}
	list, err := h.ListBackupDestinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range list.Destinations {
		if d.ID == "external:backup" {
			found = d.Path == "/mnt/disks/backup"
		}
	}
	if !found {
		t.Fatalf("destinations = %+v, want external:backup at /mnt/disks/backup", list.Destinations)
	}

	// The destination's path is the daemon's /mnt/disks/backup; the test
	// re-points the stored row at the mount root it controls.
	diskDir := filepath.Join(extRoot, "backup")
	if _, err := rig.db.Exec(`UPDATE backup_destinations SET path = ? WHERE id = 'external:backup'`, diskDir); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run with the disk ejected: %v", err)
	}
	if _, err := os.Stat(extRoot); !os.IsNotExist(err) {
		t.Fatalf("a backup with the disk ejected created %q: %v", extRoot, err)
	}

	mounted[diskDir] = true
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run with the disk mounted: %v", err)
	}
	if entries, err := os.ReadDir(diskDir); err != nil || len(entries) != 1 {
		t.Fatalf("the mounted disk holds %v (%v), want the one archive", entries, err)
	}
}

// TestNewBackupService_CreatesTheDestinationOfAnAlreadyFlaggedDisk covers a
// disk flagged before the flag created its destination: the next start
// gives it the destination.
func TestNewBackupService_CreatesTheDestinationOfAnAlreadyFlaggedDisk(t *testing.T) {
	ctx := context.Background()
	rig := newBackupRig(t, "")
	err := store.NewExternalStore(rig.db).PutExternalDisk(ctx, store.ExternalDisk{
		Label: "usb", Device: "/dev/sde", Filesystem: "xfs", FSUUID: "uuid-usb",
		Mountpoint: "/mnt/disks/usb", BackupDestination: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc, err := rig.rebuild()
	if err != nil {
		t.Fatalf("newBackupService: %v", err)
	}
	if _, err := api.NewBackupDestinationStore(rig.db).GetDestination(ctx, "external:usb"); err != nil {
		t.Fatalf("the flagged disk has no destination after a restart: %v", err)
	}
	if dests, err := svc.ListDestinations(ctx); err != nil || len(dests) != 3 {
		t.Fatalf("destinations = %+v, %v; want the two defaults and external:usb", dests, err)
	}
}
