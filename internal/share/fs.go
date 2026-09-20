package share

import (
	"os"
	"path/filepath"
)

// FS is the filesystem share operations touch: branch directories, the
// browse listing, and delete-data. Tests inject a temp-dir backed
// implementation; production uses OSFS. Nothing here walks a data disk
// on a timer.
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
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
