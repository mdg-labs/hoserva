package api_test

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
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

	h := &api.Handler{Scheduler: scheduler, Store: jobStore, Logs: logs}
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
// goroutine it woke up has finished persisting its own outcome.
func waitForStatus(t *testing.T, store *job.Store, jobID string, status job.Status) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
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
	syncJob, err := s.Submit(ctx, job.TypeSync, nil)
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
	j, err := s.Submit(ctx, job.TypeSync, nil)
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
	j, err := s.Submit(ctx, job.TypeSync, nil)
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
	j, err := s.Submit(ctx, job.TypeSync, nil)
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
