package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newShareUsageTestHandler is newShareTestHandler (share_handler_test.go)
// with a real parity.UsageStore wired in, over the same database — #223's
// own end-to-end path: the handler must read exactly what that store
// persisted, never compute anything itself.
func newShareUsageTestHandler(t *testing.T) (*api.Handler, *parity.UsageStore) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "share-usage-handler.db")))
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
		{Role: store.ArrayRoleData, RoleIndex: 2, Device: "/dev/sde", Filesystem: "xfs", FSUUID: "d2", Mountpoint: filepath.Join(root, "disk2")},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "c", Mountpoint: filepath.Join(root, "cache")},
	}
	for _, d := range layout {
		if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	arrayStore := store.NewArrayStore(db)
	created := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    created,
	}, layout); err != nil {
		t.Fatal(err)
	}

	usages := parity.NewUsageStore(db)
	svc := &share.Service{
		Shares:   store.NewShareStore(db),
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       share.OSFS{},
		Mounter:  recordingShareMounter{},
		Usages:   usages,
		Now:      func() time.Time { return created },
		CatchAll: filepath.Join(root, "user"),
	}
	if err := os.MkdirAll(svc.CatchAll, 0o755); err != nil {
		t.Fatal(err)
	}
	return &api.Handler{Shares: svc}, usages
}

func TestHandler_GetShare_UsageNullBeforeAnySync(t *testing.T) {
	ctx := context.Background()
	h, _ := newShareUsageTestHandler(t)

	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	got, err := h.GetShare(ctx, apiv1.GetShareParams{Name: "media"})
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	if !got.Usage.IsNull() {
		v, _ := got.Usage.Get()
		t.Fatalf("Usage = %+v, want null (never synced)", v)
	}
}

func TestHandler_GetShare_UsageReflectsPersistedFigures(t *testing.T) {
	ctx := context.Background()
	h, usages := newShareUsageTestHandler(t)

	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	asOf := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	if err := usages.Replace(ctx, []parity.ShareUsage{
		{Share: "media", Disk: "/mnt/disk1", Bytes: 100},
		{Share: "media", Disk: "/mnt/disk2", Bytes: 250},
	}, asOf); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	got, err := h.GetShare(ctx, apiv1.GetShareParams{Name: "media"})
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	usage, ok := got.Usage.Get()
	if !ok {
		t.Fatal("Usage is null, want a computed value")
	}
	if usage.TotalBytes != 350 {
		t.Fatalf("TotalBytes = %d, want 350", usage.TotalBytes)
	}
	if !usage.AsOf.Equal(asOf) {
		t.Fatalf("AsOf = %v, want %v", usage.AsOf, asOf)
	}
	if len(usage.PerDisk) != 2 {
		t.Fatalf("PerDisk = %+v, want 2 entries", usage.PerDisk)
	}

	list, err := h.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if len(list.Shares) != 1 {
		t.Fatalf("ListShares = %+v", list.Shares)
	}
	listUsage, ok := list.Shares[0].Usage.Get()
	if !ok || listUsage.TotalBytes != 350 {
		t.Fatalf("ListShares[0].Usage = %+v (ok=%v), want 350 total", listUsage, ok)
	}
}
