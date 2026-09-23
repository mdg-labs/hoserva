package api_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func hostConfigTestHandler(t *testing.T) (*api.Handler, *config.Generator) {
	t.Helper()
	h, g, _ := hostConfigTestEnv(t)
	return h, g
}

func hostConfigTestEnv(t *testing.T) (*api.Handler, *config.Generator, *sql.DB) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "hostconfig.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "samba"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyHostFixture(t, "host_smb.conf", filepath.Join(root, config.PathSamba))
	copyHostFixture(t, "host_exports", filepath.Join(root, config.PathNFS))
	copyHostFixture(t, "host_fstab", filepath.Join(root, config.PathFstab))

	g := config.NewGenerator(root)
	shareStore := store.NewShareStore(db)
	h := &api.Handler{
		Generator:  g,
		HostConfig: store.NewHostConfigStore(db),
		Shares: &share.Service{
			Shares: shareStore,
			Gen:    g,
			Now:    func() time.Time { return time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC) },
		},
		Docker: config.MemoryDocker{
			Containers: []config.DockerRef{{ID: "c1", Name: "jellyfin"}},
			Images:     []config.DockerRef{{ID: "i1", Name: "nginx:latest"}},
		},
	}
	return h, g, db
}

func copyHostFixture(t *testing.T, name, dst string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "parsers", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func findCheck(report *apiv1.DoctorReport, id string) apiv1.DoctorCheck {
	for _, c := range report.Checks {
		if c.ID == id {
			return c
		}
	}
	return apiv1.DoctorCheck{}
}

func TestRunDoctor_ReportsHostConfigFromTempRoot(t *testing.T) {
	h, _ := hostConfigTestHandler(t)
	report, err := h.RunDoctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	samba := findCheck(report, "host_samba")
	if samba.ID != "host_samba" || !strings.Contains(samba.Message, "media") {
		t.Fatalf("host_samba = %+v", samba)
	}
	if findCheck(report, "host_nfs").ID != "host_nfs" {
		t.Fatal("missing host_nfs")
	}
	if findCheck(report, "host_fstab").ID != "host_fstab" {
		t.Fatal("missing host_fstab")
	}
	if findCheck(report, "host_docker_containers").ID != "host_docker_containers" {
		t.Fatal("missing host_docker_containers")
	}
	if findCheck(report, "host_docker_images").ID != "host_docker_images" {
		t.Fatal("missing host_docker_images")
	}
}

func TestApplyHostConfig_LeaveDoesNotOverwrite(t *testing.T) {
	h, g := hostConfigTestHandler(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}

	got, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionLeave},
		{ID: apiv1.HostConfigIDHostNfs, Decision: apiv1.HostConfigDecisionLeave},
		{ID: apiv1.HostConfigIDHostFstab, Decision: apiv1.HostConfigDecisionLeave},
	}})
	if err != nil {
		t.Fatalf("ApplyHostConfig: %v", err)
	}
	if got.DockerDataRoot != config.DockerDataRootDefault {
		t.Fatalf("dockerDataRoot = %q", got.DockerDataRoot)
	}

	after, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("leave-unmanaged changed smb.conf")
	}

	writeErr := g.Write(ctx, config.File{Path: config.PathSamba, Command: "share create", Body: []byte("[global]\n")}, 1, time.Now())
	if !errors.Is(writeErr, config.ErrUnmanaged) {
		t.Fatalf("Write after leave = %v, want ErrUnmanaged", writeErr)
	}
	after, _ = os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if string(after) != string(before) {
		t.Fatal("Write after leave changed smb.conf")
	}

	stored, err := h.HostConfig.Get(ctx, config.KindSamba)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Decision != config.DecisionLeave {
		t.Fatalf("stored decision = %q", stored.Decision)
	}
}

func TestApplyHostConfig_ImportAllowsLaterGenerateAndDoesNotClobberYet(t *testing.T) {
	h, g := hostConfigTestHandler(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionImport},
	}}); err != nil {
		t.Fatalf("ApplyHostConfig: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("import itself overwrote smb.conf")
	}

	if err := g.Write(ctx, config.File{Path: config.PathSamba, Command: "share create", Body: []byte("[global]\n")}, 1, time.Now()); err != nil {
		t.Fatalf("Write after import: %v", err)
	}
}

// TestApplyHostConfig_ImportIngestsSharesAndSurvivesRegeneration is the
// Q76 regression: import must put parsed Samba/NFS entries into shares
// before flipping management mode, so the next generated write keeps
// them (D4). Without that ingest, regeneration drops every pre-existing
// section permanently.
func TestApplyHostConfig_ImportIngestsSharesAndSurvivesRegeneration(t *testing.T) {
	h, g, db := hostConfigTestEnv(t)
	ctx := context.Background()
	beforeSamba, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}
	beforeNFS, err := os.ReadFile(filepath.Join(g.Root, config.PathNFS))
	if err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(g.Root, config.PathSambaCustom)
	if err := os.MkdirAll(filepath.Dir(customPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPath, []byte("# keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionImport},
		{ID: apiv1.HostConfigIDHostNfs, Decision: apiv1.HostConfigDecisionImport},
	}}); err != nil {
		t.Fatalf("ApplyHostConfig: %v", err)
	}

	afterSamba, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}
	afterNFS, err := os.ReadFile(filepath.Join(g.Root, config.PathNFS))
	if err != nil {
		t.Fatal(err)
	}
	if string(afterSamba) != string(beforeSamba) {
		t.Fatal("import itself overwrote smb.conf")
	}
	if string(afterNFS) != string(beforeNFS) {
		t.Fatal("import itself overwrote exports")
	}
	customAfter, err := os.ReadFile(customPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(customAfter) != "# keep me\n" {
		t.Fatalf("import clobbered existing smb.custom.conf: %q", customAfter)
	}

	shareStore := store.NewShareStore(db)
	media, err := shareStore.Get(ctx, "media")
	if err != nil {
		t.Fatalf("media row: %v", err)
	}
	if !media.SMBEnabled || media.SMBReadOnly || !media.SMBBrowseable {
		t.Fatalf("media SMB = %+v", media)
	}
	if !media.NFSEnabled || len(media.NFSHosts) != 1 || media.NFSHosts[0] != "192.168.1.0/24" {
		t.Fatalf("media NFS = %+v", media)
	}
	homes, err := shareStore.Get(ctx, "homes")
	if err != nil {
		t.Fatalf("homes row: %v", err)
	}
	if !homes.SMBEnabled || homes.SMBBrowseable || homes.NFSEnabled {
		t.Fatalf("homes = %+v", homes)
	}
	backup, err := shareStore.Get(ctx, "backup")
	if err != nil {
		t.Fatalf("backup row: %v", err)
	}
	if !backup.NFSEnabled || backup.SMBEnabled {
		t.Fatalf("backup = %+v", backup)
	}
	if len(backup.NFSHosts) != 1 || backup.NFSHosts[0] != "*" || backup.NFSSquash != "root_squash" {
		t.Fatalf("backup NFS = %+v", backup)
	}

	rows, err := shareStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var smb []config.SambaShare
	var nfs []config.NFSShare
	for _, row := range rows {
		if row.SMBEnabled {
			smb = append(smb, config.SambaShare{
				Name:       row.Name,
				Guest:      row.SMBGuest,
				ReadOnly:   row.SMBReadOnly,
				Browseable: row.SMBBrowseable,
			})
		}
		if row.NFSEnabled {
			nfs = append(nfs, config.NFSShare{
				Name:   row.Name,
				Hosts:  append([]string(nil), row.NFSHosts...),
				Squash: row.NFSSquash,
			})
		}
	}
	now := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	if err := g.WriteSamba(ctx, smb, "share create", 1, now); err != nil {
		t.Fatalf("WriteSamba: %v", err)
	}
	if err := g.WriteNFS(ctx, nfs, "share create", 1, now); err != nil {
		t.Fatalf("WriteNFS: %v", err)
	}

	regenSamba, err := os.ReadFile(filepath.Join(g.Root, config.PathSamba))
	if err != nil {
		t.Fatal(err)
	}
	regenText := string(regenSamba)
	for _, section := range []string{"[media]", "[homes]"} {
		if !strings.Contains(regenText, section) {
			t.Fatalf("regenerated smb.conf missing %s:\n%s", section, regenText)
		}
	}
	if !strings.Contains(regenText, "browseable = no") {
		t.Fatalf("regenerated smb.conf lost homes browseable=no:\n%s", regenText)
	}
	if !strings.Contains(regenText, "path = /mnt/user/media") {
		t.Fatalf("regenerated smb.conf should use D10 path:\n%s", regenText)
	}

	regenNFS, err := os.ReadFile(filepath.Join(g.Root, config.PathNFS))
	if err != nil {
		t.Fatal(err)
	}
	nfsText := string(regenNFS)
	for _, line := range []string{"/mnt/user/media", "/mnt/user/backup", "192.168.1.0/24", "*("} {
		if !strings.Contains(nfsText, line) {
			t.Fatalf("regenerated exports missing %q:\n%s", line, nfsText)
		}
	}
}

func TestApplyHostConfig_SambaImportCreatesEmptyCustomConf(t *testing.T) {
	h, g := hostConfigTestHandler(t)
	customPath := filepath.Join(g.Root, config.PathSambaCustom)
	if _, err := os.Stat(customPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("custom conf should be absent before import: %v", err)
	}
	if _, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionImport},
	}}); err != nil {
		t.Fatalf("ApplyHostConfig: %v", err)
	}
	info, err := os.Stat(customPath)
	if err != nil {
		t.Fatalf("smb.custom.conf missing after samba import: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("new smb.custom.conf size = %d, want 0", info.Size())
	}
}

func TestApplyHostConfig_ImportSkipsDuplicateShareNames(t *testing.T) {
	h, _, db := hostConfigTestEnv(t)
	ctx := context.Background()
	shareStore := store.NewShareStore(db)
	now := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	if err := shareStore.Insert(ctx, store.Share{
		Name: "media", CacheMode: "array-only", CreatePolicy: "mspmfs",
		SMBEnabled: true, CreatedAt: now, UpdatedAt: now, NFSSquash: "root_squash",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionImport},
		{ID: apiv1.HostConfigIDHostNfs, Decision: apiv1.HostConfigDecisionImport},
	}}); err != nil {
		t.Fatalf("ApplyHostConfig: %v", err)
	}
	media, err := shareStore.Get(ctx, "media")
	if err != nil {
		t.Fatal(err)
	}
	if media.NFSEnabled {
		t.Fatal("duplicate media should have been skipped, not merged with NFS import")
	}
	if _, err := shareStore.Get(ctx, "homes"); err != nil {
		t.Fatalf("homes should still be imported: %v", err)
	}
	if _, err := shareStore.Get(ctx, "backup"); err != nil {
		t.Fatalf("backup should still be imported: %v", err)
	}
}

func TestApplyHostConfig_DockerDataRootStaysWithExistingContainers(t *testing.T) {
	h, _ := hostConfigTestHandler(t)
	got, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostDockerContainers, Decision: apiv1.HostConfigDecisionImport},
		{ID: apiv1.HostConfigIDHostDockerImages, Decision: apiv1.HostConfigDecisionImport},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.DockerDataRoot != config.DockerDataRootDefault {
		t.Fatalf("dockerDataRoot = %q, want %s when containers exist", got.DockerDataRoot, config.DockerDataRootDefault)
	}
}

func TestApplyHostConfig_RejectsUndetectedID(t *testing.T) {
	h, _ := hostConfigTestHandler(t)
	h.Docker = nil
	_, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostDockerContainers, Decision: apiv1.HostConfigDecisionLeave},
	}})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_host_config" {
		t.Fatalf("error = %+v", status)
	}
}

func TestApplyHostConfig_RejectsInvalidWithoutPersistingPrefix(t *testing.T) {
	h, _ := hostConfigTestHandler(t)
	ctx := context.Background()
	_, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionLeave},
		{ID: "host_not_a_thing", Decision: apiv1.HostConfigDecisionLeave},
	}})
	if err == nil {
		t.Fatal("invalid later choice did not error")
	}
	if _, getErr := h.HostConfig.Get(ctx, config.KindSamba); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("samba persisted after a later invalid choice: %v", getErr)
	}
}

func TestApplyHostConfig_GeneratorFailureDoesNotPersistPrefix(t *testing.T) {
	h, g := hostConfigTestHandler(t)
	ctx := context.Background()
	nfs := filepath.Join(g.Root, config.PathNFS)
	if err := os.Chmod(nfs, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(nfs, 0o644) })

	_, err := h.ApplyHostConfig(ctx, &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostSamba, Decision: apiv1.HostConfigDecisionLeave},
		{ID: apiv1.HostConfigIDHostNfs, Decision: apiv1.HostConfigDecisionLeave},
	}})
	if err == nil {
		t.Fatal("unreadable nfs did not error")
	}
	if _, getErr := h.HostConfig.Get(ctx, config.KindSamba); !errors.Is(getErr, sql.ErrNoRows) {
		t.Fatalf("samba persisted after a later generator failure: %v", getErr)
	}
	writeErr := g.Write(ctx, config.File{Path: config.PathSamba, Command: "share create", Body: []byte("[global]\n")}, 1, time.Now())
	if errors.Is(writeErr, config.ErrUnmanaged) {
		t.Fatal("samba was marked unmanaged after a later generator failure")
	}
}

func TestApplyHostConfig_EmptyDockerCanAcceptCacheMove(t *testing.T) {
	h, _, db := hostConfigTestEnv(t)
	h.Docker = config.MemoryDocker{}
	h.ArrayStore = store.NewArrayStore(db)
	if err := h.ArrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "20G",
		CreatedAt:    time.Now().UTC(),
	}, []store.ArrayDisk{{
		Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc",
		Filesystem: "ext4", FSUUID: "uuid-c", Mountpoint: "/mnt/cache",
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostDockerContainers, Decision: apiv1.HostConfigDecisionImport},
		{ID: apiv1.HostConfigIDHostDockerImages, Decision: apiv1.HostConfigDecisionImport},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.DockerDataRoot != "/mnt/cache/docker" {
		t.Fatalf("dockerDataRoot = %q, want /mnt/cache/docker", got.DockerDataRoot)
	}
}

func TestApplyHostConfig_NotConfigured(t *testing.T) {
	h := &api.Handler{}
	_, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{})
	status := apiError(t, h, err)
	if status.StatusCode != 501 {
		t.Fatalf("status = %d, want 501", status.StatusCode)
	}
}
