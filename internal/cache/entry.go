package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// entryKind names a non-regular entry's type for Entry.Kind; a regular
// file is "". An entry of a type this package does not know is "other".
func entryKind(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return ""
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode&fs.ModeNamedPipe != 0:
		return "fifo"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0 && mode&fs.ModeCharDevice != 0:
		return "char_device"
	case mode&fs.ModeDevice != 0:
		return "block_device"
	default:
		return "other"
	}
}

// canBeOpen reports whether a process can hold an entry of this type
// open. A symlink cannot: the open checker stats through it, so asking
// about one would report its target's handles instead.
func canBeOpen(mode fs.FileMode) bool { return mode&fs.ModeSymlink == 0 }

// hardLinkNote describes the split a hard-linked regular file undergoes:
// two names for one inode on the source become independent files on the
// target (doc 09 §2).
func hardLinkNote(info os.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink < 2 {
		return ""
	}
	return fmt.Sprintf("the source had %d hard links; the target holds an independent copy of this name", st.Nlink)
}

// enumerateEntries returns every entry under root that is not a
// directory — regular files, symlinks (never followed), FIFOs, device
// nodes and sockets — relative to root, sorted so a Checkpoint's LastPath
// can resume deterministically. In-flight temp entries are never
// candidates. A root that does not exist enumerates as empty.
func enumerateEntries(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rels []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.Contains(d.Name(), tempSuffix) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(rels)
	return rels, nil
}

// zeroReader yields endless zero bytes: what a hole reads as.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// copySparse copies in's size bytes onto out at the same offsets. With
// holes set, it writes only the data extents (SEEK_DATA/SEEK_HOLE) so that
// holes in the source are holes in the target; without it, the file is
// copied densely, which is what keeps a preallocated file (fallocate)
// allocated — its unwritten extents read as holes, but the source holds
// the space. hash, when non-nil, sees the file's whole logical content:
// the data, and zeros for every hole. On a filesystem without hole
// reporting the kernel answers "all data" and this is a plain copy.
func copySparse(out, in *os.File, size int64, hash io.Writer, holes bool) error {
	if !holes {
		var w io.Writer = out
		if hash != nil {
			w = io.MultiWriter(out, hash)
		}
		if n, err := io.CopyN(w, in, size); err != nil {
			return fmt.Errorf("copy: wrote %d of %d bytes: %w", n, size, err)
		}
		return nil
	}
	if err := out.Truncate(size); err != nil {
		return fmt.Errorf("size target: %w", err)
	}
	var off int64
	for off < size {
		data, err := in.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			data = size
		} else if err != nil {
			return fmt.Errorf("find data: %w", err)
		}
		if data > size {
			data = size
		}
		if hash != nil && data > off {
			if _, err := io.CopyN(hash, zeroReader{}, data-off); err != nil {
				return fmt.Errorf("hash hole: %w", err)
			}
		}
		if data >= size {
			return nil
		}
		hole, err := in.Seek(data, unix.SEEK_HOLE)
		if err != nil {
			return fmt.Errorf("find hole: %w", err)
		}
		end := min(hole, size)
		if _, err := in.Seek(data, io.SeekStart); err != nil {
			return fmt.Errorf("seek source: %w", err)
		}
		if _, err := out.Seek(data, io.SeekStart); err != nil {
			return fmt.Errorf("seek target: %w", err)
		}
		var w io.Writer = out
		if hash != nil {
			w = io.MultiWriter(out, hash)
		}
		if n, err := io.CopyN(w, in, end-data); err != nil {
			return fmt.Errorf("copy: wrote %d of %d bytes of an extent: %w", n, end-data, err)
		}
		off = end
	}
	return nil
}

// copyNode recreates a symlink, FIFO or device node at dst, through a
// temp-suffixed sibling that is verified and then renamed into place the
// same way a regular file's copy is (doc 09 §2). It never follows a
// symlink and never touches src.
func copyNode(src, dst, dstRoot string, srcInfo os.FileInfo, deps Deps) error {
	if err := mkdirAllLike(filepath.Dir(src), dstRoot, filepath.Dir(dst), deps); err != nil {
		return fmt.Errorf("create target directory: %w", err)
	}
	tmp := dst + tempSuffix + deps.UUID()
	st, ok := srcInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("source has no unix metadata")
	}
	mode, kind := srcInfo.Mode(), entryKind(srcInfo.Mode())
	perm := uint32(mode.Perm())

	var linkTarget string
	switch kind {
	case "symlink":
		var err error
		if linkTarget, err = os.Readlink(src); err != nil {
			return fmt.Errorf("read symlink: %w", err)
		}
		if err := os.Symlink(linkTarget, tmp); err != nil {
			return fmt.Errorf("create symlink: %w", err)
		}
	case "fifo":
		if err := deps.Mknod(tmp, unix.S_IFIFO|perm, 0); err != nil {
			return fmt.Errorf("create fifo: %w", err)
		}
	case "char_device", "block_device":
		typ := uint32(unix.S_IFBLK)
		if kind == "char_device" {
			typ = unix.S_IFCHR
		}
		if err := deps.Mknod(tmp, typ|perm, int(st.Rdev)); err != nil {
			return fmt.Errorf("create device node: %w", err)
		}
	default:
		return fmt.Errorf("cannot recreate a %s", kind)
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmp)
		}
	}()

	if err := os.Lchown(tmp, int(st.Uid), int(st.Gid)); err != nil {
		return fmt.Errorf("preserve ownership: %w", err)
	}
	if kind != "symlink" {
		if err := os.Chmod(tmp, mode.Perm()); err != nil {
			return fmt.Errorf("preserve mode: %w", err)
		}
		if err := copyXattrs(src, tmp); err != nil {
			return err
		}
	}
	mt := unix.NsecToTimespec(srcInfo.ModTime().UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, tmp, []unix.Timespec{mt, mt}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("preserve timestamps: %w", err)
	}

	if err := verifyNode(tmp, srcInfo, linkTarget); err != nil {
		return err
	}

	renamed, err := publishTemp(tmp, dst, deps)
	if renamed {
		published = true
	}
	return err
}

// verifyNode re-reads tmp from disk and compares it with the source:
// type, link target, device number, owner, permissions and mtime.
func verifyNode(tmp string, srcInfo os.FileInfo, linkTarget string) error {
	got, err := os.Lstat(tmp)
	if err != nil {
		return fmt.Errorf("verify: stat target: %w", err)
	}
	if got.Mode().Type() != srcInfo.Mode().Type() {
		return fmt.Errorf("verify: target is a %s, source is a %s", entryKind(got.Mode()), entryKind(srcInfo.Mode()))
	}
	want, gotSt := srcInfo.Sys().(*syscall.Stat_t), got.Sys().(*syscall.Stat_t)
	if gotSt.Uid != want.Uid || gotSt.Gid != want.Gid {
		return fmt.Errorf("verify: target owner %d:%d, source %d:%d", gotSt.Uid, gotSt.Gid, want.Uid, want.Gid)
	}
	if !got.ModTime().Equal(srcInfo.ModTime()) {
		return fmt.Errorf("verify: target mtime %v, source %v", got.ModTime(), srcInfo.ModTime())
	}
	if srcInfo.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(tmp)
		if err != nil {
			return fmt.Errorf("verify: read target symlink: %w", err)
		}
		if target != linkTarget {
			return fmt.Errorf("verify: target links to %q, source to %q", target, linkTarget)
		}
		return nil
	}
	if got.Mode().Perm() != srcInfo.Mode().Perm() {
		return fmt.Errorf("verify: target mode %v, source %v", got.Mode().Perm(), srcInfo.Mode().Perm())
	}
	if srcInfo.Mode()&fs.ModeDevice != 0 && gotSt.Rdev != want.Rdev {
		return fmt.Errorf("verify: target device %d, source %d", gotSt.Rdev, want.Rdev)
	}
	return nil
}

// sameNode reports whether dst is an earlier run's published copy of the
// non-regular src: same type, same link target or device number. Size and
// mtime are compared by the caller (isSamePendingCopy), as for a file.
func sameNode(src, dst string, srcInfo, dstInfo os.FileInfo) (bool, error) {
	if srcInfo.Mode().Type() != dstInfo.Mode().Type() {
		return false, nil
	}
	switch {
	case srcInfo.Mode()&fs.ModeSymlink != 0:
		a, err := os.Readlink(src)
		if err != nil {
			return false, fmt.Errorf("read source symlink: %w", err)
		}
		b, err := os.Readlink(dst)
		if err != nil {
			return false, fmt.Errorf("read target symlink: %w", err)
		}
		return a == b, nil
	case srcInfo.Mode()&fs.ModeDevice != 0:
		s, sok := srcInfo.Sys().(*syscall.Stat_t)
		d, dok := dstInfo.Sys().(*syscall.Stat_t)
		return sok && dok && s.Rdev == d.Rdev, nil
	}
	return true, nil
}

// hasHoles reports whether the file's allocation is smaller than its
// size, the one case in which copying only its data extents changes
// anything about the target's allocation in the source's favour.
func hasHoles(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Blocks*512 < info.Size()
}
