package job

import (
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

// TestDataDiskLabelForMountpoint_AgreesWithRenderAcrossARoleIndexGap is
// #360's own acceptance criterion: the "dN" label this returns for a
// display preview must be the same label parity.Layout.Render actually
// puts in snapraid.conf for that disk, even once an earlier removal
// (#358) has left role_index 2 missing. Position-based counting ("dN" by
// the i'th data row seen) would instead call /mnt/disk3 "d2" here.
func TestDataDiskLabelForMountpoint_AgreesWithRenderAcrossARoleIndexGap(t *testing.T) {
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 3, Mountpoint: "/mnt/disk3"},
	}

	got, err := DataDiskLabelForMountpoint(disks, "/mnt/disk3")
	if err != nil {
		t.Fatalf("DataDiskLabelForMountpoint: %v", err)
	}
	if got != "d3" {
		t.Fatalf("DataDiskLabelForMountpoint(/mnt/disk3) = %q, want d3", got)
	}

	rendered, err := layoutFromStore(disks).Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(rendered, "data "+got+" /mnt/disk3/\n") {
		t.Fatalf("Render() = %q, does not name /mnt/disk3 as %s the way DataDiskLabelForMountpoint resolved it", rendered, got)
	}
}

func TestDataDiskLabelForMountpoint_NoDiskAtMountpoint(t *testing.T) {
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
	}
	if _, err := DataDiskLabelForMountpoint(disks, "/mnt/disk9"); err == nil {
		t.Fatal("DataDiskLabelForMountpoint: got nil error, want one for a mountpoint with no data disk")
	}
}
