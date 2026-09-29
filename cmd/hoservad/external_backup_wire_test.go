package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
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

// ejectRecorder is the disk mounter of the handler under test: it notes when
// the daemon unmounts an external disk, and how many archives the disk held
// at that moment.
type ejectRecorder struct {
	*disk.FakeMounter
	dir       string
	unmounted chan int
}

func (m *ejectRecorder) Unmount(ctx context.Context, unit disk.MountUnit) error {
	if err := m.FakeMounter.Unmount(ctx, unit); err != nil {
		return err
	}
	entries, _ := os.ReadDir(m.dir)
	m.unmounted <- len(entries)
	return nil
}

// ejectRig is wireBackup's handler and newBackupService's service, joined the
// way main.go joins them, with external disk "backup" flagged as a backup
// destination and mounted at a directory the test owns.
type ejectRig struct {
	h       *api.Handler
	svc     *backup.Service
	diskDir string
	mounter *ejectRecorder
	// checking is closed, and the mount check then waits for release, the
	// first time a backup confirms the disk is mounted.
	checking chan struct{}
	release  chan struct{}
}

func newEjectRig(t *testing.T) *ejectRig {
	t.Helper()
	ctx := context.Background()
	rig := newBackupRig(t, "")
	extRoot := filepath.Join(t.TempDir(), "disks")
	diskDir := filepath.Join(extRoot, "backup")
	r := &ejectRig{svc: rig.svc, diskDir: diskDir, checking: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	rig.svc.ExternalRoot = extRoot
	rig.svc.ExternalMounted = func(context.Context, string) (bool, error) {
		once.Do(func() {
			close(r.checking)
			<-r.release
		})
		return true, nil
	}
	rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
	rig.svc.Log = func(string, ...any) {}

	provider := disk.NewFakeProvider()
	provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	provider.AddDisk("/dev/sde", disk.Disk{Size: 4 * disk.TB, Filesystem: "xfs", Label: "backup", FSUUID: "uuid-ext", Serial: "EXT1"})
	r.mounter = &ejectRecorder{FakeMounter: disk.NewFakeMounter(), dir: diskDir, unmounted: make(chan int, 1)}
	r.h = &api.Handler{ArrayStore: store.NewArrayStore(rig.db), Disks: provider, DiskRunner: disk.NewFakeRunner(), DiskMounter: r.mounter}
	wireBackup(r.h, rig.svc)

	req := &apiv1.UpdateExternalDiskRequest{}
	req.SetBackupDestination(apiv1.NewOptBool(true))
	if _, err := r.h.UpdateExternalDisk(ctx, req, apiv1.UpdateExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("UpdateExternalDisk: %v", err)
	}
	if _, err := rig.db.Exec(`UPDATE backup_destinations SET path = ? WHERE id = 'external:backup'`, diskDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestWireBackup_EjectWaitsForABackupWriteToTheDisk goes from
// POST /disks/external/{label}/eject to a running config backup: the write
// has confirmed the disk is mounted and not yet written when the eject
// arrives, and the disk is unmounted only once the archive is on it. It
// fails if either the service or the handler is left without the shared gate.
func TestWireBackup_EjectWaitsForABackupWriteToTheDisk(t *testing.T) {
	r := newEjectRig(t)
	run := make(chan error, 1)
	go func() { run <- r.svc.Run(context.Background()) }()
	<-r.checking

	ejected := make(chan error, 1)
	go func() {
		_, err := r.h.EjectExternalDisk(context.Background(), apiv1.EjectExternalDiskParams{Label: "backup"})
		ejected <- err
	}()
	select {
	case <-r.mounter.unmounted:
		t.Fatal("the disk was unmounted while a backup write to it was admitted")
	case <-time.After(300 * time.Millisecond):
	}
	close(r.release)

	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := <-ejected; err != nil {
		t.Fatalf("EjectExternalDisk: %v", err)
	}
	if n := <-r.mounter.unmounted; n != 1 {
		t.Fatalf("the disk held %d archives when it was unmounted, want the one being written", n)
	}
}

func TestWireBackup_EjectThatOutwaitsTheWriteLeavesTheDiskMounted(t *testing.T) {
	r := newEjectRig(t)
	r.h.ExternalEjectWait = 50 * time.Millisecond
	run := make(chan error, 1)
	go func() { run <- r.svc.Run(context.Background()) }()
	<-r.checking

	_, err := r.h.EjectExternalDisk(context.Background(), apiv1.EjectExternalDiskParams{Label: "backup"})
	if err == nil || !strings.Contains(err.Error(), "left mounted") {
		t.Fatalf("EjectExternalDisk = %v, want an error saying the disk was left mounted", err)
	}
	if n := len(r.mounter.Unmounts); n != 0 {
		t.Fatalf("the disk was unmounted %d times although the eject gave up", n)
	}
	close(r.release)
	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestWireBackup_MountingAnEjectedDiskAdmitsBackupsAgain(t *testing.T) {
	ctx := context.Background()
	r := newEjectRig(t)
	close(r.release)
	if _, err := r.h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("EjectExternalDisk: %v", err)
	}
	<-r.mounter.unmounted

	// The fake mount table still says mounted: only the gate refuses.
	if err := r.svc.Run(ctx); err != nil {
		t.Fatalf("Run with the boot destination still writable: %v", err)
	}
	if entries, _ := os.ReadDir(r.diskDir); len(entries) != 0 {
		t.Fatalf("an ejected disk received %d entries", len(entries))
	}

	if _, err := r.h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: "backup"}); err != nil {
		t.Fatalf("MountExternalDisk: %v", err)
	}
	if err := r.svc.Run(ctx); err != nil {
		t.Fatalf("Run after the disk was mounted again: %v", err)
	}
	if entries, _ := os.ReadDir(r.diskDir); len(entries) != 1 {
		t.Fatalf("the remounted disk holds %d entries, want the one archive", len(entries))
	}
}

func TestWireBackup_SharesOneExternalGateBetweenServiceAndHandler(t *testing.T) {
	rig := newBackupRig(t, "")
	h := &api.Handler{}
	wireBackup(h, rig.svc)
	if rig.svc.ExternalGates == nil {
		t.Fatal("newBackupService left the service without an external write gate")
	}
	if h.ExternalWriteGates != rig.svc.ExternalGates {
		t.Fatal("the handler's external write gate is not the service's")
	}
}
