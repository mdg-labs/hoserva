package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// wiringTestMounter fakes share.Mounter — never touches /mnt (CLAUDE.md)
// — standing in for the real pool.Mounter{Runner: linuxDisks.Exec} that
// newShareService wires in production. This test is about the HTTP path
// from main.go's Handler to share.Service, not mergerfs itself, which is
// already covered elsewhere (internal/pool's own tests, and for real,
// inside the loop-device lab).
type wiringTestMounter struct{}

func (wiringTestMounter) Mount(context.Context, pool.Mount) error { return nil }
func (wiringTestMounter) Unmount(context.Context, string) error   { return nil }

// TestSharesWiring_RealHandlerServesSharesOverHTTP reproduces #261: before
// this fix, the `handler := &api.Handler{...}` literal in main.go's run()
// never set Shares, so every /shares* operation returned 501
// "not_configured" regardless of array state. This builds the handler
// through the same newShareService main.go itself calls — real
// SQLite-backed ArrayStore/ShareStore, buildUnixServer's own generated
// API server and TrustedSecurityHandler — and drives real HTTP requests
// over a real Unix socket, proving createShare, listShares and
// deleteShareData all reach share.Service instead of 501ing.
func TestSharesWiring_RealHandlerServesSharesOverHTTP(t *testing.T) {
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
	dataDisks := []string{filepath.Join(root, "disk1"), filepath.Join(root, "disk2")}
	if err := arrayStore.PutArray(ctx, store.ArraySettings{
		CreatePolicy: "mfs",
		MinFreeSpace: "50G",
		CreatedAt:    time.Now().UTC(),
	}, []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p", WWN: "wwn-p", Serial: "PARITY1", ByIDName: "wwn-wwn-p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "uuid-d1", WWN: "wwn-d1", Serial: "DATA1", ByIDName: "wwn-wwn-d1", Mountpoint: dataDisks[0]},
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "uuid-d2", WWN: "wwn-d2", Serial: "DATA2", ByIDName: "wwn-wwn-d2", Mountpoint: dataDisks[1]},
	}); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	shareStore := store.NewShareStore(db)
	generator := cfggen.NewGenerator(filepath.Join(root, "etc"))
	shareService := newShareService(shareStore, arrayStore, generator, wiringTestMounter{}, nil)

	// Exactly the handler field this issue's fix adds — every other field
	// is nil, since only /shares* is under test here.
	handler := &api.Handler{Shares: shareService}

	hub := job.NewHub()
	notifyHub := notify.NewHub()
	unixServer, err := buildUnixServer(handler, authStore, hub, notifyHub)
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
	const base = "http://unix" + apiPathPrefix

	createBody, err := json.Marshal(map[string]string{"name": "wiretest", "cacheMode": "array-only"})
	if err != nil {
		t.Fatalf("marshaling create request: %v", err)
	}
	resp, err := client.Post(base+"/shares", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST /shares: %v", err)
	}
	createRespBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		t.Fatalf("POST /shares returned 501 — Handler.Shares is nil (the bug this issue fixes): %s", createRespBody)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /shares status = %d, want 200: %s", resp.StatusCode, createRespBody)
	}

	resp, err = client.Get(base + "/shares")
	if err != nil {
		t.Fatalf("GET /shares: %v", err)
	}
	listBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		t.Fatalf("GET /shares returned 501 — Handler.Shares is nil: %s", listBody)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /shares status = %d, want 200: %s", resp.StatusCode, listBody)
	}
	if !bytes.Contains(listBody, []byte("wiretest")) {
		t.Fatalf("GET /shares body missing the created share: %s", listBody)
	}

	deleteBody, err := json.Marshal(map[string]string{"confirmation": "wiretest"})
	if err != nil {
		t.Fatalf("marshaling delete-data request: %v", err)
	}
	resp, err = client.Post(base+"/shares/wiretest/data/delete", "application/json", bytes.NewReader(deleteBody))
	if err != nil {
		t.Fatalf("POST /shares/wiretest/data/delete: %v", err)
	}
	deleteRespBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotImplemented {
		t.Fatalf("POST /shares/wiretest/data/delete returned 501 — Handler.Shares is nil: %s", deleteRespBody)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /shares/wiretest/data/delete status = %d, want 204: %s", resp.StatusCode, deleteRespBody)
	}
}
