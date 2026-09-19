package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestWriteDiskMountsBodyMatchesRenderExactly(t *testing.T) {
	g := NewGenerator(t.TempDir())
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	unit := disk.MountUnit{
		Where:       "/mnt/disk1",
		UUID:        "uuid-disk1",
		Filesystem:  disk.XFS,
		Description: "Hoserva data disk 1",
	}

	if err := g.WriteDiskMounts(context.Background(), []disk.MountUnit{unit}, "array create", 1, now); err != nil {
		t.Fatalf("WriteDiskMounts: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(g.Root, "systemd", "system", disk.UnitFileName(unit.Where)))
	if err != nil {
		t.Fatalf("reading written unit: %v", err)
	}
	want := Header("array create", 1, now) + unit.Render()
	if string(got) != want {
		t.Fatalf("written unit mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
