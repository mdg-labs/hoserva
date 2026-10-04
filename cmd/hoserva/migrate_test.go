package main

import (
	"compress/gzip"
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

	// review is the disk table GET /migrate serves; importReq is the body of
	// the last POST /migrate/import and importRefusal answers it.
	review        []apiv1.MigrationDisk
	importReq     string
	importRefusal *apiv1.Error

	// undoReq is the body of the last POST /migrate/import that asked for an
	// undo and undoRefusal answers it.
	undoReq     string
	undoRefusal *apiv1.Error

	// phase, when set, is the phase GET /migrate serves in place of scanned,
	// and importLog is what the finished import job's log says.
	phase     apiv1.MigrationPhase
	importLog string

	// verify is the result GET /migrate serves, and verifyRefusal answers
	// POST /migrate/verify.
	verify        *apiv1.MigrationVerify
	verifyRefusal *apiv1.Error

	// parityInit is what GET /migrate offers for the point of no return;
	// parityReq is the body of the last POST /migrate/initialize-parity and
	// parityRefusal answers it.
	parityInit    *apiv1.MigrationParityInit
	parityReq     string
	parityRefusal *apiv1.Error
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
		if d.phase != "" {
			m.Phase = d.phase
		}
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
		if d.verify != nil {
			m.Verify = apiv1.NewOptMigrationVerify(*d.verify)
		}
		if d.parityInit != nil {
			m.ParityInit = apiv1.NewOptMigrationParityInit(*d.parityInit)
		}
		if d.review != nil {
			rep := m.Report.Value
			rep.Review = apiv1.NewOptMigrationReview(apiv1.MigrationReview{Disks: d.review, Shares: []apiv1.MigrationSharePreview{}, Capture: apiv1.MigrationCapture{State: apiv1.MigrationCaptureStatePresent}})
			m.Report = apiv1.NewOptMigrationReport(rep)
		}
		out, err := m.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/migrate/import":
		body, _ := io.ReadAll(r.Body)
		refusal, status := d.importRefusal, http.StatusBadRequest
		if strings.Contains(string(body), `"undo":true`) {
			d.undoReq = string(body)
			refusal, status = d.undoRefusal, http.StatusConflict
		} else {
			d.importReq = string(body)
		}
		if refusal != nil {
			out, err := refusal.MarshalJSON()
			reply(status, out, err)
			return
		}
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationImport, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/migrate/verify":
		if d.verifyRefusal != nil {
			out, err := d.verifyRefusal.MarshalJSON()
			reply(http.StatusConflict, out, err)
			return
		}
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationVerify, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/migrate/initialize-parity":
		body, _ := io.ReadAll(r.Body)
		d.parityReq = string(body)
		if d.parityRefusal != nil {
			out, err := d.parityRefusal.MarshalJSON()
			reply(http.StatusConflict, out, err)
			return
		}
		j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeMigrationParity, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/"+d.id.String()+"/log":
		w.Header().Set("Content-Type", "application/gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(d.importLog))
		_ = zw.Close()
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/shares":
		nfs := apiv1.ShareNFS{Hosts: []string{}, Squash: apiv1.ShareNFSSquashRootSquash}
		media := apiv1.Share{Name: "media", Path: "/mnt/user/media", CacheMode: apiv1.ShareCacheModeArrayOnly, CreatePolicy: apiv1.ArrayCreatePolicyMfs, Usage: apiv1.NilShareUsage{Null: true}, Nfs: apiv1.ShareNFS{Hosts: []string{}, Squash: apiv1.ShareNFSSquashRootSquash}}
		media.Migration = apiv1.NewOptShareMigration(apiv1.ShareMigration{
			TargetCacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeCacheOnly),
			Notes:           []string{"Unraid's High-water allocation has no equivalent in Hoserva: the share is mapped to Balance across disks (mfs) (Q11)."},
		})
		own := apiv1.Share{Name: "mine", Path: "/mnt/user/mine", CacheMode: apiv1.ShareCacheModeArrayOnly, CreatePolicy: apiv1.ArrayCreatePolicyMfs, Usage: apiv1.NilShareUsage{Null: true}, Nfs: nfs}
		list := apiv1.ListSharesOK{Shares: []apiv1.Share{media, own}}
		out, err := list.MarshalJSON()
		reply(200, out, err)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/users":
		alice := apiv1.UserSummary{ID: uuid.New(), Username: "alice", Role: apiv1.UserRoleShareOnly, CreatedAt: time.Now().UTC()}
		alice.LastLogin.SetToNull()
		admin := apiv1.UserSummary{ID: uuid.New(), Username: "admin", Role: apiv1.UserRoleAdmin, CreatedAt: time.Now().UTC()}
		admin.LastLogin.SetToNull()
		list := apiv1.ListUsersOK{Users: []apiv1.UserSummary{admin, alice}}
		out, err := list.MarshalJSON()
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

func (d *migrateDaemon) importRequest() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.importReq
}

func reviewDisk(slot, serial string, proposed apiv1.MigrationProposedRole, mod func(*apiv1.MigrationDisk)) apiv1.MigrationDisk {
	d := apiv1.MigrationDisk{Slot: apiv1.NewOptString(slot), Serial: apiv1.NewOptString(serial), Device: apiv1.NewOptString("/dev/" + slot)}
	if proposed != "" {
		d.ProposedRole = apiv1.NewOptMigrationProposedRole(proposed)
	}
	if mod != nil {
		mod(&d)
	}
	return d
}

// importDaemon serves the disk table of a scanned array.
func importDaemon(t *testing.T) *migrateDaemon {
	t.Helper()
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	d.review = []apiv1.MigrationDisk{
		reviewDisk("parity", "PAR1", apiv1.MigrationProposedRoleParity, nil),
		reviewDisk("disk1", "DAT1", apiv1.MigrationProposedRoleData, nil),
		reviewDisk("disk2", "DAT2", apiv1.MigrationProposedRoleData, nil),
		reviewDisk("disk3", "DAT3", "", func(m *apiv1.MigrationDisk) { m.Refused = true }),
		reviewDisk("cache", "CAC1", apiv1.MigrationProposedRoleCache, nil),
		reviewDisk("boot", "STICK1", apiv1.MigrationProposedRoleIgnore, nil),
		reviewDisk("nvme0n1", "NVME1", apiv1.MigrationProposedRoleCache, func(m *apiv1.MigrationDisk) { m.HostBoot = apiv1.NewOptBool(true) }),
	}
	return d
}

func TestMigrateImportSendsTheScansProposalAfterTheUserConfirms(t *testing.T) {
	d := importDaemon(t)
	printed, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes")
	if err != nil {
		t.Fatalf("migrate import --yes: %v", err)
	}
	for _, want := range []string{"parity", "serial PAR1", "serial DAT2", "serial CAC1", "adopted read-only"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	for _, never := range []string{"DAT3", "STICK1", "NVME1"} {
		if strings.Contains(d.importRequest(), never) || strings.Contains(printed, never) {
			t.Errorf("%s was sent or printed: a refused disk has no role, a stick is not listed, and the boot disk is never a whole-disk cache\nrequest %s", never, d.importRequest())
		}
	}
	var req apiv1.MigrationImportRequest
	if err := req.UnmarshalJSON([]byte(d.importRequest())); err != nil || !req.Confirm || len(req.Roles) != 4 {
		t.Fatalf("request = %s (%v), want confirm and four roles", d.importRequest(), err)
	}
	got := d.seen()
	if len(got) != 4 || got[0] != "GET /api/v1/migrate" || got[1] != "POST /api/v1/migrate/import" || got[2] != "GET /api/v1/jobs/"+d.id.String() || got[3] != "GET /api/v1/jobs/"+d.id.String()+"/log" {
		t.Errorf("requests = %v", got)
	}
}

// The import's output lists every account with the reminder to set its
// password, which the job's log carries, and says what comes next.
func TestMigrateImportPrintsTheJobsLogWithTheAccountsToSetPasswordsFor(t *testing.T) {
	d := importDaemon(t)
	d.importLog = "account alice created without a password: set one in the web UI (Users) before its SMB clients reconnect\nshare \"Family Photos\" was not created: the name is not valid for Hoserva\n"
	printed, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Import log:", "account alice created without a password", `share "Family Photos" was not created`, "Set a password for each account"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

// Once the import is pending, status lists the shares it seeded with their
// target cache mode and notes, and the accounts that have no password yet.
func TestMigrateStatusListsWhatTheImportSeeded(t *testing.T) {
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseImported
	printed, err := runBackupCLI(t, d.sock, "migrate", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Migration: imported", "Share media: array-only, cache mode cache-only once the cache exists", "mapped to Balance across disks", "Account alice: no password set yet"} {
		if !strings.Contains(printed, want) {
			t.Errorf("status output lacks %q:\n%s", want, printed)
		}
	}
	for _, never := range []string{"Share mine", "Account admin"} {
		if strings.Contains(printed, never) {
			t.Errorf("status lists %q, which the import did not create:\n%s", never, printed)
		}
	}
	d.phase = ""
	printed, err = runBackupCLI(t, d.sock, "migrate", "status")
	if err != nil || strings.Contains(printed, "Share media") {
		t.Errorf("status before an import lists shares: %q, %v", printed, err)
	}
}

// The command never confirms for the user: without --yes it prints the mapping,
// says what the flag is for, and sends nothing.
func TestMigrateImportWithoutYesSendsNothing(t *testing.T) {
	d := importDaemon(t)
	printed, err := runBackupCLI(t, d.sock, "migrate", "import")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("migrate import = %v, want it to ask for --yes", err)
	}
	if !strings.Contains(printed, "serial DAT1") {
		t.Errorf("the mapping was not printed:\n%s", printed)
	}
	if d.importRequest() != "" {
		t.Errorf("a request was sent without --yes: %s", d.importRequest())
	}
}

func TestMigrateImportRoleFlagsOverrideTheProposalAndAddDisks(t *testing.T) {
	d := importDaemon(t)
	if _, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes",
		"--role", "DAT2=ignore", "--role", "wwn:0x5000c500a1=data", "--role", "CAC1=cache",
		"--cache-partition", "nvme-x-part3:aaaa-bbbb"); err != nil {
		t.Fatal(err)
	}
	var req apiv1.MigrationImportRequest
	if err := req.UnmarshalJSON([]byte(d.importRequest())); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]apiv1.MigrationImportRole{}
	for _, r := range req.Roles {
		key := r.Serial.Or("") + "|" + r.Wwn.Or("") + "|" + r.ById.Or("")
		byKey[key] = r.Role
	}
	want := map[string]apiv1.MigrationImportRole{
		"PAR1||": apiv1.MigrationImportRoleParity, "DAT1||": apiv1.MigrationImportRoleData, "DAT2||": apiv1.MigrationImportRoleIgnore,
		"CAC1||": apiv1.MigrationImportRoleCache, "|0x5000c500a1|": apiv1.MigrationImportRoleData, "||nvme-x-part3": apiv1.MigrationImportRoleCache,
	}
	if len(byKey) != len(want) {
		t.Fatalf("roles = %v, want %v", byKey, want)
	}
	for k, v := range want {
		if byKey[k] != v {
			t.Errorf("role %q = %q, want %q (all: %v)", k, byKey[k], v, byKey)
		}
	}
	if len(req.Roles) > 0 && req.Roles[len(req.Roles)-1].PartUuid.Or("") != "aaaa-bbbb" {
		t.Errorf("the cache partition's PARTUUID = %q", req.Roles[len(req.Roles)-1].PartUuid.Or(""))
	}
}

// The scan keys a proposal by WWN when the disk has one; a --role naming the
// same disk by serial, or by WWN in other letters, replaces that proposal and
// does not send the disk twice.
func TestMigrateImportRoleFlagReplacesAProposalKeyedByWWN(t *testing.T) {
	d := importDaemon(t)
	withWWN := func(wwn string) func(*apiv1.MigrationDisk) {
		return func(m *apiv1.MigrationDisk) { m.Wwn = apiv1.NewOptString(wwn) }
	}
	d.review = append(d.review,
		reviewDisk("disk4", "DAT4", apiv1.MigrationProposedRoleData, withWWN("0x5000c500beef")),
		reviewDisk("disk5", "DAT5", apiv1.MigrationProposedRoleData, withWWN("0x5000c500cafe")),
		reviewDisk("disk6", "DAT6", "", withWWN("0x5000c500f00d")))
	if _, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes",
		"--role", "DAT4=ignore", "--role", "wwn:0x5000C500CAFE=cache", "--role", "DAT6=data"); err != nil {
		t.Fatal(err)
	}
	var req apiv1.MigrationImportRequest
	if err := req.UnmarshalJSON([]byte(d.importRequest())); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]apiv1.MigrationImportRole{}
	for _, r := range req.Roles {
		key := r.Serial.Or("") + "|" + r.Wwn.Or("")
		if _, dup := byKey[key]; dup {
			t.Errorf("%q was sent twice", key)
		}
		byKey[key] = r.Role
	}
	for k, v := range map[string]apiv1.MigrationImportRole{
		"|0x5000c500beef": apiv1.MigrationImportRoleIgnore, "|0x5000c500cafe": apiv1.MigrationImportRoleCache, "|0x5000c500f00d": apiv1.MigrationImportRoleData,
	} {
		if byKey[k] != v {
			t.Errorf("role %q = %q, want %q (all: %v)", k, byKey[k], v, byKey)
		}
	}
	for _, serial := range []string{"DAT4", "DAT5", "DAT6"} {
		if _, ok := byKey[serial+"|"]; ok {
			t.Errorf("%s was sent by serial beside its WWN", serial)
		}
	}
	if len(byKey) != 7 {
		t.Errorf("roles = %v, want one entry per disk", byKey)
	}
}

func TestMigrateImportRefusesWhatCannotBeSent(t *testing.T) {
	d := importDaemon(t)
	for _, args := range [][]string{
		{"--role", "DAT1"},
		{"--role", "=data"},
		{"--role", "DAT1=boot"},
		{"--cache-partition", "nvme-x-part3"},
		{"--cache-partition", ":aaaa"},
	} {
		if _, err := runBackupCLI(t, d.sock, append([]string{"migrate", "import", "--yes"}, args...)...); err == nil {
			t.Errorf("migrate import %v was accepted", args)
		}
	}
	if d.importRequest() != "" {
		t.Errorf("a malformed flag sent %s", d.importRequest())
	}

	// With nothing proposed, as when the capture has no disks.ini, every role is
	// the user's and none is invented.
	bare := startMigrateDaemon(t)
	_, err := runBackupCLI(t, bare.sock, "migrate", "import", "--yes")
	if err == nil || !strings.Contains(err.Error(), "--role") {
		t.Errorf("migrate import with nothing to send = %v, want it to ask for --role", err)
	}
	if bare.importRequest() != "" {
		t.Errorf("a request was sent with no roles: %s", bare.importRequest())
	}
}

func TestMigrateImportRefusalAndFailedJobAreCommandFailures(t *testing.T) {
	d := importDaemon(t)
	d.importRefusal = &apiv1.Error{Code: "invalid_import_roles", Message: "this role is not allowed for this disk"}
	if _, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes"); err == nil || !strings.Contains(err.Error(), "this role is not allowed for this disk") {
		t.Errorf("migrate import = %v, want the refusal", err)
	}
	d.importRefusal = nil
	d.jobStatus = apiv1.JobStatusFailed
	if _, err := runBackupCLI(t, d.sock, "migrate", "import", "--yes"); err == nil || !strings.Contains(err.Error(), "migration import") {
		t.Errorf("migrate import with a failed job = %v, want it named", err)
	}
}

func TestMigrateUndoImportQueuesTheUndoWaitsForTheJobAndSaysNothingWasWritten(t *testing.T) {
	d := importDaemon(t)
	printed, err := runBackupCLI(t, d.sock, "migrate", "undo-import", "--yes")
	if err != nil {
		t.Fatalf("migrate undo-import: %v", err)
	}
	if !strings.Contains(printed, "nothing was written") {
		t.Errorf("output = %q", printed)
	}
	got := d.seen()
	if len(got) != 2 || got[0] != "POST /api/v1/migrate/import" || got[1] != "GET /api/v1/jobs/"+d.id.String() {
		t.Errorf("requests = %v", got)
	}
	var req apiv1.MigrationImportRequest
	if err := req.UnmarshalJSON([]byte(d.undoReq)); err != nil || !req.Confirm || !req.Undo.Or(false) || len(req.Roles) != 0 || d.importReq != "" {
		t.Errorf("undo request = %s (%v), import request %q: want confirm and undo, no roles, and nothing adopted", d.undoReq, err, d.importReq)
	}
}

// The command never confirms for the user: without --yes it says what the undo
// does and sends nothing.
func TestMigrateUndoImportWithoutYesSendsNothing(t *testing.T) {
	d := importDaemon(t)
	_, err := runBackupCLI(t, d.sock, "migrate", "undo-import")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "forgets the verify result") {
		t.Errorf("migrate undo-import without --yes = %v, want it to say what the undo does and to ask for --yes", err)
	}
	if got := d.seen(); len(got) != 0 || d.undoReq != "" || d.importReq != "" {
		t.Errorf("requests = %v, undo %q, import %q: a request was sent without --yes", got, d.undoReq, d.importReq)
	}
}

func TestMigrateUndoImportRefusalAndFailedJobAreCommandFailures(t *testing.T) {
	d := importDaemon(t)
	d.undoRefusal = &apiv1.Error{Code: "no_import_pending", Message: "there is no pending Unraid import to undo"}
	if _, err := runBackupCLI(t, d.sock, "migrate", "undo-import", "--yes"); err == nil || !strings.Contains(err.Error(), "there is no pending Unraid import to undo") {
		t.Errorf("migrate undo-import = %v, want the refusal", err)
	}
	d.undoRefusal = nil
	d.jobStatus = apiv1.JobStatusFailed
	if _, err := runBackupCLI(t, d.sock, "migrate", "undo-import", "--yes"); err == nil || !strings.Contains(err.Error(), "undoing the import") {
		t.Errorf("migrate undo-import with a failed job = %v, want it named", err)
	}
}

func verifyScope(name string, expected, found int64, passed bool) apiv1.MigrationVerifyScope {
	list := apiv1.MigrationVerifyList{Paths: []string{}}
	return apiv1.MigrationVerifyScope{
		Name: name, Passed: passed, Expected: apiv1.MigrationVerifyCounts{Files: expected, Bytes: expected * 10}, Found: apiv1.MigrationVerifyCounts{Files: found, Bytes: found * 10},
		Hashed: 3, Missing: list, Extra: list, SizeChanged: list, ChecksumChanged: list, Changed: list,
	}
}

func TestMigrateVerifyPrintsThePassingComparisonAndExitsZero(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	d.verify = &apiv1.MigrationVerify{
		Status: apiv1.MigrationVerifyStatusPassed, StartedAt: time.Now().UTC(), Duplicates: 1,
		Disks:           []apiv1.MigrationVerifyScope{verifyScope("disk1", 5, 5, true)},
		Shares:          []apiv1.MigrationVerifyScope{verifyScope("media", 5, 5, true)},
		DuplicateSample: []apiv1.MigrationVerifyDuplicate{{Path: "media/a.txt", Disks: []string{"disk1", "disk2"}}},
	}
	printed, err := runBackupCLI(t, d.sock, "migrate", "verify")
	if err != nil {
		t.Fatalf("migrate verify: %v", err)
	}
	for _, want := range []string{"disk disk1", "share media", "match", "media/a.txt (disk1, disk2)", "Verify passed"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	got := d.seen()
	if len(got) != 3 || got[0] != "POST /api/v1/migrate/verify" || got[1] != "GET /api/v1/jobs/"+d.id.String() || got[2] != "GET /api/v1/migrate" {
		t.Errorf("requests = %v, want the start, one poll and the session", got)
	}
}

// A mismatch prints what differs and exits non-zero, and so does a job that
// succeeded without a passing result: the command never reads silence as a pass.
func TestMigrateVerifyExitsNonZeroUnlessEverythingPassed(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })

	t.Run("a mismatch", func(t *testing.T) {
		d := startMigrateDaemon(t)
		d.jobStatus = apiv1.JobStatusFailed
		bad := verifyScope("disk1", 5, 5, false)
		bad.SizeChanged = apiv1.MigrationVerifyList{Total: 1, Paths: []string{"media/a.txt"}}
		d.verify = &apiv1.MigrationVerify{
			Status: apiv1.MigrationVerifyStatusFailed, StartedAt: time.Now().UTC(),
			Disks: []apiv1.MigrationVerifyScope{bad}, Shares: []apiv1.MigrationVerifyScope{},
		}
		printed, err := runBackupCLI(t, d.sock, "migrate", "verify")
		if err == nil || !strings.Contains(err.Error(), "failed") {
			t.Fatalf("migrate verify = %v, want it to fail", err)
		}
		for _, want := range []string{"DIFFERS", "disk1: 1 size changed", "media/a.txt"} {
			if !strings.Contains(printed, want) {
				t.Errorf("output lacks %q:\n%s", want, printed)
			}
		}
		if strings.Contains(printed, "Verify passed") {
			t.Errorf("a failed verify printed that it passed:\n%s", printed)
		}
	})
	for name, result := range map[string]*apiv1.MigrationVerify{
		"a succeeded job with no result":             nil,
		"a succeeded job whose result is not a pass": {Status: apiv1.MigrationVerifyStatusFailed, StartedAt: time.Now().UTC(), Disks: []apiv1.MigrationVerifyScope{}, Shares: []apiv1.MigrationVerifyScope{}},
	} {
		t.Run(name, func(t *testing.T) {
			d := startMigrateDaemon(t)
			d.verify = result
			if _, err := runBackupCLI(t, d.sock, "migrate", "verify"); err == nil || !strings.Contains(err.Error(), "do not go on") {
				t.Errorf("migrate verify = %v, want it to refuse to go on", err)
			}
		})
	}
	t.Run("a cancelled job", func(t *testing.T) {
		d := startMigrateDaemon(t)
		d.jobStatus = apiv1.JobStatusCancelled
		if _, err := runBackupCLI(t, d.sock, "migrate", "verify"); err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("migrate verify = %v, want it to say the job was cancelled", err)
		}
	})
	t.Run("a refusal", func(t *testing.T) {
		d := startMigrateDaemon(t)
		d.verifyRefusal = &apiv1.Error{Code: "no_import_pending", Message: "there is no adopted Unraid array to verify"}
		if _, err := runBackupCLI(t, d.sock, "migrate", "verify"); err == nil || !strings.Contains(err.Error(), "no adopted Unraid array") {
			t.Errorf("migrate verify = %v, want the daemon's refusal", err)
		}
	})
}

func offeredParityInit() *apiv1.MigrationParityInit {
	return &apiv1.MigrationParityInit{
		Confirmation:      apiv1.NewOptString("ERASE /dev/nvme0n1p4, /dev/sdb"),
		UnprotectedWindow: "Between the moment Unraid's array stopped and the moment the initial sync completes, the array has no redundancy whatsoever.",
		Rollback:          []string{"Once the former parity disk(s) are formatted, going back means restoring from backup."},
		Erases: []apiv1.MigrationParityErase{
			{Role: apiv1.MigrationParityEraseRoleParity, Device: "/dev/sdb", Serial: apiv1.NewOptString("PAR1"), Size: apiv1.NewOptInt64(8 << 40)},
			{Role: apiv1.MigrationParityEraseRoleCache, Device: "/dev/nvme0n1p4", Partition: true},
		},
	}
}

// Without --confirm the command formats nothing: it prints the unprotected
// window, what rollback means, every device erased and the string to type back,
// and exits non-zero without calling the daemon's operation.
func TestMigrateInitializeParityWithoutConfirmPrintsThePlanAndFormatsNothing(t *testing.T) {
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseVerified
	d.parityInit = offeredParityInit()
	printed, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity")
	if err == nil || !strings.Contains(err.Error(), "--confirm") || !strings.Contains(err.Error(), "ERASE /dev/nvme0n1p4, /dev/sdb") {
		t.Fatalf("migrate initialize-parity = %v, want it to refuse and say what --confirm needs", err)
	}
	for _, want := range []string{"no redundancy whatsoever", "Rollback: Once the former parity", "Erases the parity /dev/sdb (the whole disk), serial PAR1, 8192.0 GiB", "Erases the cache /dev/nvme0n1p4 (this partition only; the rest of its disk is left alone)", "Confirmation: ERASE /dev/nvme0n1p4, /dev/sdb"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	for _, req := range d.seen() {
		if strings.HasPrefix(req, "POST") {
			t.Errorf("a command with no --confirm called %s", req)
		}
	}
}

func TestMigrateInitializeParityIsNotOfferedBeforeAPassingVerify(t *testing.T) {
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseVerifyFailed
	_, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity", "--confirm", "ERASE /dev/sdb")
	if err == nil || !strings.Contains(err.Error(), "not offered") {
		t.Fatalf("migrate initialize-parity = %v, want it to say it is not offered", err)
	}
	for _, req := range d.seen() {
		if strings.HasPrefix(req, "POST") {
			t.Errorf("the command called %s although the daemon does not offer the point of no return", req)
		}
	}
}

// --confirm is sent exactly as typed, never filled in from the daemon's own
// string: a command that confirmed for the user would be no confirmation.
func TestMigrateInitializeParitySendsTheTypedConfirmationAndWaitsForTheJob(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseVerified
	d.parityInit = offeredParityInit()
	d.importLog = "the former parity disk(s) and the cache are formatted XFS\nthe initial sync is queued\n"

	printed, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity", "--confirm", "ERASE /dev/nvme0n1p4, /dev/sdb")
	if err != nil {
		t.Fatalf("migrate initialize-parity: %v", err)
	}
	if d.parityReq != `{"confirmation":"ERASE /dev/nvme0n1p4, /dev/sdb"}` {
		t.Errorf("request = %s, want exactly the typed confirmation", d.parityReq)
	}
	for _, want := range []string{"no redundancy whatsoever", "Parity initialisation log:", "the initial sync is queued", "until it completes the array has no redundancy"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}

	d2 := startMigrateDaemon(t)
	d2.phase = apiv1.MigrationPhaseVerified
	d2.parityInit = offeredParityInit()
	if _, err := runBackupCLI(t, d2.sock, "migrate", "initialize-parity", "--confirm", "erase everything"); err != nil {
		t.Fatalf("migrate initialize-parity: %v", err)
	}
	if d2.parityReq != `{"confirmation":"erase everything"}` {
		t.Errorf("request = %s: the command replaced what the user typed", d2.parityReq)
	}
}

func TestMigrateInitializeParityReportsARefusalAndAFailedJob(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	t.Run("the daemon refuses the confirmation", func(t *testing.T) {
		d := startMigrateDaemon(t)
		d.phase = apiv1.MigrationPhaseVerified
		d.parityInit = offeredParityInit()
		d.parityRefusal = &apiv1.Error{Code: "confirmation_required", Message: "this operation requires an explicit confirmation"}
		if _, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity", "--confirm", "wrong"); err == nil || !strings.Contains(err.Error(), "explicit confirmation") {
			t.Errorf("migrate initialize-parity = %v, want the daemon's refusal", err)
		}
	})
	t.Run("a job that failed", func(t *testing.T) {
		d := startMigrateDaemon(t)
		d.phase = apiv1.MigrationPhaseVerified
		d.parityInit = offeredParityInit()
		d.jobStatus = apiv1.JobStatusFailed
		_, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity", "--confirm", "ERASE /dev/nvme0n1p4, /dev/sdb")
		if err == nil || !strings.Contains(err.Error(), "failed") {
			t.Errorf("migrate initialize-parity = %v, want it to fail with the job", err)
		}
	})
}

// An initialisation that stopped after the disks were formatted is finished with
// the confirmation it prints, which names nothing to erase.
func TestMigrateInitializeParityFinishesAnUnfinishedInitialisation(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseInitializing
	d.parityInit = &apiv1.MigrationParityInit{
		Finishing: true, Confirmation: apiv1.NewOptString("FINISH PARITY INITIALISATION"), Erases: []apiv1.MigrationParityErase{},
		UnprotectedWindow: "no redundancy whatsoever", Rollback: []string{"restoring from backup"},
	}
	printed, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity")
	if err == nil || !strings.Contains(err.Error(), "FINISH PARITY INITIALISATION") || !strings.Contains(printed, "it erases nothing") {
		t.Fatalf("migrate initialize-parity = %v\n%s, want the finishing confirmation and a statement that nothing is erased", err, printed)
	}
	if _, err := runBackupCLI(t, d.sock, "migrate", "initialize-parity", "--confirm", "FINISH PARITY INITIALISATION"); err != nil {
		t.Fatalf("migrate initialize-parity --confirm: %v", err)
	}
	if d.parityReq != `{"confirmation":"FINISH PARITY INITIALISATION"}` {
		t.Errorf("request = %s", d.parityReq)
	}
}

func TestMigrateStatusInInitializingPrintsWhatIsLeft(t *testing.T) {
	d := startMigrateDaemon(t)
	d.phase = apiv1.MigrationPhaseInitializing
	d.parityInit = &apiv1.MigrationParityInit{
		Finishing: true, Confirmation: apiv1.NewOptString("FINISH PARITY INITIALISATION"), Erases: []apiv1.MigrationParityErase{},
		UnprotectedWindow: "no redundancy whatsoever", Rollback: []string{},
	}
	printed, err := runBackupCLI(t, d.sock, "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	for _, want := range []string{"initializing", "Confirmation: FINISH PARITY INITIALISATION"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}
