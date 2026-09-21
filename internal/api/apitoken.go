package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/auth"
)

var (
	// ErrAPITokenNotFound is RevokeAPIToken naming an id no token has.
	ErrAPITokenNotFound = errors.New("no such api token")
	// ErrAPITokenInvalid is ValidateAPIToken's refusal of a missing,
	// unknown or revoked token — deliberately one error for all three,
	// mirroring ErrSessionInvalid.
	ErrAPITokenInvalid = errors.New("invalid or revoked api token")
	// ErrShareOnlyNoAPIToken is CreateAPIToken's refusal of a share-only
	// target account: it has no API access at all (Q27, doc 01 §5),
	// mirroring ErrShareOnlyNoLogin.
	ErrShareOnlyNoAPIToken = errors.New("this account has SMB/NFS access only — it has no API access")
	// ErrInvalidTokenRole is CreateAPIToken naming a role other than
	// admin or viewer.
	ErrInvalidTokenRole = errors.New("a token's role must be admin or viewer")
	// ErrTokenRoleExceedsAccount is CreateAPIToken naming a role the
	// target account does not itself have — a token can only narrow an
	// account's own access, never widen it.
	ErrTokenRoleExceedsAccount = errors.New("a token's role cannot exceed its account's own role")
)

// validateTokenRole rejects anything but admin or viewer (Q43) — never
// share-only, which ErrShareOnlyNoAPIToken refuses outright regardless of
// the role requested.
func validateTokenRole(role string) (Role, error) {
	switch role {
	case string(RoleAdmin), string(RoleViewer):
		return Role(role), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidTokenRole, role)
	}
}

// CreateAPIToken issues a personal API token (Q43) for username, scoped
// to role — the raw value is returned only here, once, and never stored
// again: only its SHA-256 (auth.NewAPIToken) persists, mirroring how a
// session cookie is handled (doc 01 §7). Refuses a share-only target
// account (ErrShareOnlyNoAPIToken, Q27) and a role the account does not
// itself have (ErrTokenRoleExceedsAccount): an admin account can be
// issued a viewer-scoped token to narrow a script's own blast radius, a
// viewer account can never be issued an admin-scoped one. The returned
// *User is the owning account, normalized (GetUserByUsername) — the
// caller uses its Username rather than echoing back whatever case the
// request happened to type.
func (s *AuthService) CreateAPIToken(ctx context.Context, username, name, role string) (*APIToken, *User, string, error) {
	tokenRole, err := validateTokenRole(role)
	if err != nil {
		return nil, nil, "", err
	}
	u, err := s.Store.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, "", ErrUserNotFound
		}
		return nil, nil, "", fmt.Errorf("looking up user: %w", err)
	}
	if u.Role == roleShareOnly {
		return nil, nil, "", ErrShareOnlyNoAPIToken
	}
	if !Role(u.Role).Satisfies(tokenRole) {
		return nil, nil, "", ErrTokenRoleExceedsAccount
	}

	raw, hash, err := auth.NewAPIToken()
	if err != nil {
		return nil, nil, "", err
	}
	t := &APIToken{
		TokenHash: hash,
		UserID:    u.ID,
		Name:      name,
		Role:      string(tokenRole),
		CreatedAt: s.Now(),
	}
	if err := s.Store.CreateAPIToken(ctx, t); err != nil {
		return nil, nil, "", fmt.Errorf("creating api token: %w", err)
	}
	return t, u, raw, nil
}

// ListAPITokens returns every account's tokens, most recently created
// first.
func (s *AuthService) ListAPITokens(ctx context.Context) ([]APITokenWithUser, error) {
	return s.Store.ListAPITokensWithUsernames(ctx)
}

// RevokeAPIToken ends one token immediately: ValidateAPIToken looks the
// token up fresh on every request (no caching), so a deleted row stops
// authenticating the instant this call returns.
func (s *AuthService) RevokeAPIToken(ctx context.Context, id string) error {
	return s.Store.DeleteAPIToken(ctx, id)
}

// ValidateAPIToken returns the token and its owning account for a raw
// bearer value, or ErrAPITokenInvalid for a missing, unknown or revoked
// one.
func (s *AuthService) ValidateAPIToken(ctx context.Context, rawToken string) (*APIToken, *User, error) {
	if rawToken == "" {
		return nil, nil, ErrAPITokenInvalid
	}
	t, err := s.Store.GetAPITokenByHash(ctx, auth.HashAPIToken(rawToken))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrAPITokenInvalid
		}
		return nil, nil, fmt.Errorf("looking up api token: %w", err)
	}
	u, err := s.Store.GetUserByID(ctx, t.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrAPITokenInvalid
		}
		return nil, nil, fmt.Errorf("looking up api token's user: %w", err)
	}
	return t, u, nil
}

// RecordAPITokenUse writes one audit-log entry for a request an API
// token just authenticated — extending doc 01 §7's audit log to cover
// token use specifically, per #50's acceptance criteria. Errors are
// returned, not swallowed, so the caller (SessionSecurityHandler) decides
// its own policy; it logs and continues rather than failing the request,
// mirroring metrics_handler.go's own best-effort logging — an audit-sink
// hiccup must never turn into every scripted request failing.
func (s *AuthService) RecordAPITokenUse(ctx context.Context, t *APIToken, u *User, operation string) error {
	detail := fmt.Sprintf("token %q (%s) used by %q for %s", t.Name, t.Role, u.Username, operation)
	return s.Store.InsertAuditLog(ctx, u.Username, "api_token_used", detail, s.Now())
}
