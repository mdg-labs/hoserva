package share

import (
	"errors"
	"os"
	"path/filepath"
)

// FS is the filesystem share operations touch: branch directories, the
// browse listing, and delete-data. Tests inject a temp-dir backed
// implementation; production uses OSFS. Nothing here walks a data disk
// on a timer.
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
	Chmod(path string, mode os.FileMode) error
	Chown(path string, uid, gid int) error
	RemoveAll(path string) error
	ReadDir(path string) ([]os.DirEntry, error)
	Lstat(path string) (os.FileInfo, error)
	EvalSymlinks(path string) (string, error)
	GetXattr(path, attr string) ([]byte, error)
}

// OSFS is FS against the real operating system.
type OSFS struct{}

func (OSFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (OSFS) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

// Chown sets path's group to the shared data group (Q26). hoservad
// itself always runs as root (doc 01 §7) and can chown to any group;
// an unprivileged caller cannot chgrp to a group it does not belong to,
// which os.Chown reports as a permission error — swallowed here rather
// than failing share creation over it, since it can only happen outside
// hoservad's own supported deployment. Any other error (a missing path,
// a read-only filesystem) still surfaces.
func (OSFS) Chown(path string, uid, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil
		}
		return err
	}
	return nil
}

func (OSFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (OSFS) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (OSFS) Lstat(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func (OSFS) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func (OSFS) GetXattr(path, attr string) ([]byte, error) {
	return lgetxattr(path, attr)
}

// MergerFSBasepath is the mergerfs xattr browse reads for the holding
// disk (doc 03 §4.2).
const MergerFSBasepath = "user.mergerfs.basepath"
