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
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

func hostConfigTestHandler(t *testing.T) (*api.Handler, *config.Generator) {
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
	h := &api.Handler{
		Generator:  g,
		HostConfig: store.NewHostConfigStore(db),
		Docker: config.MemoryDocker{
			Containers: []config.DockerRef{{ID: "c1", Name: "jellyfin"}},
			Images:     []config.DockerRef{{ID: "i1", Name: "nginx:latest"}},
		},
	}
	return h, g
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
	h.Docker = config.MemoryDocker{}
	_, err := h.ApplyHostConfig(context.Background(), &apiv1.ApplyHostConfigRequest{Files: []apiv1.HostConfigChoice{
		{ID: apiv1.HostConfigIDHostDockerContainers, Decision: apiv1.HostConfigDecisionLeave},
	}})
	status := apiError(t, h, err)
	if status.StatusCode != 400 || status.Response.Code != "invalid_host_config" {
		t.Fatalf("error = %+v", status)
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
