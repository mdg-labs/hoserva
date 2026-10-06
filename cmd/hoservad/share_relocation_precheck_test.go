package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/cache"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// TestShareRelocationPrecheck_ServedByTheDaemonsHandler builds the handler
// the way main.go does for the relocation precheck (newShareService,
// wireShareRelocationPrecheck, Container) and drives GET
// /shares/{name}/relocation-precheck over a real Unix socket: the container
// that bind-mounts the share's cache path is listed as active and the file
// the open-file fake reports comes back, so the operation is neither a 501
// nor an empty answer.
func TestShareRelocationPrecheck_ServedByTheDaemonsHandler(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(root, "hoservad.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	defer func() { _ = db.Close() }()
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(ctx); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	authStore := api.NewAuthStore(db)
	machineKey, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(root, "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	authService := api.NewAuthService(authStore, machineKey)
	if _, _, err := authService.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	arrayStore := store.NewArrayStore(db)
	cacheMount := filepath.Join(root, "cache")
	dataMount := filepath.Join(root, "disk1")
	if err := arrayStore.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now().UTC()}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: dataMount},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "uuid-c", WWN: "wwn-c", Serial: "CACHE1", ByIDName: "wwn-wwn-c", Mountpoint: cacheMount},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	shareStore := store.NewShareStore(db)
	shareService := newShareService(shareStore, arrayStore, cfggen.NewGenerator(filepath.Join(root, "etc")), wiringTestMounter{}, nil)
	if _, err := shareService.Create(ctx, share.CreateInput{Name: "docs", CacheMode: pool.CacheThenMove}); err != nil {
		t.Fatalf("creating the share: %v", err)
	}

	liveDB := filepath.Join(cacheMount, "docs", "live.db")
	if err := os.MkdirAll(filepath.Dir(liveDB), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := cache.NewFakeOpenChecker()
	open.SetOpen(liveDB, true)
	apps := container.NewFakeProvider()
	apps.AddContainer(container.Container{ID: "c1", Name: "database", State: "running", Mounts: []container.Mount{{Source: filepath.Join(cacheMount, "docs"), Destination: "/data"}}})
	apps.AddContainer(container.Container{ID: "c2", Name: "unrelated", State: "running", Mounts: []container.Mount{{Source: filepath.Join(cacheMount, "other"), Destination: "/data"}}})

	scheduler := job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), job.NewRegistry())
	handler := &api.Handler{Shares: shareService, Scheduler: scheduler, Container: apps, RelocationOpen: open}
	wireShareRelocationPrecheck(handler, shareStore, arrayStore)

	unixServer, err := buildUnixServer(handler, authStore, job.NewHub(), notify.NewHub())
	if err != nil {
		t.Fatalf("buildUnixServer: %v", err)
	}
	sockPath := filepath.Join(root, "hoserva.sock")
	ln, err := setupUnixListener(sockPath)
	if err != nil {
		t.Fatalf("setupUnixListener: %v", err)
	}
	go func() { _ = unixServer.Serve(ln) }()
	t.Cleanup(func() { _ = unixServer.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}}
	resp, err := client.Get("http://unix" + apiPathPrefix + "/shares/docs/relocation-precheck")
	if err != nil {
		t.Fatalf("GET relocation-precheck: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET relocation-precheck status = %d, want 200 (501 means the handler is not wired): %s", resp.StatusCode, body)
	}
	var got struct {
		DockerAvailable bool `json:"dockerAvailable"`
		Containers      []struct {
			Name   string `json:"name"`
			Active bool   `json:"active"`
		} `json:"containers"`
		OpenPaths []string `json:"openPaths"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	if !got.DockerAvailable || len(got.Containers) != 1 || got.Containers[0].Name != "database" || !got.Containers[0].Active {
		t.Fatalf("precheck = %s, want only the active container that mounts the share", body)
	}
	if len(got.OpenPaths) != 1 || got.OpenPaths[0] != "live.db" {
		t.Fatalf("precheck = %s, want live.db open", body)
	}

	if err := scheduler.EnterMaintenance(ctx); err != nil {
		t.Fatalf("EnterMaintenance: %v", err)
	}
	resp, err = client.Get("http://unix" + apiPathPrefix + "/shares/docs/relocation-precheck")
	if err != nil {
		t.Fatalf("GET relocation-precheck in maintenance mode: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("GET relocation-precheck in maintenance mode status = %d, want 409: %s", resp.StatusCode, body)
	}
}
