package api_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
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
		"StartMigrationDeviceScan": func() error {
			_, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: "/dev/sdz"})
			return err
		},
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

const testStick = "/dev/sdz"

// stickHandler is migrationHandler with an UNRAID-labelled FAT disk at
// testStick that a fake read-only mounter fills with the files mutate leaves of
// flashZip's.
func stickHandler(t *testing.T, mutate func(map[string]string)) (*api.Handler, *migrate.Service, *disk.FakeReadOnlyMounter) {
	t.Helper()
	h, _, registry := newTestHandler(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "PARITYSERIAL", Size: 2 << 40})
	disks.AddDisk("/dev/sdc", disk.Disk{Serial: "DATASERIAL", Size: 1 << 40})
	disks.AddDisk(testStick, disk.Disk{Size: 16 << 30, Model: "Flash Drive", Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234"})
	disks.AddDisk("/dev/sdy", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "1111-2222"})
	mounter := disk.NewFakeReadOnlyMounter()
	data := flashZip(t, "7.3.2", mutate)
	mounter.OnMount = func(where string) error {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			content, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
			p := filepath.Join(where, filepath.FromSlash(f.Name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, content, 0o644); err != nil {
				return err
			}
		}
		return nil
	}
	svc := &migrate.Service{
		Dir:      filepath.Join(t.TempDir(), "migrate"),
		Scanner:  &migrate.Scanner{Disks: disks, UIDOwner: func(int) (string, error) { return "", nil }},
		Sessions: store.NewMigrationSessionStore(openTestDB(t)),
		Mounter:  mounter,
		ArrayDevices: func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{"/dev/sdy": {}}, nil
		},
	}
	h.Migration = svc
	registry.Register(job.TypeMigrationScan, false, job.RunMigrationScan(svc.RunScan))
	return h, svc, mounter
}

func TestHandler_StartMigrationDeviceScan_ScansTheStickReadOnlyAndReportsItsDevice(t *testing.T) {
	h, _, mounter := stickHandler(t, nil)
	ctx := context.Background()

	before, err := h.GetMigration(ctx)
	if err != nil || len(before.FlashDevices) != 1 || before.FlashDevices[0].Device != testStick || before.ZipOnly {
		t.Fatalf("GetMigration offers %+v (zipOnly %v), %v; want only %s, as /dev/sdy is in the array", before.FlashDevices, before.ZipOnly, err, testStick)
	}
	if m, ok := before.FlashDevices[0].Model.Get(); !ok || m != "Flash Drive" || before.FlashDevices[0].Size != 16<<30 {
		t.Errorf("the offered device = %+v", before.FlashDevices[0])
	}

	j, err := h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: testStick})
	if err != nil {
		t.Fatalf("StartMigrationDeviceScan: %v", err)
	}
	if j.Type != apiv1.JobTypeMigrationScan || j.Class != apiv1.JobClassTopology {
		t.Errorf("job = %s %s, want a migration_scan topology job", j.Type, j.Class)
	}
	if done, err := h.Scheduler.Await(ctx, j.ID.String()); err != nil || done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %+v, %v", done, err)
	}
	got, err := h.GetMigration(ctx)
	if err != nil || got.Phase != apiv1.MigrationPhaseScanned {
		t.Fatalf("GetMigration = %+v, %v", got, err)
	}
	if dev, ok := got.SourceDevice.Get(); !ok || dev != testStick || got.SourceSize.Set {
		t.Errorf("source = device %q, size set %v; want %s and no zip size", dev, got.SourceSize.Set, testStick)
	}
	if len(mounter.Mounts) != 2 || len(mounter.MountedPaths()) != 0 {
		t.Errorf("mounts = %v, still mounted %v; want a mount for the inspect and one for the job, none left", mounter.Mounts, mounter.MountedPaths())
	}
}

func TestHandler_StartMigrationDeviceScan_RefusalsQueueNothingAndLeaveNothingMounted(t *testing.T) {
	cases := map[string]struct {
		device  string
		mutate  func(map[string]string)
		mount   error
		status  int
		code    string
		mounted bool
	}{
		"no device":                  {device: "", status: 400, code: "invalid_flash_device"},
		"a disk in the array":        {device: "/dev/sdy", status: 400, code: "invalid_flash_device"},
		"a data disk":                {device: "/dev/sdb", status: 400, code: "invalid_flash_device"},
		"a device that is not there": {device: "/dev/nope", status: 400, code: "invalid_flash_device"},
		"no disk.cfg":                {device: testStick, mutate: func(f map[string]string) { delete(f, "config/disk.cfg") }, status: 400, code: "invalid_flash_backup", mounted: true},
		"an unknown version":         {device: testStick, mutate: func(f map[string]string) { f["changes.txt"] = "# Version 6.9.2 2021-01-01\n" }, status: 400, code: "unsupported_layout", mounted: true},
		"an internal-boot capture": {device: testStick, mutate: func(f map[string]string) {
			f["config/hoserva/capture.json"] = `{"boot":{"mode":"internal","filesystem":"zfs","devices":[]}}`
		}, status: 409, code: "zip_only_source", mounted: true},
		"a stick that will not mount": {device: testStick, mount: errors.New("wrong fs type"), status: 409, code: "flash_device_unreadable", mounted: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, svc, mounter := stickHandler(t, tc.mutate)
			mounter.MountErr = tc.mount
			_, err := h.StartMigrationDeviceScan(context.Background(), &apiv1.StartMigrationDeviceScanReq{Device: tc.device})
			if err == nil {
				t.Fatal("StartMigrationDeviceScan accepted it")
			}
			if st, code := statusOf(h, err); st != tc.status || code != tc.code {
				t.Errorf("= %d %s (%v), want %d %s", st, code, err, tc.status, tc.code)
			}
			if tc.mounted != (len(mounter.Mounts) > 0) {
				t.Errorf("mounts = %v, want a mount: %v", mounter.Mounts, tc.mounted)
			}
			if got := mounter.MountedPaths(); len(got) != 0 {
				t.Errorf("left mounted: %v", got)
			}
			jobs, _ := h.Store.List(context.Background(), job.ListFilter{})
			if len(jobs) != 0 {
				t.Errorf("a refused scan queued %v", jobs)
			}
			if st, _ := svc.State(context.Background()); st.Phase != migrate.PhaseNone {
				t.Errorf("phase = %s", st.Phase)
			}
		})
	}
}

func TestHandler_StartMigrationDeviceScan_NotConfiguredWithoutAMounter(t *testing.T) {
	h, svc, _ := stickHandler(t, nil)
	svc.Mounter = nil
	_, err := h.StartMigrationDeviceScan(context.Background(), &apiv1.StartMigrationDeviceScanReq{Device: testStick})
	if st, code := statusOf(h, err); st != 501 || code != "not_configured" {
		t.Fatalf("= %d %s (%v), want 501 not_configured", st, code, err)
	}
	if got, err := h.GetMigration(context.Background()); err != nil || len(got.FlashDevices) != 0 {
		t.Fatalf("GetMigration without a mounter = %+v, %v; want no device offered", got, err)
	}
}

func TestHandler_StartMigrationDeviceScan_RefusedByTheSchedulerLeavesThePreviousSessionAndTheStickUnmounted(t *testing.T) {
	h, svc, mounter := stickHandler(t, nil)
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
	_, err = h.StartMigrationDeviceScan(ctx, &apiv1.StartMigrationDeviceScanReq{Device: testStick})
	if st, code := statusOf(h, err); st != 409 || code != "maintenance_mode" {
		t.Fatalf("in maintenance mode = %d %s (%v), want 409 maintenance_mode", st, code, err)
	}
	after, _ := svc.State(ctx)
	if after.Phase != migrate.PhaseScanned || after.Source.File != before.Source.File {
		t.Errorf("session after the refusal = %+v, want it unchanged", after)
	}
	if got := mounter.MountedPaths(); len(got) != 0 {
		t.Errorf("left mounted: %v", got)
	}
}
