package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/share"
)

func TestCreateAPITokenShownOnceAndStoredHashed(t *testing.T) {
	svc, db := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	token, u, raw, err := svc.CreateAPIToken(ctx, "admin", "ci-script", "viewer")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if raw == "" {
		t.Fatal("expected a non-empty raw token")
	}
	if u.Username != "admin" {
		t.Errorf("owner username = %q, want admin", u.Username)
	}
	if token.Role != "viewer" {
		t.Errorf("token.Role = %q, want viewer", token.Role)
	}

	// Only the hash is ever persisted — the raw value never appears in the
	// database at all, mirroring sessions.token_hash's own guarantee.
	var storedHash string
	if err := db.QueryRowContext(ctx, `SELECT token_hash FROM api_tokens WHERE token_hash = ?`, token.TokenHash).Scan(&storedHash); err != nil {
		t.Fatalf("reading back api_tokens row: %v", err)
	}
	if storedHash == raw {
		t.Fatal("the raw token must never be stored as its own hash")
	}

	// The raw value is exactly what ValidateAPIToken accepts back — proof
	// the stored hash actually corresponds to it, not just that some hash
	// was written.
	gotToken, gotUser, err := svc.ValidateAPIToken(ctx, raw)
	if err != nil {
		t.Fatalf("ValidateAPIToken(raw): %v", err)
	}
	if gotToken.TokenHash != token.TokenHash || gotUser.ID != u.ID {
		t.Error("ValidateAPIToken did not resolve back to the token just created")
	}
}

func TestCreateAPITokenRefusesShareOnlyAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "kid", "share-only"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, _, _, err := svc.CreateAPIToken(ctx, "kid", "kid-script", "viewer"); !errors.Is(err, api.ErrShareOnlyNoAPIToken) {
		t.Errorf("CreateAPIToken for a share-only account = %v, want ErrShareOnlyNoAPIToken", err)
	}
}

func TestCreateAPITokenRefusesRoleExceedingAccount(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "viewer1", "viewer"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if _, _, _, err := svc.CreateAPIToken(ctx, "viewer1", "viewer-script", "admin"); !errors.Is(err, api.ErrTokenRoleExceedsAccount) {
		t.Errorf("CreateAPIToken(admin role for a viewer account) = %v, want ErrTokenRoleExceedsAccount", err)
	}

	// The same account can still be issued a viewer-scoped token — the
	// refusal above is about exceeding the account's own role, not about
	// the account being unable to hold any token at all.
	if _, _, _, err := svc.CreateAPIToken(ctx, "viewer1", "viewer-script", "viewer"); err != nil {
		t.Errorf("CreateAPIToken(viewer role for a viewer account): %v", err)
	}
}

func TestCreateAPITokenRejectsInvalidRole(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	if _, _, _, err := svc.CreateAPIToken(ctx, "admin", "bad-role", "share-only"); !errors.Is(err, api.ErrInvalidTokenRole) {
		t.Errorf("CreateAPIToken(role=share-only) = %v, want ErrInvalidTokenRole", err)
	}
}

func TestListAPITokensIncludesUsername(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if _, _, _, err := svc.CreateAPIToken(ctx, "admin", "ci-script", "viewer"); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	tokens, err := svc.ListAPITokens(ctx)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("len(tokens) = %d, want 1", len(tokens))
	}
	if tokens[0].Username != "admin" || tokens[0].Name != "ci-script" {
		t.Errorf("token = %+v, want username=admin name=ci-script", tokens[0])
	}
}

func TestRevokeAPITokenReportsNotFound(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	if err := svc.RevokeAPIToken(ctx, "no-such-hash"); !errors.Is(err, api.ErrAPITokenNotFound) {
		t.Errorf("RevokeAPIToken(unknown) = %v, want ErrAPITokenNotFound", err)
	}
}

func TestValidateAPITokenRejectsEmptyAndUnknown(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	if _, _, err := svc.ValidateAPIToken(ctx, ""); !errors.Is(err, api.ErrAPITokenInvalid) {
		t.Errorf("ValidateAPIToken(\"\") = %v, want ErrAPITokenInvalid", err)
	}
	if _, _, err := svc.ValidateAPIToken(ctx, "hspat_does-not-exist"); !errors.Is(err, api.ErrAPITokenInvalid) {
		t.Errorf("ValidateAPIToken(unknown) = %v, want ErrAPITokenInvalid", err)
	}
}

// TestDeleteUserRemovesAPITokens proves DeleteUser's cascade covers
// api_tokens too, not only sessions and group membership (#50).
func TestDeleteUserRemovesAPITokens(t *testing.T) {
	svc, _ := newAuthTestService(t)
	svc.SambaAccounts = share.NewFakeSambaAccounts()
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	u, err := svc.CreateUser(ctx, "kid", "viewer")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, _, _, err := svc.CreateAPIToken(ctx, "kid", "kid-script", "viewer"); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	if err := svc.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}

	tokens, err := svc.ListAPITokens(ctx)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	for _, tok := range tokens {
		if tok.UserID == u.ID {
			t.Errorf("token %s still references deleted user %s", tok.TokenHash, u.ID)
		}
	}
}
