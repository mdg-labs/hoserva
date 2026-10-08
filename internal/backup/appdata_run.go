package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mdg-labs/hoserva/internal/container"
)

// AppdataRunRequest selects what one appdata backup covers: the named
// containers, or, with none named, every included one.
//
// Resolved is the set the job was submitted with (its scheduler scope,
// AppdataService.ScopeNames) and is required: the run acts on exactly those
// containers. One that no longer exists is skipped, and so is one that is
// no longer included when Containers named none, but a container the scope
// did not name is never added. An empty, non-nil Resolved backs up nothing;
// nil is refused, since resolving at start is what lets a run act outside
// its scope.
type AppdataRunRequest struct {
	Containers []string
	Resolved   []string
}

// selectResolved is the part of scope that names a resolved container, and
// a line for each name it had to skip.
func selectResolved(scope []AppdataContainer, req AppdataRunRequest) (selected []AppdataContainer, skipped []string) {
	byName := make(map[string]AppdataContainer, len(scope))
	for _, c := range scope {
		byName[c.Name] = c
	}
	seen := map[string]bool{}
	for _, name := range req.Resolved {
		if seen[name] {
			continue
		}
		seen[name] = true
		c, ok := byName[name]
		switch {
		case !ok:
			skipped = append(skipped, fmt.Sprintf("skipping %s: it no longer exists or no longer has appdata", name))
		case len(req.Containers) == 0 && !c.Included:
			skipped = append(skipped, fmt.Sprintf("skipping %s: it is no longer included in the backup", name))
		default:
			selected = append(selected, c)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return selected, skipped
}

// appdataStaging returns a fresh, empty staging directory next to the
// appdata location. Anything an earlier run left there is removed first:
// only one run exists at a time, so it is stale.
func appdataStaging(roots []string) (string, error) {
	base := filepath.Join(filepath.Dir(roots[0]), appdataStagingDir)
	if err := os.RemoveAll(base); err != nil {
		return "", fmt.Errorf("clearing %s: %w", base, err)
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", base, err)
	}
	return base, nil
}

type stagedAppdata struct {
	c    AppdataContainer
	name string
	path string
}

// Run is doc 10 §2's appdata backup. Every included container that is
// running and set to be stopped is stopped; every included container's
// appdata is archived, one archive each, into a staging directory next to
// the appdata location; the stopped containers are started again, in
// reverse order; and only then are the archives verified and written to
// the destinations, so a container is down for the copy and not for a slow
// upload. A container that could not be stopped is not archived, since its
// copy would not be consistent, and the run fails once everything else is
// done. Every container the run stopped is started again whatever fails,
// including after a cancel.
func (a *AppdataService) Run(ctx context.Context, req AppdataRunRequest, out io.Writer) error {
	if req.Resolved == nil {
		return errors.New("appdata backup: the run was given no resolved containers, so it has no scope")
	}
	if err := a.lockRun(ctx); err != nil {
		return err
	}
	defer a.runMu.Unlock()
	if err := a.Containers.RequireArrayRunning(); err != nil {
		return err
	}
	roots, err := a.appdataRoots(ctx)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		_, _ = fmt.Fprintln(out, "the array has no cache disk, so there is no appdata location to back up")
		return nil
	}
	scope, err := a.Scope(ctx)
	if err != nil {
		return err
	}
	selected, skipped := selectResolved(scope, req)
	for _, line := range skipped {
		_, _ = fmt.Fprintln(out, line)
	}
	if len(selected) == 0 {
		_, _ = fmt.Fprintln(out, "no container with appdata is included in the backup")
		return nil
	}
	dests, err := a.destinations(ctx)
	if err != nil {
		return err
	}
	if len(dests) == 0 {
		return ErrAppdataNoDestination
	}
	passphrase, err := a.requireEncryption(ctx, dests)
	if err != nil {
		return err
	}
	key, err := a.archiveKey()
	if err != nil {
		return err
	}
	staging, err := appdataStaging(roots)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	now := a.now()
	for _, c := range selected {
		if w := c.Warning(); w != "" {
			_, _ = fmt.Fprintf(out, "warning: %s\n", w)
		}
	}
	failures := map[string]error{}
	staged, err := a.archiveStopped(ctx, out, selected, staging, now, dests, ReasonNone, key, failures)
	if err != nil {
		return err
	}

	var errs []error
	for _, s := range staged {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := a.verifyAndUpload(ctx, out, dests, s, passphrase, now); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.c.Name, err))
		}
	}
	names := make([]string, 0, len(failures))
	for name := range failures {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		errs = append(errs, fmt.Errorf("%s: %w", name, failures[name]))
	}
	if len(errs) > 0 {
		return fmt.Errorf("appdata backup: %w", errors.Join(errs...))
	}
	return nil
}

func (a *AppdataService) verifyAndUpload(ctx context.Context, out io.Writer, dests []Destination, s stagedAppdata, passphrase string, now time.Time) error {
	if _, trailer, err := verifyAppdata(s.path); err != nil {
		return fmt.Errorf("verifying the archive: %w", err)
	} else if trailer.Changed > 0 {
		_, _ = fmt.Fprintf(out, "warning: %s: %d files changed while they were copied\n", s.c.Name, trailer.Changed)
	}
	written, failures := a.uploadAppdata(ctx, dests, s.path, s.name, s.c.Name, passphrase, "", now)
	_ = os.Remove(s.path)
	if len(failures) > 0 {
		if written > 0 {
			_, _ = fmt.Fprintf(out, "%s: archived to %d of %d destinations\n", s.c.Name, written, len(dests))
		}
		return errors.Join(failures...)
	}
	_, _ = fmt.Fprintf(out, "%s: archived to %d destinations\n", s.c.Name, written)
	return nil
}

// archiveStopped stops the containers the policy says to stop, archives
// every selected container into staging, and starts the ones it stopped
// again before returning, however it returns. A failure of one container
// is recorded in failures; only an error that ends the whole run is
// returned, with whatever was staged before it. reason marks the archives as
// taken before a change (ReasonNone for an ordinary backup).
func (a *AppdataService) archiveStopped(ctx context.Context, out io.Writer, selected []AppdataContainer, staging string, now time.Time, dests []Destination, reason Reason, key []byte, failures map[string]error) (staged []stagedAppdata, err error) {
	var toStop []AppdataContainer
	for _, c := range selected {
		if c.Stop && c.Running {
			toStop = append(toStop, c)
		}
	}
	var attempted []AppdataContainer
	rep := &anchorReport{out: out}
	if len(toStop) > 0 {
		names := make([]string, len(toStop))
		for i, c := range toStop {
			names[i] = c.Name
		}
		if err := a.journalAdd(names); err != nil {
			return nil, err
		}
		defer func() {
			if rerr := a.restartAll(ctx, out, attempted); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}()
	}

	for _, c := range toStop {
		if ctx.Err() != nil {
			break
		}
		attempted = append(attempted, c)
		_, _ = fmt.Fprintf(out, "stopping %s\n", c.Name)
		if _, err := a.Containers.Stop(ctx, c.Name); err != nil {
			failures[c.Name] = fmt.Errorf("stopping the container: %w", err)
		}
	}

	for _, c := range selected {
		if err := ctx.Err(); err != nil {
			return staged, err
		}
		if failures[c.Name] != nil {
			continue
		}
		name := resolveAppdataName(a.Backup.installationID(), c.Name, now, reason, dests)
		path := filepath.Join(staging, name)
		hdr := appdataHeader{
			Container: c.Name, Image: c.Image, CreatedAt: now, Hostname: a.Backup.Hostname,
			Stopped: c.Stop || !c.Running, DatabaseImage: c.DatabaseImage, Reason: string(reason), Dirs: c.Dirs,
		}
		_, _ = fmt.Fprintf(out, "archiving %s\n", c.Name)
		a.anchorDirs(rep, c.Dirs)
		if _, err := packAppdata(ctx, path, hdr, key); err != nil {
			if ctx.Err() != nil {
				return staged, ctx.Err()
			}
			failures[c.Name] = fmt.Errorf("archiving: %w", err)
			continue
		}
		staged = append(staged, stagedAppdata{c: c, name: name, path: path})
	}
	return staged, ctx.Err()
}

// restartAll starts cs again in reverse order. Each start has its own
// deadline and does not depend on ctx, so a cancelled run still starts
// what it stopped. Each container started is removed from the journal; one
// whose start failed stays in it, with whatever an earlier run left there,
// so RecoverStopped finishes the job. A container that no longer exists has
// nothing to start.
func (a *AppdataService) restartAll(ctx context.Context, out io.Writer, cs []AppdataContainer) error {
	if len(cs) == 0 {
		return nil
	}
	var done []string
	var errs []error
	for i := len(cs) - 1; i >= 0; i-- {
		c := cs[i]
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.startTimeout())
		_, err := a.Containers.Start(sctx, c.Name)
		cancel()
		switch {
		case err == nil:
			done = append(done, c.Name)
			_, _ = fmt.Fprintf(out, "started %s\n", c.Name)
		case errors.Is(err, container.ErrNotFound):
			done = append(done, c.Name)
		default:
			errs = append(errs, fmt.Errorf("starting %s again: %w", c.Name, err))
		}
	}
	if err := a.journalRemove(done); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
