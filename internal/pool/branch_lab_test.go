//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It proves the mover write target's branch binds (#656, doc 02
// §1) against the real kernel and a real mergerfs: every branch is a bind
// with nosymfollow, a symlink a user put on a data disk still reads through
// the mount but a write through it is refused, and a data disk that is not
// mounted leaves the mover target serving from the rest until it is back.

package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func labBranchTopology(t *testing.T, name string) (Mounter, Mount, []string) {
	t.Helper()
	lab := labDir(t)
	mounter := Mounter{Runner: disk.CommandRunner{}}
	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	for _, d := range dataDisks {
		mustMkdirAll(t, filepath.Join(d, name))
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(d, name)) })
	}
	share := Share{Name: name, CacheMode: ArrayOnly, CreatePolicy: BalanceAcrossDisks}
	mnt, err := MoverTargetMount(share, dataDisks, Options{MinFreeSpace: "10M", Responsiveness: Responsive})
	if err != nil {
		t.Fatalf("MoverTargetMount: %v", err)
	}
	arrayRoot := filepath.Join(lab, "mnt", "branch-test-array-"+name)
	mnt.Where = filepath.Join(arrayRoot, name)
	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), mnt.Where)
		labReleaseBinds(mounter, mnt)
		_ = os.Remove(mnt.Where)
		_ = os.Remove(arrayRoot)
	})
	return mounter, mnt, dataDisks
}

func TestLabMoverTarget_BranchesAreNosymfollowBindsThatRefuseAUserSymlink(t *testing.T) {
	ctx := context.Background()
	mounter, mnt, dataDisks := labBranchTopology(t, "branchbinds")
	if err := mounter.Mount(ctx, mnt); err != nil {
		t.Fatalf("mounting the mover target: %v", err)
	}

	for i, b := range mnt.Binds {
		opts, mounted, err := disk.ReadMountTarget(b.Where)
		if err != nil || !mounted || !slices.Contains(opts, "nosymfollow") {
			t.Fatalf("bind %s: mounted %v, options %v, err %v — want a nosymfollow mount", b.Where, mounted, opts, err)
		}
		if b.Source != dataDisks[i] {
			t.Fatalf("bind %d is of %s, want %s in branch order", i, b.Source, dataDisks[i])
		}
	}

	outside := filepath.Join(labDir(t), "outside-branchbinds")
	_ = os.RemoveAll(outside)
	mustMkdirAll(t, outside)
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	// A directory made through the mount, looked up and held open there
	// as the mover's copy does, then swapped on its disk for a symlink to
	// outside behind mergerfs's back.
	if err := os.Mkdir(filepath.Join(mnt.Where, "target"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirfd, err := syscall.Open(filepath.Join(mnt.Where, "target"), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(dirfd) }()
	swapped := ""
	for _, d := range dataDisks {
		dir := filepath.Join(d, "branchbinds", "target")
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		if err := os.Rename(dir, dir+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, dir); err != nil {
			t.Fatal(err)
		}
		swapped = dir
		t.Cleanup(func() {
			_ = os.Remove(dir)
			_ = os.RemoveAll(dir + ".moved")
		})
	}
	if swapped == "" {
		t.Fatal("the directory made through the mount is on no data disk")
	}

	// mergerfs creates the file on its branch by path; the bind refuses to
	// follow the symlink there, so nothing lands outside.
	fd, err := unix.Openat(dirfd, "planted", syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err == nil {
		_ = syscall.Close(fd)
		t.Errorf("creating a file in the swapped directory through the mount succeeded")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("%d entries landed outside the disks", len(entries))
	}

	// A user's own symlink still reads, as a symlink, through the mount,
	// and the mover can still recreate one: it creates a symlink and never
	// follows it.
	link := filepath.Join(dataDisks[0], "branchbinds", "elsewhere")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if got, err := os.Readlink(filepath.Join(mnt.Where, "elsewhere")); err != nil || got != outside {
		t.Fatalf("readlink through the mover target = %q, %v, want %q", got, err, outside)
	}
	if err := os.Symlink("target", filepath.Join(mnt.Where, "made-by-mover")); err != nil {
		t.Fatalf("creating a symlink through the mover target: %v", err)
	}
}

// labUnmountDataDisk unmounts the lab data disk at where — its branch bind
// first (disk.DirectMounter) — and returns its loop device, which is
// mounted back when the test ends if the test has not done so itself.
func labUnmountDataDisk(t *testing.T, where string) string {
	t.Helper()
	ctx := context.Background()
	r := disk.CommandRunner{}
	out, err := r.Run(ctx, "findmnt", "-n", "-o", "SOURCE", where)
	dev := strings.TrimSpace(string(out))
	if err != nil || !strings.HasPrefix(dev, "/dev/loop") {
		t.Fatalf("findmnt %s = %q, %v — want the lab's loop device", where, dev, err)
	}
	if err := (disk.DirectMounter{Runner: r}).Unmount(ctx, disk.MountUnit{Where: where}); err != nil {
		t.Fatalf("unmounting %s: %v", where, err)
	}
	t.Cleanup(func() {
		if _, mounted, _ := disk.ReadMountTarget(where); !mounted {
			if _, err := r.Run(context.Background(), "mount", dev, where); err != nil {
				t.Errorf("mounting %s back at %s: %v", dev, where, err)
			}
		}
	})
	return dev
}

// With a data disk not mounted, the mover target still mounts and places
// and serves files on the rest; once the disk is back, mounting the target
// again binds the disk's real filesystem instead of the empty mountpoint
// directory, and its files are served again.
func TestLabMoverTarget_MountsAndServesWithADataDiskUnmounted(t *testing.T) {
	ctx := context.Background()
	mounter, mnt, dataDisks := labBranchTopology(t, "branchmissing")
	mustWriteFile(t, filepath.Join(dataDisks[2], "branchmissing", "on-disk3.txt"), "from disk3")
	mustWriteFile(t, filepath.Join(dataDisks[0], "branchmissing", "on-disk1.txt"), "from disk1")
	dev := labUnmountDataDisk(t, dataDisks[2])

	if err := mounter.Mount(ctx, mnt); err != nil {
		t.Fatalf("mounting the mover target with disk3 unmounted: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(mnt.Where, "on-disk1.txt")); got != "from disk1" {
		t.Fatalf("on-disk1.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(mnt.Where, "on-disk3.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("on-disk3.txt with disk3 unmounted: %v, want it absent", err)
	}
	mustWriteFile(t, filepath.Join(mnt.Where, "placed.txt"), "placed while disk3 is out")
	landed := 0
	for _, d := range dataDisks[:2] {
		if _, err := os.Stat(filepath.Join(d, "branchmissing", "placed.txt")); err == nil {
			landed++
		}
	}
	if landed != 1 {
		t.Fatalf("placed.txt is on %d of the mounted disks, want exactly one", landed)
	}
	if entries, _ := os.ReadDir(dataDisks[2]); len(entries) != 0 {
		t.Fatalf("disk3's bare mountpoint directory holds %d entries, want none", len(entries))
	}

	if _, err := (disk.CommandRunner{}).Run(ctx, "mount", dev, dataDisks[2]); err != nil {
		t.Fatalf("mounting disk3 back: %v", err)
	}
	if err := mounter.Mount(ctx, mnt); err != nil {
		t.Fatalf("mounting the mover target again with disk3 back: %v", err)
	}
	// Read by a name never looked up while disk3 was out, so mergerfs's
	// negative entry cache cannot answer for it.
	mustWriteFile(t, filepath.Join(dataDisks[2], "branchmissing", "back-on-disk3.txt"), "from disk3")
	if got := mustReadFile(t, filepath.Join(mnt.Where, "back-on-disk3.txt")); got != "from disk3" {
		t.Fatalf("back-on-disk3.txt once disk3 is back = %q", got)
	}
}
