package backup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestExternalDestination_IsLocalMountPath(t *testing.T) {
	got, err := ExternalDestination("backup")
	if err != nil {
		t.Fatalf("ExternalDestination: %v", err)
	}
	if got.Path != "/mnt/disks/backup" {
		t.Fatalf("Path = %q, want /mnt/disks/backup", got.Path)
	}
	if !disk.IsExternalMountpoint(got.Path) {
		t.Fatalf("Path %q is not an external mountpoint", got.Path)
	}
	if got.ID != "external:backup" || !got.Enabled {
		t.Fatalf("got %+v", got)
	}
}

func TestWriteArchive_AcceptsExternalDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "hoserva-config-2026-09-20T03-00.tar.zst")
	if err := os.WriteFile(src, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := Destination{ID: "external:backup", Path: dir, Enabled: true}
	if err := writeArchive(dest, src); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(src))); err != nil {
		t.Fatalf("archive missing at destination: %v", err)
	}
}

func TestExternalDestination_RejectsInvalidLabel(t *testing.T) {
	if _, err := ExternalDestination("../etc"); err == nil {
		t.Fatal("ExternalDestination(../etc): expected an error")
	}
}
