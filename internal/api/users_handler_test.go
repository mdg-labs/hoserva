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

	list, err := h.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if list.Users[0].HasCredential || !list.Users[0].LastLogin.IsNull() {
		t.Errorf("kid = %+v, want no credential and no last login before SetUserPassword/Login", list.Users[0])
	}

	if err := h.SetUserPassword(ctx, &apiv1.SetUserPasswordRequest{Password: "correct horse battery staple"}, apiv1.SetUserPasswordParams{UserId: created.ID}); err != nil {
		t.Fatalf("SetUserPassword: %v", err)
	}

	list, err = h.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if !list.Users[0].HasCredential {
		t.Error("HasCredential must be true once SetUserPassword succeeds")
	}
	if !list.Users[0].LastLogin.IsNull() {
		t.Error("LastLogin must still be null — setting a password is not signing in")
	}

	_, token, err := authSvc.Login(ctx, "kid", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" {
		t.Fatal("expected a session token")
	}

	list, err = h.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if list.Users[0].LastLogin.IsNull() {
		t.Error("LastLogin must be set after a successful Login")
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

func TestHandlerCreateListAndRevokeApiToken(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	created, err := h.CreateApiToken(ctx,
		&apiv1.CreateApiTokenRequest{Name: "ci-script", Role: apiv1.ApiTokenRoleViewer},
		apiv1.CreateApiTokenParams{Username: "admin"})
	if err != nil {
		t.Fatalf("CreateApiToken: %v", err)
	}
	if created.Token == "" {
		t.Fatal("expected a non-empty raw token, shown once")
	}
	if created.Username != "admin" || created.Role != apiv1.ApiTokenRoleViewer {
		t.Errorf("created = %+v, want username=admin role=viewer", created)
	}

	list, err := h.ListApiTokens(ctx)
	if err != nil {
		t.Fatalf("ListApiTokens: %v", err)
	}
	if len(list.Tokens) != 1 || list.Tokens[0].ID != created.ID {
		t.Fatalf("ListApiTokens = %+v, want exactly the token just created", list.Tokens)
	}
	// The list response never carries the raw token itself, only enough
	// to identify and revoke it (ApiTokenSummary has no token field at
	// all — this is a compile-time guarantee, not something this
	// assertion could fail to catch at runtime, but the point is worth a
	// comment: the raw value is only ever seen once, on creation).

	if err := h.RevokeApiToken(ctx, apiv1.RevokeApiTokenParams{TokenId: created.ID}); err != nil {
		t.Fatalf("RevokeApiToken: %v", err)
	}
	list, err = h.ListApiTokens(ctx)
	if err != nil {
		t.Fatalf("ListApiTokens after revoke: %v", err)
	}
	if len(list.Tokens) != 0 {
		t.Errorf("ListApiTokens after revoke = %d, want 0", len(list.Tokens))
	}
}

func TestHandlerRevokeApiTokenNotFound(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)
	err := h.RevokeApiToken(ctx, apiv1.RevokeApiTokenParams{TokenId: "no-such-token"})
	status := apiError(t, h, err)
	if status.StatusCode != 404 || status.Response.Code != "api_token_not_found" {
		t.Errorf("RevokeApiToken(unknown) error = %+v, want 404 api_token_not_found", status)
	}
}

func TestHandlerCreateApiTokenRefusesShareOnly(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if _, err := h.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "kid"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err := h.CreateApiToken(ctx,
		&apiv1.CreateApiTokenRequest{Name: "kid-script", Role: apiv1.ApiTokenRoleViewer},
		apiv1.CreateApiTokenParams{Username: "kid"})
	status := apiError(t, h, err)
	if status.StatusCode != 403 || status.Response.Code != "share_only_no_api_token" {
		t.Errorf("CreateApiToken for a share-only account error = %+v, want 403 share_only_no_api_token", status)
	}
}
