package api_test

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
)

func TestSessionSecurityHandlerRejectsUnknownSession(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	sec := &api.SessionSecurityHandler{Auth: authSvc}

	_, err := sec.HandleSessionCookie(context.Background(), apiv1.GetCurrentSessionOperation, apiv1.SessionCookie{APIKey: "no-such-token"})
	if !errors.Is(err, api.ErrSessionInvalid) {
		t.Errorf("HandleSessionCookie(unknown token) = %v, want ErrSessionInvalid", err)
	}
}

func TestSessionSecurityHandlerRefusesViewerOnAdminOperation(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	ctx := context.Background()

	// createFirstAdmin only ever creates an admin (Q27's viewer role has
	// no user-management operation in this issue) — this test builds a
	// viewer session directly against the store to exercise the refusal,
	// exactly as a future viewer account would hit it.
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &api.User{ID: "viewer-1", Username: "viewer", Role: "viewer", PasswordHash: hash, CreatedAt: authSvc.Now()}
	if err := authSvc.Store.CreateUser(ctx, viewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, token, err := authSvc.Login(ctx, "viewer", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}
	_, err = sec.HandleSessionCookie(ctx, apiv1.CancelJobOperation, apiv1.SessionCookie{APIKey: token})
	if err == nil {
		t.Fatal("a viewer must be refused an admin-only operation (cancelJob)")
	}

	// The same session is accepted for a viewer-role operation.
	if _, err := sec.HandleSessionCookie(ctx, apiv1.ListJobsOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
		t.Errorf("a viewer should be allowed a viewer operation (listJobs): %v", err)
	}
}

func TestSessionSecurityHandlerAdminSatisfiesViewerOperation(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	ctx := context.Background()
	_, token, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}
	if _, err := sec.HandleSessionCookie(ctx, apiv1.ListJobsOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
		t.Errorf("admin should satisfy a viewer-role operation: %v", err)
	}
	if _, err := sec.HandleSessionCookie(ctx, apiv1.CancelJobOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
		t.Errorf("admin should satisfy an admin-role operation: %v", err)
	}
}

func TestSessionSecurityHandlerRejectsUnknownAPIToken(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	sec := &api.SessionSecurityHandler{Auth: authSvc}

	_, err := sec.HandleApiToken(context.Background(), apiv1.ListJobsOperation, apiv1.ApiToken{Token: "no-such-token"})
	if !errors.Is(err, api.ErrAPITokenInvalid) {
		t.Errorf("HandleApiToken(unknown token) = %v, want ErrAPITokenInvalid", err)
	}
}

func TestSessionSecurityHandlerAcceptsValidAPIToken(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	ctx := context.Background()
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	_, _, raw, err := authSvc.CreateAPIToken(ctx, "admin", "ci-script", "viewer")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}

	// The token was created as viewer-scoped, so it satisfies a viewer
	// operation but not an admin one, even though the owning account
	// itself is admin (Q43: a token can only narrow an account's access).
	if _, err := sec.HandleApiToken(ctx, apiv1.ListJobsOperation, apiv1.ApiToken{Token: raw}); err != nil {
		t.Errorf("a viewer-scoped token should satisfy a viewer operation: %v", err)
	}
	if _, err := sec.HandleApiToken(ctx, apiv1.CancelJobOperation, apiv1.ApiToken{Token: raw}); err == nil {
		t.Error("a viewer-scoped token must not satisfy an admin operation")
	}
}

// TestSessionSecurityHandlerAPITokenRevocationTakesEffectImmediately is
// #50's own acceptance criterion: RevokeAPIToken deletes the row
// HandleApiToken looks up fresh on every call (no caching), so a revoked
// token stops authenticating on its very next use.
func TestSessionSecurityHandlerAPITokenRevocationTakesEffectImmediately(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	ctx := context.Background()
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	token, _, raw, err := authSvc.CreateAPIToken(ctx, "admin", "ci-script", "admin")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}
	if _, err := sec.HandleApiToken(ctx, apiv1.ListJobsOperation, apiv1.ApiToken{Token: raw}); err != nil {
		t.Fatalf("token should authenticate before revocation: %v", err)
	}

	if err := authSvc.RevokeAPIToken(ctx, token.TokenHash); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, err := sec.HandleApiToken(ctx, apiv1.ListJobsOperation, apiv1.ApiToken{Token: raw}); !errors.Is(err, api.ErrAPITokenInvalid) {
		t.Errorf("HandleApiToken after revocation = %v, want ErrAPITokenInvalid", err)
	}
}

// TestSessionSecurityHandlerAPITokenRecordsUseInAuditLog is #50's other
// acceptance criterion: an API token authenticating a request is recorded
// in the audit log.
func TestSessionSecurityHandlerAPITokenRecordsUseInAuditLog(t *testing.T) {
	authSvc, db := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	_, _, raw, err := authSvc.CreateAPIToken(ctx, "admin", "ci-script", "admin")
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}
	if _, err := sec.HandleApiToken(ctx, apiv1.ListJobsOperation, apiv1.ApiToken{Token: raw}); err != nil {
		t.Fatalf("HandleApiToken: %v", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action = 'api_token_used'`).Scan(&count); err != nil {
		t.Fatalf("counting audit_log: %v", err)
	}
	if count != 1 {
		t.Errorf("audit_log rows with action=api_token_used = %d, want 1", count)
	}
}

func TestTrustedSecurityHandlerAlwaysGrantsAdmin(t *testing.T) {
	trusted := api.TrustedSecurityHandler{}

	ctx, err := trusted.HandleSessionCookie(context.Background(), apiv1.CancelJobOperation, apiv1.SessionCookie{APIKey: "irrelevant"})
	if err != nil {
		t.Fatalf("TrustedSecurityHandler must never refuse: %v", err)
	}
	p, ok := api.PrincipalFromContext(ctx)
	if !ok || p.Role != api.RoleAdmin || !p.Local {
		t.Errorf("principal = %+v, want a local admin principal", p)
	}

	ctx, err = trusted.HandleApiToken(context.Background(), apiv1.ListJobsOperation, apiv1.ApiToken{Token: "irrelevant"})
	if err != nil {
		t.Fatalf("TrustedSecurityHandler must never refuse: %v", err)
	}
	p, ok = api.PrincipalFromContext(ctx)
	if !ok || p.Role != api.RoleAdmin || !p.Local {
		t.Errorf("principal = %+v, want a local admin principal", p)
	}
}
