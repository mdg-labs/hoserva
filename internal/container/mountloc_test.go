package container

import (
	"errors"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

func TestClassifyMount(t *testing.T) {
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 10, Mountpoint: "/mnt/disk10"},
		{Role: store.ArrayRoleCache, Mountpoint: "/mnt/cache"},
	}
	tests := []struct {
		name   string
		source string
		want   MountLocation
	}{
		{"a share", "/mnt/user/media", MountLocation{Kind: MountPool, Share: "media"}},
		{"below a share", "/mnt/user/media/movies/", MountLocation{Kind: MountPool, Share: "media"}},
		{"the pool root", "/mnt/user", MountLocation{Kind: MountPool}},
		{"an uncleaned path into the pool", "/mnt/user/../disk1/x", MountLocation{Kind: MountDisk, Disk: 1}},
		{"a data disk", "/mnt/disk1/media", MountLocation{Kind: MountDisk, Disk: 1}},
		{"a data disk's root", "/mnt/disk1", MountLocation{Kind: MountDisk, Disk: 1}},
		{"disk 10 is not disk 1", "/mnt/disk10/x", MountLocation{Kind: MountDisk, Disk: 10}},
		{"the cache disk", "/mnt/cache/appdata/jellyfin", MountLocation{Kind: MountCache}},
		{"a name that only starts like the pool", "/mnt/users/x", MountLocation{Kind: MountOutside}},
		{"a name that only starts like a disk", "/mnt/disk11/x", MountLocation{Kind: MountOutside}},
		{"a parity disk", "/mnt/parity1/x", MountLocation{Kind: MountOutside}},
		{"the boot device", "/var/run/docker.sock", MountLocation{Kind: MountOutside}},
		{"a path no disk is mounted at", "/mnt/disks/usb", MountLocation{Kind: MountOutside}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyMount(tt.source, disks); got != tt.want {
				t.Fatalf("ClassifyMount(%q) = %+v, want %+v", tt.source, got, tt.want)
			}
		})
	}
}

func TestClassifyMount_NoArrayStillKnowsThePool(t *testing.T) {
	if got := ClassifyMount("/mnt/user/media", nil); got != (MountLocation{Kind: MountPool, Share: "media"}) {
		t.Fatalf("pool path with no array = %+v", got)
	}
	if got := ClassifyMount("/mnt/cache/appdata/x", nil); got.Kind != MountOutside {
		t.Fatalf("cache path with no array = %+v, want outside: no cache disk is known", got)
	}
}

func TestFakeProvider_Runtime(t *testing.T) {
	ctx := t.Context()
	f := NewFakeProvider()
	f.AddContainer(Container{ID: "abc", Name: "jellyfin", State: "exited"})
	created := timeAt(t, "2026-09-01T08:00:00Z")
	if err := f.SetCreatedAt("jellyfin", created); err != nil {
		t.Fatal(err)
	}
	if err := f.SetRestartCount("jellyfin", 3); err != nil {
		t.Fatal(err)
	}
	got, err := f.Runtime(ctx, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(created) || !got.StartedAt.IsZero() || got.RestartCount != 3 {
		t.Fatalf("Runtime = %+v, want created %v, never started, 3 restarts", got, created)
	}
	if _, err := f.Runtime(ctx, "ghost"); err == nil {
		t.Fatal("Runtime of an unknown container succeeded")
	}
}

func timeAt(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func TestEngineClient_RuntimeReadsCreationStartAndRestartsFromOneInspection(t *testing.T) {
	ctx := t.Context()
	e := newStateEngine(true)
	info := &e.containers[0].info
	info.RestartCount = 4
	wantCreated := timeAt(t, info.Created)
	wantStarted := timeAt(t, info.State.StartedAt)
	c := &EngineClient{cli: e}

	got, err := c.Runtime(ctx, "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(wantCreated) || !got.StartedAt.Equal(wantStarted) || got.RestartCount != 4 {
		t.Fatalf("Runtime = %+v, want created %v, started %v, 4 restarts", got, wantCreated, wantStarted)
	}

	info.State.StartedAt = neverStamp
	got, err = c.Runtime(ctx, "jellyfin")
	if err != nil || !got.StartedAt.IsZero() {
		t.Fatalf("Runtime of a container never started = %+v, %v, want the zero start time", got, err)
	}

	if _, err := c.Runtime(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Runtime of an unknown container = %v, want ErrNotFound", err)
	}
	info.Created = "last tuesday"
	if got, err := c.Runtime(ctx, "jellyfin"); err == nil {
		t.Fatalf("Runtime with an unreadable creation time = %+v, nil, want an error", got)
	}
}
