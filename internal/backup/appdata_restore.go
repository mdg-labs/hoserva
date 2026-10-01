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
// decrypted and verified end to end and every directory it names has been
// checked to lie inside the appdata location. Then the container, and
// every running container sharing those directories, is stopped, a snapshot of the appdata about to be replaced
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

	staging, err := appdataStaging(roots)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	_, _ = fmt.Fprintf(out, "fetching %s from %s\n", req.Archive, source.Name)
	plain, hdr, err := a.fetchVerifiedAppdata(ctx, req, source, roots, staging)
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

	if err := a.snapshotAppdata(ctx, out, dests, hdr, passphrase, staging, req.Archive); err != nil {
		return err
	}
	return a.replaceAppdata(ctx, out, plain, hdr)
}

// fetchVerifiedAppdata is the read-only part of a restore that the preview
// shares: it fetches and decrypts the archive into staging, verifies it end
// to end, and checks that it holds the requested container and that every
// directory it names is inside the appdata location. It changes nothing but
// staging.
func (a *AppdataService) fetchVerifiedAppdata(ctx context.Context, req AppdataRestoreRequest, source Destination, roots []string, staging string) (string, appdataHeader, error) {
	plain, err := a.fetchAppdata(ctx, source, req.Archive, staging)
	if err != nil {
		return "", appdataHeader{}, err
	}
	hdr, _, err := verifyAppdata(plain)
	if err != nil {
		return "", appdataHeader{}, invalidArchivef("%v", err)
	}
	if hdr.Container != req.Container {
		return "", appdataHeader{}, invalidArchivef("the archive holds %s, not %s", hdr.Container, req.Container)
	}
	if err := validateRestoreDirs(hdr.Dirs, roots); err != nil {
		return "", appdataHeader{}, err
	}
	return plain, hdr, nil
}

// snapshotAppdata writes the appdata the restore is about to replace to
// the destinations, and fails unless at least one holds it. Retention never
// prunes the archive being restored, so a restore that fails after its
// snapshot can be run again from the same archive.
func (a *AppdataService) snapshotAppdata(ctx context.Context, out io.Writer, dests []Destination, restoring appdataHeader, passphrase, staging, archive string) error {
	var existing []string
	for _, d := range restoring.Dirs {
		if _, err := os.Lstat(d); err == nil {
			existing = append(existing, d)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: reading %s: %w", ErrPreRestoreSnapshot, d, err)
		}
	}
	if len(existing) == 0 {
		_, _ = fmt.Fprintln(out, "there is no current appdata to snapshot")
		return nil
	}
	now := a.now()
	name := resolveAppdataName(a.Backup.installationID(), restoring.Container, now, ReasonPreRestore, dests)
	path := filepath.Join(staging, name)
	_, _ = fmt.Fprintf(out, "snapshotting the current appdata of %s\n", restoring.Container)
	if _, err := packAppdata(ctx, path, appdataHeader{
		Container: restoring.Container, Image: restoring.Image, CreatedAt: now, Hostname: a.Backup.Hostname,
		Stopped: true, DatabaseImage: restoring.DatabaseImage, Reason: string(ReasonPreRestore), Dirs: existing,
	}); err != nil {
		return fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, err)
	}
	if _, _, err := verifyAppdata(path); err != nil {
		return fmt.Errorf("%w: verifying it: %w", ErrPreRestoreSnapshot, err)
	}
	written, failures := a.uploadAppdata(ctx, dests, path, name, restoring.Container, passphrase, archive, now)
	_ = os.Remove(path)
	if written == 0 {
		return fmt.Errorf("%w: %w", ErrPreRestoreSnapshot, errors.Join(failures...))
	}
	for _, f := range failures {
		_, _ = fmt.Fprintf(out, "warning: the snapshot did not reach every destination: %v\n", f)
	}
	return nil
}

// replaceAppdata unpacks the archive next to each live directory and swaps
// it in.
func (a *AppdataService) replaceAppdata(ctx context.Context, out io.Writer, archive string, hdr appdataHeader) error {
	id, err := randomID()
	if err != nil {
		return err
	}
	fresh := make([]string, len(hdr.Dirs))
	swaps := make([]appdataSwap, len(hdr.Dirs))
	for i, d := range hdr.Dirs {
		fresh[i] = d + ".hoserva-restore-" + id
		swaps[i] = appdataSwap{live: d, fresh: fresh[i], old: d + ".hoserva-old-" + id}
	}
	removeFresh := func() {
		for _, f := range fresh {
			_ = os.RemoveAll(f)
		}
	}
	_, _ = fmt.Fprintf(out, "restoring %s\n", hdr.Container)
	if err := extractAppdata(ctx, archive, hdr, fresh); err != nil {
		removeFresh()
		return fmt.Errorf("unpacking the archive: %w", err)
	}
	for _, f := range fresh {
		if err := syncAppdataTree(f); err != nil {
			removeFresh()
			return err
		}
	}
	if err := swapAppdataDirs(swaps); err != nil {
		removeFresh()
		return err
	}
	for _, s := range swaps {
		if err := os.RemoveAll(s.old); err != nil {
			_, _ = fmt.Fprintf(out, "warning: the appdata that was replaced is still at %s: %v\n", s.old, err)
		}
	}
	return nil
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
}

// swapAppdataDirs puts each fresh tree in place of its live directory,
// keeping the live one at old. If any step fails, everything already
// swapped is put back, so the live appdata is either all replaced or all
// as it was.
func swapAppdataDirs(swaps []appdataSwap) error {
	for i, s := range swaps {
		if err := swapOne(s); err != nil {
			if rerr := rollbackSwaps(swaps[:i]); rerr != nil {
				return fmt.Errorf("swapping in %s: %w (and putting the earlier ones back failed: %w)", s.live, err, rerr)
			}
			return fmt.Errorf("swapping in %s: %w", s.live, err)
		}
	}
	for _, s := range swaps {
		if err := fsyncDir(filepath.Dir(s.live)); err != nil {
			return err
		}
	}
	return nil
}

func swapOne(s appdataSwap) error {
	_, err := os.Lstat(s.live)
	switch {
	case err == nil:
		if err := os.Rename(s.live, s.old); err != nil {
			return err
		}
		if err := os.Rename(s.fresh, s.live); err != nil {
			if rerr := os.Rename(s.old, s.live); rerr != nil {
				return fmt.Errorf("%w (and putting %s back failed: %w)", err, s.live, rerr)
			}
			return err
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return os.Rename(s.fresh, s.live)
	default:
		return err
	}
}

func rollbackSwaps(done []appdataSwap) error {
	var errs []error
	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]
		if err := os.Rename(s.live, s.fresh); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := os.Lstat(s.old); err == nil {
			if err := os.Rename(s.old, s.live); err != nil {
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
