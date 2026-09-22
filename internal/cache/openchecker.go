package cache

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// OpenChecker reports whether any process currently holds path open —
// including mergerfs's own fuse process, which is how a client reached
// through /mnt/user/<share> shows up (doc 09 §2): "Clients reach cache
// files through /mnt/user/<share>, so a file a container or SMB client
// has open shows up in fuser/lsof as held by the mergerfs process, not by
// the client." Every mover run checks it twice: before starting a file's
// copy, and again immediately before unlinking the source, so a file that
// becomes open in between is never corrupted.
type OpenChecker interface {
	IsOpen(ctx context.Context, path string) (bool, error)
}

// ProcOpenChecker is the real OpenChecker. It walks /proc/<pid>/fd and
// compares each open file descriptor's device and inode to path's own,
// rather than shelling out to fuser or lsof — neither is guaranteed
// installed, and CLAUDE.md rules out interpolating a path into a shell
// command in the first place. Matching by device+inode rather than by
// resolved path string is what makes this correct when mergerfs holds
// path open through a different mount than the one the mover itself
// walks: os.Stat on a /proc/<pid>/fd entry follows the symlink to the
// same underlying file however it was opened.
type ProcOpenChecker struct {
	// ProcPath overrides "/proc" — for tests only; production leaves it
	// empty.
	ProcPath string
}

// IsOpen reports whether any process visible under ProcPath currently has
// path open. A process this call cannot inspect (already exited, or not
// permitted) is silently skipped rather than treated as an error: the
// check runs as root in production, where every process is visible, and
// a transient race with a process exiting mid-scan is not a reason to
// fail the whole check.
func (c ProcOpenChecker) IsOpen(ctx context.Context, path string) (bool, error) {
	proc := c.ProcPath
	if proc == "" {
		proc = "/proc"
	}

	var target syscall.Stat_t
	if err := syscall.Stat(path, &target); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	pids, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	for _, p := range pids {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join(proc, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			var st syscall.Stat_t
			if err := syscall.Stat(filepath.Join(fdDir, fd.Name()), &st); err != nil {
				continue
			}
			if st.Dev == target.Dev && st.Ino == target.Ino {
				return true, nil
			}
		}
	}
	return false, nil
}

var _ OpenChecker = ProcOpenChecker{}

// OpenSnapshot answers IsOpen for any number of paths from one /proc walk
// already taken, rather than repeating that walk per call — resolved from
// an in-memory device+inode set, with no further syscalls beyond the
// queried path's own Stat.
type OpenSnapshot interface {
	IsOpen(path string) (bool, error)
}

// Snapshotter is the optional capability an OpenChecker can implement to
// serve a batch of pre-copy checks from one /proc walk instead of one walk
// per file — Run's own use of it, once per share, is what keeps a pass
// over N files to at most one full walk (doc 09 §2, #238). It is
// deliberately not part of OpenChecker itself: the pre-unlink re-check
// immediately before a source unlink always calls OpenChecker.IsOpen
// directly and must never be answered from a snapshot taken before the
// copy that re-check exists to guard against.
type Snapshotter interface {
	Snapshot(ctx context.Context) (OpenSnapshot, error)
}

// procOpenSnapshot is ProcOpenChecker's own OpenSnapshot: every
// device+inode pair that was open across every process at the moment the
// walk ran.
type procOpenSnapshot map[devIno]bool

type devIno struct {
	dev, ino uint64
}

// IsOpen reports whether path's current device and inode were in the open
// set at snapshot time.
func (s procOpenSnapshot) IsOpen(path string) (bool, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return s[devIno{dev: uint64(st.Dev), ino: st.Ino}], nil
}

// Snapshot walks /proc once and returns an OpenSnapshot that can answer
// IsOpen for any number of paths afterward without repeating the walk.
func (c ProcOpenChecker) Snapshot(ctx context.Context) (OpenSnapshot, error) {
	proc := c.ProcPath
	if proc == "" {
		proc = "/proc"
	}

	open := make(procOpenSnapshot)
	pids, err := os.ReadDir(proc)
	if err != nil {
		return nil, err
	}
	for _, p := range pids {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join(proc, p.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			var st syscall.Stat_t
			if err := syscall.Stat(filepath.Join(fdDir, fd.Name()), &st); err != nil {
				continue
			}
			open[devIno{dev: uint64(st.Dev), ino: st.Ino}] = true
		}
	}
	return open, nil
}

var _ Snapshotter = ProcOpenChecker{}

// snapshotChecker adapts an OpenSnapshot back onto OpenChecker, so the
// mover's pre-copy check can use whichever one a share's snapshot
// produced without needing to know it came from a snapshot rather than a
// live walk.
type snapshotChecker struct {
	snap OpenSnapshot
}

func (s snapshotChecker) IsOpen(_ context.Context, path string) (bool, error) {
	return s.snap.IsOpen(path)
}

var _ OpenChecker = snapshotChecker{}

// FakeOpenChecker is the scriptable OpenChecker unit tests use in place
// of scanning a real /proc (CLAUDE.md: "every system-touching subsystem
// sits behind a package interface with a scriptable fake").
type FakeOpenChecker struct {
	open map[string]bool
}

// NewFakeOpenChecker returns a FakeOpenChecker with nothing marked open.
func NewFakeOpenChecker() *FakeOpenChecker {
	return &FakeOpenChecker{open: make(map[string]bool)}
}

// SetOpen scripts path as held open (or not) by some process, standing in
// for a real held file descriptor — including one mergerfs itself would
// hold on behalf of a client using the union path.
func (f *FakeOpenChecker) SetOpen(path string, open bool) {
	f.open[path] = open
}

// IsOpen returns exactly what SetOpen last recorded for path, false for
// anything never scripted.
func (f *FakeOpenChecker) IsOpen(_ context.Context, path string) (bool, error) {
	return f.open[path], nil
}

var _ OpenChecker = (*FakeOpenChecker)(nil)
