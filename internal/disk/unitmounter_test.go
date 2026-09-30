package disk

import (
	"context"
	"errors"
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
	r.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte("uuid-disk1\n"), nil)
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	calls := r.Calls()
	if len(calls) != 2 || calls[1].Name != "findmnt" {
		t.Fatalf("calls = %+v, want one mount then a findmnt confirming the UUID", calls)
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

func TestDirectMounter_AlreadyMountedByUUIDIsSuccess(t *testing.T) {
	r := NewFakeRunner()
	where := t.TempDir()
	mountArgs := []string{"-t", "xfs", "-o", "defaults,nofail", "-U", "uuid-disk1", where}
	r.Script("mount", mountArgs, nil, errors.New("already mounted"))
	r.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte("uuid-disk1\n"), nil)
	m := DirectMounter{Runner: r}
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err != nil {
		t.Fatalf("Mount: %v", err)
	}
}

func TestDirectMounter_AlreadyMountedWrongUUIDIsError(t *testing.T) {
	r := NewFakeRunner()
	where := t.TempDir()
	mountErr := errors.New("already mounted")
	r.Script("mount", []string{"-t", "xfs", "-o", "defaults,nofail", "-U", "uuid-disk1", where}, nil, mountErr)
	r.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte("other-uuid\n"), nil)
	m := DirectMounter{Runner: r}
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err == nil {
		t.Fatal("Mount: expected an error when the mountpoint is occupied by a different UUID")
	}
}

func TestDirectMounter_MountExitsZeroButNothingMountedIsError(t *testing.T) {
	r := NewFakeRunner()
	where := t.TempDir()
	r.Script("mount", []string{"-t", "xfs", "-o", "defaults,nofail", "-U", "uuid-disk1", where}, nil, nil)
	r.Script("findmnt", []string{"-n", "-o", "UUID", where}, nil, errors.New("exit status 1"))
	m := DirectMounter{Runner: r}
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	err := m.Mount(context.Background(), u)
	if err == nil {
		t.Fatal("Mount: nil for a mount that exited 0 without mounting anything (nofail on an absent UUID)")
	}
	if !strings.Contains(err.Error(), "uuid-disk1") || !strings.Contains(err.Error(), where) {
		t.Fatalf("Mount error = %q, want it to name the UUID and the path", err)
	}
}

func TestDirectMounter_MountExitsZeroWithOtherUUIDIsError(t *testing.T) {
	r := NewFakeRunner()
	where := t.TempDir()
	r.Script("mount", []string{"-t", "xfs", "-o", "defaults,nofail", "-U", "uuid-disk1", where}, nil, nil)
	r.Script("findmnt", []string{"-n", "-o", "UUID", where}, []byte("other-uuid\n"), nil)
	m := DirectMounter{Runner: r}
	u := MountUnit{Where: where, UUID: "uuid-disk1", Filesystem: XFS}
	if err := m.Mount(context.Background(), u); err == nil {
		t.Fatal("Mount: nil although a different UUID is mounted at the path")
	}
}
