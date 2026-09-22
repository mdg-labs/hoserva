//go:build linux

package cache

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace is rename(2) that fails if newpath already exists
// (RENAME_NOREPLACE). copyMoveFile uses it for the final rename into the
// array so a file created there during the copy cannot be silently
// discarded (doc 09 §2: a conflict is never auto-resolved).
//
// mergerfs never implements the FUSE rename operation that carries flags
// (confirmed against its upstream source, #243): the kernel's own dentry
// check still catches an existing target and reports EEXIST without ever
// asking mergerfs, but once that check finds nothing, completing the
// rename itself needs flags support mergerfs doesn't have, and the syscall
// fails outright with EINVAL — on every mergerfs version, not a gap a
// mount option or version pin can close. copyMoveFile's tmp file and dst
// are always same-directory siblings, so this EINVAL can only mean "the
// destination filesystem can't carry the flag through", never rename(2)'s
// unrelated "directory into its own subdirectory" EINVAL case.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return os.ErrExist
	}
	if errors.Is(err, unix.EINVAL) {
		return renameNoReplaceFallback(oldpath, newpath)
	}
	return err
}

// renameNoReplaceFallback is link(2)-then-unlink(2): the destination
// filesystem can't carry RENAME_NOREPLACE through rename(2) itself, but
// link(2) has the same create-if-absent atomicity — it claims newpath and
// fails closed with EEXIST if it's already taken, with no window between
// a check and an act. oldpath is only ever removed once the link has
// claimed newpath, so a conflict leaves both oldpath and whatever already
// occupies newpath untouched. This is doc 13 Q28's established pattern for
// an atomic no-clobber publish (a synced temp file linked, never renamed,
// into place).
func renameNoReplaceFallback(oldpath, newpath string) error {
	if err := os.Link(oldpath, newpath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	return os.Remove(oldpath)
}
