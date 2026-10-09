package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// dirPreservedBits is the part of a directory's mode a copy carries over:
// the permission bits plus setuid, setgid and sticky. A share directory is
// setgid (Q26), so dropping that bit would change the group every file
// created in it afterwards gets.
const dirPreservedBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// mkdirAllLike creates every directory of dstDir that does not exist yet
// and gives each the owner, group and mode of the source directory at the
// same depth, the way copyRegular and copyNode give their file the source's
// (doc 09 §2). srcDirs is the metadata of the source's directories, outermost
// first, read from the descriptors the source was opened through
// (sourceEntry.dirs), never by path; dstDir is the target's parent of the
// same entry, so it ends in the same relative path and the two correspond one
// for one. dstRoot is the trusted directory the
// copy writes into (the share's array-only mount for the mover, a data
// disk's mount for a rebalance or evacuation, the cache mount for a
// relocation to the cache) and dstDir lies beneath it.
//
// The chain from dstRoot is walked one directory descriptor at a time with
// O_NOFOLLOW, and every directory is created, given its owner and mode and
// renamed relative to its parent's descriptor, so a component that is or
// becomes a symlink, at any depth, fails the copy with beneath.ErrSymlink
// instead of creating a directory somewhere else as the share's user. On
// the mover's mergerfs mount, where mergerfs makes each of these calls by
// path on its branch, the branch's nosymfollow bind is what refuses a
// symlink the walk has not seen (copyEntry).
// A directory that already exists on the target is never touched: it may
// hold other files, and its metadata is not this copy's to change.
//
// It returns the descriptor of dstDir itself, which the caller closes and
// creates the entry's temp file in, so the entry lands in the directory
// that was walked and not in whatever the path names a moment later.
func mkdirAllLike(srcDirs []os.FileInfo, dstRoot, dstDir string, deps Deps) (int, error) {
	rel, err := relBeneath(dstRoot, dstDir)
	if err != nil {
		return -1, err
	}
	if rel == "" {
		return beneath.OpenRoot(dstRoot)
	}
	comps, err := beneath.Components(rel)
	if err != nil {
		return -1, err
	}
	if len(srcDirs) != len(comps) {
		return -1, fmt.Errorf("%s has %d directories beneath %s, the source %d", dstDir, len(comps), dstRoot, len(srcDirs))
	}

	parent, err := beneath.OpenRoot(dstRoot)
	if err != nil {
		return -1, err
	}
	for i, name := range comps {
		fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
		if errors.Is(err, fs.ErrNotExist) {
			if err = mkdirLikeAt(parent, name, srcDirs[i], deps); err == nil {
				fd, err = beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
			}
		}
		_ = unix.Close(parent)
		if err != nil {
			return -1, fmt.Errorf("create %s: %w", filepath.Join(dstRoot, filepath.Join(comps[:i+1]...)), err)
		}
		parent = fd
	}
	return parent, nil
}

// relBeneath is dir's path relative to root, "" for root itself, refusing a
// dir that is not beneath it.
func relBeneath(root, dir string) (string, error) {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is not beneath %s", dir, root)
	}
	if rel == "." {
		return "", nil
	}
	return rel, nil
}

// requireNoSymlinks walks dstDir again from dstRoot and fails when any
// component is now a symlink or gone, so a copy whose directory was swapped
// for a symlink while it ran is not published, and its source deleted, as
// if it were at its path. It is not what keeps the copy on the target:
// that is the held directory descriptor on a plain filesystem, and the
// nosymfollow branch binds on the mover's mergerfs mount (copyEntry),
// where a swap made on a data disk behind mergerfs can stay hidden from
// this walk for as long as the kernel's FUSE entry cache keeps the old
// lookup.
func requireNoSymlinks(dstRoot, dstDir string) error {
	rel, err := relBeneath(dstRoot, dstDir)
	if err != nil {
		return err
	}
	w, err := beneath.NewWalker(dstRoot)
	if err != nil {
		return err
	}
	defer w.Close()
	if _, err := w.Dir(rel, nil); err != nil {
		return fmt.Errorf("target directory changed during the copy: %w", err)
	}
	return nil
}

// mkdirLikeAt creates the directory name in the directory parentfd with
// srcInfo's owner, group and mode. The directory is first made closed (0700)
// under a "<name>.hoserva-moving-<uuid>" name, opened with O_NOFOLLOW, given
// its owner and mode through that descriptor (so what is changed is the
// directory that was created), and only then renamed to its real name,
// all relative to parentfd and never by path. A directory with the real
// name therefore always has the source's owner and mode: a failure leaves
// nothing behind, and a crash at worst an empty temp-named directory.
// Where such a directory is swept is the stray-temp sweep's (doc 09 §2).
func mkdirLikeAt(parentfd int, name string, srcInfo os.FileInfo, deps Deps) error {
	st, ok := srcInfo.Sys().(*syscall.Stat_t)
	if !ok || !srcInfo.IsDir() {
		return fmt.Errorf("the source directory of %s is not a directory with unix metadata", name)
	}
	tmp := name + tempSuffix + deps.UUID()
	if err := unix.Mkdirat(parentfd, tmp, 0o700); err != nil {
		return &fs.PathError{Op: "mkdir", Path: tmp, Err: err}
	}
	if err := openDirLikeAt(parentfd, tmp, st, srcInfo.Mode()&dirPreservedBits); err != nil {
		_ = unix.Unlinkat(parentfd, tmp, unix.AT_REMOVEDIR)
		return err
	}
	if err := unix.Renameat(parentfd, tmp, parentfd, name); err != nil {
		_ = unix.Unlinkat(parentfd, tmp, unix.AT_REMOVEDIR)
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("rename %s into place: %w", name, err)
	}
	return nil
}

// openDirLikeAt gives the directory just created as tmp in parentfd the
// owner, group and mode of src through a descriptor, which is checked to be
// a directory the daemon's user owns before anything is changed.
func openDirLikeAt(parentfd int, tmp string, src *syscall.Stat_t, mode fs.FileMode) error {
	fd, err := beneath.Open(parentfd, tmp, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open new directory %s: %w", tmp, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var got unix.Stat_t
	if err := unix.Fstat(fd, &got); err != nil {
		return fmt.Errorf("inspect new directory %s: %w", tmp, err)
	}
	if got.Mode&unix.S_IFMT != unix.S_IFDIR || got.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("%s is not the directory just created", tmp)
	}
	if err := unix.Fchown(fd, int(src.Uid), int(src.Gid)); err != nil {
		return fmt.Errorf("preserve ownership of %s: %w", tmp, err)
	}
	if err := unix.Fchmod(fd, goModeToUnix(mode)); err != nil {
		return fmt.Errorf("preserve mode of %s: %w", tmp, err)
	}
	return nil
}

func goModeToUnix(m fs.FileMode) uint32 {
	mode := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		mode |= unix.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		mode |= unix.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		mode |= unix.S_ISVTX
	}
	return mode
}
