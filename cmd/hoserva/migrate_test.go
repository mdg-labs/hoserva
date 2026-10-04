package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

const migrateTestReport = "# Hoserva migration scan report\n\n**GO WITH WARNINGS**\n"

// migrateDaemon is a stand-in daemon on a Unix socket for the /migrate
// operations.
type migrateDaemon struct {
	sock string
	id   uuid.UUID

	mu        sync.Mutex
	requests  []string
	uploaded  string
	unverif   string
	fullSums  string
	deviceReq string
	jobStatus apiv1.JobStatus
	refuse    *apiv1.Error

	zipOnly      bool
	sourceDevice string
}

func startMigrateDaemon(t *testing.T) *migrateDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &migrateDaemon{sock: filepath.Join(dir, "d.sock"), id: uuid.New(), jobStatus: apiv1.JobStatusSucceeded}
	ln, err := net.Listen("unix", d.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(d.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return d
}

func (d *migrateDaemon) serve(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r.Method+" "+r.URL.Path)
	reply := func(status int, out []byte, err error) {
		if err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(out)
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/migrate/scan":
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		data, _ := io.ReadAll(f)
		d.uploaded = string(data)
		d.unverif = r.FormValue("unverifiedLayout")
		d.fullSums = r.FormValue("fullChecksums")
		if d.refuse != nil {
			out, err := d.refuse.MarshalJSON()
			reply(http.StatusBadRequest, out, err)
			return
		}
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationScan, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/migrate/scan/device":
		body, _ := io.ReadAll(r.Body)
		d.deviceReq = string(body)
		if d.refuse != nil {
			out, err := d.refuse.MarshalJSON()
			reply(http.StatusBadRequest, out, err)
			return
		}
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationScan, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/"+d.id.String():
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationScan, Class: apiv1.JobClassTopology, Status: d.jobStatus, CreatedAt: time.Now().UTC()}
		if d.jobStatus == apiv1.JobStatusFailed {
			j.SetError(apiv1.NewOptNilError(apiv1.Error{Code: "job_failed", Message: "migration scan: listing this machine's disks: udev gone"}))
		}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/migrate":
		m := apiv1.Migration{Phase: apiv1.MigrationPhaseScanned, ZipOnly: d.zipOnly, FlashDevices: []apiv1.MigrationFlashDevice{}}
		if !d.zipOnly {
			m.FlashDevices = append(m.FlashDevices, apiv1.MigrationFlashDevice{Device: "/dev/sdb", Size: 16 << 30, Model: apiv1.NewOptString("Flash Drive")})
		}
		if d.sourceDevice != "" {
			m.SourceDevice = apiv1.NewOptString(d.sourceDevice)
		}
		m.Report = apiv1.NewOptMigrationReport(apiv1.MigrationReport{
			GeneratedAt: time.Now().UTC(), UnraidVersion: apiv1.NewOptString("7.3.2"), UnverifiedLayout: true, Verdict: apiv1.MigrationVerdictGoWithWarnings,
			Rows: []apiv1.MigrationReportRow{{Check: "smart", Status: apiv1.MigrationCheckStatusFlag, Detail: "x"}},
		})
		out, err := m.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/migrate/report":
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write([]byte(migrateTestReport))
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/migrate":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (d *migrateDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func (d *migrateDaemon) upload() (zip, unverified string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.uploaded, d.unverif
}

func (d *migrateDaemon) fullChecksums() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fullSums
}

func (d *migrateDaemon) deviceRequest() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deviceReq
}

func writeZip(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "boot.zip")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMigrateScanUploadsTheZipWaitsForTheJobAndPrintsTheReport(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	zipPath := writeZip(t, "zip bytes")

	printed, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", zipPath)
	if err != nil {
		t.Fatalf("migrate scan: %v", err)
	}
	if printed != migrateTestReport {
		t.Errorf("output = %q, want the report document", printed)
	}
	if zipData, override := d.upload(); zipData != "zip bytes" || override != "" {
		t.Errorf("uploaded %q with unverifiedLayout %q, want the zip and no override", zipData, override)
	}
	got := d.seen()
	if len(got) != 3 || got[0] != "POST /api/v1/migrate/scan" || got[1] != "GET /api/v1/jobs/"+d.id.String() || got[2] != "GET /api/v1/migrate/report" {
		t.Errorf("requests = %v, want the upload, one poll and the report", got)
	}
}

func TestMigrateScanSendsTheUnverifiedLayoutOverride(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z"), "--unverified-layout"); err != nil {
		t.Fatal(err)
	}
	if _, override := d.upload(); override != "true" {
		t.Errorf("unverifiedLayout sent as %q, want true", override)
	}
}

func TestMigrateScanSendsTheFullChecksumsOption(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z")); err != nil {
		t.Fatal(err)
	}
	if got := d.fullChecksums(); got != "" {
		t.Errorf("fullChecksums sent as %q without --full-checksums, want it absent", got)
	}
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z"), "--full-checksums"); err != nil {
		t.Fatal(err)
	}
	if got := d.fullChecksums(); got != "true" {
		t.Errorf("fullChecksums sent as %q, want true", got)
	}
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-device", "/dev/sdb", "--full-checksums"); err != nil {
		t.Fatal(err)
	}
	if got := d.deviceRequest(); got != `{"device":"/dev/sdb","fullChecksums":true}` {
		t.Errorf("device request = %s, want the device and fullChecksums", got)
	}
}

func TestMigrateScanOfTheStickSendsTheDeviceWaitsForTheJobAndPrintsTheReport(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)

	printed, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-device", "/dev/sdb", "--unverified-layout")
	if err != nil {
		t.Fatalf("migrate scan --flash-device: %v", err)
	}
	if printed != migrateTestReport {
		t.Errorf("output = %q, want the report document", printed)
	}
	if got := d.deviceRequest(); got != `{"device":"/dev/sdb","unverifiedLayout":true}` {
		t.Errorf("request body = %s", got)
	}
	got := d.seen()
	if len(got) != 3 || got[0] != "POST /api/v1/migrate/scan/device" || got[1] != "GET /api/v1/jobs/"+d.id.String() || got[2] != "GET /api/v1/migrate/report" {
		t.Errorf("requests = %v, want the device scan, one poll and the report", got)
	}
}

func TestMigrateScanTakesExactlyOneSource(t *testing.T) {
	d := startMigrateDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z"), "--flash-device", "/dev/sdb"); err == nil {
		t.Error("migrate scan with both sources succeeded")
	}
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-device", ""); err == nil || !strings.Contains(err.Error(), "needs a device") {
		t.Errorf("migrate scan with an empty --flash-device = %v, want it refused for naming no device", err)
	}
	if got := d.seen(); len(got) != 0 {
		t.Errorf("requests = %v, want none for an unusable command line", got)
	}
}

func TestMigrateScanRequiresTheZipAndFailsOnAMissingFile(t *testing.T) {
	d := startMigrateDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan"); err == nil {
		t.Error("migrate scan without a source succeeded")
	}
	if _, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", filepath.Join(t.TempDir(), "missing.zip")); err == nil {
		t.Error("migrate scan of a file that does not exist succeeded")
	}
	if got := d.seen(); len(got) != 0 {
		t.Errorf("requests = %v, want none before the file is known", got)
	}
}

func TestMigrateScanRefusalIsACommandFailureNamingTheCode(t *testing.T) {
	d := startMigrateDaemon(t)
	d.mu.Lock()
	d.refuse = &apiv1.Error{Code: "unsupported_layout", Message: "Unraid 6.9.2 is neither 6.12.x nor 7.x"}
	d.mu.Unlock()
	_, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z"))
	if err == nil || !strings.Contains(err.Error(), "unsupported_layout") {
		t.Fatalf("migrate scan = %v, want the refusal's code", err)
	}
}

func TestMigrateScanFailsWithTheFailedJobsError(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	d.mu.Lock()
	d.jobStatus = apiv1.JobStatusFailed
	d.mu.Unlock()
	printed, err := runBackupCLI(t, d.sock, "migrate", "scan", "--flash-backup", writeZip(t, "z"))
	if err == nil || !strings.Contains(err.Error(), "udev gone") {
		t.Fatalf("migrate scan over a failed job = %v, want the job's error", err)
	}
	if printed != "" {
		t.Errorf("printed %q for a failed scan", printed)
	}
}

func TestMigrateStatusSummarisesTheReport(t *testing.T) {
	d := startMigrateDaemon(t)
	printed, err := runBackupCLI(t, d.sock, "migrate", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Migration: scanned", "Unraid version: 7.3.2", "Unverified layout", "Verdict: go_with_warnings", "1 flag"} {
		if !strings.Contains(printed, want) {
			t.Errorf("status output lacks %q:\n%s", want, printed)
		}
	}
	if !strings.Contains(printed, "Flash device: /dev/sdb (Flash Drive 16.0 GiB)") {
		t.Errorf("status output does not list the stick:\n%s", printed)
	}
	printed, err = runBackupCLI(t, d.sock, "--json", "migrate", "status")
	if err != nil || !strings.Contains(printed, `"go_with_warnings"`) {
		t.Errorf("--json output = %q, %v", printed, err)
	}
}

func TestMigrateReportPrintsOrSavesTheDocument(t *testing.T) {
	d := startMigrateDaemon(t)
	printed, err := runBackupCLI(t, d.sock, "migrate", "report")
	if err != nil || printed != migrateTestReport {
		t.Fatalf("migrate report = %q, %v", printed, err)
	}
	out := filepath.Join(t.TempDir(), "report.md")
	if _, err := runBackupCLI(t, d.sock, "migrate", "report", "-o", out); err != nil {
		t.Fatal(err)
	}
	if saved, _ := os.ReadFile(out); string(saved) != migrateTestReport {
		t.Errorf("saved report = %q", saved)
	}
	// The report names the source's shares, user accounts and containers.
	if fi, err := os.Stat(out); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("saved report mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Errorf("the directory holds %v, want only the report", entries)
	}
}

func TestMigrateForgetDeletesTheSession(t *testing.T) {
	d := startMigrateDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "forget"); err != nil {
		t.Fatal(err)
	}
	if got := d.seen(); len(got) != 1 || got[0] != "DELETE /api/v1/migrate" {
		t.Errorf("requests = %v", got)
	}
}

func TestMigrateStatusSaysWhenTheZipIsTheOnlySourceAndNamesAStickSource(t *testing.T) {
	d := startMigrateDaemon(t)
	d.mu.Lock()
	d.zipOnly, d.sourceDevice = true, "/dev/sdb"
	d.mu.Unlock()
	printed, err := runBackupCLI(t, d.sock, "migrate", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Source: the Unraid USB stick at /dev/sdb", "the Flash Backup zip is the only source"} {
		if !strings.Contains(printed, want) {
			t.Errorf("status output lacks %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "Flash device: ") {
		t.Errorf("status offers a stick although the zip is the only source:\n%s", printed)
	}
}
