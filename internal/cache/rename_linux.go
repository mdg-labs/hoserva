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
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EEXIST) {
		return os.ErrExist
	}
	return err
}
