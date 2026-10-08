package cache

import (
	"context"
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

// openSourceParent opens the directory that holds rel beneath root, one
// component at a time with O_NOFOLLOW, and returns it with the leaf's name.
// root is the trusted directory a source lives in (the cache path, a data
// disk); rel is a slash-separated path beneath it. Every directory between
// them that is, or has become, a symbolic link fails the walk with
// beneath.ErrSymlink, so a directory a share user swapped after the tree
// was enumerated can never redirect what is done to the source (#731). The
// caller closes the returned Walker, which owns the parent descriptor.
func openSourceParent(root, rel string) (w *beneath.Walker, parentfd int, base string, err error) {
	comps, err := beneath.Components(filepath.ToSlash(rel))
	if err != nil {
		return nil, -1, "", err
	}
	w, err = beneath.NewWalker(root)
	if err != nil {
		return nil, -1, "", err
	}
	parentfd, err = w.Dir(strings.Join(comps[:len(comps)-1], "/"), nil)
	if err != nil {
		w.Close()
		return nil, -1, "", fmt.Errorf("resolve %s: %w", rel, err)
	}
	return w, parentfd, comps[len(comps)-1], nil
}

// lstatAt is os.Lstat for the entry base in the directory parentfd: the
// directory is the descriptor, not a name that could have been swapped, and
// the entry itself is never followed.
func lstatAt(parentfd int, base string) (os.FileInfo, error) {
	info, err := os.Lstat(fdPath(parentfd) + "/" + base)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return nil, fmt.Errorf("lstat %s: %w", base, err)
	}
	return info, nil
}

// lstatBeneath is os.Lstat for rel beneath root, resolving every directory
// on the way without following a symbolic link (openSourceParent).
func lstatBeneath(root, rel string) (os.FileInfo, error) {
	w, parentfd, base, err := openSourceParent(root, rel)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	return lstatAt(parentfd, base)
}

// fileOpenChecker is the capability of an OpenChecker that can answer for a
// file by its device and inode, so the question is about the file a
// descriptor-relative lookup found and not about whatever a path names by
// the time the checker resolves it. ProcOpenChecker answers this way; a
// checker that can only answer for a path is asked for the joined path
// (isOpenSource).
type fileOpenChecker interface {
	IsOpenFile(ctx context.Context, dev, ino uint64) (bool, error)
}

func isOpenSource(ctx context.Context, open OpenChecker, info os.FileInfo, root, rel string) (bool, error) {
	if c, ok := open.(fileOpenChecker); ok {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return false, fmt.Errorf("no device and inode for %s", rel)
		}
		return c.IsOpenFile(ctx, uint64(st.Dev), uint64(st.Ino))
	}
	return open.IsOpen(ctx, filepath.Join(root, rel))
}

// removeSource is the shared last step of every relocation: re-check the
// source rel beneath root for an open handle and, only when it is clear,
// unlink it. entry carries the identifying fields; its Kind, and on success
// its hard-link note, its Result and its Err are set here. The directory
// holding the source is resolved afresh, without following a symbolic link,
// and the entry's metadata, the open-handle check and the unlink are all
// made against that one directory descriptor and the leaf name: a directory
// swapped for a symlink after the copy fails the entry with
// beneath.ErrSymlink and leaves the source in place, where both copies are
// complete and nothing is lost. A symlink source is never "open" — the
// checker would follow it to its target — so it is unlinked without that
// check.
func removeSource(ctx context.Context, root, rel string, deps Deps, entry Entry) Entry {
	w, parentfd, base, err := openSourceParent(root, rel)
	if err != nil {
		entry.Result, entry.Err = ResultFailed, err.Error()
		return entry
	}
	defer w.Close()

	info, err := lstatAt(parentfd, base)
	if err != nil {
		entry.Result, entry.Err = ResultFailed, err.Error()
		return entry
	}
	entry.Kind = entryKind(info.Mode())
	if canBeOpen(info.Mode()) {
		open, err := isOpenSource(ctx, deps.Open, info, root, rel)
		if err != nil {
			entry.Result, entry.Err = ResultMovedPendingDelete, err.Error()
			return entry
		}
		if open {
			entry.Result = ResultMovedPendingDelete
			return entry
		}
	}
	if err := unix.Unlinkat(parentfd, base, 0); err != nil {
		entry.Result, entry.Err = ResultFailed, fmt.Sprintf("remove %s: %v", base, err)
		return entry
	}
	entry.Result = ResultMoved
	if entry.Reason == "" {
		entry.Reason = hardLinkNote(info)
	}
	return entry
}
