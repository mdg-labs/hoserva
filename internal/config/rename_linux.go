//go:build linux

package config

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace is rename(2) that fails if newpath already exists
// (RENAME_NOREPLACE). Write uses it for the first create of an untracked
// host file so a file that appears after Stat cannot be overwritten.
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
