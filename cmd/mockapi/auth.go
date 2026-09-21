package main

import (
	"context"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// mockAdminID is a fixed id for the mock's single canned account (doc 06
// §8): the mock has no real users table, so every scenario but
// fresh-install reports this same admin, already signed in.
var mockAdminID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func mockUser() apiv1.User {
	return apiv1.User{
		ID:           mockAdminID,
		Username:     "admin",
		Role:         apiv1.UserRoleAdmin,
		TotpEnrolled: false,
	}
}

// mockUserSummary is the #49 /users list-shape twin of mockUser — the
// mock's single canned admin account, as it appears in ListUsers rather
// than in a login response.
func mockUserSummary() apiv1.UserSummary {
	return apiv1.UserSummary{
		ID:           mockAdminID,
		Username:     "admin",
		Role:         apiv1.UserRoleAdmin,
		TotpEnrolled: false,
		CreatedAt:    time.Unix(0, 0).UTC(),
	}
}

// mockSessionCookie and mockClearedSessionCookie are fixed Set-Cookie
// values — the mock never validates a session (doc 06 §8's "any
// credential accepted"), so there is nothing scenario-specific about
// them, and no real token to generate.
const (
	mockSessionCookie        = "hoserva_session=mock-session-token; Path=/; HttpOnly; Secure; SameSite=Strict"
	mockClearedSessionCookie = "hoserva_session=; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=0"
)

// GetSetupStatus reports fresh-install as the one scenario with no admin
// account yet — the others all represent an already-configured system
// (doc 06 §8's scenario list).
func (h *handler) GetSetupStatus(ctx context.Context) (*apiv1.SetupStatus, error) {
	return &apiv1.SetupStatus{AdminExists: h.scenario != "fresh-install"}, nil
}

func (h *handler) CreateFirstAdmin(ctx context.Context, req *apiv1.CreateFirstAdminRequest) (*apiv1.UserHeaders, error) {
	if h.scenario != "fresh-install" {
		return nil, &mockError{code: "setup_complete", statusCode: 409, message: "an admin account already exists"}
	}
	out := &apiv1.UserHeaders{Response: mockUser()}
	out.SetCookie.SetTo(mockSessionCookie)
	return out, nil
}

func (h *handler) Login(ctx context.Context, req *apiv1.LoginRequest) (*apiv1.UserHeaders, error) {
	out := &apiv1.UserHeaders{Response: mockUser()}
	out.SetCookie.SetTo(mockSessionCookie)
	return out, nil
}

func (h *handler) Logout(ctx context.Context) (*apiv1.LogoutNoContent, error) {
	out := &apiv1.LogoutNoContent{}
	out.SetCookie.SetTo(mockClearedSessionCookie)
	return out, nil
}

func (h *handler) GetCurrentSession(ctx context.Context) (*apiv1.User, error) {
	u := mockUser()
	return &u, nil
}

// mockTotpSecret is a fixed, valid-looking base32 secret — the mock never
// validates a TOTP code either, so nothing needs to compute a real one
// against it.
const mockTotpSecret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"

func (h *handler) EnrollTotp(ctx context.Context, req *apiv1.TotpEnrollRequest) (*apiv1.TotpEnrollResponse, error) {
	return &apiv1.TotpEnrollResponse{
		Secret:     mockTotpSecret,
		OtpauthUri: "otpauth://totp/Hoserva:admin?secret=" + mockTotpSecret + "&issuer=Hoserva&algorithm=SHA1&digits=6&period=30",
	}, nil
}

func (h *handler) ConfirmTotp(ctx context.Context, req *apiv1.TotpConfirmRequest) error {
	return nil
}
