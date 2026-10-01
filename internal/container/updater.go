package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

// ErrUpdatePinned is returned by Update for a container created from an
// image digest: it runs exactly that image, so there is nothing newer to
// pull.
var ErrUpdatePinned = errors.New("container: the container is pinned to an image digest, so it cannot be updated")

// ErrNothingToRevert is returned by Revert, and by the check the API makes
// before queueing it, for a container with no update to undo: it was never
// updated by Hoserva, was already reverted, runs the previous image again,
// or now uses a different image reference.
var ErrNothingToRevert = errors.New("container: there is no update of this container to revert")

// ErrRevertUnavailable is returned by Revert when the update is on record
// but can no longer be undone: the previous image has been removed or its
// keep period has ended, or the pre-update snapshot is no longer on its
// destination. Nothing has been changed.
var ErrRevertUnavailable = errors.New("container: the update can no longer be reverted")

// UpdateHistory is the update record, the bulk-update opt-out and the keep
// period (store.ImageHistoryStore).
type UpdateHistory interface {
	InsertImageHistory(ctx context.Context, h store.ImageHistory) (store.ImageHistory, error)
	LatestImageHistory(ctx context.Context, container string) (store.ImageHistory, bool, error)
	ListImageHistory(ctx context.Context) ([]store.ImageHistory, error)
	MarkImageHistoryReverted(ctx context.Context, id int64, at time.Time) error
	MarkImageHistorySnapshotRestored(ctx context.Context, id int64, at time.Time) error
	DeleteImageHistory(ctx context.Context, id int64) error
	SetBulkExcluded(ctx context.Context, container string, excluded bool) error
	BulkExcluded(ctx context.Context) ([]string, error)
	ImageKeepDays(ctx context.Context) (int, error)
	SetImageKeepDays(ctx context.Context, days int) error
}

var _ UpdateHistory = (*store.ImageHistoryStore)(nil)

// SnapshotRef names a pre-update appdata archive on the destination that
// holds it.
type SnapshotRef struct {
	Archive       string
	DestinationID string
}

// AppdataScope says what an appdata snapshot or restore of one container
// touches.
type AppdataScope struct {
	// Has is whether the container has appdata on the cache disk, and so
	// is snapshotted before an update.
	Has bool
	// Sharers are the other containers whose appdata overlaps it: a
	// restore stops them with it, so they belong to its job's scope.
	Sharers []string
	// Resources are the scheduler resources a snapshot or restore of this
	// container must hold, beside the container's own.
	Resources []string
}

// AppdataSnapshots is the appdata snapshot mechanism internal/backup owns
// (doc 10 §2); an update calls it and does not archive anything itself.
type AppdataSnapshots interface {
	Scope(ctx context.Context, name string) (AppdataScope, error)
	// Snapshot archives the container's appdata with the container stopped
	// as its backup policy says, starts it again, and returns where the
	// archive is. It fails unless some destination holds it.
	Snapshot(ctx context.Context, name string, out io.Writer) (SnapshotRef, error)
	// Find reports whether the archive is still on its destination.
	Find(ctx context.Context, name string, ref SnapshotRef) error
	// Restore replaces the container's appdata with the archive's, after
	// writing a snapshot of what it replaces.
	Restore(ctx context.Context, name string, ref SnapshotRef, sharers []string, out io.Writer) error
}

// UpdateStatuses is the stored result of the daily update check.
type UpdateStatuses interface {
	Statuses(ctx context.Context) ([]ContainerUpdate, error)
}

var _ UpdateStatuses = (*UpdateChecker)(nil)

const (
	// keepRepository is where a kept image is tagged, so the Engine's own
	// cleanup of untagged images leaves it alone: "hoserva-previous:<id>".
	keepRepository = "hoserva-previous"
	// undoTimeout bounds the steps that put things back after a failure; it
	// does not depend on the request or job context, which a cancel may
	// have ended.
	undoTimeout = 2 * time.Minute
)

// KeepRef is the tag that holds the image of update record id.
func KeepRef(id int64) string {
	return fmt.Sprintf("%s:%d", keepRepository, id)
}

// Updater is doc 04 §6's update execution: a pre-update appdata snapshot,
// the update itself, the previous image kept for a revert, and the revert.
// A revert deletes nothing from the pool. It swaps the container back to a
// kept local image and restores an archive internal/backup wrote, which
// writes a snapshot of what it replaces first; Q14's two-phase rule is for
// array-to-array relocation and does not apply.
type Updater struct {
	Lifecycle *Lifecycle
	History   UpdateHistory
	Snapshots AppdataSnapshots
	Statuses  UpdateStatuses
	// Now reports the time stored with a record; nil means time.Now.
	Now func() time.Time
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) ready() error {
	if u.Lifecycle == nil || u.History == nil || u.Snapshots == nil {
		return errors.New("container: updates are not configured on this daemon")
	}
	return nil
}

func containerResource(name string) string {
	return "container:" + name
}

// UpdateScope is the scheduler resources an update of the container holds:
// the container and, when it has appdata on the cache disk, whatever the
// snapshot of that holds.
func (u *Updater) UpdateScope(ctx context.Context, name string) ([]string, error) {
	if err := u.ready(); err != nil {
		return nil, err
	}
	c, err := u.Lifecycle.Provider.Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	scope, err := u.Snapshots.Scope(ctx, c.Name)
	if err != nil {
		return nil, err
	}
	return append([]string{containerResource(c.Name)}, scope.Resources...), nil
}

// RevertScope is UpdateScope for a revert, which also holds every container
// sharing the appdata it restores. The sharers are returned apart: the
// restore is given them as the containers it may stop.
func (u *Updater) RevertScope(ctx context.Context, name string) (resources, sharers []string, err error) {
	if err := u.ready(); err != nil {
		return nil, nil, err
	}
	c, err := u.Lifecycle.Provider.Inspect(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	scope, err := u.Snapshots.Scope(ctx, c.Name)
	if err != nil {
		return nil, nil, err
	}
	resources = append([]string{containerResource(c.Name)}, scope.Resources...)
	for _, s := range scope.Sharers {
		resources = append(resources, containerResource(s))
	}
	return resources, scope.Sharers, nil
}

// CheckUpdate refuses, changing nothing, an update that Update would refuse
// before it changed anything, and returns the container; the API calls it
// before queueing the job.
func (u *Updater) CheckUpdate(ctx context.Context, name string) (Container, error) {
	c, _, err := u.checkUpdate(ctx, name)
	return c, err
}

// checkUpdate is CheckUpdate that also returns the reference the container
// was created with, which is what an update pulls and moves.
func (u *Updater) checkUpdate(ctx context.Context, name string) (Container, string, error) {
	if err := u.ready(); err != nil {
		return Container{}, "", err
	}
	if err := u.Lifecycle.RequireArrayRunning(); err != nil {
		return Container{}, "", err
	}
	c, err := u.Lifecycle.Provider.Inspect(ctx, name)
	if err != nil {
		return Container{}, "", err
	}
	img, err := u.Lifecycle.Provider.ConfiguredImage(ctx, c.ID)
	if err != nil {
		return Container{}, "", err
	}
	switch {
	case isRecreateTemp(c.Name):
		return Container{}, "", fmt.Errorf("container %q is a leftover of an interrupted recreate and cannot be updated", c.Name)
	case c.Pinned || img.Pinned:
		return Container{}, "", fmt.Errorf("%w: %s", ErrUpdatePinned, c.Name)
	case c.ImageID == "":
		return Container{}, "", fmt.Errorf("container %q reports no image, so its previous image cannot be kept for a revert", c.Name)
	}
	return c, img.Ref, nil
}

// Update brings the container to the image its reference names at the
// registry now. The image is pulled first, which changes nothing about the
// container: a failed pull, or one that brings nothing newer, ends the update
// with no snapshot taken (each snapshot uses one of the few pre-change
// archives a destination keeps), nothing stopped and nothing kept. Only then
// is the appdata snapshotted, when it is on the cache disk, the image the
// container runs held under a tag of its own, and the container replaced by
// one built from the pulled image without pulling again. Every way out
// before the replacement is done puts the pull's one change, the reference
// now pointing at the new image, back onto the image the container runs and
// removes the update's record and kept tag, so the container is left on its
// previous image and the error says so; a step that cannot be put back is
// named in the error, and its leftovers are cleaned up when their keep
// period ends.
func (u *Updater) Update(ctx context.Context, name string, out io.Writer) error {
	c, ref, err := u.checkUpdate(ctx, name)
	if err != nil {
		return err
	}
	days, err := u.History.ImageKeepDays(ctx)
	if err != nil {
		return err
	}
	if err := u.Prune(ctx, out); err != nil {
		_, _ = fmt.Fprintf(out, "warning: removing expired kept images: %v\n", err)
	}

	prov := u.Lifecycle.Provider
	_, _ = fmt.Fprintf(out, "pulling %s\n", ref)
	if err := prov.PullImage(ctx, ref); err != nil {
		return fmt.Errorf("pulling the image of %s failed, so it was not updated: %w", c.Name, err)
	}
	pulledID, err := u.imageOf(ctx, ref)
	if err != nil {
		return errors.Join(fmt.Errorf("finding the image pulled for %s, so it was not updated: %w", c.Name, err), u.undo(ctx, c, ref, nil))
	}
	if pulledID == c.ImageID {
		_, _ = fmt.Fprintf(out, "%s already runs the newest image of %s, so nothing was changed\n", c.Name, ref)
		return nil
	}

	scope, err := u.Snapshots.Scope(ctx, c.Name)
	if err != nil {
		return errors.Join(fmt.Errorf("finding the appdata of %s, so it was not updated: %w", c.Name, err), u.undo(ctx, c, ref, nil))
	}
	var snap SnapshotRef
	if scope.Has {
		_, _ = fmt.Fprintf(out, "snapshotting the appdata of %s\n", c.Name)
		if snap, err = u.Snapshots.Snapshot(ctx, c.Name, out); err != nil {
			return errors.Join(fmt.Errorf("the pre-update snapshot of %s failed, so it was not updated: %w", c.Name, err), u.undo(ctx, c, ref, nil))
		}
	}

	now := u.now()
	rec, err := u.History.InsertImageHistory(ctx, store.ImageHistory{
		Container: c.Name, Image: ref, PreviousImageID: c.ImageID,
		SnapshotArchive: snap.Archive, SnapshotDestination: snap.DestinationID,
		UpdatedAt: now, KeepUntil: now.Add(time.Duration(days) * 24 * time.Hour),
	})
	if err != nil {
		return errors.Join(fmt.Errorf("recording the update of %s, so it was not updated: %w", c.Name, err), u.undo(ctx, c, ref, nil))
	}
	if err := prov.TagImage(ctx, c.ImageID, KeepRef(rec.ID)); err != nil {
		return errors.Join(fmt.Errorf("keeping the image %s runs, so it was not updated: %w", c.Name, err), u.undo(ctx, c, ref, &rec))
	}

	_, _ = fmt.Fprintf(out, "replacing %s\n", c.Name)
	if _, err := u.Lifecycle.RecreateLocal(ctx, c.Name); err != nil {
		return u.failedUpdate(ctx, c, rec, ref, err)
	}
	_, _ = fmt.Fprintf(out, "%s updated; its previous image is kept for %d days, and the appdata snapshot is %s\n", c.Name, days, snapshotLabel(snap))
	return nil
}

// imageOf is the ID of the local image that holds the reference ref.
func (u *Updater) imageOf(ctx context.Context, ref string) (string, error) {
	images, err := u.Lifecycle.Provider.Images(ctx)
	if err != nil {
		return "", err
	}
	for _, img := range images {
		for _, t := range img.RepoTags {
			if t == ref {
				return img.ID, nil
			}
		}
	}
	return "", fmt.Errorf("%w: no local image holds %s after the pull", ErrImageNotFound, ref)
}

func snapshotLabel(s SnapshotRef) string {
	if s.Archive == "" {
		return "none (no appdata on the cache disk)"
	}
	return s.Archive
}

// failedUpdate reports a RecreateLocal that failed and puts back what the
// update changed. The container is left as RecreateLocal left it (the
// original, under its own name).
func (u *Updater) failedUpdate(ctx context.Context, c Container, rec store.ImageHistory, ref string, cause error) error {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	state := "its state could not be read afterwards"
	if now, err := u.Lifecycle.Provider.Inspect(undoCtx, c.Name); err == nil {
		if now.ImageID == c.ImageID {
			state = "it still runs its previous image"
		} else {
			state = "it does not run its previous image"
		}
	}
	return errors.Join(
		fmt.Errorf("updating %s failed and %s: %w", c.Name, state, cause),
		u.undo(ctx, c, ref, &rec),
	)
}

// undo puts back what an update that did not happen changed: the reference
// pointed at the image the container runs again, then, when the update got
// as far as a record, its kept-image tag and the record, in that order. The
// record goes last and stays when an earlier step fails, so the expiry of its
// keep period still finds the tag; a record left behind cannot be reverted,
// since the container runs the image it names, or its tag is gone.
func (u *Updater) undo(ctx context.Context, c Container, ref string, rec *store.ImageHistory) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	prov := u.Lifecycle.Provider
	if err := prov.TagImage(ctx, c.ImageID, ref); err != nil {
		if rec != nil {
			return fmt.Errorf("pointing %s back at the image %s runs failed, so the kept image %s and update record %d stay until their keep period ends: %w", ref, c.Name, KeepRef(rec.ID), rec.ID, err)
		}
		return fmt.Errorf("pointing %s back at the image %s runs failed, so it names the newly pulled image although %s still runs the old one: %w", ref, c.Name, c.Name, err)
	}
	if rec == nil {
		return nil
	}
	if err := prov.UntagImage(ctx, KeepRef(rec.ID)); err != nil && !errors.Is(err, ErrImageNotFound) {
		return fmt.Errorf("removing the kept image %s failed, so update record %d stays until its keep period ends: %w", KeepRef(rec.ID), rec.ID, err)
	}
	if err := u.History.DeleteImageHistory(ctx, rec.ID); err != nil {
		return fmt.Errorf("removing update record %d of %s: %w", rec.ID, c.Name, err)
	}
	return nil
}

// checkRevert is everything that decides a revert can go ahead, with
// nothing changed: the newest record of the container is unreverted and
// within its keep period, the container runs the reference and a different
// image than the record names, the kept image is still there, and so is the
// snapshot, unless an earlier revert restored it and the container that was in
// place then has not run since (restoreStands). The bool reports that case.
func (u *Updater) checkRevert(ctx context.Context, name string) (store.ImageHistory, Container, bool, error) {
	c, err := u.Lifecycle.Provider.Inspect(ctx, name)
	if err != nil {
		return store.ImageHistory{}, Container{}, false, err
	}
	h, ok, err := u.History.LatestImageHistory(ctx, c.Name)
	if err != nil {
		return store.ImageHistory{}, Container{}, false, err
	}
	var ref string
	if ok {
		img, err := u.Lifecycle.Provider.ConfiguredImage(ctx, c.ID)
		if err != nil {
			return h, c, false, err
		}
		ref = img.Ref
	}
	switch {
	case !ok:
		return h, c, false, fmt.Errorf("%w: %s has no recorded update", ErrNothingToRevert, c.Name)
	case !h.RevertedAt.IsZero():
		return h, c, false, fmt.Errorf("%w: its latest update was already reverted", ErrNothingToRevert)
	case c.ImageID == h.PreviousImageID:
		return h, c, false, fmt.Errorf("%w: %s already runs the previous image", ErrNothingToRevert, c.Name)
	case ref != h.Image:
		return h, c, false, fmt.Errorf("%w: %s now uses %s, not %s", ErrNothingToRevert, c.Name, ref, h.Image)
	case !u.now().Before(h.KeepUntil):
		return h, c, false, fmt.Errorf("%w: the previous image was only kept until %s", ErrRevertUnavailable, h.KeepUntil.UTC().Format(time.RFC3339))
	}
	images, err := u.Lifecycle.Provider.Images(ctx)
	if err != nil {
		return h, c, false, err
	}
	if !keepsImage(images, h) {
		return h, c, false, fmt.Errorf("%w: the previous image %s is no longer held on this host", ErrRevertUnavailable, h.PreviousImageID)
	}
	restored := false
	if h.SnapshotArchive != "" {
		restored = u.restoreStands(ctx, h, c)
		if !restored {
			if err := u.Snapshots.Find(ctx, c.Name, SnapshotRef{Archive: h.SnapshotArchive, DestinationID: h.SnapshotDestination}); err != nil {
				return h, c, false, fmt.Errorf("%w: the pre-update snapshot %s is gone: %w; %s", ErrRevertUnavailable, h.SnapshotArchive, err, goneRecovery(h, c.Name))
			}
		}
	}
	return h, c, restored, nil
}

// restoreStands reports whether the restore recorded on h still is what the
// appdata of c holds: it was recorded, c is not running, the Engine created
// c no later than the restore began, and did not start it after. A container
// that ran since may have written to the restored data with the updated
// image, so the record then says nothing. A recreation hides that: it makes a
// new container that reports no start even when the one it replaced ran, so
// only the container that was in place during the restore is covered, and a
// replacement created after it is not. A creation or start time that cannot
// be read says nothing either, since an error is not proof that the container
// did not run. The record's time has whole seconds, which can only make a
// time look later than it was, never earlier.
func (u *Updater) restoreStands(ctx context.Context, h store.ImageHistory, c Container) bool {
	if h.SnapshotRestoredAt.IsZero() || containerActive(c.State) {
		return false
	}
	created, err := u.Lifecycle.Provider.CreatedAt(ctx, c.ID)
	if err != nil || created.IsZero() || created.After(h.SnapshotRestoredAt) {
		return false
	}
	started, err := u.Lifecycle.Provider.StartedAt(ctx, c.ID)
	if err != nil {
		return false
	}
	return !started.After(h.SnapshotRestoredAt)
}

// goneRecovery is what the refusal says about the appdata of a container
// whose snapshot is gone: a restore of it may have run, so it names the
// pre-restore archive that restore wrote, which holds what the updated image
// had written before.
func goneRecovery(h store.ImageHistory, name string) string {
	if h.SnapshotRestoredAt.IsZero() {
		return fmt.Sprintf("if an earlier revert of this update began restoring it and was cut short, the appdata of %s may already be the pre-update content, and the pre-restore archive that revert took holds the appdata the updated image had written", name)
	}
	return fmt.Sprintf("an earlier revert of this update restored it, but %s has run or been recreated since, or whether it has could not be checked, so its appdata is no longer known to be that restored content; the pre-restore archive that revert took holds the appdata the updated image had written before it", name)
}

func keepsImage(images []Image, h store.ImageHistory) bool {
	keep := KeepRef(h.ID)
	for _, img := range images {
		if img.ID != h.PreviousImageID {
			continue
		}
		for _, t := range img.RepoTags {
			if t == keep {
				return true
			}
		}
	}
	return false
}

// CheckRevert refuses, changing nothing, a revert that Revert would refuse
// before it changed anything; the API calls it before queueing the job.
func (u *Updater) CheckRevert(ctx context.Context, name string) error {
	if err := u.ready(); err != nil {
		return err
	}
	if err := u.Lifecycle.RequireArrayRunning(); err != nil {
		return err
	}
	_, _, _, err := u.checkRevert(ctx, name)
	return err
}

// Revert puts the container back on the image it ran before its latest
// update and restores the appdata snapshot taken just before it. An
// application that migrates its data when it starts must see the restored
// data only under the previous image, so a running container is stopped
// before the snapshot is restored (the restore then leaves it stopped), the
// reference is pointed at the kept image and the container recreated from it
// without a pull while it is stopped, and only then is it started, once. The
// restore writes a snapshot of the appdata it replaces, so no write made
// since the update is lost without a copy. Once the container has been
// stopped, a failure leaves it stopped, whether the restore failed (which may
// have replaced the appdata before it did) or a later step did, because the
// updated image must not run against restored data; the record stays usable,
// and reverting again finishes it. A restore that completed is recorded on
// the update's record, with the time it began, and a retry then skips it and
// needs the snapshot no more, so it finishes even once a later archive has
// pruned the snapshot, but only while the appdata can still be that restored
// content: the container is not running, the Engine created it no later than
// the restore began (a recreation since made a new container, which reports no
// start even if the one it replaced ran), and has not started it since the
// restore began. Otherwise, or when its creation or start time cannot be read,
// or when the record could not be written, the retry takes the snapshot as not
// restored: it stops the container if it runs, restores the snapshot again,
// and refuses once the snapshot is gone. A retry that skipped the restore
// leaves the container stopped too, since it cannot tell that it ran before,
// and the error says to start it. A failure to stop the container
// changes nothing. A container that was stopped stays stopped. A container
// with no snapshot to restore is only recreated, which starts it if it ran.
// sharers are the other containers the restore may stop.
func (u *Updater) Revert(ctx context.Context, name string, sharers []string, out io.Writer) error {
	if err := u.ready(); err != nil {
		return err
	}
	if err := u.Lifecycle.RequireArrayRunning(); err != nil {
		return err
	}
	h, c, restored, err := u.checkRevert(ctx, name)
	if err != nil {
		return err
	}

	// stopped is set once this revert has stopped a running container, which
	// it alone then starts again.
	stopped := false
	if h.SnapshotArchive != "" && !restored {
		if containerActive(c.State) {
			_, _ = fmt.Fprintf(out, "stopping %s\n", c.Name)
			if _, err := u.Lifecycle.Stop(ctx, c.Name); err != nil {
				return fmt.Errorf("stopping %s failed, so it was not reverted: %w", c.Name, err)
			}
			stopped = true
		}
		_, _ = fmt.Fprintf(out, "restoring the appdata snapshot %s\n", h.SnapshotArchive)
		// Taken before the restore, so a start at any moment of it, the
		// container being stopped, shows up as later than the record.
		restoreBegan := u.now()
		if err := u.Snapshots.Restore(ctx, c.Name, SnapshotRef{Archive: h.SnapshotArchive, DestinationID: h.SnapshotDestination}, sharers, out); err != nil {
			return fmt.Errorf("restoring the pre-update snapshot of %s failed, so it was not reverted; it is left stopped, because the failed restore may already have replaced its appdata and the updated image must not run against that; revert again to finish, then start it: %w", c.Name, err)
		}
		restored = true
		u.recordRestored(ctx, h, restoreBegan, out)
	}

	left := "it still runs the updated image; revert again to finish"
	if restored {
		left = "it is left stopped, because the updated image must not run against the restored appdata; revert again to finish, then start it"
	}
	prov := u.Lifecycle.Provider
	if err := prov.TagImage(ctx, h.PreviousImageID, h.Image); err != nil {
		return fmt.Errorf("%s: pointing %s at the previous image failed, so %s: %w", c.Name, h.Image, left, err)
	}
	_, _ = fmt.Fprintf(out, "replacing %s with its previous image\n", c.Name)
	if _, err := u.Lifecycle.RecreateLocal(ctx, c.Name); err != nil {
		return fmt.Errorf("%s: replacing the container with its previous image failed, so %s: %w", c.Name, left, err)
	}

	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	var errs []error
	if err := u.History.MarkImageHistoryReverted(finish, h.ID, u.now()); err != nil {
		errs = append(errs, fmt.Errorf("%s was reverted, but recording it failed: %w", c.Name, err))
	} else if err := prov.UntagImage(finish, KeepRef(h.ID)); err != nil && !errors.Is(err, ErrImageNotFound) {
		_, _ = fmt.Fprintf(out, "warning: the kept image tag %s was not removed and goes when its keep period ends: %v\n", KeepRef(h.ID), err)
	}
	if stopped {
		_, _ = fmt.Fprintf(out, "starting %s\n", c.Name)
		if err := u.startAgain(ctx, c.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s was reverted, but starting it failed: %w", c.Name, err))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	_, _ = fmt.Fprintf(out, "%s reverted to its previous image\n", c.Name)
	return nil
}

// recordRestored marks the update's record as having its snapshot restored,
// the restore having begun at began, however the request or job context
// ended. The revert goes on when it cannot: only a retry needs the mark, and
// without it that retry restores the snapshot again or refuses once it is
// gone, never skips a restore.
func (u *Updater) recordRestored(ctx context.Context, h store.ImageHistory, began time.Time, out io.Writer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	if err := u.History.MarkImageHistorySnapshotRestored(ctx, h.ID, began); err != nil {
		_, _ = fmt.Fprintf(out, "warning: recording that the snapshot %s was restored failed, so a retry of a failed revert needs it still on its destination: %v\n", h.SnapshotArchive, err)
	}
}

// startAgain starts a container this revert stopped, however the request or
// job context ended.
func (u *Updater) startAgain(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), undoTimeout)
	defer cancel()
	if _, err := u.Lifecycle.Start(ctx, name); err != nil {
		return fmt.Errorf("starting %s again: %w", name, err)
	}
	return nil
}

// containerActive is whether a container in this Engine state is running or
// about to.
func containerActive(state string) bool {
	switch state {
	case "running", "restarting", "paused":
		return true
	}
	return false
}

// Prune removes the kept images whose keep period has ended, and their
// records. A record stays when its tag could not be removed, to be tried
// again, and an image a container still runs keeps its tag until none does.
// The kept images of a revert that already happened have no tag left to
// remove. A kept-image tag with no record at all, as after a config import or
// a restore replaced the database, is removed too, unless a container runs
// its image; when the images or containers cannot be listed, none is.
func (u *Updater) Prune(ctx context.Context, out io.Writer) error {
	if err := u.ready(); err != nil {
		return err
	}
	// The images are read before the records: an update records the image
	// before it tags it, so a tag seen here has its record in the read below.
	images, imagesErr := u.Lifecycle.Provider.Images(ctx)
	rows, err := u.History.ListImageHistory(ctx)
	if err != nil {
		return err
	}
	now := u.now()
	var errs []error
	for _, h := range rows {
		if now.Before(h.KeepUntil) {
			continue
		}
		err := u.Lifecycle.Provider.UntagImage(ctx, KeepRef(h.ID))
		switch {
		case err == nil, errors.Is(err, ErrImageNotFound):
		case errors.Is(err, ErrImageInUse):
			_, _ = fmt.Fprintf(out, "the previous image of %s is kept past its keep period: a container still runs it\n", h.Container)
			continue
		default:
			errs = append(errs, fmt.Errorf("removing the kept image of %s: %w", h.Container, err))
			continue
		}
		if err := u.History.DeleteImageHistory(ctx, h.ID); err != nil {
			errs = append(errs, err)
		}
	}
	if imagesErr != nil {
		errs = append(errs, fmt.Errorf("listing images to find kept images without a record: %w", imagesErr))
	} else if err := u.pruneOrphanedKeepTags(ctx, images, rows, out); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// pruneOrphanedKeepTags removes the kept-image tags no record names. Only a
// tag of exactly the form KeepRef makes counts as Hoserva's own.
func (u *Updater) pruneOrphanedKeepTags(ctx context.Context, images []Image, rows []store.ImageHistory, out io.Writer) error {
	recorded := make(map[int64]bool, len(rows))
	for _, h := range rows {
		recorded[h.ID] = true
	}
	type orphan struct{ ref, imageID string }
	var orphans []orphan
	for _, img := range images {
		for _, t := range img.RepoTags {
			id, ok := keepTagID(t)
			if ok && !recorded[id] {
				orphans = append(orphans, orphan{ref: t, imageID: img.ID})
			}
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	containers, err := u.Lifecycle.Provider.List(ctx)
	if err != nil {
		return fmt.Errorf("listing containers to find kept images without a record: %w", err)
	}
	running := make(map[string]bool, len(containers))
	for _, c := range containers {
		running[c.ImageID] = true
	}
	var errs []error
	for _, o := range orphans {
		if running[o.imageID] {
			_, _ = fmt.Fprintf(out, "%s has no update record but is kept: a container runs its image\n", o.ref)
			continue
		}
		err := u.Lifecycle.Provider.UntagImage(ctx, o.ref)
		switch {
		case err == nil:
			_, _ = fmt.Fprintf(out, "removed %s: it has no update record\n", o.ref)
		case errors.Is(err, ErrImageNotFound):
		case errors.Is(err, ErrImageInUse):
			_, _ = fmt.Fprintf(out, "%s has no update record but is kept: a container runs its image\n", o.ref)
		default:
			errs = append(errs, fmt.Errorf("removing %s, which has no update record: %w", o.ref, err))
		}
	}
	return errors.Join(errs...)
}

// keepTagID is the update record id a kept-image tag names, and false for any
// tag that is not exactly KeepRef of one.
func keepTagID(tag string) (int64, bool) {
	rest, ok := strings.CutPrefix(tag, keepRepository+":")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 || KeepRef(id) != tag {
		return 0, false
	}
	return id, true
}

// BulkSelection is what a bulk update would update and what it skips.
type BulkSelection struct {
	Containers []string
	Skipped    []BulkSkip
}

// BulkSkip is a container with an update available that a bulk update
// leaves alone, and why.
type BulkSkip struct {
	Container string
	Reason    string
}

// BulkReasonExcluded is the BulkSkip reason of a container that opted out.
const BulkReasonExcluded = "excluded from bulk updates"

// BulkTargets is every container the update check found a newer image for,
// except those that opted out. A container whose check could not tell is
// not a target: it is not known to have an update.
func (u *Updater) BulkTargets(ctx context.Context) (BulkSelection, error) {
	if err := u.ready(); err != nil {
		return BulkSelection{}, err
	}
	if u.Statuses == nil {
		return BulkSelection{}, errors.New("container: update checks are not configured on this daemon")
	}
	statuses, err := u.Statuses.Statuses(ctx)
	if err != nil {
		return BulkSelection{}, err
	}
	excluded, err := u.History.BulkExcluded(ctx)
	if err != nil {
		return BulkSelection{}, err
	}
	optedOut := make(map[string]bool, len(excluded))
	for _, n := range excluded {
		optedOut[n] = true
	}
	sel := BulkSelection{Containers: []string{}, Skipped: []BulkSkip{}}
	for _, s := range statuses {
		if s.Status != store.UpdateAvailable {
			continue
		}
		if optedOut[s.Container] {
			sel.Skipped = append(sel.Skipped, BulkSkip{Container: s.Container, Reason: BulkReasonExcluded})
			continue
		}
		sel.Containers = append(sel.Containers, s.Container)
	}
	sort.Strings(sel.Containers)
	return sel, nil
}

// SetBulkExcluded sets whether a bulk update skips the container, and
// returns ErrNotFound for one the Engine does not know. Updating it by name
// is still allowed.
func (u *Updater) SetBulkExcluded(ctx context.Context, name string, excluded bool) (string, error) {
	if err := u.ready(); err != nil {
		return "", err
	}
	c, err := u.Lifecycle.Provider.Inspect(ctx, name)
	if err != nil {
		return "", err
	}
	return c.Name, u.History.SetBulkExcluded(ctx, c.Name, excluded)
}

// BulkExcluded is the names of the containers a bulk update skips.
func (u *Updater) BulkExcluded(ctx context.Context) ([]string, error) {
	if u.History == nil {
		return nil, errors.New("container: updates are not configured on this daemon")
	}
	return u.History.BulkExcluded(ctx)
}

// KeepDays is how many days a previous image is kept for a revert.
func (u *Updater) KeepDays(ctx context.Context) (int, error) {
	if u.History == nil {
		return 0, errors.New("container: updates are not configured on this daemon")
	}
	return u.History.ImageKeepDays(ctx)
}

// SetKeepDays sets the keep period; it applies to updates made afterwards.
func (u *Updater) SetKeepDays(ctx context.Context, days int) error {
	if u.History == nil {
		return errors.New("container: updates are not configured on this daemon")
	}
	return u.History.SetImageKeepDays(ctx, days)
}

// UpdateRecord is one recorded update as the API reports it.
type UpdateRecord struct {
	store.ImageHistory
	// Revertible is whether a revert of this record would go ahead now,
	// short of the snapshot still being on its destination, which only a
	// revert itself looks up.
	Revertible bool
}

// Records lists every recorded update, newest first.
func (u *Updater) Records(ctx context.Context) ([]UpdateRecord, error) {
	if err := u.ready(); err != nil {
		return nil, err
	}
	rows, err := u.History.ListImageHistory(ctx)
	if err != nil {
		return nil, err
	}
	containers, err := u.Lifecycle.Provider.List(ctx)
	if err != nil {
		return nil, err
	}
	images, err := u.Lifecycle.Provider.Images(ctx)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]Container, len(containers))
	for _, c := range containers {
		byName[c.Name] = c
	}
	newest := map[string]int64{}
	for _, h := range rows {
		if h.ID > newest[h.Container] {
			newest[h.Container] = h.ID
		}
	}
	now := u.now()
	out := make([]UpdateRecord, 0, len(rows))
	for _, h := range rows {
		c, running := byName[h.Container]
		revertible := running && newest[h.Container] == h.ID && h.RevertedAt.IsZero() &&
			now.Before(h.KeepUntil) && c.ImageID != h.PreviousImageID && keepsImage(images, h)
		if revertible {
			img, err := u.Lifecycle.Provider.ConfiguredImage(ctx, c.ID)
			switch {
			case errors.Is(err, ErrNotFound):
				revertible = false
			case err != nil:
				return nil, err
			default:
				revertible = img.Ref == h.Image
			}
		}
		out = append(out, UpdateRecord{ImageHistory: h, Revertible: revertible})
	}
	return out, nil
}
