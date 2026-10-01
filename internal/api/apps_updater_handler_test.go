package api_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// updaterSnapshots is the appdata snapshot mechanism the handler tests
// stand in for internal/backup's, recording what was restored.
type updaterSnapshots struct {
	has       bool
	snapErr   error
	snapshots int
	restores  []string
}

func (s *updaterSnapshots) Scope(context.Context, string) (container.AppdataScope, error) {
	return container.AppdataScope{Has: s.has, Resources: []string{"appdata"}}, nil
}

func (s *updaterSnapshots) Snapshot(context.Context, string, io.Writer) (container.SnapshotRef, error) {
	s.snapshots++
	if s.snapErr != nil {
		return container.SnapshotRef{}, s.snapErr
	}
	return container.SnapshotRef{Archive: "hoserva-appdata-0123456789ab-nginx-2026-10-01T08-00-00.pre-update.tar.zst", DestinationID: "pool"}, nil
}

func (s *updaterSnapshots) Find(context.Context, string, container.SnapshotRef) error { return nil }

func (s *updaterSnapshots) Restore(_ context.Context, _ string, ref container.SnapshotRef, _ []string, _ io.Writer) error {
	s.restores = append(s.restores, ref.Archive)
	return nil
}

type fixedUpdateStatuses []container.ContainerUpdate

func (s fixedUpdateStatuses) Statuses(context.Context) ([]container.ContainerUpdate, error) {
	return s, nil
}

type updaterFixture struct {
	*appsFixture
	updater   *container.Updater
	history   *store.ImageHistoryStore
	snapshots *updaterSnapshots
	statuses  *fixedUpdateStatuses
}

// newUpdaterFixture is the update execution wired the way hoservad wires it:
// the Updater on the handler and the container_update job running it, over a
// fake Docker with one running container, nginx, on an old image and a newer
// one to pull.
func newUpdaterFixture(t *testing.T) *updaterFixture {
	t.Helper()
	f := &updaterFixture{appsFixture: newAppsFixture(t), snapshots: &updaterSnapshots{has: true}}
	f.fake.AddContainer(container.Container{ID: "c2", Name: "nginx", Image: "nginx", Tag: "1.27", ImageID: "sha256:old", State: "running"})
	f.fake.AddContainer(container.Container{ID: "c3", Name: "redis", Image: "redis", Tag: "7", ImageID: "sha256:redis", State: "running"})
	f.fake.AddImage(container.Image{ID: "sha256:old", RepoTags: []string{"nginx:1.27"}})
	f.fake.AddImage(container.Image{ID: "sha256:new"})
	f.fake.AddImage(container.Image{ID: "sha256:redis", RepoTags: []string{"redis:7"}})
	f.fake.SetPull("nginx:1.27", "sha256:new")

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "updater-test.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.history = store.NewImageHistoryStore(db)
	f.statuses = &fixedUpdateStatuses{
		{Container: "nginx", Status: store.UpdateAvailable},
		{Container: "redis", Status: store.UpdateAvailable},
		{Container: "jellyfin", Status: store.UpdateUpToDate},
	}
	f.updater = &container.Updater{Lifecycle: f.h.Lifecycle, History: f.history, Snapshots: f.snapshots, Statuses: f.statuses}
	f.h.AppUpdater = f.updater
	f.reg.Register(job.TypeContainerUpdate, true, job.RunContainerUpdate(job.ContainerUpdateDeps{
		Update: f.updater.Update,
		Revert: f.updater.Revert,
	}))
	return f
}

func (f *updaterFixture) imageID(t *testing.T, name string) string {
	t.Helper()
	c, err := f.fake.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.ImageID
}

func TestHandler_UpdateApp_RunsAsAServiceJobThatUpdatesTheContainer(t *testing.T) {
	f := newUpdaterFixture(t)
	j, err := f.h.UpdateApp(context.Background(), apiv1.UpdateAppParams{ID: "nginx"})
	if err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	if j.Type != apiv1.JobTypeContainerUpdate || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want container_update in the service class", j.Type, j.Class)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	if got := f.imageID(t, "nginx"); got != "sha256:new" {
		t.Fatalf("nginx runs %s, want the new image", got)
	}
	if f.snapshots.snapshots != 1 {
		t.Fatalf("snapshots = %d, want one before the update", f.snapshots.snapshots)
	}
}

func TestHandler_UpdateApp_SnapshotFailureFailsTheJobAndChangesNothing(t *testing.T) {
	f := newUpdaterFixture(t)
	f.snapshots.snapErr = errors.New("no destination can take appdata archives")
	j, err := f.h.UpdateApp(context.Background(), apiv1.UpdateAppParams{ID: "nginx"})
	if err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "no destination can take appdata archives") {
		t.Fatalf("job = %s %q, want failed with the snapshot error", done.Status, done.ErrorMessage)
	}
	if got := f.imageID(t, "nginx"); got != "sha256:old" {
		t.Fatalf("nginx runs %s after a failed snapshot, want its previous image", got)
	}
}

func TestHandler_UpdateApp_Refusals(t *testing.T) {
	ctx := context.Background()

	t.Run("not configured", func(t *testing.T) {
		h, _, _ := newTestHandler(t)
		_, err := h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "nginx"})
		if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
			t.Fatalf("UpdateApp = %d %s, want 501 not_configured", st, code)
		}
	})
	t.Run("unknown container", func(t *testing.T) {
		f := newUpdaterFixture(t)
		_, err := f.h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "nope"})
		if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
			t.Fatalf("UpdateApp = %d %s, want 404 app_not_found", st, code)
		}
	})
	t.Run("array stopped", func(t *testing.T) {
		f := newUpdaterFixture(t)
		f.stopped.Store(true)
		_, err := f.h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "nginx"})
		if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
			t.Fatalf("UpdateApp = %d %s, want 409 array_stopped", st, code)
		}
	})
	t.Run("pinned", func(t *testing.T) {
		f := newUpdaterFixture(t)
		f.fake.AddContainer(container.Container{ID: "c4", Name: "pinned", Image: "nginx", ImageID: "sha256:old", Pinned: true, State: "running"})
		_, err := f.h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "pinned"})
		if st, code := statusOf(f.h, err); st != 409 || code != "app_pinned" {
			t.Fatalf("UpdateApp = %d %s, want 409 app_pinned", st, code)
		}
	})
}

func TestHandler_RevertApp_RestoresTheSnapshotAndThePreviousImage(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()

	_, err := f.h.RevertApp(ctx, apiv1.RevertAppParams{ID: "nginx"})
	if st, code := statusOf(f.h, err); st != 409 || code != "nothing_to_revert" {
		t.Fatalf("RevertApp before any update = %d %s, want 409 nothing_to_revert", st, code)
	}

	up, err := f.h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	if done := awaitJob(t, f.sched, up.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("update: %s (%s)", done.Status, done.ErrorMessage)
	}
	hist, err := f.h.ListAppUpdateHistory(ctx)
	if err != nil || !hist.Available || len(hist.Records) != 1 || !hist.Records[0].Revertible || !hist.Records[0].SnapshotArchive.Set {
		t.Fatalf("history = %+v, %v, want one revertible record with its snapshot", hist, err)
	}

	j, err := f.h.RevertApp(ctx, apiv1.RevertAppParams{ID: "nginx"})
	if err != nil {
		t.Fatalf("RevertApp: %v", err)
	}
	if j.Type != apiv1.JobTypeContainerUpdate || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want container_update in the service class", j.Type, j.Class)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("revert: %s (%s)", done.Status, done.ErrorMessage)
	}
	if got := f.imageID(t, "nginx"); got != "sha256:old" || len(f.snapshots.restores) != 1 {
		t.Fatalf("nginx runs %s, restores %v, want the previous image and the snapshot restored once", got, f.snapshots.restores)
	}
	hist, _ = f.h.ListAppUpdateHistory(ctx)
	if hist.Records[0].Revertible || !hist.Records[0].RevertedAt.Set {
		t.Fatalf("record after the revert = %+v, want it reverted and no longer revertible", hist.Records[0])
	}
	_, err = f.h.RevertApp(ctx, apiv1.RevertAppParams{ID: "nginx"})
	if st, code := statusOf(f.h, err); st != 409 || code != "nothing_to_revert" {
		t.Fatalf("a second RevertApp = %d %s, want 409 nothing_to_revert", st, code)
	}
}

func TestHandler_RevertApp_RefusesWithItsReasonWhenThePreviousImageIsGone(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()
	up, err := f.h.UpdateApp(ctx, apiv1.UpdateAppParams{ID: "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	awaitJob(t, f.sched, up.ID.String())
	if err := f.fake.UntagImage(ctx, container.KeepRef(1)); err != nil {
		t.Fatal(err)
	}
	jobsBefore, _ := f.h.Store.List(ctx, job.ListFilter{Limit: 50})

	_, err = f.h.RevertApp(ctx, apiv1.RevertAppParams{ID: "nginx"})
	if st, code := statusOf(f.h, err); st != 409 || code != "revert_unavailable" {
		t.Fatalf("RevertApp = %d %s, want 409 revert_unavailable", st, code)
	}
	if jobsAfter, _ := f.h.Store.List(ctx, job.ListFilter{Limit: 50}); len(jobsAfter) != len(jobsBefore) {
		t.Fatalf("a refused revert queued a job: %d jobs, was %d", len(jobsAfter), len(jobsBefore))
	}
	if len(f.snapshots.restores) != 0 {
		t.Fatal("a refused revert restored the snapshot")
	}
}

func TestHandler_StartAppUpdates_UpdatesTheContainersWithAnUpdateExceptThoseThatOptedOut(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()

	pol, err := f.h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "redis"})
	if err != nil || pol.Container != "redis" || !pol.BulkExcluded {
		t.Fatalf("SetAppUpdatePolicy = %+v, %v", pol, err)
	}
	_, err = f.h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "nope"})
	if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
		t.Fatalf("SetAppUpdatePolicy of an unknown container = %d %s, want 404", st, code)
	}

	got, err := f.h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
	if err != nil {
		t.Fatalf("StartAppUpdates: %v", err)
	}
	if !reflect.DeepEqual(got.Containers, []string{"nginx"}) || len(got.Skipped) != 1 || got.Skipped[0].Container != "redis" || got.Skipped[0].Reason != container.BulkReasonExcluded {
		t.Fatalf("StartAppUpdates = %+v, want nginx updated and redis skipped as excluded", got)
	}
	if !got.Job.Set || got.Job.Value.Type != apiv1.JobTypeContainerUpdate {
		t.Fatalf("job = %+v, want one container_update job", got.Job)
	}
	if done := awaitJob(t, f.sched, got.Job.Value.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("job = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	if f.imageID(t, "nginx") != "sha256:new" || f.imageID(t, "redis") != "sha256:redis" {
		t.Fatalf("nginx runs %s and redis %s, want only nginx updated", f.imageID(t, "nginx"), f.imageID(t, "redis"))
	}
}

// ListAppUpdates reads its container list from the Handler's AppUpdates; the
// opt-out flag is the part this operation adds.
func withUpdateChecker(f *updaterFixture) context.Context {
	f.h.AppUpdates = &container.UpdateChecker{Provider: f.fake, Results: noUpdateResults{}}
	return context.Background()
}

type noUpdateResults struct{}

func (noUpdateResults) PutImageUpdateCheck(context.Context, store.ImageUpdateCheck) error { return nil }
func (noUpdateResults) ListImageUpdateChecks(context.Context) ([]store.ImageUpdateCheck, error) {
	return nil, nil
}
func (noUpdateResults) DeleteImageUpdateCheck(context.Context, string) error { return nil }

func TestHandler_ListAppUpdates_ReportsTheBulkOptOut(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := withUpdateChecker(f)
	if _, err := f.h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "redis"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.h.ListAppUpdates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, u := range got.Updates {
		if !u.BulkExcluded.Set {
			t.Fatalf("%s has no bulkExcluded: the daemon always sets it", u.Container)
		}
		flags[u.Container] = u.BulkExcluded.Value
	}
	if !flags["redis"] || flags["nginx"] {
		t.Fatalf("bulkExcluded = %v, want only redis", flags)
	}
}

func TestHandler_StartAppUpdates_NamedContainersAreUpdatedWhetherOrNotTheyOptedOut(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()
	if _, err := f.h.SetAppUpdatePolicy(ctx, &apiv1.SetAppUpdatePolicyRequest{BulkExcluded: true}, apiv1.SetAppUpdatePolicyParams{ID: "nginx"}); err != nil {
		t.Fatal(err)
	}
	got, err := f.h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"nginx", "c2"}}))
	if err != nil || !reflect.DeepEqual(got.Containers, []string{"nginx"}) || len(got.Skipped) != 0 || !got.Job.Set {
		t.Fatalf("StartAppUpdates = %+v, %v, want nginx once, by name or ID, and nothing skipped", got, err)
	}
	awaitJob(t, f.sched, got.Job.Value.ID.String())
}

// One name that cannot be updated queues nothing, not even for the others.
func TestHandler_StartAppUpdates_RefusalsQueueNothing(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()
	_, err := f.h.StartAppUpdates(ctx, apiv1.NewOptStartAppUpdatesRequest(apiv1.StartAppUpdatesRequest{Containers: []string{"nginx", "nope"}}))
	if st, code := statusOf(f.h, err); st != 404 || code != "app_not_found" {
		t.Fatalf("StartAppUpdates with an unknown container = %d %s, want 404", st, code)
	}
	f.stopped.Store(true)
	_, err = f.h.StartAppUpdates(ctx, apiv1.OptStartAppUpdatesRequest{})
	if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
		t.Fatalf("StartAppUpdates with the array stopped = %d %s, want 409 array_stopped", st, code)
	}
	if jobs, _ := f.h.Store.List(ctx, job.ListFilter{Limit: 50}); len(jobs) != 0 {
		t.Fatalf("refused requests queued %d jobs", len(jobs))
	}
}

func TestHandler_StartAppUpdates_NothingToUpdateQueuesNoJob(t *testing.T) {
	f := newUpdaterFixture(t)
	*f.statuses = fixedUpdateStatuses{{Container: "nginx", Status: store.UpdateUpToDate}}
	got, err := f.h.StartAppUpdates(context.Background(), apiv1.OptStartAppUpdatesRequest{})
	if err != nil || got.Job.Set || len(got.Containers) != 0 || got.Skipped == nil {
		t.Fatalf("StartAppUpdates = %+v, %v, want no job and empty lists", got, err)
	}
}

func TestHandler_AppSettings(t *testing.T) {
	f := newUpdaterFixture(t)
	ctx := context.Background()
	got, err := f.h.GetAppSettings(ctx)
	if err != nil || got.ImageKeepDays != store.DefaultImageKeepDays {
		t.Fatalf("GetAppSettings = %+v, %v, want the default", got, err)
	}
	if got, err = f.h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: 30}); err != nil || got.ImageKeepDays != 30 {
		t.Fatalf("UpdateAppSettings = %+v, %v", got, err)
	}
	if got, _ = f.h.GetAppSettings(ctx); got.ImageKeepDays != 30 {
		t.Fatalf("GetAppSettings after the update = %+v", got)
	}
	for _, days := range []int{0, -3, 366} {
		_, err = f.h.UpdateAppSettings(ctx, &apiv1.AppSettings{ImageKeepDays: days})
		if st, code := statusOf(f.h, err); st != 400 || code != "invalid_image_keep_days" {
			t.Fatalf("UpdateAppSettings(%d) = %d %s, want 400 invalid_image_keep_days", days, st, code)
		}
	}
	if got, _ = f.h.GetAppSettings(ctx); got.ImageKeepDays != 30 {
		t.Fatalf("a refused period changed the setting to %d", got.ImageKeepDays)
	}

	h, _, _ := newTestHandler(t)
	if _, err := h.GetAppSettings(ctx); err == nil {
		t.Fatal("GetAppSettings with no updater succeeded")
	} else if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("GetAppSettings with no updater = %d %s, want 501 not_configured", st, code)
	}
}
