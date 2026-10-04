package main

import (
	"context"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// TestCreateApiTokenRefusesShareOnlyAccount and
// TestCreateApiTokenRefusesRoleExceedingAccount are #228's own review
// finding: the mock previously let a caller create any token for any
// account, unlike production's CreateAPIToken (internal/api/apitoken.go),
// which refuses a share-only target outright (Q27) and a token role wider
// than the account's own role. A test or CLI flow exercised only against
// the mock could pass here and fail against the real daemon.
func TestCreateApiTokenRefusesShareOnlyAccount(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, "healthy")

	created, err := client.CreateUser(ctx, &apiv1.CreateUserRequest{
		Username: "share-only-kid",
		Role:     apiv1.NewOptCreateUserRequestRole(apiv1.CreateUserRequestRoleShareOnly),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = client.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{
		Name: "a token",
		Role: apiv1.ApiTokenRoleViewer,
	}, apiv1.CreateApiTokenParams{Username: created.Username})
	if err == nil {
		t.Fatal("CreateApiToken for a share-only account succeeded, want share_only_no_api_token")
	}
	if code := errorCode(t, err); code != "share_only_no_api_token" {
		t.Errorf("CreateApiToken for a share-only account error code = %q, want share_only_no_api_token", code)
	}
}

func TestCreateApiTokenRefusesRoleExceedingAccount(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, "healthy")

	created, err := client.CreateUser(ctx, &apiv1.CreateUserRequest{
		Username: "viewer-kid",
		Role:     apiv1.NewOptCreateUserRequestRole(apiv1.CreateUserRequestRoleViewer),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	_, err = client.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{
		Name: "an admin-scoped token",
		Role: apiv1.ApiTokenRoleAdmin,
	}, apiv1.CreateApiTokenParams{Username: created.Username})
	if err == nil {
		t.Fatal("CreateApiToken with an admin role for a viewer account succeeded, want token_role_exceeds_account")
	}
	if code := errorCode(t, err); code != "token_role_exceeds_account" {
		t.Errorf("CreateApiToken with an admin role for a viewer account error code = %q, want token_role_exceeds_account", code)
	}

	// A viewer-scoped token for the same account must still be allowed —
	// only widening the role is refused.
	if _, err := client.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{
		Name: "a viewer-scoped token",
		Role: apiv1.ApiTokenRoleViewer,
	}, apiv1.CreateApiTokenParams{Username: created.Username}); err != nil {
		t.Errorf("CreateApiToken with a viewer role for a viewer account: %v", err)
	}
}

// TestCreateApiTokenAllowsAdminAccountToNarrowRole is the success-path
// mirror: an admin account may still be issued either role of token,
// including one narrower than its own.
func TestCreateApiTokenAllowsAdminAccountToNarrowRole(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, "healthy")

	admin, err := client.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(admin.Users) == 0 {
		t.Fatal("healthy scenario has no seeded admin account")
	}

	if _, err := client.CreateApiToken(ctx, &apiv1.CreateApiTokenRequest{
		Name: "a viewer-scoped token for the admin",
		Role: apiv1.ApiTokenRoleViewer,
	}, apiv1.CreateApiTokenParams{Username: admin.Users[0].Username}); err != nil {
		t.Errorf("CreateApiToken with a viewer role for the admin account: %v", err)
	}
}

// The mock answers CreateUser as internal/api does: a name is folded to lower
// case, one a system account cannot carry is invalid_username, and one that
// differs from an existing name only in case is user_exists.
func TestCreateUserFoldsAndValidatesTheNameLikeProduction(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, "healthy")

	created, err := client.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "Mock-Kid"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.Username != "mock-kid" {
		t.Errorf("username = %q, want the lower-cased form", created.Username)
	}
	if _, err := client.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "MOCK-KID"}); err == nil || errorCode(t, err) != "user_exists" {
		t.Errorf("CreateUser(MOCK-KID) = %v, want user_exists", err)
	}
	if _, err := client.CreateUser(ctx, &apiv1.CreateUserRequest{Username: "mock kid"}); err == nil || errorCode(t, err) != "invalid_username" {
		t.Errorf("CreateUser(mock kid) = %v, want invalid_username", err)
	}
}
