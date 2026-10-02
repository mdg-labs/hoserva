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
	if !ok || len(report.Rows) == 0 || report.Verdict != apiv1.MigrationVerdictGoWithWarnings {
		t.Fatalf("the migration-pending session has no completed report: %+v", m)
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
