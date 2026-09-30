package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newTestHandler wires a *api.Handler to a real SQLite database (through
// the same migration runner the daemon uses) and a fresh job.Scheduler —
// this package's tests never touch a fixture map, unlike cmd/mockapi's,
// because there is real logic here to exercise (#19).
func newTestHandler(t *testing.T) (*api.Handler, *job.Scheduler, *job.Registry) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "api-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	registry := job.NewRegistry()
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), registry)

	h := &api.Handler{
		Scheduler:          scheduler,
		Store:              jobStore,
		Logs:               logs,
		RelocationManifest: parity.NewRelocationManifestStore(db),
	}
	return h, scheduler, registry
}

func blockingRunFunc(started chan<- struct{}, release <-chan struct{}) job.RunFunc {
	return func(ctx context.Context, rc *job.RunContext) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForStatus polls until jobID reaches status, so a test doesn't
// return (and let its Cleanup close the database) before the job
// goroutine it woke up has finished persisting its own outcome. The
// deadline has to clear the test database's own busy_timeout(5000) (this
// file's newTestHandler) with real headroom, not race it — a 1s budget
// could lose to a store write that legitimately retries for up to 5s
// under load, matching dcb18bf's own 10s Drain budget for the same store.
func waitForStatus(t *testing.T, store *job.Store, jobID string, status job.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := store.Get(context.Background(), jobID)
		if err == nil && got.Status == status {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %s in time", jobID, status)
}

func apiError(t *testing.T, h *api.Handler, err error) *apiv1.ErrorStatusCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	return h.NewError(context.Background(), err)
}

func TestHandler_ListJobs_FiltersAndLimit(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	r.Register(job.TypeSync, false, blockingRunFunc(make(chan struct{}), make(chan struct{})))
	syncJob, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	got, err := h.ListJobs(ctx, apiv1.ListJobsParams{Class: apiv1.NewOptJobClass(apiv1.JobClassParity)})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].ID.String() != syncJob.ID {
		t.Fatalf("ListJobs(class=parity) = %+v, want just %s", got.Jobs, syncJob.ID)
	}

	none, err := h.ListJobs(ctx, apiv1.ListJobsParams{Class: apiv1.NewOptJobClass(apiv1.JobClassVM)})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(none.Jobs) != 0 {
		t.Fatalf("ListJobs(class=vm) = %+v, want empty", none.Jobs)
	}
}

func TestHandler_GetJob_NotFound(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	id := uuid.New()
	_, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "job_not_found" {
		t.Fatalf("GetJob(missing) error = %+v, want 404 job_not_found", status)
	}
}

func TestHandler_GetJob_Found(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	started := make(chan struct{})
	release := make(chan struct{})
	r.Register(job.TypeSync, false, blockingRunFunc(started, release))
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	id, err := uuid.Parse(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: id})
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != apiv1.JobStatusRunning {
		t.Fatalf("GetJob status = %s, want running", got.Status)
	}
	close(release)
	waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)
}

func TestHandler_CancelJob_NotCancellableRefuses(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	started := make(chan struct{})
	release := make(chan struct{})
	r.Register(job.TypeSync, false, blockingRunFunc(started, release))
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	id, err := uuid.Parse(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.CancelJob(ctx, apiv1.CancelJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "job_not_cancellable" {
		t.Fatalf("CancelJob(non-cancellable) error = %+v, want 409 job_not_cancellable", status)
	}
	close(release)
	waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)
}

func TestHandler_CancelJob_Cancellable(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	started := make(chan struct{})
	r.Register(job.TypeSync, true, func(ctx context.Context, rc *job.RunContext) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	id, err := uuid.Parse(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.CancelJob(ctx, apiv1.CancelJobParams{JobId: id})
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if got.ID != id {
		t.Fatalf("CancelJob returned job %s, want %s", got.ID, id)
	}
	waitForStatus(t, h.Store, j.ID, job.StatusCancelled)
}

func TestHandler_ResumeJob_UnregisteredTypeReports501(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	j := &job.Job{ID: uuid.New().String(), Type: job.TypeMover, Class: job.ClassArrayWrite, Status: job.StatusInterrupted, Resumable: true, CreatedAt: time.Now().UTC()}
	if err := h.Store.Create(ctx, j); err != nil {
		t.Fatalf("seeding job: %v", err)
	}

	id, _ := uuid.Parse(j.ID)
	_, err := h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 501 || status.Response.Code != "job_type_not_registered" {
		t.Fatalf("ResumeJob(unregistered type) error = %+v, want 501 job_type_not_registered", status)
	}
}

// TestHandler_ResumeJob_DatabaseRestoreHeldReports409 is #402's own
// handler-level regression test: while BeginDatabaseRestore's hold is
// held, ResumeJob must reach the caller as 409 database_restore_in_progress,
// never an opaque 500 — mapSchedulerError has to know job.
// ErrDatabaseRestoreInProgress for this to happen.
func TestHandler_ResumeJob_DatabaseRestoreHeldReports409(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeSync, true, blockingRunFunc(make(chan struct{}), make(chan struct{})))

	j := &job.Job{ID: uuid.New().String(), Type: job.TypeSync, Class: job.ClassParity, Status: job.StatusInterrupted, Resumable: true, Cancellable: true, CreatedAt: time.Now().UTC()}
	if err := h.Store.Create(ctx, j); err != nil {
		t.Fatalf("seeding job: %v", err)
	}

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore: %v", err)
	}
	defer release()

	id, _ := uuid.Parse(j.ID)
	_, err = h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "database_restore_in_progress" {
		t.Fatalf("ResumeJob during a database restore hold error = %+v, want 409 database_restore_in_progress", status)
	}
}

// TestHandler_ResumeJob_LiveRunnerReports409 is #402's own handler-level
// regression test for a row a restore overwrote or mislabelled back to
// interrupted while its runner is still going: ResumeJob must refuse with
// 409 job_already_running, never start a second concurrent run, and never
// surface as an opaque 500.
func TestHandler_ResumeJob_LiveRunnerReports409(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	started := make(chan struct{})
	release := make(chan struct{})
	r.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error {
		close(started)
		<-release
		return nil
	})
	j, err := s.Submit(ctx, job.TypeMover, []string{"diskA"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	// Stands in for a restore that overwrote this still-running job's row
	// back to interrupted (#402) while its runner keeps going.
	if err := h.Store.UpdateStatus(ctx, j.ID, job.StatusInterrupted, nil, "", "", j.StartedAt, nil); err != nil {
		t.Fatalf("simulating the restore's overwrite of the stored row: %v", err)
	}

	id, _ := uuid.Parse(j.ID)
	_, err = h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "job_already_running" {
		t.Fatalf("ResumeJob of a live runner error = %+v, want 409 job_already_running", status)
	}

	close(release)
	waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)
}

// TestHandler_CancelJob_DatabaseRestoreHeldReports409 is #402's own
// handler-level regression test for the scope update's Cancel direction:
// CancelJob during the hold must reach the caller as 409
// database_restore_in_progress, never an opaque 500.
func TestHandler_CancelJob_DatabaseRestoreHeldReports409(t *testing.T) {
	ctx := context.Background()
	h, s, _ := newTestHandler(t)

	j := &job.Job{ID: uuid.New().String(), Type: job.TypeMover, Class: job.ClassArrayWrite, Status: job.StatusInterrupted, Resumable: true, Cancellable: true, CreatedAt: time.Now().UTC()}
	if err := h.Store.Create(ctx, j); err != nil {
		t.Fatalf("seeding job: %v", err)
	}

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore: %v", err)
	}
	defer release()

	id, _ := uuid.Parse(j.ID)
	_, err = h.CancelJob(ctx, apiv1.CancelJobParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "database_restore_in_progress" {
		t.Fatalf("CancelJob during a database restore hold error = %+v, want 409 database_restore_in_progress", status)
	}
}

// TestHandler_StartSync_DatabaseRestoreHeldReports409 is #402's own
// handler-level regression test for every Submit-backed handler: StartSync
// during the hold must reach the caller as 409
// database_restore_in_progress, never an opaque 500 — every other
// Submit-backed handler shares the same mapSchedulerError call.
func TestHandler_StartSync_DatabaseRestoreHeldReports409(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeSync, true, blockingRunFunc(make(chan struct{}), make(chan struct{})))

	release, err := s.BeginDatabaseRestore(ctx)
	if err != nil {
		t.Fatalf("BeginDatabaseRestore: %v", err)
	}
	defer release()

	_, err = h.StartSync(ctx, &apiv1.StartSyncRequest{})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "database_restore_in_progress" {
		t.Fatalf("StartSync during a database restore hold error = %+v, want 409 database_restore_in_progress", status)
	}
}

func TestHandler_GetJobLog_NotFoundWhenNoLogWasCaptured(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	j := &job.Job{ID: uuid.New().String(), Type: job.TypeSync, Class: job.ClassParity, Status: job.StatusSucceeded, CreatedAt: time.Now().UTC()}
	if err := h.Store.Create(ctx, j); err != nil {
		t.Fatalf("seeding job: %v", err)
	}

	id, _ := uuid.Parse(j.ID)
	_, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "job_log_not_found" {
		t.Fatalf("GetJobLog(no captured log) error = %+v, want 404 job_log_not_found", status)
	}
}

func TestHandler_GetJobLog_ServesCapturedOutput(t *testing.T) {
	ctx := context.Background()
	h, _, _ := newTestHandler(t)

	j := &job.Job{ID: uuid.New().String(), Type: job.TypeSync, Class: job.ClassParity, Status: job.StatusSucceeded, CreatedAt: time.Now().UTC()}
	if err := h.Store.Create(ctx, j); err != nil {
		t.Fatalf("seeding job: %v", err)
	}

	w, err := h.Logs.Create(j.ID)
	if err != nil {
		t.Fatalf("Logs.Create: %v", err)
	}
	if _, err := w.Write([]byte("captured output")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	id, _ := uuid.Parse(j.ID)
	got, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id})
	if err != nil {
		t.Fatalf("GetJobLog: %v", err)
	}
	data, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("reading GetJobLogOK: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("GetJobLog returned no bytes")
	}
}

func TestHandler_NewError_UnclassifiedErrorMapsToInternal500(t *testing.T) {
	h, _, _ := newTestHandler(t)

	status := h.NewError(context.Background(), errUnclassified{})
	if status.StatusCode != 500 || status.Response.Code != "internal" {
		t.Fatalf("NewError(unclassified) = %+v, want 500 internal", status)
	}
}

type errUnclassified struct{}

func (errUnclassified) Error() string { return "boom" }

func TestHandler_GetJobLog_ServesOutputOfARunningJob(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	wrote := make(chan struct{})
	release := make(chan struct{})
	r.Register(job.TypeSync, false, func(ctx context.Context, rc *job.RunContext) error {
		if _, err := io.WriteString(rc.Output(), "syncing disk 1\n"); err != nil {
			return err
		}
		close(wrote)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	t.Cleanup(func() {
		close(release)
		waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)
	})

	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("job never wrote its output")
	}

	id, _ := uuid.Parse(j.ID)
	got, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id})
	if err != nil {
		t.Fatalf("GetJobLog: %v", err)
	}
	gz, err := gzip.NewReader(got)
	if err != nil {
		t.Fatalf("a running job's log must start a valid gzip stream: %v", err)
	}
	data, err := io.ReadAll(gz)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reading running job's log: %v", err)
	}
	if string(data) != "syncing disk 1\n" {
		t.Fatalf("GetJobLog of a running job = %q, want %q", data, "syncing disk 1\n")
	}
}

// A followed log must reach the client while the job still runs and must end
// with a complete gzip stream once the job finishes. The request goes through
// the generated server and FlushLogStream, as in hoservad.
func TestGeneratedServer_FollowedJobLogStreamsUntilTheJobFinishes(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)

	wrote := make(chan struct{})
	release := make(chan struct{})
	r.Register(job.TypeSync, false, func(ctx context.Context, rc *job.RunContext) error {
		if _, err := io.WriteString(rc.Output(), "syncing disk 1\n"); err != nil {
			return err
		}
		close(wrote)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(rc.Output(), "done\n")
		return err
	})
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var released bool
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(func() {
		releaseOnce()
		waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)
	})
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("job never wrote its output")
	}

	server, err := apiv1.NewServer(h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.FlushLogStream("/api/v1", server))
	defer srv.Close()

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/v1/jobs/"+j.ID+"/log?follow=true", nil)
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("a followed running job's log must start a valid gzip stream: %v", err)
	}
	first := make([]byte, len("syncing disk 1\n"))
	if _, err := io.ReadFull(gz, first); err != nil || string(first) != "syncing disk 1\n" {
		t.Fatalf("first read = %q, %v; want the output written so far while the job is still running", first, err)
	}

	releaseOnce()
	rest, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a finished job's followed log must end with a clean gzip trailer: %v", err)
	}
	if string(rest) != "done\n" {
		t.Fatalf("rest = %q, want %q", rest, "done\n")
	}
}

func TestHandler_GetJobLog_FollowOfAFinishedJobServesTheWholeLog(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	r.Register(job.TypeSync, false, func(ctx context.Context, rc *job.RunContext) error {
		_, err := io.WriteString(rc.Output(), "all done\n")
		return err
	})
	j, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForStatus(t, h.Store, j.ID, job.StatusSucceeded)

	id, _ := uuid.Parse(j.ID)
	got, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id, Follow: apiv1.NewOptBool(true)})
	if err != nil {
		t.Fatalf("GetJobLog(follow): %v", err)
	}
	gz, err := gzip.NewReader(got)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	data, err := io.ReadAll(gz)
	if err != nil || string(data) != "all done\n" {
		t.Fatalf("followed finished log = %q, %v; want %q", data, err, "all done\n")
	}
}

func TestHandler_GetJobLog_FollowOfAnUnknownJobIsNotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.GetJobLog(context.Background(), apiv1.GetJobLogParams{JobId: uuid.New(), Follow: apiv1.NewOptBool(true)})
	if status := apiError(t, h, err); status.StatusCode != 404 {
		t.Fatalf("status = %d, want 404", status.StatusCode)
	}
}

// submitInterruptedMover registers a mover whose first run writes "run one" and stops
// at a resumable checkpoint, and whose second run waits for secondRun to be
// closed, then writes "run two". It returns the interrupted job's id.
func submitInterruptedMover(t *testing.T, h *api.Handler, s *job.Scheduler, r *job.Registry, secondRun <-chan struct{}) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var runs atomic.Int32
	r.Register(job.TypeMover, true, func(ctx context.Context, rc *job.RunContext) error {
		if runs.Add(1) == 1 {
			if _, err := io.WriteString(rc.Output(), "run one\n"); err != nil {
				return err
			}
			return job.ErrJobNeedsRetry
		}
		select {
		case <-secondRun:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(rc.Output(), "run two\n")
		return err
	})
	j, err := s.Submit(ctx, job.TypeMover, []string{"diskA"}, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForStatus(t, h.Store, j.ID, job.StatusInterrupted)
	id, err := uuid.Parse(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHandler_GetJobLog_ServesBothRunsOfAResumedJob(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	secondRun := make(chan struct{})
	close(secondRun)
	id := submitInterruptedMover(t, h, s, r, secondRun)

	if _, err := h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id}); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	waitForStatus(t, h.Store, id.String(), job.StatusSucceeded)

	got, err := h.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: id})
	if err != nil {
		t.Fatalf("GetJobLog: %v", err)
	}
	gz, err := gzip.NewReader(got)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	data, err := io.ReadAll(gz)
	if err != nil || string(data) != "run one\nrun two\n" {
		t.Fatalf("GetJobLog of a resumed job = %q, %v; want both runs' output in order", data, err)
	}
}

func TestGeneratedServer_FollowedJobLogCrossesTheResumeOfAJob(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	secondRun := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(secondRun) }) }
	id := submitInterruptedMover(t, h, s, r, secondRun)
	t.Cleanup(func() {
		release()
		waitForStatus(t, h.Store, id.String(), job.StatusSucceeded)
	})

	if _, err := h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id}); err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}

	server, err := apiv1.NewServer(h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.FlushLogStream("/api/v1", server))
	defer srv.Close()

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/v1/jobs/"+id.String()+"/log?follow=true", nil)
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("a followed resumed job's log must start a valid gzip stream: %v", err)
	}
	first := make([]byte, len("run one\n"))
	if _, err := io.ReadFull(gz, first); err != nil || string(first) != "run one\n" {
		t.Fatalf("first read = %q, %v; want the interrupted run's output", first, err)
	}

	release()
	rest, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a finished job's followed log must end with a clean gzip trailer: %v", err)
	}
	if string(rest) != "run two\n" {
		t.Fatalf("rest = %q, want the resumed run's output %q", rest, "run two\n")
	}
}

func TestGeneratedServer_FollowOfAResumedJobWithADamagedLogSeesTheRepairedLog(t *testing.T) {
	ctx := context.Background()
	h, s, r := newTestHandler(t)
	secondRun := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(secondRun) }) }
	id := submitInterruptedMover(t, h, s, r, secondRun)

	logPath := filepath.Join(h.Logs.Dir, id.String()+".log.gz")
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(logPath, info.Size()-8); err != nil {
		t.Fatal(err)
	}

	blockerStarted, blockerRelease := make(chan struct{}), make(chan struct{})
	var blockerOnce sync.Once
	releaseBlocker := func() { blockerOnce.Do(func() { close(blockerRelease) }) }
	r.Register(job.TypeSync, false, blockingRunFunc(blockerStarted, blockerRelease))
	blocker, err := s.Submit(ctx, job.TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	t.Cleanup(func() {
		releaseBlocker()
		release()
		waitForStatus(t, h.Store, blocker.ID, job.StatusSucceeded)
		waitForStatus(t, h.Store, id.String(), job.StatusSucceeded)
	})
	<-blockerStarted

	resumed, err := h.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: id})
	if err != nil {
		t.Fatalf("ResumeJob: %v", err)
	}
	if resumed.Status != apiv1.JobStatusQueued {
		t.Fatalf("resumed job status = %q, want queued behind the conflicting job", resumed.Status)
	}
	queuedInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	queuedSize := queuedInfo.Size()

	server, err := apiv1.NewServer(h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.FlushLogStream("/api/v1", server))
	defer srv.Close()

	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodGet, srv.URL+"/api/v1/jobs/"+id.String()+"/log?follow=true", nil)
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	heldBack := make([]byte, queuedSize)
	if _, err := io.ReadFull(resp.Body, heldBack); err != nil {
		t.Fatalf("reading the log the follower opened while the job was queued: %v", err)
	}

	releaseBlocker()
	release()
	gz, err := gzip.NewReader(io.MultiReader(bytes.NewReader(heldBack), resp.Body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	all, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("a follower opened while the job was queued must read to a clean end: %v", err)
	}
	if string(all) != "run one\nrun two\n" {
		t.Fatalf("followed log = %q, want both runs' output in order", all)
	}
}
