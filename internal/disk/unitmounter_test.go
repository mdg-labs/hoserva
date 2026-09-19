package disk

import (
	"context"
	"strings"
	"testing"
)

func TestFakeMounter_RecordsMounts(t *testing.T) {
	f := NewFakeMounter()
	u := MountUnit{Where: "/mnt/disk1", UUID: "uuid-disk1", Filesystem: XFS}
	if err := f.Mount(context.Background(), u); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(f.Mounts) != 1 || f.Mounts[0].UUID != "uuid-disk1" {
		t.Fatalf("Mounts = %+v", f.Mounts)
	}
}

func TestSystemdMounter_StartsUnitByFilename(t *testing.T) {
	r := NewFakeRunner()
	m := SystemdMounter{Runner: r}
	u := MountUnit{Where: "/mnt/parity1", UUID: "uuid-p", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v, want daemon-reload then start", calls)
	}
	if calls[0].Name != "systemctl" || strings.Join(calls[0].Args, " ") != "daemon-reload" {
		t.Fatalf("first call = %+v, want systemctl daemon-reload", calls[0])
	}
	if calls[1].Name != "systemctl" || strings.Join(calls[1].Args, " ") != "start mnt-parity1.mount" {
		t.Fatalf("second call = %+v, want systemctl start mnt-parity1.mount", calls[1])
	}
}

func TestDirectMounter_MountsByUUIDArgv(t *testing.T) {
	r := NewFakeRunner()
	m := DirectMounter{Runner: r}
	where := t.TempDir()
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one mount", calls)
	}
	if calls[0].Name != "mount" {
		t.Fatalf("call name = %q, want mount", calls[0].Name)
	}
	joined := strings.Join(calls[0].Args, " ")
	if !strings.Contains(joined, "-U uuid-disk1") || !strings.Contains(joined, where) {
		t.Fatalf("mount argv = %q, want -U uuid-disk1 and the mountpoint", joined)
	}
	if strings.Contains(joined, "/dev/") {
		t.Fatalf("mount argv named a device path: %q", joined)
	}
}
