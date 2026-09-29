package main

import (
	"bytes"
	"context"
	"database/sql"
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
	"github.com/mdg-labs/hoserva/internal/job"
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
		Destinations: []backup.Destination{{ID: "boot", Name: "Boot device", Path: filepath.Join(root, "backups"), Enabled: true,
			Retention: backup.Retention{Daily: 7, Weekly: 4, Monthly: 6}}},
	})
	topologyRuns := 0
	wireConfigImport(handler, func(ctx context.Context) error {
		topologyRuns++
		return shareService.ApplyTopology(ctx, false)
	})

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
	archive := exportArchive(t, handler)

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

	if err := handler.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader(archive)}}); err != nil {
		t.Fatalf("ImportConfig: %v", err)
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
	err := handler.ImportConfig(ctx, &apiv1.ImportConfigReq{Confirm: true, Archive: ht.MultipartFile{File: bytes.NewReader([]byte("x"))}})
	if got := handler.NewError(ctx, err); got.StatusCode != 501 || got.Response.Code != "not_configured" {
		t.Fatalf("ImportConfig = %+v, want 501 not_configured", got)
	}

	wireConfigImport(handler, func(context.Context) error { return nil })
	if handler.RegenerateConfig == nil {
		t.Fatal("wireConfigImport left RegenerateConfig unset")
	}
}

// The hook reports each part's failure and runs the other part regardless.
func TestWireConfigImport_ReportsEveryFailureAndRunsBothParts(t *testing.T) {
	handler := &api.Handler{}
	ran := false
	topologyErr := errors.New("share files not written")
	wireConfigImport(handler, func(context.Context) error { ran = true; return topologyErr })
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
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "wireConfigImport" && len(call.Args) == 2 {
			if sel, ok := call.Args[1].(*ast.SelectorExpr); ok && sel.Sel.Name == "callArrayReady" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Fatal("main.go does not call wireConfigImport(handler, parityReg.callArrayReady)")
	}
}
