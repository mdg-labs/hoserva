package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	sharedNVMeByID    = "nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456X"
	sharedNVMeCacheID = sharedNVMeByID + "-part3"
)

// newSharedNVMeSource is an installation whose cache sits on a spare
// partition of its boot disk, and the archive it exported.
func newSharedNVMeSource(t *testing.T) (*wiredInstall, []byte) {
	t.Helper()
	src := newWiredInstall(t)
	if err := src.arrays.PutArray(context.Background(), store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p1", WWN: "wwn-p1", Serial: "ser-uuid-p1", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "ser-uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "ser-uuid-d2", Mountpoint: "/mnt/disk2"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/nvme0n1p3", Filesystem: "ext4", FSUUID: "uuid-cache", Serial: "S4EWNX0M123456X", ByIDName: sharedNVMeCacheID, Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatal(err)
	}
	return src, exportArchive(t, src.handler)
}

func (w *wiredInstall) attachSharedNVMe(partitions ...disk.BootPartition) {
	w.attach("/dev/sdx", "uuid-p1", "wwn-p1")
	w.attach("/dev/sdy", "uuid-d1", "wwn-d1")
	w.attach("/dev/sdz", "uuid-d2", "wwn-d2")
	w.disks.AddDisk("/dev/nvme0n1", disk.Disk{Serial: "S4EWNX0M123456X", ByIDName: sharedNVMeByID, Boot: true, Size: 1 << 40, Partitions: append([]disk.BootPartition{
		{Device: "/dev/nvme0n1p1", ByIDName: sharedNVMeByID + "-part1", Filesystem: "vfat", FSUUID: "uuid-efi"},
		{Device: "/dev/nvme0n1p2", ByIDName: sharedNVMeByID + "-part2", Filesystem: "ext4", FSUUID: "uuid-root"},
	}, partitions...)})
}

func previewMapping(t *testing.T, srv *wireClient, archive []byte) (disks []struct {
	Role   string `json:"role"`
	State  string `json:"state"`
	Device string `json:"device"`
}, mapping string) {
	t.Helper()
	body, ct := importForm(t, archive, false, "")
	code, resp := srv.viaUnix("/config/import/preview", body, ct)
	if code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, resp)
	}
	var preview struct {
		BareMetal struct {
			Disks []struct {
				Role   string `json:"role"`
				State  string `json:"state"`
				Device string `json:"device"`
			} `json:"disks"`
			DiskMapping json.RawMessage `json:"diskMapping"`
		} `json:"bareMetal"`
	}
	if err := json.Unmarshal(resp, &preview); err != nil {
		t.Fatalf("decoding the preview: %v\n%s", err, resp)
	}
	return preview.BareMetal.Disks, string(preview.BareMetal.DiskMapping)
}

// TestBareMetalRestore_ACacheOnABootDiskPartitionIsMatchedAndMounted restores
// an archive whose cache is a partition of the boot disk onto the same
// layout, through the daemon's socket and the generated client: the preview
// matches the cache to the partition, the confirmed mapping carries it, and
// the restore writes the cache's mount unit, reports no disk as not restored
// and records the partition's device.
func TestBareMetalRestore_ACacheOnABootDiskPartitionIsMatchedAndMounted(t *testing.T) {
	ctx := context.Background()
	_, archive := newSharedNVMeSource(t)
	box := newWiredInstall(t)
	box.attachSharedNVMe(disk.BootPartition{Device: "/dev/nvme0n1p3", ByIDName: sharedNVMeCacheID, Filesystem: "ext4", FSUUID: "uuid-cache"})
	srv := box.serve(t)

	disks, mapping := previewMapping(t, srv, archive)
	for _, d := range disks {
		want := "/dev/sdx"
		switch d.Role {
		case "data":
			want = "/dev/sdy"
			if d.Device == "/dev/sdz" {
				want = "/dev/sdz"
			}
		case "cache":
			want = "/dev/nvme0n1p3"
		}
		if d.State != "matched" || d.Device != want {
			t.Errorf("preview %s = %+v, want matched on %s", d.Role, d, want)
		}
	}
	if len(disks) != 4 || !strings.Contains(mapping, `"device":"/dev/nvme0n1p3"`) || strings.Contains(mapping, `"device":"/dev/nvme0n1"`) {
		t.Fatalf("preview disks %+v, mapping %s, want the four disks and the cache confirmed on its partition", disks, mapping)
	}

	report, err := srv.generated().ImportConfig(ctx, &apiv1.ImportConfigReq{
		Confirm:     true,
		Archive:     ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)},
		DiskMapping: apiv1.NewOptString(mapping),
	})
	if err != nil {
		t.Fatalf("ImportConfig with the confirmed mapping: %v", err)
	}
	for _, n := range report.NotRestored {
		if n.Kind == apiv1.ConfigImportNotRestoredKindDisk {
			t.Errorf("the report lists a disk as not restored: %+v", n)
		}
	}
	if got := rowsOf(t, box.db, `SELECT role, device FROM array_disks WHERE role = 'cache'`); got != "cache|/dev/nvme0n1p3" {
		t.Errorf("cache row = %s, want the partition's device", got)
	}
	unit, err := os.ReadFile(filepath.Join(box.etc, "systemd", "system", "mnt-cache.mount"))
	if err != nil || !strings.Contains(string(unit), "What=/dev/disk/by-uuid/uuid-cache") {
		t.Errorf("mnt-cache.mount = %q (%v), want a unit binding the cache's filesystem", unit, err)
	}
}

func TestBareMetalRestore_ACacheBootPartitionThatIsNotTheCacheIsNeitherMountedNorFormatted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		part   []disk.BootPartition
		state  string
		reason apiv1.ConfigImportNotRestoredReason
	}{
		{"another filesystem", []disk.BootPartition{{Device: "/dev/nvme0n1p3", ByIDName: sharedNVMeCacheID, Filesystem: "ext4", FSUUID: "uuid-other"}},
			"replaced", apiv1.ConfigImportNotRestoredReasonDiskReplaced},
		{"no filesystem", []disk.BootPartition{{Device: "/dev/nvme0n1p3", ByIDName: sharedNVMeCacheID}},
			"replaced", apiv1.ConfigImportNotRestoredReasonDiskReplaced},
		{"no partition", nil, "absent", apiv1.ConfigImportNotRestoredReasonDiskAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			_, archive := newSharedNVMeSource(t)
			box := newWiredInstall(t)
			box.attachSharedNVMe(tc.part...)
			srv := box.serve(t)

			disks, mapping := previewMapping(t, srv, archive)
			for _, d := range disks {
				if d.Role == "cache" && d.State != tc.state {
					t.Errorf("preview cache = %+v, want %s", d, tc.state)
				}
			}
			if strings.Contains(mapping, "nvme0n1") {
				t.Fatalf("mapping %s confirms a cache that is not matched", mapping)
			}
			report, err := srv.generated().ImportConfig(ctx, &apiv1.ImportConfigReq{
				Confirm:     true,
				Archive:     ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)},
				DiskMapping: apiv1.NewOptString(mapping),
			})
			if err != nil {
				t.Fatalf("ImportConfig: %v", err)
			}
			named := false
			for _, n := range report.NotRestored {
				named = named || (n.Kind == apiv1.ConfigImportNotRestoredKindDisk && n.Reason == tc.reason && strings.Contains(n.Name, "cache disk 1"))
			}
			if !named {
				t.Fatalf("report = %+v, want the cache reported %s", report.NotRestored, tc.reason)
			}
			if calls := box.disks.FormatCalls(); len(calls) != 0 {
				t.Errorf("the restore formatted %v", calls)
			}
		})
	}
}
