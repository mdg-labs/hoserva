package api

import (
	"context"
	"fmt"
	"time"

	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// APIToken is an api_tokens table row (#50, Q43), translated out of
// storedb's generated shape — mirrors Session's own split from
// sessions_admin.go.
type APIToken struct {
	TokenHash string
	UserID    string
	Name      string
	Role      string // "admin" or "viewer" (Q43) — never "share-only".
	CreatedAt time.Time
}

// APITokenWithUser is one api_tokens row joined with its account's
// username, for the admin-facing token list (doc 03 §7) — mirrors
// SessionWithUser.
type APITokenWithUser struct {
	TokenHash string
	UserID    string
	Username  string
	Name      string
	Role      string
	CreatedAt time.Time
}

// CreateAPIToken inserts t as a new row.
func (s *AuthStore) CreateAPIToken(ctx context.Context, t *APIToken) error {
	return s.q.CreateApiToken(ctx, storedb.CreateApiTokenParams{
		TokenHash: t.TokenHash,
		UserID:    t.UserID,
		Name:      t.Name,
		Role:      t.Role,
		CreatedAt: t.CreatedAt.UTC().Format(timeFormat),
	})
}

// GetAPITokenByHash returns sql.ErrNoRows when no such token exists.
func (s *AuthStore) GetAPITokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	row, err := s.q.GetApiTokenByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	return apiTokenFromRow(row)
}

// DeleteAPIToken revokes one token by its hash (its public id — never the
// raw token). Reports ErrAPITokenNotFound when no token has that id.
func (s *AuthStore) DeleteAPIToken(ctx context.Context, id string) error {
	n, err := s.q.DeleteApiToken(ctx, id)
	if err != nil {
		return fmt.Errorf("revoking api token: %w", err)
	}
	if n == 0 {
		return ErrAPITokenNotFound
	}
	return nil
}

// ListAPITokensWithUsernames joins api_tokens with users for the admin
// token list, most recently created first — mirrors
// ListSessionsWithUsernames.
func (s *AuthStore) ListAPITokensWithUsernames(ctx context.Context) ([]APITokenWithUser, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT t.token_hash, t.user_id, u.username, t.name, t.role, t.created_at
FROM api_tokens t
JOIN users u ON u.id = t.user_id
ORDER BY t.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing api tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []APITokenWithUser
	for rows.Next() {
		var t APITokenWithUser
		var createdAt string
		if err := rows.Scan(&t.TokenHash, &t.UserID, &t.Username, &t.Name, &t.Role, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning api token row: %w", err)
		}
		ct, err := time.Parse(timeFormat, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parsing api token created_at: %w", err)
		}
		t.CreatedAt = ct
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing api tokens: %w", err)
	}
	return out, nil
}

func apiTokenFromRow(row *storedb.ApiToken) (*APIToken, error) {
	createdAt, err := time.Parse(timeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing api token %s created_at: %w", row.TokenHash, err)
	}
	return &APIToken{
		TokenHash: row.TokenHash,
		UserID:    row.UserID,
		Name:      row.Name,
		Role:      row.Role,
		CreatedAt: createdAt,
	}, nil
}
