package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func testWhere(t *testing.T) string {
	return filepath.Join(t.TempDir(), "mnt", "user")
}

func TestMounter_Mount_CreatesWhere(t *testing.T) {
	where := testWhere(t)
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r := disk.NewFakeRunner()
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if _, err := os.Stat(where); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Where= %s should not exist before Mount", where)
	}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	info, err := os.Stat(where)
	if err != nil {
		t.Fatalf("after Mount, Stat(%s): %v", where, err)
	}
	if !info.IsDir() {
		t.Fatalf("after Mount, %s is not a directory", where)
	}
}

func TestMounter_Mount(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
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
	where := testWhere(t)
	r := disk.NewFakeRunner()
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
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
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	m := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	r.Script("fusermount", []string{"-u", where}, nil, nil)
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), previous, m); err != nil {
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

	previous := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	m := previous
	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), previous, m); !errors.Is(err, wantErr) {
		t.Fatalf("Remount: got %v, want it to wrap %v", err, wantErr)
	}
	if len(r.Calls()) != 1 {
		t.Fatalf("Remount: got calls %+v, want mount never attempted after a failed unmount", r.Calls())
	}
}

// TestMounter_Remount_RollsBackOnMountFailure is this issue's central
// safety property: if the grown mount fails, Remount must not leave
// mnt.Where unmounted — it re-mounts previous so a malformed branch list
// degrades to "the expansion didn't take" rather than a storage outage.
func TestMounter_Remount_RollsBackOnMountFailure(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	grown := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r.Script("fusermount", []string{"-u", where}, nil, nil)
	wantErr := errors.New("mergerfs: invalid branch")
	grownArgv := grown.Argv()
	r.Script(grownArgv[0], grownArgv[1:], nil, wantErr)
	previousArgv := previous.Argv()
	r.Script(previousArgv[0], previousArgv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), previous, grown); !errors.Is(err, wantErr) {
		t.Fatalf("Remount: got %v, want it to wrap %v", err, wantErr)
	}

	calls := r.Calls()
	if len(calls) != 3 {
		t.Fatalf("Remount: got calls %+v, want [fusermount, failed mergerfs, rollback mergerfs]", calls)
	}
	if calls[0].Name != "fusermount" || calls[1].Name != "mergerfs" || calls[2].Name != "mergerfs" {
		t.Fatalf("Remount: got calls %+v, want [fusermount mergerfs mergerfs]", calls)
	}
	if calls[2].Args[len(calls[2].Args)-2] != "/mnt/disk1=RW" {
		t.Fatalf("Remount: rollback call args %+v, want the previous (ungrown) branch list", calls[2].Args)
	}
}

// TestMounter_Remount_ReportsRollbackFailureAlongsideTheOriginalError
// covers the case where the pool can't even go back to how it was: both
// errors must reach the caller, since losing the original failure would
// hide why Remount stopped short, and losing the rollback failure would
// hide that mnt.Where is now unmounted.
func TestMounter_Remount_ReportsRollbackFailureAlongsideTheOriginalError(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	grown := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r.Script("fusermount", []string{"-u", where}, nil, nil)
	mountErr := errors.New("mergerfs: invalid branch")
	grownArgv := grown.Argv()
	r.Script(grownArgv[0], grownArgv[1:], nil, mountErr)
	rollbackErr := errors.New("mergerfs: still invalid")
	previousArgv := previous.Argv()
	r.Script(previousArgv[0], previousArgv[1:], nil, rollbackErr)

	mounter := Mounter{Runner: r}
	err := mounter.Remount(context.Background(), previous, grown)
	if !errors.Is(err, mountErr) {
		t.Fatalf("Remount: got %v, want it to wrap the original mount error %v", err, mountErr)
	}
	if err == nil || !strings.Contains(err.Error(), rollbackErr.Error()) {
		t.Fatalf("Remount: got %v, want it to also mention the rollback failure %v", err, rollbackErr)
	}
}
