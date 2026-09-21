package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/auth"
)

var (
	// ErrUserExists is CreateUser's refusal of a duplicate username.
	ErrUserExists = errors.New("a user with that name already exists")
	// ErrUserNotFound is UpdateUserRole/DeleteUser/SetUserPassword/the
	// permission and group-membership operations naming an unknown user.
	ErrUserNotFound = errors.New("no such user")
	// ErrInvalidRole is CreateUser/UpdateUser naming a role other than
	// viewer or share-only.
	ErrInvalidRole = errors.New("role must be viewer or share-only")
	// ErrCannotModifyAdmin is UpdateUserRole/DeleteUser refusing to touch
	// the sole admin account (users_one_admin_idx, doc 13 Q27's scope
	// note): this issue manages viewer and share-only accounts, never the
	// admin row createFirstAdmin created.
	ErrCannotModifyAdmin = errors.New("the sole admin account cannot be modified through this operation")
	// ErrSambaPasswordFailed wraps a SambaAccounts.SetPassword failure
	// inside SetUserPassword, once the UI credential has already been
	// rolled back to its previous value.
	ErrSambaPasswordFailed = errors.New("writing the Samba account failed — the UI credential was rolled back")
	// ErrSambaDeleteFailed wraps a SambaAccounts.Delete failure inside
	// DeleteUser: the whole delete is rolled back with it, so the user is
	// not left half-deleted (DB row gone, Samba account still active).
	ErrSambaDeleteFailed = errors.New("removing the Samba account failed — the user was not deleted")
)

// validateManagedRole rejects anything but viewer or share-only —
// createUser/updateUser never create or reassign the admin role (single
// admin is enforced by users_one_admin_idx; this issue does not extend
// it). Empty defaults to share-only (Q27: the default for a new account).
func validateManagedRole(role string) (string, error) {
	switch role {
	case "":
		return roleShareOnly, nil
	case roleViewer, roleShareOnly:
		return role, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
}

// CreateUser creates a viewer or share-only account with no usable
// password yet (Q27) — setUserPassword is the separate action that gives
// it one, on both the UI and Samba side together. The placeholder hash
// written here is a random value nobody knows, so the account has no
// working credential until a real password is set, mirroring "no default
// credential ever exists" (doc 01 §7).
func (s *AuthService) CreateUser(ctx context.Context, username, role string) (*User, error) {
	role, err := validateManagedRole(role)
	if err != nil {
		return nil, err
	}
	placeholder, err := auth.HashPasswordContext(ctx, uuid.NewString())
	if err != nil {
		return nil, fmt.Errorf("hashing placeholder password: %w", err)
	}
	u := &User{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: placeholder,
		Role:         role,
		TOTPLastStep: 0,
		CreatedAt:    s.Now(),
	}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrUserExists
		}
		return nil, fmt.Errorf("creating user: %w", err)
	}
	return u, nil
}

// ListUsers returns every account, sorted by username.
func (s *AuthService) ListUsers(ctx context.Context) ([]*User, error) {
	return s.Store.ListUsers(ctx)
}

// UpdateUserRole changes userID's role to viewer or share-only. It
// refuses (ErrCannotModifyAdmin) when the target account is the sole
// admin.
func (s *AuthService) UpdateUserRole(ctx context.Context, userID, role string) (*User, error) {
	role, err := validateManagedRole(role)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateUserRole(ctx, userID, role); err != nil {
		return nil, err
	}
	return s.Store.GetUserByID(ctx, userID)
}

// DeleteUser removes the account, its sessions, its group memberships,
// its per-share permissions and its Samba passdb entry, if it has one —
// SambaAccounts.Delete is a safe no-op for a user who never had a
// password set (#49). It refuses (ErrCannotModifyAdmin) the sole admin
// account. If removing the Samba account fails, the whole delete is
// rolled back and nothing is removed (ErrSambaDeleteFailed), rather than
// leaving a half-deleted user.
func (s *AuthService) DeleteUser(ctx context.Context, userID string) error {
	return s.Store.DeleteUser(ctx, userID, func(ctx context.Context, username string) error {
		if err := s.SambaAccounts.Delete(ctx, username); err != nil {
			return fmt.Errorf("%w: %v", ErrSambaDeleteFailed, err)
		}
		return nil
	})
}

// SetUserPassword writes the UI credential hash and the Samba passdb
// entry together (Q27, doc 03 §7): the UI hash is written first (a
// reliable, purely local write), then the Samba account — the step far
// more likely to fail, since it execs an external binary. If that Samba
// write fails, the UI hash is rolled back to its previous value before
// the error is returned, so a failure never leaves the two credentials
// disagreeing about what the current password is.
//
// It refuses (ErrCannotModifyAdmin) an admin-role target, the same as
// UpdateUserRole/DeleteUser: this is an ordinary admin-API operation, not
// the uid-0-gated recovery path (Q78), and TrustedSecurityHandler grants
// RoleAdmin to every Unix-socket caller unixSocketAuthMiddleware admits —
// not only uid 0. The sole admin's own password can only be reset by the
// root-only `hoserva user reset-password` command.
func (s *AuthService) SetUserPassword(ctx context.Context, userID, password string) (*User, error) {
	u, err := s.Store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("looking up user: %w", err)
	}
	if u.Role == roleAdmin {
		return nil, ErrCannotModifyAdmin
	}
	oldHash := u.PasswordHash
	firstSMBCredential := u.SMBCredentialSetAt == nil

	newHash, err := auth.HashPasswordContext(ctx, password)
	if err != nil {
		return nil, fmt.Errorf("hashing password: %w", err)
	}

	// The password hash and (the first time) the smb_credential_set_at
	// marker are written together, before the Samba call: that closes the
	// window a separate, later marker write left open, where a
	// marker-write failure after a successful Samba write could leave a
	// fully, correctly changed password looking unprovisioned
	// (HasSMBCredential false) (#225 CodeRabbit finding on PR #228).
	var setAt time.Time
	if firstSMBCredential {
		setAt = s.Now()
		if err := s.Store.UpdatePasswordHashAndMarkSMBCredential(ctx, userID, newHash, setAt); err != nil {
			return nil, fmt.Errorf("writing password: %w", err)
		}
	} else if err := s.Store.UpdatePasswordHash(ctx, userID, newHash); err != nil {
		return nil, fmt.Errorf("writing password: %w", err)
	}

	if err := s.SambaAccounts.SetPassword(ctx, u.Username, password); err != nil {
		var rbErr error
		if firstSMBCredential {
			rbErr = s.Store.RestorePasswordHashAndClearSMBCredential(ctx, userID, oldHash)
		} else {
			rbErr = s.Store.UpdatePasswordHash(ctx, userID, oldHash)
		}
		if rbErr != nil {
			return nil, fmt.Errorf("samba password write failed (%v), and rolling back the UI credential also failed: %w", err, rbErr)
		}
		return nil, fmt.Errorf("%w: %v", ErrSambaPasswordFailed, err)
	}

	u.PasswordHash = newHash
	if firstSMBCredential {
		u.SMBCredentialSetAt = &setAt
	}
	return u, nil
}
