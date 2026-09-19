package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/auth"
)

// applyPasswordResetTx replaces username's password hash inside txStore.
func (s *AuthService) applyPasswordResetTx(ctx context.Context, txStore *AuthStore, username, password string) (*recoveryOutcome, error) {
	u, err := txStore.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("looking up user: %w", err)
	}
	hash, err := auth.HashPasswordContext(ctx, password)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}
	if err := txStore.UpdatePasswordHash(ctx, u.ID, hash); err != nil {
		return nil, err
	}
	return &recoveryOutcome{
		limiterSubject: "user:" + normalizeUsername(u.Username),
		limiterKind:    auth.SubjectAccount,
	}, nil
}

// applyDisableTotpTx clears username's TOTP enrollment inside txStore.
func (s *AuthService) applyDisableTotpTx(ctx context.Context, txStore *AuthStore, username string) (*recoveryOutcome, error) {
	u, err := txStore.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("looking up user: %w", err)
	}
	if err := txStore.ClearUserTOTP(ctx, u.ID); err != nil {
		return nil, err
	}
	return nil, nil
}

// applyUnlockTx verifies username exists and returns the limiter subject to
// clear after the recovery transaction commits.
func (s *AuthService) applyUnlockTx(ctx context.Context, txStore *AuthStore, username string) (*recoveryOutcome, error) {
	u, err := txStore.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUserNotFound
		}
		return nil, fmt.Errorf("looking up user: %w", err)
	}
	return &recoveryOutcome{
		limiterSubject: "user:" + normalizeUsername(u.Username),
		limiterKind:    auth.SubjectAccount,
	}, nil
}
