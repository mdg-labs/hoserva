package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

type memHistory struct {
	rows     []store.ImageHistory
	nextID   int64
	excluded map[string]bool
	days     int
	// failDelete and failMark script the matching call to fail.
	failDelete, failMark error
}

func newMemHistory() *memHistory {
	return &memHistory{excluded: map[string]bool{}, days: 7}
}

func (m *memHistory) InsertImageHistory(_ context.Context, h store.ImageHistory) (store.ImageHistory, error) {
	m.nextID++
	h.ID = m.nextID
	m.rows = append(m.rows, h)
	return h, nil
}

func (m *memHistory) LatestImageHistory(_ context.Context, container string) (store.ImageHistory, bool, error) {
	for i := len(m.rows) - 1; i >= 0; i-- {
		if m.rows[i].Container == container {
			return m.rows[i], true, nil
		}
	}
	return store.ImageHistory{}, false, nil
}

func (m *memHistory) ListImageHistory(context.Context) ([]store.ImageHistory, error) {
	out := make([]store.ImageHistory, len(m.rows))
	for i, r := range m.rows {
		out[len(m.rows)-1-i] = r
	}
	return out, nil
}

func (m *memHistory) MarkImageHistoryReverted(_ context.Context, id int64, at time.Time) error {
	if m.failMark != nil {
		return m.failMark
	}
	for i := range m.rows {
		if m.rows[i].ID == id {
			m.rows[i].RevertedAt = at
			return nil
		}
	}
	return store.ErrImageHistoryNotFound
}

func (m *memHistory) DeleteImageHistory(_ context.Context, id int64) error {
	if m.failDelete != nil {
		return m.failDelete
	}
	for i := range m.rows {
		if m.rows[i].ID == id {
			m.rows = append(m.rows[:i], m.rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *memHistory) SetBulkExcluded(_ context.Context, c string, excluded bool) error {
	m.excluded[c] = excluded
	return nil
}

func (m *memHistory) BulkExcluded(context.Context) ([]string, error) {
	out := []string{}
	for c, ex := range m.excluded {
		if ex {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *memHistory) ImageKeepDays(context.Context) (int, error) { return m.days, nil }

func (m *memHistory) SetImageKeepDays(_ context.Context, days int) error {
	m.days = days
	return nil
}

// fakeSnapshots scripts the appdata snapshot mechanism and records, with
// each call, which image-changing calls the provider had already seen, so a
// test can tell "before the update" from "after it".
type fakeSnapshots struct {
	provider *FakeProvider
	scope    AppdataScope
	archive  SnapshotRef

	snapshotErr, findErr, restoreErr error

	snapshots int
	restores  []string
	// providerCallsAtSnapshot is the provider's call log when Snapshot ran.
	providerCallsAtSnapshot []FakeCall
	providerCallsAtRestore  []FakeCall
	// stateAtRestore is the state of the container when Restore was called.
	stateAtRestore  string
	restoredSharers []string
}

func (f *fakeSnapshots) Scope(context.Context, string) (AppdataScope, error) { return f.scope, nil }

func (f *fakeSnapshots) Snapshot(_ context.Context, name string, _ io.Writer) (SnapshotRef, error) {
	f.providerCallsAtSnapshot = f.provider.Calls()
	f.snapshots++
	if f.snapshotErr != nil {
		return SnapshotRef{}, f.snapshotErr
	}
	return f.archive, nil
}

func (f *fakeSnapshots) Find(context.Context, string, SnapshotRef) error { return f.findErr }

// Restore behaves like internal/backup's: a running container is stopped for
// the swap and started again afterwards, whether or not the swap worked; a
// container that is already stopped is left stopped.
func (f *fakeSnapshots) Restore(ctx context.Context, name string, ref SnapshotRef, sharers []string, _ io.Writer) error {
	f.providerCallsAtRestore = f.provider.Calls()
	f.restoredSharers = sharers
	c, err := f.provider.Inspect(ctx, name)
	if err != nil {
		return err
	}
	f.stateAtRestore = c.State
	if c.State == "running" {
		if err := f.provider.Stop(ctx, name); err != nil {
			return err
		}
		defer func() { _ = f.provider.Start(ctx, name) }()
	}
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.restores = append(f.restores, ref.Archive)
	return nil
}

type fixedStatuses []ContainerUpdate

func (s fixedStatuses) Statuses(context.Context) ([]ContainerUpdate, error) { return s, nil }

const (
	jfRef      = "lscr.io/linuxserver/jellyfin:10.9.7"
	imgOld     = "sha256:old"
	imgNew     = "sha256:new"
	jfArchive  = "hoserva-appdata-0123456789ab-jellyfin-2026-10-01T08-00-00.pre-update.tar.zst"
	jfSnapshot = "pool"
)

type updaterRig struct {
	u         *Updater
	provider  *FakeProvider
	history   *memHistory
	snapshots *fakeSnapshots
	now       time.Time
	halted    bool
}

func newUpdaterRig() *updaterRig {
	r := &updaterRig{provider: NewFakeProvider(), history: newMemHistory(), now: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
	r.provider.AddContainer(Container{
		ID: "id-jf", Name: "jellyfin", Image: "lscr.io/linuxserver/jellyfin", Tag: "10.9.7",
		ImageID: imgOld, State: "running",
	})
	r.provider.AddImage(Image{ID: imgOld, RepoTags: []string{jfRef}})
	r.provider.AddImage(Image{ID: imgNew})
	r.provider.SetPull(jfRef, imgNew)
	r.snapshots = &fakeSnapshots{
		provider: r.provider,
		scope:    AppdataScope{Has: true, Resources: []string{"appdata"}},
		archive:  SnapshotRef{Archive: jfArchive, DestinationID: jfSnapshot},
	}
	r.u = &Updater{
		Lifecycle: &Lifecycle{Provider: r.provider, Halted: func() bool { return r.halted }, StorageReady: func() bool { return true }},
		History:   r.history,
		Snapshots: r.snapshots,
		Now:       func() time.Time { return r.now },
	}
	return r
}

func (r *updaterRig) update(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := r.u.Update(context.Background(), "jellyfin", &out)
	return out.String(), err
}

func (r *updaterRig) revert(t *testing.T, sharers ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := r.u.Revert(context.Background(), "jellyfin", sharers, &out)
	return out.String(), err
}

func (r *updaterRig) imageID(t *testing.T) string {
	t.Helper()
	c, err := r.provider.Inspect(context.Background(), "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	return c.ImageID
}

// tags is the reference -> image ID table as the Engine holds it.
func (r *updaterRig) tags(t *testing.T) map[string]string {
	t.Helper()
	images, err := r.provider.Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, img := range images {
		for _, tag := range img.RepoTags {
			out[tag] = img.ID
		}
	}
	return out
}

func (r *updaterRig) ops() []string {
	var out []string
	for _, c := range r.provider.Calls() {
		out = append(out, c.Op)
	}
	return out
}

// The data-loss scenario this part exists to prevent. The image is pulled (so
// the tag already points at the new image), the snapshot is taken, and then
// the swap fails. The container is still on its previous image, running the
// same data. If the update's record survived, a later revert would restore
// the snapshot over everything the container wrote since, for an update that
// never happened. The record, the kept tag and the moved tag must all be put
// back, and a revert must refuse without touching the appdata.
func TestUpdate_FailedSwapAfterThePullLeavesThePreviousImageAndNothingToRevert(t *testing.T) {
	r := newUpdaterRig()
	r.provider.FailOn("recreate-local", "jellyfin", errors.New("starting the replacement container: port is already allocated"))

	_, err := r.update(t)
	if err == nil || !strings.Contains(err.Error(), "it still runs its previous image") || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("Update error = %v, want the failure and that the container still runs its previous image", err)
	}
	if got := r.imageID(t); got != imgOld {
		t.Fatalf("container runs %s after the failed update, want its previous image %s", got, imgOld)
	}
	tags := r.tags(t)
	if tags[jfRef] != imgOld {
		t.Fatalf("%s points at %q after the failed update, want the previous image: the next recreate would otherwise run an image the container never did", jfRef, tags[jfRef])
	}
	if _, kept := tags[KeepRef(1)]; kept {
		t.Fatalf("the kept-image tag survived a failed update: %v", tags)
	}
	if len(r.history.rows) != 0 {
		t.Fatalf("update records after a failed update = %+v, want none", r.history.rows)
	}

	_, err = r.revert(t)
	if !errors.Is(err, ErrNothingToRevert) {
		t.Fatalf("Revert after a failed update = %v, want ErrNothingToRevert", err)
	}
	if len(r.snapshots.restores) != 0 || r.snapshots.providerCallsAtRestore != nil {
		t.Fatal("a revert after a failed update restored the snapshot over live appdata")
	}
}

// A failed snapshot means the update does not go ahead: the container is not
// replaced and nothing is recorded or kept, and the reference the pull moved
// is pointed back at the image the container runs.
func TestUpdate_FailedSnapshotChangesNothing(t *testing.T) {
	r := newUpdaterRig()
	r.snapshots.snapshotErr = errors.New("no destination can take appdata archives")

	_, err := r.update(t)
	if err == nil || !strings.Contains(err.Error(), "so it was not updated") || !strings.Contains(err.Error(), "no destination can take appdata archives") {
		t.Fatalf("Update error = %v, want the snapshot failure and that it was not updated", err)
	}
	if got := r.ops(); !reflect.DeepEqual(got, []string{"pull-image", "tag-image"}) {
		t.Fatalf("provider calls after a failed snapshot = %v, want the pull and the reference pointed back, no keep tag and no recreate", got)
	}
	if len(r.history.rows) != 0 || r.imageID(t) != imgOld || r.tags(t)[jfRef] != imgOld {
		t.Fatalf("a failed snapshot left rows %+v, image %s, tags %v", r.history.rows, r.imageID(t), r.tags(t))
	}
}

// A failed pull is the commonest way an update fails, and it must cost
// nothing: no snapshot (each one uses a pre-change archive slot a revertible
// update may be relying on), no downtime, no record.
func TestUpdate_FailedPullTakesNoSnapshotAndChangesNothing(t *testing.T) {
	r := newUpdaterRig()
	r.provider.FailOn("pull-image", jfRef, errors.New("manifest unknown"))

	if _, err := r.update(t); err == nil || !strings.Contains(err.Error(), "manifest unknown") || !strings.Contains(err.Error(), "so it was not updated") {
		t.Fatalf("Update error = %v, want the pull failure", err)
	}
	if r.snapshots.snapshots != 0 {
		t.Fatalf("snapshots = %d after a failed pull, want none", r.snapshots.snapshots)
	}
	if got := r.ops(); !reflect.DeepEqual(got, []string{"pull-image"}) {
		t.Fatalf("provider calls = %v, want only the pull", got)
	}
	if len(r.history.rows) != 0 || r.imageID(t) != imgOld || r.tags(t)[jfRef] != imgOld {
		t.Fatalf("a failed pull left rows %+v, image %s, tags %v", r.history.rows, r.imageID(t), r.tags(t))
	}
}

// When the update cannot be undone completely the error says what is left,
// and the leftover record cannot be used for a revert.
func TestUpdate_AFailedUndoIsReportedAndLeavesNothingRevertible(t *testing.T) {
	r := newUpdaterRig()
	r.provider.FailOn("recreate-local", "jellyfin", errors.New("cannot start"))
	r.provider.FailOn("untag-image", KeepRef(1), errors.New("engine busy"))

	_, err := r.update(t)
	if err == nil || !strings.Contains(err.Error(), "cannot start") || !strings.Contains(err.Error(), "engine busy") || !strings.Contains(err.Error(), "keep period") {
		t.Fatalf("Update error = %v, want both failures and that the leftovers stay until their keep period ends", err)
	}
	if len(r.history.rows) != 1 {
		t.Fatalf("records = %+v, want the one the tag still belongs to, left for the expiry to find", r.history.rows)
	}
	if err := r.u.CheckRevert(context.Background(), "jellyfin"); !errors.Is(err, ErrNothingToRevert) {
		t.Fatalf("CheckRevert with a leftover record = %v, want ErrNothingToRevert", err)
	}
	if _, err := r.revert(t); err == nil || len(r.snapshots.restores) != 0 {
		t.Fatalf("Revert = %v, restores %v, want a refusal that restored nothing", err, r.snapshots.restores)
	}
}

func TestUpdate_SnapshotsBeforeTheContainerChangesThenKeepsThePreviousImage(t *testing.T) {
	r := newUpdaterRig()
	out, err := r.update(t)
	if err != nil {
		t.Fatalf("Update: %v\n%s", err, out)
	}
	if r.snapshots.snapshots != 1 || len(r.snapshots.providerCallsAtSnapshot) != 1 || r.snapshots.providerCallsAtSnapshot[0].Op != "pull-image" {
		t.Fatalf("snapshots %d, provider calls at snapshot %v, want one snapshot after the pull and before anything else", r.snapshots.snapshots, r.snapshots.providerCallsAtSnapshot)
	}
	if got := r.ops(); !reflect.DeepEqual(got, []string{"pull-image", "tag-image", "recreate-local"}) {
		t.Fatalf("provider calls = %v, want the pull, the previous image tagged, then the recreate from the local image", got)
	}
	if got := r.imageID(t); got != imgNew {
		t.Fatalf("container runs %s, want the new image", got)
	}
	tags := r.tags(t)
	if tags[KeepRef(1)] != imgOld || tags[jfRef] != imgNew {
		t.Fatalf("tags = %v, want the previous image held under %s and %s on the new one", tags, KeepRef(1), jfRef)
	}
	want := store.ImageHistory{
		ID: 1, Container: "jellyfin", Image: jfRef, PreviousImageID: imgOld,
		SnapshotArchive: jfArchive, SnapshotDestination: jfSnapshot,
		UpdatedAt: r.now, KeepUntil: r.now.AddDate(0, 0, 7),
	}
	if !reflect.DeepEqual(r.history.rows, []store.ImageHistory{want}) {
		t.Fatalf("records = %+v, want %+v", r.history.rows, want)
	}
}

func TestUpdate_KeepsThePreviousImageForTheConfiguredPeriod(t *testing.T) {
	r := newUpdaterRig()
	r.history.days = 30
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	if got := r.history.rows[0].KeepUntil; !got.Equal(r.now.AddDate(0, 0, 30)) {
		t.Fatalf("KeepUntil = %v, want 30 days after the update", got)
	}
}

func TestUpdate_AContainerWithNoAppdataOnTheCacheIsNotSnapshotted(t *testing.T) {
	r := newUpdaterRig()
	r.snapshots.scope = AppdataScope{}
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	if r.snapshots.snapshots != 0 {
		t.Fatalf("snapshots = %d, want none for a container without appdata on the cache", r.snapshots.snapshots)
	}
	if row := r.history.rows[0]; row.SnapshotArchive != "" || row.SnapshotDestination != "" {
		t.Fatalf("record = %+v, want no snapshot", row)
	}
	if _, err := r.revert(t); err != nil || len(r.snapshots.restores) != 0 || r.imageID(t) != imgOld {
		t.Fatalf("Revert = %v, restores %v, image %s, want the image back and no restore", err, r.snapshots.restores, r.imageID(t))
	}
}

// A pull that finds no newer image changes nothing: no snapshot, no restart
// of the container, nothing kept — there is nothing to revert to.
func TestUpdate_NoNewerImageChangesNothing(t *testing.T) {
	r := newUpdaterRig()
	r.provider.SetPull(jfRef, imgOld)
	out, err := r.update(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "already runs the newest image") {
		t.Fatalf("output = %q, want it to say the container already runs the newest image", out)
	}
	if r.snapshots.snapshots != 0 || len(r.history.rows) != 0 {
		t.Fatalf("snapshots %d, records %+v, want none", r.snapshots.snapshots, r.history.rows)
	}
	if got := r.ops(); !reflect.DeepEqual(got, []string{"pull-image"}) {
		t.Fatalf("provider calls = %v, want only the pull: the container is not recreated for an image it already runs", got)
	}
}

func TestUpdate_Refusals(t *testing.T) {
	t.Run("array stopped", func(t *testing.T) {
		r := newUpdaterRig()
		r.halted = true
		_, err := r.update(t)
		if !errors.Is(err, ErrArrayStopped) || r.snapshots.snapshots != 0 || len(r.ops()) != 0 {
			t.Fatalf("Update with the array stopped = %v, snapshots %d, calls %v, want ErrArrayStopped and nothing done", err, r.snapshots.snapshots, r.ops())
		}
	})
	t.Run("pinned to a digest", func(t *testing.T) {
		r := newUpdaterRig()
		r.provider.RemoveContainer("id-jf")
		r.provider.AddContainer(Container{ID: "id-jf", Name: "jellyfin", Image: "lscr.io/linuxserver/jellyfin", ImageID: imgOld, Pinned: true, State: "running"})
		_, err := r.update(t)
		if !errors.Is(err, ErrUpdatePinned) || r.snapshots.snapshots != 0 {
			t.Fatalf("Update of a pinned container = %v, snapshots %d, want ErrUpdatePinned and no snapshot", err, r.snapshots.snapshots)
		}
	})
	t.Run("unknown container", func(t *testing.T) {
		r := newUpdaterRig()
		err := r.u.Update(context.Background(), "nope", io.Discard)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Update of an unknown container = %v, want ErrNotFound", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		if err := (&Updater{}).Update(context.Background(), "jellyfin", io.Discard); err == nil {
			t.Fatal("an Updater with no services updated a container")
		}
	})
}

// The data-loss scenario of a revert: an application that migrates its
// database when it starts must never see the restored pre-update data under
// the updated image, or it migrates the restored data again and the revert
// has not undone the update. The container is stopped before the snapshot is
// restored, left stopped through the swap to the previous image, and started
// once, on that image.
func TestRevert_RestoresTheSnapshotWhileStoppedThenStartsOnThePreviousImageOnce(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	callsBeforeRevert := len(r.provider.Calls())
	startsBeforeRevert := len(r.provider.Starts())
	out, err := r.revert(t, "transcoder")
	if err != nil {
		t.Fatalf("Revert: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(r.snapshots.restores, []string{jfArchive}) || !reflect.DeepEqual(r.snapshots.restoredSharers, []string{"transcoder"}) {
		t.Fatalf("restores %v with sharers %v, want the pre-update archive restored with the sharers it was given", r.snapshots.restores, r.snapshots.restoredSharers)
	}
	if r.snapshots.stateAtRestore == "running" {
		t.Fatal("the snapshot was restored while the container ran the updated image: it would be started on restored data it has not seen the pre-update version of")
	}
	if got := r.ops()[callsBeforeRevert:]; !reflect.DeepEqual(got, []string{"stop", "tag-image", "recreate-local", "untag-image", "start"}) {
		t.Fatalf("provider calls during the revert = %v, want stop, the reference moved, the recreate, the kept tag removed, then the one start", got)
	}
	starts := r.provider.Starts()[startsBeforeRevert:]
	if len(starts) != 1 || starts[0].ImageID != imgOld {
		t.Fatalf("starts during the revert = %+v, want exactly one, on the previous image %s", starts, imgOld)
	}
	if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State != "running" {
		t.Fatalf("container is %s after the revert, want it running again as it was", c.State)
	}
	if got := r.imageID(t); got != imgOld {
		t.Fatalf("container runs %s after the revert, want the previous image", got)
	}
	tags := r.tags(t)
	if tags[jfRef] != imgOld {
		t.Fatalf("%s points at %s, want the previous image", jfRef, tags[jfRef])
	}
	if _, kept := tags[KeepRef(1)]; kept {
		t.Fatalf("the kept-image tag outlived the revert: %v", tags)
	}
	if got := r.history.rows[0].RevertedAt; !got.Equal(r.now) {
		t.Fatalf("RevertedAt = %v, want the revert recorded", got)
	}

	if _, err := r.revert(t); !errors.Is(err, ErrNothingToRevert) || len(r.snapshots.restores) != 1 {
		t.Fatalf("a second revert = %v, restores %v, want ErrNothingToRevert and no second restore", err, r.snapshots.restores)
	}
}

// Everything that can refuse a revert is checked before the first change:
// a snapshot restored for an image that cannot come back would leave the
// data and the image of different versions.
func TestRevert_RefusesBeforeChangingAnythingWhenItCannotComplete(t *testing.T) {
	cases := map[string]struct {
		break_ func(r *updaterRig)
		want   error
	}{
		"kept image removed": {func(r *updaterRig) {
			if err := r.provider.UntagImage(context.Background(), KeepRef(1)); err != nil {
				t.Fatal(err)
			}
		}, ErrRevertUnavailable},
		"snapshot gone from its destination": {func(r *updaterRig) { r.snapshots.findErr = errors.New("no such appdata archive") }, ErrRevertUnavailable},
		"keep period over":                   {func(r *updaterRig) { r.now = r.now.AddDate(0, 0, 8) }, ErrRevertUnavailable},
		"container uses another reference": {func(r *updaterRig) {
			r.provider.RemoveContainer("id-jf")
			r.provider.AddContainer(Container{ID: "id-jf", Name: "jellyfin", Image: "lscr.io/linuxserver/jellyfin", Tag: "10.10.0", ImageID: imgNew, State: "running"})
		}, ErrNothingToRevert},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newUpdaterRig()
			if _, err := r.update(t); err != nil {
				t.Fatal(err)
			}
			tc.break_(r)
			before := r.provider.Calls()
			imageBefore := r.imageID(t)

			if _, err := r.revert(t); !errors.Is(err, tc.want) {
				t.Fatalf("Revert = %v, want %v", err, tc.want)
			}
			if err := r.u.CheckRevert(context.Background(), "jellyfin"); !errors.Is(err, tc.want) {
				t.Fatalf("CheckRevert = %v, want %v", err, tc.want)
			}
			if len(r.snapshots.restores) != 0 || r.snapshots.providerCallsAtRestore != nil {
				t.Fatal("a revert that could not complete restored the snapshot")
			}
			if !reflect.DeepEqual(r.provider.Calls()[:len(before)], before) || len(r.provider.Calls()) != len(before) || r.imageID(t) != imageBefore {
				t.Fatalf("a refused revert changed things: calls %v, image %s", r.provider.Calls(), r.imageID(t))
			}
			if !r.history.rows[0].RevertedAt.IsZero() {
				t.Fatal("a refused revert was recorded as done")
			}
		})
	}
}

// A restore that fails may already have replaced the appdata (it can fail
// afterwards, restarting a container that shared it), and nothing here can
// tell, so the container is not started on the updated image: it stays
// stopped, the image and reference are untouched, and reverting again retries.
func TestRevert_AFailedRestoreLeavesTheContainerStopped(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	startsBefore := len(r.provider.Starts())
	r.snapshots.restoreErr = errors.New("destination unreadable")
	_, err := r.revert(t)
	if err == nil || !strings.Contains(err.Error(), "destination unreadable") || !strings.Contains(err.Error(), "so it was not reverted") || !strings.Contains(err.Error(), "left stopped") {
		t.Fatalf("Revert = %v, want the restore failure, that it was not reverted and that the container is left stopped", err)
	}
	if r.imageID(t) != imgNew || r.tags(t)[jfRef] != imgNew || r.tags(t)[KeepRef(1)] != imgOld || !r.history.rows[0].RevertedAt.IsZero() {
		t.Fatalf("a failed restore changed the image: container %s, tags %v, records %+v", r.imageID(t), r.tags(t), r.history.rows)
	}
	if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State == "running" {
		t.Fatal("the container runs the updated image after a failed restore that may have replaced its appdata")
	}
	if got := r.provider.Starts()[startsBefore:]; len(got) != 0 {
		t.Fatalf("starts after a failed restore = %+v, want none", got)
	}

	r.snapshots.restoreErr = nil
	if _, err := r.revert(t); err != nil || r.imageID(t) != imgOld || r.history.rows[0].RevertedAt.IsZero() {
		t.Fatalf("retried Revert = %v, image %s, records %+v, want it finished", err, r.imageID(t), r.history.rows)
	}
}

// A container that cannot be stopped is untouched: the revert changes
// nothing.
func TestRevert_AContainerThatCannotBeStoppedChangesNothing(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	r.provider.FailOn("stop", "jellyfin", errors.New("cannot stop"))
	if _, err := r.revert(t); err == nil || !strings.Contains(err.Error(), "so it was not reverted") || len(r.snapshots.restores) != 0 || r.snapshots.providerCallsAtRestore != nil {
		t.Fatalf("Revert = %v, restores %v, want a refusal that restored nothing", err, r.snapshots.restores)
	}
	if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State != "running" || c.ImageID != imgNew {
		t.Fatalf("container %s on %s, want it running on the updated image as before", c.State, c.ImageID)
	}
}

// Once the snapshot is restored the container must not run again until it is
// on the previous image: if the swap then fails, it stays stopped, with the
// updated image it still has and the restored data, and reverting again
// finishes the job.
func TestRevert_AFailureAfterTheRestoreLeavesTheContainerStoppedAndCanBeRetried(t *testing.T) {
	cases := map[string]func(r *updaterRig){
		"the reference cannot be moved": func(r *updaterRig) {
			r.provider.FailOn("tag-image", jfRef, errors.New("engine busy"))
		},
		"the container cannot be replaced": func(r *updaterRig) {
			r.provider.FailOn("recreate-local", "jellyfin", errors.New("starting the replacement container: no such file"))
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			r := newUpdaterRig()
			if _, err := r.update(t); err != nil {
				t.Fatal(err)
			}
			startsBefore := len(r.provider.Starts())
			breakIt(r)
			_, err := r.revert(t)
			if err == nil || !strings.Contains(err.Error(), "revert again to finish, then start it") || !strings.Contains(err.Error(), "left stopped") {
				t.Fatalf("Revert = %v, want the failure, that the container is left stopped, and how to finish", err)
			}
			if got := r.provider.Starts()[startsBefore:]; len(got) != 0 {
				t.Fatalf("starts after a failed revert = %+v, want none: the updated image must not run against the restored data", got)
			}
			if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State == "running" || c.ImageID != imgNew {
				t.Fatalf("container %s on %s after a failed revert, want it stopped on the updated image", c.State, c.ImageID)
			}
			if !r.history.rows[0].RevertedAt.IsZero() || r.tags(t)[KeepRef(1)] != imgOld {
				t.Fatalf("records %+v, tags %v: want the record usable and the kept tag still there", r.history.rows, r.tags(t))
			}

			r.provider.FailOn("tag-image", jfRef, nil)
			r.provider.FailOn("recreate-local", "jellyfin", nil)
			if _, err := r.revert(t); err != nil {
				t.Fatalf("retried Revert: %v", err)
			}
			if got := r.provider.Starts()[startsBefore:]; len(got) != 0 {
				t.Fatalf("starts after the retry = %+v, want none: the retry cannot know the container ran, and the error told the operator to start it", got)
			}
			if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State == "running" || c.ImageID != imgOld {
				t.Fatalf("after the retry the container is %s on %s, want it stopped on the previous image", c.State, c.ImageID)
			}
			if len(r.snapshots.restores) != 2 || r.history.rows[0].RevertedAt.IsZero() {
				t.Fatalf("after the retry: restores %v, records %+v, want the snapshot restored again and the revert recorded", r.snapshots.restores, r.history.rows)
			}
		})
	}
}

// A container that was stopped stays stopped.
func TestRevert_AStoppedContainerStaysStopped(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	if err := r.provider.Stop(context.Background(), "jellyfin"); err != nil {
		t.Fatal(err)
	}
	startsBefore := len(r.provider.Starts())
	if _, err := r.revert(t); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.State == "running" || c.ImageID != imgOld {
		t.Fatalf("container %s on %s, want it stopped on the previous image", c.State, c.ImageID)
	}
	if got := r.provider.Starts()[startsBefore:]; len(got) != 0 {
		t.Fatalf("starts = %+v, want none for a container that was stopped", got)
	}
}

// The container is on the previous image and its revert is recorded; only
// starting it failed. That is reported, and the revert is not repeated.
func TestRevert_AFailedStartIsReportedAfterTheRevertIsRecorded(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	r.provider.FailOn("start", "jellyfin", errors.New("port is already allocated"))
	_, err := r.revert(t)
	if err == nil || !strings.Contains(err.Error(), "was reverted, but starting it failed") || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("Revert = %v, want the start failure reported as one after the revert", err)
	}
	if r.imageID(t) != imgOld || r.history.rows[0].RevertedAt.IsZero() {
		t.Fatalf("image %s, records %+v, want the previous image and the revert recorded", r.imageID(t), r.history.rows)
	}
}

// Two containers created from one reference: the first update moves the
// reference to the new image, after which the Engine lists the second under
// the image ID of the image it still runs. The second must be updated by the
// reference it was created with, not by that ID.
func TestUpdate_TwoContainersOnOneReferenceBothUpdate(t *testing.T) {
	r := newUpdaterRig()
	r.provider.AddContainer(Container{
		ID: "id-jf2", Name: "jellyfin2", Image: "lscr.io/linuxserver/jellyfin", Tag: "10.9.7",
		ImageID: imgOld, State: "running",
	})
	for _, name := range []string{"jellyfin", "jellyfin2"} {
		var out bytes.Buffer
		if err := r.u.Update(context.Background(), name, &out); err != nil {
			t.Fatalf("Update of %s: %v\n%s", name, err, out.String())
		}
	}
	for _, name := range []string{"jellyfin", "jellyfin2"} {
		if c, err := r.provider.Inspect(context.Background(), name); err != nil || c.ImageID != imgNew {
			t.Fatalf("%s runs %s (%v), want the new image", name, c.ImageID, err)
		}
	}
	for _, c := range r.provider.Calls() {
		if c.Op == "pull-image" && c.ID != jfRef {
			t.Fatalf("pulled %q, want only %s", c.ID, jfRef)
		}
	}
	if len(r.history.rows) != 2 {
		t.Fatalf("records = %+v, want one per container", r.history.rows)
	}
}

// A crash between the pull and the swap leaves the reference on the new image
// and the container on the old one. Updating again must go through, with its
// snapshot, rather than refuse because the container now lists under an image
// ID.
func TestUpdate_AfterAnInterruptedUpdateTheNextOneGoesThrough(t *testing.T) {
	r := newUpdaterRig()
	if err := r.provider.PullImage(context.Background(), jfRef); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.provider.Inspect(context.Background(), "jellyfin"); c.Image != "sha256" {
		t.Fatalf("test setup: the container lists as %s:%s, want the Engine's rewrite to an image ID", c.Image, c.Tag)
	}
	if err := r.u.Update(context.Background(), "jellyfin", io.Discard); err != nil {
		t.Fatalf("Update after an interrupted one: %v", err)
	}
	if r.snapshots.snapshots != 1 || r.imageID(t) != imgNew || len(r.history.rows) != 1 {
		t.Fatalf("snapshots %d, image %s, records %+v, want the update snapshotted, done and recorded", r.snapshots.snapshots, r.imageID(t), r.history.rows)
	}
}

// A crash between the revert's retag and its recreate leaves the reference
// on the previous image and the container on the updated one. The revert must
// still be offered, and finishes.
func TestRevert_AfterAnInterruptedRevertItFinishes(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	if err := r.provider.TagImage(context.Background(), imgOld, jfRef); err != nil {
		t.Fatal(err)
	}
	if err := r.u.CheckRevert(context.Background(), "jellyfin"); err != nil {
		t.Fatalf("CheckRevert after an interrupted revert = %v, want it still offered", err)
	}
	if _, err := r.revert(t); err != nil {
		t.Fatalf("Revert after an interrupted one: %v", err)
	}
	if r.imageID(t) != imgOld || r.history.rows[0].RevertedAt.IsZero() {
		t.Fatalf("image %s, records %+v, want the previous image and the revert recorded", r.imageID(t), r.history.rows)
	}
}

func TestRevert_ARecordThatCannotBeSavedIsReported(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	r.history.failMark = errors.New("database is locked")
	_, err := r.revert(t)
	if err == nil || !strings.Contains(err.Error(), "was reverted, but recording it failed") {
		t.Fatalf("Revert = %v, want the bookkeeping failure reported", err)
	}
	if r.imageID(t) != imgOld {
		t.Fatalf("container runs %s, want the previous image", r.imageID(t))
	}
	if _, err := r.revert(t); !errors.Is(err, ErrNothingToRevert) {
		t.Fatalf("Revert again = %v, want ErrNothingToRevert: the container already runs the previous image", err)
	}
}

func TestRevert_NoRecord(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.revert(t); !errors.Is(err, ErrNothingToRevert) {
		t.Fatalf("Revert of a container never updated = %v, want ErrNothingToRevert", err)
	}
}

func TestPrune(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}

	r.now = r.now.AddDate(0, 0, 6)
	if err := r.u.Prune(context.Background(), io.Discard); err != nil || len(r.history.rows) != 1 || r.tags(t)[KeepRef(1)] != imgOld {
		t.Fatalf("Prune inside the keep period = %v, records %d, tags %v, want nothing removed", err, len(r.history.rows), r.tags(t))
	}

	r.now = r.now.AddDate(0, 0, 2)
	if err := r.u.Prune(context.Background(), io.Discard); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, kept := r.tags(t)[KeepRef(1)]; kept || len(r.history.rows) != 0 {
		t.Fatalf("after the keep period: tags %v, records %+v, want the tag and the record gone", r.tags(t), r.history.rows)
	}
}

func TestPrune_AnImageAContainerStillRunsKeepsItsTagAndRecord(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	r.provider.AddContainer(Container{ID: "id-other", Name: "other", Image: "lscr.io/linuxserver/jellyfin", Tag: "10.9.7", ImageID: imgOld, State: "running"})
	r.now = r.now.AddDate(0, 0, 8)

	var out bytes.Buffer
	if err := r.u.Prune(context.Background(), &out); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if r.tags(t)[KeepRef(1)] != imgOld || len(r.history.rows) != 1 || !strings.Contains(out.String(), "still runs it") {
		t.Fatalf("tags %v, records %d, output %q: want the image still held and the reason said", r.tags(t), len(r.history.rows), out.String())
	}
}

func TestPrune_ARevertedRecordWhoseTagIsGoneIsDropped(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	if _, err := r.revert(t); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.AddDate(0, 0, 8)
	if err := r.u.Prune(context.Background(), io.Discard); err != nil || len(r.history.rows) != 0 {
		t.Fatalf("Prune = %v, records %+v, want the reverted record dropped", err, r.history.rows)
	}
}

func TestBulkTargets(t *testing.T) {
	r := newUpdaterRig()
	r.u.Statuses = fixedStatuses{
		{Container: "jellyfin", Status: store.UpdateAvailable},
		{Container: "nginx", Status: store.UpdateAvailable},
		{Container: "postgres", Status: store.UpdateAvailable},
		{Container: "redis", Status: store.UpdateUpToDate},
		{Container: "portainer", Status: store.UpdateSkipped},
		{Container: "grafana", Status: store.UpdateNotChecked},
		{Container: "traefik", Status: store.UpdateFailed},
	}
	if _, err := r.u.SetBulkExcluded(context.Background(), "jellyfin", true); err != nil {
		t.Fatal(err)
	}
	r.history.excluded["postgres"] = true

	got, err := r.u.BulkTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Containers, []string{"nginx"}) {
		t.Fatalf("targets = %v, want only the container with an update that has not opted out", got.Containers)
	}
	want := []BulkSkip{{"jellyfin", BulkReasonExcluded}, {"postgres", BulkReasonExcluded}}
	if !reflect.DeepEqual(got.Skipped, want) {
		t.Fatalf("skipped = %v, want %v", got.Skipped, want)
	}
}

func TestSetBulkExcluded_UnknownContainer(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.u.SetBulkExcluded(context.Background(), "nope", true); !errors.Is(err, ErrNotFound) || len(r.history.excluded) != 0 {
		t.Fatalf("SetBulkExcluded of an unknown container = %v, excluded %v, want ErrNotFound and nothing stored", err, r.history.excluded)
	}
}

func TestRecords_RevertibleOnlyForTheNewestUndoneUpdate(t *testing.T) {
	r := newUpdaterRig()
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	recs, err := r.u.Records(context.Background())
	if err != nil || len(recs) != 1 || !recs[0].Revertible {
		t.Fatalf("Records = %+v, %v, want the one update revertible", recs, err)
	}

	r.provider.AddImage(Image{ID: "sha256:newer"})
	r.provider.SetPull(jfRef, "sha256:newer")
	if _, err := r.update(t); err != nil {
		t.Fatal(err)
	}
	recs, err = r.u.Records(context.Background())
	if err != nil || len(recs) != 2 || !recs[0].Revertible || recs[1].Revertible {
		t.Fatalf("Records = %+v, %v, want only the newest update revertible", recs, err)
	}

	r.now = r.now.AddDate(0, 0, 8)
	recs, _ = r.u.Records(context.Background())
	if recs[0].Revertible {
		t.Fatalf("Records after the keep period = %+v, want none revertible", recs)
	}
}

func TestScopes(t *testing.T) {
	r := newUpdaterRig()
	r.snapshots.scope = AppdataScope{Has: true, Sharers: []string{"transcoder"}, Resources: []string{"appdata"}}

	got, err := r.u.UpdateScope(context.Background(), "jellyfin")
	if err != nil || !reflect.DeepEqual(got, []string{"container:jellyfin", "appdata"}) {
		t.Fatalf("UpdateScope = %v, %v", got, err)
	}
	resources, sharers, err := r.u.RevertScope(context.Background(), "jellyfin")
	if err != nil || !reflect.DeepEqual(resources, []string{"container:jellyfin", "appdata", "container:transcoder"}) || !reflect.DeepEqual(sharers, []string{"transcoder"}) {
		t.Fatalf("RevertScope = %v, %v, %v", resources, sharers, err)
	}
	if _, err := r.u.UpdateScope(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateScope of an unknown container = %v, want ErrNotFound", err)
	}
}
