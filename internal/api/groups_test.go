package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
)

func TestCreateGroupRejectsDuplicateName(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateGroup(ctx, "family"); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if _, err := svc.CreateGroup(ctx, "family"); !errors.Is(err, api.ErrGroupExists) {
		t.Errorf("CreateGroup(duplicate) = %v, want ErrGroupExists", err)
	}
}

func TestSetGroupMembersRefusesUnknownUser(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, "family")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	if _, err := svc.SetGroupMembers(ctx, g.ID, []string{"no-such-user"}); !errors.Is(err, api.ErrUserNotFound) {
		t.Errorf("SetGroupMembers(unknown user) = %v, want ErrUserNotFound", err)
	}

	// The bad id must not have partially written a membership row before
	// failing.
	unchanged, err := svc.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(unchanged) != 1 || len(unchanged[0].MemberIDs) != 0 {
		t.Errorf("groups after a refused SetGroupMembers = %+v, want the group still empty", unchanged)
	}
}

func TestSetGroupMembersRefusesDuplicateUser(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, "family")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	u, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, err := svc.SetGroupMembers(ctx, g.ID, []string{u.ID, u.ID}); !errors.Is(err, api.ErrDuplicateGrant) {
		t.Errorf("SetGroupMembers(duplicate user) = %v, want ErrDuplicateGrant", err)
	}

	unchanged, err := svc.ListGroups(ctx)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if len(unchanged) != 1 || len(unchanged[0].MemberIDs) != 0 {
		t.Errorf("groups after a refused SetGroupMembers = %+v, want the group still empty", unchanged)
	}
}

func TestSetGroupMembersReplacesMembership(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, "family")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	u1, err := svc.CreateUser(ctx, "alice", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u2, err := svc.CreateUser(ctx, "bob", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, err := svc.SetGroupMembers(ctx, g.ID, []string{u1.ID, u2.ID}); err != nil {
		t.Fatalf("SetGroupMembers: %v", err)
	}
	updated, err := svc.SetGroupMembers(ctx, g.ID, []string{u2.ID})
	if err != nil {
		t.Fatalf("SetGroupMembers (replace): %v", err)
	}
	if len(updated.MemberIDs) != 1 || updated.MemberIDs[0] != u2.ID {
		t.Errorf("members after replace = %v, want only %s", updated.MemberIDs, u2.ID)
	}
}

func TestDeleteGroupRemovesSharePermissions(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, "family")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	seedShare(t, db, "media")

	if err := svc.SetSharePermissions(ctx, "media", nil, []api.PermissionGrant{{ID: g.ID, Access: "read-only"}}); err != nil {
		t.Fatalf("SetSharePermissions: %v", err)
	}
	if err := svc.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}

	_, groups, err := svc.GetSharePermissions(ctx, "media")
	if err != nil {
		t.Fatalf("GetSharePermissions: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("group permissions after deleting the group = %v, want none left over", groups)
	}
}
