package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

// staticSecurity is the test double for apiv1.SecuritySource: mockapi's own
// securityHandler accepts any credential, so any well-formed one proves the
// round trip through the generated client and server.
type staticSecurity struct{}

func (staticSecurity) ApiToken(ctx context.Context, _ apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: "test-token"}, nil
}

func (staticSecurity) SessionCookie(ctx context.Context, _ apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

// newTestClient starts scenario's handler behind httptest and returns the
// generated Go client pointed at it — every assertion below goes through
// this client, never the handler directly, so a spec/handler mismatch shows
// up the same way it would for a real caller (D18).
func newTestClient(t *testing.T, scenario string) apiv1.Invoker {
	t.Helper()

	h, err := newHandler(scenario)
	if err != nil {
		t.Fatalf("newHandler(%q): %v", scenario, err)
	}
	srv, err := apiv1.NewServer(h, securityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	client, err := apiv1.NewClient(ts.URL+"/api/v1", staticSecurity{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// errorCode unwraps the client's own request-invocation wrapping (the
// generated Invoker wraps a non-2xx response's decode error before
// returning it) to reach the *apiv1.ErrorStatusCode the mock's NewError
// produced, then returns its doc-01-§5 error code.
func errorCode(t *testing.T, err error) string {
	t.Helper()
	var apiErr *apiv1.ErrorStatusCode
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *apiv1.ErrorStatusCode, got %T: %v", err, err)
	}
	return apiErr.Response.Code
}

func TestScenariosServeEveryOperation(t *testing.T) {
	for _, scenario := range fixtures.Scenarios {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			client := newTestClient(t, scenario)

			listed, err := client.ListJobs(ctx, apiv1.ListJobsParams{})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}

			// A job id ListJobs never returned always hits the same
			// job_not_found path, for every scenario including the empty
			// fresh-install one.
			if _, err := client.GetJob(ctx, apiv1.GetJobParams{JobId: uuid.New()}); err == nil {
				t.Fatal("GetJob(unknown id): expected an error")
			} else if code := errorCode(t, err); code != "job_not_found" {
				t.Fatalf("GetJob(unknown id): code = %q, want job_not_found", code)
			}

			if len(listed.Jobs) == 0 {
				if scenario != "fresh-install" {
					t.Fatalf("scenario %q: ListJobs returned no jobs", scenario)
				}
				return
			}

			for _, job := range listed.Jobs {
				got, err := client.GetJob(ctx, apiv1.GetJobParams{JobId: job.ID})
				if err != nil {
					t.Fatalf("GetJob(%s): %v", job.ID, err)
				}
				if got.ID != job.ID {
					t.Fatalf("GetJob(%s): id = %s", job.ID, got.ID)
				}

				log, err := client.GetJobLog(ctx, apiv1.GetJobLogParams{JobId: job.ID})
				if err != nil {
					t.Fatalf("GetJobLog(%s): %v", job.ID, err)
				}
				if n, err := io.Copy(io.Discard, log); err != nil {
					t.Fatalf("GetJobLog(%s): read: %v", job.ID, err)
				} else if n == 0 {
					t.Fatalf("GetJobLog(%s): empty body", job.ID)
				}
			}

			assertCancelAndResume(t, ctx, client, listed.Jobs)
		})
	}
}

// assertCancelAndResume exercises CancelJob and ResumeJob against every
// fixture job's own cancellable/resumable flags, so the mock's honesty
// about those flags (doc 01 §4) is what a UI or CLI would actually see.
func assertCancelAndResume(t *testing.T, ctx context.Context, client apiv1.Invoker, jobs []apiv1.Job) {
	t.Helper()

	for _, job := range jobs {
		job := job
		t.Run(job.ID.String()+"/cancel", func(t *testing.T) {
			res, err := client.CancelJob(ctx, apiv1.CancelJobParams{JobId: job.ID})
			if job.Cancellable {
				if err != nil {
					t.Fatalf("CancelJob: %v", err)
				}
				if res.Status != apiv1.JobStatusCancelled {
					t.Fatalf("CancelJob: status = %v, want cancelled", res.Status)
				}
			} else {
				if err == nil {
					t.Fatal("CancelJob: expected an error for a non-cancellable job")
				} else if code := errorCode(t, err); code != "job_not_cancellable" {
					t.Fatalf("CancelJob: code = %q, want job_not_cancellable", code)
				}
			}
		})

		t.Run(job.ID.String()+"/resume", func(t *testing.T) {
			res, err := client.ResumeJob(ctx, apiv1.ResumeJobParams{JobId: job.ID})
			if job.Resumable {
				if err != nil {
					t.Fatalf("ResumeJob: %v", err)
				}
				if res.Status != apiv1.JobStatusRunning {
					t.Fatalf("ResumeJob: status = %v, want running", res.Status)
				}
			} else {
				if err == nil {
					t.Fatal("ResumeJob: expected an error for a non-resumable job")
				} else if code := errorCode(t, err); code != "job_not_resumable" {
					t.Fatalf("ResumeJob: code = %q, want job_not_resumable", code)
				}
			}
		})
	}
}

func TestUnknownScenarioRejected(t *testing.T) {
	if _, err := newHandler("does-not-exist"); err == nil {
		t.Fatal("newHandler: expected an error for an unknown scenario")
	}
}

// newRawTestServer starts scenario's handler behind httptest without a
// generated client in front of it, so a test can send exactly the request
// bytes it wants — including one with no credential at all, which the
// generated apiv1.Invoker can never produce, since its SecuritySource
// always attaches one.
func newRawTestServer(t *testing.T, scenario string) *httptest.Server {
	t.Helper()

	h, err := newHandler(scenario)
	if err != nil {
		t.Fatalf("newHandler(%q): %v", scenario, err)
	}
	srv, err := apiv1.NewServer(h, securityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// TestNoCredentialGetsHonest401 is the regression test for the previous
// attempt's rejected finding: a request with neither an Authorization
// header nor a hoserva_session cookie never reaches securityHandler (it
// only runs once some credential is present), so this exercises NewError's
// own handling of the resulting *ogenerrors.SecurityError.
func TestNoCredentialGetsHonest401(t *testing.T) {
	ts := newRawTestServer(t, "healthy")

	resp, err := http.Get(ts.URL + "/api/v1/jobs")
	if err != nil {
		t.Fatalf("GET /jobs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var apiErr apiv1.Error
	if err := apiErr.UnmarshalJSON(body); err != nil {
		t.Fatalf("decode error body: %v\nbody: %s", err, body)
	}
	if apiErr.Code != unauthorizedCode {
		t.Fatalf("code = %q, want %q", apiErr.Code, unauthorizedCode)
	}
	if strings.Contains(strings.ToLower(apiErr.Message), "ogen") ||
		strings.Contains(strings.ToLower(apiErr.Message), "security requirement") {
		t.Fatalf("message leaks internal error text: %q", apiErr.Message)
	}
}

// TestSessionCookieOnlyIsAccepted is the counterpart: a request presenting
// only a hoserva_session cookie (no Authorization header) is exactly what
// a browser client sends, and must succeed like any other credential.
func TestSessionCookieOnlyIsAccepted(t *testing.T) {
	ts := newRawTestServer(t, "healthy")

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: "any-value"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /jobs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}
}

func TestCreateArray_RequiresMatchingConfirmation(t *testing.T) {
	client := newTestClient(t, "fresh-install")
	ctx := context.Background()

	disks := []apiv1.ArrayDiskAssignment{
		{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
		{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
	}

	_, err := client.CreateArray(ctx, &apiv1.CreateArrayRequest{Disks: disks, Confirmation: "erase /dev/sdb, /dev/sdc"})
	if err == nil {
		t.Fatal("CreateArray(wrong confirm): expected an error")
	}
	if code := errorCode(t, err); code != "confirmation_required" {
		t.Fatalf("CreateArray(wrong confirm): code = %q, want confirmation_required", code)
	}

	got, err := client.CreateArray(ctx, &apiv1.CreateArrayRequest{Disks: disks, Confirmation: "ERASE /dev/sdb, /dev/sdc"})
	if err != nil {
		t.Fatalf("CreateArray: %v", err)
	}
	if got.Type != apiv1.JobTypeDiskFormat || got.Class != apiv1.JobClassTopology {
		t.Fatalf("job type/class = %s/%s, want disk_format/topology", got.Type, got.Class)
	}
}

func TestCreateArray_RejectsASecondCacheDisk(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	disks := []apiv1.ArrayDiskAssignment{
		{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
		{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
		{Device: "/dev/sdd", Role: apiv1.ArrayDiskRoleCache, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
		{Device: "/dev/sde", Role: apiv1.ArrayDiskRoleCache, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
	}
	_, err := client.CreateArray(ctx, &apiv1.CreateArrayRequest{
		Disks:        disks,
		Confirmation: "ERASE /dev/sdb, /dev/sdc, /dev/sdd, /dev/sde",
	})
	if err == nil {
		t.Fatal("CreateArray(two cache disks): expected an error")
	}
	if code := errorCode(t, err); code != "invalid_plan" {
		t.Fatalf("CreateArray(two cache disks): code = %q, want invalid_plan", code)
	}
}

func TestCreateArray_RejectsNonXFSParity(t *testing.T) {
	client := newTestClient(t, "fresh-install")
	ctx := context.Background()

	disks := []apiv1.ArrayDiskAssignment{
		{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemExt4)},
		{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
	}
	_, err := client.CreateArray(ctx, &apiv1.CreateArrayRequest{
		Disks:        disks,
		Confirmation: "ERASE /dev/sdb, /dev/sdc",
	})
	if err == nil {
		t.Fatal("CreateArray(ext4 parity): expected an error")
	}
	if code := errorCode(t, err); code != "invalid_plan" {
		t.Fatalf("CreateArray(ext4 parity): code = %q, want invalid_plan", code)
	}
}

func TestUpdateGeneralSettings_RejectsUnknownTimezone(t *testing.T) {
	client := newTestClient(t, "fresh-install")
	ctx := context.Background()

	req := &apiv1.UpdateGeneralSettingsRequest{}
	req.SetTimezone(apiv1.NewOptString("Not/AZone"))
	_, err := client.UpdateGeneralSettings(ctx, req)
	if err == nil {
		t.Fatal("UpdateGeneralSettings(unknown timezone): expected an error")
	}
	if code := errorCode(t, err); code != "settings_invalid_input" {
		t.Fatalf("UpdateGeneralSettings(unknown timezone): code = %q, want settings_invalid_input", code)
	}
}

func TestArrayStopAndStartFlipsMaintenanceMode(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	before, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if before.MaintenanceMode.Or(false) {
		t.Fatal("healthy scenario starts in maintenanceMode")
	}

	_, err = client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: false})
	if err == nil {
		t.Fatal("StopArray(confirm=false): expected an error")
	}
	if code := errorCode(t, err); code != "confirmation_required" {
		t.Fatalf("StopArray(confirm=false): code = %q, want confirmation_required", code)
	}
	still, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after refused stop: %v", err)
	}
	if still.MaintenanceMode.Or(false) {
		t.Fatal("refused StopArray flipped maintenanceMode")
	}

	stopped, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true})
	if err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if !stopped.MaintenanceMode.Or(false) {
		t.Fatal("StopArray: maintenanceMode is false")
	}
	afterStop, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after stop: %v", err)
	}
	if !afterStop.MaintenanceMode.Or(false) {
		t.Fatal("GetStatus after StopArray: maintenanceMode is false")
	}

	started, err := client.StartArray(ctx)
	if err != nil {
		t.Fatalf("StartArray: %v", err)
	}
	if started.MaintenanceMode.Or(false) {
		t.Fatal("StartArray: maintenanceMode is still true")
	}
	afterStart, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus after start: %v", err)
	}
	if afterStart.MaintenanceMode.Or(false) {
		t.Fatal("GetStatus after StartArray: maintenanceMode is still true")
	}
}
