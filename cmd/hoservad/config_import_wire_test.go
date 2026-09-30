package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ht "github.com/ogen-go/ogen/http"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/backup"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type recordingNUTReloader struct{ calls []cfggen.UPSConnection }

func (r *recordingNUTReloader) Reload(_ context.Context, c cfggen.UPSConnection) error {
	r.calls = append(r.calls, c)
	return nil
}

func exportArchive(t *testing.T, h *api.Handler) []byte {
	t.Helper()
	out, err := h.ExportConfig(context.Background())
	if err != nil {
		t.Fatalf("ExportConfig: %v", err)
	}
	data, err := io.ReadAll(out.Data)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := out.Data.(io.Closer); ok {
		_ = c.Close()
	}
	return data
}

func readFileOrEmpty(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

// TestWireConfigImport_ImportRegeneratesTheConfigsTheDatabaseDescribes
// builds the handler the way main.go does for config import (wireBackup,
// wireConfigImport, the real share service and UPS service, the real
// generator) and runs a full in-place restore: after the import a share
// deleted since the export is back in generated smb.conf, the NFS
// exports and its pool mount unit, the NUT files are the archived
// settings' again, and the custom config file the archive holds is back.
// The topology hook is share.Service.ApplyTopology with live=false, which
// is what newTopologyChangedHook runs first with no pool mounted; the
// hook itself stats the real /mnt/user and, with a live pool, would create
// branch directories under /mnt, so a test never runs it.
func TestWireConfigImport_ImportRegeneratesTheConfigsTheDatabaseDescribes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "hoservad.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	authStore := api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), authStore)
	if err != nil {
		t.Fatal(err)
	}

	etc := filepath.Join(root, "etc")
	configRoot := filepath.Join(etc, "hoserva")
	for _, d := range []string{configRoot, filepath.Join(etc, "nut"), filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arrays, shares := store.NewArrayStore(db), store.NewShareStore(db)
	if err := arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", Mountpoint: "/mnt/parity1"},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	generator := cfggen.NewGenerator(etc)
	generator.LookupGroup = func(string) (int, error) { return os.Getgid(), nil }
	shareService := newShareService(shares, arrays, generator, nil, nil)
	nut := &recordingNUTReloader{}

	jobStore := job.NewStore(db)
	handler := &api.Handler{
		Scheduler: job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry()),
		Store:     jobStore,
		UPS:       api.NewUPSService(api.NewUPSStore(db), machineKey, generator, nut, nil),
	}
	wireBackup(handler, &backup.Service{
		DB: db,
		Paths: backup.Paths{
			DBPath:       dbPath,
			ConfigRoot:   configRoot,
			TemplatesDir: filepath.Join(root, "state", "templates"),
			StacksDir:    filepath.Join(root, "state", "stacks"),
		},
		Secrets: &backup.ServiceSecretSource{BackupPassphraseFn: func(context.Context) (string, bool, error) {
			return "wire passphrase", true, nil
		}},
		Destinations: []backup.Destination{{ID: "boot", Name: "Boot device", Path: filepath.Join(root, "backups"), Enabled: true,
			Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}},
	})
	topologyRuns := 0
	wireConfigImport(handler, func(ctx context.Context) error {
		topologyRuns++
		return shareService.ApplyTopology(ctx, false)
	}, regenerateArrayFiles(arrays, shares, generator))

	// The state at export time: a share with Samba and NFS, a UPS, a custom
	// config file.
	now := time.Now().UTC()
	if err := shares.Insert(ctx, store.Share{Name: "media", CacheMode: "array-only", CreatePolicy: "mfs",
		SMBEnabled: true, SMBBrowseable: true, NFSEnabled: true, NFSHosts: []string{"192.168.1.0/24"}, NFSSquash: "root_squash",
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := shareService.ApplyTopology(ctx, false); err != nil {
		t.Fatal(err)
	}
	saveUPS := func(host string) {
		t.Helper()
		if _, err := handler.UpdateUPSSettings(ctx, &apiv1.UpdateUPSSettingsRequest{
			Connection:      apiv1.UPSConnectionNetwork,
			NetworkHost:     apiv1.NewOptString(host),
			NetworkPort:     apiv1.NewOptInt32(3493),
			NetworkUpsName:  apiv1.NewOptString("ups"),
			NetworkUsername: apiv1.NewOptString("hoserva"),
			NetworkPassword: apiv1.NewOptString("secret"),
		}); err != nil {
			t.Fatalf("UpdateUPSSettings(%s): %v", host, err)
		}
	}
	saveUPS("nut-archived.lan")
	customPath := filepath.Join(configRoot, "smb.custom.conf")
	if err := os.WriteFile(customPath, []byte("archived custom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stackDir := filepath.Join(root, "state", "stacks", "web")
	if err := os.MkdirAll(stackDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(stackDir, ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=archived\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, handler)
	if err := os.WriteFile(envPath, []byte("TOKEN=edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Then: the share is deleted, the UPS changed, the custom file edited.
	if err := shares.Delete(ctx, "media"); err != nil {
		t.Fatal(err)
	}
	if err := shareService.ApplyTopology(ctx, false); err != nil {
		t.Fatal(err)
	}
	saveUPS("nut-edited.lan")
	if err := os.WriteFile(customPath, []byte("edited after export\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	smbPath, exportsPath := filepath.Join(etc, "samba", "smb.conf"), filepath.Join(etc, "exports")
	if strings.Contains(readFileOrEmpty(smbPath), "[media]") || strings.Contains(readFileOrEmpty(exportsPath), "media") {
		t.Fatal("precondition: the deleted share is still in the generated configs")
	}
	if units, _ := filepath.Glob(filepath.Join(etc, "systemd", "system", "*media*.mount")); len(units) != 0 {
		t.Fatalf("precondition: the deleted share's mount units remain: %v", units)
	}
	nut.calls = nil
	topologyRuns = 0

	report, err := handler.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if report.Secrets != apiv1.ConfigImportSecretsStatusOpened || len(report.NotRestored) != 0 {
		t.Errorf("report secrets = %s, notRestored = %+v, want the configured passphrase to have opened the archive's secrets", report.Secrets, report.NotRestored)
	}
	if got := readFileOrEmpty(envPath); got != "TOKEN=archived\n" {
		t.Errorf("the stack's .env = %q, want the archived one restored with the configured passphrase", got)
	}

	if !strings.Contains(readFileOrEmpty(smbPath), "[media]") {
		t.Errorf("smb.conf does not have the restored share:\n%s", readFileOrEmpty(smbPath))
	}
	if !strings.Contains(readFileOrEmpty(exportsPath), "media") {
		t.Errorf("the NFS exports do not have the restored share:\n%s", readFileOrEmpty(exportsPath))
	}
	if units, _ := filepath.Glob(filepath.Join(etc, "systemd", "system", "*media*.mount")); len(units) == 0 {
		t.Error("the restored share has no pool mount unit")
	}
	if got := readFileOrEmpty(filepath.Join(etc, "nut", "upsmon.conf")); !strings.Contains(got, "nut-archived.lan") || strings.Contains(got, "nut-edited.lan") {
		t.Errorf("upsmon.conf is not the archived settings':\n%s", got)
	}
	if len(nut.calls) != 1 || nut.calls[0] != cfggen.UPSConnectionNetwork {
		t.Errorf("NUT reloads = %v, want one network reload", nut.calls)
	}
	if topologyRuns != 1 {
		t.Errorf("the topology hook ran %d times, want once", topologyRuns)
	}
	if got := readFileOrEmpty(customPath); got != "archived custom\n" {
		t.Errorf("smb.custom.conf = %q, want the archived content", got)
	}
}

// Without wireConfigImport an import is refused, not silently run without
// regenerating anything.
func TestWireConfigImport_AnImportWithoutTheHookIsRefused(t *testing.T) {
	ctx := context.Background()
	handler := &api.Handler{Scheduler: job.NewScheduler(nil, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())}
	wireBackup(handler, &backup.Service{DB: &sql.DB{}})
	if handler.RegenerateConfig != nil {
		t.Fatal("wireBackup set RegenerateConfig")
	}
	_, err := handler.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader([]byte("x"))}})
	if got := handler.NewError(ctx, err); got.StatusCode != 501 || got.Response.Code != "not_configured" {
		t.Fatalf("ImportConfig = %+v, want 501 not_configured", got)
	}

	wireConfigImport(handler, func(context.Context) error { return nil }, nil)
	if handler.RegenerateConfig == nil {
		t.Fatal("wireConfigImport left RegenerateConfig unset")
	}
}

// The hook reports each part's failure and runs the other part regardless.
func TestWireConfigImport_ReportsEveryFailureAndRunsBothParts(t *testing.T) {
	handler := &api.Handler{}
	ran := false
	topologyErr := errors.New("share files not written")
	wireConfigImport(handler, func(context.Context) error { ran = true; return topologyErr }, nil)
	err := handler.RegenerateConfig(context.Background())
	if !ran || !errors.Is(err, topologyErr) || !strings.Contains(err.Error(), "UPS settings service is not configured") {
		t.Fatalf("RegenerateConfig = %v (topology ran: %v), want both failures", err, ran)
	}
}

// main.go must call wireConfigImport, with the strict topology hook: a
// handler nobody wires would refuse every import.
func TestMain_WiresConfigImportWithTheStrictTopologyHook(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "wireConfigImport" && len(call.Args) == 3 {
			if sel, ok := call.Args[1].(*ast.SelectorExpr); ok && sel.Sel.Name == "callArrayReady" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call wireConfigImport(handler, parityReg.callArrayReady, regenerateArrayFiles(...))")
	}
}

// wireBox is one installation wired the way main.go wires config import:
// its own database, machine key, generator under its own etc, the real share
// service and the real regeneration hooks. array puts a parity and a data
// disk on it, with mountpoints inside the box's own directory, so nothing a
// share write does reaches /mnt.
type wireBox struct {
	root, etc string
	db        *sql.DB
	handler   *api.Handler
	generator *cfggen.Generator
	shares    *store.ShareStore
	service   *share.Service
	arrays    *store.ArrayStore
	backups   string
}

func newWireBox(t *testing.T, array bool) *wireBox {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "hoservad.db")
	db, err := sql.Open("sqlite", store.DSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := (&store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), api.NewAuthStore(db)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO backup_recipient (id, public_recipient, wrapped_identity, check_value, created_at)
		VALUES (1, 'age1box', CAST('wrapped' AS BLOB), CAST('recipient-check' AS BLOB), '2026-09-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	b := &wireBox{root: root, etc: filepath.Join(root, "etc"), db: db, backups: filepath.Join(root, "backups")}
	configRoot := filepath.Join(b.etc, "hoserva")
	for _, d := range []string{configRoot, filepath.Join(b.etc, "samba"), filepath.Join(root, "state"), filepath.Join(root, "user")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	b.arrays, b.shares = store.NewArrayStore(db), store.NewShareStore(db)
	if array {
		disks := []store.ArrayDisk{
			{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "serial-p", Mountpoint: filepath.Join(root, "parity1")},
			{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "serial-d1", Mountpoint: filepath.Join(root, "disk1")},
			{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "serial-d2", Mountpoint: filepath.Join(root, "disk2")},
		}
		for _, d := range disks {
			if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "20G", CreatedAt: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}, disks); err != nil {
			t.Fatal(err)
		}
	}
	b.generator = cfggen.NewGenerator(b.etc)
	b.generator.LookupGroup = func(string) (int, error) { return os.Getgid(), nil }
	b.service = newShareService(b.shares, b.arrays, b.generator, nil, nil)
	b.service.CatchAll = filepath.Join(root, "user")

	jobStore := job.NewStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), api.NewAuthStore(db))
	if err != nil {
		t.Fatal(err)
	}
	b.handler = &api.Handler{
		Scheduler: job.NewScheduler(jobStore, job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry()),
		Store:     jobStore,
		UPS:       api.NewUPSService(api.NewUPSStore(db), machineKey, b.generator, &recordingNUTReloader{}, nil),
		Shares:    b.service,
		Generator: b.generator,
	}
	wireBackup(b.handler, &backup.Service{
		DB: db,
		Paths: backup.Paths{
			DBPath:       dbPath,
			ConfigRoot:   configRoot,
			TemplatesDir: filepath.Join(root, "state", "templates"),
			StacksDir:    filepath.Join(root, "state", "stacks"),
		},
		Destinations: []backup.Destination{{ID: "boot", Name: "Boot device", Path: b.backups, Enabled: true,
			Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}},
	})
	wireConfigImport(b.handler, func(ctx context.Context) error {
		return b.service.ApplyTopology(ctx, false)
	}, regenerateArrayFiles(b.arrays, b.shares, b.generator))
	return b
}

func (b *wireBox) read(rel string) string { return readFileOrEmpty(filepath.Join(b.etc, rel)) }

func (b *wireBox) put(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(b.etc, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *wireBox) decide(t *testing.T, rows ...store.HostConfig) {
	t.Helper()
	for i := range rows {
		rows[i].Facts = "{}"
	}
	if err := store.NewHostConfigStore(b.db).PutAll(context.Background(), rows); err != nil {
		t.Fatal(err)
	}
}

const (
	packageSmbConf = "[global]\n   workgroup = WORKGROUP\n   # the package default\n"
	packageExports = "# /etc/exports: the access control list for filesystems which may be exported\n"
)

// sourceArchive is an installation that chose to import its host's Samba and
// NFS configuration and then created a share; it returns its export.
func sourceArchive(t *testing.T, decisions ...store.HostConfig) []byte {
	t.Helper()
	ctx := context.Background()
	src := newWireBox(t, true)
	now := time.Now().UTC()
	if err := src.shares.Insert(ctx, store.Share{Name: "media", CacheMode: "array-only", CreatePolicy: "mfs",
		SMBEnabled: true, SMBBrowseable: true, NFSEnabled: true, NFSHosts: []string{"192.168.1.0/24"}, NFSSquash: "root_squash",
		CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := src.service.ApplyTopology(ctx, false); err != nil {
		t.Fatal(err)
	}
	src.decide(t, decisions...)
	return exportArchive(t, src.handler)
}

// attachSourceDisks gives the fresh box the source's two disks under other
// device names, and returns the mapping the preview asks to confirm.
func (b *wireBox) previewAndMap(t *testing.T, archive []byte) (*apiv1.ConfigImportPreview, apiv1.OptString) {
	t.Helper()
	fake := disk.NewFakeProvider()
	fake.AddDisk("/dev/sdx", disk.Disk{WWN: "wwn-p", Serial: "serial-p", FSUUID: "uuid-p", Filesystem: "xfs", Size: 4 << 40})
	fake.AddDisk("/dev/sdy", disk.Disk{WWN: "wwn-d1", Serial: "serial-d1", FSUUID: "uuid-d1", Filesystem: "xfs", Size: 4 << 40})
	fake.AddDisk("/dev/sdz", disk.Disk{WWN: "wwn-d2", Serial: "serial-d2", FSUUID: "uuid-d2", Filesystem: "xfs", Size: 4 << 40})
	b.handler.Disks = fake
	p, err := b.handler.PreviewConfigImport(context.Background(), &apiv1.PreviewConfigImportReq{Archive: ht.MultipartFile{File: bytes.NewReader(archive)}})
	if err != nil {
		t.Fatalf("PreviewConfigImport: %v", err)
	}
	bm, ok := p.BareMetal.Get()
	if !ok {
		t.Fatal("the preview of a box with no array has no bareMetal block")
	}
	doc, err := json.Marshal(&bm.DiskMapping)
	if err != nil {
		t.Fatal(err)
	}
	return p, apiv1.NewOptString(string(doc))
}

func (b *wireBox) restore(t *testing.T, archive []byte, mapping apiv1.OptString) (*apiv1.ConfigImportReport, error) {
	t.Helper()
	return b.handler.ImportConfig(context.Background(), &apiv1.ImportConfigReq{
		Confirm: true, DiskMapping: mapping, Archive: ht.MultipartFile{File: bytes.NewReader(archive)},
	})
}

func (b *wireBox) createShare(name string) error {
	_, err := b.handler.CreateShare(context.Background(), &apiv1.CreateShareRequest{Name: apiv1.ShareName(name), CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly)})
	return err
}

// A bare-metal restore onto a freshly installed OS, whose package-installed
// smb.conf and exports the new host's manifest has never heard of, must leave
// the box able to change its shares (#471). The restored host_config says the
// source imported both files; without the source's manifest, which is not in
// the archive, the next share write was refused with 409 unmanaged_config.
func TestWireConfigImport_BareMetalRestoreLeavesTheImportedHostFilesManaged(t *testing.T) {
	ctx := context.Background()
	archive := sourceArchive(t,
		store.HostConfig{Kind: cfggen.KindSamba, Decision: cfggen.DecisionImport},
		store.HostConfig{Kind: cfggen.KindNFS, Decision: cfggen.DecisionImport},
	)
	box := newWireBox(t, false)
	box.put(t, cfggen.PathSamba, packageSmbConf)
	box.put(t, cfggen.PathNFS, packageExports)

	_, mapping := box.previewAndMap(t, archive)
	if _, err := box.restore(t, archive, mapping); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	if !strings.Contains(box.read(cfggen.PathSamba), "[media]") {
		t.Fatalf("smb.conf does not hold the restored share:\n%s", box.read(cfggen.PathSamba))
	}
	for _, p := range []string{cfggen.PathSamba, cfggen.PathNFS} {
		if st, err := box.generator.Check(ctx, p); err != nil || st != cfggen.StatusManaged {
			t.Errorf("Check(%s) = %v, %v; want managed", p, st, err)
		}
	}

	if err := box.createShare("docs"); err != nil {
		t.Fatalf("CreateShare after the restore: %v", err)
	}
	if got := box.read(cfggen.PathSamba); !strings.Contains(got, "[docs]") || !strings.Contains(got, "[media]") {
		t.Errorf("smb.conf after the new share:\n%s", got)
	}
}
