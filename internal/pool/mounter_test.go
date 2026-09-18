package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestMounter_Mount(t *testing.T) {
	r := disk.NewFakeRunner()
	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "mergerfs" {
		t.Fatalf("Mount: got calls %+v, want exactly one mergerfs call", calls)
	}
}

func TestMounter_Mount_PropagatesError(t *testing.T) {
	r := disk.NewFakeRunner()
	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	argv := m.Argv()
	wantErr := errors.New("mount failed")
	r.Script(argv[0], argv[1:], nil, wantErr)

	mounter := Mounter{Runner: r}
	if err := mounter.Mount(context.Background(), m); !errors.Is(err, wantErr) {
		t.Fatalf("Mount: got %v, want it to wrap %v", err, wantErr)
	}
}

func TestMounter_Unmount(t *testing.T) {
	r := disk.NewFakeRunner()
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Unmount(context.Background(), "/mnt/user"); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "fusermount" {
		t.Fatalf("Unmount: got calls %+v, want exactly one fusermount call", calls)
	}
}

// TestMounter_Remount is doc 02 §4 "Adding a disk" step 6: unmount, then
// mount the grown Mount — in that order, and both against the real
// mount point.
func TestMounter_Remount(t *testing.T) {
	r := disk.NewFakeRunner()
	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, nil)
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), m); err != nil {
		t.Fatalf("Remount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 || calls[0].Name != "fusermount" || calls[1].Name != "mergerfs" {
		t.Fatalf("Remount: got calls %+v, want [fusermount mergerfs] in that order", calls)
	}
}

func TestMounter_Remount_PropagatesUnmountError(t *testing.T) {
	r := disk.NewFakeRunner()
	wantErr := errors.New("busy")
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, wantErr)

	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), m); !errors.Is(err, wantErr) {
		t.Fatalf("Remount: got %v, want it to wrap %v", err, wantErr)
	}
	if len(r.Calls()) != 1 {
		t.Fatalf("Remount: got calls %+v, want mount never attempted after a failed unmount", r.Calls())
	}
}
