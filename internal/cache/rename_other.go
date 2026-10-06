//go:build !linux

package cache

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace falls back to a stat-then-rename within the directory
// dirfd off Linux. Hoserva's production target is Debian; this exists so
// the package still builds.
func renameNoReplace(dirfd int, oldname, newname string) error {
	var st unix.Stat_t
	switch err := unix.Fstatat(dirfd, newname, &st, unix.AT_SYMLINK_NOFOLLOW); err {
	case nil:
		return os.ErrExist
	case unix.ENOENT:
	default:
		return err
	}
	return unix.Renameat(dirfd, oldname, dirfd, newname)
}
