package parity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// SnapRAID keeps no ownership or mode, and `fix` creates what it restores
// as the daemon's user: a file with mode 0600 (open(..., O_CREAT, 0600) in
// SnapRAID's cmdline/handle.c) in directories it creates with mode 0755
// (mkdir in mkancestor, cmdline/support.c), with no chown or chmod
// anywhere in a restore. After a fix Hoserva gives each file it restored
// the owner, group and mode a sync recorded for it (doc 02 §2, #626).
const (
	snapraidFileMode = 0o600
	snapraidDirMode  = 0o755

	// Fallback when a restored path was never recorded: a share's files
	// and directories as Samba creates them for it (Q26).
	defaultFileMode = 0o664
	defaultDirMode  = 0o2775

	metaModeMask = 0o7777

	// maxNamedRestoreErrors bounds how many failures one fix names: a
	// whole-disk fix can restore millions of files.
	maxNamedRestoreErrors = 10
)

func (e *SnapraidEngine) fileMetaStore() *FileMetaStore {
	if e.Usage == nil {
		return nil
	}
	return e.Usage.FileMeta()
}

// recordFileMetadata replaces the recorded owner, group and mode of every
// tracked file and the directories above them with what is on the data
// disks now. Sync calls it once after a sync that finished, never on a
// timer and never from a request: the sync's own scan has just walked every
// file, so this stats what that scan left in the kernel's cache. A sync
// that found nothing to do keeps what is recorded unless nothing is, so a
// nightly sync with no changes writes nothing. A nil Usage leaves it
// unwired, like ComputeShareUsage.
func (e *SnapraidEngine) recordFileMetadata(ctx context.Context, nothingToSync bool) error {
	store := e.fileMetaStore()
	if store == nil {
		return nil
	}
	if nothingToSync {
		recorded, err := store.Recorded(ctx)
		if err != nil {
			return err
		}
		if recorded {
			return nil
		}
	}
	list, err := e.List(ctx)
	if err != nil {
		return fmt.Errorf("parity: listing tracked files to record their ownership: %w", err)
	}
	return store.Replace(ctx, func(emit func(FileMeta) error) error {
		return walkTrackedMetadata(ctx, list, emit)
	})
}

// walkTrackedMetadata emits the current metadata of every file list tracks
// and of each directory above one, once. Paths are resolved beneath the
// disk's mount without following a symlink (internal/beneath), so a
// directory a share user swapped for a link cannot record some other file's
// owner and mode under a tracked path. A file that has gone since the sync,
// or whose path is now a symlink or no longer a regular file, is left out;
// any other failure to read one aborts, since a record that silently lacks
// files would give them the fallback after a fix. It runs outside any
// database transaction.
func walkTrackedMetadata(ctx context.Context, list ListReport, emit func(FileMeta) error) error {
	seenDirs := map[string]struct{}{}
	walkers := map[string]*beneath.Walker{}
	defer func() {
		for _, w := range walkers {
			w.Close()
		}
	}()
	for i, f := range list.Files {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		mount, ok := list.DataMounts[f.Disk]
		if !ok {
			continue
		}
		disk := filepath.Clean(mount)
		w := walkers[disk]
		if w == nil {
			var err error
			if w, err = beneath.NewWalker(disk); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return fmt.Errorf("parity: reading the ownership of files on %s: %w", disk, err)
			}
			walkers[disk] = w
		}
		dir := path.Dir(f.RelPath)
		if dir == "." {
			dir = ""
		}
		parent, err := w.Dir(dir, func(_, fd int, rel string) error {
			key := disk + "\x00" + rel
			if _, seen := seenDirs[key]; seen {
				return nil
			}
			seenDirs[key] = struct{}{}
			var st unix.Stat_t
			if err := unix.Fstat(fd, &st); err != nil {
				return fmt.Errorf("parity: reading the ownership of %s: %w", filepath.Join(disk, rel), err)
			}
			return emit(metaFrom(disk, rel, true, st))
		})
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, beneath.ErrSymlink) {
			continue
		}
		if err != nil {
			return fmt.Errorf("parity: reading the ownership of %s: %w", filepath.Join(disk, f.RelPath), err)
		}
		var st unix.Stat_t
		err = unix.Fstatat(parent, path.Base(f.RelPath), &st, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && st.Mode&unix.S_IFMT != unix.S_IFREG) {
			continue
		}
		if err != nil {
			return fmt.Errorf("parity: reading the ownership of %s: %w", filepath.Join(disk, f.RelPath), err)
		}
		if err := emit(metaFrom(disk, f.RelPath, false, st)); err != nil {
			return err
		}
	}
	return nil
}

func metaFrom(disk, rel string, dir bool, st unix.Stat_t) FileMeta {
	return FileMeta{Disk: disk, RelPath: rel, Dir: dir, UID: st.Uid, GID: st.Gid, Mode: st.Mode & metaModeMask}
}

// birthSlack is how far before a fix's start a creation time may fall and
// still count as made by it, covering the filesystem's timestamp granularity.
const birthSlack = time.Second

// errNoBirthTime means the filesystem did not report when an inode was
// created, so a restore cannot tell what SnapRAID made from what was there.
var errNoBirthTime = errors.New("the filesystem reports no creation time, so what SnapRAID created cannot be told from what was already there")

// restoreFixedOwnership gives every file s says fix recovered, and each
// directory above it that SnapRAID created for it, its recorded owner,
// group and mode, or the share defaults for a path nothing was recorded
// for. It touches only what the fix created: an inode whose creation time
// is no earlier than started (less birthSlack), owned by the daemon's user
// with the mode SnapRAID creates with. A file fix rewrote in place and a
// directory that already existed keep the owner and mode they had, as does
// anything created in the second before the fix started. Every path is
// reached by descending from the data disk's mount one directory
// descriptor at a time without following a symlink, and owner and mode are
// set through the descriptor, so a path a share user swaps for a link
// during the restore is reported as an error, never followed. It carries
// on past one path's failure and reports every failure together, so one
// unreadable file does not leave the rest of a whole-disk restore owned by
// root.
func restoreFixedOwnership(ctx context.Context, store *FileMetaStore, s RunSummary, started time.Time) error {
	r := &ownershipRestorer{store: store, euid: uint32(os.Geteuid()), since: started.Add(-birthSlack), walkers: map[string]*beneath.Walker{}}
	defer r.close()
	var errs []error
	for _, rec := range s.RecoveredFiles {
		id, rel, ok := strings.Cut(rec, ":")
		mount, known := s.DataMounts[id]
		if !ok || !known || rel == "" {
			errs = append(errs, fmt.Errorf("recovered file %q names no known data disk", rec))
			continue
		}
		if err := r.restore(ctx, filepath.Clean(mount), rel); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	named := errs[:min(len(errs), maxNamedRestoreErrors)]
	msg := make([]string, len(named))
	for i, err := range named {
		msg[i] = err.Error()
	}
	more := ""
	if extra := len(errs) - len(named); extra > 0 {
		more = fmt.Sprintf("; and %d more", extra)
	}
	return fmt.Errorf("could not restore the owner and mode of %d recovered file(s): %s%s", len(errs), strings.Join(msg, "; "), more)
}

type ownershipRestorer struct {
	store   *FileMetaStore
	euid    uint32
	since   time.Time
	walkers map[string]*beneath.Walker
}

func (r *ownershipRestorer) close() {
	for _, w := range r.walkers {
		w.Close()
	}
}

func (r *ownershipRestorer) restore(ctx context.Context, mount, rel string) error {
	w := r.walkers[mount]
	if w == nil {
		var err error
		if w, err = beneath.NewWalker(mount); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		r.walkers[mount] = w
	}
	p := filepath.Join(mount, filepath.FromSlash(rel))
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	parent, err := w.Dir(dir, func(parentFD, fd int, dirRel string) error {
		depth := strings.Count(dirRel, "/") + 1
		return r.restoreEntry(ctx, mount, dirRel, true, depth, parentFD, fd)
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s: %w", p, err)
	}
	fd, err := beneath.Open(parent, path.Base(rel), unix.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("%s: %w", p, err)
	}
	defer func() { _ = unix.Close(fd) }()
	return r.restoreEntry(ctx, mount, rel, false, strings.Count(rel, "/")+1, parent, fd)
}

// restoreEntry works on the open descriptor fd of rel, whose directory is
// parentFD. depth is the number of path components rel has: the share
// defaults apply only below a share's own top-level directory, which
// Hoserva's share service owns.
func (r *ownershipRestorer) restoreEntry(ctx context.Context, mount, rel string, dir bool, depth, parentFD, fd int) error {
	p := filepath.Join(mount, filepath.FromSlash(rel))
	wantType, createdMode := uint32(unix.S_IFREG), uint32(snapraidFileMode)
	if dir {
		wantType, createdMode = unix.S_IFDIR, snapraidDirMode
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", p, err)
	}
	if st.Mode&unix.S_IFMT != wantType || st.Uid != r.euid || st.Mode&0o777 != createdMode {
		return nil
	}
	created, err := r.createdByThisFix(fd)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if !created {
		return nil
	}
	want, ok, err := r.target(ctx, mount, rel, dir, depth, parentFD)
	if err != nil || !ok {
		return err
	}
	if st.Uid != want.UID || st.Gid != want.GID {
		if err := unix.Fchown(fd, int(want.UID), int(want.GID)); err != nil {
			return fmt.Errorf("set the owner of %s to %d:%d: %w", p, want.UID, want.GID, err)
		}
	}
	// After the chown, which clears the setuid and setgid bits of a file.
	if err := unix.Fchmod(fd, want.Mode); err != nil {
		return fmt.Errorf("set the mode of %s to %04o: %w", p, want.Mode, err)
	}
	return nil
}

func (r *ownershipRestorer) createdByThisFix(fd int) (bool, error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_BTIME, &stx); err != nil {
		return false, fmt.Errorf("read the creation time: %w", err)
	}
	if stx.Mask&unix.STATX_BTIME == 0 {
		return false, errNoBirthTime
	}
	return !time.Unix(stx.Btime.Sec, int64(stx.Btime.Nsec)).Before(r.since), nil
}

func (r *ownershipRestorer) target(ctx context.Context, mount, rel string, dir bool, depth, parentFD int) (FileMeta, bool, error) {
	if r.store != nil {
		m, ok, err := r.store.Get(ctx, mount, rel)
		if err != nil {
			return FileMeta{}, false, err
		}
		if ok && m.Dir == dir {
			return m, true, nil
		}
	}
	if depth < 2 {
		return FileMeta{}, false, nil
	}
	var parent unix.Stat_t
	if err := unix.Fstat(parentFD, &parent); err != nil {
		return FileMeta{}, false, fmt.Errorf("read the owner of the directory above %s: %w", filepath.Join(mount, rel), err)
	}
	mode := uint32(defaultFileMode)
	if dir {
		mode = defaultDirMode
	}
	return FileMeta{Disk: mount, RelPath: rel, Dir: dir, UID: parent.Uid, GID: parent.Gid, Mode: mode}, true, nil
}
