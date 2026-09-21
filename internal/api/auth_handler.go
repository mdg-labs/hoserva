package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/store"
)

// mapAuthError classifies every sentinel AuthService, SessionSecurityHandler
// and enforceRole can return into the spec's shared Error schema
// (doc 01 §5) — the same role Handler.NewError's mapStoreError/
// mapSchedulerError already play for the job system, extended to cover
// errors that can also originate from HandleSessionCookie itself (which
// runs, and can fail, before any Handler method is ever called).
func mapAuthError(err error) error {
	switch {
	case errors.Is(err, ErrSetupComplete):
		return &apiError{code: "setup_complete", statusCode: 409, message: "an admin account already exists"}
	case errors.Is(err, ErrInvalidCredentials):
		return &apiError{code: "invalid_credentials", statusCode: 401, message: "invalid username or password"}
	case errors.Is(err, ErrTOTPRequired):
		return &apiError{code: "totp_required", statusCode: 401, message: "a TOTP code is required"}
	case errors.Is(err, ErrTOTPInvalid):
		return &apiError{code: "totp_invalid", statusCode: 401, message: "invalid TOTP code"}
	case errors.Is(err, ErrTOTPNotPending):
		return &apiError{code: "totp_not_pending", statusCode: 409, message: "no pending TOTP enrolment to confirm"}
	case errors.Is(err, ErrTOTPReverifyRequired):
		// 403, not 401: a bare session cookie is still a valid,
		// unexpired credential here — what's missing is proof the caller
		// still holds the account (password or a current TOTP code), not
		// authentication itself, so "unauthorized" would read to a
		// client as "your session expired", which it hasn't.
		return &apiError{code: "totp_reverify_required", statusCode: 403, message: "re-enrolling an active TOTP credential requires the current password or a current TOTP code"}
	case errors.Is(err, ErrTOTPReverifyAmbiguous):
		// 400, not 403: the session is valid and proof was supplied — the
		// request itself is what's wrong, by supplying two guesses where
		// exactly one is accepted.
		return &apiError{code: "totp_reverify_ambiguous", statusCode: 400, message: "supply exactly one of password or code when re-enrolling an active TOTP credential, not both"}
	case errors.Is(err, ErrShareOnlyNoLogin):
		return &apiError{code: "share_only_no_login", statusCode: 403, message: "this account has SMB/NFS access only — it has no UI login"}
	case errors.Is(err, ErrUserExists):
		return &apiError{code: "user_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, ErrUserNotFound):
		return &apiError{code: "user_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, ErrInvalidRole):
		return &apiError{code: "invalid_role", statusCode: 400, message: err.Error()}
	case errors.Is(err, ErrCannotModifyAdmin):
		return &apiError{code: "cannot_modify_admin", statusCode: 403, message: err.Error()}
	case errors.Is(err, ErrGroupExists):
		return &apiError{code: "group_exists", statusCode: 409, message: err.Error()}
	case errors.Is(err, ErrGroupNotFound):
		return &apiError{code: "group_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, ErrSessionNotFound):
		return &apiError{code: "session_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, ErrSambaPasswordFailed):
		return &apiError{code: "samba_password_failed", statusCode: 502, message: err.Error()}
	case errors.Is(err, ErrSambaDeleteFailed):
		return &apiError{code: "samba_delete_failed", statusCode: 502, message: err.Error()}
	case errors.Is(err, ErrInvalidAccessLevel):
		return &apiError{code: "invalid_access_level", statusCode: 400, message: err.Error()}
	case errors.Is(err, ErrDuplicateGrant):
		return &apiError{code: "duplicate_grant", statusCode: 400, message: err.Error()}
	case errors.Is(err, store.ErrShareNotFound):
		// Same code and status share_handler.go's own mapShareError uses
		// for the same sentinel — the permission endpoints reach this
		// through AuthService rather than internal/share.Service, but a
		// bad share name should read identically either way.
		return &apiError{code: "share_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, ErrSessionInvalid):
		return &apiError{code: "unauthorized", statusCode: 401, message: "invalid or expired session"}
	case errors.Is(err, ErrAPITokenInvalid):
		return &apiError{code: "unauthorized", statusCode: 401, message: "invalid or revoked api token"}
	case errors.Is(err, ErrAPITokenNotFound):
		return &apiError{code: "api_token_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, ErrShareOnlyNoAPIToken):
		return &apiError{code: "share_only_no_api_token", statusCode: 403, message: err.Error()}
	case errors.Is(err, ErrInvalidTokenRole):
		return &apiError{code: "invalid_token_role", statusCode: 400, message: err.Error()}
	case errors.Is(err, ErrTokenRoleExceedsAccount):
		return &apiError{code: "token_role_exceeds_account", statusCode: 403, message: err.Error()}
	case errors.Is(err, ogenerrors.ErrSecurityRequirementIsNotSatisfied):
		return &apiError{code: "unauthorized", statusCode: 401, message: "this request requires a credential"}
	case errors.Is(err, auth.ErrPasswordWorkBusy), errors.Is(err, context.DeadlineExceeded):
		return &apiError{code: "too_busy", statusCode: 503, message: "too many concurrent password operations — try again shortly"}
	case errors.Is(err, ErrRoleDenied):
		return &apiError{code: "forbidden", statusCode: 403, message: "the current account does not have permission for this operation"}
	}
	var lockout *LockoutError
	if errors.As(err, &lockout) {
		return &apiError{code: "rate_limited", statusCode: 429, message: lockout.Error()}
	}
	return err
}

// requirePrincipal returns the calling user's Principal, or an
// unauthorized apiError if none is attached — this should never happen
// for an operation whose x-hoserva-role required a credential (security
// already refused the request first), so reaching this branch is itself
// a bug, not a normal client-facing case; failing closed here rather than
// panicking still keeps that bug from ever becoming a privilege check
// silently skipped.
func requirePrincipal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return Principal{}, &apiError{code: "unauthorized", statusCode: 401, message: "this request requires a credential"}
	}
	return p, nil
}

func userToAPI(u *User) apiv1.User {
	return apiv1.User{
		ID:           uuid.MustParse(u.ID),
		Username:     u.Username,
		Role:         apiv1.UserRole(u.Role),
		TotpEnrolled: u.TOTPEnrolled(),
	}
}

// sessionCookie builds the hoserva_session Set-Cookie value (doc 01 §7):
// HttpOnly, Secure and SameSite=Strict, with an expiry — TLS-only :8008
// (Q9) means "Secure" never blocks the cookie being stored in practice.
func sessionCookie(token string, expiresAt time.Time) string {
	c := &http.Cookie{
		Name:     "hoserva_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		Expires:  expiresAt,
	}
	return c.String()
}

// clearedSessionCookie immediately expires the session cookie (logout).
func clearedSessionCookie() string {
	c := &http.Cookie{
		Name:     "hoserva_session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
	return c.String()
}

func (h *Handler) GetSetupStatus(ctx context.Context) (*apiv1.SetupStatus, error) {
	n, err := h.Auth.Store.CountAdmins(ctx)
	if err != nil {
		return nil, err
	}
	return &apiv1.SetupStatus{AdminExists: n > 0}, nil
}

func (h *Handler) CreateFirstAdmin(ctx context.Context, req *apiv1.CreateFirstAdminRequest) (*apiv1.UserHeaders, error) {
	u, token, err := h.Auth.CreateFirstAdmin(ctx, req.Username, req.Password)
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := &apiv1.UserHeaders{Response: userToAPI(u)}
	out.SetCookie.SetTo(sessionCookie(token, h.Auth.Now().Add(sessionTTL)))
	return out, nil
}

func (h *Handler) Login(ctx context.Context, req *apiv1.LoginRequest) (*apiv1.UserHeaders, error) {
	totpCode, _ := req.TotpCode.Get()
	u, token, err := h.Auth.Login(ctx, req.Username, req.Password, totpCode, sourceAddrFromContext(ctx))
	if err != nil {
		return nil, mapAuthError(err)
	}
	out := &apiv1.UserHeaders{Response: userToAPI(u)}
	out.SetCookie.SetTo(sessionCookie(token, h.Auth.Now().Add(sessionTTL)))
	return out, nil
}

func (h *Handler) Logout(ctx context.Context) (*apiv1.LogoutNoContent, error) {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if principal.SessionTokenHash != "" {
		if err := h.Auth.LogoutByTokenHash(ctx, principal.SessionTokenHash); err != nil {
			return nil, err
		}
	}
	out := &apiv1.LogoutNoContent{}
	out.SetCookie.SetTo(clearedSessionCookie())
	return out, nil
}

func (h *Handler) GetCurrentSession(ctx context.Context) (*apiv1.User, error) {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if principal.Local {
		// The Unix socket's peer-credential-trusted identity has no
		// users row at all (Q44) — it is not a signed-in user, it's
		// root-equivalent local access. There is nothing meaningful to
		// return as "the current session's user" for it.
		return nil, &apiError{code: "no_session", statusCode: 404, message: "the Unix socket has no session — it authenticates by peer credentials, not a signed-in user"}
	}
	u, err := h.Auth.Store.GetUserByID(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	out := userToAPI(u)
	return &out, nil
}

func (h *Handler) EnrollTotp(ctx context.Context, req *apiv1.TotpEnrollRequest) (*apiv1.TotpEnrollResponse, error) {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	if principal.Local {
		return nil, &apiError{code: "no_session", statusCode: 404, message: "the Unix socket has no user account to enroll TOTP for"}
	}
	password, _ := req.Password.Get()
	code, _ := req.Code.Get()
	secret, otpauthURI, err := h.Auth.EnrollTOTP(ctx, principal.UserID, password, code)
	if err != nil {
		return nil, mapAuthError(err)
	}
	return &apiv1.TotpEnrollResponse{Secret: secret, OtpauthUri: otpauthURI}, nil
}

func (h *Handler) ConfirmTotp(ctx context.Context, req *apiv1.TotpConfirmRequest) error {
	principal, err := requirePrincipal(ctx)
	if err != nil {
		return err
	}
	if principal.Local {
		return &apiError{code: "no_session", statusCode: 404, message: "the Unix socket has no user account to confirm TOTP for"}
	}
	if err := h.Auth.ConfirmTOTP(ctx, principal.UserID, req.Code, principal.SessionTokenHash); err != nil {
		return mapAuthError(err)
	}
	return nil
}
