package beneath

import (
	"errors"
	"fmt"
	"os"

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
	if err := removeEntry(parent, name, st.Mode); err != nil {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}

// beforeDescend, when set, runs just before removeEntry opens a directory, so
// a test can rearrange the tree at that moment.
var beforeDescend func(name string)

func removeEntry(parent int, name string, mode uint32) error {
	if mode&unix.S_IFMT != unix.S_IFDIR {
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
	names, err := ReadNames(fd)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
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
		if err := removeEntry(fd, n, st.Mode); err != nil {
			return fmt.Errorf("%s/%w", name, err)
		}
	}
	return unlinkat(parent, name, unix.AT_REMOVEDIR)
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
