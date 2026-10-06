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
// and gives each the owner, group and mode of the directory at the same
// depth above srcDir, the way copyRegular and copyNode give their file
// the source's (doc 09 §2). srcDir and dstDir are the parents of one
// copied entry, so they end in the same relative path and their
// ancestors correspond one for one. dstRoot is the trusted directory the
// copy writes into (the share's array-only mount for the mover, a data
// disk's mount for a rebalance or evacuation, the cache mount for a
// relocation to the cache) and dstDir lies beneath it.
//
// The chain from dstRoot is walked one directory descriptor at a time with
// O_NOFOLLOW, and every directory is created, given its owner and mode and
// renamed relative to its parent's descriptor, so a component that is or
// becomes a symlink, at any depth, fails the copy with beneath.ErrSymlink
// instead of creating a directory somewhere else as the share's user.
// A directory that already exists on the target is never touched: it may
// hold other files, and its metadata is not this copy's to change.
func mkdirAllLike(srcDir, dstRoot, dstDir string, deps Deps) error {
	rel, err := filepath.Rel(dstRoot, dstDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%s is not beneath %s", dstDir, dstRoot)
	}
	if rel == "." {
		return nil
	}
	comps, err := beneath.Components(rel)
	if err != nil {
		return err
	}
	srcDirs := make([]string, len(comps))
	for i, d := len(comps)-1, srcDir; i >= 0; i, d = i-1, filepath.Dir(d) {
		srcDirs[i] = d
	}

	parent, err := beneath.OpenRoot(dstRoot)
	if err != nil {
		return err
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
			return fmt.Errorf("create %s: %w", filepath.Join(dstRoot, filepath.Join(comps[:i+1]...)), err)
		}
		parent = fd
	}
	_ = unix.Close(parent)
	return nil
}

// mkdirLikeAt creates the directory name in the directory parentfd with
// srcDir's owner, group and mode. The directory is first made closed (0700)
// under a "<name>.hoserva-moving-<uuid>" name, opened with O_NOFOLLOW, given
// its owner and mode through that descriptor (so what is changed is the
// directory that was created), and only then renamed to its real name,
// all relative to parentfd and never by path. A directory with the real
// name therefore always has the source's owner and mode: a failure leaves
// nothing behind, and a crash at worst an empty temp-named directory.
// Where such a directory is swept is the stray-temp sweep's (doc 09 §2).
func mkdirLikeAt(parentfd int, name, srcDir string, deps Deps) error {
	srcInfo, err := os.Lstat(srcDir)
	if err != nil {
		return fmt.Errorf("inspect source directory: %w", err)
	}
	st, ok := srcInfo.Sys().(*syscall.Stat_t)
	if !ok || !srcInfo.IsDir() {
		return fmt.Errorf("source %s is not a directory with unix metadata", srcDir)
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
