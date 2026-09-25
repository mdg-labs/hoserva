package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/store"
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

// TestScheduleUpdates_RefusedRequestChangesNothing proves a 400 from
// either schedule write leaves every field it named untouched — a valid
// field listed before the invalid one is not applied on its own. It calls
// the handler directly: ogen's own request decoding already refuses a
// weekday of 9 over HTTP, but the handler must not rely on that.
func TestScheduleUpdates_RefusedRequestChangesNothing(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatalf("newHandler(healthy): %v", err)
	}
	refused := func(op string, err error) {
		t.Helper()
		var me *mockError
		if !errors.As(err, &me) || me.code != "schedule_invalid_input" {
			t.Fatalf("%s = %v, want schedule_invalid_input", op, err)
		}
	}
	ctx := context.Background()

	before, err := h.GetSchedules(ctx)
	if err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}
	_, err = h.UpdateMaintenanceChainSchedule(ctx, &apiv1.UpdateMaintenanceChainScheduleRequest{
		StartTime:      apiv1.NewOptString("03:17"),
		WeeklyScrubDay: apiv1.NewOptWeekday(9),
	})
	refused("UpdateMaintenanceChainSchedule(valid time, bad day)", err)
	_, err = h.UpdateScheduledJob(ctx, &apiv1.UpdateScheduledJobRequest{
		Enabled:   apiv1.NewOptBool(false),
		Frequency: apiv1.NewOptScheduleFrequency("hourly"),
	}, apiv1.UpdateScheduledJobParams{JobId: apiv1.OtherScheduleJobIdSmartSelfTest})
	refused("UpdateScheduledJob(enabled, bad frequency)", err)
	after, err := h.GetSchedules(ctx)
	if err != nil {
		t.Fatalf("GetSchedules: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("schedules changed after two refused writes:\nbefore %+v\nafter  %+v", before, after)
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

// TestShares_RefusedInMaintenanceMode: production refuses create/update/
// deleteShare with maintenance_mode while the array is stopped (#333),
// and the mock mirrors that refusal rather than mutating shares that a
// real daemon would never change under bare mountpoints.
func TestShares_RefusedInMaintenanceMode(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	if _, err := client.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
		t.Fatalf("CreateShare(media) before stop: %v", err)
	}
	if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}

	_, err := client.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "hidden"})
	if err == nil {
		t.Fatal("CreateShare after StopArray: expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("CreateShare after StopArray: code = %q, want maintenance_mode", code)
	}

	_, err = client.UpdateShare(ctx, &apiv1.UpdateShareRequest{
		CreatePolicy: apiv1.NewOptArrayCreatePolicy(apiv1.ArrayCreatePolicyMfs),
	}, apiv1.UpdateShareParams{Name: "media"})
	if err == nil {
		t.Fatal("UpdateShare after StopArray: expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("UpdateShare after StopArray: code = %q, want maintenance_mode", code)
	}

	err = client.DeleteShare(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareParams{Name: "media"})
	if err == nil {
		t.Fatal("DeleteShare after StopArray: expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("DeleteShare after StopArray: code = %q, want maintenance_mode", code)
	}

	err = client.DeleteShareData(ctx, &apiv1.DeleteShareDataRequest{Confirmation: "media"}, apiv1.DeleteShareDataParams{Name: "media"})
	if err == nil {
		t.Fatal("DeleteShareData after StopArray: expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("DeleteShareData after StopArray: code = %q, want maintenance_mode", code)
	}

	err = client.DeleteShareFile(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareFileParams{Name: "media", Path: "movie.mkv"})
	if err == nil {
		t.Fatal("DeleteShareFile after StopArray: expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("DeleteShareFile after StopArray: code = %q, want maintenance_mode", code)
	}

	got, err := client.GetShare(ctx, apiv1.GetShareParams{Name: "media"})
	if err != nil {
		t.Fatalf("GetShare(media) after refused delete: %v", err)
	}
	if got.Name != "media" {
		t.Fatalf("GetShare after refused delete = %+v, want media still present", got)
	}
}

// TestUpgradeDisk_ParityRefusedInMaintenanceMode: production's
// Scheduler.Submit refuses a parity-disk upgrade with maintenance_mode
// while the array is stopped, and the mock mirrors that refusal rather
// than queuing a job production would never accept.
func TestUpgradeDisk_ParityRefusedInMaintenanceMode(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	plan, err := client.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{
		Mountpoint: "/mnt/parity",
		Device:     "/dev/sdf",
	})
	if err != nil {
		t.Fatalf("PlanDiskUpgrade(parity): %v", err)
	}
	newMountpoint, ok := plan.NewMountpoint.Get()
	if !ok || newMountpoint == "" {
		t.Fatal("PlanDiskUpgrade(parity): NewMountpoint is empty, want a fresh parity slot")
	}

	if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}

	_, err = client.UpgradeDisk(ctx, &apiv1.UpgradeDiskRequest{
		Mountpoint:    "/mnt/parity",
		Device:        "/dev/sdf",
		Confirmation:  plan.Confirmation,
		NewMountpoint: apiv1.NewOptString(newMountpoint),
	})
	if err == nil {
		t.Fatal("UpgradeDisk(parity, maintenance mode): expected an error")
	}
	if code := errorCode(t, err); code != "maintenance_mode" {
		t.Fatalf("UpgradeDisk(parity, maintenance mode): code = %q, want maintenance_mode", code)
	}
}

// TestJobSubmission_RefusedInMaintenanceMode proves that every mockapi
// operation submitting a job mirrors production's Scheduler.Submit
// (#330): a maintenance-mode refusal (409 maintenance_mode) once StopArray
// has completed, for every job type but TypeDiskUpgradeData (covered
// separately by TestUpgradeDisk_ParityRefusedInMaintenanceMode and
// production's own data-disk upgrade exception, #289's Q70/Q71).
func TestJobSubmission_RefusedInMaintenanceMode(t *testing.T) {
	ctx := context.Background()

	t.Run("AddDisk", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		plan, err := client.PlanDiskAdd(ctx, &apiv1.AddDiskPlanRequest{Device: "/dev/sdf"})
		if err != nil {
			t.Fatalf("PlanDiskAdd: %v", err)
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err = client.AddDisk(ctx, &apiv1.AddDiskRequest{Device: "/dev/sdf", Confirmation: plan.Confirmation})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("AddDisk after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("ReplaceDisk", func(t *testing.T) {
		// degraded's disk4 is the one slot with no matching inventory
		// entry (mockArrayDisks), so it is the only one
		// ConfirmReplacementTargetAbsent lets past slot_disk_present.
		client := newTestClient(t, "degraded")
		plan, err := client.PlanDiskReplace(ctx, &apiv1.ReplaceDiskPlanRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf"})
		if err != nil {
			t.Fatalf("PlanDiskReplace: %v", err)
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err = client.ReplaceDisk(ctx, &apiv1.ReplaceDiskRequest{Mountpoint: "/mnt/disk4", Device: "/dev/sdf", Confirmation: plan.Confirmation})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("ReplaceDisk after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartSync", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.StartSync(ctx, &apiv1.StartSyncRequest{})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartSync after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartScrub", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.StartScrub(ctx, &apiv1.StartScrubRequest{})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartScrub after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartFix", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.StartFix(ctx, &apiv1.StartFixRequest{Confirm: true})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartFix after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartMover", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.StartMover(ctx)
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartMover after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartRebalance", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		plan, err := client.PlanRebalance(ctx)
		if err != nil {
			t.Fatalf("PlanRebalance: %v", err)
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err = client.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: plan.Confirmation})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartRebalance after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("EvacuateDisk", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		plan, err := client.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: "/mnt/disk1"})
		if err != nil {
			t.Fatalf("PlanDiskEvacuation: %v", err)
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err = client.EvacuateDisk(ctx, &apiv1.EvacuateDiskRequest{Mountpoint: "/mnt/disk1", Confirmation: plan.Confirmation})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("EvacuateDisk after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("StartShareRelocation", func(t *testing.T) {
		client := newTestClient(t, "healthy")
		if _, err := client.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err != nil {
			t.Fatalf("CreateShare: %v", err)
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.StartShareRelocation(ctx, &apiv1.StartShareRelocationRequest{To: apiv1.StartShareRelocationRequestToArray}, apiv1.StartShareRelocationParams{Name: "media"})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("StartShareRelocation after StopArray: code = %q, want maintenance_mode", code)
		}
	})

	t.Run("CreateArray", func(t *testing.T) {
		client := newTestClient(t, "fresh-install")
		disks := []apiv1.ArrayDiskAssignment{
			{Device: "/dev/sdb", Role: apiv1.ArrayDiskRoleParity, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
			{Device: "/dev/sdc", Role: apiv1.ArrayDiskRoleData, Filesystem: apiv1.NewOptArrayDiskFilesystem(apiv1.ArrayDiskFilesystemXfs)},
		}
		if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
			t.Fatalf("StopArray: %v", err)
		}
		_, err := client.CreateArray(ctx, &apiv1.CreateArrayRequest{Disks: disks, Confirmation: "ERASE /dev/sdb, /dev/sdc"})
		if code := errorCode(t, err); code != "maintenance_mode" {
			t.Fatalf("CreateArray after StopArray: code = %q, want maintenance_mode", code)
		}
	})
}

// TestUpgradeDisk_DataMirrorsProductionAdmission mirrors production's
// doc 02 §4 rules for a data-disk upgrade: refused with array_not_stopped
// while the array runs (E8), admitted after `array stop`, then — while it
// is pending — a second upgrade, `array start` and `array stop` are all
// refused with disk_upgrade_pending (E8, E6, E7) until it is cancelled.
func TestUpgradeDisk_DataMirrorsProductionAdmission(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	plan, err := client.PlanDiskUpgrade(ctx, &apiv1.DiskUpgradePlanRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf"})
	if err != nil {
		t.Fatalf("PlanDiskUpgrade(data): %v", err)
	}
	req := &apiv1.UpgradeDiskRequest{Mountpoint: "/mnt/disk1", Device: "/dev/sdf", Confirmation: plan.Confirmation}
	if _, err := client.UpgradeDisk(ctx, req); errorCode(t, err) != "array_not_stopped" {
		t.Fatalf("UpgradeDisk with the array running: %v, want array_not_stopped", err)
	}
	if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	queued, err := client.UpgradeDisk(ctx, req)
	if err != nil {
		t.Fatalf("UpgradeDisk after array stop: %v", err)
	}
	if _, err := client.UpgradeDisk(ctx, req); errorCode(t, err) != "disk_upgrade_pending" {
		t.Fatalf("second UpgradeDisk: %v, want disk_upgrade_pending", err)
	}
	if _, err := client.StartArray(ctx); errorCode(t, err) != "disk_upgrade_pending" {
		t.Fatalf("StartArray while pending: %v, want disk_upgrade_pending", err)
	}
	if _, err := client.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); errorCode(t, err) != "disk_upgrade_pending" {
		t.Fatalf("StopArray while pending: %v, want disk_upgrade_pending", err)
	}
	if _, err := client.CancelJob(ctx, apiv1.CancelJobParams{JobId: queued.ID}); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if _, err := client.StartArray(ctx); err != nil {
		t.Fatalf("StartArray after the cancel: %v", err)
	}
}

// TestEvacuateDisk_MirrorsProductionAdmission proves the mock refuses a
// new evacuation while one is pending, as job.Scheduler does (#359), and
// admits one again once the pending job is cancelled.
func TestEvacuateDisk_MirrorsProductionAdmission(t *testing.T) {
	client := newTestClient(t, "healthy")
	ctx := context.Background()

	req := func(mountpoint string) *apiv1.EvacuateDiskRequest {
		plan, err := client.PlanDiskEvacuation(ctx, &apiv1.EvacuateDiskPlanRequest{Mountpoint: mountpoint})
		if err != nil {
			t.Fatalf("PlanDiskEvacuation(%s): %v", mountpoint, err)
		}
		return &apiv1.EvacuateDiskRequest{Mountpoint: mountpoint, Confirmation: plan.Confirmation}
	}
	queued, err := client.EvacuateDisk(ctx, req("/mnt/disk1"))
	if err != nil {
		t.Fatalf("EvacuateDisk: %v", err)
	}
	if _, err := client.EvacuateDisk(ctx, req("/mnt/disk1")); errorCode(t, err) != "evacuation_pending" {
		t.Fatalf("second EvacuateDisk for the same disk: %v, want evacuation_pending", err)
	}
	if _, err := client.EvacuateDisk(ctx, req("/mnt/disk2")); errorCode(t, err) != "evacuation_pending" {
		t.Fatalf("EvacuateDisk for another disk while one is pending: %v, want evacuation_pending", err)
	}
	if _, err := client.CancelJob(ctx, apiv1.CancelJobParams{JobId: queued.ID}); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if _, err := client.EvacuateDisk(ctx, req("/mnt/disk1")); err != nil {
		t.Fatalf("EvacuateDisk after the pending one was cancelled: %v", err)
	}
}

func TestCreateShareNFSEncodes(t *testing.T) {
	client := newTestClient(t, "fresh-install")
	ctx := context.Background()

	created, err := client.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name: "media",
	})
	if err != nil {
		t.Fatalf("CreateShare(omit nfs): %v", err)
	}
	if created.Nfs.Enabled {
		t.Fatal("CreateShare(omit nfs): nfs.enabled = true, want false")
	}
	if created.Nfs.Squash != apiv1.ShareNFSSquashRootSquash {
		t.Fatalf("CreateShare(omit nfs): squash = %q, want root_squash", created.Nfs.Squash)
	}
	if created.Nfs.Hosts == nil {
		t.Fatal("CreateShare(omit nfs): nfs.hosts is nil, want empty slice")
	}
	if len(created.Nfs.Hosts) != 0 {
		t.Fatalf("CreateShare(omit nfs): nfs.hosts = %v, want empty", created.Nfs.Hosts)
	}
	wantFsid := config.NFSExportFsid("media")
	if v, ok := created.Nfs.Fsid.Get(); !ok || v != wantFsid {
		t.Fatalf("CreateShare(omit nfs): nfs.fsid = %v (ok=%v), want %s", v, ok, wantFsid)
	}

	got, err := client.GetShare(ctx, apiv1.GetShareParams{Name: created.Name})
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	assertShareNFS(t, "GetShare", got.Nfs, created.Nfs)

	listed, err := client.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(listed.Shares) != 1 {
		t.Fatalf("ListShares: len = %d, want 1", len(listed.Shares))
	}
	assertShareNFS(t, "ListShares", listed.Shares[0].Nfs, created.Nfs)

	withNFS, err := client.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name: "backup",
		Nfs: apiv1.NewOptShareNFS(apiv1.ShareNFS{
			Enabled: true,
			Hosts:   []string{"192.168.1.0/24", "10.0.0.5"},
			Squash:  apiv1.ShareNFSSquashNoRootSquash,
		}),
	})
	if err != nil {
		t.Fatalf("CreateShare(with nfs): %v", err)
	}
	if !withNFS.Nfs.Enabled {
		t.Fatal("CreateShare(with nfs): nfs.enabled = false, want true")
	}
	if withNFS.Nfs.Squash != apiv1.ShareNFSSquashNoRootSquash {
		t.Fatalf("CreateShare(with nfs): squash = %q, want no_root_squash", withNFS.Nfs.Squash)
	}
	if len(withNFS.Nfs.Hosts) != 2 {
		t.Fatalf("CreateShare(with nfs): hosts = %v, want two entries", withNFS.Nfs.Hosts)
	}
	wantBackupFsid := config.NFSExportFsid("backup")
	if v, ok := withNFS.Nfs.Fsid.Get(); !ok || v != wantBackupFsid {
		t.Fatalf("CreateShare(with nfs): nfs.fsid = %v (ok=%v), want %s", v, ok, wantBackupFsid)
	}

	updated, err := client.UpdateShare(ctx, &apiv1.UpdateShareRequest{
		Nfs: apiv1.NewOptShareNFS(apiv1.ShareNFS{
			Enabled: false,
			Hosts:   []string{"192.168.2.1"},
			Squash:  apiv1.ShareNFSSquashAllSquash,
		}),
	}, apiv1.UpdateShareParams{Name: withNFS.Name})
	if err != nil {
		t.Fatalf("UpdateShare(nfs): %v", err)
	}
	if updated.Nfs.Enabled {
		t.Fatal("UpdateShare(nfs): nfs.enabled = true, want false")
	}
	if updated.Nfs.Squash != apiv1.ShareNFSSquashAllSquash {
		t.Fatalf("UpdateShare(nfs): squash = %q, want all_squash", updated.Nfs.Squash)
	}
	if len(updated.Nfs.Hosts) != 1 || updated.Nfs.Hosts[0] != "192.168.2.1" {
		t.Fatalf("UpdateShare(nfs): hosts = %v, want [192.168.2.1]", updated.Nfs.Hosts)
	}
}

func assertShareNFS(t *testing.T, label string, got, want apiv1.ShareNFS) {
	t.Helper()
	if got.Enabled != want.Enabled {
		t.Fatalf("%s: nfs.enabled = %v, want %v", label, got.Enabled, want.Enabled)
	}
	if got.Squash != want.Squash {
		t.Fatalf("%s: nfs.squash = %q, want %q", label, got.Squash, want.Squash)
	}
	if len(got.Hosts) != len(want.Hosts) {
		t.Fatalf("%s: nfs.hosts = %v, want %v", label, got.Hosts, want.Hosts)
	}
	for i := range want.Hosts {
		if got.Hosts[i] != want.Hosts[i] {
			t.Fatalf("%s: nfs.hosts[%d] = %q, want %q", label, i, got.Hosts[i], want.Hosts[i])
		}
	}
	gotFsid, gotOK := got.Fsid.Get()
	wantFsid, wantOK := want.Fsid.Get()
	if gotOK != wantOK || gotFsid != wantFsid {
		t.Fatalf("%s: nfs.fsid = %v (ok=%v), want %v (ok=%v)", label, gotFsid, gotOK, wantFsid, wantOK)
	}
}

// TestMockEvacuationDataDisk_RefusesADiskThatLeftThePool proves the mock
// mirrors production's evacuationDataDisk (#366): disk_slot_not_found
// first, then disk_leaving_array for an unpooled or unlisted disk, while
// an evacuating or evacuated disk passes as the resume path. No fixture
// scenario has a disk past its evacuation, so the check runs against a
// constructed array.
func TestMockEvacuationDataDisk_RefusesADiskThatLeftThePool(t *testing.T) {
	for _, tc := range []struct {
		state string
		code  string
	}{
		{"", ""},
		{store.RemovalStateEvacuating, ""},
		{store.RemovalStateEvacuated, ""},
		{store.RemovalStateUnpooled, "disk_leaving_array"},
		{store.RemovalStateUnlisted, "disk_leaving_array"},
	} {
		disks := []store.ArrayDisk{{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1", RemovalState: tc.state}}
		err := mockEvacuationDataDisk(disks, "/mnt/disk1")
		got := ""
		if err != nil {
			var me *mockError
			if !errors.As(err, &me) {
				t.Fatalf("state %q: error %v is not a mockError", tc.state, err)
			}
			got = me.code
		}
		if got != tc.code {
			t.Fatalf("state %q: mockEvacuationDataDisk = %q, want %q", tc.state, got, tc.code)
		}
	}
	err := mockEvacuationDataDisk([]store.ArrayDisk{{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1", RemovalState: store.RemovalStateUnlisted}}, "/mnt/disk9")
	var me *mockError
	if !errors.As(err, &me) || me.code != "disk_slot_not_found" {
		t.Fatalf("unknown mountpoint = %v, want disk_slot_not_found", err)
	}
}
