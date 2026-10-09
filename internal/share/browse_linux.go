//go:build linux

package share

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"golang.org/x/sys/unix"
)

// listConfined lists the directory rel names beneath root through a
// descriptor, never through a pathname walk that a concurrent writer could
// redirect (CWE-367). root is opened with O_NOFOLLOW, so a share root that is
// itself a symlink is refused. rel is opened beneath it with openat2's
// RESOLVE_IN_ROOT|RESOLVE_NO_SYMLINKS: a symlink anywhere along rel, such as
// one swapped in after the caller validated the path, fails the open with
// ELOOP instead of being followed out of the share. Each entry's type and
// size come from fstatat on that descriptor without following a link, and its
// mergerfs basepath from an lgetxattr through /proc/self/fd/<dirfd>, which
// resolves the directory from the descriptor and the entry as the last,
// unfollowed component.
func listConfined(root, rel string) ([]BrowseEntry, error) {
	rootFd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%w: share root %s is not a directory of its own", ErrPathEscapes, root)
		}
		return nil, fmt.Errorf("share: opening share root %s: %w", root, err)
	}
	defer func() { _ = unix.Close(rootFd) }()

	dirFd := rootFd
	if rel != "" && rel != "." {
		dirFd, err = unix.Openat2(rootFd, rel, &unix.OpenHow{
			Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return nil, fmt.Errorf("%w: %q", ErrPathEscapes, rel)
			}
			return nil, fmt.Errorf("share: opening %s: %w", rel, err)
		}
		defer func() { _ = unix.Close(dirFd) }()
	}

	names, err := direntNames(dirFd)
	if err != nil {
		return nil, fmt.Errorf("share: reading %s: %w", rel, err)
	}
	sort.Strings(names)

	dirProc := "/proc/self/fd/" + strconv.Itoa(dirFd) + "/"
	out := make([]BrowseEntry, 0, len(names))
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		item := BrowseEntry{Name: name, Directory: st.Mode&unix.S_IFMT == unix.S_IFDIR}
		if !item.Directory {
			item.SizeBytes = st.Size
		}
		if x, err := lgetxattr(dirProc+name, MergerFSBasepath); err == nil {
			item.Disk = string(x)
		}
		out = append(out, item)
	}
	return out, nil
}

func direntNames(fd int) ([]string, error) {
	var names []string
	buf := make([]byte, 16*1024)
	for {
		n, err := unix.ReadDirent(fd, buf)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return names, nil
		}
		_, _, names = unix.ParseDirent(buf[:n], -1, names)
	}
}
