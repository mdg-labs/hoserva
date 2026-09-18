package pool

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestMountController_MountAndUnmount(t *testing.T) {
	m := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	argv := m.Argv()
	r := disk.NewFakeRunner()
	r.Script(argv[0], argv[1:], nil, nil)
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, nil)

	c := MountController{Mnt: m, Mounter: Mounter{Runner: r}}
	if got := c.Where(); got != "/mnt/user" {
		t.Fatalf("Where() = %q, want /mnt/user", got)
	}
	if err := c.Mount(context.Background()); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if err := c.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 || calls[0].Name != "mergerfs" || calls[1].Name != "fusermount" {
		t.Fatalf("got calls %+v, want one mergerfs call then one fusermount call", calls)
	}
}
