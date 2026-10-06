//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It proves, through the real array-only mergerfs mount the mover
// and a relocation to the array write through, that a target directory on a
// data disk swapped for a symlink to somewhere else on the host while a copy
// runs makes the root daemon create, rename or remove nothing there (#656).
//
// The swap is made on the data disk itself, behind mergerfs's back, after
// the copy has looked the directory up through the mount, so the kernel's
// FUSE entry cache still names a directory: no check above mergerfs can see
// it. mergerfs then carries out the create by path on its branch, and only
// the branch mount itself refusing to follow a symlink (nosymfollow, doc 02
// §1) keeps that path on the disk. An inotify watch on the outside directory
// records every entry created, moved in or removed there, so a temp file that
// was created outside and cleaned up again still fails the test.

package cache

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// labWatchedOutside creates a directory outside every disk and the pool,
// watched for any entry created, moved in or removed, and returns it with a
// function reporting every such event seen so far.
func labWatchedOutside(t *testing.T, name string) (string, func() []string) {
	t.Helper()
	outside := filepath.Join(labDir(t), "outside-"+name)
	if err := os.RemoveAll(outside); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, outside)
	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatalf("inotify_init1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, outside, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_DELETE); err != nil {
		t.Fatalf("inotify_add_watch %s: %v", outside, err)
	}

	var seen []string
	return outside, func() []string {
		buf := make([]byte, 64<<10)
		for {
			n, err := unix.Read(fd, buf)
			if errors.Is(err, unix.EAGAIN) || n <= 0 {
				return seen
			}
			if err != nil {
				t.Fatalf("reading inotify events: %v", err)
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				mask := binary.NativeEndian.Uint32(buf[off+4:])
				nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
				nameBytes := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nameLen]
				seen = append(seen, fmt.Sprintf("%s %s", labInotifyMask(mask), strings.TrimRight(string(nameBytes), "\x00")))
				off += unix.SizeofInotifyEvent + nameLen
			}
		}
	}
}

func labInotifyMask(mask uint32) string {
	var names []string
	for _, m := range []struct {
		bit  uint32
		name string
	}{{unix.IN_CREATE, "IN_CREATE"}, {unix.IN_MOVED_TO, "IN_MOVED_TO"}, {unix.IN_DELETE, "IN_DELETE"}, {unix.IN_ISDIR, "IN_ISDIR"}} {
		if mask&m.bit != 0 {
			names = append(names, m.name)
		}
	}
	return strings.Join(names, "|")
}

// labSwapOnFirstUUID returns a UUID hook that, the first time a copy asks for
// its temp name — after the target directory was resolved through the mount
// and before the temp file is created — moves dir aside on its data disk and
// puts a symlink to outside in its place, the way a share user with write
// access to the parent can at any moment.
func labSwapOnFirstUUID(t *testing.T, dir, outside string) func() string {
	t.Helper()
	t.Cleanup(func() {
		if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(dir)
		}
		_ = os.RemoveAll(dir + ".moved")
	})
	swapped := false
	return func() string {
		if !swapped {
			swapped = true
			if err := os.Rename(dir, dir+".moved"); err != nil {
				t.Errorf("moving %s aside: %v", dir, err)
			}
			if err := os.Symlink(outside, dir); err != nil {
				t.Errorf("symlinking %s to %s: %v", dir, outside, err)
			}
		}
		return "swap-uuid"
	}
}

// labSwapFixture builds the share tree on the cache and the target directory
// on exactly one data disk — so the share's path-preserving create policy
// places the copy there — and looks that directory up through the mover
// target, warming the kernel's FUSE entry cache the way the previous file
// moved into the same directory does.
func labSwapFixture(t *testing.T, top labMoverTopology) (share Share, src, target string) {
	t.Helper()
	share = top.cacheShare()
	labBuildShareTree(t, share.CachePath)
	target = filepath.Join(top.dataDisks[0], share.Name, "documents", "Reports")
	mustMkdirAll(t, target)
	if _, err := os.Stat(filepath.Join(share.ArrayPath, "documents", "Reports")); err != nil {
		t.Fatalf("looking the target directory up through the mover target: %v", err)
	}
	return share, filepath.Join(share.CachePath, "documents", "Reports", "Q3.txt"), target
}

func labAssertSwapWroteNothingOutside(t *testing.T, report Report, src, target, outside string, events func() []string) {
	t.Helper()
	if got := events(); len(got) != 0 {
		t.Errorf("the root daemon touched the outside directory: %v", got)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("%d entries are outside the disks after the run", len(entries))
	}
	e, ok := resultFor(report, "documents/Reports/Q3.txt")
	if !ok || e.Result != ResultFailed || !strings.Contains(e.Err, "is a symbolic link") {
		t.Errorf("entry = %+v, want failed naming the symlink", e)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("the source was not kept: %v", err)
	}
	if entries, _ := os.ReadDir(target + ".moved"); len(entries) != 0 {
		names := make([]string, len(entries))
		for i, en := range entries {
			names[i] = en.Name()
		}
		t.Errorf("the directory swapped away holds %v, want nothing", names)
	}
}

func TestLabMover_ABranchDirectorySwappedForASymlinkMidCopyWritesNothingOutside(t *testing.T) {
	top := bringUpLabMoverTopology(t, "swapmover")
	share, src, target := labSwapFixture(t, top)
	outside, events := labWatchedOutside(t, "swapmover")

	deps := Deps{UUID: labSwapOnFirstUUID(t, target, outside)}
	report, err := Run(context.Background(), []Share{share}, Config{SkipGracePeriod: true, VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	labAssertSwapWroteNothingOutside(t, report, src, target, outside, events)
}

func TestLabRelocateToArray_ABranchDirectorySwappedForASymlinkMidCopyWritesNothingOutside(t *testing.T) {
	top := bringUpLabMoverTopology(t, "swapreloc")
	share, src, target := labSwapFixture(t, top)
	outside, events := labWatchedOutside(t, "swapreloc")

	deps := Deps{UUID: labSwapOnFirstUUID(t, target, outside)}
	report, err := RelocateToArray(context.Background(), share, Config{VerifyChecksum: true}, deps, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("RelocateToArray: %v", err)
	}
	labAssertSwapWroteNothingOutside(t, report, src, target, outside, events)
}
