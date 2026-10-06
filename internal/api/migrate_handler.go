package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
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
	case errors.Is(err, migrate.ErrNoDeviceSource), errors.Is(err, migrate.ErrImportNotConfigured):
		return errMigrationNotConfigured()
	case errors.Is(err, migrate.ErrImportNoReport):
		return &apiError{code: "no_migration_report", statusCode: 404, message: err.Error()}
	case errors.Is(err, migrate.ErrImportScanUnfinished):
		return &apiError{code: "scan_not_finished", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrImportNoGo):
		return &apiError{code: "migration_no_go", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrImportNoReview):
		return &apiError{code: "scan_outdated", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrImportPending):
		return &apiError{code: "migration_in_progress", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrVerifyNotPending):
		return &apiError{code: "no_import_pending", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrNoBaseline):
		return &apiError{code: "no_migration_baseline", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrVerifyNotConfigured), errors.Is(err, migrate.ErrParityNotConfigured):
		return errMigrationNotConfigured()
	case errors.Is(err, migrate.ErrVerifyRequired):
		return &apiError{code: "verify_required", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrPoolBelowMinFreeSpace):
		return &apiError{code: "pool_below_min_free_space", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrContainersNotConfigured), errors.Is(err, migrate.ErrChecklistNotConfigured):
		return errMigrationNotConfigured()
	case errors.Is(err, migrate.ErrMigrationNotFinished):
		return &apiError{code: "migration_not_finished", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrChecklistItemNotFound):
		return &apiError{code: "checklist_item_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, migrate.ErrChecklistItemHasRecord):
		return &apiError{code: "checklist_item_has_record", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrParityNotInitialized):
		return &apiError{code: "parity_not_initialized", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrInvalidSelection):
		return &apiError{code: "invalid_selection", statusCode: 400, message: err.Error()}
	case errors.Is(err, migrate.ErrTemplateUnconvertible):
		return &apiError{code: "template_unconvertible", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrWarningsNotAcknowledged):
		return &apiError{code: "warnings_not_acknowledged", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrStackNotMigrated):
		return &apiError{code: "migrated_stack_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, migrate.ErrContainerUnconfirmed):
		return &apiError{code: "container_unconfirmed", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrContainerConfirmed):
		return &apiError{code: "container_confirmed", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrContainerNotStarted):
		return &apiError{code: "container_not_started", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrNoContainer):
		return &apiError{code: "no_container", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrDataCheckRequired):
		return &apiError{code: "data_check_required", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrDataCheckFailed):
		return &apiError{code: "data_check_failed", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrContainerNotRunning):
		return &apiError{code: "container_not_running", statusCode: 409, message: err.Error()}
	case errors.Is(err, migrate.ErrParityNotPending):
		return &apiError{code: "no_import_pending", statusCode: 409, message: err.Error()}
	case errors.Is(err, disk.ErrUnraidStick):
		return errUnraidStick(err)
	case errors.Is(err, job.ErrAdoptionLayout):
		return &apiError{code: "invalid_import_roles", statusCode: 400, message: err.Error()}
	case migrate.IsImportRoleError(err):
		return &apiError{code: "invalid_import_roles", statusCode: 400, message: err.Error()}
	}
	return err
}

func errArrayExistsNotPending() error {
	return &apiError{code: "array_exists", statusCode: 409, message: "an array already exists and is not an Unraid import waiting for its point of no return"}
}

// StartMigrationImport checks the confirmed disk-role mapping against the
// scan's report and this machine's disks and queues the migration_import job,
// which adopts the Unraid data disks read-only (doc 05 §4 steps 14-16). With
// undo it takes a pending import back instead.
func (h *Handler) StartMigrationImport(ctx context.Context, req *apiv1.MigrationImportRequest) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil || h.ArrayStore == nil {
		return nil, errMigrationNotConfigured()
	}
	if req == nil || !req.Confirm {
		return nil, errConfirmRequired
	}
	if req.Undo.Or(false) {
		if len(req.Roles) != 0 {
			return nil, &apiError{code: "invalid_import_roles", statusCode: 400, message: "an undo takes no disk-role mapping"}
		}
		return h.undoMigrationImport(ctx)
	}
	if len(req.Roles) == 0 {
		return nil, &apiError{code: "invalid_import_roles", statusCode: 400, message: "the import needs a disk-role mapping: at least one disk"}
	}
	assignments := make([]disk.AdoptionAssignment, 0, len(req.Roles))
	for _, r := range req.Roles {
		assignments = append(assignments, disk.AdoptionAssignment{
			Role: disk.AdoptionRole(r.Role), Serial: r.Serial.Or(""), WWN: r.Wwn.Or(""),
			ByIDName: r.ById.Or(""), PartUUID: r.PartUuid.Or(""),
		})
	}
	plan, err := h.Migration.PlanImport(ctx, assignments)
	if err != nil {
		return nil, migrateError(err)
	}
	exists, err := h.ArrayStore.Exists(ctx)
	if err != nil {
		return nil, err
	}
	if exists {
		pending, err := h.ArrayStore.MigrationPending(ctx)
		if err != nil {
			return nil, err
		}
		if !pending {
			return nil, errArrayExistsNotPending()
		}
	}
	if err := job.CheckAdoptionLayout(plan.Plan); err != nil {
		return nil, migrateError(err)
	}
	body, err := json.Marshal(job.MigrationImportParams{Assignments: plan.Assignments, Plan: plan.Plan})
	if err != nil {
		return nil, fmt.Errorf("encoding migration_import params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMigrationImport, []string{migrate.JobResource}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// undoMigrationImport queues the migration_import job in its undo form, which
// takes a pending adoption back without writing to any adopted disk. It is the
// way out of a pending import that step 17 would refuse. An array that is not a
// pending import's is refused before anything is queued.
func (h *Handler) undoMigrationImport(ctx context.Context) (*apiv1.Job, error) {
	pending, err := h.ArrayStore.MigrationPending(ctx)
	if err != nil {
		return nil, err
	}
	if !pending {
		return nil, &apiError{code: "no_import_pending", statusCode: 409, message: job.ErrMigrationUndoNotPending.Error()}
	}
	body, err := json.Marshal(job.MigrationImportParams{Undo: true})
	if err != nil {
		return nil, fmt.Errorf("encoding migration_import params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMigrationImport, []string{migrate.JobResource}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// StartMigrationVerify queues the migration_verify job, which compares the
// adopted disks and the pool with the scan's baseline (doc 05 §4 step 16).
func (h *Handler) StartMigrationVerify(ctx context.Context) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil {
		return nil, errMigrationNotConfigured()
	}
	if err := h.Migration.CheckVerify(ctx); err != nil {
		return nil, migrateError(err)
	}
	if h.Migration.Adopted == nil || h.Migration.ConfirmReadOnly == nil {
		return nil, errMigrationNotConfigured()
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMigrationVerify, []string{migrate.JobResource}, nil)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
}

// InitializeMigrationParity is the point of no return (doc 05 §4 step 17): it
// refuses unless the latest verify passed and the typed confirmation is the one
// the plan computes from the disks as they are now, and queues the
// migration_parity job, which formats the former parity disks and the cache and
// nothing else. The gate is checked before the confirmation, so a request made
// without a passing verify is refused as that whatever it carries.
func (h *Handler) InitializeMigrationParity(ctx context.Context, req *apiv1.MigrationInitializeParityRequest) (*apiv1.Job, error) {
	if h.Migration == nil || h.Scheduler == nil {
		return nil, errMigrationNotConfigured()
	}
	want, err := h.Migration.ExpectedParityConfirmation(ctx)
	if err != nil {
		return nil, migrateError(err)
	}
	if req == nil || req.Confirmation == "" || req.Confirmation != want {
		return nil, errConfirmRequired
	}
	body, err := json.Marshal(job.MigrationParityParams{Confirmation: req.Confirmation})
	if err != nil {
		return nil, fmt.Errorf("encoding migration_parity params: %w", err)
	}
	j, err := h.Scheduler.Submit(ctx, job.TypeMigrationParity, []string{migrate.JobResource}, body)
	if err != nil {
		return nil, mapSchedulerError(uuid.Nil, err)
	}
	return jobToAPI(j)
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
	opts := migrate.ScanOptions{UnverifiedLayout: req.UnverifiedLayout.Or(false), FullChecksums: req.FullChecksums.Or(false)}
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
	opts := migrate.ScanOptions{UnverifiedLayout: req.UnverifiedLayout.Or(false), FullChecksums: req.FullChecksums.Or(false)}
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
	if st.Verify != nil {
		out.Verify = apiv1.NewOptMigrationVerify(migrationVerifyToAPI(st.Verify))
	}
	pi, err := h.Migration.ParityInit(ctx)
	if err != nil {
		return nil, migrateError(err)
	}
	if pi != nil {
		out.ParityInit = apiv1.NewOptMigrationParityInit(migrationParityInitToAPI(pi))
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

func migrationParityInitToAPI(pi *migrate.ParityInit) apiv1.MigrationParityInit {
	out := apiv1.MigrationParityInit{
		Finishing: pi.Finishing, Confirmation: optString(pi.Confirmation), Problem: optString(pi.Problem),
		UnprotectedWindow: pi.Window, Rollback: append([]string{}, pi.Rollback...),
		Erases: make([]apiv1.MigrationParityErase, 0, len(pi.Erases)),
	}
	for _, e := range pi.Erases {
		item := apiv1.MigrationParityErase{Role: apiv1.MigrationParityEraseRole(e.Role), Device: e.Device, Partition: e.Partition, Serial: optString(e.Serial), Wwn: optString(e.WWN)}
		if e.Size > 0 {
			item.Size = apiv1.NewOptInt64(e.Size)
		}
		out.Erases = append(out.Erases, item)
	}
	return out
}

func migrationVerifyToAPI(v *migrate.VerifyResult) apiv1.MigrationVerify {
	out := apiv1.MigrationVerify{
		Status: apiv1.MigrationVerifyStatus(v.Status), Error: optString(v.Error), StartedAt: v.StartedAt,
		Disks: verifyScopesToAPI(v.Disks), Shares: verifyScopesToAPI(v.Shares), Duplicates: v.Duplicates,
		DuplicateSample: make([]apiv1.MigrationVerifyDuplicate, 0, len(v.DuplicateSample)),
	}
	if !v.FinishedAt.IsZero() {
		out.FinishedAt = apiv1.NewOptDateTime(v.FinishedAt)
	}
	for _, d := range v.DuplicateSample {
		out.DuplicateSample = append(out.DuplicateSample, apiv1.MigrationVerifyDuplicate{Path: d.Path, Disks: d.Disks})
	}
	return out
}

func verifyScopesToAPI(scopes []migrate.VerifyScope) []apiv1.MigrationVerifyScope {
	out := make([]apiv1.MigrationVerifyScope, 0, len(scopes))
	counts := func(c migrate.VerifyCounts) apiv1.MigrationVerifyCounts {
		return apiv1.MigrationVerifyCounts{Files: c.Files, Symlinks: c.Symlinks, Special: c.Special, Bytes: c.Bytes}
	}
	list := func(l migrate.VerifyList) apiv1.MigrationVerifyList {
		paths := l.Paths
		if paths == nil {
			paths = []string{}
		}
		return apiv1.MigrationVerifyList{Total: l.Total, Paths: paths}
	}
	for i := range scopes {
		sc := &scopes[i]
		out = append(out, apiv1.MigrationVerifyScope{
			Name: sc.Name, Passed: sc.Passed(), Problem: optString(sc.Problem), Expected: counts(sc.Expected), Found: counts(sc.Found),
			Hashed: sc.Hashed, Missing: list(sc.Missing), Extra: list(sc.Extra), SizeChanged: list(sc.SizeChanged),
			ChecksumChanged: list(sc.ChecksumChanged), Changed: list(sc.Changed),
		})
	}
	return out
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
	if r.Review != nil {
		out.Review = apiv1.NewOptMigrationReview(migrationReviewToAPI(r.Review))
	}
	return out
}

func migrationReviewToAPI(rv *migrate.Review) apiv1.MigrationReview {
	out := apiv1.MigrationReview{
		Disks:   make([]apiv1.MigrationDisk, 0, len(rv.Disks)),
		Shares:  make([]apiv1.MigrationSharePreview, 0, len(rv.Shares)),
		Capture: apiv1.MigrationCapture{State: apiv1.MigrationCaptureState(rv.Capture.State)},
	}
	for _, d := range rv.Disks {
		item := apiv1.MigrationDisk{
			Slot: optString(d.Slot), UnraidId: optString(d.UnraidID), Device: optString(d.Device),
			Serial: optString(d.Serial), Wwn: optString(d.WWN), ById: optString(d.ByID), Model: optString(d.Model),
			Filesystem: optString(d.Filesystem), Problem: optString(d.Problem), Refusal: optString(d.Refusal),
			Refused: d.Refused,
		}
		if d.DiskNumber > 0 {
			item.DiskNumber = apiv1.NewOptInt(d.DiskNumber)
		}
		if d.Size > 0 {
			item.Size = apiv1.NewOptInt64(d.Size)
		}
		if d.UnraidRole != "" {
			item.UnraidRole = apiv1.NewOptMigrationUnraidRole(apiv1.MigrationUnraidRole(d.UnraidRole))
		}
		if d.ProposedRole != "" {
			item.ProposedRole = apiv1.NewOptMigrationProposedRole(apiv1.MigrationProposedRole(d.ProposedRole))
		}
		if d.RefusalCode != "" {
			item.RefusalCode = apiv1.NewOptMigrationRefusalCode(apiv1.MigrationRefusalCode(d.RefusalCode))
		}
		if d.WeakIdentity != nil {
			item.WeakIdentity = apiv1.NewOptBool(*d.WeakIdentity)
		}
		if d.UnraidBoot {
			item.UnraidBoot = apiv1.NewOptBool(true)
		}
		if d.HostBoot != nil {
			item.HostBoot = apiv1.NewOptBool(*d.HostBoot)
		}
		out.Disks = append(out.Disks, item)
	}
	for _, sh := range rv.Shares {
		out.Shares = append(out.Shares, apiv1.MigrationSharePreview{
			Name: sh.Name, AllocationMethod: optString(sh.AllocationMethod), HighWater: sh.HighWater,
			Include: sh.Include, Exclude: sh.Exclude, WarningCount: sh.WarningCount,
		})
	}
	switch rv.Boot.Mode {
	case "usb", "internal":
		out.Boot.Mode = apiv1.NewOptMigrationBootMode(apiv1.MigrationBootMode(rv.Boot.Mode))
	}
	if rv.Boot.Mirrored != nil {
		out.Boot.Mirrored = apiv1.NewOptBool(*rv.Boot.Mirrored)
	}
	if rv.Boot.SharedWithCache != nil {
		out.Boot.SharedWithCache = apiv1.NewOptBool(*rv.Boot.SharedWithCache)
	}
	if rv.Capture.CapturedAt != nil {
		out.Capture.CapturedAt = apiv1.NewOptDateTime(*rv.Capture.CapturedAt)
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
