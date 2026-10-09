package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
)

// AppdataRestoreRequest names one archive on one destination and the
// container it belongs to.
type AppdataRestoreRequest struct {
	Container     string
	Archive       string
	DestinationID string
	// Sharers are the other containers whose appdata overlaps Container's,
	// resolved when the job was submitted (RestoreSharers) and part of its
	// scheduler scope, so the restore may stop them.
	Sharers []string
}

// RestoreSharers names every other container with a bind mount in the
// appdata of name, or with one that holds it: the restore swaps that tree,
// so they are stopped with it and, as its job's scope, cannot be recreated
// beside it.
func (a *AppdataService) RestoreSharers(ctx context.Context, name string) ([]string, error) {
	scope, err := a.Scope(ctx)
	if err != nil {
		return nil, err
	}
	var target []string
	for _, c := range scope {
		if c.Name == name {
			target = c.Dirs
		}
	}
	var out []string
	for _, c := range scope {
		if c.Name != name && dirsOverlap(c.Dirs, target) {
			out = append(out, c.Name)
		}
	}
	return out, nil
}

func dirsOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if withinDir(x, y) || withinDir(y, x) {
				return true
			}
		}
	}
	return false
}

// restoreStopList is what the restore stops: each running container that
// shares hdr's directories, in name order, then the restored container. A
// running sharer the job was not submitted with refuses the restore, since
// nothing keeps a recreate of it from running beside it.
func (a *AppdataService) restoreStopList(ctx context.Context, req AppdataRestoreRequest, hdr appdataHeader, roots []string) ([]AppdataContainer, error) {
	listed, err := a.Containers.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	resolved := resolveRoots(roots)
	allowed := map[string]bool{}
	for _, n := range req.Sharers {
		allowed[n] = true
	}
	var sharers []string
	running := false
	for _, c := range listed {
		if c.Name == req.Container {
			running = containerActive(c.State)
			continue
		}
		if !containerActive(c.State) || !dirsOverlap(appdataDirs(c, resolved), hdr.Dirs) {
			continue
		}
		if !allowed[c.Name] {
			return nil, fmt.Errorf("%s is running and mounts appdata this restore replaces, but it was not in the restore's scope when it was queued: restore again", c.Name)
		}
		sharers = append(sharers, c.Name)
	}
	sort.Strings(sharers)
	var out []AppdataContainer
	for _, n := range sharers {
		out = append(out, AppdataContainer{Name: n})
	}
	if running {
		out = append(out, AppdataContainer{Name: req.Container})
	}
	return out, nil
}

func invalidArchivef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrAppdataArchiveInvalid, fmt.Sprintf(format, args...))
}

// Restore replaces one container's appdata with an archive's content
// (doc 10 §2). Nothing is changed until the archive has been fetched,
// decrypted, verified end to end and authenticated, and every directory it
// names has been checked to be one of the container's own inside the appdata
// location. Then the container, and
// every running container sharing those directories, is stopped, the
// directories it will replace are opened by their parents and each one's
// identity is recorded, a snapshot of the appdata about to be replaced
// is written to the destinations, and only if that snapshot was written
// somewhere is the appdata replaced: the archive is unpacked next to the
// live directories first and swapped in by renames, so a failure while
// unpacking or swapping leaves the live appdata as it was. What was stopped
// is started again whatever happens.
func (a *AppdataService) Restore(ctx context.Context, req AppdataRestoreRequest, out io.Writer) (err error) {
	if err := a.lockRun(ctx); err != nil {
		return err
	}
	defer a.runMu.Unlock()
	if err := a.Containers.RequireArrayRunning(); err != nil {
		return err
	}
	source, err := a.checkRestoreRequest(ctx, req)
	if err != nil {
		return err
	}
	roots, err := a.appdataRoots(ctx)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return invalidArchivef("the array has no cache disk, so there is no appdata location to restore into")
	}
	dests, err := a.destinations(ctx)
	if err != nil {
		return err
	}
	if len(dests) == 0 {
		return fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, ErrAppdataNoDestination)
	}
	passphrase, err := a.requireEncryption(ctx, dests)
	if err != nil {
		return err
	}
	key, err := a.archiveKey()
	if err != nil {
		return err
	}
	privileged, err := resolvePrivilegedGroups(a.groupLookup())
	if err != nil {
		return fmt.Errorf("the privileged groups could not be read, so the restore cannot tell which setgid bits to leave off: %w", err)
	}

	staging, err := appdataStaging(roots)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	_, _ = fmt.Fprintf(out, "fetching %s from %s\n", req.Archive, source.Name)
	plain, hdr, err := a.fetchVerifiedAppdata(ctx, req, source, roots, staging, key)
	if err != nil {
		return err
	}

	toStop, err := a.restoreStopList(ctx, req, hdr, roots)
	if err != nil {
		return err
	}
	var attempted []AppdataContainer
	if len(toStop) > 0 {
		names := make([]string, len(toStop))
		for i, c := range toStop {
			names[i] = c.Name
		}
		if err := a.journalAdd(names); err != nil {
			return err
		}
		defer func() {
			if rerr := a.restartAll(ctx, out, attempted); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}()
	}
	for _, c := range toStop {
		attempted = append(attempted, c)
		_, _ = fmt.Fprintf(out, "stopping %s\n", c.Name)
		if _, err := a.Containers.Stop(ctx, c.Name); err != nil {
			return fmt.Errorf("stopping %s: %w", c.Name, err)
		}
	}

	parents := &heldDirs{}
	defer parents.close()
	if beforeAppdataParents != nil {
		beforeAppdataParents()
	}
	recorded, err := parents.recordAll(hdr.Dirs)
	if err != nil {
		return err
	}
	if err := parents.requireAnchored(a.dirAttrs(), hdr.Dirs, recorded); err != nil {
		return fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, err)
	}
	archived, err := a.snapshotAppdata(ctx, out, dests, hdr, parents, recorded, passphrase, key, staging, req.Archive)
	if err != nil {
		return err
	}
	return a.replaceAppdata(ctx, out, plain, hdr, parents, recorded, archived, privileged)
}

// fetchVerifiedAppdata is the read-only part of a restore that the preview
// shares: it fetches and decrypts the archive into staging, verifies it end
// to end, checks that this installation wrote it (authenticateAppdata, under
// key), that it holds the requested container and that every directory it
// names is one of that container's own inside the appdata location. It
// changes nothing but staging.
func (a *AppdataService) fetchVerifiedAppdata(ctx context.Context, req AppdataRestoreRequest, source Destination, roots []string, staging string, key []byte) (string, appdataHeader, error) {
	plain, err := a.fetchAppdata(ctx, source, req.Archive, staging)
	if err != nil {
		return "", appdataHeader{}, err
	}
	hdr, trailer, err := verifyAppdata(plain)
	if err != nil {
		return "", appdataHeader{}, invalidArchivef("%v", err)
	}
	if err := authenticateAppdata(trailer, key, source, req.Archive); err != nil {
		return "", appdataHeader{}, err
	}
	if hdr.Container != req.Container {
		return "", appdataHeader{}, invalidArchivef("the archive holds %s, not %s", hdr.Container, req.Container)
	}
	if err := validateRestoreDirs(hdr.Dirs, roots); err != nil {
		return "", appdataHeader{}, err
	}
	if err := a.requireOwnDirs(ctx, req.Container, hdr.Dirs, roots); err != nil {
		return "", appdataHeader{}, err
	}
	return plain, hdr, nil
}

// authenticateAppdata refuses an archive this installation did not write. A
// tag that is there must be the one key gives the trailer. An archive written
// before archives carried a tag has none; it is accepted only when it came
// out of a destination that encrypts, since what opens there is an archive
// encrypted to the onboarding recipient, whose identity is persisted only
// wrapped under the machine key and travels only wrapped under the backup
// passphrase, and refused from one that does not, where anyone able to write
// the folder can leave a consistent archive of their own.
func authenticateAppdata(trailer appdataTrailer, key []byte, source Destination, name string) error {
	if trailer.MAC != "" {
		if !trailer.authentic(key) {
			return invalidArchivef("the archive's authentication tag is not the one this installation gives its archives: it was written elsewhere or was altered")
		}
		return nil
	}
	if _, _, _, encrypted, _ := parseAppdataName(name); encrypted && (source.Encrypt || source.isRemote()) {
		return nil
	}
	return invalidArchivef("the archive predates archive authentication and %q does not encrypt, so nothing shows this installation wrote it: restore it by hand if you trust it", source.Name)
}

// requireOwnDirs refuses a list of directories unless each is one of the
// bind-mount sources of the container being restored that lie strictly inside
// an appdata location, as the container mounts them now, resolved through
// links where they exist. A directory that is missing on disk is still the
// container's if its mount names it. A header that names anything else would
// make a restore of one container replace another's appdata.
func (a *AppdataService) requireOwnDirs(ctx context.Context, name string, dirs, roots []string) error {
	listed, err := a.Containers.List(ctx)
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	resolved := resolveRoots(roots)
	for _, c := range listed {
		if c.Name != name {
			continue
		}
		own := map[string]bool{}
		for _, m := range c.Mounts {
			if m.Source == "" || !filepath.IsAbs(m.Source) {
				continue
			}
			real := resolveExisting(filepath.Clean(m.Source))
			if info, err := os.Stat(real); err == nil && !info.IsDir() {
				continue
			}
			for _, r := range resolved {
				if withinDir(real, r) && real != r {
					own[real] = true
					break
				}
			}
		}
		for _, d := range dirs {
			if !own[d] {
				return invalidArchivef("%s is not one of %s's appdata directories", d, name)
			}
		}
		return nil
	}
	return invalidArchivef("%s is not a container on this server, so there is no appdata directory of its to restore into: create it again first", name)
}

// snapshotAppdata writes the appdata the restore is about to replace to
// the destinations, and fails unless at least one holds it. Before packing,
// and again after, it checks relative to the held parents that each restored
// directory is still the entry recorded for it (or still absent, if none was
// recorded) and refuses otherwise, before anything is uploaded. The packing
// itself resolves the paths by name, so the two checks bracket it. Retention
// never prunes the archive being restored, so a restore that fails after its
// snapshot can be run again from the same archive. It returns the device and
// inode of every entry the snapshot archived under each directory it packed.
func (a *AppdataService) snapshotAppdata(ctx context.Context, out io.Writer, dests []Destination, restoring appdataHeader, parents *heldDirs, recorded []liveIdentity, passphrase string, key []byte, staging, archive string) (archivedIDs, error) {
	if afterAppdataRecord != nil {
		afterAppdataRecord()
	}
	if err := parents.requireRecorded(restoring.Dirs, recorded); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, err)
	}
	var existing []string
	for i, d := range restoring.Dirs {
		if recorded[i].present {
			existing = append(existing, d)
		}
	}
	if len(existing) == 0 {
		_, _ = fmt.Fprintln(out, "there is no current appdata to snapshot")
		return nil, nil
	}
	now := a.now()
	name := resolveAppdataName(a.Backup.installationID(), restoring.Container, now, ReasonPreRestore, dests)
	path := filepath.Join(staging, name)
	_, _ = fmt.Fprintf(out, "snapshotting the current appdata of %s\n", restoring.Container)
	archived := archivedIDs{}
	if _, err := packAppdataRecording(ctx, path, appdataHeader{
		Container: restoring.Container, Image: restoring.Image, CreatedAt: now, Hostname: a.Backup.Hostname,
		Stopped: true, DatabaseImage: restoring.DatabaseImage, Reason: string(ReasonPreRestore), Dirs: existing,
	}, key, archived); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, err)
	}
	if afterAppdataPack != nil {
		afterAppdataPack()
	}
	if err := parents.requireRecorded(restoring.Dirs, recorded); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("%w: after packing: %w", ErrPreRestoreSnapshot, err)
	}
	if _, _, err := verifyAppdata(path); err != nil {
		return nil, fmt.Errorf("%w: verifying it: %w", ErrPreRestoreSnapshot, err)
	}
	written, failures := a.uploadAppdata(ctx, dests, path, name, restoring.Container, passphrase, archive, now)
	_ = os.Remove(path)
	if written == 0 {
		return nil, fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, errors.Join(failures...))
	}
	for _, f := range failures {
		_, _ = fmt.Fprintf(out, "warning: the snapshot did not reach every destination: %v\n", f)
	}
	return archived, nil
}

// heldDirs holds a descriptor for each directory that contains one of
// the directories being restored, opened once with no link followed in any
// component and kept for the whole restore. The restored tree is created,
// unpacked, swapped and removed through them, never by re-resolving a path,
// so a parent replaced by a link after it was opened changes nothing.
type heldDirs struct {
	fds map[string]int
}

// parent returns the held descriptor of the directory containing path and
// path's name in it, opening the directory the first time.
func (p *heldDirs) parent(path string) (int, string, error) {
	dir := filepath.Dir(path)
	fd, ok := p.fds[dir]
	if !ok {
		var err error
		if fd, err = beneath.OpenResolvedDir(dir); err != nil {
			return -1, "", fmt.Errorf("opening %s: %w", dir, err)
		}
		if p.fds == nil {
			p.fds = map[string]int{}
		}
		p.fds[dir] = fd
	}
	return fd, filepath.Base(path), nil
}

func (p *heldDirs) close() {
	for _, fd := range p.fds {
		_ = unix.Close(fd)
	}
	p.fds = nil
}

// beforeAppdataParents, when set, runs in Restore just before it opens the
// directories it works in and records the identity of each restored
// directory. afterAppdataRecord runs just after the identities are recorded,
// before the snapshot checks them, and afterAppdataPack after the snapshot is
// packed, before it checks them again. beforeAppdataUnpack runs in
// replaceAppdata after the snapshot, before anything is unpacked. A test uses
// them to rearrange the tree at those moments.
var beforeAppdataParents, afterAppdataRecord, afterAppdataPack, beforeAppdataUnpack func()

// beforeAppdataMoveAside, when set, runs in swapOne after the live entry has
// been checked and just before it is renamed to its old name.
var beforeAppdataMoveAside func()

// beforeAppdataCleanup, when set, runs in replaceAppdata just before it
// removes what it unpacked or set aside, on every path out of the swap.
var beforeAppdataCleanup func()

// beforeAppdataWorkOpen, when set, runs in makeWork between creating a work
// directory and opening it, and in extract between creating a tree and
// opening it, with the path of the directory just created.
var beforeAppdataWorkOpen, beforeAppdataTreeOpen func(path string)

// beforeAppdataRollback, when set, runs just before swap puts already swapped
// directories back.
var beforeAppdataRollback func()

// liveIdentity is what a restore recorded about the entry at a live
// directory's name before unpacking: that there was none, or the device and
// inode of the entry that was there.
type liveIdentity struct {
	present  bool
	dev, ino uint64
}

func identityOf(st *unix.Stat_t) liveIdentity {
	return liveIdentity{present: true, dev: uint64(st.Dev), ino: uint64(st.Ino)}
}

// recordLive reads the identity of the entry at path, without following a
// link, relative to its held parent.
func (p *heldDirs) recordLive(path string) (liveIdentity, error) {
	parent, name, err := p.parent(path)
	if err != nil {
		return liveIdentity{}, err
	}
	var st unix.Stat_t
	switch err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return identityOf(&st), nil
	case errors.Is(err, unix.ENOENT):
		return liveIdentity{}, nil
	default:
		return liveIdentity{}, fmt.Errorf("reading %s: %w", path, err)
	}
}

// recordAll records the identity of the entry at each of dirs.
func (p *heldDirs) recordAll(dirs []string) ([]liveIdentity, error) {
	recorded := make([]liveIdentity, len(dirs))
	for i, d := range dirs {
		var err error
		if recorded[i], err = p.recordLive(d); err != nil {
			return nil, err
		}
	}
	return recorded, nil
}

// requireRecorded refuses unless the entry at each of dirs, read without
// following a link relative to its held parent, is the one recorded for it:
// the same device and inode, or absent where none was recorded.
func (p *heldDirs) requireRecorded(dirs []string, recorded []liveIdentity) error {
	for i, d := range dirs {
		now, err := p.recordLive(d)
		if err != nil {
			return err
		}
		if now != recorded[i] {
			return fmt.Errorf("%s is not the directory the restore recorded", d)
		}
	}
	return nil
}

// restoreWork is the directory a restore creates next to a live directory
// to work in: the unpacked tree is built in it as "tree" and the live
// directory is kept in it as "old" while the swap is in place. Its descriptor
// is held, and the tree, the replaced directory and the renames between them
// are reached through that descriptor, not through a name in the appdata
// location. Its identity is recorded once checkCreatedDir has accepted it.
type restoreWork struct {
	path string
	id   liveIdentity
}

// checkCreatedDir reads the directory fd, which a restore has just made with
// mkdirat and opened by name, and refuses it unless it is a directory owned by
// the daemon's user with mode 0700 (the group's inherited setgid bit aside)
// that is empty. Another principal with write access to the parent can put a
// directory of its own at the name between the two calls; it cannot make one
// that is owned by the daemon's user when that is root. On success it returns
// the identity of fd.
func checkCreatedDir(fd int, path string) (liveIdentity, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return liveIdentity{}, fmt.Errorf("reading %s: %w", path, err)
	}
	switch {
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return liveIdentity{}, fmt.Errorf("%s is not a directory", path)
	case int(st.Uid) != os.Geteuid():
		return liveIdentity{}, fmt.Errorf("%s is not owned by the user the restore runs as", path)
	case st.Mode&0o5777 != 0o700:
		return liveIdentity{}, fmt.Errorf("%s does not have mode 0700", path)
	}
	names, err := beneath.ReadNames(fd)
	if err != nil {
		return liveIdentity{}, fmt.Errorf("listing %s: %w", path, err)
	}
	if len(names) != 0 {
		return liveIdentity{}, fmt.Errorf("%s is not empty", path)
	}
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return liveIdentity{}, fmt.Errorf("rewinding %s: %w", path, err)
	}
	return identityOf(&st), nil
}

// makeWork creates the work directory at path, opens it and holds it, and
// refuses it if checkCreatedDir does. A refused directory is left where it
// is: whatever is at the name is not removed, and the directory this call
// created, if it is no longer at the name, stays wherever it was moved to.
func (p *heldDirs) makeWork(path string) (restoreWork, error) {
	parent, name, err := p.parent(path)
	if err != nil {
		return restoreWork{}, err
	}
	if err := unix.Mkdirat(parent, name, 0o700); err != nil {
		return restoreWork{}, fmt.Errorf("creating %s: %w", path, err)
	}
	if beforeAppdataWorkOpen != nil {
		beforeAppdataWorkOpen(path)
	}
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return restoreWork{}, fmt.Errorf("opening %s: %w", path, err)
	}
	id, err := checkCreatedDir(fd, path)
	if err != nil {
		_ = unix.Close(fd)
		return restoreWork{}, fmt.Errorf("refusing the work directory: %w", err)
	}
	p.fds[path] = fd
	return restoreWork{path: path, id: id}, nil
}

// replaceAppdata unpacks the archive next to each live directory and swaps
// it in, replacing only the entries recorded in recorded, which Restore read
// through parents before the snapshot. Of the directory it replaced it
// removes only what archived says the snapshot archived.
func (a *AppdataService) replaceAppdata(ctx context.Context, out io.Writer, archive string, hdr appdataHeader, parents *heldDirs, recorded []liveIdentity, archived archivedIDs, privileged privilegedGIDs) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	if beforeAppdataUnpack != nil {
		beforeAppdataUnpack()
	}
	works := make([]restoreWork, 0, len(hdr.Dirs))
	swaps := make([]appdataSwap, len(hdr.Dirs))
	for i, d := range hdr.Dirs {
		path := d + ".hoserva-restore-" + id
		swaps[i] = appdataSwap{live: d, fresh: path + "/tree", old: path + "/old", recorded: recorded[i], archived: archived[d]}
	}
	// discard removes what this restore put in its work directories, each
	// only if it is still the entry the restore recorded, and then the work
	// directories, which it removes only while they are empty. Whatever is
	// not what was recorded is left where it is and reported. Inside the
	// replaced directory only the entries the snapshot archived are removed;
	// the work directory is then left too, since it still holds the rest.
	discard := func(keepOld bool) {
		if beforeAppdataCleanup != nil {
			beforeAppdataCleanup()
		}
		for i := range works {
			s := &swaps[i]
			if keepOld {
				if err := parents.removeFresh(s); err != nil {
					_, _ = fmt.Fprintf(out, "warning: left %s in place: %v\n", s.fresh, err)
				}
			} else {
				kept, err := parents.removeOld(s)
				if err != nil {
					_, _ = fmt.Fprintf(out, "warning: the appdata that was replaced is still at %s: %v\n", s.old, err)
					continue
				}
				if len(kept) > 0 {
					_, _ = fmt.Fprintf(out, "warning: %s\n", keptMessage(filepath.Dir(s.old), kept))
					continue
				}
			}
			if err := parents.removeWork(works[i]); err != nil {
				_, _ = fmt.Fprintf(out, "warning: left %s in place: %v\n", works[i].path, err)
			}
		}
	}
	_, _ = fmt.Fprintf(out, "restoring %s\n", hdr.Container)
	for i := range swaps {
		w, err := parents.makeWork(filepath.Dir(swaps[i].fresh))
		if err != nil {
			discard(true)
			return fmt.Errorf("unpacking the archive: %w", err)
		}
		works = append(works, w)
	}
	fresh := make([]string, len(swaps))
	for i, s := range swaps {
		fresh[i] = s.fresh
	}
	trees := make([]liveIdentity, len(swaps))
	var stripped []string
	err = parents.extract(ctx, archive, hdr, fresh, trees, privileged, func(path, bits string) {
		stripped = append(stripped, fmt.Sprintf("restored %s without its %s bit: the archive gives it to root or to a root-equivalent group", path, bits))
	})
	for i := range swaps {
		swaps[i].tree = trees[i]
	}
	if err != nil {
		discard(true)
		return fmt.Errorf("unpacking the archive: %w", err)
	}
	for _, line := range stripped {
		_, _ = fmt.Fprintln(out, line)
	}
	rep := &anchorReport{out: out}
	for i := range swaps {
		note, err := parents.anchorTree(a.dirAttrs(), &swaps[i])
		if err != nil {
			discard(true)
			return fmt.Errorf("anchoring the unpacked archive: %w", err)
		}
		rep.note(swaps[i].live, note)
	}
	for _, f := range fresh {
		if err := parents.syncTree(f); err != nil {
			discard(true)
			return err
		}
	}
	if err := parents.swap(swaps); err != nil {
		discard(true)
		return err
	}
	discard(false)
	return nil
}

// removeFresh removes the unpacked tree of s, only if it is the directory
// extract recorded when it created it. A tree that was never recorded is
// left alone.
func (p *heldDirs) removeFresh(s *appdataSwap) error {
	if !s.tree.present {
		return nil
	}
	return p.removeRecorded(s.fresh, s.tree)
}

// removeOld removes the directory a swap moved aside, only if it is the one
// the restore recorded before unpacking, and inside it only the entries whose
// device and inode the snapshot recorded when it archived them, by whatever
// name they now have. It returns the paths, relative to the work directory,
// of the entries it left in place. An error stops the removal where it is and
// leaves the rest.
func (p *heldDirs) removeOld(s *appdataSwap) ([]string, error) {
	if !s.recorded.present {
		return nil, nil
	}
	parent, name, err := p.parent(s.old)
	if err != nil {
		return nil, err
	}
	kept, err := beneath.RemoveDirIfListed(parent, name,
		func(dev, ino uint64) bool { return dev == s.recorded.dev && ino == s.recorded.ino },
		func(dev, ino uint64) bool { _, ok := s.archived[devIno{dev, ino}]; return ok })
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return kept, err
}

// keptMessage names the entries a restore left in the directory dir because
// the pre-restore snapshot did not archive them, at most five of them.
func keptMessage(dir string, kept []string) string {
	sort.Strings(kept)
	shown := kept
	if len(shown) > 5 {
		shown = shown[:5]
	}
	paths := make([]string, len(shown))
	for i, k := range shown {
		paths[i] = strconv.Quote(filepath.Join(dir, k))
	}
	msg := fmt.Sprintf("left in place because the pre-restore snapshot did not archive them: %s", strings.Join(paths, ", "))
	if len(kept) > len(shown) {
		msg += fmt.Sprintf(" and %d more", len(kept)-len(shown))
	}
	return msg
}

// removeRecorded removes the directory at path through a descriptor whose
// device and inode are checked against want; any other directory at path is
// left in place.
func (p *heldDirs) removeRecorded(path string, want liveIdentity) error {
	parent, name, err := p.parent(path)
	if err != nil {
		return err
	}
	err = beneath.RemoveDirIf(parent, name, func(dev, ino uint64) bool { return dev == want.dev && ino == want.ino })
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// removeWork removes a work directory whose device and inode are the ones
// recorded for it. The final removal resolves the name once more and removes
// an empty directory only.
func (p *heldDirs) removeWork(w restoreWork) error {
	parent, name, err := p.parent(w.path)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(parent, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", w.path, err)
	}
	if identityOf(&st) != w.id {
		return fmt.Errorf("%s is not the directory the restore created", w.path)
	}
	if err := unix.Unlinkat(parent, name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("removing %s: %w", w.path, err)
	}
	return nil
}

func (p *heldDirs) syncTree(path string) error {
	parent, name, err := p.parent(path)
	if err != nil {
		return err
	}
	fd, err := beneath.Open(parent, name, unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	return syncAppdataTree(fd, path)
}

func randomID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating an id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// validateRestoreDirs refuses an archive naming a directory that is not
// exactly where the appdata backup would have put it: absolute, already
// resolved, strictly inside an appdata location, and not inside another of
// its directories. The archive's header is data from a backup destination,
// not something to trust with a path.
func validateRestoreDirs(dirs, roots []string) error {
	if len(dirs) == 0 {
		return invalidArchivef("the archive names no directory")
	}
	resolved := resolveRoots(roots)
	for _, d := range dirs {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d {
			return invalidArchivef("%q is not a clean absolute path", d)
		}
		if real := resolveExisting(d); real != d {
			return invalidArchivef("%s now resolves to %s", d, real)
		}
		inside := false
		for _, r := range resolved {
			if withinDir(d, r) && d != r {
				inside = true
			}
		}
		if !inside {
			return invalidArchivef("%s is not inside the appdata location", d)
		}
	}
	for i, d := range dirs {
		for j, e := range dirs {
			if i != j && withinDir(d, e) {
				return invalidArchivef("%s is inside %s", d, e)
			}
		}
	}
	return nil
}

type appdataSwap struct {
	live, fresh, old string
	recorded         liveIdentity
	// tree is the identity of the fresh tree, recorded by extract from the
	// descriptor it unpacks into.
	tree liveIdentity
	// archived is what the pre-restore snapshot archived from the live
	// directory; removeOld removes nothing else from old.
	archived map[devIno]struct{}
}

// swap puts each fresh tree in place of its live directory, keeping the
// live one at old. Only the entry recorded before the unpack is moved aside;
// any other entry at the live name fails the swap. If any step fails,
// everything already swapped is put back, so the live appdata is either all
// replaced or all as it was.
func (p *heldDirs) swap(swaps []appdataSwap) error {
	for i := range swaps {
		s := &swaps[i]
		if err := p.swapOne(s); err != nil {
			if rerr := p.rollback(swaps[:i]); rerr != nil {
				return fmt.Errorf("swapping in %s: %w (and putting the earlier ones back failed: %w)", s.live, err, rerr)
			}
			return fmt.Errorf("swapping in %s: %w", s.live, err)
		}
	}
	for _, s := range swaps {
		parent, _, err := p.parent(s.live)
		if err != nil {
			return err
		}
		if err := unix.Fsync(parent); err != nil {
			return fmt.Errorf("syncing directory %s: %w", filepath.Dir(s.live), err)
		}
	}
	return nil
}

func renameat(fromDir int, from string, toDir int, to string) error {
	if err := unix.Renameat(fromDir, from, toDir, to); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", from, to, err)
	}
	return nil
}

// swapInTree renames the tree at fresh in freshDir to live in parent if it
// has the identity extract recorded for it. Without a recorded identity, or
// with another, it renames nothing.
func swapInTree(s *appdataSwap, freshDir int, fresh string, parent int, live string) error {
	if !s.tree.present {
		return fmt.Errorf("no identity was recorded for %s", s.fresh)
	}
	var st unix.Stat_t
	if err := unix.Fstatat(freshDir, fresh, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("reading %s: %w", s.fresh, err)
	}
	if identityOf(&st) != s.tree {
		return fmt.Errorf("%s is not the tree the restore unpacked", s.fresh)
	}
	return renameat(freshDir, fresh, parent, live)
}

func (p *heldDirs) swapOne(s *appdataSwap) error {
	parent, live, err := p.parent(s.live)
	if err != nil {
		return err
	}
	freshDir, fresh, err := p.parent(s.fresh)
	if err != nil {
		return err
	}
	oldDir, old, err := p.parent(s.old)
	if err != nil {
		return err
	}
	var st unix.Stat_t
	switch err := unix.Fstatat(parent, live, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		if identityOf(&st) != s.recorded {
			return fmt.Errorf("%s is not the directory the restore recorded before unpacking", s.live)
		}
		if beforeAppdataMoveAside != nil {
			beforeAppdataMoveAside()
		}
		if err := renameat(parent, live, oldDir, old); err != nil {
			return err
		}
		if err := unix.Fstatat(oldDir, old, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityOf(&st) != s.recorded {
			cause := fmt.Errorf("%s is not the directory the restore recorded before unpacking", s.live)
			if err != nil {
				cause = fmt.Errorf("reading %s: %w", s.old, err)
			}
			if rerr := renameat(oldDir, old, parent, live); rerr != nil {
				return fmt.Errorf("%w (and putting %s back failed: %w)", cause, s.live, rerr)
			}
			return cause
		}
		if err := swapInTree(s, freshDir, fresh, parent, live); err != nil {
			if rerr := renameat(oldDir, old, parent, live); rerr != nil {
				return fmt.Errorf("%w (and putting %s back failed: %w)", err, s.live, rerr)
			}
			return err
		}
		return nil
	case errors.Is(err, unix.ENOENT):
		if s.recorded.present {
			return fmt.Errorf("%s is gone since the restore recorded it", s.live)
		}
		return swapInTree(s, freshDir, fresh, parent, live)
	default:
		return fmt.Errorf("reading %s: %w", s.live, err)
	}
}

// rollback puts the directories already swapped back. It moves the entry at
// a live name back into its work directory only if it turns out to be the
// tree the restore swapped in; anything else is returned to the live name and
// reported, and the directory that was moved aside stays where it is.
func (p *heldDirs) rollback(done []appdataSwap) error {
	if beforeAppdataRollback != nil {
		beforeAppdataRollback()
	}
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		s := &done[i]
		parent, live, err := p.parent(s.live)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		freshDir, fresh, err := p.parent(s.fresh)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		oldDir, old, err := p.parent(s.old)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := renameat(parent, live, freshDir, fresh); err != nil {
			errs = append(errs, err)
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(freshDir, fresh, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || identityOf(&st) != s.tree {
			cause := fmt.Errorf("%s is not the tree the restore swapped in; left in place, and the replaced directory is at %s", s.live, s.old)
			if err != nil {
				cause = fmt.Errorf("reading %s: %w", s.fresh, err)
			}
			if rerr := renameat(freshDir, fresh, parent, live); rerr != nil {
				cause = fmt.Errorf("%w (and putting %s back failed: %w)", cause, s.live, rerr)
			}
			errs = append(errs, cause)
			continue
		}
		var ost unix.Stat_t
		if err := unix.Fstatat(oldDir, old, &ost, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			if err := renameat(oldDir, old, parent, live); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// CheckAppdataArchiveName refuses an archive name that is not a plain
// appdata archive file name written by the installation for that container.
// It is the check the daemon and cmd/mockapi both run (D18).
func CheckAppdataArchiveName(archive, containerName, installation string) error {
	if archive == "" || archive != filepath.Base(archive) {
		return invalidArchivef("%q is not an archive name", archive)
	}
	inst, archiveContainer, _, _, ok := parseAppdataName(archive)
	if !ok {
		return invalidArchivef("%q is not an appdata archive name", archive)
	}
	if inst != installation {
		return invalidArchivef("%q was written by another installation", archive)
	}
	if archiveContainer != containerName {
		return invalidArchivef("%q is an archive of %s, not %s", archive, archiveContainer, containerName)
	}
	return nil
}

// checkRestoreRequest refuses, before anything is fetched, a request whose
// archive name is not one this installation wrote for that container, or
// whose destination does not exist. It returns the destination.
func (a *AppdataService) checkRestoreRequest(ctx context.Context, req AppdataRestoreRequest) (Destination, error) {
	if err := CheckAppdataArchiveName(req.Archive, req.Container, a.Backup.installationID()); err != nil {
		return Destination{}, err
	}
	all, err := a.Backup.loadDestinations(ctx)
	if err != nil {
		return Destination{}, err
	}
	for _, d := range all {
		if d.ID == req.DestinationID {
			return d, nil
		}
	}
	return Destination{}, fmt.Errorf("%w: %s", ErrDestinationNotFound, req.DestinationID)
}

// RequireArrayRunning refuses while the array is stopped or its storage is
// not ready; the API checks it before queueing a job that would fail.
func (a *AppdataService) RequireArrayRunning() error {
	return a.Containers.RequireArrayRunning()
}

// FindArchive reports whether the named archive is on the named
// destination, so a restore is refused up front instead of failing later
// as a job. It reads the destination's listing.
func (a *AppdataService) FindArchive(ctx context.Context, containerName, archive, destinationID string) error {
	req := AppdataRestoreRequest{Container: containerName, Archive: archive, DestinationID: destinationID}
	dest, err := a.checkRestoreRequest(ctx, req)
	if err != nil {
		return err
	}
	release, why := a.Backup.admitDestination(ctx, dest)
	if why != "" {
		return fmt.Errorf("destination %q cannot be read: %s", dest.ID, why)
	}
	defer release()
	entries, err := a.listDestination(ctx, dest)
	if err != nil {
		return fmt.Errorf("listing destination %q: %w", dest.ID, err)
	}
	for _, e := range entries {
		if e.name == archive {
			return nil
		}
	}
	return fmt.Errorf("%w: %s on destination %q", ErrAppdataArchiveNotFound, archive, dest.ID)
}
