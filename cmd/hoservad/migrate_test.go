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
	return flashBackupZipWith(t, version, nil)
}

// flashBackupZipWith is flashBackupZip with extra entries.
func flashBackupZipWith(t *testing.T, version string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entries := map[string]string{
		"changes.txt":              "# Version " + version + " 2026-01-01\n",
		"bzimage":                  "kernel",
		"config/disk.cfg":          "startArray=\"yes\"\ndiskIdSlot.1=\"-\"\ndiskFsType.1=\"xfs\"\n",
		"config/hoserva/disks.ini": "[\"disk1\"]\nidx=\"1\"\nid=\"M_WIREDSERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\n",
	}
	for name, content := range extra {
		entries[name] = content
	}
	for name, content := range entries {
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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

// The configuration inventory is part of the report the daemon serves: the rows
// in GET /migrate and the document GET /migrate/report downloads.
func TestMigrationWiring_TheConfigurationInventoryIsInTheServedReport(t *testing.T) {
	w := newContainersWiringHarness(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40})
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	inspect := func(name, label string) string {
		return `{"Name":"/` + name + `","State":{"Status":"running","Running":true},"Config":{"Labels":{` + label + `}}}`
	}
	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/share.cfg":                                     "shareMoverSchedule=\"40 3 * * *\"\n",
		"config/shares/media.cfg":                              "shareAllocator=\"highwater\"\nshareUseCache=\"no\"\nshareExport=\"e\"\n",
		"config/shares/backup.cfg":                             "shareAllocator=\"fillup\"\nshareUseCache=\"no\"\nshareExport=\"-\"\n",
		"config/passwd":                                        "root:x:0:0:r:/root:/bin/bash\nalice:x:1000:100:a:/dev/null:/bin/false\n",
		"config/parity-checks.log":                             "2020 Jan 05 03:00:02|31845|125.6 MB/s|0|0\n",
		"config/hoserva/capture.json":                          `{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":{"mode":"usb"},"docker":{"state":"running","directory_location":"array","writable_layers":[]},"libvirt_img_location":"none"}`,
		"config/hoserva/containers.json":                       "[" + inspect("notes", `"net.unraid.docker.managed":"dockerman"`) + "," + inspect("handmade", "") + "]",
		"config/hoserva/autostart":                             "notes 30\n",
		"config/plugins/dockerMan/templates-user/my-notes.xml": `<Container version="2"><Name>notes</Name></Container>`,
		"config/plugins/user.scripts/scripts/nightly/script":   "#!/bin/bash\n",
		"config/plugins/user.scripts/customSchedule.cron":      "30 2 * * * /usr/local/emhttp/plugins/user.scripts/startCustom.php /boot/config/plugins/user.scripts/scripts/nightly/script\n",
	})
	status, body := w.uploadScan(t, zipData, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
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

	status, body = w.do(t, http.MethodGet, "/migrate")
	var got struct {
		Report struct {
			Rows []struct {
				Check   string `json:"check"`
				Status  string `json:"status"`
				Subject string `json:"subject"`
				Detail  string `json:"detail"`
			} `json:"rows"`
		} `json:"report"`
	}
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	has := func(check, status, subject, detail string) bool {
		for _, row := range got.Report.Rows {
			if row.Check == check && row.Status == status && row.Subject == subject && strings.Contains(row.Detail, detail) {
				return true
			}
		}
		return false
	}
	for _, want := range []struct{ check, status, subject, detail string }{
		{"shares", "flag", "media", "High-water: no exact equivalent, mapped to Balance across disks (mfs)"},
		{"shares", "info", "backup", "Fill-up maps to Fill disks in order (ff)"},
		{"docker_templates", "info", "", "1 template parsed: 1 autostart, 0 running, 0 stopped, 0 template only"},
		{"containers", "flag", "handmade", "Created by hand"},
		{"user_scripts", "info", "nightly", "30 2 * * *"},
		{"parity_history", "warn", "", "days old"},
		{"users", "info", "", "1 user account: alice"},
		{"settings", "info", "mover schedule", "40 3 * * *"},
	} {
		if !has(want.check, want.status, want.subject, want.detail) {
			t.Errorf("GET /migrate has no %s %s row for %q containing %q: %s", want.check, want.status, want.subject, want.detail, body)
		}
	}
	if bytes.Contains(body, []byte(`"import"`)) {
		t.Errorf("the parsed import model is in the API report: %s", body)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/report")
	if status != http.StatusOK {
		t.Fatalf("GET /migrate/report = %d %s", status, body)
	}
	for _, want := range []string{"## Share configuration", "| flag | media |", "## User Scripts (plugin)", "| info | nightly |", "## Last Unraid parity check"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the downloadable report lacks %q:\n%s", want, body)
		}
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), sessions, root); err != nil {
		t.Fatal(err)
	}
	status, body := w.do(t, http.MethodGet, "/migrate")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"scan_failed"`)) {
		t.Fatalf("GET /migrate = %d %s, want scan_failed", status, body)
	}
}

// main.go must call wireMigration with the real disk provider, the real
// read-only mounter, the database and the state directory: a handler nobody
// wires answers 501 to every migrate operation, and a mounter that is not the
// read-only one would mount the Unraid stick read-write.
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
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "wireMigration" && len(call.Args) == 7 {
			disks, okDisks := call.Args[3].(*ast.Ident)
			mounter, okMounter := call.Args[4].(*ast.CompositeLit)
			sessions, okSessions := call.Args[5].(*ast.CallExpr)
			dir, okDir := call.Args[6].(*ast.Ident)
			if okDisks && okMounter && okSessions && okDir && disks.Name == "disks" && dir.Name == "absStateDir" {
				sel, okSel := mounter.Type.(*ast.SelectorExpr)
				if sessSel, ok := sessions.Fun.(*ast.SelectorExpr); ok && okSel && sel.Sel.Name == "KernelReadOnlyMounter" && sessSel.Sel.Name == "NewMigrationSessionStore" {
					found = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call wireMigration(ctx, handler, registry, disks, disk.KernelReadOnlyMounter{...}, store.NewMigrationSessionStore(db), absStateDir)")
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
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

const wiredStickDevice = "/dev/sdz"

// wireStick wires the migrator with a stick at wiredStickDevice whose mount
// holds a Flash Backup's files, plus a second UNRAID-labelled FAT disk that is
// in the harness's array.
func wireStick(t *testing.T, w *containersWiringHarness, version string) *disk.FakeReadOnlyMounter {
	t.Helper()
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdq", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40})
	disks.AddDisk(wiredStickDevice, disk.Disk{Size: 16 << 30, Model: "Flash Drive", Filesystem: "vfat", Label: "UNRAID", FSUUID: "ABCD-1234"})
	disks.AddDisk("/dev/sdb", disk.Disk{Filesystem: "vfat", Label: "UNRAID", FSUUID: "1111-2222"})
	mounter := disk.NewFakeReadOnlyMounter()
	data := flashBackupZip(t, version)
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
	w.handler.ArrayStore = w.arrays
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, mounter, store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	return mounter
}

type migrationView struct {
	Phase        string `json:"phase"`
	ScanError    string `json:"scanError"`
	SourceSize   *int64 `json:"sourceSize"`
	SourceDevice string `json:"sourceDevice"`
	ZipOnly      bool   `json:"zipOnly"`
	FlashDevices []struct {
		Device string `json:"device"`
		Size   int64  `json:"size"`
		Model  string `json:"model"`
	} `json:"flashDevices"`
	Report *struct {
		UnraidVersion string `json:"unraidVersion"`
	} `json:"report"`
}

func (w *containersWiringHarness) migration(t *testing.T) migrationView {
	t.Helper()
	status, body := w.do(t, http.MethodGet, "/migrate")
	var v migrationView
	if err := json.Unmarshal(body, &v); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	return v
}

// The stick is reachable through the daemon's server and its job registry: it
// is offered by getMigration, a scan of it runs as a migration_scan job, and the
// stick is mounted read-only and unmounted again for both reads.
func TestMigrationWiring_StickScanIsReachableOverHTTP(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounter := wireStick(t, w, "7.3.2")

	v := w.migration(t)
	if len(v.FlashDevices) != 1 || v.FlashDevices[0].Device != wiredStickDevice || v.FlashDevices[0].Model != "Flash Drive" || v.ZipOnly {
		t.Fatalf("GET /migrate offers %+v (zipOnly %v), want only %s: /dev/sdb is in the array", v.FlashDevices, v.ZipOnly, wiredStickDevice)
	}

	for _, device := range []string{"/dev/sdb", "/dev/sda", "/dev/nope", ""} {
		status, body := w.doBody(t, http.MethodPost, "/migrate/scan/device", `{"device":"`+device+`"}`)
		if status != http.StatusBadRequest || !bytes.Contains(body, []byte("invalid_flash_device")) {
			t.Errorf("POST /migrate/scan/device %q = %d %s, want 400 invalid_flash_device", device, status, body)
		}
	}
	if len(mounter.Mounts) != 0 {
		t.Fatalf("a refused device was mounted: %v", mounter.Mounts)
	}

	status, body := w.doBody(t, http.MethodPost, "/migrate/scan/device", `{"device":"`+wiredStickDevice+`"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan/device = %d %s", status, body)
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

	v = w.migration(t)
	if v.Phase != "scanned" || v.SourceDevice != wiredStickDevice || v.SourceSize != nil || v.Report == nil || v.Report.UnraidVersion != "7.3.2" {
		t.Fatalf("GET /migrate = %+v, want a scanned session sourced from %s", v, wiredStickDevice)
	}
	if len(mounter.Mounts) != 2 {
		t.Fatalf("mounted %v, want once to inspect and once for the job", mounter.Mounts)
	}
	for _, m := range mounter.Mounts {
		if m.FSType != "vfat" || m.UUID != "ABCD-1234" || m.Where != filepath.Join(w.root, "migrate", "stick") {
			t.Errorf("mount = %+v", m)
		}
	}
	if got := mounter.MountedPaths(); len(got) != 0 {
		t.Fatalf("the stick is still mounted at %v", got)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate/report"); status != http.StatusOK || !strings.HasPrefix(string(body), "# Hoserva migration scan report") {
		t.Fatalf("GET /migrate/report = %d %s", status, body)
	}

	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent {
		t.Fatalf("DELETE /migrate = %d %s", status, body)
	}
	if v := w.migration(t); v.Phase != "none" || v.SourceDevice != "" {
		t.Fatalf("after DELETE /migrate: %+v", v)
	}
}

// The one operation that offers a device also names what it cannot do.
func TestMigrationWiring_StickIsRefusedWhenTheZipIsTheOnlySource(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounter := wireStick(t, w, "7.3.2")
	capture := `{"boot":{"mode":"internal","filesystem":"zfs","devices":[]}}`
	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{"config/hoserva/capture.json": capture})
	if status, body := w.uploadScan(t, zipData, false); status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	} else {
		var queued struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &queued)
		if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
			t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
		}
	}

	if v := w.migration(t); !v.ZipOnly || len(v.FlashDevices) != 0 {
		t.Fatalf("GET /migrate = zipOnly %v, devices %+v; want the zip as the only source", v.ZipOnly, v.FlashDevices)
	}
	status, body := w.doBody(t, http.MethodPost, "/migrate/scan/device", `{"device":"`+wiredStickDevice+`"}`)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("zip_only_source")) {
		t.Fatalf("POST /migrate/scan/device = %d %s, want 409 zip_only_source", status, body)
	}
	if len(mounter.Mounts) != 0 {
		t.Fatalf("mounted %v although the zip is the only source", mounter.Mounts)
	}
}

func TestMigrationWiring_StickOperationAnswers501WhenNothingIsWired(t *testing.T) {
	w := newContainersWiringHarness(t)
	if status, _ := w.doBody(t, http.MethodPost, "/migrate/scan/device", `{"device":"/dev/sdz"}`); status != http.StatusNotImplemented {
		t.Errorf("POST /migrate/scan/device without wiring = %d, want 501", status)
	}
}

// A mount a previous process left at the private mountpoint is released when
// the daemon wires the migrator.
func TestMigrationWiring_AStickMountLeftByThePreviousProcessIsReleasedAtStart(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounter := disk.NewFakeReadOnlyMounter()
	where := filepath.Join(w.root, "migrate", "stick")
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	mounter.SetMounted(where)
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), mounter, store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if got := mounter.MountedPaths(); len(got) != 0 {
		t.Fatalf("still mounted after wiring: %v", got)
	}
}
