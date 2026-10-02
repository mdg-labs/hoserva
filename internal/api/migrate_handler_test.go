package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

func flashZip(t *testing.T, version string, mutate func(map[string]string)) []byte {
	t.Helper()
	files := map[string]string{
		"changes.txt":     "# Version " + version + " 2026-01-01\n",
		"bzimage":         "kernel",
		"config/disk.cfg": "startArray=\"yes\"\ndiskIdSlot.0=\"-\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
		"config/hoserva/disks.ini": "[\"parity\"]\nidx=\"0\"\nid=\"M_PARITYSERIAL\"\nsize=\"1000\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n" +
			"[\"disk1\"]\nidx=\"1\"\nid=\"M_DATASERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\n",
	}
	if mutate != nil {
		mutate(files)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func migrationHandler(t *testing.T) (*api.Handler, *migrate.Service) {
	t.Helper()
	h, _, registry := newTestHandler(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "PARITYSERIAL", Size: 2 << 40})
	disks.AddDisk("/dev/sdc", disk.Disk{Serial: "DATASERIAL", Size: 1 << 40})
	svc := &migrate.Service{
		Dir:      filepath.Join(t.TempDir(), "migrate"),
		Scanner:  &migrate.Scanner{Disks: disks, UIDOwner: func(int) (string, error) { return "", nil }},
		Sessions: store.NewMigrationSessionStore(openTestDB(t)),
	}
	h.Migration = svc
	registry.Register(job.TypeMigrationScan, false, job.RunMigrationScan(svc.RunScan))
	return h, svc
}

func scanRequest(zipData []byte, unverified bool) *apiv1.StartMigrationScanReq {
	req := &apiv1.StartMigrationScanReq{File: ht.MultipartFile{Name: "boot.zip", File: bytes.NewReader(zipData)}}
	if unverified {
		req.UnverifiedLayout = apiv1.NewOptBool(true)
	}
	return req
}

func TestHandler_MigrationOperations_Return501WithoutAService(t *testing.T) {
	h, _, _ := newTestHandler(t)
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"GetMigration":       func() error { _, err := h.GetMigration(ctx); return err },
		"GetMigrationReport": func() error { _, err := h.GetMigrationReport(ctx); return err },
		"ForgetMigration":    func() error { return h.ForgetMigration(ctx) },
		"StartMigrationScan": func() error { _, err := h.StartMigrationScan(ctx, scanRequest(nil, false)); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s = nil without a migration service", name)
		}
		if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
			t.Errorf("%s = %d %s, want 501 not_configured", name, st, code)
		}
	}
}

func TestHandler_StartMigrationScan_RunsAScanAndServesItsReport(t *testing.T) {
	h, svc := migrationHandler(t)
	ctx := context.Background()

	got, err := h.GetMigration(ctx)
	if err != nil || got.Phase != apiv1.MigrationPhaseNone {
		t.Fatalf("GetMigration before a scan = %+v, %v", got, err)
	}
	if _, err := h.GetMigrationReport(ctx); err == nil {
		t.Fatal("GetMigrationReport before a scan = nil")
	} else if st, code := statusOf(h, err); st != 404 || code != "no_migration_report" {
		t.Fatalf("GetMigrationReport before a scan = %d %s", st, code)
	}

	j, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
	if err != nil {
		t.Fatalf("StartMigrationScan: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationScan || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %s %s, want a migration_scan topology job", j.Type, j.Class)
	}
	done, err := h.Scheduler.Await(ctx, j.ID.String())
	if err != nil || done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %+v, %v", done, err)
	}

	got, err = h.GetMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report, ok := got.Report.Get()
	if got.Phase != apiv1.MigrationPhaseScanned || !ok || report.UnraidVersion.Or("") != "7.3.2" || len(report.Rows) == 0 {
		t.Fatalf("GetMigration = %+v", got)
	}
	var mapped int
	for _, row := range report.Rows {
		if row.Check == "disk_mapping" && row.Status == apiv1.MigrationCheckStatusPass {
			mapped++
		}
	}
	if mapped != 2 {
		t.Errorf("%d disk_mapping passes, want the parity and data slot: %+v", mapped, report.Rows)
	}

	doc, err := h.GetMigrationReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := io.ReadAll(doc.Data)
	if !strings.HasPrefix(string(md), "# Hoserva migration scan report") || !strings.Contains(string(md), "Unraid 7.3.2") {
		t.Errorf("report document:\n%s", md)
	}

	if err := h.ForgetMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := svc.State(ctx); st.Phase != migrate.PhaseNone {
		t.Errorf("phase after forget = %s", st.Phase)
	}
	if _, err := h.GetMigrationReport(ctx); err == nil {
		t.Error("the report survived a forget")
	}
}

func TestHandler_StartMigrationScan_RefusalsQueueNothingAndKeepNothing(t *testing.T) {
	cases := map[string]struct {
		req    func(t *testing.T) *apiv1.StartMigrationScanReq
		status int
		code   string
	}{
		"no file":   {func(*testing.T) *apiv1.StartMigrationScanReq { return &apiv1.StartMigrationScanReq{} }, 400, "file_required"},
		"not a zip": {func(*testing.T) *apiv1.StartMigrationScanReq { return scanRequest([]byte("hello"), false) }, 400, "invalid_zip"},
		"entry escapes": {func(t *testing.T) *apiv1.StartMigrationScanReq {
			return scanRequest(flashZip(t, "7.3.2", func(f map[string]string) { f["../evil"] = "x" }), false)
		}, 400, "invalid_zip"},
		"no disk.cfg": {func(t *testing.T) *apiv1.StartMigrationScanReq {
			return scanRequest(flashZip(t, "7.3.2", func(f map[string]string) { delete(f, "config/disk.cfg") }), false)
		}, 400, "invalid_flash_backup"},
		"unknown version": {func(t *testing.T) *apiv1.StartMigrationScanReq { return scanRequest(flashZip(t, "6.9.2", nil), false) }, 400, "unsupported_layout"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, svc := migrationHandler(t)
			_, err := h.StartMigrationScan(context.Background(), tc.req(t))
			if err == nil {
				t.Fatal("StartMigrationScan accepted it")
			}
			if st, code := statusOf(h, err); st != tc.status || code != tc.code {
				t.Errorf("= %d %s, want %d %s", st, code, tc.status, tc.code)
			}
			jobs, _ := h.Store.List(context.Background(), job.ListFilter{})
			if len(jobs) != 0 {
				t.Errorf("a refused upload queued %v", jobs)
			}
			if st, _ := svc.State(context.Background()); st.Phase != migrate.PhaseNone {
				t.Errorf("phase = %s", st.Phase)
			}
		})
	}
}

func TestHandler_StartMigrationScan_UnverifiedLayoutIsRecordedInTheReport(t *testing.T) {
	h, _ := migrationHandler(t)
	ctx := context.Background()
	j, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "6.9.2", nil), true))
	if err != nil {
		t.Fatalf("StartMigrationScan with the override: %v", err)
	}
	if done, err := h.Scheduler.Await(ctx, j.ID.String()); err != nil || done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %+v, %v", done, err)
	}
	got, _ := h.GetMigration(ctx)
	if report, _ := got.Report.Get(); !report.UnverifiedLayout {
		t.Errorf("the override is not recorded: %+v", report)
	}
}

func TestHandler_StartMigrationScan_RefusedByTheSchedulerLeavesThePreviousSession(t *testing.T) {
	h, svc := migrationHandler(t)
	ctx := context.Background()
	j, err := h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Scheduler.Await(ctx, j.ID.String()); err != nil {
		t.Fatal(err)
	}
	before, _ := svc.State(ctx)

	if err := h.Scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = h.StartMigrationScan(ctx, scanRequest(flashZip(t, "7.3.2", nil), false))
	if st, code := statusOf(h, err); st != 409 || code != "maintenance_mode" {
		t.Fatalf("StartMigrationScan in maintenance mode = %d %s (%v), want 409 maintenance_mode", st, code, err)
	}
	after, _ := svc.State(ctx)
	if after.Phase != migrate.PhaseScanned || after.Source.File != before.Source.File {
		t.Errorf("session after the refusal = %+v, want it unchanged", after)
	}
}
