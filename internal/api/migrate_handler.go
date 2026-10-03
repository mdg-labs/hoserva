package api

import (
	"context"
	"encoding/json"
	"errors"
	"path"
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
	case errors.Is(err, migrate.ErrNoPreview):
		return &apiError{code: "no_template_preview", statusCode: 404, message: err.Error()}
	case errors.Is(err, migrate.ErrSourceUnavailable):
		return &apiError{code: "template_source_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrTemplateNotFound), errors.Is(err, migrate.ErrNoCompose):
		return &apiError{code: "template_not_found", statusCode: 404, message: err.Error()}
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

// ListMigrationTemplates returns the templates and Compose Manager projects of
// the latest report with how each converted and the counts the report shows.
func (h *Handler) ListMigrationTemplates(ctx context.Context) (*apiv1.MigrationTemplates, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	list, err := h.Migration.Templates(ctx)
	if err != nil {
		return nil, migrateError(err)
	}
	out := &apiv1.MigrationTemplates{
		Counts: apiv1.MigrationTemplateCounts{
			Clean: list.Counts.Clean, WithWarnings: list.Counts.WithWarnings, Failed: list.Counts.Failed,
			TemplateOnly: list.Counts.TemplateOnly, AllTemplates: list.Counts.AllTemplates, ComposeProjects: list.Counts.ComposeProjects,
		},
		Templates:       make([]apiv1.MigrationTemplateSummary, 0, len(list.Templates)),
		ComposeProjects: make([]apiv1.MigrationComposeProjectSummary, 0, len(list.ComposeProjects)),
	}
	for _, e := range list.Templates {
		item := apiv1.MigrationTemplateSummary{
			Name: e.Name, File: path.Base(e.File), Class: apiv1.MigrationTemplateClass(e.Class), Counted: e.Counted(),
			Status: apiv1.MigrationTemplateStatus(e.Outcome.Status), WarningCount: e.Outcome.ActionWarnings(),
			Error: optString(e.Outcome.FailureText()),
		}
		if e.AutostartPosition > 0 {
			item.AutostartPosition = apiv1.NewOptInt(e.AutostartPosition)
		}
		out.Templates = append(out.Templates, item)
	}
	for _, p := range list.ComposeProjects {
		status, errText := composeProjectStatus(p.Outcome)
		containers := p.Containers
		if containers == nil {
			containers = []string{}
		}
		out.ComposeProjects = append(out.ComposeProjects, apiv1.MigrationComposeProjectSummary{
			Name: p.Name, Containers: containers, Status: status, Error: optString(errText),
		})
	}
	return out, nil
}

func composeProjectStatus(o *migrate.Outcome) (apiv1.MigrationTemplateStatus, string) {
	switch {
	case o == nil:
		return apiv1.MigrationTemplateStatusMissing, ""
	case o.Status == migrate.PreviewFailed:
		return apiv1.MigrationTemplateStatusFailed, o.FailureText()
	}
	return apiv1.MigrationTemplateStatusPreviewed, ""
}

// GetMigrationTemplate returns one template's or Compose Manager project's
// preview from the latest report.
func (h *Handler) GetMigrationTemplate(ctx context.Context, params apiv1.GetMigrationTemplateParams) (*apiv1.MigrationTemplatePreview, error) {
	if h.Migration == nil {
		return nil, errMigrationNotConfigured()
	}
	v, err := h.Migration.Template(ctx, params.Name)
	if err != nil {
		return nil, migrateError(err)
	}
	out := &apiv1.MigrationTemplatePreview{Name: v.Name, Warnings: []apiv1.ConversionWarning{}, Privileges: []apiv1.TemplatePrivilege{}}
	if v.Kind == migrate.KindTemplate {
		out.Kind = apiv1.MigrationTemplatePreviewKindTemplate
		out.Title = apiv1.NewOptString(v.Entry.Name)
		out.Class = apiv1.NewOptMigrationTemplateClass(apiv1.MigrationTemplateClass(v.Entry.Class))
		out.Counted = apiv1.NewOptBool(v.Entry.Counted())
		out.Status = apiv1.MigrationTemplateStatus(v.Entry.Outcome.Status)
	} else {
		out.Kind = apiv1.MigrationTemplatePreviewKindComposeProject
		out.Status, _ = composeProjectStatus(v.Project.Outcome)
	}
	out.Source = v.Preview.Source
	out.Compose = optString(v.Preview.Compose)
	out.Error = optString(v.Preview.Error)
	for _, w := range v.Preview.Warnings {
		out.Warnings = append(out.Warnings, apiv1.ConversionWarning{Class: apiv1.ConversionWarningClass(w.Class), Message: w.Message, Detail: optString(w.Detail), Command: optString(w.Command)})
	}
	for _, pr := range v.Preview.Privileges {
		out.Privileges = append(out.Privileges, apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Detail: optString(pr.Detail), Description: pr.Description})
	}
	return out, nil
}
