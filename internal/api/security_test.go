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

func TestSessionSecurityHandlerApiTokenNotImplemented(t *testing.T) {
	sec := &api.SessionSecurityHandler{}
	_, err := sec.HandleApiToken(context.Background(), apiv1.ListJobsOperation, apiv1.ApiToken{Token: "anything"})
	if err == nil {
		t.Error("API tokens are Phase 2 (Q43) — HandleApiToken must always refuse for now")
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
