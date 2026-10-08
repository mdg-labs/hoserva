package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// sourceEntry is a source entry of a copy or a delete, held by the
// descriptor of the directory that holds it. The directory is opened from the
// trusted root (the cache path, a data disk) one component at a time with
// O_NOFOLLOW, so a directory between them that is, or has become, a symbolic
// link fails the open with beneath.ErrSymlink: a share user who swaps one
// after the tree was enumerated can redirect neither what is read of the
// source (#776) nor what is done to it (#731). Every read of the entry's
// content, extended attributes and link target is then made against that
// descriptor and the leaf's name, never by path, and checks that what it found
// is the file the entry's metadata was read from, by device, inode and change
// time. The caller closes it.
type sourceEntry struct {
	w        *beneath.Walker
	parentfd int
	base     string
	// path is the root joined with the entry's path beneath it, for the checks
	// that can only take a path and for messages.
	path string
	// info is the entry's own metadata, read when it was opened without
	// following the entry itself.
	info os.FileInfo
	// dirs is the metadata of each directory between the root and the entry,
	// outermost first, read from the descriptors the walk opened.
	dirs []os.FileInfo
}

// openSource opens the directory that holds rel beneath root and reads the
// entry's metadata from it. rel is a slash-separated path beneath root.
func openSource(root, rel string) (*sourceEntry, error) {
	comps, err := beneath.Components(filepath.ToSlash(rel))
	if err != nil {
		return nil, err
	}
	w, err := beneath.NewWalker(root)
	if err != nil {
		return nil, err
	}
	var dirs []os.FileInfo
	parentfd, err := w.Dir(strings.Join(comps[:len(comps)-1], "/"), func(_, fd int, _ string) error {
		info, err := os.Stat(fdPath(fd))
		if err != nil {
			return err
		}
		dirs = append(dirs, info)
		return nil
	})
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("resolve %s: %w", rel, err)
	}
	base := comps[len(comps)-1]
	info, err := lstatAt(parentfd, base)
	if err != nil {
		w.Close()
		return nil, err
	}
	return &sourceEntry{w: w, parentfd: parentfd, base: base, path: filepath.Join(root, rel), info: info, dirs: dirs}, nil
}

func (s *sourceEntry) Close() { s.w.Close() }

// lstatAt is os.Lstat for the entry base in the directory parentfd: the
// directory is the descriptor, not a name that could have been swapped, and
// the entry itself is never followed.
func lstatAt(parentfd int, base string) (os.FileInfo, error) {
	info, err := os.Lstat(fdPath(parentfd) + "/" + base)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("lstat %s: %w", base, err)
	}
	return info, nil
}

func errSourceChanged(base string) error {
	return fmt.Errorf("%s is not the file that was chosen to be copied", base)
}

// sameFile reports whether st describes the file the entry's metadata was read
// from: the same device and inode, and the same change time. The inode number
// alone does not tell a file from the one that replaced it, because a
// filesystem hands a freed inode number to the next file it creates, and the
// change time tells them apart only when the clock ticked between the two:
// before kernel 6.13 ext4 and XFS stamp from a coarse clock (one to ten
// milliseconds a tick), so a file replaced within the tick its predecessor
// last changed in, with the freed inode number, passes this check. A file
// whose attributes, link count or content changed since, with a later change
// time, fails the entry and the source is kept. What still holds when a
// replacement does pass: the descriptor is opened beneath the source root, so
// the read stays inside the source directory; the checksum is taken from the
// descriptor the copy was read from, so the target holds exactly what was
// verified; and the delete keeps a source whose size or modification time
// differs from what was copied (sourceStamp).
func sameFile(st *unix.Stat_t, info os.FileInfo) bool {
	want, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(want.Dev) == uint64(st.Dev) && want.Ino == st.Ino &&
		want.Ctim.Sec == st.Ctim.Sec && want.Ctim.Nsec == st.Ctim.Nsec
}

// openFile opens the regular file for reading, without following a link, and
// fails unless it is the file the entry's metadata describes.
func (s *sourceEntry) openFile() (*os.File, error) {
	fd, err := beneath.Open(s.parentfd, s.base, unix.O_RDONLY)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("stat %s: %w", s.base, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || !sameFile(&st, s.info) {
		_ = unix.Close(fd)
		return nil, errSourceChanged(s.base)
	}
	return os.NewFile(uintptr(fd), s.path), nil
}

// hash returns the SHA-256 of the regular file as read back from disk.
func (s *sourceEntry) hash() (string, error) {
	f, err := s.openFile()
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return hashReader(f)
}

// readlink returns the target of the symbolic link, which is never followed,
// and fails unless the link is still the one the entry's metadata describes.
func (s *sourceEntry) readlink() (string, error) {
	buf := make([]byte, s.info.Size()+1)
	for {
		n, err := unix.Readlinkat(s.parentfd, s.base, buf)
		if err != nil {
			return "", &fs.PathError{Op: "readlinkat", Path: s.base, Err: err}
		}
		if n < len(buf) {
			var st unix.Stat_t
			if err := unix.Fstatat(s.parentfd, s.base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return "", fmt.Errorf("lstat %s: %w", s.base, err)
			}
			if !sameFile(&st, s.info) {
				return "", errSourceChanged(s.base)
			}
			return string(buf[:n]), nil
		}
		buf = make([]byte, 2*len(buf))
	}
}

// copyXattrsTo copies the entry's extended attributes onto dst, reading them
// through a descriptor opened on the entry itself, so nothing is read from
// whatever the path names by then.
func (s *sourceEntry) copyXattrsTo(dst string) error {
	fd, err := beneath.Open(s.parentfd, s.base, unix.O_PATH)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", s.base, err)
	}
	if !sameFile(&st, s.info) {
		return errSourceChanged(s.base)
	}
	return copyXattrs(fdPath(fd), dst)
}

// fileOpenChecker is the capability of an OpenChecker that can answer for a
// file by its device and inode, so the question is about the file a
// descriptor-relative lookup found and not about whatever a path names by
// the time the checker resolves it. ProcOpenChecker answers this way; a
// checker that can only answer for a path is asked for the joined path
// (isOpenSource).
type fileOpenChecker interface {
	IsOpenFile(ctx context.Context, dev, ino uint64) (bool, error)
}

// fileOpenSnapshot is the same capability of an OpenSnapshot, which is what
// the pre-copy check of a pass reads (shareOpenChecker).
type fileOpenSnapshot interface {
	IsOpenFile(dev, ino uint64) (bool, error)
}

// isOpenSource reports whether the entry described by info, found beneath a
// held directory, is open. A checker, or a snapshot, that answers by identity
// is asked for the device and inode of info; one that cannot is asked for
// path. A source whose identity cannot be read is an error, never "not open".
func isOpenSource(ctx context.Context, open OpenChecker, info os.FileInfo, path string) (bool, error) {
	if sc, ok := open.(snapshotChecker); ok {
		if snap, ok := sc.snap.(fileOpenSnapshot); ok {
			dev, ino, err := identityOf(info, path)
			if err != nil {
				return false, err
			}
			return snap.IsOpenFile(dev, ino)
		}
	}
	if c, ok := open.(fileOpenChecker); ok {
		dev, ino, err := identityOf(info, path)
		if err != nil {
			return false, err
		}
		return c.IsOpenFile(ctx, dev, ino)
	}
	return open.IsOpen(ctx, path)
}

func identityOf(info os.FileInfo, path string) (dev, ino uint64, err error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("no device and inode for %s", path)
	}
	return uint64(st.Dev), st.Ino, nil
}

// isOpen asks open about the entry itself, by the identity the walk found it
// with, so a directory swapped after the entry was held cannot redirect the
// question to another file.
func (s *sourceEntry) isOpen(ctx context.Context, open OpenChecker) (bool, error) {
	return isOpenSource(ctx, open, s.info, s.path)
}

// sourceStamp is what a copy read of its source: the size and modification
// time of the entry. A copy is of the file as it was then, and the manifest a
// relocation or rebalance syncs carries the same two values (parity.ManifestEntry),
// so a source that differs at the delete was written to or replaced after
// its content was copied and verified.
type sourceStamp struct {
	size  int64
	mtime time.Time
}

func stampOf(info os.FileInfo) sourceStamp {
	return sourceStamp{size: info.Size(), mtime: info.ModTime()}
}

// matches reports whether info is the entry the stamp was taken from: the same
// size and the same modification time. The guarded sync that sits between a
// relocation's copy and its delete runs `snapraid touch` first, which gives a
// file whose modification time is a whole second a non-zero sub-second part
// within that same second, so a recorded time with no sub-second part also
// matches a time in its second. That is the only tolerance: a source of
// another size, or with a modification time in another second or a different
// sub-second part of a recorded one that had any, does not match and is kept.
// A write of the same size in the very second a whole-second time names is not
// told apart from the touch.
func (s sourceStamp) matches(info os.FileInfo) bool {
	if info.Size() != s.size {
		return false
	}
	got := info.ModTime()
	if got.Equal(s.mtime) {
		return true
	}
	return s.mtime.Nanosecond() == 0 && got.Unix() == s.mtime.Unix()
}

// removeSource is the shared last step of every relocation: re-check the
// source rel beneath root for an open handle and, only when it is clear,
// unlink it. entry carries the identifying fields; its Kind, and on success
// its hard-link note, its Result and its Err are set here. The directory
// holding the source is resolved afresh, without following a symbolic link,
// and the entry's metadata, the open-handle check and the unlink are all
// made against that one directory descriptor and the leaf name: a directory
// swapped for a symlink after the copy fails the entry with
// beneath.ErrSymlink and leaves the source in place, where both copies are
// complete and nothing is lost. A source whose size or modification time is
// no longer what copied holds was written to or replaced since, so what was
// copied is not what is about to be deleted: it fails the entry and is kept.
// A symlink source is never "open" — the checker would follow it to its
// target — so it is unlinked without that check.
func removeSource(ctx context.Context, root, rel string, deps Deps, entry Entry, copied sourceStamp) Entry {
	src, err := openSource(root, rel)
	if err != nil {
		entry.Result, entry.Err = ResultFailed, err.Error()
		return entry
	}
	defer src.Close()

	info := src.info
	entry.Kind = entryKind(info.Mode())
	if !copied.matches(info) {
		entry.Result, entry.Err = ResultFailed, fmt.Sprintf("%s changed after it was copied; it was kept", src.base)
		return entry
	}
	if canBeOpen(info.Mode()) {
		open, err := isOpenSource(ctx, deps.Open, info, filepath.Join(root, rel))
		if err != nil {
			entry.Result, entry.Err = ResultMovedPendingDelete, err.Error()
			return entry
		}
		if open {
			entry.Result = ResultMovedPendingDelete
			return entry
		}
	}
	if err := unix.Unlinkat(src.parentfd, src.base, 0); err != nil {
		entry.Result, entry.Err = ResultFailed, fmt.Sprintf("remove %s: %v", src.base, err)
		return entry
	}
	entry.Result = ResultMoved
	if entry.Reason == "" {
		entry.Reason = hardLinkNote(info)
	}
	return entry
}
