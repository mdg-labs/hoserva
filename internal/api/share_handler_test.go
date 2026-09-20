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
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

type recordingShareMounter struct{}

func (recordingShareMounter) Mount(context.Context, pool.Mount) error { return nil }
func (recordingShareMounter) Unmount(context.Context, string) error   { return nil }

func newShareTestHandler(t *testing.T) *api.Handler {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "share-handler.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	layout := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "d1", Mountpoint: filepath.Join(root, "disk1")},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "c", Mountpoint: filepath.Join(root, "cache")},
	}
	for _, d := range layout {
		if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}, layout); err != nil {
		t.Fatal(err)
	}

	svc := &share.Service{
		Shares:   store.NewShareStore(db),
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       share.OSFS{},
		Mounter:  recordingShareMounter{},
		Now:      func() time.Time { return time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC) },
		CatchAll: filepath.Join(root, "user"),
	}
	if err := os.MkdirAll(svc.CatchAll, 0o755); err != nil {
		t.Fatal(err)
	}
	return &api.Handler{Shares: svc}
}

func TestHandler_SharesCRUDAndSeparateDeletes(t *testing.T) {
	ctx := context.Background()
	h := newShareTestHandler(t)

	created, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "media",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
		Smb: apiv1.NewOptShareSMB(apiv1.ShareSMB{
			Enabled:    true,
			Browseable: true,
			Guest:      true,
		}),
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if created.Name != "media" || created.Path != "/mnt/user/media" || !created.Smb.Guest {
		t.Fatalf("created = %+v", created)
	}

	listed, err := h.ListShares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Shares) != 1 {
		t.Fatalf("list = %+v", listed.Shares)
	}

	if err := h.DeleteShare(ctx, &apiv1.ConfirmShareRequest{Confirm: false}, apiv1.DeleteShareParams{Name: "media"}); err == nil {
		t.Fatal("delete without confirm must fail")
	}

	if err := h.DeleteShare(ctx, &apiv1.ConfirmShareRequest{Confirm: true}, apiv1.DeleteShareParams{Name: "media"}); err != nil {
		t.Fatalf("DeleteShare: %v", err)
	}
	if _, err := h.GetShare(ctx, apiv1.GetShareParams{Name: "media"}); err == nil {
		t.Fatal("definition still present")
	}
}

func TestHandler_DeleteShareDataRequiresNameConfirmation(t *testing.T) {
	ctx := context.Background()
	h := newShareTestHandler(t)
	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "media",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
	}); err != nil {
		t.Fatal(err)
	}
	err := h.DeleteShareData(ctx, &apiv1.DeleteShareDataRequest{Confirmation: "nope"}, apiv1.DeleteShareDataParams{Name: "media"})
	if err == nil {
		t.Fatal("wrong confirmation must fail")
	}
	var ae interface{ Error() string }
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
}

func TestHandler_SharesNilIs501(t *testing.T) {
	h := &api.Handler{}
	_, err := h.ListShares(context.Background())
	if err == nil {
		t.Fatal("expected not_configured")
	}
}

func TestHandler_CreateShareNFS(t *testing.T) {
	ctx := context.Background()
	h := newShareTestHandler(t)
	created, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "media",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
		Nfs: apiv1.NewOptShareNFS(apiv1.ShareNFS{
			Enabled: true,
			Hosts:   []string{"192.168.1.0/24", "10.0.0.5"},
			Squash:  apiv1.ShareNFSSquashRootSquash,
		}),
	})
	if err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	if !created.Nfs.Enabled || created.Nfs.Squash != apiv1.ShareNFSSquashRootSquash || len(created.Nfs.Hosts) != 2 {
		t.Fatalf("created NFS = %+v", created.Nfs)
	}
}

func TestHandler_InvalidNFSHostIs400(t *testing.T) {
	ctx := context.Background()
	h := newShareTestHandler(t)
	_, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{
		Name:      "media",
		CacheMode: apiv1.NewOptShareCacheMode(apiv1.ShareCacheModeArrayOnly),
		Nfs: apiv1.NewOptShareNFS(apiv1.ShareNFS{
			Enabled: true,
			Hosts:   []string{"not a host"},
			Squash:  apiv1.ShareNFSSquashRootSquash,
		}),
	})
	status := h.NewError(ctx, err)
	if status.StatusCode != 400 || status.Response.Code != "share_invalid_input" {
		t.Fatalf("status = %d %s (%s), want 400 share_invalid_input", status.StatusCode, status.Response.Code, status.Response.Message)
	}
	if status.Response.Message == "" || !strings.Contains(status.Response.Message, "not a host") {
		t.Fatalf("message = %q, want it to name the invalid host", status.Response.Message)
	}
}
