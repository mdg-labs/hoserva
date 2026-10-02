package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

// mockMigration is the migration session this mock instance keeps: a report
// and the size of the zip it came from. Like production's session it holds
// the report only; the mock keeps no zip.
type mockMigration struct {
	mu         sync.Mutex
	report     *migrate.Report
	size       int64
	receivedAt time.Time
}

func errMigrationRefusal(code string, status int, err error) error {
	return &mockError{code: code, statusCode: status, message: err.Error()}
}

// mockMigrateError gives an upload refusal the status and code production
// answers it with.
func mockMigrateError(err error) error {
	switch {
	case errors.Is(err, migrate.ErrZipTooLarge):
		return errMigrationRefusal("zip_too_large", 413, err)
	case errors.Is(err, migrate.ErrInvalidZip):
		return errMigrationRefusal("invalid_zip", 400, err)
	case errors.Is(err, migrate.ErrNoDiskCfg):
		return errMigrationRefusal("invalid_flash_backup", 400, err)
	case errors.Is(err, migrate.ErrUnsupportedLayout):
		return errMigrationRefusal("unsupported_layout", 400, err)
	}
	return err
}

func errNoMigrationReport() error {
	return &mockError{code: "no_migration_report", statusCode: 404, message: migrate.ErrNoReport.Error()}
}

// mockMigrationReport is a completed scan of a healthy single-parity array:
// what the UI shows after a scan, with one SMART finding so a flagged row has
// something to render.
func mockMigrationReport(version string, unverified bool, at time.Time) *migrate.Report {
	r := &migrate.Report{GeneratedAt: at, UnraidVersion: version, UnverifiedLayout: unverified}
	add := func(check string, st migrate.Status, subject, detail string) {
		r.Rows = append(r.Rows, migrate.Row{Check: check, Status: st, Subject: subject, Detail: detail})
	}
	if unverified {
		add(migrate.CheckVersion, migrate.StatusWarn, "", "This Unraid version or flash layout is not one Hoserva has been verified against. The scan went ahead only because --unverified-layout was given; this override is recorded in the report (Q24).")
	} else {
		add(migrate.CheckVersion, migrate.StatusPass, "", "Unraid "+version+", flash layout recognised.")
	}
	add(migrate.CheckBootDevice, migrate.StatusInfo, "", "Unraid boots from a USB stick. Keep the stick: it is the rollback.")
	add(migrate.CheckMapping, migrate.StatusPass, "parity", "serial EXAMPLE_PARITY is Unraid parity slot 0; this machine has it as /dev/sdb, 8.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusPass, "disk1", "serial EXAMPLE_DISK1 is Unraid disk 1 (xfs); this machine has it as /dev/sdc, 4.0 TiB.")
	add(migrate.CheckMapping, migrate.StatusFlag, "disk2", "Unraid had a disk with serial EXAMPLE_DISK2 here, and no disk on this machine has this serial or WWN. Attach it before the import.")
	add(migrate.CheckIdentity, migrate.StatusPass, "parity", "/dev/sdb has a WWN or serial.")
	add(migrate.CheckParity, migrate.StatusInfo, "", "1 parity disk(s). Parity is rewritten from scratch either way.")
	add(migrate.CheckParitySize, migrate.StatusPass, "", "The smallest parity disk (8.0 TiB) is at least as large as the largest data disk (4.0 TiB).")
	add(migrate.CheckSMART, migrate.StatusPass, "parity", "/dev/sdb has no reallocated or pending sectors.")
	add(migrate.CheckSMART, migrate.StatusFlag, "disk1", "Recommend aborting: /dev/sdc has 8 reallocated and 0 pending sectors, and the unprotected window of the migration is when a marginal disk fails.")
	add(migrate.CheckUID99, migrate.StatusPass, "", "UID 99 is free for the hoserva-apps user.")
	add(migrate.CheckSyncEstimate, migrate.StatusInfo, "", "About 2.8 hours for 4.0 TiB of data disks, if they are full, at an assumed 400 MB/s. It is a planning figure, not a measurement: the first sync is a long job, and it runs only when you start it.")
	r.Verdict = migrate.VerdictGoWithWarnings
	return r
}

func seededMigration(scenario string) *mockMigration {
	m := &mockMigration{}
	if scenario == "migration-pending" {
		at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
		m.report, m.size, m.receivedAt = mockMigrationReport("7.3.2", false, at), 612<<20, at
	}
	return m
}

func (h *handler) StartMigrationScan(ctx context.Context, req *apiv1.StartMigrationScanReq) (*apiv1.Job, error) {
	if req == nil || req.File.File == nil {
		return nil, &mockError{code: "file_required", statusCode: 400, message: "the Flash Backup zip is required as the file part"}
	}
	unverified := req.UnverifiedLayout.Or(false)
	var size countingReader
	size.r = req.File.File
	flash, err := migrate.InspectUpload(&size, migrate.ScanOptions{UnverifiedLayout: unverified})
	if err != nil {
		return nil, mockMigrateError(err)
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationScan, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	// The mock has no scheduler, so the scan is done as soon as it is queued.
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()

	h.migration.mu.Lock()
	h.migration.report = mockMigrationReport(flash.Version, flash.LayoutProblem() != "", now)
	h.migration.size, h.migration.receivedAt = size.n, now
	h.migration.mu.Unlock()
	return j, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (h *handler) GetMigration(ctx context.Context) (*apiv1.Migration, error) {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	out := &apiv1.Migration{Phase: apiv1.MigrationPhaseNone}
	if r := h.migration.report; r != nil {
		out.Phase = apiv1.MigrationPhaseScanned
		out.SourceSize = apiv1.NewOptInt64(h.migration.size)
		out.SourceReceivedAt = apiv1.NewOptDateTime(h.migration.receivedAt)
		out.Report = apiv1.NewOptMigrationReport(mockMigrationReportToAPI(r))
	}
	return out, nil
}

func mockMigrationReportToAPI(r *migrate.Report) apiv1.MigrationReport {
	out := apiv1.MigrationReport{
		GeneratedAt: r.GeneratedAt, UnverifiedLayout: r.UnverifiedLayout, Verdict: apiv1.MigrationVerdict(r.Verdict),
		Rows: make([]apiv1.MigrationReportRow, 0, len(r.Rows)),
	}
	if r.UnraidVersion != "" {
		out.UnraidVersion = apiv1.NewOptString(r.UnraidVersion)
	}
	for _, row := range r.Rows {
		item := apiv1.MigrationReportRow{Check: row.Check, Status: apiv1.MigrationCheckStatus(row.Status), Detail: row.Detail}
		if row.Subject != "" {
			item.Subject = apiv1.NewOptString(row.Subject)
		}
		out.Rows = append(out.Rows, item)
	}
	return out
}

func (h *handler) GetMigrationReport(ctx context.Context) (apiv1.GetMigrationReportOK, error) {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	if h.migration.report == nil {
		return apiv1.GetMigrationReportOK{}, errNoMigrationReport()
	}
	return apiv1.GetMigrationReportOK{Data: strings.NewReader(h.migration.report.Markdown())}, nil
}

func (h *handler) ForgetMigration(ctx context.Context) error {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	h.migration.report, h.migration.size, h.migration.receivedAt = nil, 0, time.Time{}
	return nil
}
