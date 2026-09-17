package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newAuthTestHandler is newTestHandler (handler_test.go) plus a wired
// AuthService, for the Handler methods #22 adds.
func newAuthTestHandler(t *testing.T) (*api.Handler, *api.AuthService) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "auth-handler-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	authStore := api.NewAuthStore(db)
	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	authSvc := api.NewAuthService(authStore, key)

	jobStore := job.NewStore(db)
	logs := job.NewLogStore(t.TempDir())
	scheduler := job.NewScheduler(jobStore, logs, job.NewHub(), job.NewRegistry())

	h := &api.Handler{Scheduler: scheduler, Store: jobStore, Logs: logs, Auth: authSvc}
	return h, authSvc
}

func TestHandlerGetSetupStatus(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)

	status, err := h.GetSetupStatus(ctx)
	if err != nil {
		t.Fatalf("GetSetupStatus: %v", err)
	}
	if status.AdminExists {
		t.Error("AdminExists should be false before any admin is created")
	}

	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	status, err = h.GetSetupStatus(ctx)
	if err != nil {
		t.Fatalf("GetSetupStatus: %v", err)
	}
	if !status.AdminExists {
		t.Error("AdminExists should be true once an admin exists")
	}
}

func TestHandlerCreateFirstAdminSetsCookie(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)

	out, err := h.CreateFirstAdmin(ctx, &apiv1.CreateFirstAdminRequest{Username: "admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if out.Response.Username != "admin" || out.Response.Role != apiv1.UserRoleAdmin {
		t.Errorf("response = %+v, want username=admin role=admin", out.Response)
	}
	cookie, ok := out.SetCookie.Get()
	if !ok || !strings.Contains(cookie, "hoserva_session=") {
		t.Errorf("Set-Cookie = %q, want a hoserva_session cookie", cookie)
	}
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "Secure") || !strings.Contains(cookie, "SameSite=Strict") {
		t.Errorf("Set-Cookie = %q, want HttpOnly, Secure and SameSite=Strict", cookie)
	}

	// Second call is refused once an admin exists.
	_, err = h.CreateFirstAdmin(ctx, &apiv1.CreateFirstAdminRequest{Username: "someone-else", Password: "another password entirely"})
	status := apiError(t, h, err)
	if status.StatusCode != 409 || status.Response.Code != "setup_complete" {
		t.Errorf("second CreateFirstAdmin error = %+v, want 409 setup_complete", status)
	}
}

func TestHandlerLoginAndLogout(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	out, err := h.Login(ctx, &apiv1.LoginRequest{Username: "admin", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	cookie, ok := out.SetCookie.Get()
	if !ok {
		t.Fatal("expected a Set-Cookie header")
	}
	token := sessionTokenFromCookie(t, cookie)

	sessionCtx := principalContextForToken(ctx, authSvc, token)
	current, err := h.GetCurrentSession(sessionCtx)
	if err != nil {
		t.Fatalf("GetCurrentSession: %v", err)
	}
	if current.Username != "admin" {
		t.Errorf("GetCurrentSession = %+v, want username=admin", current)
	}

	logoutOut, err := h.Logout(sessionCtx)
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}
	cleared, ok := logoutOut.SetCookie.Get()
	if !ok || !strings.Contains(cleared, "hoserva_session=;") {
		t.Errorf("Logout Set-Cookie = %q, want a cleared hoserva_session cookie", cleared)
	}

	if _, err := authSvc.ValidateSession(ctx, token); err == nil {
		t.Error("session should be revoked after Logout")
	}
}

func TestHandlerLoginWrongPassword(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	if _, _, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	_, err := h.Login(ctx, &apiv1.LoginRequest{Username: "admin", Password: "wrong"})
	status := apiError(t, h, err)
	if status.StatusCode != 401 || status.Response.Code != "invalid_credentials" {
		t.Errorf("Login(wrong password) error = %+v, want 401 invalid_credentials", status)
	}
}

func TestHandlerEnrollAndConfirmTotp(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	_, token, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	sessionCtx := principalContextForToken(ctx, authSvc, token)

	enrolled, err := h.EnrollTotp(sessionCtx, &apiv1.TotpEnrollRequest{})
	if err != nil {
		t.Fatalf("EnrollTotp: %v", err)
	}
	if enrolled.Secret == "" || enrolled.OtpauthUri == "" {
		t.Fatalf("EnrollTotp = %+v, want a non-empty secret and otpauth URI", enrolled)
	}

	code, err := auth.CurrentCode(enrolled.Secret, authSvc.Now())
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := h.ConfirmTotp(sessionCtx, &apiv1.TotpConfirmRequest{Code: code}); err != nil {
		t.Fatalf("ConfirmTotp: %v", err)
	}

	session, err := h.GetCurrentSession(sessionCtx)
	if err != nil {
		t.Fatalf("GetCurrentSession: %v", err)
	}
	if !session.TotpEnrolled {
		t.Error("TotpEnrolled should be true after ConfirmTotp")
	}
}

func TestHandlerAuthOperationsRequirePrincipal(t *testing.T) {
	ctx := context.Background()
	h, _ := newAuthTestHandler(t)

	if _, err := h.GetCurrentSession(ctx); err == nil {
		t.Error("GetCurrentSession with no principal in context should fail")
	}
	if _, err := h.Logout(ctx); err == nil {
		t.Error("Logout with no principal in context should fail")
	}
	if _, err := h.EnrollTotp(ctx, &apiv1.TotpEnrollRequest{}); err == nil {
		t.Error("EnrollTotp with no principal in context should fail")
	}
}

// TestHandlerEnrollTotpReplacingActiveRequiresReverification is the
// handler-level counterpart of AuthService's own test: a bare re-
// enrolment while TOTP is already active must surface as
// totp_reverify_required, not silently succeed.
func TestHandlerEnrollTotpReplacingActiveRequiresReverification(t *testing.T) {
	ctx := context.Background()
	h, authSvc := newAuthTestHandler(t)
	_, token, err := authSvc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	sessionCtx := principalContextForToken(ctx, authSvc, token)

	enrolled, err := h.EnrollTotp(sessionCtx, &apiv1.TotpEnrollRequest{})
	if err != nil {
		t.Fatalf("EnrollTotp: %v", err)
	}
	code, err := auth.CurrentCode(enrolled.Secret, authSvc.Now())
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := h.ConfirmTotp(sessionCtx, &apiv1.TotpConfirmRequest{Code: code}); err != nil {
		t.Fatalf("ConfirmTotp: %v", err)
	}

	_, err = h.EnrollTotp(sessionCtx, &apiv1.TotpEnrollRequest{})
	status := apiError(t, h, err)
	if status.StatusCode != 403 || status.Response.Code != "totp_reverify_required" {
		t.Errorf("bare re-enrolment while TOTP is active = %+v, want 403 totp_reverify_required", status)
	}
}

// TestRoleDenialMapsTo403 is the review finding this issue closes: a role
// mismatch from enforceRole had no *apiError mapping at all, so
// Handler.NewError treated it as an unclassified error — logged
// server-side and reported to the client as an opaque 500 "internal", not
// the 403 a permission refusal should be.
func TestRoleDenialMapsTo403(t *testing.T) {
	h, authSvc := newAuthTestHandler(t)
	ctx := context.Background()

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
	status := apiError(t, h, err)
	if status.StatusCode != 403 || status.Response.Code != "forbidden" {
		t.Errorf("a viewer denied an admin-only operation = %+v, want 403 forbidden", status)
	}
}

// sessionTokenFromCookie extracts the raw session token from a Set-Cookie
// header value built by sessionCookie (auth_handler.go) — the same
// parsing a browser or an http.Client's cookie jar does.
func sessionTokenFromCookie(t *testing.T, setCookie string) string {
	t.Helper()
	header := http.Header{}
	header.Add("Set-Cookie", setCookie)
	resp := http.Response{Header: header}
	for _, c := range resp.Cookies() {
		if c.Name == "hoserva_session" {
			return c.Value
		}
	}
	t.Fatalf("no hoserva_session cookie in %q", setCookie)
	return ""
}

// principalContextForToken re-derives the Principal SecurityHandler would
// attach for token, for tests that call a Handler method directly without
// going through the generated Server's own security dispatch.
func principalContextForToken(ctx context.Context, authSvc *api.AuthService, token string) context.Context {
	sec := &api.SessionSecurityHandler{Auth: authSvc}
	sctx, err := sec.HandleSessionCookie(ctx, apiv1.GetCurrentSessionOperation, apiv1.SessionCookie{APIKey: token})
	if err != nil {
		panic(err)
	}
	return sctx
}
