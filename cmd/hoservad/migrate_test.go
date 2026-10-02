package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/backup"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

func flashBackupZip(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"changes.txt":              "# Version " + version + " 2026-01-01\n",
		"bzimage":                  "kernel",
		"config/disk.cfg":          "startArray=\"yes\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
		"config/hoserva/disks.ini": "[\"disk1\"]\nidx=\"1\"\nid=\"M_WIREDSERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\n",
	} {
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

// paddedFlashBackupZip is flashBackupZip with an incompressible, stored entry
// the scan never reads, so the upload is about pad bytes long.
func paddedFlashBackupZip(t *testing.T, version string, pad int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range map[string]string{
		"changes.txt":     "# Version " + version + " 2026-01-01\n",
		"bzimage":         "kernel",
		"config/disk.cfg": "startArray=\"yes\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(content))
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "extra/pad.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	filler := make([]byte, pad)
	if _, err := rand.Read(filler); err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(filler)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (w *containersWiringHarness) uploadScan(t *testing.T, zipData []byte, unverified bool) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if zipData != nil {
		part, err := mw.CreateFormFile("file", "boot.zip")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(zipData)
	}
	if unverified {
		_ = mw.WriteField("unverifiedLayout", "true")
	}
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, "http://unix"+apiPathPrefix+"/migrate/scan", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestMigrationWiring_OperationsAnswer501WhenNothingIsWired(t *testing.T) {
	w := newContainersWiringHarness(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/migrate"}, {http.MethodGet, "/migrate/report"}, {http.MethodDelete, "/migrate"},
	} {
		req, _ := http.NewRequest(tc.method, "http://unix"+apiPathPrefix+tc.path, nil)
		resp, err := w.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s without wiring = %d, want 501", tc.method, tc.path, resp.StatusCode)
		}
	}
	if status, _ := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false); status != http.StatusNotImplemented {
		t.Errorf("POST /migrate/scan without wiring = %d, want 501", status)
	}
}

// The three operations reach the real service through the daemon's server and
// its job registry: a scan queued over HTTP runs as a migration_scan job, and
// its report is read back as rows and as a document.
func TestMigrationWiring_ScanIsReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40})
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}

	status, body := w.do(t, http.MethodGet, "/migrate")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"none"`)) {
		t.Fatalf("GET /migrate before a scan = %d %s", status, body)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate/report"); status != http.StatusNotFound || !bytes.Contains(body, []byte("no_migration_report")) {
		t.Fatalf("GET /migrate/report before a scan = %d %s", status, body)
	}
	if status, body := w.uploadScan(t, nil, false); status != http.StatusBadRequest {
		t.Fatalf("POST /migrate/scan with no file = %d %s, want 400", status, body)
	}
	if status, body := w.uploadScan(t, flashBackupZip(t, "6.9.2"), false); status != http.StatusBadRequest || !bytes.Contains(body, []byte("unsupported_layout")) {
		t.Fatalf("POST /migrate/scan of an unknown version = %d %s, want 400 unsupported_layout", status, body)
	}

	status, body = w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Class string `json:"class"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Type != "migration_scan" || queued.Class != "topology" {
		t.Fatalf("job = %s (%v), want a migration_scan topology job", body, err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.do(t, http.MethodGet, "/migrate")
	var got struct {
		Phase  string `json:"phase"`
		Report struct {
			UnraidVersion string `json:"unraidVersion"`
			Verdict       string `json:"verdict"`
			Rows          []struct {
				Check   string `json:"check"`
				Status  string `json:"status"`
				Subject string `json:"subject"`
			} `json:"rows"`
		} `json:"report"`
	}
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil || got.Phase != "scanned" || got.Report.UnraidVersion != "7.3.2" {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	var mapped bool
	for _, row := range got.Report.Rows {
		mapped = mapped || (row.Check == "disk_mapping" && row.Subject == "disk1" && row.Status == "pass")
	}
	if !mapped {
		t.Errorf("the report has no passing disk_mapping row for disk1: %s", body)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/report")
	if status != http.StatusOK || !strings.HasPrefix(string(body), "# Hoserva migration scan report") {
		t.Fatalf("GET /migrate/report = %d %s", status, body)
	}

	stored, _ := filepath.Glob(filepath.Join(w.root, "migrate", "upload-*.zip"))
	if len(stored) != 1 {
		t.Fatalf("stored zips = %v, want the one source", stored)
	}
	if info, _ := os.Stat(stored[0]); info.Mode().Perm() != 0o600 {
		t.Errorf("the stored zip is %v, want 0600", info.Mode().Perm())
	}

	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent {
		t.Fatalf("DELETE /migrate = %d %s", status, body)
	}
	if left, _ := filepath.Glob(filepath.Join(w.root, "migrate", "*")); len(left) != 0 {
		t.Errorf("DELETE /migrate left %v", left)
	}
}

// A scan the previous process left running is failed at start, never shown as
// running forever.
func TestMigrationWiring_AScanLeftRunningByThePreviousProcessIsFailedAtStart(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "migrate")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upload-x.zip"), []byte("zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newContainersWiringHarness(t)
	sessions := store.NewMigrationSessionStore(w.db)
	if err := sessions.Put(context.Background(), store.MigrationSession{ScanFile: "upload-x.zip", ScanSize: 3, ScanReceivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), sessions, root); err != nil {
		t.Fatal(err)
	}
	status, body := w.do(t, http.MethodGet, "/migrate")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"scan_failed"`)) {
		t.Fatalf("GET /migrate = %d %s, want scan_failed", status, body)
	}
}

// main.go must call wireMigration with the real disk provider, the database and
// the state directory: a handler nobody wires answers 501 to every migrate operation.
func TestMain_WiresTheMigrator(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "wireMigration" && len(call.Args) == 6 {
			disks, okDisks := call.Args[3].(*ast.Ident)
			sessions, okSessions := call.Args[4].(*ast.CallExpr)
			dir, okDir := call.Args[5].(*ast.Ident)
			if okDisks && okSessions && okDir && disks.Name == "disks" && dir.Name == "absStateDir" {
				if sel, ok := sessions.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewMigrationSessionStore" {
					found = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call wireMigration(ctx, handler, registry, disks, store.NewMigrationSessionStore(db), absStateDir)")
	}
}

// The uploaded Flash Backup zip holds secrets and is never part of a config
// archive, even with the state directory it sits in named in the archive's paths.
func TestMigrationZipIsNeverInAConfigArchive(t *testing.T) {
	box := newWireBox(t, false)
	state := filepath.Join(box.root, "state")
	box.handler.Backup.Paths.StateDir = state
	if err := os.MkdirAll(filepath.Join(state, "migrate"), 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "WIREGUARD-PRIVATE-KEY-IN-THE-FLASH-BACKUP"
	if err := os.WriteFile(filepath.Join(state, "migrate", "upload-abc.zip"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "export.tar.zst")
	if err := os.WriteFile(archive, exportArchive(t, box.handler), 0o600); err != nil {
		t.Fatal(err)
	}
	tree, err := backup.ExtractVerifiedArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tree) })
	files := 0
	err = filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(p, "upload-") || bytes.Contains(data, []byte(secret)) {
			t.Errorf("the archive holds the migration zip: %s", p)
		}
		return nil
	})
	if err != nil || files == 0 {
		t.Fatalf("walking the archive: %v (%d files)", err, files)
	}
}

// A real Flash Backup is far larger than the 64 KiB every other request body is
// held to: the upload goes through the daemon's own body limit.
func TestMigrationWiring_AScanUploadLargerThanTheGeneralBodyLimitIsAccepted(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	data := paddedFlashBackupZip(t, "7.3.2", 8<<20)
	if len(data) <= 2*maxRequestBodyBytes {
		t.Fatalf("the test zip is %d bytes", len(data))
	}
	status, body := w.uploadScan(t, data, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan of %d bytes = %d %s, want 200", len(data), status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate"); status != http.StatusOK || !bytes.Contains(body, []byte(`"scanned"`)) {
		t.Fatalf("GET /migrate = %d %s", status, body)
	}
}

// The scan upload's body limit is its own: past it the answer is the spec's 413
// zip_too_large, and the ordinary limit still holds for every other operation.
func TestMigrationWiring_AScanUploadPastItsBodyLimitIsRefusedAsTooLarge(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	server, err := apiv1.NewServer(w.handler, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix(apiPathPrefix), apiv1.WithErrorHandler(decodeError))
	if err != nil {
		t.Fatal(err)
	}
	const limit = 256 << 10
	ts := httptest.NewServer(limitRequestBodyWith(server, limit, 0))
	defer ts.Close()

	post := func(path, contentType string, body []byte) (int, string) {
		req, err := http.NewRequest(http.MethodPost, ts.URL+apiPathPrefix+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	multipartOf := func(zipData []byte) (string, []byte) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "boot.zip")
		_, _ = part.Write(zipData)
		_ = mw.Close()
		return mw.FormDataContentType(), buf.Bytes()
	}

	ct, within := multipartOf(paddedFlashBackupZip(t, "7.3.2", 128<<10))
	if status, out := post("/migrate/scan", ct, within); status != http.StatusOK {
		t.Fatalf("an upload of %d bytes under the %d limit = %d %s, want 200", len(within), limit, status, out)
	}
	ct, over := multipartOf(paddedFlashBackupZip(t, "7.3.2", 2*limit))
	status, out := post("/migrate/scan", ct, over)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(out, `"zip_too_large"`) {
		t.Fatalf("an upload of %d bytes over the %d limit = %d %s, want 413 zip_too_large", len(over), limit, status, out)
	}
	status, out = post("/auth/login", "application/json", []byte(`{"username":"`+strings.Repeat("a", 2*maxRequestBodyBytes)+`"}`))
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(out, "request_too_large") {
		t.Errorf("an oversized login body = %d %s, want 413 request_too_large", status, out)
	}
}

// The larger upload limit is only ever reached by a caller the generated
// server has authenticated: a request without a session is answered 401 with
// its body unread, so the limit and the longer read deadline cost nothing to an
// unauthenticated caller.
func TestMigrationWiring_AnUnauthenticatedScanUploadIsRefusedBeforeItsBodyIsRead(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	server, err := apiv1.NewServer(w.handler, &api.SessionSecurityHandler{}, apiv1.WithPathPrefix(apiPathPrefix), apiv1.WithErrorHandler(decodeError))
	if err != nil {
		t.Fatal(err)
	}
	var read atomic.Int64
	counted := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.Body = &countingBody{ReadCloser: r.Body, n: &read}
		server.ServeHTTP(rw, r)
	})
	ts := httptest.NewServer(limitRequestBodyWith(counted, migrate.UploadBodyLimit, migrate.UploadReadTimeout))
	defer ts.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "boot.zip")
	_, _ = part.Write(paddedFlashBackupZip(t, "7.3.2", 128<<10))
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, ts.URL+apiPathPrefix+"/migrate/scan", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated POST /migrate/scan = %d, want 401", resp.StatusCode)
	}
	if n := read.Load(); n != 0 {
		t.Errorf("the handler chain read %d bytes of an unauthenticated upload, want none", n)
	}
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// queueScanBehindAScrub holds a scrub running, so the scan queues behind it, and
// returns the scan's job id. release lets the scrub end.
func queueScanBehindAScrub(t *testing.T, w *containersWiringHarness) (scanID string, release func()) {
	t.Helper()
	stop := make(chan struct{})
	var once atomic.Bool
	release = func() {
		if once.CompareAndSwap(false, true) {
			close(stop)
		}
	}
	t.Cleanup(release)
	started := make(chan struct{})
	w.registry.Register(job.TypeScrub, false, func(ctx context.Context, rc *job.RunContext) error {
		close(started)
		select {
		case <-stop:
		case <-rc.StopRequested():
		case <-ctx.Done():
		}
		return nil
	})
	if _, err := w.scheduler.Submit(context.Background(), job.TypeScrub, nil, nil); err != nil {
		t.Fatal(err)
	}
	<-started

	status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &queued); err != nil || queued.Status != "queued" {
		t.Fatalf("the scan job = %s (%v), want it queued behind the scrub", body, err)
	}
	return queued.ID, release
}

// A scan job that ends without running leaves no session stuck in scanning: the
// zip it holds can be forgotten and a new scan started.
func TestMigrationWiring_ACancelledQueuedScanIsFailedAndCanBeForgottenAndRepeated(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	scanID, release := queueScanBehindAScrub(t, w)
	if status, body := w.do(t, http.MethodGet, "/migrate"); status != http.StatusOK || !bytes.Contains(body, []byte(`"scanning"`)) {
		t.Fatalf("GET /migrate with the scan queued = %d %s, want scanning", status, body)
	}

	if status, body := w.do(t, http.MethodPost, "/jobs/"+scanID+"/cancel"); status != http.StatusOK {
		t.Fatalf("cancel = %d %s", status, body)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate"); status != http.StatusOK || !bytes.Contains(body, []byte(`"scan_failed"`)) || !bytes.Contains(body, []byte("cancelled")) {
		t.Fatalf("GET /migrate after the cancel = %d %s, want scan_failed naming the cancel", status, body)
	}
	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent {
		t.Fatalf("DELETE /migrate after the cancel = %d %s, want 204", status, body)
	}
	if left, _ := filepath.Glob(filepath.Join(w.root, "migrate", "*")); len(left) != 0 {
		t.Errorf("forget left %v", left)
	}

	release()
	status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("a new scan after the cancel = %d %s, want 200", status, body)
	}
	var again struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &again)
	if done := w.awaitJobByID(t, again.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("the new scan = %s %s", done.Status, done.ErrorMessage)
	}
}

// Array stop marks a queued scan interrupted without running it; the session
// is failed, not stuck.
func TestMigrationWiring_AQueuedScanDroppedByMaintenanceIsFailedAndCanBeForgottenAndRepeated(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	scanID, _ := queueScanBehindAScrub(t, w)

	if err := w.scheduler.EnterMaintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.awaitJobByID(t, scanID); got.Status != job.StatusInterrupted {
		t.Fatalf("the queued scan = %s, want interrupted", got.Status)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate"); status != http.StatusOK || !bytes.Contains(body, []byte(`"scan_failed"`)) || !bytes.Contains(body, []byte("interrupted")) {
		t.Fatalf("GET /migrate after maintenance = %d %s, want scan_failed naming the interruption", status, body)
	}
	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent {
		t.Fatalf("DELETE /migrate after maintenance = %d %s, want 204", status, body)
	}
	if left, _ := filepath.Glob(filepath.Join(w.root, "migrate", "*")); len(left) != 0 {
		t.Errorf("forget left %v", left)
	}

	w.scheduler.ExitMaintenance()
	status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("a new scan after maintenance = %d %s, want 200", status, body)
	}
	var again struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &again)
	if done := w.awaitJobByID(t, again.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("the new scan = %s %s", done.Status, done.ErrorMessage)
	}
}
