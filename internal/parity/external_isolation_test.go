package parity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestJournal_DoesNotTrackExternalMount(t *testing.T) {
	j := NewJournal(NewFakeWatcher(), "")
	t.Cleanup(func() { _ = j.Close() })
	if err := j.AddDisk(context.Background(), "d1", "/mnt/disk1"); err != nil {
		t.Fatalf("AddDisk: %v", err)
	}
	if _, err := j.Summary("backup"); !errors.Is(err, ErrDiskNotTracked) {
		t.Fatalf("Summary(backup) = %v, want ErrDiskNotTracked", err)
	}
	if _, err := j.Summary("/mnt/disks/backup"); !errors.Is(err, ErrDiskNotTracked) {
		t.Fatalf("Summary(/mnt/disks/backup) = %v, want ErrDiskNotTracked", err)
	}
}

func TestLayout_RenderOmitsExternalMount(t *testing.T) {
	l := Layout{
		ParityMounts: []string{"/mnt/parity1"},
		DataMounts:   []DataMount{{RoleIndex: 1, Mountpoint: "/mnt/disk1"}, {RoleIndex: 2, Mountpoint: "/mnt/disk2"}},
		CacheMount:   "/mnt/cache",
	}
	body, err := l.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(body, "/mnt/disks/") {
		t.Fatalf("snapraid.conf named an external disk:\n%s", body)
	}
}

func TestGuard_EvaluateUsesOnlyDiffPerDisk(t *testing.T) {
	// SnapRAID diffs array data disks only. An external mount is not a
	// PerDisk key, so emptying /mnt/disks/backup does not trip the guard.
	diff := DiffReport{
		Added:   0,
		Removed: 0,
		Updated: 0,
		PerDisk: map[string]DiskDiff{
			"/mnt/disk1": {FilesBefore: 10, FilesAfter: 10},
		},
	}
	result := Guard{}.Evaluate(diff, nil, nil)
	if result.Blocked {
		t.Fatalf("Evaluate blocked on an array-only diff: %+v", result)
	}
	for _, z := range result.ZeroFilesDisks {
		if disk.IsExternalMountpoint(z.Disk) {
			t.Fatalf("guard treated external mount as a data disk: %+v", result.ZeroFilesDisks)
		}
	}
}
