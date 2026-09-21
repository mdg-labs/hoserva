package api

import (
	"context"
	"errors"
	"fmt"
	"log"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/auth"
)

// ErrRoleDenied is enforceRole's sentinel for a principal whose role
// doesn't satisfy an operation's declared x-hoserva-role — mapAuthError
// classifies it as 403, not the 500 "internal" a plain, unclassified error
// would otherwise fall through to (Handler.NewError logs anything it
// can't classify and reports it as an opaque internal error).
var ErrRoleDenied = errors.New("the caller's role does not satisfy this operation's required role")

// hashSessionCookie hashes a raw session cookie value exactly as
// AuthService.createSession/ValidateSession do, so Principal carries only
// the hash — never the raw secret — for Logout to revoke by.
func hashSessionCookie(raw string) string {
	if raw == "" {
		return ""
	}
	return auth.HashSessionToken(raw)
}

// enforceRole refuses a request whose principal role doesn't satisfy
// operation's declared x-hoserva-role, and fails closed — refuses,
// not allows — when operation has no entry at all (D18, doc 01 §5).
func enforceRole(operation apiv1.OperationName, role Role) error {
	required, ok := RoleFor(operation)
	if !ok {
		return fmt.Errorf("operation %s has no declared role — refusing (fail closed)", operation)
	}
	if !role.Satisfies(required) {
		return fmt.Errorf("%w: operation %s requires role %q, principal has %q", ErrRoleDenied, operation, required, role)
	}
	return nil
}

// SessionSecurityHandler implements apiv1.SecurityHandler for the TCP
// transport (doc 01 §5): real session cookies against AuthService, and
// personal API tokens (Q43, #50) for scripts and the remote CLI. It never
// treats any particular cookie or header value as automatically trusted;
// TrustedSecurityHandler below is the one place that shortcut exists, and
// it is wired only to the Unix-socket listener.
type SessionSecurityHandler struct {
	Auth *AuthService
}

var _ apiv1.SecurityHandler = (*SessionSecurityHandler)(nil)

// HandleApiToken authenticates a bearer personal API token (Q43): looked
// up fresh against api_tokens.token_hash on every call (no caching, so
// revocation takes effect immediately), capped to its owning account's
// current role (effectiveTokenRole), then recorded in the audit log
// (#50's own acceptance criteria) — best-effort, since an audit-sink
// hiccup must never fail the caller's own request (RecordAPITokenUse's
// own doc comment).
func (h *SessionSecurityHandler) HandleApiToken(ctx context.Context, operationName apiv1.OperationName, t apiv1.ApiToken) (context.Context, error) {
	tok, u, err := h.Auth.ValidateAPIToken(ctx, t.Token)
	if err != nil {
		return ctx, err
	}
	role := effectiveTokenRole(u.Role, tok.Role)
	if err := enforceRole(operationName, role); err != nil {
		return ctx, err
	}
	if err := h.Auth.RecordAPITokenUse(ctx, tok, u, string(operationName)); err != nil {
		log.Printf("hoservad: recording api token use: %v", err)
	}
	principal := Principal{
		UserID:   u.ID,
		Username: u.Username,
		Role:     role,
	}
	return withPrincipal(ctx, principal), nil
}

func (h *SessionSecurityHandler) HandleSessionCookie(ctx context.Context, operationName apiv1.OperationName, t apiv1.SessionCookie) (context.Context, error) {
	u, err := h.Auth.ValidateSession(ctx, t.APIKey)
	if err != nil {
		return ctx, err
	}
	role := Role(u.Role)
	if err := enforceRole(operationName, role); err != nil {
		return ctx, err
	}
	principal := Principal{
		UserID:           u.ID,
		Username:         u.Username,
		Role:             role,
		SessionTokenHash: hashSessionCookie(t.APIKey),
	}
	return withPrincipal(ctx, principal), nil
}

// TrustedSecurityHandler implements apiv1.SecurityHandler for the Unix
// socket only (Q44, doc 01 §5): the socket carries no bearer or cookie
// credential of its own — "there is no auth token required because the
// kernel already vouches for the caller's identity via SO_PEERCRED". By
// the time a request reaches this handler at all, cmd/hoservad's own
// connection-level middleware has already confirmed the peer is uid 0,
// this daemon's own uid (Q44's implementation note: a non-root dev run
// has no other identity it could ever vouch for, since CLAUDE.md forbids
// creating the hoserva group from a dev/agent session), or a member of
// the hoserva group (root-equivalent, exactly like the docker group) and
// attached a synthetic credential purely so ogen's generated per-scheme
// presence check has something to call this handler with — this handler
// grants every operation admin access, because by construction it is
// never reachable from anywhere but that already-authorized connection.
// It still runs every operation through enforceRole, exactly like
// SessionSecurityHandler does: RoleAdmin satisfies every declared
// operation today, but an operation with no entry at all in the role map
// must fail closed on this transport too, not only on TCP. It must never
// be wired to the TCP listener.
type TrustedSecurityHandler struct{}

var _ apiv1.SecurityHandler = TrustedSecurityHandler{}

func (TrustedSecurityHandler) HandleApiToken(ctx context.Context, operationName apiv1.OperationName, t apiv1.ApiToken) (context.Context, error) {
	if err := enforceRole(operationName, RoleAdmin); err != nil {
		return ctx, err
	}
	return withPrincipal(ctx, Principal{Role: RoleAdmin, Local: true}), nil
}

func (TrustedSecurityHandler) HandleSessionCookie(ctx context.Context, operationName apiv1.OperationName, t apiv1.SessionCookie) (context.Context, error) {
	if err := enforceRole(operationName, RoleAdmin); err != nil {
		return ctx, err
	}
	return withPrincipal(ctx, Principal{Role: RoleAdmin, Local: true}), nil
}

// UnixSocketCredentialHeader is the header cmd/hoservad's own
// peer-credential middleware sets on every request it forwards from the
// Unix listener, purely so ogen's generated apiToken security check sees
// a credential present and calls TrustedSecurityHandler at all (it is
// otherwise never invoked for a request that presents neither a cookie
// nor an Authorization header). TrustedSecurityHandler ignores this
// header's value entirely — the middleware setting it is only ever
// reached after the connection's SO_PEERCRED already passed.
const UnixSocketCredentialHeader = "Authorization"

// UnixSocketCredentialValue is the fixed value cmd/hoservad's middleware
// sets for UnixSocketCredentialHeader.
const UnixSocketCredentialValue = "Bearer peer-credentials-trusted"
