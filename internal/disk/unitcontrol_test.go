package disk

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMountUnitController_MountAndUnmount(t *testing.T) {
	unit := MountUnit{Where: "/mnt/disk1", UUID: "1234", Filesystem: XFS, DeviceTimeout: 30 * time.Second}
	r := NewFakeRunner()
	r.Script("systemctl", []string{"start", "mnt-disk1.mount"}, nil, nil)
	r.Script("systemctl", []string{"stop", "mnt-disk1.mount"}, nil, nil)

	c := MountUnitController{Unit: unit, Runner: r}
	if got := c.Where(); got != "/mnt/disk1" {
		t.Fatalf("Where() = %q, want /mnt/disk1", got)
	}
	if err := c.Mount(context.Background()); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := c.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "systemctl" || calls[0].Args[0] != "start" || calls[0].Args[1] != "mnt-disk1.mount" {
		t.Fatalf("first call = %+v, want systemctl start mnt-disk1.mount", calls[0])
	}
	if calls[1].Name != "systemctl" || calls[1].Args[0] != "stop" || calls[1].Args[1] != "mnt-disk1.mount" {
		t.Fatalf("second call = %+v, want systemctl stop mnt-disk1.mount", calls[1])
	}
}

func TestMountUnitController_PropagatesErrors(t *testing.T) {
	unit := MountUnit{Where: "/mnt/disk1"}
	r := NewFakeRunner()
	wantErr := errors.New("unit not found")
	r.Script("systemctl", []string{"start", "mnt-disk1.mount"}, nil, wantErr)
	r.Script("systemctl", []string{"stop", "mnt-disk1.mount"}, nil, wantErr)

	c := MountUnitController{Unit: unit, Runner: r}
	if err := c.Mount(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Mount: got %v, want it to wrap %v", err, wantErr)
	}
	if err := c.Unmount(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Unmount: got %v, want it to wrap %v", err, wantErr)
	}
}
