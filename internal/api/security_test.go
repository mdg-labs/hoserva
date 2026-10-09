package api_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
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
	_, err = sec.HandleSessionCookie(withSessionSecret(t, authSvc, ctx, token), apiv1.CancelJobOperation, apiv1.SessionCookie{APIKey: token})
	if err == nil {
		t.Fatal("a viewer must be refused an admin-only operation (cancelJob)")
	}

	// The same session is accepted for a viewer-role operation.
	if _, err := sec.HandleSessionCookie(withSessionSecret(t, authSvc, ctx, token), apiv1.ListJobsOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
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
	if _, err := sec.HandleSessionCookie(withSessionSecret(t, authSvc, ctx, token), apiv1.ListJobsOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
		t.Errorf("admin should satisfy a viewer-role operation: %v", err)
	}
	if _, err := sec.HandleSessionCookie(withSessionSecret(t, authSvc, ctx, token), apiv1.CancelJobOperation, apiv1.SessionCookie{APIKey: token}); err != nil {
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

// withSessionSecret returns ctx carrying the second secret of token's own
// session, as the guard in front of the generated server puts it there for
// a request that sent the header.
func withSessionSecret(t *testing.T, authSvc *api.AuthService, ctx context.Context, token string) context.Context {
	t.Helper()
	secret, err := authSvc.SessionSecret(token)
	if err != nil {
		t.Fatalf("SessionSecret: %v", err)
	}
	return api.WithSessionSecret(ctx, secret)
}

func TestSessionSecurityHandlerRefusesACookieWithoutItsSessionSecret(t *testing.T) {
	_, authSvc := newAuthTestHandler(t)
	ctx := context.Background()
	_, token, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := authSvc.Login(ctx, "admin", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatal(err)
	}
	otherSecret, err := authSvc.SessionSecret(otherToken)
	if err != nil {
		t.Fatal(err)
	}
	unrelated, err := authSvc.MachineKey.Encrypt([]byte("a TOTP secret sealed under the same key"))
	if err != nil {
		t.Fatal(err)
	}

	sec := &api.SessionSecurityHandler{Auth: authSvc}
	cookie := apiv1.SessionCookie{APIKey: token}
	for name, sctx := range map[string]context.Context{
		"no secret":                       ctx,
		"an empty secret":                 api.WithSessionSecret(ctx, ""),
		"a secret that is not hex":        api.WithSessionSecret(ctx, "not-hex"),
		"another session's secret":        api.WithSessionSecret(ctx, otherSecret),
		"a secret sealed for another use": api.WithSessionSecret(ctx, hex.EncodeToString(unrelated)),
	} {
		if _, err := sec.HandleSessionCookie(sctx, apiv1.CancelJobOperation, cookie); !errors.Is(err, api.ErrSessionInvalid) {
			t.Errorf("admin operation with %s = %v, want ErrSessionInvalid", name, err)
		}
	}

	if _, err := sec.HandleSessionCookie(withSessionSecret(t, authSvc, ctx, token), apiv1.CancelJobOperation, cookie); err != nil {
		t.Errorf("admin operation with the session's own secret: %v", err)
	}
	if _, err := sec.HandleSessionCookie(ctx, apiv1.GetCatalogTemplateIconOperation, cookie); err != nil {
		t.Errorf("a catalog icon, which an <img> loads, with the cookie alone: %v", err)
	}
}

func TestGuardBrowserRequestsRefusesAStateChangeFromAnotherOrigin(t *testing.T) {
	reached := false
	guarded := api.GuardBrowserRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	for _, tc := range []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"another port of the same host", http.MethodPost, map[string]string{"Origin": "https://hoserva.lan:8443"}, http.StatusForbidden},
		{"another host", http.MethodPut, map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"the null origin", http.MethodDelete, map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"an origin with a path", http.MethodPost, map[string]string{"Origin": "https://hoserva.lan:8008/x"}, http.StatusForbidden},
		{"same-site", http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cross-site", http.MethodPatch, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"this origin", http.MethodPost, map[string]string{"Origin": "https://hoserva.lan:8008", "Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"this origin in another letter case", http.MethodPost, map[string]string{"Origin": "https://HOSERVA.lan:8008"}, http.StatusOK},
		{"a user-initiated request", http.MethodPost, map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK},
		{"no browser headers", http.MethodPost, nil, http.StatusOK},
		{"a read from another origin", http.MethodGet, map[string]string{"Origin": "https://hoserva.lan:8443", "Sec-Fetch-Site": "same-site"}, http.StatusOK},
	} {
		reached = false
		req := httptest.NewRequest(tc.method, "https://hoserva.lan:8008/api/v1/jobs", nil)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
		if reached != (tc.want == http.StatusOK) {
			t.Errorf("%s: reached the handler = %v, want %v", tc.name, reached, tc.want == http.StatusOK)
		}
	}
}
