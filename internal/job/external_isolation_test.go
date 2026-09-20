package job

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestApplyArrayFromStore_IgnoresExternalDisks(t *testing.T) {
	ctx := context.Background()
	st := store.NewArrayStore(newTestDB(t))
	if err := st.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G"}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "uuid-c", Mountpoint: "/mnt/cache"},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := st.External().PutExternalDisk(ctx, store.ExternalDisk{
		Label:      "backup",
		Device:     "/dev/sde",
		Filesystem: "xfs",
		FSUUID:     "uuid-ext",
		Mountpoint: "/mnt/disks/backup",
	}); err != nil {
		t.Fatalf("PutExternalDisk: %v", err)
	}

	genRoot := t.TempDir()
	mounter := disk.NewFakeMounter()
	if err := applyArrayFromStore(ctx, st, config.NewGenerator(genRoot), mounter, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("applyArrayFromStore: %v", err)
	}

	for _, u := range mounter.Mounts {
		if disk.IsExternalMountpoint(u.Where) {
			t.Fatalf("array apply mounted external disk %+v", u)
		}
	}

	settings, disks, err := st.GetArray(ctx)
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	poolState := poolStateFromStore(settings, disks)
	for _, p := range poolState.DataDisks {
		if disk.IsExternalMountpoint(p) {
			t.Fatalf("pool data disk %q is an external mount", p)
		}
	}
	layout := layoutFromStore(disks)
	for _, p := range append(append([]string{}, layout.DataMounts...), layout.ParityMounts...) {
		if disk.IsExternalMountpoint(p) {
			t.Fatalf("snapraid layout includes external mount %q", p)
		}
	}

	body, err := os.ReadFile(filepath.Join(genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	if strings.Contains(string(body), "/mnt/disks/") {
		t.Fatalf("snapraid.conf names an external disk:\n%s", body)
	}
	if err := filepath.Walk(genRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), "/mnt/disks/") {
			t.Errorf("%s names an external disk", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
