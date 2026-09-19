package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mdg-labs/hoserva/internal/auth"
)

// ResetUserPassword replaces username's password (Q78).
func (s *AuthService) ResetUserPassword(ctx context.Context, username, password string) error {
	u, err := s.Store.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errUserNotFound
		}
		return fmt.Errorf("looking up user: %w", err)
	}
	hash, err := auth.HashPasswordContext(ctx, password)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	if err := s.Store.UpdatePasswordHash(ctx, u.ID, hash); err != nil {
		return err
	}
	s.Limiter.Clear("user:"+normalizeUsername(u.Username), auth.SubjectAccount)
	return nil
}

// DisableUserTotp clears username's TOTP enrollment (Q78).
func (s *AuthService) DisableUserTotp(ctx context.Context, username string) error {
	u, err := s.Store.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errUserNotFound
		}
		return fmt.Errorf("looking up user: %w", err)
	}
	return s.Store.ClearUserTOTP(ctx, u.ID)
}

// UnlockUser clears username's login rate-limiter lockout (Q78).
func (s *AuthService) UnlockUser(ctx context.Context, username string) error {
	u, err := s.Store.GetUserByUsername(ctx, normalizeUsername(username))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errUserNotFound
		}
		return fmt.Errorf("looking up user: %w", err)
	}
	s.Limiter.Clear("user:"+normalizeUsername(u.Username), auth.SubjectAccount)
	return nil
}
