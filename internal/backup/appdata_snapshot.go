package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mdg-labs/hoserva/internal/container"
)

// Snapshot is the appdata backup's single-container run for a change that is
// about to happen to the container (reason says which, ReasonPreUpdate for a
// container update): the same archive, verification and destinations as the
// ordinary backup, one archive of the one container, written whether or not
// its backup policy includes it. The container is stopped for the copy if its
// policy says so and started again before the upload, as in Run. It returns
// where the archive is, and fails unless some destination holds it: the
// caller does not change the container otherwise.
func (a *AppdataService) Snapshot(ctx context.Context, name string, reason Reason, out io.Writer) (container.SnapshotRef, error) {
	if err := a.lockRun(ctx); err != nil {
		return container.SnapshotRef{}, err
	}
	defer a.runMu.Unlock()
	if err := a.Containers.RequireArrayRunning(); err != nil {
		return container.SnapshotRef{}, err
	}
	roots, err := a.appdataRoots(ctx)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	if len(roots) == 0 {
		return container.SnapshotRef{}, errors.New("the array has no cache disk, so there is no appdata location to snapshot")
	}
	scope, err := a.Scope(ctx)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	var c *AppdataContainer
	for i := range scope {
		if scope[i].Name == name {
			c = &scope[i]
		}
	}
	if c == nil {
		return container.SnapshotRef{}, fmt.Errorf("%w: %s", ErrAppdataContainerNotFound, name)
	}
	dests, err := a.destinations(ctx)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	if len(dests) == 0 {
		return container.SnapshotRef{}, ErrAppdataNoDestination
	}
	passphrase, err := a.requireEncryption(ctx, dests)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	staging, err := appdataStaging(roots)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if w := c.Warning(); w != "" {
		_, _ = fmt.Fprintf(out, "warning: %s\n", w)
	}
	failures := map[string]error{}
	staged, err := a.archiveStopped(ctx, out, []AppdataContainer{*c}, staging, a.now(), dests, reason, failures)
	if err != nil {
		return container.SnapshotRef{}, err
	}
	if f := failures[name]; f != nil {
		return container.SnapshotRef{}, fmt.Errorf("snapshotting %s: %w", name, f)
	}
	if len(staged) != 1 {
		return container.SnapshotRef{}, fmt.Errorf("snapshotting %s: the archive was not written", name)
	}
	s := staged[0]
	if _, trailer, err := verifyAppdata(s.path); err != nil {
		return container.SnapshotRef{}, fmt.Errorf("verifying the snapshot of %s: %w", name, err)
	} else if trailer.Changed > 0 {
		_, _ = fmt.Fprintf(out, "warning: %s: %d files changed while they were copied\n", name, trailer.Changed)
	}
	written, uploadFailures := a.uploadAppdata(ctx, dests, s.path, s.name, name, passphrase, "", a.now())
	_ = os.Remove(s.path)
	if written == 0 {
		return container.SnapshotRef{}, fmt.Errorf("snapshotting %s: %w", name, errors.Join(uploadFailures...))
	}
	for _, f := range uploadFailures {
		_, _ = fmt.Fprintf(out, "warning: the snapshot did not reach every destination: %v\n", f)
	}
	return a.snapshotRef(ctx, name, s.name)
}

// snapshotRef finds the archive just written on a destination that lists it.
// An encrypted destination holds it under the name plus ".age".
func (a *AppdataService) snapshotRef(ctx context.Context, name, archive string) (container.SnapshotRef, error) {
	archives, _, err := a.ListArchives(ctx, name)
	if err != nil {
		return container.SnapshotRef{}, fmt.Errorf("finding the snapshot of %s on its destination: %w", name, err)
	}
	for _, found := range archives {
		if strings.TrimSuffix(found.Name, ".age") == archive {
			return container.SnapshotRef{Archive: found.Name, DestinationID: found.DestinationID}, nil
		}
	}
	return container.SnapshotRef{}, fmt.Errorf("the snapshot %s of %s was written but no destination lists it", archive, name)
}

// UpdateSnapshots is the container update's view of the appdata backup
// (container.AppdataSnapshots): it snapshots with ReasonPreUpdate and
// restores through AppdataService.Restore, which writes a snapshot of what it
// replaces first.
type UpdateSnapshots struct {
	Appdata *AppdataService
}

var _ container.AppdataSnapshots = UpdateSnapshots{}

func (s UpdateSnapshots) Scope(ctx context.Context, name string) (container.AppdataScope, error) {
	scope, err := s.Appdata.Scope(ctx)
	if err != nil {
		return container.AppdataScope{}, err
	}
	for _, c := range scope {
		if c.Name != name {
			continue
		}
		sharers, err := s.Appdata.RestoreSharers(ctx, name)
		if err != nil {
			return container.AppdataScope{}, err
		}
		return container.AppdataScope{Has: true, Sharers: sharers, Resources: []string{AppdataJobResource}}, nil
	}
	return container.AppdataScope{}, nil
}

func (s UpdateSnapshots) Snapshot(ctx context.Context, name string, out io.Writer) (container.SnapshotRef, error) {
	return s.Appdata.Snapshot(ctx, name, ReasonPreUpdate, out)
}

func (s UpdateSnapshots) Find(ctx context.Context, name string, ref container.SnapshotRef) error {
	return s.Appdata.FindArchive(ctx, name, ref.Archive, ref.DestinationID)
}

func (s UpdateSnapshots) Restore(ctx context.Context, name string, ref container.SnapshotRef, sharers []string, out io.Writer) error {
	return s.Appdata.Restore(ctx, AppdataRestoreRequest{Container: name, Archive: ref.Archive, DestinationID: ref.DestinationID, Sharers: sharers}, out)
}
