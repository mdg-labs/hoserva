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
