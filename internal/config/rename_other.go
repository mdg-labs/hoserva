//go:build !linux

package config

import "os"

// renameNoReplace falls back to Stat-then-Rename off Linux. Hoserva's
// production target is Debian; this exists so the package still builds.
func renameNoReplace(oldpath, newpath string) error {
	if _, err := os.Stat(newpath); err == nil {
		return os.ErrExist
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(oldpath, newpath)
}
