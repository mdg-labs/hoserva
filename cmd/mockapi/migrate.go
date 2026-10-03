package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/template"
	migrationpending "github.com/mdg-labs/hoserva/web/fixtures/migration-pending"
)

// mockFlashDevice is the Unraid USB stick this mock offers: a FAT filesystem
// labelled UNRAID that is neither the boot disk nor in the array. The mock reads
// no stick; like production it refuses any other device.
const mockFlashDevice = "/dev/sdu"

// mockMigration is the migration session this mock instance keeps: a report
// and the size of the zip it came from, or the device it was read from. Like
// production's session it holds the report only; the mock keeps no zip.
type mockMigration struct {
	mu         sync.Mutex
	report     *migrate.Report
	size       int64
	device     string
	receivedAt time.Time
}

func (m *mockMigration) zipOnly() bool {
	return m.report != nil && m.report.BootMode == "internal"
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
	case errors.Is(err, migrate.ErrNotFlashDevice):
		return errMigrationRefusal("invalid_flash_device", 400, err)
	case errors.Is(err, migrate.ErrZipOnly):
		return errMigrationRefusal("zip_only_source", 409, err)
	}
	return err
}

func errNoMigrationReport() error {
	return &mockError{code: "no_migration_report", statusCode: 404, message: migrate.ErrNoReport.Error()}
}

// mockMigrationReport is a completed scan of a healthy single-parity array:
// what the UI shows after a scan, with one SMART finding so a flagged row has
// something to render, and one row group per part of the configuration
// inventory, including the flagged containers and shares.
func mockMigrationReport(version string, unverified bool, at time.Time) *migrate.Report {
	r := &migrate.Report{GeneratedAt: at, UnraidVersion: version, UnverifiedLayout: unverified, BootMode: "usb"}
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
	add(migrate.CheckParityHistory, migrate.StatusPass, "", "The last parity check, on 2026-09-25 (from the capture's var.ini), completed clean with 0 errors.")
	add(migrate.CheckShares, migrate.StatusInfo, "", "3 shares configured. Each share's allocation method and cache setting are mapped below (Q11).")
	add(migrate.CheckShares, migrate.StatusFlag, "media", "allocation High-water: no exact equivalent, mapped to Balance across disks (mfs) (Q11); cache setting no maps to array-only; exported over SMB (e); directory on disk1.")
	add(migrate.CheckShares, migrate.StatusInfo, "backup", "allocation Fill-up maps to Fill disks in order (ff); cache setting no maps to array-only; not exported over SMB; directory on disk1.")
	add(migrate.CheckShares, migrate.StatusInfo, "documents", "allocation Most-free maps to Balance across disks (mfs); cache setting yes maps to cache-then-move; exported over SMB (e); directory on disk1.")
	add(migrate.CheckCache, migrate.StatusWarn, "appdata", "Docker keeps container data under /mnt/user/appdata/; cache setting prefer; its directory is on disk1, pool cache. Phase A step 5 must move it to the array before the cache is re-created.")
	add(migrate.CheckCache, migrate.StatusInfo, "Docker storage", "Docker's directory (/mnt/user/system/docker/dockerdir) is on the cache. It is not moved: images are pulled again when containers are recreated. What does not come back is each container's writable layer (doc 04 §5). No container's writable layer holds data.")
	add(migrate.CheckUsers, migrate.StatusInfo, "", "2 user accounts: alice, bob. Names only are read; passwords cannot be carried over, so each is set again at the import (doc 05 §4 step 4).")
	add(migrate.CheckTemplates, migrate.StatusInfo, "", "2 templates parsed: 1 autostart, 1 running, 0 stopped, 0 template only. 2 are installed; a template with no container is a record of an app once installed.")
	add(migrate.CheckTemplates, migrate.StatusInfo, "", "Of the 2 installed templates, 1 convert cleanly and 1 with warnings (Q36).")
	add(migrate.CheckTemplates, migrate.StatusInfo, "gateway", "Converts with 2 warnings to review (flagged_path, missing_network). Open its preview before recreating the container.")
	add(migrate.CheckContainers, migrate.StatusInfo, "", "1 Compose Manager project previewed with its own compose.yaml and not converted.")
	add(migrate.CheckContainers, migrate.StatusInfo, "", "8 containers in the capture: 6 from the Docker page (dockerMan), 1 from Compose Manager, 1 created by hand.")
	add(migrate.CheckContainers, migrate.StatusFlag, "dbtool", "A dockerMan container with no template whose <Name> matches. It cannot be converted: open it on the Docker page, edit it and apply to save its template, then run the prepare script again (doc 05 §4 step 2).")
	add(migrate.CheckContainers, migrate.StatusFlag, "handmade", "Created by hand (docker run), so it has no template to convert. Recreate it from its run command.")
	add(migrate.CheckUserScripts, migrate.StatusInfo, "", "1 User Scripts entry found. They are listed, never executed or translated (Q83): recreate what is still wanted as a cron job or systemd timer (doc 05 §4 step 24).")
	add(migrate.CheckUserScripts, migrate.StatusInfo, "nightly-report", "Scheduled in customSchedule.cron (30 2 * * *), so it runs while enabled.")
	add(migrate.CheckPlugins, migrate.StatusInfo, "user.scripts", "Installed. Hoserva counterpart: its scripts are listed, never executed or translated (Q83).")
	add(migrate.CheckCustomConfig, migrate.StatusWarn, "smb-extra.conf", "2 lines of custom Samba configuration. It is not imported: look at the lines on the Unraid server and recreate any that are still wanted.")
	add(migrate.CheckSettings, migrate.StatusInfo, "mover schedule", "40 3 * * *. It can be offered as Hoserva's mover schedule.")
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

	report := mockMigrationReport(flash.Version, flash.LayoutProblem() != "", now)
	if flash.Capture != nil {
		report.BootMode = flash.Capture.Boot.Mode
	}
	h.migration.mu.Lock()
	h.migration.report = report
	h.migration.size, h.migration.device, h.migration.receivedAt = size.n, "", now
	h.migration.mu.Unlock()
	return j, nil
}

// StartMigrationDeviceScan answers as production does for the stick: a device
// that is not the one UNRAID-labelled FAT disk on offer is refused, and so is
// any stick once the session's capture says Unraid booted internally.
func (h *handler) StartMigrationDeviceScan(ctx context.Context, req *apiv1.StartMigrationDeviceScanReq) (*apiv1.Job, error) {
	if req == nil || req.Device == "" {
		return nil, &mockError{code: "invalid_flash_device", statusCode: 400, message: "the device is required"}
	}
	h.migration.mu.Lock()
	zipOnly := h.migration.zipOnly()
	h.migration.mu.Unlock()
	if zipOnly {
		return nil, errMigrationRefusal("zip_only_source", 409, migrate.ErrZipOnly)
	}
	if req.Device != mockFlashDevice {
		return nil, errMigrationRefusal("invalid_flash_device", 400, fmt.Errorf("%w: %s is not a FAT filesystem labelled UNRAID on a disk outside the array", migrate.ErrNotFlashDevice, req.Device))
	}
	j, err := h.queueMockJobIn(apiv1.JobTypeMigrationScan, apiv1.JobClassTopology)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Second)
	h.mu.Lock()
	j.Status = apiv1.JobStatusSucceeded
	j.StartedAt = apiv1.NewOptNilDateTime(now)
	j.FinishedAt = apiv1.NewOptNilDateTime(now)
	h.jobs[j.ID] = *j
	h.mu.Unlock()

	h.migration.mu.Lock()
	h.migration.report = mockMigrationReport("7.3.2", req.UnverifiedLayout.Or(false), now)
	h.migration.size, h.migration.device, h.migration.receivedAt = 0, req.Device, now
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
	out := &apiv1.Migration{Phase: apiv1.MigrationPhaseNone, FlashDevices: []apiv1.MigrationFlashDevice{}, ZipOnly: h.migration.zipOnly()}
	if r := h.migration.report; r != nil {
		out.Phase = apiv1.MigrationPhaseScanned
		out.SourceReceivedAt = apiv1.NewOptDateTime(h.migration.receivedAt)
		if h.migration.device != "" {
			out.SourceDevice = apiv1.NewOptString(h.migration.device)
		} else {
			out.SourceSize = apiv1.NewOptInt64(h.migration.size)
		}
		out.Report = apiv1.NewOptMigrationReport(mockMigrationReportToAPI(r))
	}
	if !out.ZipOnly {
		out.FlashDevices = append(out.FlashDevices, apiv1.MigrationFlashDevice{
			Device: mockFlashDevice, Size: 16 * disk.GB,
			Model: apiv1.NewOptString("SanDisk Cruzer Fit"), Serial: apiv1.NewOptString("4C530001240603119335"),
		})
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
	h.migration.report, h.migration.size, h.migration.device, h.migration.receivedAt = nil, 0, "", time.Time{}
	return nil
}

// mockComposeProject is the compose.yaml of the one Compose Manager project the
// mock's report lists.
const mockComposeProject = "services:\n  web:\n    image: example/web:1.0\n    container_name: stack-web\n    volumes:\n      - /mnt/user/appdata/stack:/data\n"

// mockMigrationTemplate is one template of the migration-pending scenario, converted by
// the production converter with the capture's br0 network.
type mockMigrationTemplate struct {
	file     string
	class    apiv1.MigrationTemplateClass
	position int
	conv     *template.Conversion
}

func mockMigrationTemplates() ([]mockMigrationTemplate, error) {
	classes := map[string]mockMigrationTemplate{
		"my-photos.xml":  {class: apiv1.MigrationTemplateClassAutostart, position: 1},
		"my-gateway.xml": {class: apiv1.MigrationTemplateClassRunning},
	}
	networks := []template.NetworkDef{{
		Name: "br0", Driver: "ipvlan", Subnet: "192.168.50.0/24", Gateway: "192.168.50.1", Parent: "ens20",
		Options: map[string]string{"ipvlan_mode": "l2", "parent": "ens20"},
	}}
	entries, err := fs.ReadDir(migrationpending.Templates, ".")
	if err != nil {
		return nil, err
	}
	var out []mockMigrationTemplate
	for _, e := range entries {
		data, err := fs.ReadFile(migrationpending.Templates, e.Name())
		if err != nil {
			return nil, err
		}
		conv, err := template.ConvertUnraid(data, template.ConvertOptions{Networks: networks})
		if err != nil {
			return nil, fmt.Errorf("converting the fixture template %s: %w", e.Name(), err)
		}
		t := classes[e.Name()]
		t.file, t.conv = e.Name(), conv
		out = append(out, t)
	}
	return out, nil
}

func actionWarnings(c *template.Conversion) int {
	n := 0
	for _, w := range c.Warnings {
		if w.Class != template.WarnWritableLayer && w.Class != template.WarnNote {
			n++
		}
	}
	return n
}

func (h *handler) requireMigrationReport() error {
	h.migration.mu.Lock()
	defer h.migration.mu.Unlock()
	if h.migration.report == nil {
		return errNoMigrationReport()
	}
	return nil
}

func (h *handler) ListMigrationTemplates(ctx context.Context) (*apiv1.MigrationTemplates, error) {
	if err := h.requireMigrationReport(); err != nil {
		return nil, err
	}
	templates, err := mockMigrationTemplates()
	if err != nil {
		return nil, err
	}
	out := &apiv1.MigrationTemplates{
		Templates: make([]apiv1.MigrationTemplateSummary, 0, len(templates)),
		ComposeProjects: []apiv1.MigrationComposeProjectSummary{
			{Name: "stack", Containers: []string{"stack-web"}, Status: apiv1.MigrationTemplateStatusPreviewed},
		},
	}
	out.Counts.ComposeProjects = 1
	for _, t := range templates {
		status := apiv1.MigrationTemplateStatusClean
		if !t.conv.Clean() {
			status = apiv1.MigrationTemplateStatusWarnings
			out.Counts.WithWarnings++
		} else {
			out.Counts.Clean++
		}
		item := apiv1.MigrationTemplateSummary{
			Name: t.conv.Metadata.Title, File: t.file, Class: t.class, Counted: true,
			Status: status, WarningCount: actionWarnings(t.conv),
		}
		if t.position > 0 {
			item.AutostartPosition = apiv1.NewOptInt(t.position)
		}
		out.Templates = append(out.Templates, item)
	}
	return out, nil
}

func (h *handler) GetMigrationTemplate(ctx context.Context, params apiv1.GetMigrationTemplateParams) (*apiv1.MigrationTemplatePreview, error) {
	if err := h.requireMigrationReport(); err != nil {
		return nil, err
	}
	h.migration.mu.Lock()
	fromStick := h.migration.device != ""
	h.migration.mu.Unlock()
	if fromStick {
		return nil, errMigrationRefusal("template_source_unavailable", 409, migrate.ErrSourceUnavailable)
	}
	opt := func(s string) apiv1.OptString {
		if s == "" {
			return apiv1.OptString{}
		}
		return apiv1.NewOptString(s)
	}
	if params.Name == "stack" {
		return &apiv1.MigrationTemplatePreview{
			Kind: apiv1.MigrationTemplatePreviewKindComposeProject, Name: "stack", Status: apiv1.MigrationTemplateStatusPreviewed,
			Source: mockComposeProject, Compose: apiv1.NewOptString(mockComposeProject),
			Warnings: []apiv1.ConversionWarning{}, Privileges: []apiv1.TemplatePrivilege{},
		}, nil
	}
	templates, err := mockMigrationTemplates()
	if err != nil {
		return nil, err
	}
	for _, t := range templates {
		if t.file != params.Name {
			continue
		}
		out := &apiv1.MigrationTemplatePreview{
			Kind: apiv1.MigrationTemplatePreviewKindTemplate, Name: t.file, Title: apiv1.NewOptString(t.conv.Metadata.Title),
			Class: apiv1.NewOptMigrationTemplateClass(t.class), Counted: apiv1.NewOptBool(true), Status: apiv1.MigrationTemplateStatusClean,
			Source: t.conv.Source, Compose: apiv1.NewOptString(t.conv.Compose),
			Warnings: make([]apiv1.ConversionWarning, len(t.conv.Warnings)), Privileges: make([]apiv1.TemplatePrivilege, len(t.conv.Privileges)),
		}
		if !t.conv.Clean() {
			out.Status = apiv1.MigrationTemplateStatusWarnings
		}
		for i, w := range t.conv.Warnings {
			out.Warnings[i] = apiv1.ConversionWarning{Class: apiv1.ConversionWarningClass(w.Class), Message: w.Message, Detail: opt(w.Detail), Command: opt(w.Command)}
		}
		for i, pr := range t.conv.Privileges {
			out.Privileges[i] = apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description, Detail: opt(pr.Detail)}
		}
		return out, nil
	}
	return nil, &mockError{code: "template_not_found", statusCode: 404, message: migrate.ErrTemplateNotFound.Error()}
}
