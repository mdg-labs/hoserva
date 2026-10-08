package beneath

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// RemoveAll removes the file or directory tree at rel beneath the directory
// root, relative to descriptors the whole way: every directory on the path
// and every directory in the tree is opened with O_NOFOLLOW from the one
// above it and entries are removed with unlinkat, so no name is resolved a
// second time and a component swapped for a symbolic link is an error, never
// a redirect. The entry at rel itself must not be a symbolic link
// (ErrSymlink); symbolic links inside a tree are removed as links and never
// followed. A missing directory on the way to rel, or a missing rel, is an
// error wrapping unix.ENOENT, so the caller can tell removing nothing from
// removing something; an entry that disappears from inside the tree while it
// is being removed is already gone and is skipped.
func RemoveAll(root int, rel string) error {
	comps, err := Components(rel)
	if err != nil {
		return err
	}
	parent := root
	for _, c := range comps[:len(comps)-1] {
		fd, err := Open(parent, c, unix.O_RDONLY|unix.O_DIRECTORY)
		if parent != root {
			_ = unix.Close(parent)
		}
		if err != nil {
			return err
		}
		parent = fd
	}
	if parent != root {
		defer func() { _ = unix.Close(parent) }()
	}
	name := comps[len(comps)-1]
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("%s: %w", rel, ErrSymlink)
	}
	if err := removeEntry(parent, name, &st, nil); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}

// beforeDescend, when set, runs just before removeEntry opens a directory, so
// a test can rearrange the tree at that moment.
var beforeDescend func(name string)

// selection limits a removal to the entries a caller listed. An entry whose
// device and inode listed does not accept is kept, and so is every directory
// that still holds one; a directory listed does not accept is not opened. A
// nil selection removes everything.
type selection struct {
	listed func(dev, ino uint64) bool
	trail  []string
	kept   []string
}

func (s *selection) keeps(st *unix.Stat_t) bool {
	return s != nil && !s.listed(uint64(st.Dev), uint64(st.Ino))
}

func (s *selection) keep(name string) {
	s.kept = append(s.kept, strings.Join(append(append([]string(nil), s.trail...), name), "/"))
}

func removeEntry(parent int, name string, st *unix.Stat_t, sel *selection) error {
	if sel.keeps(st) {
		sel.keep(name)
		return nil
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unlinkat(parent, name, 0)
	}
	if beforeDescend != nil {
		beforeDescend(name)
	}
	fd, err := Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if sel != nil {
		var opened unix.Stat_t
		if err := unix.Fstat(fd, &opened); err != nil {
			return fmt.Errorf("stat %s: %w", name, err)
		}
		if sel.keeps(&opened) {
			sel.keep(name)
			return nil
		}
		sel.trail = append(sel.trail, name)
	}
	before := 0
	if sel != nil {
		before = len(sel.kept)
	}
	err = removeContents(fd, name, sel)
	if sel != nil {
		sel.trail = sel.trail[:len(sel.trail)-1]
	}
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if sel != nil && len(sel.kept) > before {
		return nil
	}
	err = unlinkat(parent, name, unix.AT_REMOVEDIR)
	if sel != nil && errors.Is(err, unix.ENOTEMPTY) {
		sel.keep(name)
		return nil
	}
	return err
}

// removeContents removes everything in the directory fd, or only what sel
// accepts, which is named name for error messages. Listing a directory that
// has been removed is an error wrapping unix.ENOENT.
func removeContents(fd int, name string, sel *selection) error {
	names, err := ReadNames(fd)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	for _, n := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, n, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("stat %s: %w", n, err)
		}
		if err := removeEntry(fd, n, &st, sel); err != nil {
			return fmt.Errorf("%s/%w", name, err)
		}
	}
	return nil
}

// ErrNotExpected is returned by RemoveDirIf for a directory that is not the
// one the caller expected; nothing in it was removed.
var ErrNotExpected = errors.New("is not the expected directory")

// RemoveDirIf removes the directory name in parent, and everything in it, only
// if it is the directory same accepts by device and inode. It opens name once
// with O_NOFOLLOW, asks same about the descriptor it got, and removes the
// contents through that descriptor with the same per-level walk as RemoveAll,
// so a name swapped for another directory after the open cannot redirect it.
// Only the final unlinkat resolves the name again, and it removes an empty
// directory only, so a name that now holds something else fails with ENOTEMPTY
// and loses nothing. A name that is a symbolic link is ErrSymlink, a
// directory that is not the expected one ErrNotExpected and a missing one an
// error wrapping unix.ENOENT.
func RemoveDirIf(parent int, name string, same func(dev, ino uint64) bool) error {
	_, err := removeDirIf(parent, name, same, nil)
	return err
}

// RemoveDirIfListed is RemoveDirIf limited to what the caller listed: it
// removes the directory name in parent only if same accepts it, and inside it
// only the entries whose device and inode listed accepts, with the same
// per-level walk. An entry listed does not accept is left in place and is
// reported in kept as a path starting with name; a directory listed does not
// accept is not opened, and a directory that still holds such an entry is not
// removed. The directory name itself is removed only when nothing was kept.
// Nothing is removed by name alone: an error ends the walk and what it has
// not reached stays in place. The device and inode of a file are read just
// before it is unlinked, so a file that another principal renames over it in
// between is unlinked in its place; directories are removed only while empty.
func RemoveDirIfListed(parent int, name string, same, listed func(dev, ino uint64) bool) (kept []string, err error) {
	return removeDirIf(parent, name, same, &selection{listed: listed, trail: []string{name}})
}

func removeDirIf(parent int, name string, same func(dev, ino uint64) bool, sel *selection) ([]string, error) {
	fd, err := Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat %s: %w", name, err)
	}
	if !same(uint64(st.Dev), uint64(st.Ino)) {
		return nil, fmt.Errorf("%s: %w", name, ErrNotExpected)
	}
	if err := removeContents(fd, name, sel); err != nil {
		return keptIn(sel), fmt.Errorf("remove %s: %w", name, err)
	}
	if sel != nil && len(sel.kept) > 0 {
		return sel.kept, nil
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil {
		if sel != nil && errors.Is(err, unix.ENOTEMPTY) {
			return []string{name}, nil
		}
		return keptIn(sel), fmt.Errorf("remove %s: %w", name, err)
	}
	return nil, nil
}

func keptIn(sel *selection) []string {
	if sel == nil {
		return nil
	}
	return sel.kept
}

func unlinkat(parent int, name string, flags int) error {
	if err := unix.Unlinkat(parent, name, flags); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// ReadNames lists the names in the directory dirfd and leaves dirfd open for
// the caller. The listing shares the descriptor's offset, so a descriptor
// is listed once.
func ReadNames(dirfd int) ([]string, error) {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(dup)
	f := os.NewFile(uintptr(dup), "dir")
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}
