package api_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// newShareAndAuthTestHandler wires share.Service and AuthService against
// the same database — the handler methods under test here validate a
// share through h.Shares, then read/write through h.Auth, and both must
// see the same "media" row for that to mean anything.
func newShareAndAuthTestHandler(t *testing.T) (*api.Handler, *api.AuthService) {
	t.Helper()
	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	db, err := sql.Open("sqlite", store.DSN(filepath.Join(t.TempDir(), "share-permissions.db")))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	root := t.TempDir()
	layout := []store.ArrayDisk{
		{Role: store.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "p", Mountpoint: filepath.Join(root, "parity1")},
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdb", Filesystem: "xfs", FSUUID: "d1", Mountpoint: filepath.Join(root, "disk1")},
		{Role: store.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "ext4", FSUUID: "c", Mountpoint: filepath.Join(root, "cache")},
	}
	for _, d := range layout {
		if err := os.MkdirAll(d.Mountpoint, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d.Mountpoint, err)
		}
	}
	arrayStore := store.NewArrayStore(db)
	if err := arrayStore.PutArray(context.Background(), store.ArraySettings{
		CreatePolicy: string(pool.DefaultCreatePolicy),
		MinFreeSpace: "50G",
		CreatedAt:    time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}, layout); err != nil {
		t.Fatalf("PutArray: %v", err)
	}

	shareSvc := &share.Service{
		Shares:   store.NewShareStore(db),
		Array:    arrayStore,
		Gen:      config.NewGenerator(filepath.Join(root, "etc")),
		FS:       share.OSFS{},
		Now:      func() time.Time { return time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC) },
		CatchAll: filepath.Join(root, "user"),
	}
	if err := os.MkdirAll(shareSvc.CatchAll, 0o755); err != nil {
		t.Fatalf("mkdir catch-all: %v", err)
	}

	authStore := api.NewAuthStore(db)
	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	authSvc := api.NewAuthService(authStore, key)

	return &api.Handler{Shares: shareSvc, Auth: authSvc}, authSvc
}

func TestHandlerSharePermissionsRoundTrip(t *testing.T) {
	ctx := context.Background()
	h, _ := newShareAndAuthTestHandler(t)
	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}
	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "alice"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	result, err := h.UpdateSharePermissions(ctx, &apiv1.UpdateSharePermissionsRequest{
		Users: []apiv1.UpdateSharePermissionsRequestUsersItem{
			{UserId: created.ID, Access: apiv1.ShareAccessLevelReadWrite},
		},
		Groups: []apiv1.UpdateSharePermissionsRequestGroupsItem{},
	}, apiv1.UpdateSharePermissionsParams{Name: "media"})
	if err != nil {
		t.Fatalf("UpdateSharePermissions: %v", err)
	}
	if len(result.Users) != 1 || result.Users[0].Username != "alice" || result.Users[0].Access != apiv1.ShareAccessLevelReadWrite {
		t.Fatalf("UpdateSharePermissions result = %+v, want alice at read-write", result.Users)
	}

	fetched, err := h.GetSharePermissions(ctx, apiv1.GetSharePermissionsParams{Name: "media"})
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(fetched.Users) != 1 || fetched.Users[0].UserId != created.ID {
		t.Fatalf("GetSharePermissions = %+v, want the row just written", fetched.Users)
	}
}

func TestHandlerGetSharePermissionsUnknownShare(t *testing.T) {
	ctx := context.Background()
	h, _ := newShareAndAuthTestHandler(t)
	_, err := h.GetSharePermissions(ctx, apiv1.GetSharePermissionsParams{Name: "no-such-share"})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "share_not_found" {
		t.Errorf("GetSharePermissions(unknown share) = %+v, want 404 share_not_found", status)
	}
}

func TestHandlerUpdateSharePermissionsUnknownUser(t *testing.T) {
	ctx := context.Background()
	h, _ := newShareAndAuthTestHandler(t)
	if _, err := h.CreateShare(ctx, &apiv1.CreateShareRequest{Name: "media"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	_, err := h.UpdateSharePermissions(ctx, &apiv1.UpdateSharePermissionsRequest{
		Users:  []apiv1.UpdateSharePermissionsRequestUsersItem{{UserId: uuid.New(), Access: apiv1.ShareAccessLevelReadOnly}},
		Groups: []apiv1.UpdateSharePermissionsRequestGroupsItem{},
	}, apiv1.UpdateSharePermissionsParams{Name: "media"})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "user_not_found" {
		t.Errorf("UpdateSharePermissions(unknown user) = %+v, want 404 user_not_found", status)
	}
}
