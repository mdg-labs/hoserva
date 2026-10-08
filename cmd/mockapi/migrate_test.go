package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/migrate"
)

// contractFlashFiles is a minimal Flash Backup: a version, a kernel and a
// disk.cfg, as the migrator's allowlist asks for.
func contractFlashFiles(version string, mutate func(map[string]string)) map[string]string {
	files := map[string]string{
		"changes.txt":     "# Version " + version + " 2026-01-01\n",
		"bzimage":         "kernel",
		"config/disk.cfg": "startArray=\"yes\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
	}
	if mutate != nil {
		mutate(files)
	}
	return files
}

func contractFlashZip(version string, mutate func(map[string]string)) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range contractFlashFiles(version, mutate) {
		w, err := zw.Create(name)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func contractScanRequest(data []byte, unverified bool) *apiv1.StartMigrationScanReq {
	req := &apiv1.StartMigrationScanReq{File: ht.MultipartFile{Name: "boot.zip", File: bytes.NewReader(data)}}
	if unverified {
		req.UnverifiedLayout = apiv1.NewOptBool(true)
	}
	return req
}

// contractAwaitScanned waits until the scan the case started has finished: the
// mock finishes at once, production's job runs in the scheduler.
func contractAwaitScanned(ctx context.Context, h apiv1.Handler) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m, err := h.GetMigration(ctx)
		if err != nil {
			return err
		}
		switch m.Phase {
		case apiv1.MigrationPhaseScanned:
			return nil
		case apiv1.MigrationPhaseScanFailed:
			return fmt.Errorf("the scan failed: %s", m.ScanError.Or(""))
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("the scan did not finish")
}

func TestMockMigration_StartsEmptyExceptInTheMigrationPendingScenario(t *testing.T) {
	ctx := context.Background()
	for scenario, want := range map[string]apiv1.MigrationPhase{
		"healthy":           apiv1.MigrationPhaseNone,
		"fresh-install":     apiv1.MigrationPhaseNone,
		"migration-pending": apiv1.MigrationPhaseScanned,
	} {
		h, err := newHandler(scenario)
		if err != nil {
			t.Fatal(err)
		}
		m, err := h.GetMigration(ctx)
		if err != nil || m.Phase != want {
			t.Fatalf("%s: GetMigration = %+v, %v; want phase %s", scenario, m, err, want)
		}
	}

	h, _ := newHandler("migration-pending")
	m, _ := h.GetMigration(ctx)
	report, ok := m.Report.Get()
	if !ok || len(report.Rows) == 0 || report.Verdict != apiv1.MigrationVerdictNoGo {
		t.Fatalf("the migration-pending session has no completed report that refuses a disk: %+v", m)
	}
	refused := ""
	for _, row := range report.Rows {
		if row.Status == apiv1.MigrationCheckStatusRefuse && row.Check == "disk_integrity" {
			refused = row.Subject.Or("")
		}
	}
	if refused != "disk3" {
		t.Errorf("the migration-pending report refuses %q, want its disk3 with a failed filesystem check", refused)
	}
	doc, err := h.GetMigrationReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := io.ReadAll(doc.Data)
	if !strings.HasPrefix(string(md), "# Hoserva migration scan report") {
		t.Errorf("report document:\n%s", md)
	}
}

// The migration-pending report carries a row of every part of the configuration
// inventory, in both the operation's answer and the downloadable document.
func TestMockMigration_TheReportIncludesTheConfigurationInventory(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	m, _ := h.GetMigration(ctx)
	report, _ := m.Report.Get()
	checks := map[string]apiv1.MigrationCheckStatus{}
	flagged := map[string]bool{}
	for _, row := range report.Rows {
		checks[row.Check] = row.Status
		if row.Status == apiv1.MigrationCheckStatusFlag {
			flagged[row.Subject.Or("")] = true
		}
	}
	for _, check := range []string{"data_disks", "disk_integrity", "baseline", "content_space", "parity_history", "shares", "cache_contents", "users", "docker_templates", "containers", "user_scripts", "plugins", "custom_config", "settings"} {
		if _, ok := checks[check]; !ok {
			t.Errorf("the report has no %s row", check)
		}
	}
	for _, subject := range []string{"media", "dbtool", "handmade"} {
		if !flagged[subject] {
			t.Errorf("%s is not a flagged row", subject)
		}
	}
	doc, err := h.GetMigrationReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := io.ReadAll(doc.Data)
	for _, heading := range []string{"## Share configuration", "## Docker templates", "## User Scripts (plugin)", "## Last Unraid parity check"} {
		if !strings.Contains(string(md), heading) {
			t.Errorf("the document lacks %q", heading)
		}
	}
}

func TestMockMigration_ScanQueuesAFinishedTopologyJobAndForgetClearsTheSession(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("healthy")
	j, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("6.9.2", nil), true))
	if err != nil {
		t.Fatal(err)
	}
	if j.Type != apiv1.JobTypeMigrationScan || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %s %s, want a migration_scan topology job", j.Type, j.Class)
	}
	got, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID})
	if err != nil || got.Status != apiv1.JobStatusSucceeded {
		t.Fatalf("the scan job = %+v, %v; the mock has no scheduler, so it must already be finished", got, err)
	}
	m, _ := h.GetMigration(ctx)
	if report, _ := m.Report.Get(); m.Phase != apiv1.MigrationPhaseScanned || !report.UnverifiedLayout || report.UnraidVersion.Or("") != "6.9.2" {
		t.Errorf("session after the scan = %+v", m)
	}
	if err := h.ForgetMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseNone {
		t.Errorf("phase after forget = %s", m.Phase)
	}
}

// The mock offers the stick GetMigration lists and refuses every other device,
// as production does; a zip whose capture says Unraid booted internally leaves
// the zip as the only source.
func TestMockMigration_OffersOneUNRAIDStickAndRefusesAnyOtherDevice(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("healthy")
	m, _ := h.GetMigration(ctx)
	if len(m.FlashDevices) != 1 || m.FlashDevices[0].Device != mockFlashDevice || m.ZipOnly {
		t.Fatalf("GetMigration offers %+v (zipOnly %v), want %s", m.FlashDevices, m.ZipOnly, mockFlashDevice)
	}
	for _, dev := range []string{"/dev/sdb", "/dev/sdf", mockFlashDevice + "1", ""} {
		_, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: dev})
		if status := h.NewError(ctx, err); status.StatusCode != 400 || status.Response.Code != "invalid_flash_device" {
			t.Errorf("StartMigrationDeviceScan(%q) = %v, want 400 invalid_flash_device", dev, err)
		}
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseNone {
		t.Fatalf("a refused device changed the session: %+v", m)
	}

	if _, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: mockFlashDevice}); err != nil {
		t.Fatal(err)
	}
	m, _ = h.GetMigration(ctx)
	if m.Phase != apiv1.MigrationPhaseScanned || m.SourceDevice.Or("") != mockFlashDevice || m.SourceSize.Set {
		t.Fatalf("session after the stick scan = %+v, want a scanned session sourced from the device with no zip size", m)
	}

	zipData := contractFlashZip("7.3.2", func(f map[string]string) {
		f["config/hoserva/capture.json"] = `{"boot":{"mode":"internal","filesystem":"zfs","devices":[]}}`
	})
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(zipData, false)); err != nil {
		t.Fatal(err)
	}
	m, _ = h.GetMigration(ctx)
	if !m.ZipOnly || len(m.FlashDevices) != 0 || m.SourceDevice.Set || !m.SourceSize.Set {
		t.Fatalf("session after an internal-boot zip = %+v, want zip only with no stick", m)
	}
	_, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: mockFlashDevice})
	if status := h.NewError(ctx, err); status.StatusCode != 409 || status.Response.Code != "zip_only_source" {
		t.Fatalf("StartMigrationDeviceScan = %v, want 409 zip_only_source", err)
	}
}

// An upload past the scan body limit is refused over HTTP the way the daemon
// refuses it: 413 zip_too_large, not the generic request_too_large or a decode
// error. A zip well past the 64 KiB general limit is taken.
func TestMockMigration_ARequestBodyPastTheScanLimitIsRefusedAsTooLarge(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	const limit = 256 << 10
	srv, err := newAPIServerWithScanLimit(h, limit)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client, err := apiv1.NewClient(ts.URL+"/api/v1", staticSecurity{})
	if err != nil {
		t.Fatal(err)
	}

	padded := func(n int) []byte {
		pad := make([]byte, n)
		_, _ = rand.Read(pad)
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for name, content := range map[string]string{
			"changes.txt":     "# Version 7.3.2 2026-01-01\n",
			"bzimage":         "kernel",
			"config/disk.cfg": "startArray=\"yes\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
		} {
			w, _ := zw.Create(name)
			_, _ = w.Write([]byte(content))
		}
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: "extra/pad.bin", Method: zip.Store})
		_, _ = w.Write(pad)
		_ = zw.Close()
		return buf.Bytes()
	}

	if _, err := client.StartMigrationScan(context.Background(), contractScanRequest(padded(128<<10), false)); err != nil {
		t.Fatalf("an upload under the limit and over 64 KiB = %v, want it taken", err)
	}
	_, err = client.StartMigrationScan(context.Background(), contractScanRequest(padded(2*limit), false))
	var apiErr *apiv1.ErrorStatusCode
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 413 || apiErr.Response.Code != "zip_too_large" {
		t.Fatalf("an upload past the limit = %v, want 413 zip_too_large", err)
	}
}

// The scenario's templates are converted by the production converter: one
// converts cleanly and one with warnings, and the report's rows agree with the
// operations' answers.
func TestMockMigration_TemplatePreviews(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	list, err := h.ListMigrationTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c := list.Counts; c.Clean != 1 || c.WithWarnings != 1 || c.Failed != 0 || c.TemplateOnly != 0 || c.AllTemplates || c.ComposeProjects != 1 {
		t.Fatalf("counts = %+v, want one clean, one with warnings and one project", c)
	}
	byFile := map[string]apiv1.MigrationTemplateSummary{}
	for _, it := range list.Templates {
		byFile[it.File] = it
	}
	if it := byFile["my-photos.xml"]; it.Status != apiv1.MigrationTemplateStatusClean || it.WarningCount != 0 || it.Class != apiv1.MigrationTemplateClassAutostart {
		t.Errorf("my-photos.xml = %+v", it)
	}
	gateway := byFile["my-gateway.xml"]
	if gateway.Status != apiv1.MigrationTemplateStatusWarnings || gateway.WarningCount != 2 {
		t.Fatalf("my-gateway.xml = %+v", gateway)
	}

	pv, err := h.GetMigrationTemplate(ctx, apiv1.GetMigrationTemplateParams{Name: "my-gateway.xml"})
	if err != nil {
		t.Fatal(err)
	}
	var command string
	for _, w := range pv.Warnings {
		if w.Class == apiv1.ConversionWarningClassMissingNetwork {
			command = w.Command.Or("")
		}
	}
	if !strings.Contains(command, "-d ipvlan --subnet 192.168.50.0/24 --gateway 192.168.50.1 -o parent=ens20") {
		t.Errorf("network command = %q, want the capture's exact one", command)
	}
	if pv.Compose.Or("") == "" || !strings.Contains(pv.Source, "<Name>gateway</Name>") {
		t.Errorf("preview = %+v", pv)
	}

	m, _ := h.GetMigration(ctx)
	report, _ := m.Report.Get()
	var found bool
	for _, row := range report.Rows {
		if row.Check == "docker_templates" && row.Subject.Or("") == "gateway" && strings.Contains(row.Detail, "2 warnings") {
			found = true
		}
	}
	if !found {
		t.Error("the report's docker_templates rows do not agree with the gateway's preview")
	}

	if _, err := h.GetMigrationTemplate(ctx, apiv1.GetMigrationTemplateParams{Name: "nothing.xml"}); err == nil || !strings.Contains(err.Error(), "no template") {
		t.Errorf("an unknown template = %v, want template_not_found", err)
	}
	if _, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: mockFlashDevice}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetMigrationTemplate(ctx, apiv1.GetMigrationTemplateParams{Name: "my-gateway.xml"}); err == nil || !strings.Contains(err.Error(), "not kept") {
		t.Errorf("a preview of a report made from the stick = %v, want template_source_unavailable", err)
	}
	empty, _ := newHandler("healthy")
	if _, err := empty.ListMigrationTemplates(ctx); err == nil {
		t.Error("ListMigrationTemplates answered with no report")
	}
}

func servedReview(t *testing.T, h apiv1.Handler) apiv1.MigrationReview {
	t.Helper()
	m, err := h.GetMigration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report, ok := m.Report.Get()
	review, hasReview := report.Review.Get()
	if !ok || !hasReview {
		t.Fatalf("GetMigration has no report with a review: %+v", m)
	}
	return review
}

// The migration-pending session carries the states the Review step's tests
// need: a refused disk, a weak-identity disk, a slot with no disk, a boot
// device, a disk the capture does not name, a High-water share and a fresh
// capture of a USB-booted server.
func TestMockMigration_TheSeededReviewHasTheStatesTheReviewStepNeeds(t *testing.T) {
	h, _ := newHandler("migration-pending")
	review := servedReview(t, h)

	bySlot := map[string]apiv1.MigrationDisk{}
	var unnamed []apiv1.MigrationDisk
	for _, d := range review.Disks {
		if slot, ok := d.Slot.Get(); ok && slot != "boot" {
			bySlot[slot] = d
		} else if !ok {
			unnamed = append(unnamed, d)
		}
	}
	refused := bySlot["disk3"]
	if !refused.Refused || refused.RefusalCode.Or("") != apiv1.MigrationRefusalCodeIntegrityCheck || !strings.Contains(refused.Refusal.Or(""), "disk3 is not adopted") || refused.ProposedRole.Set {
		t.Errorf("disk3 = %+v, want refused as integrity_check and proposed no role", refused)
	}
	if weak := bySlot["disk4"]; !weak.WeakIdentity.Value || !weak.WeakIdentity.Set || weak.Refused || weak.ProposedRole.Or("") != apiv1.MigrationProposedRoleData {
		t.Errorf("disk4 = %+v, want a weak identity that is not refused and is proposed data", weak)
	}
	if missing := bySlot["disk2"]; missing.Device.Set || missing.WeakIdentity.Set || missing.Problem.Or("") == "" || missing.Refused || missing.ProposedRole.Set {
		t.Errorf("disk2 = %+v, want a slot with no disk here and no proposed role", missing)
	}
	if p := bySlot["parity"]; p.UnraidRole.Or("") != apiv1.MigrationUnraidRoleParity || p.ProposedRole.Or("") != apiv1.MigrationProposedRoleParity || p.Filesystem.Or("") != "xfs" {
		t.Errorf("parity = %+v", p)
	}
	var stick *apiv1.MigrationDisk
	for i, d := range review.Disks {
		if d.Slot.Or("") == "boot" {
			stick = &review.Disks[i]
		}
	}
	if stick == nil || stick.Device.Or("") != mockFlashDevice || stick.ProposedRole.Or("") != apiv1.MigrationProposedRoleIgnore {
		t.Errorf("the stick = %+v, want a boot row that can only be ignored", stick)
	}
	if len(unnamed) != 1 || unnamed[0].UnraidRole.Or("") != apiv1.MigrationUnraidRoleUnassigned {
		t.Errorf("unnamed disks = %+v, want one unassigned", unnamed)
	}
	for _, d := range review.Disks {
		if d.HostBoot.Set != d.Device.Set || d.HostBoot.Value {
			t.Errorf("%+v: hostBoot is known and false exactly where a disk of this machine matched, since the seeded machine boots from none of them", d)
		}
	}

	var highWater, plain int
	for _, sh := range review.Shares {
		if sh.HighWater {
			highWater++
			if sh.Name != "media" || sh.AllocationMethod.Or("") != "highwater" || sh.WarningCount != 1 {
				t.Errorf("High-water share = %+v", sh)
			}
		} else {
			plain++
		}
	}
	if highWater != 1 || plain != 2 {
		t.Errorf("shares = %+v, want one High-water and two others", review.Shares)
	}
	if review.Boot.Mode.Or("") != apiv1.MigrationBootModeUsb || review.Boot.Mirrored.Set || review.Boot.SharedWithCache.Set {
		t.Errorf("boot = %+v, want usb with no layout flags", review.Boot)
	}
	if review.Capture.State != apiv1.MigrationCaptureStatePresent || !review.Capture.CapturedAt.Set {
		t.Errorf("capture = %+v, want present at a time", review.Capture)
	}

	// The prose rows say the same: the refused disk's refusal is the row's text.
	m, _ := h.GetMigration(context.Background())
	report, _ := m.Report.Get()
	for _, row := range report.Rows {
		if row.Check == "disk_integrity" && row.Subject.Or("") == "disk3" && row.Detail != refused.Refusal.Or("") {
			t.Errorf("disk3's refusal %q is not the row's %q", refused.Refusal.Or(""), row.Detail)
		}
	}
}

func contractFlashZipWithTimes(version string, files map[string]string, times map[string]time.Time) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range contractFlashFiles(version, func(f map[string]string) {
		for n, c := range files {
			f[n] = c
		}
	}) {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: times[name]}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			panic(err)
		}
		_, _ = w.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// A scanned zip gives the boot mode and the capture's state production gives it:
// internal boot with its layout (shared with the cache or on a device of its
// own, mirrored or not), and a capture that is missing, unreadable, stale or
// present, with no stick on offer once Unraid booted internally.
func TestMockMigration_AScannedZipGivesTheBootModeAndCaptureStateProductionGives(t *testing.T) {
	const tmpl = "config/plugins/dockerMan/templates-user/my-notes.xml"
	captured := time.Date(2026, 10, 3, 7, 2, 18, 0, time.UTC)
	capture := func(boot string) map[string]string {
		return map[string]string{"config/hoserva/capture.json": `{"captured_at":"` + captured.Format(time.RFC3339) + `","boot":` + boot + `}`}
	}
	internal := func(mirrored, shared bool) string {
		return fmt.Sprintf(`{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"BOOT1","model":"Boot SSD","size":"500G"}],"mirrored":%v,"shared_with_data_pool":%v}`, mirrored, shared)
	}
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		files    map[string]string
		times    map[string]time.Time
		mode     apiv1.MigrationBootMode
		mirrored *bool
		shared   *bool
		state    apiv1.MigrationCaptureState
	}{
		{"usb", capture(`{"mode":"usb","devices":[]}`), nil, apiv1.MigrationBootModeUsb, nil, nil, apiv1.MigrationCaptureStatePresent},
		{"internal on its own device", capture(internal(false, false)), nil, apiv1.MigrationBootModeInternal, &no, &no, apiv1.MigrationCaptureStatePresent},
		{"internal sharing its disk with the cache", capture(internal(false, true)), nil, apiv1.MigrationBootModeInternal, &no, &yes, apiv1.MigrationCaptureStatePresent},
		{"internal mirrored pair", capture(internal(true, false)), nil, apiv1.MigrationBootModeInternal, &yes, &no, apiv1.MigrationCaptureStatePresent},
		{"stale capture", func() map[string]string {
			f := capture(`{"mode":"usb"}`)
			f[tmpl] = "<Container><Name>notes</Name></Container>"
			return f
		}(), map[string]time.Time{tmpl: captured.Add(time.Hour)}, apiv1.MigrationBootModeUsb, nil, nil, apiv1.MigrationCaptureStateStale},
		{"missing capture", nil, nil, "", nil, nil, apiv1.MigrationCaptureStateMissing},
		{"unreadable capture", map[string]string{"config/hoserva/capture.json": "{not json"}, nil, "", nil, nil, apiv1.MigrationCaptureStateUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHandler("healthy")
			if _, err := h.StartMigrationScan(context.Background(), contractScanRequest(contractFlashZipWithTimes("7.3.2", tc.files, tc.times), false)); err != nil {
				t.Fatal(err)
			}
			review := servedReview(t, h)
			if review.Boot.Mode.Or("") != tc.mode || review.Boot.Mirrored.Set != (tc.mirrored != nil) || review.Boot.SharedWithCache.Set != (tc.shared != nil) {
				t.Errorf("boot = %+v, want mode %q mirrored %v shared %v", review.Boot, tc.mode, tc.mirrored, tc.shared)
			}
			if tc.mirrored != nil && review.Boot.Mirrored.Value != *tc.mirrored || tc.shared != nil && review.Boot.SharedWithCache.Value != *tc.shared {
				t.Errorf("boot = %+v, want mirrored %v shared %v", review.Boot, *tc.mirrored, *tc.shared)
			}
			if review.Capture.State != tc.state {
				t.Errorf("capture = %+v, want %s", review.Capture, tc.state)
			}
			if review.Capture.CapturedAt.Set != (tc.state == apiv1.MigrationCaptureStatePresent || tc.state == apiv1.MigrationCaptureStateStale) {
				t.Errorf("capture = %+v: the time is given when the capture was read, and only then", review.Capture)
			}

			// The stick is attached whatever the boot mode, as production lists any
			// stick it finds. An internal boot's device is a boot row of its own,
			// unless it is shared with the cache, when it is the pool's row marked
			// as the boot device instead; no device is in the table twice.
			var sticks, boots int
			seen := map[string]int{}
			var pool apiv1.MigrationDisk
			for _, d := range review.Disks {
				if dev := d.Device.Or(""); dev != "" {
					seen[dev]++
				}
				if d.Slot.Or("") == "pool cache" {
					pool = d
				}
				if d.UnraidRole.Or("") == apiv1.MigrationUnraidRoleBoot {
					boots++
					if d.Device.Or("") == mockFlashDevice {
						sticks++
					}
				}
			}
			for dev, n := range seen {
				if n != 1 {
					t.Errorf("%s is in the table %d times: %+v", dev, n, review.Disks)
				}
			}
			internal := tc.mode == apiv1.MigrationBootModeInternal
			shared := tc.shared != nil && *tc.shared
			wantBoots := 1
			if internal && !shared {
				wantBoots = 2
			}
			if sticks != 1 || boots != wantBoots {
				t.Errorf("boot rows = %d (%d the stick), want %d with the stick always there: %+v", boots, sticks, wantBoots, review.Disks)
			}
			for _, d := range review.Disks {
				wantBoot := internal && shared && d.Slot.Or("") == "pool cache"
				if d.HostBoot.Set != d.Device.Set || d.HostBoot.Value != wantBoot {
					t.Errorf("%+v: hostBoot is true only on the shared NVMe's row, and known wherever a disk matched", d)
				}
			}
			if internal && shared {
				if pool.Serial.Or("") != "BOOT1" || !pool.UnraidBoot.Value || pool.ProposedRole.Or("") != apiv1.MigrationProposedRoleCache || !pool.Device.Set {
					t.Errorf("pool cache = %+v, want the attached boot disk, proposed cache and marked as the Unraid boot device", pool)
				}
			} else if pool.UnraidBoot.Set {
				t.Errorf("pool cache = %+v, marked as the boot device without sharing its disk", pool)
			}
			if internal && !shared {
				for _, d := range review.Disks {
					if d.Serial.Or("") == "BOOT1" && (!d.Device.Set || d.ProposedRole.Or("") != apiv1.MigrationProposedRoleIgnore) {
						t.Errorf("the boot device = %+v, want it attached and only ignored", d)
					}
				}
			}
		})
	}
}

// A device scan answers as the seeded session does, and forgetting the session
// leaves no review behind.
func TestMockMigration_ADeviceScanAndAForgetKeepTheReviewConsistent(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("healthy")
	if _, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: mockFlashDevice}); err != nil {
		t.Fatal(err)
	}
	if review := servedReview(t, h); review.Boot.Mode.Or("") != apiv1.MigrationBootModeUsb || review.Capture.State != apiv1.MigrationCaptureStatePresent {
		t.Errorf("review after a stick scan = %+v", review)
	}
	if err := h.ForgetMigration(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := h.GetMigration(ctx); m.Report.Set {
		t.Errorf("a forgotten session still has a report: %+v", m)
	}
}

func mockImportRole(role apiv1.MigrationImportRole, serial string) apiv1.MigrationImportDisk {
	return apiv1.MigrationImportDisk{Role: role, Serial: apiv1.NewOptString(serial)}
}

func mockImportAll() []apiv1.MigrationImportDisk {
	return []apiv1.MigrationImportDisk{
		mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_PARITY"),
		mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1"),
		mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK3"),
		mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK4"),
		mockImportRole(apiv1.MigrationImportRoleCache, "EXAMPLE_CACHE"),
	}
}

func mockErrCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var me *mockError
	if !errors.As(err, &me) {
		t.Fatalf("error %v is not a mock error", err)
	}
	return me.statusCode, me.code
}

// The seeded session's report refuses disk3, so production would refuse an
// import from it; a rescan finds the disk repaired, and the import can go ahead
// and leaves the session imported, which refuses what production refuses.
func TestMockMigration_TheMigrationPendingScenarioAdvancesToAnImportedState(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")

	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Roles: mockImportAll()}); err == nil {
		t.Fatal("an import without confirm was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "confirmation_required" {
		t.Errorf("without confirm = %d %s", st, code)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err == nil {
		t.Fatal("an import from the no-go report was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_no_go" {
		t.Errorf("from the seeded no-go report = %d %s, want 409 migration_no_go", st, code)
	}

	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	m, _ := h.GetMigration(ctx)
	if report, _ := m.Report.Get(); report.Verdict == apiv1.MigrationVerdictNoGo {
		t.Fatalf("the rescan is still no-go: %+v", report.Rows)
	}

	j, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()})
	if err != nil {
		t.Fatalf("StartMigrationImport: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationImport || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID}); err != nil || done.Status != apiv1.JobStatusSucceeded {
		t.Errorf("the import job = %+v, %v, want it finished", done, err)
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseImported {
		t.Errorf("phase = %s, want imported", m.Phase)
	}

	// Like production, from here on.
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err == nil {
		t.Error("a scan was accepted while an import is pending")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
		t.Errorf("scan = %d %s, want 409 migration_in_progress", st, code)
	}
	if err := h.ForgetMigration(ctx); err == nil {
		t.Error("a forget was accepted while an import is pending")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
		t.Errorf("forget = %d %s", st, code)
	}
	if _, err := h.StartSync(ctx, &apiv1.StartSyncRequest{}); err == nil {
		t.Error("a sync was accepted while an import is pending")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
		t.Errorf("sync = %d %s", st, code)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Errorf("a retry of the pending import = %v, want it accepted", err)
	}
}

// The undo is the way out of a pending import, as in production: it needs the
// confirmation and no mapping, is refused unless an import is pending, and leaves
// the session scanned, so the session can be forgotten.
func TestMockMigration_UndoTakesAPendingImportBack(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	undo := &apiv1.MigrationImportRequest{Confirm: true, Undo: apiv1.NewOptBool(true)}

	if _, err := h.StartMigrationImport(ctx, undo); err == nil {
		t.Fatal("an undo with no import pending was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "no_import_pending" {
		t.Errorf("undo with nothing pending = %d %s, want 409 no_import_pending", st, code)
	}
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatalf("StartMigrationImport: %v", err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Undo: apiv1.NewOptBool(true)}); err == nil {
		t.Error("an undo without confirm was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "confirmation_required" {
		t.Errorf("undo without confirm = %d %s", st, code)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Undo: apiv1.NewOptBool(true), Roles: mockImportAll()}); err == nil {
		t.Error("an undo with a mapping was accepted")
	} else if st, code := mockErrCode(t, err); st != 400 || code != "invalid_import_roles" {
		t.Errorf("undo with roles = %d %s", st, code)
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseImported {
		t.Fatalf("a refused undo changed the phase to %s", m.Phase)
	}

	j, err := h.StartMigrationImport(ctx, undo)
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationImport || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID}); err != nil || done.Status != apiv1.JobStatusSucceeded {
		t.Errorf("the undo job = %+v, %v, want it finished", done, err)
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseScanned {
		t.Errorf("phase = %s after the undo, want scanned", m.Phase)
	}
	if err := h.ForgetMigration(ctx); err != nil {
		t.Errorf("forget after the undo = %v, want it accepted", err)
	}
}

func TestMockMigration_ImportRefusesWhatProductionRefuses(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	base := func(extra ...apiv1.MigrationImportDisk) []apiv1.MigrationImportDisk {
		return append([]apiv1.MigrationImportDisk{
			mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_PARITY"),
			mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1"),
		}, extra...)
	}
	for _, tc := range []struct {
		name   string
		roles  []apiv1.MigrationImportDisk
		status int
		code   string
	}{
		{"the stick as data", base(mockImportRole(apiv1.MigrationImportRoleData, "4C530001240603119335")), 409, "unraid_stick"},
		{"the stick as parity", []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleParity, "4C530001240603119335"), mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1")}, 409, "unraid_stick"},
		{"the stick as cache", base(mockImportRole(apiv1.MigrationImportRoleCache, "4C530001240603119335")), 409, "unraid_stick"},
		{"Unraid's parity disk as data", []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_DISK1"), mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_PARITY")}, 400, "invalid_import_roles"},
		{"a data disk as parity", []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_DISK4"), mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1")}, 400, "invalid_import_roles"},
		{"a disk the scan did not list", base(mockImportRole(apiv1.MigrationImportRoleData, "NO-SUCH-SERIAL")), 400, "invalid_import_roles"},
		{"no parity", []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1")}, 400, "invalid_import_roles"},
		{"no data", []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_PARITY")}, 400, "invalid_import_roles"},
		{"one parity disk and one data disk with no cache, which Q18's content files cannot be placed on", base(), 400, "invalid_import_roles"},
		{"no mapping at all", nil, 400, "invalid_import_roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: tc.roles})
			if err == nil {
				t.Fatal("accepted")
			}
			if st, code := mockErrCode(t, err); st != tc.status || code != tc.code {
				t.Errorf("= %d %s (%v), want %d %s", st, code, err, tc.status, tc.code)
			}
			if m, _ := h.GetMigration(ctx); m.Phase == apiv1.MigrationPhaseImported {
				t.Error("a refused import left the session imported")
			}
		})
	}
}

// The shared NVMe of an internal-boot server is the disk this mock boots from:
// as a whole disk it is never parity, data or cache.
func TestMockMigration_TheBootDiskIsNeverAWholeDiskRole(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	zipData := contractFlashZip("7.3.2", func(f map[string]string) {
		f["config/hoserva/capture.json"] = `{"boot":{"mode":"internal","filesystem":"zfs","mirrored":false,"shared_with_data_pool":true,"devices":[{"serial":"BOOTNVME1","model":"EXAMPLE NVMe"}]}}`
	})
	for i := 0; i < 2; i++ {
		if _, err := h.StartMigrationScan(ctx, contractScanRequest(zipData, false)); err != nil {
			t.Fatal(err)
		}
	}
	if rv := servedReview(t, h); !rv.Boot.SharedWithCache.Or(false) {
		t.Fatalf("boot = %+v, want the boot disk shared with the cache", rv.Boot)
	}
	for _, role := range []apiv1.MigrationImportRole{apiv1.MigrationImportRoleParity, apiv1.MigrationImportRoleData, apiv1.MigrationImportRoleCache} {
		roles := []apiv1.MigrationImportDisk{mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_PARITY"), mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1")}
		if role == apiv1.MigrationImportRoleParity {
			roles = []apiv1.MigrationImportDisk{mockImportRole(role, "BOOTNVME1"), mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1")}
		} else {
			roles = append(roles, mockImportRole(role, "BOOTNVME1"))
		}
		_, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: roles})
		if err == nil {
			t.Errorf("the boot disk was accepted as %s", role)
			continue
		}
		if st, code := mockErrCode(t, err); st != 400 || code != "invalid_import_roles" {
			t.Errorf("the boot disk as %s = %d %s (%v)", role, st, code, err)
		}
	}
}

// An import over an array that is not a pending import's is refused as
// production refuses it, once the report and the mapping are accepted.
func TestMockMigration_ImportRefusesAnOrdinaryArray(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("healthy")
	for i := 0; i < 2; i++ {
		if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()})
	if err == nil {
		t.Fatal("an import over the healthy scenario's array was accepted")
	}
	if st, code := mockErrCode(t, err); st != 409 || code != "array_exists" {
		t.Errorf("= %d %s (%v), want 409 array_exists", st, code, err)
	}
	if m, _ := h.GetMigration(ctx); m.Phase == apiv1.MigrationPhaseImported {
		t.Error("a refused import left the session imported")
	}
}

// Production refuses an array that is not a pending import's before it checks
// the layout, so a layout that is too small over such an array is array_exists.
func TestMockMigration_ImportOverAnOrdinaryArrayIsArrayExistsBeforeTheLayoutRefusal(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("healthy")
	for i := 0; i < 2; i++ {
		if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
			t.Fatal(err)
		}
	}
	tooSmall := []apiv1.MigrationImportDisk{
		mockImportRole(apiv1.MigrationImportRoleParity, "EXAMPLE_PARITY"),
		mockImportRole(apiv1.MigrationImportRoleData, "EXAMPLE_DISK1"),
	}
	_, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: tooSmall})
	if err == nil {
		t.Fatal("an import over the healthy scenario's array was accepted")
	}
	if st, code := mockErrCode(t, err); st != 409 || code != "array_exists" {
		t.Errorf("= %d %s (%v), want 409 array_exists", st, code, err)
	}
}

// migration-pending serves no array until the import has recorded one.
func TestMockMigration_ThePoolIsEmptyUntilTheImportAdoptsIt(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	if pool, _ := h.GetPool(ctx); pool.Mounted || len(mockPoolMembers(pool)) != 0 {
		t.Fatalf("pool before the import = %+v, want no array", pool)
	}
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	if pool, _ := h.GetPool(ctx); !pool.Mounted || len(mockPoolMembers(pool)) == 0 {
		t.Errorf("pool after the import = %+v, want the adopted pool", pool)
	}
}

// The cache pool's row of an internal boot that shares its disk with the cache
// is imported as that device's data partition, never the disk.
func TestMockMigration_TheCacheOfAnUnraidBootAndDataDeviceIsItsDataPartition(t *testing.T) {
	rv := mockMigrationReport(nil, "7.3.2", false, time.Now(), true).Review
	cache := slices.IndexFunc(rv.Disks, func(d migrate.ReviewDisk) bool { return d.Serial == "EXAMPLE_CACHE" })
	rv.Disks[cache].UnraidBoot = true
	rv.Boot.SharedWithCache = new(bool)
	*rv.Boot.SharedWithCache = true
	assignments := []disk.AdoptionAssignment{
		{Role: disk.AdoptParity, Serial: "EXAMPLE_PARITY"}, {Role: disk.AdoptData, Serial: "EXAMPLE_DISK1"},
		{Role: disk.AdoptCache, Serial: "EXAMPLE_CACHE"},
	}
	p, err := migrate.PlanFromReview(rv, mockMachineDisks(rv), assignments)
	if err != nil {
		t.Fatalf("PlanFromReview: %v", err)
	}
	if c := p.Plan.Cache; c == nil || c.Device != "/dev/nvme0n1p4" || c.ByIDName != "nvme-EXAMPLE_CACHE-part4" || c.PartUUID == "" {
		t.Errorf("cache = %+v, want partition 4 only", c)
	}
}

// The verify phase is refused until an import is pending, as production refuses
// it. Once one is, the first run fails with the scenario's failing result (the
// job failed, the phase verify_failed, the files named) and the next passes, so a
// UI sees both.
func TestMockMigration_VerifyFailsOnceThenPasses(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	if _, err := h.StartMigrationVerify(ctx); err == nil {
		t.Fatal("a verify before an import was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "no_import_pending" {
		t.Errorf("before an import = %d %s, want 409 no_import_pending", st, code)
	}
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}

	j, err := h.StartMigrationVerify(ctx)
	if err != nil {
		t.Fatalf("StartMigrationVerify after the import: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationVerify || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID})
	if err != nil || done.Status != apiv1.JobStatusFailed {
		t.Fatalf("the first verify job = %+v, %v, want failed", done, err)
	}
	m, _ := h.GetMigration(ctx)
	v, ok := m.Verify.Get()
	if m.Phase != apiv1.MigrationPhaseVerifyFailed || !ok || v.Status != apiv1.MigrationVerifyStatusFailed {
		t.Fatalf("after the first verify: phase = %s, verify = %+v", m.Phase, v)
	}
	var named bool
	for _, d := range v.Disks {
		named = named || (!d.Passed && len(d.SizeChanged.Paths) == 1 && len(d.ChecksumChanged.Paths) == 1 && len(d.Missing.Paths) == 1)
	}
	if !named {
		t.Errorf("the failing result names no file of each kind: %+v", v.Disks)
	}

	j, err = h.StartMigrationVerify(ctx)
	if err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID}); err != nil || done.Status != apiv1.JobStatusSucceeded {
		t.Fatalf("the re-run job = %+v, %v, want succeeded", done, err)
	}
	m, _ = h.GetMigration(ctx)
	v, _ = m.Verify.Get()
	if m.Phase != apiv1.MigrationPhaseVerified || v.Status != apiv1.MigrationVerifyStatusPassed || v.Duplicates == 0 {
		t.Errorf("after the re-run: phase = %s, verify = %+v", m.Phase, v)
	}
	for _, scope := range append(append([]apiv1.MigrationVerifyScope{}, v.Disks...), v.Shares...) {
		if !scope.Passed {
			t.Errorf("scope %s did not pass in the passing fixture", scope.Name)
		}
	}
}

// Once the mock's import has run, the shares and accounts it seeds are in
// listShares and listUsers as production's job leaves them: array-only with the
// Unraid cache mode as a target, notes, no password, and no new share or share
// data deletion while the import is pending.
func TestMockMigration_TheImportSeedsSharesAndUsers(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if shares, _ := h.ListShares(ctx); len(shares.Shares) != 0 {
		t.Fatalf("shares before the import = %d", len(shares.Shares))
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	shares, _ := h.ListShares(ctx)
	byName := map[string]apiv1.Share{}
	for _, s := range shares.Shares {
		byName[string(s.Name)] = s
	}
	if len(byName) != 3 {
		t.Fatalf("shares = %v, want backup, documents and media", shares.Shares)
	}
	media := byName["media"]
	if media.CacheMode != apiv1.ShareCacheModeArrayOnly || media.CreatePolicy != apiv1.ArrayCreatePolicyMfs || media.MinFreeSpace.Or("") != "50000000K" {
		t.Errorf("media = %+v", media)
	}
	if m, ok := media.Migration.Get(); !ok || len(m.Notes) == 0 || !strings.Contains(strings.Join(m.Notes, "\n"), "mapped to Balance across disks") {
		t.Errorf("media migration = %+v", media.Migration)
	}
	docs := byName["documents"]
	if m, ok := docs.Migration.Get(); !ok || m.TargetCacheMode.Or("") != apiv1.ShareCacheModeCacheThenMove || docs.CacheMode != apiv1.ShareCacheModeArrayOnly || !docs.Smb.Guest {
		t.Errorf("documents = %+v", docs)
	}
	users, _ := h.ListUsers(ctx)
	seeded := map[string]apiv1.UserSummary{}
	for _, u := range users.Users {
		seeded[u.Username] = u
	}
	for _, name := range []string{"alice", "bob"} {
		if u, ok := seeded[name]; !ok || u.Role != apiv1.UserRoleShareOnly || u.HasCredential {
			t.Errorf("%s = %+v, want a share-only account without a credential", name, u)
		}
	}
	perms, err := h.GetSharePermissions(ctx, apiv1.GetSharePermissionsParams{Name: "media"})
	if err != nil || len(perms.Users) != 2 {
		t.Errorf("media permissions = %+v, %v", perms, err)
	}

	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "fresh", CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)}); err == nil {
		t.Error("a share was created while the import is pending")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
		t.Errorf("createShare = %d %s, want 409 migration_in_progress", st, code)
	}
	if err := h.DeleteShareData(ctx, &apiv1.DeleteShareDataRequest{Confirmation: "media"}, apiv1.DeleteShareDataParams{Name: "media"}); err == nil {
		t.Error("a share's files were deleted while the import is pending")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
		t.Errorf("deleteShareData = %d %s, want 409 migration_in_progress", st, code)
	}
}

// The point of no return answers as production does, in production's order:
// refused before an import, refused until the latest verify passed whatever the
// request carries, then the typed confirmation must be the one the plan computes;
// and only then does it finish, leaving the array past it.
func TestMockMigration_InitializeParityNeedsAPassingVerifyAndTheTypedConfirmation(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	refused := func(req *apiv1.MigrationInitializeParityRequest, wantStatus int, wantCode string) {
		t.Helper()
		_, err := h.InitializeMigrationParity(ctx, req)
		if err == nil {
			t.Fatalf("InitializeMigrationParity(%+v) was accepted, want %d %s", req, wantStatus, wantCode)
		}
		if st, code := mockErrCode(t, err); st != wantStatus || code != wantCode {
			t.Errorf("InitializeMigrationParity(%+v) = %d %s, want %d %s", req, st, code, wantStatus, wantCode)
		}
	}
	refused(&apiv1.MigrationInitializeParityRequest{Confirmation: "x"}, 409, "no_import_pending")

	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	// No verify has run, and the first one fails: no confirmation is looked at.
	refused(&apiv1.MigrationInitializeParityRequest{Confirmation: ""}, 409, "verify_required")
	if m, _ := h.GetMigration(ctx); m.ParityInit.IsSet() {
		t.Error("getMigration offers the point of no return before a verify")
	}
	if _, err := h.StartMigrationVerify(ctx); err != nil {
		t.Fatal(err)
	}
	refused(&apiv1.MigrationInitializeParityRequest{Confirmation: "ERASE /dev/sdb"}, 409, "verify_required")
	if _, err := h.StartMigrationVerify(ctx); err != nil {
		t.Fatal(err)
	}

	m, _ := h.GetMigration(ctx)
	pi, ok := m.ParityInit.Get()
	if m.Phase != apiv1.MigrationPhaseVerified || !ok || pi.Finishing || pi.Problem.IsSet() || !pi.Confirmation.IsSet() {
		t.Fatalf("phase = %s, parityInit = %+v", m.Phase, pi)
	}
	want := pi.Confirmation.Value
	if !strings.HasPrefix(want, "ERASE ") || pi.UnprotectedWindow == "" || len(pi.Rollback) == 0 || len(pi.Erases) == 0 {
		t.Errorf("parityInit = %+v", pi)
	}
	for _, e := range pi.Erases {
		if !strings.Contains(want, e.Device) {
			t.Errorf("%s is erased but the confirmation %q does not name it", e.Device, want)
		}
	}
	for _, wrong := range []string{"", want + " ", strings.ToLower(want), "ERASE /dev/sdc"} {
		refused(&apiv1.MigrationInitializeParityRequest{Confirmation: wrong}, 409, "confirmation_required")
	}
	if _, err := h.InitializeMigrationParity(ctx, nil); err == nil {
		t.Error("a request with no body was accepted")
	}

	j, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: want})
	if err != nil {
		t.Fatalf("InitializeMigrationParity: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationParity || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID}); err != nil || done.Status != apiv1.JobStatusSucceeded {
		t.Fatalf("the job = %+v, %v, want succeeded", done, err)
	}
	mockRequireInitialSync(t, h, j.CreatedAt)
	m, _ = h.GetMigration(ctx)
	if m.Phase != apiv1.MigrationPhaseScanned || m.ParityInit.IsSet() || m.Verify.IsSet() {
		t.Errorf("after the point of no return: phase = %s, parityInit = %v, verify = %v", m.Phase, m.ParityInit.IsSet(), m.Verify.IsSet())
	}
	if pool, _ := h.GetPool(ctx); !pool.Mounted || len(mockPoolMembers(pool)) == 0 {
		t.Errorf("pool after the point of no return = %+v, want the array", pool)
	}
	// Past it nothing is pending: a second run is refused, and a job that
	// pending migrations refuse is accepted again.
	refused(&apiv1.MigrationInitializeParityRequest{Confirmation: want}, 409, "no_import_pending")
	if _, err := h.StartSync(ctx, &apiv1.StartSyncRequest{}); err != nil {
		t.Errorf("a sync after the point of no return: %v", err)
	}
}

// An import that runs again forgets the verify result, as production's job does:
// the point of no return is not offered until the array is verified again.
func TestMockMigration_AnImportThatRunsAgainForgetsTheVerifyResult(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.StartMigrationVerify(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseVerified {
		t.Fatalf("phase = %s, want verified", m.Phase)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.GetMigration(ctx)
	if m.Phase != apiv1.MigrationPhaseImported || m.Verify.IsSet() || m.ParityInit.IsSet() {
		t.Errorf("after a second import: phase = %s, verify = %v, parityInit = %v, want imported with neither", m.Phase, m.Verify.IsSet(), m.ParityInit.IsSet())
	}
	_, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: "ERASE /dev/sdb"})
	if st, code := mockErrCode(t, err); st != 409 || code != "verify_required" {
		t.Errorf("InitializeMigrationParity after a second import = %d %s, want 409 verify_required", st, code)
	}
}

// mockRequireInitialSync fails unless exactly one parity-class sync job was
// queued, no earlier than the migration_parity job created at parityAt: the one
// the web wizard follows for the initial sync.
func mockRequireInitialSync(t *testing.T, h *handler, parityAt time.Time) apiv1.Job {
	t.Helper()
	listed, err := h.ListJobs(context.Background(), apiv1.ListJobsParams{Class: apiv1.NewOptJobClass(apiv1.JobClassParity)})
	if err != nil {
		t.Fatal(err)
	}
	var syncs []apiv1.Job
	for _, j := range listed.Jobs {
		if j.Type == apiv1.JobTypeSync && !j.CreatedAt.Before(parityAt) {
			syncs = append(syncs, j)
		}
	}
	if len(syncs) != 1 {
		t.Fatalf("parity-class sync jobs created since the point of no return = %+v, want exactly one", syncs)
	}
	if syncs[0].Status != apiv1.JobStatusQueued {
		t.Errorf("initial sync = %+v, want queued", syncs[0])
	}
	return syncs[0]
}

func mockVerifiedMigration(t *testing.T, h *handler) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartMigrationImport(ctx, &apiv1.MigrationImportRequest{Confirm: true, Roles: mockImportAll()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := h.StartMigrationVerify(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// Until the point of no return has run, no initial sync is queued; a mock told to
// stop it part-way leaves the session initializing, which only the finishing
// confirmation completes, and the sync is queued only then, as production's retry
// does.
func TestMockMigration_StoppedParityInitReportsInitializingUntilFinished(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	h.migration.stopParityInit = true
	mockVerifiedMigration(t, h)
	m, _ := h.GetMigration(ctx)
	pi, _ := m.ParityInit.Get()
	stopped, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: pi.Confirmation.Value})
	if err != nil {
		t.Fatalf("InitializeMigrationParity: %v", err)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: stopped.ID}); err != nil || done.Status != apiv1.JobStatusFailed || !done.Error.IsSet() {
		t.Fatalf("the stopped job = %+v, %v, want failed with an error", done, err)
	}
	m, _ = h.GetMigration(ctx)
	pi, ok := m.ParityInit.Get()
	if m.Phase != apiv1.MigrationPhaseInitializing || !ok || !pi.Finishing || pi.Confirmation.Value != "FINISH PARITY INITIALISATION" ||
		pi.Problem.IsSet() || len(pi.Erases) != 0 || pi.UnprotectedWindow == "" || len(pi.Rollback) == 0 || m.Verify.IsSet() {
		t.Fatalf("phase = %s, parityInit = %+v, verify = %v", m.Phase, pi, m.Verify.IsSet())
	}
	if err := m.Validate(); err != nil {
		t.Errorf("the initializing migration fails the spec's validation: %v", err)
	}
	if pool, _ := h.GetPool(ctx); !pool.Mounted || len(mockPoolMembers(pool)) == 0 {
		t.Errorf("pool after the stopped run = %+v, want the recorded array", pool)
	}
	if listed, _ := h.ListJobs(ctx, apiv1.ListJobsParams{Class: apiv1.NewOptJobClass(apiv1.JobClassParity)}); len(listed.Jobs) != 0 {
		t.Errorf("parity-class jobs before the finish = %+v, want none", listed.Jobs)
	}

	refused := func(req *apiv1.MigrationInitializeParityRequest) {
		t.Helper()
		_, err := h.InitializeMigrationParity(ctx, req)
		if err == nil {
			t.Fatalf("InitializeMigrationParity(%+v) finished the initialisation", req)
		}
		if st, code := mockErrCode(t, err); st != 409 || code != "confirmation_required" {
			t.Errorf("InitializeMigrationParity(%+v) = %d %s, want 409 confirmation_required", req, st, code)
		}
	}
	refused(nil)
	for _, wrong := range []string{"", "ERASE /dev/sdb", "finish parity initialisation", "FINISH PARITY INITIALISATION "} {
		refused(&apiv1.MigrationInitializeParityRequest{Confirmation: wrong})
	}
	if m, _ := h.GetMigration(ctx); m.Phase != apiv1.MigrationPhaseInitializing {
		t.Errorf("phase after the refusals = %s, want initializing", m.Phase)
	}

	inProgress := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s was accepted while the initialisation is unfinished", what)
		} else if st, code := mockErrCode(t, err); st != 409 || code != "migration_in_progress" {
			t.Errorf("%s = %d %s, want 409 migration_in_progress", what, st, code)
		}
	}
	_, err = h.StartSync(ctx, &apiv1.StartSyncRequest{})
	inProgress("a sync", err)
	_, err = h.StartScrub(ctx, &apiv1.StartScrubRequest{})
	inProgress("a scrub", err)
	_, err = h.StartFix(ctx, &apiv1.StartFixRequest{Confirm: true})
	inProgress("a fix", err)
	_, err = h.StartMigrationScan(ctx, contractScanRequest(contractFlashZip("7.3.2", nil), false))
	inProgress("a scan", err)
	inProgress("a forget", h.ForgetMigration(ctx))
	if listed, _ := h.ListJobs(ctx, apiv1.ListJobsParams{Class: apiv1.NewOptJobClass(apiv1.JobClassParity)}); len(listed.Jobs) != 0 {
		t.Errorf("parity-class jobs after the refusals = %+v, want none", listed.Jobs)
	}

	j, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation})
	if err != nil {
		t.Fatalf("finishing: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationParity || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %+v", j)
	}
	if done, err := h.GetJob(ctx, apiv1.GetJobParams{JobId: j.ID}); err != nil || done.Status != apiv1.JobStatusSucceeded {
		t.Fatalf("the finishing job = %+v, %v, want succeeded", done, err)
	}
	mockRequireInitialSync(t, h, j.CreatedAt)
	m, _ = h.GetMigration(ctx)
	if m.Phase != apiv1.MigrationPhaseScanned || m.ParityInit.IsSet() {
		t.Errorf("after finishing: phase = %s, parityInit = %v, want scanned with none", m.Phase, m.ParityInit.IsSet())
	}
	if _, err := h.StartScrub(ctx, &apiv1.StartScrubRequest{}); err != nil {
		t.Errorf("a scrub after the finish: %v", err)
	}
	if err := h.ForgetMigration(ctx); err != nil {
		t.Errorf("a forget after the finish: %v", err)
	}
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation}); err == nil {
		t.Error("a second finish was accepted")
	} else if st, code := mockErrCode(t, err); st != 409 || code != "no_import_pending" {
		t.Errorf("second finish = %d %s, want 409 no_import_pending", st, code)
	}
}

// Without the stop control the point of no return finishes in one call and never
// reports initializing.
func TestMockMigration_ParityInitQueuesTheInitialSyncAndNeverReportsInitializing(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	mockVerifiedMigration(t, h)
	m, _ := h.GetMigration(ctx)
	j, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: m.ParityInit.Value.Confirmation.Value})
	if err != nil {
		t.Fatal(err)
	}
	sync := mockRequireInitialSync(t, h, j.CreatedAt)
	if sync.Class != apiv1.JobClassParity {
		t.Errorf("initial sync = %+v", sync)
	}
	if m, _ := h.GetMigration(ctx); m.Phase == apiv1.MigrationPhaseInitializing {
		t.Error("phase = initializing without the stop control")
	}
}

func mockContainersCode(t *testing.T, what string, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted, want %d %s", what, status, code)
	}
	if st, c := mockErrCode(t, err); st != status || c != code {
		t.Errorf("%s = %d %s, want %d %s", what, st, c, status, code)
	}
}

func mockSelect(names ...string) *apiv1.MigrationStacksRequest {
	req := &apiv1.MigrationStacksRequest{}
	for _, n := range names {
		req.Items = append(req.Items, apiv1.MigrationStackSelection{Name: n})
	}
	return req
}

// Phase D's operations are gated on the migration the way production's are, by
// the same unfinished state the scheduler's gate reads: refused with no array,
// with an import pending and with the initialisation stopped part-way, and open
// once the point of no return has finished.
func TestMockMigration_ContainersAreRefusedUntilThePointOfNoReturnHasFinished(t *testing.T) {
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	refused := func(when string) {
		t.Helper()
		_, err := h.CreateMigrationStacks(ctx, mockSelect("my-photos.xml"))
		mockContainersCode(t, when+": create", err, 409, "parity_not_initialized")
		_, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "photos"})
		mockContainersCode(t, when+": start", err, 409, "parity_not_initialized")
		_, err = h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "photos"})
		mockContainersCode(t, when+": check", err, 409, "parity_not_initialized")
		_, err = h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "photos"})
		mockContainersCode(t, when+": confirm", err, 409, "parity_not_initialized")
		if list, err := h.ListMigrationContainers(ctx); err != nil || list.ParityInitialized {
			t.Errorf("%s: list = %+v, %v, want the offer with parityInitialized false", when, list, err)
		}
		if got, _ := h.ListStacks(ctx); got != nil {
			for _, s := range got.Stacks {
				if s.Name == "photos" || s.Name == "gateway" {
					t.Errorf("%s: a refused request created stack %s", when, s.Name)
				}
			}
		}
	}
	refused("before the import")

	h.migration.stopParityInit = true
	mockVerifiedMigration(t, h)
	refused("with the import pending")
	m, _ := h.GetMigration(ctx)
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: m.ParityInit.Value.Confirmation.Value}); err != nil {
		t.Fatal(err)
	}
	refused("with the initialisation stopped part-way")
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: disk.ParityInitFinishConfirmation}); err != nil {
		t.Fatal(err)
	}
	if list, err := h.ListMigrationContainers(ctx); err != nil || !list.ParityInitialized {
		t.Fatalf("list after the point of no return = %+v, %v", list, err)
	}
	if _, err := h.CreateMigrationStacks(ctx, mockSelect("my-photos.xml")); err != nil {
		t.Errorf("create after the point of no return: %v", err)
	}
}

func mockCrossedMigration(t *testing.T) *handler {
	t.Helper()
	ctx := context.Background()
	h, _ := newHandler("migration-pending")
	mockVerifiedMigration(t, h)
	m, _ := h.GetMigration(ctx)
	if _, err := h.InitializeMigrationParity(ctx, &apiv1.MigrationInitializeParityRequest{Confirmation: m.ParityInit.Value.Confirmation.Value}); err != nil {
		t.Fatal(err)
	}
	return h
}

// The offer pre-selects only the autostart list, in its order, lists the
// project and the container made by hand, and a request is refused as
// production refuses it: warnings need an acknowledgement, nothing is created
// from a refused request, one start at a time, a data check before a
// confirmation.
func TestMockMigration_ContainersOfferCreateStartAndConfirmAsProductionDoes(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)

	list, err := h.ListMigrationContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pre, order []string
	for _, tm := range list.Templates {
		order = append(order, string(tm.Class)+":"+tm.File)
		if tm.Preselected {
			pre = append(pre, tm.File)
		}
	}
	if len(pre) != 1 || pre[0] != "my-photos.xml" || strings.Join(order, " ") != "autostart:my-photos.xml running:my-gateway.xml" {
		t.Errorf("pre-selected %v in groups %v, want only the autostart template, first", pre, order)
	}
	if len(list.ComposeProjects) != 1 || list.ComposeProjects[0].Name != "stack" || len(list.ByHand) != 1 || list.ByHand[0].Image.Or("") == "" {
		t.Errorf("projects %+v, by hand %+v", list.ComposeProjects, list.ByHand)
	}

	_, err = h.CreateMigrationStacks(ctx, mockSelect("my-photos.xml", "my-gateway.xml"))
	mockContainersCode(t, "create with unacknowledged warnings", err, 409, "warnings_not_acknowledged")
	_, err = h.CreateMigrationStacks(ctx, mockSelect())
	mockContainersCode(t, "create with nothing selected", err, 400, "invalid_selection")
	_, err = h.CreateMigrationStacks(ctx, mockSelect("my-photos.xml", "my-photos.xml"))
	mockContainersCode(t, "create with a name twice", err, 400, "invalid_selection")
	_, err = h.CreateMigrationStacks(ctx, mockSelect("nothing.xml"))
	mockContainersCode(t, "create of an unknown template", err, 404, "template_not_found")
	if stacks, _ := h.ListStacks(ctx); len(stacks.Stacks) > 0 {
		for _, s := range stacks.Stacks {
			if s.Name == "photos" || s.Name == "gateway" {
				t.Fatalf("a refused request created stack %s", s.Name)
			}
		}
	}

	req := mockSelect("my-gateway.xml", "my-photos.xml", "stack")
	req.Items[0].Acknowledged = apiv1.NewOptBool(true)
	res, err := h.CreateMigrationStacks(ctx, req)
	if err != nil || len(res.Results) != 3 || res.Results[0].Name != "my-photos.xml" {
		t.Fatalf("create = %+v, %v, want three results, the autostart template first", res, err)
	}
	for _, r := range res.Results {
		if r.Status != apiv1.MigrationStackResultStatusCreated {
			t.Errorf("result %+v, want created", r)
		}
	}
	if res, _ = h.CreateMigrationStacks(ctx, mockSelect("my-photos.xml")); res == nil || res.Results[0].Status != apiv1.MigrationStackResultStatusAlreadyCreated {
		t.Errorf("a second create = %+v, want already created", res)
	}

	_, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "nothing"})
	mockContainersCode(t, "start of an unknown stack", err, 404, "migrated_stack_not_found")
	_, err = h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "photos"})
	mockContainersCode(t, "check before the start", err, 409, "container_not_started")
	if _, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "photos"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "gateway"})
	mockContainersCode(t, "a second start while the first is unconfirmed", err, 409, "container_unconfirmed")
	if l, _ := h.ListMigrationContainers(ctx); l.Awaiting.Or("") != "photos" || l.Next.Set {
		t.Errorf("awaiting %q next %q, want photos awaiting and nothing next", l.Awaiting.Or(""), l.Next.Or(""))
	}
	_, err = h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "photos"})
	mockContainersCode(t, "confirm before the data check", err, 409, "data_check_required")
	check, err := h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "photos"})
	if err != nil || !check.AllOk {
		t.Fatalf("check = %+v, %v", check, err)
	}
	st, err := h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "photos"})
	if err != nil || st.State != apiv1.MigrationContainerStackStateConfirmed {
		t.Fatalf("confirm = %+v, %v", st, err)
	}
	_, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "photos"})
	mockContainersCode(t, "start of a confirmed stack", err, 409, "container_confirmed")
	if _, err = h.StartMigrationContainer(ctx, apiv1.StartMigrationContainerParams{Name: "gateway"}); err != nil {
		t.Errorf("start of the next stack once the first is confirmed: %v", err)
	}
}

// Production creates a migration stack with a masked template value in its
// .env and a ${NAME} reference in the Compose text, because getStack, a viewer
// operation, returns the Compose text. The mock does the same.
func TestMockMigration_AMaskedTemplateValueGoesToTheStacksEnvAndNotItsCompose(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)
	prev := mockMigrationTemplateFiles
	t.Cleanup(func() { mockMigrationTemplateFiles = prev })
	mockMigrationTemplateFiles = fstest.MapFS{"my-vault.xml": {Data: []byte(`<?xml version="1.0"?><Container version="2"><Name>vault</Name><Repository>fixture/vault:1</Repository>
<Config Target="DB_PASS" Type="Variable" Mask="true">hunter2 $x</Config>
<Config Target="APOS" Type="Variable" Mask="true">it's</Config>
<Config Target="DIR" Type="Variable" Mask="true">c:\dir\</Config>
<Config Target="TZ" Type="Variable" Mask="false">UTC</Config></Container>`)}}

	res, err := h.CreateMigrationStacks(ctx, mockSelect("my-vault.xml"))
	if err != nil || len(res.Results) != 1 || res.Results[0].Status != apiv1.MigrationStackResultStatusCreated {
		t.Fatalf("create = %+v, %v, want the stack created", res, err)
	}
	st, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "vault"})
	if err != nil {
		t.Fatal(err)
	}
	compose := st.Compose.Or("")
	for _, secret := range []string{"hunter2", "it's", `c:\dir`} {
		if strings.Contains(compose, secret) {
			t.Errorf("getStack returns the Compose\n%s\nwith the masked value %q in it", compose, secret)
		}
	}
	if !strings.Contains(compose, "DB_PASS: ${DB_PASS}") || !strings.Contains(compose, "APOS: ${APOS}") || !strings.Contains(compose, "DIR: ${DIR}") || !strings.Contains(compose, "TZ: UTC") {
		t.Errorf("getStack returns the Compose\n%s\nwant the masked value out of it and referenced as ${DB_PASS}", compose)
	}
	if got, want := h.stackEnvs["vault"], "APOS=\"it's\"\nDB_PASS=\"hunter2 \\$x\"\nDIR=\"c:\\\\dir\\\\\"\n"; got != want {
		t.Errorf("the stack's .env = %q, want %q", got, want)
	}

	view, err := h.GetMigrationTemplate(ctx, apiv1.GetMigrationTemplateParams{Name: "my-vault.xml"})
	if err != nil || strings.Contains(view.Compose.Or(""), "hunter2") {
		t.Errorf("the template preview = %+v, %v, want the Compose without the masked value", view, err)
	}
}

// A path a check finds empty or missing needs the user to accept it; the mock
// reads no disk and so reads it off the path's name.
func TestMockMigration_AFailedDataCheckNeedsAcceptingToConfirm(t *testing.T) {
	ctx := context.Background()
	h := mockCrossedMigration(t)
	compose := "services:\n  web:\n    image: example/web:1\n    volumes:\n      - /mnt/user/appdata/web-empty:/data\n      - /mnt/cache/appdata/web:/config\n      - /var/lib/other:/x\n"
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "other", Compose: compose}); err != nil {
		t.Fatal(err)
	}
	h.migration.containers = append(h.migration.containers, mockMigratedStack{name: "other", source: "other.xml", kind: apiv1.MigrationContainerStackKindTemplate, state: apiv1.MigrationContainerStackStateStarted})
	check, err := h.CheckMigrationContainer(ctx, apiv1.CheckMigrationContainerParams{Name: "other"})
	if err != nil || check.AllOk || len(check.Paths) != 2 || check.Paths[0].Status != apiv1.MigrationDataPathStatusEmpty || check.Paths[1].Status != apiv1.MigrationDataPathStatusOk {
		t.Fatalf("check = %+v, %v, want the two paths under /mnt, the first empty", check, err)
	}
	_, err = h.ConfirmMigrationContainer(ctx, apiv1.OptMigrationContainerConfirmRequest{}, apiv1.ConfirmMigrationContainerParams{Name: "other"})
	mockContainersCode(t, "confirm after a failed check", err, 409, "data_check_failed")
	st, err := h.ConfirmMigrationContainer(ctx, apiv1.NewOptMigrationContainerConfirmRequest(apiv1.MigrationContainerConfirmRequest{AcceptFailedCheck: apiv1.NewOptBool(true)}), apiv1.ConfirmMigrationContainerParams{Name: "other"})
	if err != nil || st.State != apiv1.MigrationContainerStackStateConfirmed {
		t.Errorf("confirm accepting the failed check = %+v, %v", st, err)
	}
}
