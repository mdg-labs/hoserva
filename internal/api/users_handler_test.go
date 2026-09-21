package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/share"
)

func TestHandlerCreateAndListUsers(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.Role != apiv1.UserRoleShareOnly {
		t.Errorf("role = %q, want share-only when omitted", created.Role)
	}

	list, err := h.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(list.Users) != 2 {
		t.Fatalf("ListUsers = %d users, want 2 (admin + kid)", len(list.Users))
	}
}

func TestHandlerCreateUserDuplicateUsernameConflict(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)
	if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "user_exists" {
		t.Errorf("duplicate CreateUser error = %+v, want 409 user_exists", status)
	}
}

func TestHandlerUpdateUserChangesRole(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)
	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	updated, err := h.UpdateUser(ctx, &apiv1.UpdateUserRequest{Role: apiv1.UpdateUserRequestRoleViewer}, apiv1.UpdateUserParams{UserId: created.ID})
	if err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if updated.Role != apiv1.UserRoleViewer {
		t.Errorf("role = %q, want viewer", updated.Role)
	}
}

func TestHandlerDeleteUserRefusesSoleAdmin(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	admin, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	err = h.DeleteUser(ctx, apiv1.DeleteUserParams{UserId: uuid.MustParse(admin.ID)})
	status := apiError(t, h, err)
	if status.StatusCode != 403 || status.Response.Code != "cannot_modify_admin" {
		t.Errorf("DeleteUser(admin) error = %+v, want 403 cannot_modify_admin", status)
	}
}

func TestHandlerSetUserPasswordAndSessions(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	authSvc.SambaAccounts = share.NewFakeSambaAccounts()
	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{
		Username: "kid",
		Role:     apiv1.NewOptCreateUserRequestRole(apiv1.CreateUserRequestRoleViewer),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: "correct horse battery staple"}, apiv1.SetUserPasswordParams{UserId: created.ID}); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	_, token, err := authSvc.Login(ctx, "kid", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Fatal("expected a session token")
	}

	sessions, err := h.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].Username != "kid" {
		t.Fatalf("ListSessions = %+v, want one session for kid", sessions.Sessions)
	}

	if err := h.RevokeSession(ctx, apiv1.RevokeSessionParams{SessionId: sessions.Sessions[0].ID}); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := authSvc.ValidateSession(ctx, token); err == nil {
		t.Error("session should be invalid after RevokeSession")
	}
}

func TestHandlerUserGroupsLifecycle(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)
	created, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	g, err := h.CreateUserGroup(ctx, &apiv1.CreateUserGroupRequest{Name: "family"})
	if err != nil {
		t.Fatalf("CreateUserGroup: %v", err)
	}
	updated, err := h.SetUserGroupMembers(ctx, &apiv1.SetUserGroupMembersRequest{UserIds: []uuid.UUID{created.ID}}, apiv1.SetUserGroupMembersParams{GroupId: g.ID})
	if err != nil {
		t.Fatalf("SetUserGroupMembers: %v", err)
	}
	if len(updated.MemberUserIds) != 1 || updated.MemberUserIds[0] != created.ID {
		t.Errorf("group members = %v, want [%s]", updated.MemberUserIds, created.ID)
	}

	list, err := h.ListUserGroups(ctx)
	if err != nil {
		t.Fatalf("ListUserGroups: %v", err)
	}
	if len(list.Groups) != 1 {
		t.Fatalf("ListUserGroups = %d, want 1", len(list.Groups))
	}

	if err := h.DeleteUserGroup(ctx, apiv1.DeleteUserGroupParams{GroupId: g.ID}); err != nil {
		t.Fatalf("DeleteUserGroup: %v", err)
	}
	list, err = h.ListUserGroups(ctx)
	if err != nil {
		t.Fatalf("ListUserGroups: %v", err)
	}
	if len(list.Groups) != 0 {
		t.Errorf("ListUserGroups after delete = %d, want 0", len(list.Groups))
	}
}
