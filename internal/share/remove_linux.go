//go:build linux

package share

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// unlinkConfined removes base from the directory dirRel (relative to
// resolvedRoot, or "." for resolvedRoot itself). dirRel is reopened with
// openat2's RESOLVE_IN_ROOT, which confines resolution to resolvedRoot as
// though it were the root directory (available since Linux 5.6, well
// within Debian stable per D2), together with RESOLVE_NO_SYMLINKS, which
// fails the whole resolution with ELOOP if any component of dirRel —
// immediate parent, grandparent, or any deeper ancestor — is currently a
// symlink. confineSharePathOnFS's EvalSymlinks already made dirRel
// symlink-free at validation time, so the only way a component of dirRel
// can be a symlink at this reopen is a swap that happened after
// validation and before this call; RESOLVE_NO_SYMLINKS then refuses the
// reopen outright rather than silently following the swap to some other
// directory that happens to still be under resolvedRoot.
//
// That guarantees the directory this reopen lands in is symlink-free and
// was reachable, by name, from resolvedRoot without following a symlink —
// it does not by itself guarantee that directory is the exact object
// validation saw (a same-named, non-symlink directory swap is not
// detected), and it says nothing about base. expectedLeaf is the FileInfo
// confineSharePathOnFS's own Lstat produced for base at validation time,
// before the race window opened; after the reopen succeeds, base is
// stat'd by fd-relative Fstatat (so this second stat cannot itself be
// redirected by a symlink swapped into dirRel) and refused unless its
// device and inode match expectedLeaf. This is a comparison against a
// value captured before the race window, not a second independent
// re-resolution of the same pathname, so a leaf swapped for a different
// object of the same name — in an otherwise untouched, legitimate parent
// directory — is caught too (CWE-367).
func unlinkConfined(resolvedRoot, dirRel, base string, expectedLeaf os.FileInfo) error {
	rootFd, err := unix.Open(resolvedRoot, unix.O_DIRECTORY|unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("share: opening share root %s: %w", resolvedRoot, err)
	}
	defer func() { _ = unix.Close(rootFd) }()

	dirFd := rootFd
	if dirRel != "." {
		dirFd, err = unix.Openat2(rootFd, dirRel, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("%w: %s", ErrFileNotFound, base)
			}
			if errors.Is(err, unix.ELOOP) {
				return fmt.Errorf("%w: %s", ErrPathEscapes, base)
			}
			return fmt.Errorf("share: resolving %s: %w", dirRel, err)
		}
		defer func() { _ = unix.Close(dirFd) }()
	}

	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%w: %s", ErrFileNotFound, base)
		}
		return fmt.Errorf("share: statting %s: %w", base, err)
	}
	if expectedLeaf == nil {
		return fmt.Errorf("%w: %s", ErrFileNotFound, base)
	}
	expectedSt, ok := expectedLeaf.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("share: no captured identity for %s", base)
	}
	if st.Dev != expectedSt.Dev || st.Ino != expectedSt.Ino {
		return fmt.Errorf("%w: %s", ErrPathEscapes, base)
	}

	var flags int
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(dirFd, base, flags); err != nil {
		return fmt.Errorf("share: deleting %s: %w", base, err)
	}
	return nil
}
