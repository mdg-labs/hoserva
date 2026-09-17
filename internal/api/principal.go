package api

import "context"

// Principal is the authenticated identity behind a request (D18): a real
// user (over TCP, once SessionSecurityHandler validates a session or,
// later, an API token — Q43), or the fixed local identity
// TrustedSecurityHandler grants once cmd/hoservad's own peer-credential
// check has already authorized the Unix-socket connection (Q44) — the
// group is documented as root-equivalent, so that identity is always
// RoleAdmin.
type Principal struct {
	UserID   string
	Username string
	Role     Role
	// Local is true only for the Unix-socket, peer-credential-trusted
	// identity — never for a real signed-in user, even one with the admin
	// role.
	Local bool
	// SessionTokenHash is the hash of the raw session cookie value that
	// authenticated this request (empty for Local, and for the
	// not-yet-implemented apiToken scheme) — the generated Logout
	// operation takes no parameters of its own (D18: it's a bare
	// POST /auth/logout), so this is how the handler learns which
	// session to revoke without re-parsing the request.
	SessionTokenHash string
}

type principalContextKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, p)
}

// PrincipalFromContext returns the request's authenticated identity, set
// by SessionSecurityHandler or TrustedSecurityHandler once security
// passed. Handler methods use it to know who is making the call (e.g.
// GetCurrentSession, EnrollTotp, ConfirmTotp all act on "the signed-in
// user", never a caller-supplied id).
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalContextKey{}).(Principal)
	return p, ok
}
