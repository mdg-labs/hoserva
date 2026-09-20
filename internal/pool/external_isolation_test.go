package pool

import (
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestCatchAllMount_OmitsExternalMounts(t *testing.T) {
	m, err := CatchAllMount([]string{"/mnt/disk1", "/mnt/disk2"}, DefaultOptions())
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	body := m.Render()
	if strings.Contains(body, "/mnt/disks/") || strings.Contains(m.What, "/mnt/disks/") {
		t.Fatalf("mergerfs unit named an external disk: What=%q body=\n%s", m.What, body)
	}
	if disk.IsExternalMountpoint("/mnt/disk1") {
		t.Fatal("array data path classified as external")
	}
}
