//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host. It proves CheckCreatable's rule against the real mergerfs: a directory
// made through a catch-all whose minfreespace is above every branch's free space
// fails with ENOSPC, and CheckCreatable refuses that same pool; with the floor
// below the room of at least one branch the directory is made and CheckCreatable
// agrees.

package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func TestLabCheckCreatable_AgreesWithMergerfsAboutWhenADirectoryCanBeMade(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	mounter := Mounter{Runner: disk.CommandRunner{}}
	statter := StatfsSpaceStatter{}

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	var largest int64
	for _, d := range dataDisks {
		st, err := statter.StatSpace(ctx, d)
		if err != nil {
			t.Fatalf("data disk %s not present — expected create-array.sh to have mounted it: %v", d, err)
		}
		largest = max(largest, st.FreeBytes)
	}

	where := filepath.Join(lab, "mnt", "pool-creatable-test")
	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), where)
		_ = os.RemoveAll(where)
	})

	for _, tc := range []struct {
		name    string
		dir     string
		minFree string
		refused bool
	}{
		{"every disk below the floor", "below-floor", fmt.Sprintf("%d", largest+(1<<20)), true},
		{"one disk above the floor", "above-floor", fmt.Sprintf("%d", largest/2), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = mounter.Unmount(ctx, where)
			if err := os.RemoveAll(where); err != nil {
				t.Fatalf("removing stale %s: %v", where, err)
			}
			catchAll, err := CatchAllMount(dataDisks, Options{MinFreeSpace: tc.minFree, Responsiveness: Responsive})
			if err != nil {
				t.Fatalf("CatchAllMount: %v", err)
			}
			catchAll.Where = where
			if err := mounter.Mount(ctx, catchAll); err != nil {
				t.Fatalf("mounting the catch-all: %v", err)
			}
			t.Cleanup(func() { _ = mounter.Unmount(context.Background(), where) })

			// A directory made through the pool lives on a data disk and outlives the
			// mount, so each case has its own and removes it before unmounting.
			dir := filepath.Join(where, tc.dir)
			_ = os.Remove(dir)
			t.Cleanup(func() { _ = os.Remove(dir) })

			mkdirErr := os.Mkdir(dir, 0o755)
			checkErr := CheckCreatable(ctx, statter, dataDisks, tc.minFree)

			if tc.refused {
				if !errors.Is(mkdirErr, syscall.ENOSPC) {
					t.Fatalf("mkdir through the pool = %v, want ENOSPC with minfreespace %s above every disk's room (%d)", mkdirErr, tc.minFree, largest)
				}
				if !errors.Is(checkErr, ErrBelowMinFreeSpace) {
					t.Fatalf("CheckCreatable = %v, want ErrBelowMinFreeSpace for the pool mergerfs refused", checkErr)
				}
				return
			}
			if mkdirErr != nil || checkErr != nil {
				t.Fatalf("mkdir through the pool = %v, CheckCreatable = %v, want both to succeed with minfreespace %s below one disk's room (%d)", mkdirErr, checkErr, tc.minFree, largest)
			}
		})
	}
}
