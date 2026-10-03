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
	"strings"
	"testing"
	"time"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
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
