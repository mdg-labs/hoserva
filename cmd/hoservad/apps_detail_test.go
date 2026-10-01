package main

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestGetAppWiring_ReportsTheStorageOfEachMountAndTheContainersLife builds
// the handler the way main.go does for Apps (the Docker provider and the
// array store the daemon holds) and reads one container: its mounts name the
// storage they lie on and it carries when it was created and how often it
// restarted.
func TestGetAppWiring_ReportsTheStorageOfEachMountAndTheContainersLife(t *testing.T) {
	ctx := context.Background()
	db := newNotifyTestDB(t)
	arrays := store.NewArrayStore(db)
	err := arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdd", Filesystem: "ext4", FSUUID: "c", Mountpoint: "/mnt/cache"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := container.NewFakeProvider()
	provider.AddContainer(container.Container{ID: "c1", Name: "jellyfin", State: "running", Mounts: []container.Mount{
		{Source: "/mnt/cache/appdata/jellyfin", Destination: "/config"},
		{Source: "/mnt/user/media", Destination: "/media"},
	}})
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	if err := provider.SetCreatedAt("jellyfin", created); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetRestartCount("jellyfin", 5); err != nil {
		t.Fatal(err)
	}
	h := &api.Handler{Container: provider}
	h.ArrayStore = arrays

	app, err := h.GetApp(ctx, apiv1.GetAppParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	kinds := map[string]apiv1.AppMountLocationKind{}
	for _, m := range app.Mounts {
		loc, _ := m.Location.Get()
		kinds[m.Destination] = loc.Kind
	}
	if kinds["/config"] != apiv1.AppMountLocationKindCache || kinds["/media"] != apiv1.AppMountLocationKindPool {
		t.Errorf("mount kinds = %v, want /config on the cache and /media in the pool", kinds)
	}
	if at, _ := app.CreatedAt.Get(); !at.Equal(created) {
		t.Errorf("createdAt = %v, want %v", app.CreatedAt, created)
	}
	if n, _ := app.RestartCount.Get(); n != 5 {
		t.Errorf("restartCount = %v, want 5", app.RestartCount)
	}
}
