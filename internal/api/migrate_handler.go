package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

func errMigrationNotConfigured() error {
	return &apiError{code: "not_configured", statusCode: 501, message: "the migrator is not configured on this daemon"}
}

// migrateError maps a migration service error to the spec's error codes. An
// error already built for the API (the scheduler's refusals) passes through.
func migrateError(err error) error {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, migrate.ErrZipTooLarge):
		return &apiError{code: "zip_too_large", statusCode: 413, message: err.Error()}
	case errors.Is(err, migrate.ErrInvalidZip):
		return &apiError{code: "invalid_zip", statusCode: 400, message: err.Error()}
	case errors.Is(err, migrate.ErrNoDiskCfg):
		return &apiError{code: "invalid_flash_backup", statusCode: 400, message: err.Error()}
	case errors.Is(err, migrate.ErrUnsupportedLayout):
		return &apiError{code: "unsupported_layout", statusCode: 400, message: err.Error()}
	case errors.Is(err, migrate.ErrScanInProgress):
		return &apiError{code: "scan_in_progress", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrNoReport):
		return &apiError{code: "no_migration_report", statusCode: 404, message: err.Error()}
	case errors.Is(err, migrate.ErrNotFlashDevice):
		return &apiError{code: "invalid_flash_device", statusCode: 400, message: err.Error()}
	case errors.Is(err, migrate.ErrZipOnly):
		return &apiError{code: "zip_only_source", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrFlashDevice):
		return &apiError{code: "flash_device_unreadable", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrNoDeviceSource):
		return errMigrationNotConfigured()
	}
	return err
}

// StartMigrationScan stages the uploaded Flash Backup zip, refuses it unless it
// is a usable one, and queues the migration_scan job (doc 05 §3).
func (h *Handler) StartMigrationScan(ctx context.Context, req *apiv1.StartMigrationScanReq) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil {
		return nil, errMigrationNotConfigured()
	}
	if req == nil || req.File.File == nil {
		return nil, &apiError{code: "file_required", statusCode: 400, message: "the Flash Backup zip is required as the file part"}
	}
	opts := migrate.ScanOptions{UnverifiedLayout: req.UnverifiedLayout.Or(false)}
	var queued *job.Job
	err := h.Migration.StartScan(ctx, req.File.File, opts, func(ctx context.Context, upload string) (string, error) {
		params, err := json.Marshal(job.MigrationScanParams{Upload: upload})
		if err != nil {
			return "", err
		}
		j, err := h.Scheduler.Submit(ctx, job.TypeMigrationScan, []string{migrate.JobResource}, params)
		if err != nil {
			return "", mapSchedulerError(uuid.Nil, err)
		}
		queued = j
		return j.ID, nil
	})
	if err != nil {
		return nil, migrateError(err)
	}
	return jobToAPI(queued)
}

// StartMigrationDeviceScan reads Unraid's configuration from the USB stick,
// mounted read-only for the duration, and queues the migration_scan job (doc 05
// §3, Q25).
func (h *Handler) StartMigrationDeviceScan(ctx context.Context, req *apiv1.StartMigrationDeviceScanReq) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil {
		return nil, errMigrationNotConfigured()
	}
	if req == nil || req.Device == "" {
		return nil, &apiError{code: "invalid_flash_device", statusCode: 400, message: "the device is required"}
	}
	opts := migrate.ScanOptions{UnverifiedLayout: req.UnverifiedLayout.Or(false)}
	var queued *job.Job
	err := h.Migration.StartDeviceScan(ctx, req.Device, opts, func(ctx context.Context, scan string) (string, error) {
		params, err := json.Marshal(job.MigrationScanParams{Upload: scan})
		if err != nil {
			return "", err
		}
		j, err := h.Scheduler.Submit(ctx, job.TypeMigrationScan, []string{migrate.JobResource}, params)
		if err != nil {
			return "", mapSchedulerError(uuid.Nil, err)
		}
		queued = j
		return j.ID, nil
	})
	if err != nil {
		return nil, migrateError(err)
	}
	return jobToAPI(queued)
}

// GetMigration returns the migration session: its phase and its report.
func (h *Handler) GetMigration(ctx context.Context) (*apiv1.Migration, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	st, err := h.Migration.State(ctx)
	if err != nil {
		return nil, err
	}
	out := &apiv1.Migration{Phase: apiv1.MigrationPhase(st.Phase)}
	if st.ScanError != "" {
		out.ScanError = apiv1.NewOptString(st.ScanError)
	}
	if st.Source != nil {
		out.SourceReceivedAt = apiv1.NewOptDateTime(st.Source.ReceivedAt)
		if dev, ok := st.Source.IsDevice(); ok {
			out.SourceDevice = apiv1.NewOptString(dev)
		} else {
			out.SourceSize = apiv1.NewOptInt64(st.Source.Size)
		}
	}
	if st.Report != nil {
		out.Report = apiv1.NewOptMigrationReport(migrationReportToAPI(st.Report))
	}
	offer, err := h.Migration.FlashOffer(ctx)
	if err != nil {
		return nil, migrateError(err)
	}
	out.ZipOnly = offer.ZipOnly
	out.FlashDevices = make([]apiv1.MigrationFlashDevice, 0, len(offer.Devices))
	for _, d := range offer.Devices {
		item := apiv1.MigrationFlashDevice{Device: d.Device, Size: d.Size}
		if d.Model != "" {
			item.Model = apiv1.NewOptString(d.Model)
		}
		if d.Serial != "" {
			item.Serial = apiv1.NewOptString(d.Serial)
		}
		out.FlashDevices = append(out.FlashDevices, item)
	}
	return out, nil
}

func migrationReportToAPI(r *migrate.Report) apiv1.MigrationReport {
	out := apiv1.MigrationReport{
		GeneratedAt:      r.GeneratedAt,
		UnverifiedLayout: r.UnverifiedLayout,
		Verdict:          apiv1.MigrationVerdict(r.Verdict),
		Rows:             make([]apiv1.MigrationReportRow, 0, len(r.Rows)),
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

// GetMigrationReport returns the latest report as a Markdown document.
func (h *Handler) GetMigrationReport(ctx context.Context) (apiv1.GetMigrationReportOK, error) {
	if h.Migration == nil {
		return apiv1.GetMigrationReportOK{}, errMigrationNotConfigured()
	}
	md, err := h.Migration.ReportMarkdown(ctx)
	if err != nil {
		return apiv1.GetMigrationReportOK{}, migrateError(err)
	}
	return apiv1.GetMigrationReportOK{Data: strings.NewReader(md)}, nil
}

// ForgetMigration deletes the migration session and the uploaded zip.
func (h *Handler) ForgetMigration(ctx context.Context) error {
	if h.Migration == nil {
		return errMigrationNotConfigured()
	}
	return migrateError(h.Migration.Forget(ctx))
}
