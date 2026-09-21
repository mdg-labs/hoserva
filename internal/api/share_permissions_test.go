package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestSharePermissionsEditableFromEitherSide is #49's third acceptance
// criterion: setting per-user access from the share side is visible from
// the user side, and vice versa — the same rows, not two copies.
func TestSharePermissionsEditableFromEitherSide(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")
	seedShare(t, db, "backups")
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{{ID: u.ID, Access: "read-write"}}, nil); err != nil {
		t.Fatalf("SetSharePermissions: %v", err)
	}
	fromUserSide, err := svc.GetUserSharePermissions(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserSharePermissions: %v", err)
	}
	if len(fromUserSide) != 1 || fromUserSide[0].ShareName != "media" || fromUserSide[0].Access != "read-write" {
		t.Fatalf("GetUserSharePermissions after a share-side write = %+v, want [{media read-write}]", fromUserSide)
	}

	if err := svc.SetUserSharePermissions(ctx, u.ID, []api.UserSharePermission{
		{ShareName: "media", Access: "read-only"},
		{ShareName: "backups", Access: "none"},
	}); err != nil {
		t.Fatalf("SetUserSharePermissions: %v", err)
	}
	users, _, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(users) != 1 || users[0].UserID != u.ID || users[0].Access != "read-only" {
		t.Fatalf("GetSharePermissions(media) after a user-side write = %+v, want [{%s alice read-only}]", users, u.ID)
	}
}

func TestSetSharePermissionsRefusesUnknownShare(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = svc.SetUserSharePermissions(ctx, u.ID, []api.UserSharePermission{{ShareName: "no-such-share", Access: "read-only"}})
	if !errors.Is(err, store.ErrShareNotFound) {
		t.Errorf("SetUserSharePermissions(unknown share) = %v, want store.ErrShareNotFound", err)
	}
}

func TestSetSharePermissionsRefusesInvalidAccessLevel(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{{ID: u.ID, Access: "read-write-and-then-some"}}, nil)
	if !errors.Is(err, api.ErrInvalidAccessLevel) {
		t.Errorf("SetSharePermissions(bad access level) = %v, want ErrInvalidAccessLevel", err)
	}
}

func TestSetSharePermissionsRefusesUnknownUser(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")

	err := svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{{ID: "no-such-user", Access: "read-only"}}, nil)
	if !errors.Is(err, api.ErrUserNotFound) {
		t.Errorf("SetSharePermissions(unknown user) = %v, want ErrUserNotFound", err)
	}

	// Nothing should have been written for the share at all.
	users, _, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("share permissions after a refused write = %v, want none", users)
	}
}

func TestSetUserSharePermissionsRefusesUnknownUser(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")

	err := svc.SetUserSharePermissions(ctx, "no-such-user", []api.UserSharePermission{{ShareName: "media", Access: "read-only"}})
	if !errors.Is(err, api.ErrUserNotFound) {
		t.Errorf("SetUserSharePermissions(unknown user) = %v, want ErrUserNotFound", err)
	}

	// Nothing should have been written for the share at all.
	users, _, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("share permissions after a refused write = %v, want none", users)
	}
}

func TestGetUserSharePermissionsRefusesUnknownUser(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	_, err := svc.GetUserSharePermissions(ctx, "no-such-user")
	if !errors.Is(err, api.ErrUserNotFound) {
		t.Errorf("GetUserSharePermissions(unknown user) = %v, want ErrUserNotFound", err)
	}
}

func TestSetSharePermissionsRefusesDuplicateUser(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{
		{ID: u.ID, Access: "read-only"},
		{ID: u.ID, Access: "read-write"},
	}, nil)
	if !errors.Is(err, api.ErrDuplicateGrant) {
		t.Errorf("SetSharePermissions(duplicate user) = %v, want ErrDuplicateGrant", err)
	}

	users, _, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("share permissions after a refused write = %v, want none", users)
	}
}

func TestSetUserSharePermissionsRefusesDuplicateShare(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	err = svc.SetUserSharePermissions(ctx, u.ID, []api.UserSharePermission{
		{ShareName: "media", Access: "read-only"},
		{ShareName: "media", Access: "read-write"},
	})
	if !errors.Is(err, api.ErrDuplicateGrant) {
		t.Errorf("SetUserSharePermissions(duplicate share) = %v, want ErrDuplicateGrant", err)
	}

	fromUserSide, err := svc.GetUserSharePermissions(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUserSharePermissions: %v", err)
	}
	if len(fromUserSide) != 0 {
		t.Errorf("user share permissions after a refused write = %v, want none", fromUserSide)
	}
}

func TestSetSharePermissionsReplacesExistingRows(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	seedShare(t, db, "media")
	u1, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u2, err := svc.CreateUser(ctx, "bob", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{
		{ID: u1.ID, Access: "read-write"},
		{ID: u2.ID, Access: "read-only"},
	}, nil); err != nil {
		t.Fatalf("SetSharePermissions: %v", err)
	}
	if err := svc.SetSharePermissions(ctx, "media", []api.PermissionGrant{
		{ID: u2.ID, Access: "none"},
	}, nil); err != nil {
		t.Fatalf("SetSharePermissions (replace): %v", err)
	}

	users, _, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(users) != 1 || users[0].UserID != u2.ID || users[0].Access != "none" {
		t.Fatalf("share permissions after replace = %+v, want only bob at none", users)
	}
}
