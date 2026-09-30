package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	ht "github.com/ogen-go/ogen/http"
	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// wiredInstall is an installation built the way main.go builds it for config
// export and import: its own database, machine key file, backup recipient,
// generator, share service and UPS service, wireBackup, wireConfigImport with
// the real array-files hook, and a fake disk inventory.
type wiredInstall struct {
	root, etc, stateDir, dbPath, keyPath string

	db          *sql.DB
	authStore   *api.AuthStore
	authService *api.AuthService
	recipient   *backup.Recipient
	handler     *api.Handler
	arrays      *store.ArrayStore
	shares      *store.ShareStore
	scheduler   *job.Scheduler
	disks       *disk.FakeProvider
	hooks       []string
}

func newWiredInstall(t *testing.T) *wiredInstall {
	t.Helper()
	ctx := context.Background()
	w := &wiredInstall{root: t.TempDir(), disks: disk.NewFakeProvider()}
	w.etc = filepath.Join(w.root, "etc")
	w.stateDir = filepath.Join(w.root, "state")
	w.dbPath = filepath.Join(w.stateDir, "hoserva.db")
	w.keyPath = filepath.Join(w.stateDir, "secret.key")
	configRoot := filepath.Join(w.etc, "hoserva")
	for _, d := range []string{configRoot, filepath.Join(w.etc, "nut"), w.stateDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(w.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w.db = db
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	w.authStore = api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, w.keyPath, w.authStore)
	if err != nil {
		t.Fatal(err)
	}
	w.authService = api.NewAuthService(w.authStore, machineKey)
	w.recipient, err = backup.LoadOrGenerateRecipient(ctx, machineKey, api.NewBackupRecipientStore(db), time.Now)
	if err != nil {
		t.Fatal(err)
	}

	w.arrays, w.shares = store.NewArrayStore(db), store.NewShareStore(db)
	generator := cfggen.NewGenerator(w.etc)
	generator.LookupGroup = func(string) (int, error) { return os.Getgid(), nil }
	shareService := newShareService(w.shares, w.arrays, generator, nil, nil)
	jobStore := job.NewStore(db)
	w.scheduler = job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	w.handler = &api.Handler{
		Scheduler: w.scheduler,
		Store:     jobStore,
		Disks:     w.disks,
		UPS:       api.NewUPSService(api.NewUPSStore(db), machineKey, generator, &recordingNUTReloader{}, nil),
	}
	wireBackup(w.handler, &backup.Service{
		DB: db,
		Paths: backup.Paths{
			DBPath:       w.dbPath,
			ConfigRoot:   configRoot,
			TemplatesDir: filepath.Join(w.stateDir, "templates"),
			StacksDir:    filepath.Join(w.stateDir, "stacks"),
		},
		Recipient: w.recipient,
		Destinations: []backup.Destination{{ID: "boot", Name: "Boot device", Path: filepath.Join(w.root, "backups"), Enabled: true,
			Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}},
	})
	arrayFiles := regenerateArrayFiles(w.arrays, w.shares, generator)
	wireConfigImport(w.handler, func(ctx context.Context) error {
		w.hooks = append(w.hooks, "config")
		return shareService.ApplyTopology(ctx, false)
	}, func(ctx context.Context) error {
		w.hooks = append(w.hooks, "array")
		return arrayFiles(ctx)
	})
	return w
}

func (w *wiredInstall) attach(dev, uuid, wwn string) {
	w.disks.AddDisk(dev, disk.Disk{WWN: wwn, Serial: "ser-" + uuid, FSUUID: uuid, Filesystem: "xfs", Size: 8 << 40})
}

// newSourceWiredInstall is another installation, with a running array, a
// share and an admin, and the archive it exported.
func newSourceWiredInstall(t *testing.T) (*wiredInstall, []byte) {
	t.Helper()
	ctx := context.Background()
	src := newWiredInstall(t)
	if err := src.arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p1", WWN: "wwn-p1", Serial: "ser-uuid-p1", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "ser-uuid-d1", Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "ser-uuid-d2", Mountpoint: "/mnt/disk2"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := src.shares.Insert(ctx, store.Share{Name: "media", CacheMode: "array-only", CreatePolicy: "mfs",
		SMBEnabled: true, SMBBrowseable: true, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.authService.CreateFirstAdmin(ctx, "alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(src.etc, "hoserva", "smb.custom.conf")
	if err := os.WriteFile(custom, []byte("archived custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src, exportArchive(t, src.handler)
}

type wireClient struct {
	t    *testing.T
	unix *http.Client
	tcp  http.Handler
}

const wireBase = "http://unix" + apiPathPrefix

// serve starts both listeners' servers for the install's handler, built by
// the same functions main.go uses: the Unix socket over a real socket, the
// TCP server called through its handler.
func (w *wiredInstall) serve(t *testing.T) *wireClient {
	t.Helper()
	unixServer, err := buildUnixServer(w.handler, w.authStore, job.NewHub(), notify.NewHub())
	if err != nil {
		t.Fatal(err)
	}
	tcpServer, err := buildTCPServer(w.handler, w.authStore, w.authService, job.NewHub(), notify.NewHub(), fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("spa")}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(w.root, "hoserva.sock")
	ln, err := setupUnixListener(sock)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = unixServer.Serve(ln) }()
	t.Cleanup(func() { _ = unixServer.Close() })
	return &wireClient{t: t, tcp: tcpServer.Handler, unix: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}}
}

type testUnixSecurity struct{}

func (testUnixSecurity) ApiToken(context.Context, apiv1.OperationName) (apiv1.ApiToken, error) {
	return apiv1.ApiToken{Token: strings.TrimPrefix(api.UnixSocketCredentialValue, "Bearer ")}, nil
}

func (testUnixSecurity) SessionCookie(context.Context, apiv1.OperationName) (apiv1.SessionCookie, error) {
	return apiv1.SessionCookie{}, ogenerrors.ErrSkipClientSecurity
}

// generated is the client the CLI builds: the generated one, over the socket.
func (c *wireClient) generated() *apiv1.Client {
	c.t.Helper()
	client, err := apiv1.NewClient("http://unix"+apiPathPrefix, testUnixSecurity{}, apiv1.WithClient(c.unix))
	if err != nil {
		c.t.Fatal(err)
	}
	return client
}

func importForm(t *testing.T, archive []byte, confirm bool, mappingJSON string) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("archive", "hoserva-config.tar.zst")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(archive)
	if confirm {
		if err := mw.WriteField("confirm", "true"); err != nil {
			t.Fatal(err)
		}
	}
	if mappingJSON != "" {
		if err := mw.WriteField("diskMapping", mappingJSON); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, mw.FormDataContentType()
}

func (c *wireClient) viaUnix(path string, body io.Reader, contentType string) (int, []byte) {
	c.t.Helper()
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, wireBase+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.unix.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s over the socket: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (c *wireClient) viaTCP(path string, body io.Reader, contentType string) (int, []byte) {
	c.t.Helper()
	req := httptest.NewRequest(http.MethodPost, apiPathPrefix+path, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	c.tcp.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// TestBareMetalRestore_ThroughTheDaemonsSocketRebuildsTheSystemAndTheDaemonStartsAgain
// is the safety-critical reachability test: on a box with no admin and no
// array, both import operations are closed on the TCP listener and every
// other operation is closed on both; over the Unix socket the preview shows
// the mapping, the import without it is refused, and the import with it
// restores another installation's archive, regenerates the array's files and
// the configs, and leaves a database the daemon starts on again with this
// box's own machine key and recipient.
func TestBareMetalRestore_ThroughTheDaemonsSocketRebuildsTheSystemAndTheDaemonStartsAgain(t *testing.T) {
	ctx := context.Background()
	src, archive := newSourceWiredInstall(t)
	box := newWiredInstall(t)
	box.attach("/dev/sdx", "uuid-p1", "wwn-p1")
	box.attach("/dev/sdy", "uuid-d1", "wwn-d1")
	box.attach("/dev/sdz", "uuid-d2", "wwn-d2")
	boxRecipient := box.recipient.Public
	srv := box.serve(t)

	// TCP: closed to both import operations and to everything else.
	for _, path := range []string{"/config/import", "/config/import/preview"} {
		body, ct := importForm(t, archive, true, "")
		if code, resp := srv.viaTCP(path, body, ct); code != http.StatusConflict || !strings.Contains(string(resp), "setup_required") {
			t.Errorf("TCP POST %s = %d %s, want 409 setup_required while no admin exists", path, code, resp)
		}
	}
	if code, resp := srv.viaTCP("/config/export", nil, ""); code != http.StatusConflict || !strings.Contains(string(resp), "setup_required") {
		t.Errorf("TCP POST /config/export = %d %s, want 409 setup_required", code, resp)
	}
	// The socket: every other operation is closed too.
	if code, resp := srv.viaUnix("/jobs", nil, ""); code != http.StatusConflict || !strings.Contains(string(resp), "setup_required") {
		t.Errorf("socket GET /jobs = %d %s, want 409 setup_required: only the two import operations pass the gate", code, resp)
	}

	// The preview, over the socket, shows the mapping to confirm.
	body, ct := importForm(t, archive, false, "")
	code, resp := srv.viaUnix("/config/import/preview", body, ct)
	if code != http.StatusOK {
		t.Fatalf("socket POST /config/import/preview = %d %s, want 200", code, resp)
	}
	var preview struct {
		Blockers  []json.RawMessage `json:"blockers"`
		BareMetal struct {
			Disks []struct {
				State  string `json:"state"`
				Device string `json:"device"`
			} `json:"disks"`
			DiskMapping json.RawMessage `json:"diskMapping"`
		} `json:"bareMetal"`
	}
	if err := json.Unmarshal(resp, &preview); err != nil {
		t.Fatalf("decoding the preview: %v\n%s", err, resp)
	}
	if len(preview.Blockers) != 0 || len(preview.BareMetal.Disks) != 3 {
		t.Fatalf("preview = %s, want no blockers and the archive's three disks", resp)
	}
	for _, d := range preview.BareMetal.Disks {
		if d.State != "matched" || !strings.HasPrefix(d.Device, "/dev/sd") {
			t.Fatalf("preview disk = %+v, want matched onto an attached device", d)
		}
	}

	// Without the confirmed mapping, nothing is restored.
	body, ct = importForm(t, archive, true, "")
	if code, resp := srv.viaUnix("/config/import", body, ct); code != http.StatusConflict || !strings.Contains(string(resp), "disk_mapping_required") {
		t.Fatalf("socket POST /config/import without diskMapping = %d %s, want 409 disk_mapping_required", code, resp)
	}
	if n := countRows(t, box.db, `SELECT COUNT(*) FROM shares`); n != 0 || len(box.hooks) != 0 {
		t.Fatalf("a refused import left %d shares and hooks %v", n, box.hooks)
	}

	// The same, through the generated client the CLI uses: an unset diskMapping
	// is not sent as an empty field the server cannot decode.
	client := srv.generated()
	_, err := client.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)}})
	var status *apiv1.ErrorStatusCode
	if !errors.As(err, &status) || status.StatusCode != http.StatusConflict || status.Response.Code != "disk_mapping_required" {
		t.Fatalf("generated client ImportConfig without a mapping = %v, want 409 disk_mapping_required", err)
	}

	// With the mapping the preview showed, the archive is restored.
	report, err := client.ImportConfig(ctx, &apiv1.ImportConfigReq{
		Confirm:     true,
		Archive:     ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)},
		DiskMapping: apiv1.NewOptString(string(preview.BareMetal.DiskMapping)),
	})
	if err != nil {
		t.Fatalf("generated client ImportConfig with the confirmed mapping: %v", err)
	}
	for _, n := range report.NotRestored {
		if n.Kind == apiv1.ConfigImportNotRestoredKindDisk {
			t.Errorf("the report lists a disk as not restored: %+v", n)
		}
	}
	if got := strings.Join(box.hooks, ","); got != "array,config" {
		t.Errorf("hooks ran %q, want the array's files, then the configs", got)
	}

	// The generated files describe the restored database.
	if b, _ := os.ReadFile(filepath.Join(box.etc, "samba", "smb.conf")); !strings.Contains(string(b), "[media]") {
		t.Errorf("smb.conf has no restored share:\n%s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(box.etc, "hoserva", "smb.custom.conf")); string(b) != "archived custom\n" {
		t.Errorf("smb.custom.conf = %q, want the archive's", b)
	}
	units, _ := filepath.Glob(filepath.Join(box.etc, "systemd", "system", "mnt-*.mount"))
	unitBodies := ""
	for _, u := range units {
		b, _ := os.ReadFile(u)
		unitBodies += string(b)
	}
	for _, uuid := range []string{"uuid-p1", "uuid-d1", "uuid-d2"} {
		if !strings.Contains(unitBodies, "What=/dev/disk/by-uuid/"+uuid) {
			t.Errorf("no disk mount unit binds %s; units: %v", uuid, units)
		}
	}
	if _, err := os.Stat(filepath.Join(box.etc, "snapraid.conf")); err != nil {
		t.Errorf("snapraid.conf was not regenerated: %v", err)
	}

	// The database equals the exported one, with the attached devices.
	for _, q := range []string{
		`SELECT name FROM shares ORDER BY name`,
		`SELECT username, role FROM users ORDER BY id`,
		`SELECT role, role_index, fs_uuid, mountpoint FROM array_disks ORDER BY role, role_index`,
	} {
		if want, got := rowsOf(t, src.db, q), rowsOf(t, box.db, q); want != got {
			t.Errorf("%s:\nexported: %s\nrestored: %s", q, want, got)
		}
	}
	if got := rowsOf(t, box.db, `SELECT role, role_index, device FROM array_disks ORDER BY role, role_index`); got != "data|1|/dev/sdy;data|2|/dev/sdz;parity|1|/dev/sdx" {
		t.Errorf("devices = %s, want the attached disks'", got)
	}

	// The daemon starts again on the restored database: its own machine key
	// is accepted, its own recipient still verifies, and the archive's admin
	// is the only account.
	if err := box.db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := sql.Open("sqlite", store.DSN(box.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db2.Close() }()
	if err := applyMigrations(ctx, db2, box.stateDir); err != nil {
		t.Fatalf("applying migrations to the restored database: %v", err)
	}
	authStore2 := api.NewAuthStore(db2)
	key2, err := auth.LoadOrGenerateMachineKey(ctx, box.keyPath, authStore2)
	if err != nil {
		t.Fatalf("the daemon does not start on the restored database: this box's own machine key was refused: %v", err)
	}
	recipient2, err := backup.LoadOrGenerateRecipient(ctx, key2, api.NewBackupRecipientStore(db2), time.Now)
	if err != nil {
		t.Fatalf("the daemon does not start on the restored database: the recipient check failed: %v", err)
	}
	if recipient2.Public != boxRecipient {
		t.Errorf("recipient after the restore = %s, want this box's own %s", recipient2.Public, boxRecipient)
	}
	if n, err := authStore2.CountAdmins(ctx); err != nil || n != 1 {
		t.Errorf("admins after the restore = %d (%v), want the archive's one", n, err)
	}
	// The array sequence the daemon builds from it finds every disk present.
	arrays2, shares2 := store.NewArrayStore(db2), store.NewShareStore(db2)
	scheduler2 := job.NewScheduler(job.NewStore(db2), job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	seq, err := newArraySequence(ctx, scheduler2, arrays2, shares2, box.disks, disk.NewFakeRunner(), nil)
	if err != nil || seq == nil || !seq.Gate.Ready() {
		t.Fatalf("array sequence on the restored database = %v (err %v), want the storage gate ready with every matched disk attached", seq, err)
	}
}

// A disk the restore did not match stays unmounted: the storage gate reports
// the array degraded, and it is the gate that mounts nothing until the user
// acknowledges it or the replace flow adopts a disk.
func TestBareMetalRestore_AnAbsentDiskLeavesTheStorageGateClosed(t *testing.T) {
	ctx := context.Background()
	_, archive := newSourceWiredInstall(t)
	box := newWiredInstall(t)
	box.attach("/dev/sdy", "uuid-d1", "wwn-d1")
	box.attach("/dev/sdz", "uuid-d2", "wwn-d2")
	srv := box.serve(t)

	body, ct := importForm(t, archive, false, "")
	code, resp := srv.viaUnix("/config/import/preview", body, ct)
	if code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, resp)
	}
	var preview struct {
		BareMetal struct {
			DiskMapping json.RawMessage `json:"diskMapping"`
		} `json:"bareMetal"`
	}
	if err := json.Unmarshal(resp, &preview); err != nil {
		t.Fatal(err)
	}
	report, err := srv.generated().ImportConfig(ctx, &apiv1.ImportConfigReq{
		Confirm:     true,
		Archive:     ht.MultipartFile{Name: "a.tar.zst", File: bytes.NewReader(archive)},
		DiskMapping: apiv1.NewOptString(string(preview.BareMetal.DiskMapping)),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	named := false
	for _, n := range report.NotRestored {
		named = named || (n.Kind == apiv1.ConfigImportNotRestoredKindDisk && n.Reason == apiv1.ConfigImportNotRestoredReasonDiskAbsent && strings.Contains(n.Name, "parity disk 1"))
	}
	if !named {
		t.Fatalf("report = %+v, want the absent parity disk reported by name", report.NotRestored)
	}

	seq, err := newArraySequence(ctx, box.scheduler, box.arrays, box.shares, box.disks, disk.NewFakeRunner(), nil)
	if err != nil || seq == nil {
		t.Fatalf("array sequence = %v (err %v)", seq, err)
	}
	if seq.Gate.Ready() {
		t.Fatal("the storage gate is ready with the parity disk absent: the array would mount without it")
	}
	gate, ok := storageGateOf(seq.Gate)
	if !ok {
		t.Fatal("the array sequence has no storage gate")
	}
	if missing := gate.Missing(); len(missing) != 1 || missing[0].Role != store.ArrayRoleParity {
		t.Fatalf("the gate's missing disks = %+v, want exactly the parity disk", missing)
	}
	if got := rowsOf(t, box.db, `SELECT role, role_index, fs_uuid FROM array_disks WHERE role = 'parity'`); got != "parity|1|uuid-p1" {
		t.Errorf("the absent disk's row = %s, want the recorded one kept for the replace flow", got)
	}
}

func countRows(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rowsOf renders a query's rows as "a|b;c|d".
func rowsOf(t *testing.T, db *sql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, v := range vals {
			switch x := v.(type) {
			case []byte:
				parts = append(parts, string(x))
			default:
				parts = append(parts, strings.TrimSpace(strings.Join(strings.Fields(toString(x)), " ")))
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return strings.Join(out, ";")
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}
