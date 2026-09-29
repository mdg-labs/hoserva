//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` and the resulting
// binary is run inside the lab container by scripts/devenv/run-lab-tests.sh.
// It proves #434 end to end against a real XFS filesystem on a loop device,
// mounted and unmounted at /mnt/disks/<label> inside the lab container —
// the service's mount check is its production default (mountinfo), never a
// fake.

package api_test

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

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

func labLoopFilesystem(ctx context.Context, t *testing.T, r disk.Runner, lab, name string) (dev, uuid string) {
	t.Helper()
	imgDir := filepath.Join(lab, "img")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", imgDir, err)
	}
	img := filepath.Join(imgDir, name+".img")
	if err := os.Remove(img); err != nil && !os.IsNotExist(err) {
		t.Fatalf("removing stale %s: %v", img, err)
	}
	if _, err := r.Run(ctx, "truncate", "-s", "320M", img); err != nil {
		t.Fatalf("truncate %s: %v", img, err)
	}
	out, err := r.Run(ctx, "losetup", "--find", "--show", img)
	if err != nil {
		t.Fatalf("losetup --find --show %s: %v", img, err)
	}
	dev = strings.TrimSpace(string(out))
	backing, err := r.Run(ctx, "losetup", "-j", img, "--output", "NAME", "--noheadings")
	if err != nil || strings.TrimSpace(string(backing)) != dev || !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("losetup -j %s: got %q (err %v), want %q — refusing to trust a device this call did not just attach", img, backing, err, dev)
	}
	t.Cleanup(func() { _, _ = r.Run(context.Background(), "losetup", "-d", dev) })
	if _, err := r.Run(ctx, "mkfs.xfs", "-q", "-L", name, dev); err != nil {
		t.Fatalf("mkfs.xfs %s: %v", dev, err)
	}
	uuid, err = disk.FilesystemUUID(ctx, r, dev)
	if err != nil {
		t.Fatalf("reading the filesystem UUID of %s: %v", dev, err)
	}
	return dev, uuid
}

func labArchives(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "hoserva-config-") {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestLabExternalBackupDestination_FollowsTheDisksMount(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := disk.CommandRunner{}
	mounter := disk.DirectMounter{Runner: r}

	const label, ghost = "labbk", "labghost"
	dev, uuid := labLoopFilesystem(ctx, t, r, lab, "external-434")
	unit, err := disk.ExternalMountUnit(label, uuid, disk.XFS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", unit.Where)
		_ = os.Remove(unit.Where)
	})
	if err := disk.MountExternal(ctx, mounter, unit); err != nil {
		t.Fatalf("MountExternal: %v", err)
	}
	ghostPath, _ := disk.ExternalMountPoint(ghost)
	if _, err := os.Stat(ghostPath); !os.IsNotExist(err) {
		t.Fatalf("%s exists before the test: %v", ghostPath, err)
	}

	db := openTestDB(t)
	stateDir := t.TempDir()
	svc := &backup.Service{
		DB:       db,
		Paths:    backup.DefaultPaths(stateDir, filepath.Join(t.TempDir(), "hoserva")),
		Store:    api.NewBackupDestinationStore(db),
		Hostname: "lab-host",
		Version:  "0.0.0-lab",
	}
	provider := disk.NewFakeProvider()
	provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	h := &api.Handler{ArrayStore: store.NewArrayStore(db), Disks: provider, DiskRunner: r, Backup: svc}

	for _, reg := range []struct{ label, device string }{{label, dev}, {ghost, "/dev/loop250"}} {
		got, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
			Device: reg.device, Label: apiv1.ExternalDiskLabel(reg.label), BackupDestination: apiv1.NewOptBool(true),
		})
		if err != nil || !got.BackupDestination {
			t.Fatalf("RegisterExternalDisk(%s) = %+v, %v", reg.label, got, err)
		}
	}
	list, err := h.ListBackupDestinations(ctx)
	if err != nil || len(list.Destinations) != 2 {
		t.Fatalf("destinations = %+v, %v; want external:%s and external:%s", list, err, label, ghost)
	}

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run with %s mounted and %s never mounted: %v", label, ghost, err)
	}
	got := labArchives(t, unit.Where)
	if len(got) != 1 {
		t.Fatalf("archives on the mounted disk = %v, want one", got)
	}
	firstArchive := got[0]
	if _, err := os.Stat(ghostPath); !os.IsNotExist(err) {
		t.Fatalf("%s was created on the root filesystem although no disk was mounted there: %v", ghostPath, err)
	}

	if err := mounter.Unmount(ctx, unit); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if err := svc.Run(ctx); err == nil {
		t.Fatal("Run reported success although every destination was ejected")
	}
	if got := labArchives(t, unit.Where); len(got) != 0 {
		t.Fatalf("archives on the root filesystem under %s while the disk was ejected: %v", unit.Where, got)
	}
	if _, err := os.Stat(ghostPath); !os.IsNotExist(err) {
		t.Fatalf("%s was created on the root filesystem while ejected: %v", ghostPath, err)
	}
	res, err := h.TestBackupDestination(ctx, apiv1.TestBackupDestinationParams{DestinationId: "external:" + label})
	if err != nil || res.Success || !strings.Contains(res.Error.Value, "not mounted") {
		t.Fatalf("connection test on the ejected disk = %+v, %v; want a not-mounted refusal", res, err)
	}

	if err := disk.MountExternal(ctx, mounter, unit); err != nil {
		t.Fatalf("MountExternal again: %v", err)
	}
	if got := labArchives(t, unit.Where); len(got) != 1 {
		t.Fatalf("archives after remounting = %v, want the first one back", got)
	}

	off := &apiv1.UpdateExternalDiskRequest{}
	off.SetBackupDestination(apiv1.NewOptBool(false))
	if _, err := h.UpdateExternalDisk(ctx, off, apiv1.UpdateExternalDiskParams{Label: label}); err != nil {
		t.Fatalf("flag off: %v", err)
	}
	if list, err = h.ListBackupDestinations(ctx); err != nil || len(list.Destinations) != 1 || list.Destinations[0].ID != "external:"+ghost {
		t.Fatalf("destinations after flag off = %+v, %v", list, err)
	}
	if got := labArchives(t, unit.Where); len(got) != 1 {
		t.Fatalf("flag off touched the archives already written: %v", got)
	}

	on := &apiv1.UpdateExternalDiskRequest{}
	on.SetBackupDestination(apiv1.NewOptBool(true))
	if _, err := h.UpdateExternalDisk(ctx, on, apiv1.UpdateExternalDiskParams{Label: label}); err != nil {
		t.Fatalf("flag on: %v", err)
	}
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run after flag on: %v", err)
	}
	// Retention keeps one daily archive, so the new one replaces the first.
	if got := labArchives(t, unit.Where); len(got) != 1 || got[0] == firstArchive {
		t.Fatalf("archives after the flag was set again = %v, want a new archive in place of %s", got, firstArchive)
	}
	if err := h.DeleteBackupDestination(ctx, apiv1.DeleteBackupDestinationParams{DestinationId: "external:" + label}); err != nil {
		t.Fatalf("DeleteBackupDestination: %v", err)
	}
	row, err := store.NewExternalStore(db).GetExternalDisk(ctx, label)
	if err != nil || row.BackupDestination {
		t.Fatalf("stored disk after deleting its destination = %+v, %v; want the flag cleared", row, err)
	}
}

// TestLabExternalBackupDestination_EjectWaitsForTheWriteInFlight proves #454
// against a real mount: a backup has confirmed the disk is mounted (the
// production mountinfo check, held open by the test) when the eject arrives
// through the handler. The disk stays mounted until the archive is on it, and
// nothing lands in the mount point on the root filesystem afterwards.
func TestLabExternalBackupDestination_EjectWaitsForTheWriteInFlight(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	r := disk.CommandRunner{}

	const label = "labeject"
	dev, uuid := labLoopFilesystem(ctx, t, r, lab, "external-454")
	unit, err := disk.ExternalMountUnit(label, uuid, disk.XFS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), "umount", unit.Where)
		_ = os.Remove(unit.Where)
	})

	db := openTestDB(t)
	checking, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	svc := &backup.Service{
		DB:            db,
		Paths:         backup.DefaultPaths(t.TempDir(), filepath.Join(t.TempDir(), "hoserva")),
		Store:         api.NewBackupDestinationStore(db),
		Hostname:      "lab-host",
		Version:       "0.0.0-lab",
		ExternalGates: &backup.ExternalWriteGates{},
		ExternalMounted: func(ctx context.Context, path string) (bool, error) {
			mounted, err := disk.KernelMounts{}.IsMounted(ctx, path)
			once.Do(func() {
				close(checking)
				<-release
			})
			return mounted, err
		},
	}
	provider := disk.NewFakeProvider()
	provider.AddDisk("/dev/sda", disk.Disk{Size: 8 * disk.TB, Boot: true})
	provider.AddDisk(dev, disk.Disk{Size: 320 * disk.MB, Filesystem: "xfs", FSUUID: uuid})
	h := &api.Handler{ArrayStore: store.NewArrayStore(db), Disks: provider, DiskRunner: r, Backup: svc, ExternalWriteGates: svc.ExternalGates}

	if _, err := h.RegisterExternalDisk(ctx, &apiv1.RegisterExternalDiskRequest{
		Device: dev, Label: label, BackupDestination: apiv1.NewOptBool(true),
	}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	if _, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: label}); err != nil {
		t.Fatalf("MountExternalDisk: %v", err)
	}

	run := make(chan error, 1)
	go func() { run <- svc.Run(ctx) }()
	<-checking

	ejected := make(chan error, 1)
	go func() {
		_, err := h.EjectExternalDisk(ctx, apiv1.EjectExternalDiskParams{Label: label})
		ejected <- err
	}()
	select {
	case err := <-ejected:
		t.Fatalf("EjectExternalDisk returned while a write to the disk was admitted: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if mounted, err := disk.IsMountpoint(unit.Where); err != nil || !mounted {
		t.Fatalf("%s mounted = %v, %v while a write was in flight, want it still mounted", unit.Where, mounted, err)
	}
	close(release)

	if err := <-run; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := <-ejected; err != nil {
		t.Fatalf("EjectExternalDisk: %v", err)
	}
	if mounted, err := disk.IsMountpoint(unit.Where); err != nil || mounted {
		t.Fatalf("%s mounted = %v, %v after the eject, want it unmounted", unit.Where, mounted, err)
	}
	if got := labArchives(t, unit.Where); len(got) != 0 {
		t.Fatalf("archives on the root filesystem under %s after the eject: %v", unit.Where, got)
	}
	if _, err := h.MountExternalDisk(ctx, apiv1.MountExternalDiskParams{Label: label}); err != nil {
		t.Fatalf("MountExternalDisk again: %v", err)
	}
	if got := labArchives(t, unit.Where); len(got) != 1 {
		t.Fatalf("archives on the remounted disk = %v, want the one written before the eject", got)
	}
}
