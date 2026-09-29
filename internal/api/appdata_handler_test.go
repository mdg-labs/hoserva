package api_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

type appdataFixture struct {
	h       *api.Handler
	sched   *job.Scheduler
	fake    *container.FakeProvider
	svc     *backup.AppdataService
	stopped atomic.Bool
	appdata string
	pool    string
	failed  atomic.Int32
	// previewGate, when set before a preview is queued, holds the preview
	// job until it is closed.
	previewGate chan struct{}
	backupJobID uuid.UUID
}

func newAppdataFixture(t *testing.T) *appdataFixture {
	t.Helper()
	h, sched, reg := newTestHandler(t)
	base := t.TempDir()
	f := &appdataFixture{h: h, sched: sched, fake: container.NewFakeProvider(),
		appdata: filepath.Join(base, "cache", "appdata"), pool: filepath.Join(base, "pool")}
	for name, image := range map[string]string{"sonarr": "lscr.io/linuxserver/sonarr", "db": "postgres"} {
		dir := filepath.Join(f.appdata, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "data"), []byte(name+"-v1"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.fake.AddContainer(container.Container{
			ID: "id-" + name, Name: name, Image: image, State: "running",
			Mounts: []container.Mount{{Source: dir, Destination: "/config"}},
		})
	}
	life := &container.Lifecycle{Provider: f.fake, Halted: f.stopped.Load, StorageReady: func() bool { return true }}
	dests := &backup.FakeDestinationStore{}
	if err := dests.CreateDestination(context.Background(), backup.Destination{
		ID: backup.DefaultPoolID, Name: "Pool", Type: backup.TypeLocal, Path: f.pool, Enabled: true,
		Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	f.svc = &backup.AppdataService{
		Backup:      &backup.Service{Store: dests, Hostname: "test-host", Now: func() time.Time { return time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC) }},
		Containers:  backup.LifecycleContainers{Lifecycle: life},
		Roots:       func(context.Context) ([]string, error) { return []string{f.appdata}, nil },
		Policies:    &backup.FakeAppdataPolicyStore{},
		JournalPath: filepath.Join(base, "state", "journal.json"),
	}
	h.Appdata = f.svc
	reg.Register(job.TypeAppdataBackup, true, job.RunAppdataBackup(job.AppdataBackupDeps{
		Backup: func(ctx context.Context, containers []string, out io.Writer) error {
			return f.svc.Run(ctx, backup.AppdataRunRequest{Containers: containers}, out)
		},
		Failed: func(context.Context, error) { f.failed.Add(1) },
	}))
	reg.Register(job.TypeAppdataRestore, false, job.RunAppdataRestore(func(ctx context.Context, p job.AppdataRestoreParams, out io.Writer) error {
		return f.svc.Restore(ctx, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID}, out)
	}))
	reg.Register(job.TypeAppdataRestorePreview, true, job.RunAppdataRestorePreview(func(ctx context.Context, id string, p job.AppdataRestoreParams, out io.Writer) error {
		if gate := f.previewGate; gate != nil {
			<-gate
		}
		return f.svc.RunPreview(ctx, id, backup.AppdataRestoreRequest{Container: p.Container, Archive: p.Archive, DestinationID: p.DestinationID}, out)
	}))
	return f
}

func TestHandler_AppdataOperations_Return501WithoutAService(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	_, err := h.GetAppdataBackup(ctx)
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("GetAppdataBackup = %d %s, want 501 not_configured", st, code)
	}
	_, err = h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
	if st, _ := statusOf(h, err); st != 501 {
		t.Fatalf("StartAppdataBackup = %d, want 501", st)
	}
	_, err = h.RestoreAppdata(ctx, &apiv1.RestoreAppdataRequest{Container: "a", Archive: "x", DestinationId: "pool", Confirm: true})
	if st, _ := statusOf(h, err); st != 501 {
		t.Fatalf("RestoreAppdata = %d, want 501", st)
	}
}

func TestHandler_GetAndSetAppdataBackup_FlagAnOptedOutDatabaseImage(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()

	got, err := f.h.GetAppdataBackup(ctx)
	if err != nil {
		t.Fatalf("GetAppdataBackup: %v", err)
	}
	if len(got.Containers) != 2 || got.Containers[0].Name != "db" || got.Containers[1].Name != "sonarr" {
		t.Fatalf("containers = %+v, want db and sonarr", got.Containers)
	}
	for _, c := range got.Containers {
		if _, ok := c.Warning.Get(); !c.Stop || !c.Included || ok {
			t.Fatalf("default policy of %s = %+v", c.Name, c)
		}
	}
	if !got.Containers[0].DatabaseImage || got.Containers[1].DatabaseImage {
		t.Fatalf("database flags = %v, %v", got.Containers[0].DatabaseImage, got.Containers[1].DatabaseImage)
	}

	c, err := f.h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: false, Included: true},
		apiv1.SetAppdataBackupContainerParams{Name: "db"})
	if err != nil {
		t.Fatalf("SetAppdataBackupContainer: %v", err)
	}
	if w, ok := c.Warning.Get(); !ok || !strings.Contains(w, "database") {
		t.Fatalf("opting a database out of being stopped returned warning %v, %v", w, ok)
	}
	c, err = f.h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: false, Included: true},
		apiv1.SetAppdataBackupContainerParams{Name: "sonarr"})
	if _, ok := c.Warning.Get(); err != nil || ok {
		t.Fatalf("sonarr = %+v, %v; want no warning", c, err)
	}
	_, err = f.h.SetAppdataBackupContainer(ctx, &apiv1.SetAppdataBackupContainerRequest{Stop: true, Included: true},
		apiv1.SetAppdataBackupContainerParams{Name: "ghost"})
	if st, code := statusOf(f.h, err); st != 404 || code != "container_not_found" {
		t.Fatalf("unknown container = %d %s, want 404 container_not_found", st, code)
	}
}

func TestHandler_StartAppdataBackup_RunsAsAServiceJobAndWritesTheArchives(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()

	j, err := f.h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
	if err != nil {
		t.Fatalf("StartAppdataBackup: %v", err)
	}
	if j.Type != apiv1.JobTypeAppdataBackup || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want appdata_backup in the service class", j.Type, j.Class)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", done.Status, done.ErrorMessage)
	}
	archives, unavailable, err := f.svc.ListArchives(ctx, "")
	if err != nil || len(unavailable) != 0 || len(archives) != 2 {
		t.Fatalf("archives = %v, %v, %v; want one per container", archives, unavailable, err)
	}
	if f.failed.Load() != 0 {
		t.Fatal("a successful backup raised the failure alert")
	}
	for _, name := range []string{"db", "sonarr"} {
		c, _ := f.fake.Inspect(ctx, name)
		if c.State != "running" {
			t.Fatalf("%s = %s after the backup, want it started again", name, c.State)
		}
	}
}

func TestHandler_StartAppdataBackup_NamedContainerOnlyAndRefusals(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()

	j, err := f.h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"sonarr"}}))
	if err != nil {
		t.Fatalf("StartAppdataBackup: %v", err)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("status = %s (%s)", done.Status, done.ErrorMessage)
	}
	archives, _, _ := f.svc.ListArchives(ctx, "")
	if len(archives) != 1 || archives[0].Container != "sonarr" {
		t.Fatalf("archives = %+v, want sonarr's only", archives)
	}

	_, err = f.h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"ghost"}}))
	if st, code := statusOf(f.h, err); st != 404 || code != "container_not_found" {
		t.Fatalf("unknown container = %d %s, want 404 container_not_found", st, code)
	}
	f.stopped.Store(true)
	_, err = f.h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
	if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
		t.Fatalf("array stopped = %d %s, want 409 array_stopped", st, code)
	}
}

func TestHandler_StartAppdataBackup_FailureFailsTheJobAndAlerts(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	f.fake.FailOn("stop", "sonarr", errors.New("stuck"))

	j, err := f.h.StartAppdataBackup(ctx, apiv1.OptStartAppdataBackupRequest{})
	if err != nil {
		t.Fatalf("StartAppdataBackup: %v", err)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "sonarr") {
		t.Fatalf("job = %s %q, want failed naming sonarr", done.Status, done.ErrorMessage)
	}
	if f.failed.Load() != 1 {
		t.Fatalf("failure alert raised %d times, want once", f.failed.Load())
	}
}

func TestHandler_RestoreAppdata_NeedsConfirmAndReplacesTheAppdata(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	j, err := f.h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"sonarr"}}))
	if err != nil {
		t.Fatal(err)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("backup: %s %s", done.Status, done.ErrorMessage)
	}
	archives, err := f.h.ListAppdataArchives(ctx, apiv1.ListAppdataArchivesParams{Container: apiv1.NewOptString("sonarr")})
	if err != nil || len(archives.Archives) != 1 || len(archives.Unavailable) != 0 {
		t.Fatalf("ListAppdataArchives = %+v, %v", archives, err)
	}
	a := archives.Archives[0]
	live := filepath.Join(f.appdata, "sonarr", "data")
	if err := os.WriteFile(live, []byte("sonarr-broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &apiv1.RestoreAppdataRequest{Container: "sonarr", Archive: a.Name, DestinationId: a.DestinationId}

	_, err = f.h.RestoreAppdata(ctx, req)
	if st, code := statusOf(f.h, err); st != 400 || code != "confirmation_required" {
		t.Fatalf("without confirm = %d %s, want 400 confirmation_required", st, code)
	}
	if got, _ := os.ReadFile(live); string(got) != "sonarr-broken" {
		t.Fatalf("an unconfirmed restore changed the appdata: %q", got)
	}

	req.Confirm = true
	j, err = f.h.RestoreAppdata(ctx, req)
	if err != nil {
		t.Fatalf("RestoreAppdata: %v", err)
	}
	if j.Type != apiv1.JobTypeAppdataRestore || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want appdata_restore in the service class", j.Type, j.Class)
	}
	done := awaitJob(t, f.sched, j.ID.String())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("restore: %s %s", done.Status, done.ErrorMessage)
	}
	if got, _ := os.ReadFile(live); string(got) != "sonarr-v1" {
		t.Fatalf("appdata after the restore = %q, want sonarr-v1", got)
	}
	after, _ := f.h.ListAppdataArchives(ctx, apiv1.ListAppdataArchivesParams{Container: apiv1.NewOptString("sonarr")})
	var snapshots int
	for _, s := range after.Archives {
		if r, ok := s.Reason.Get(); ok && r == "pre-restore" {
			snapshots++
		}
	}
	if snapshots != 1 {
		t.Fatalf("archives after the restore = %+v, want one pre-restore snapshot", after.Archives)
	}
}

func TestHandler_RestoreAppdata_RefusesWhatItCannotRestore(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	req := &apiv1.RestoreAppdataRequest{Container: "sonarr", Archive: "hoserva-appdata-000000000000-sonarr-2026-01-01T00-00-00.tar.zst", DestinationId: backup.DefaultPoolID, Confirm: true}

	_, err := f.h.RestoreAppdata(ctx, req)
	if st, code := statusOf(f.h, err); st != 400 || code != "appdata_archive_invalid" {
		t.Fatalf("another installation's archive = %d %s, want 400 appdata_archive_invalid", st, code)
	}
	f.stopped.Store(true)
	_, err = f.h.RestoreAppdata(ctx, req)
	if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
		t.Fatalf("array stopped = %d %s, want 409 array_stopped", st, code)
	}
}

// backedUpSonarr backs sonarr up and returns its archive.
func (f *appdataFixture) backedUpSonarr(t *testing.T) apiv1.AppdataArchive {
	t.Helper()
	ctx := context.Background()
	j, err := f.h.StartAppdataBackup(ctx, apiv1.NewOptStartAppdataBackupRequest(apiv1.StartAppdataBackupRequest{Containers: []string{"sonarr"}}))
	if err != nil {
		t.Fatal(err)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("backup: %s %s", done.Status, done.ErrorMessage)
	}
	f.backupJobID = j.ID
	archives, err := f.h.ListAppdataArchives(ctx, apiv1.ListAppdataArchivesParams{Container: apiv1.NewOptString("sonarr")})
	if err != nil || len(archives.Archives) != 1 {
		t.Fatalf("ListAppdataArchives = %+v, %v", archives, err)
	}
	return archives.Archives[0]
}

func (f *appdataFixture) queuePreview(t *testing.T, a apiv1.AppdataArchive) *apiv1.Job {
	t.Helper()
	j, err := f.h.PreviewAppdataRestore(context.Background(), &apiv1.PreviewAppdataRestoreRequest{Container: "sonarr", Archive: a.Name, DestinationId: a.DestinationId})
	if err != nil {
		t.Fatalf("PreviewAppdataRestore: %v", err)
	}
	return j
}

func TestHandler_PreviewAppdataRestore_QueuesAServiceJobWhoseResultReportsWhatARestoreWouldOverwrite(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	a := f.backedUpSonarr(t)
	dir := filepath.Join(f.appdata, "sonarr")
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("sonarr-broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new"), []byte("live only"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := f.queuePreview(t, a)
	if j.Type != apiv1.JobTypeAppdataRestorePreview || j.Class != apiv1.JobClassService {
		t.Fatalf("job = %s/%s, want appdata_restore_preview in the service class", j.Type, j.Class)
	}
	if done := awaitJob(t, f.sched, j.ID.String()); done.Status != job.StatusSucceeded {
		t.Fatalf("preview job: %s %s", done.Status, done.ErrorMessage)
	}
	got, err := f.h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: j.ID})
	if err != nil {
		t.Fatalf("GetAppdataRestorePreview: %v", err)
	}
	if len(got.Directories) != 1 {
		t.Fatalf("directories = %+v", got.Directories)
	}
	d := got.Directories[0]
	if d.Directory != "sonarr" ||
		d.Replaced.Files != 1 || d.Replaced.Bytes != int64(len("sonarr-broken")) || len(d.Replaced.Sample) != 1 || d.Replaced.Sample[0] != "data" ||
		d.Removed.Files != 1 || d.Removed.Sample[0] != "new" ||
		d.Added.Files != 0 || d.Added.Sample == nil {
		t.Fatalf("directory = %+v", d)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data")); string(b) != "sonarr-broken" {
		t.Fatalf("the preview changed the appdata: %q", b)
	}
	if st, err := f.fake.Inspect(ctx, "sonarr"); err != nil || st.State != "running" {
		t.Fatalf("sonarr = %+v, %v after the preview, want running", st, err)
	}
	if left, _ := os.ReadDir(filepath.Dir(f.appdata)); len(left) != 1 {
		t.Fatalf("the preview left %v next to the appdata location, want only appdata", left)
	}
}

func TestHandler_PreviewAppdataRestore_RefusesWhatTheRestoreRefuses(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.PreviewAppdataRestore(context.Background(), &apiv1.PreviewAppdataRestoreRequest{Container: "a", Archive: "x", DestinationId: "pool"})
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("without a service = %d %s, want 501 not_configured", st, code)
	}

	f := newAppdataFixture(t)
	ctx := context.Background()
	req := &apiv1.PreviewAppdataRestoreRequest{Container: "sonarr", Archive: "hoserva-appdata-000000000000-sonarr-2026-01-01T00-00-00.tar.zst", DestinationId: backup.DefaultPoolID}
	_, err = f.h.PreviewAppdataRestore(ctx, req)
	if st, code := statusOf(f.h, err); st != 400 || code != "appdata_archive_invalid" {
		t.Fatalf("another installation's archive = %d %s, want 400 appdata_archive_invalid", st, code)
	}
	f.stopped.Store(true)
	_, err = f.h.PreviewAppdataRestore(ctx, req)
	if st, code := statusOf(f.h, err); st != 409 || code != "array_stopped" {
		t.Fatalf("array stopped = %d %s, want 409 array_stopped", st, code)
	}
}

func TestHandler_GetAppdataRestorePreview_SaysWhyThereIsNoResult(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	a := f.backedUpSonarr(t)
	get := func(id uuid.UUID) (int, string, string) {
		_, err := f.h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: id})
		if err == nil {
			return 200, "", ""
		}
		st, code := statusOf(f.h, err)
		return st, code, err.Error()
	}

	if st, code, _ := get(uuid.New()); st != 404 || code != "job_not_found" {
		t.Fatalf("unknown job = %d %s, want 404 job_not_found", st, code)
	}
	if st, code, _ := get(f.backupJobID); st != 404 || code != "job_not_found" {
		t.Fatalf("a backup job = %d %s, want 404 job_not_found", st, code)
	}

	f.previewGate = make(chan struct{})
	queued := f.queuePreview(t, a)
	if st, code, _ := get(queued.ID); st != 409 || code != "appdata_preview_not_ready" {
		t.Fatalf("a preview that has not finished = %d %s, want 409 appdata_preview_not_ready", st, code)
	}
	if err := os.Remove(filepath.Join(f.pool, a.Name)); err != nil {
		t.Fatal(err)
	}
	close(f.previewGate)
	if done := awaitJob(t, f.sched, queued.ID.String()); done.Status != job.StatusFailed {
		t.Fatalf("preview of a vanished archive = %s, want failed", done.Status)
	}
	if st, code, msg := get(queued.ID); st != 409 || code != "appdata_preview_failed" || !strings.Contains(msg, a.Name) {
		t.Fatalf("a failed preview = %d %s %q, want 409 appdata_preview_failed naming the archive", st, code, msg)
	}
}

func TestHandler_GetAppdataRestorePreview_HoldsOnlyTheNewestResults(t *testing.T) {
	f := newAppdataFixture(t)
	ctx := context.Background()
	a := f.backedUpSonarr(t)
	first := f.queuePreview(t, a)
	awaitJob(t, f.sched, first.ID.String())
	if _, err := f.h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: first.ID}); err != nil {
		t.Fatalf("the newest result: %v", err)
	}
	for i := 0; i < backup.AppdataPreviewKeep; i++ {
		awaitJob(t, f.sched, f.queuePreview(t, a).ID.String())
	}
	_, err := f.h.GetAppdataRestorePreview(ctx, apiv1.GetAppdataRestorePreviewParams{JobId: first.ID})
	if st, code := statusOf(f.h, err); st != 404 || code != "appdata_preview_gone" {
		t.Fatalf("the oldest result after %d newer ones = %d %s, want 404 appdata_preview_gone", backup.AppdataPreviewKeep, st, code)
	}
}
