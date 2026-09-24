package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mdg-labs/hoserva/internal/auth"
	storedb "github.com/mdg-labs/hoserva/internal/store/db"
)

// timeFormat matches internal/job/store.go's own convention for every
// TEXT timestamp column (Q60: no SQL-side DEFAULT, computed in Go).
const timeFormat = time.RFC3339

// User is a users table row (#22, Q27, Q28), translated out of
// storedb's generated shape.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	Role         string // "admin" or "viewer" (Q27)
	// TOTPSecret is the *active*, confirmed credential — encrypted with
	// the machine key (Q28); nil until confirmTotp first activates one.
	TOTPSecret []byte
	// TOTPPendingSecret is a secret enrollTotp generated but confirmTotp
	// has not yet activated — also encrypted with the machine key, and
	// entirely independent of TOTPSecret, so starting or replacing a
	// pending enrolment can never disturb an already-active credential.
	TOTPPendingSecret []byte
	TOTPConfirmedAt   *time.Time
	TOTPLastStep      int64
	CreatedAt         time.Time
	// LastLoginAt is set only by a successful Login (#225, doc 03 §7) —
	// never derived from session liveness — and nil until the first one.
	LastLoginAt *time.Time
	// SMBCredentialSetAt is set the first time SetUserPassword's Samba
	// write succeeds for this account (#225, doc 03 §7) and never cleared
	// again: there is no operation that revokes SMB access short of
	// deleting the whole account, so "ever set" is exactly "currently
	// provisioned".
	SMBCredentialSetAt *time.Time
}

// TOTPEnrolled reports whether TOTP is confirmed and active — a pending,
// unconfirmed secret (TOTPSecret set, TOTPConfirmedAt nil) never counts.
func (u *User) TOTPEnrolled() bool { return u.TOTPConfirmedAt != nil }

// HasSMBCredential reports whether a Samba/password credential is
// currently provisioned for this account.
func (u *User) HasSMBCredential() bool { return u.SMBCredentialSetAt != nil }

// Session is a sessions table row.
type Session struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// AuthStore persists users and sessions (D4) through the sqlc-generated
// internal/store/db package. It has no business logic of its own —
// AuthService owns password/TOTP verification and rate limiting; AuthStore
// only reads and writes rows (mirroring internal/job.Store's own split).
type AuthStore struct {
	q  *storedb.Queries
	db storedb.DBTX
}

// NewAuthStore wraps db (typically *sql.DB) for auth persistence.
func NewAuthStore(db storedb.DBTX) *AuthStore {
	return &AuthStore{q: storedb.New(db), db: db}
}

// WithTx returns a store that reads and writes through tx.
func (s *AuthStore) WithTx(tx *sql.Tx) *AuthStore {
	return &AuthStore{q: s.q.WithTx(tx), db: tx}
}

// RunRecoveryTx runs fn inside one SQLite transaction. The transaction
// commits only when fn returns nil — a Q78 recovery's credential change,
// audit row and notify_deliveries inserts all succeed or none do.
func (s *AuthStore) RunRecoveryTx(ctx context.Context, fn func(*AuthStore) error) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: recovery transaction requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning recovery transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing recovery transaction: %w", err)
	}
	return nil
}

// AuthStore also implements auth.MachineKeyStore (Q28): the machine key's
// check value lives in this same database, so LoadOrGenerateMachineKey
// reads and writes it through the same store setup.go already builds for
// everything else, rather than a second, parallel store type.
var _ auth.MachineKeyStore = (*AuthStore)(nil)

// CountAdmins reports how many admin accounts exist — 0 before the
// first-run setup flow's createFirstAdmin succeeds, exactly 1 afterwards
// (the users_one_admin_idx partial unique index enforces that ceiling at
// the database level, not just here).
func (s *AuthStore) CountAdmins(ctx context.Context) (int64, error) {
	return s.q.CountAdmins(ctx)
}

// CreateUser inserts u as a new row. u.CreatedAt must already be set.
//
// It normalizes u.Username (normalizeUsername) before writing it, and
// reflects that normalization back onto u itself, so every creation
// path — not only CreateFirstAdmin, today's only one — ends up with the
// one folded form GetUserByUsername's own case-sensitive column
// comparison, and every rate-limiter subject built from a username
// (accountSubject), already assume every row holds. A caller normalizing
// separately before calling this is harmless (normalizeUsername is
// idempotent), but no longer required for correctness.
func (s *AuthStore) CreateUser(ctx context.Context, u *User) error {
	u.Username = normalizeUsername(u.Username)
	return s.q.CreateUser(ctx, storedb.CreateUserParams{
		ID:                u.ID,
		Username:          u.Username,
		PasswordHash:      u.PasswordHash,
		Role:              u.Role,
		TotpSecret:        u.TOTPSecret,
		TotpPendingSecret: u.TOTPPendingSecret,
		TotpConfirmedAt:   optionalTimeToSQL(u.TOTPConfirmedAt),
		TotpLastStep:      u.TOTPLastStep,
		CreatedAt:         u.CreatedAt.UTC().Format(timeFormat),
	})
}

// GetUserByUsername returns sql.ErrNoRows when no such user exists.
func (s *AuthStore) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	row, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	return userFromRow(row)
}

// GetUserByID returns sql.ErrNoRows when no such user exists.
func (s *AuthStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return userFromRow(row)
}

// SetPendingTOTPSecret replaces userID's pending TOTP secret, entirely
// independent of any already-active one (enrolling again before
// confirming replaces the still-pending secret, per the spec's own
// description — it never touches totp_secret, totp_confirmed_at or
// totp_last_step).
func (s *AuthStore) SetPendingTOTPSecret(ctx context.Context, userID string, secret []byte) error {
	return s.q.SetUserPendingTOTPSecret(ctx, storedb.SetUserPendingTOTPSecretParams{PendingSecret: secret, ID: userID})
}

// ActivateTOTP promotes userID's pending secret (pendingSecret — the
// caller's own already-read copy) into the active one, atomically: the
// write is conditional on totp_pending_secret still holding exactly
// pendingSecret, so two concurrent confirmTotp calls for the same pending
// secret can never both activate it (the second's WHERE clause matches
// zero rows, since the first has already cleared the column to NULL) —
// closing the same check-then-write race UpdateTOTPLastStepIfNewer closes
// for an ordinary login. ok is false when no row matched: either the
// pending secret was already consumed (a concurrent activation, or a
// replay of an old code) or it was replaced by a newer enrolment
// (secret is stale) in the meantime.
func (s *AuthStore) ActivateTOTP(ctx context.Context, userID string, secret []byte, confirmedAt time.Time, step int64, pendingSecret []byte) (ok bool, err error) {
	n, err := s.q.ActivateUserTOTP(ctx, storedb.ActivateUserTOTPParams{
		Secret:        secret,
		ConfirmedAt:   sql.NullString{String: confirmedAt.UTC().Format(timeFormat), Valid: true},
		Step:          step,
		ID:            userID,
		PendingSecret: pendingSecret,
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateTOTPLastStepIfNewer records step as the newest TOTP step accepted
// for userID, but only if it is still newer than whatever is currently
// stored (doc 01 §7's replay protection) — the write is conditional, not
// a read-validate-write pair, so two concurrent logins presenting the
// same code can never both succeed: at most one UPDATE's WHERE clause
// still matches by the time it runs. ok is false when the row was not
// newer (a replay, or a concurrent login already recorded a step at
// least this new).
func (s *AuthStore) UpdateTOTPLastStepIfNewer(ctx context.Context, userID string, step int64) (ok bool, err error) {
	n, err := s.q.UpdateUserTOTPLastStepIfNewer(ctx, storedb.UpdateUserTOTPLastStepIfNewerParams{Step: step, ID: userID})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// HasEncryptedSecrets implements auth.MachineKeyStore: whether any user
// row already holds a TOTP secret (active or pending), any
// notify_channels row already holds a credential (#35, Q28), ACME
// account/DNS secrets (#211, Q28), UPS passwords (#249, Q28), or a
// backup passphrase, encrypted under some machine key. Any one existing
// is reason enough to refuse regenerating it.
func (s *AuthStore) HasEncryptedSecrets(ctx context.Context) (bool, error) {
	hasTOTP, err := s.q.HasEncryptedTOTPSecrets(ctx)
	if err != nil {
		return false, err
	}
	if hasTOTP {
		return true, nil
	}
	hasNotify, err := s.q.HasEncryptedNotifySecrets(ctx)
	if err != nil {
		return false, err
	}
	if hasNotify {
		return true, nil
	}
	hasACME, err := s.q.HasEncryptedACMESecrets(ctx)
	if err != nil {
		return false, err
	}
	if hasACME {
		return true, nil
	}
	hasUPS, err := s.q.HasEncryptedUPSSecrets(ctx)
	if err != nil {
		return false, err
	}
	if hasUPS {
		return true, nil
	}
	return s.q.HasEncryptedBackupPassphrase(ctx)
}

// KeyCheckValue implements auth.MachineKeyStore.
func (s *AuthStore) KeyCheckValue(ctx context.Context) ([]byte, bool, error) {
	value, err := s.q.GetMachineKeyCheck(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return value, true, nil
}

// SetKeyCheckValue implements auth.MachineKeyStore.
func (s *AuthStore) SetKeyCheckValue(ctx context.Context, value []byte) error {
	return s.q.SetMachineKeyCheck(ctx, storedb.SetMachineKeyCheckParams{
		CheckValue: value,
		CreatedAt:  time.Now().UTC().Format(timeFormat),
	})
}

// CreateSession inserts sess as a new row.
func (s *AuthStore) CreateSession(ctx context.Context, sess *Session) error {
	return s.q.CreateSession(ctx, storedb.CreateSessionParams{
		TokenHash: sess.TokenHash,
		UserID:    sess.UserID,
		CreatedAt: sess.CreatedAt.UTC().Format(timeFormat),
		ExpiresAt: sess.ExpiresAt.UTC().Format(timeFormat),
	})
}

// GetSession returns sql.ErrNoRows when tokenHash has no session.
func (s *AuthStore) GetSession(ctx context.Context, tokenHash string) (*Session, error) {
	row, err := s.q.GetSession(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	return sessionFromRow(row)
}

// DeleteSession revokes a session server-side (logout).
func (s *AuthStore) DeleteSession(ctx context.Context, tokenHash string) error {
	return s.q.DeleteSession(ctx, tokenHash)
}

// DeleteOtherUserSessions revokes every session of userID except the one
// identified by keepTokenHash.
func (s *AuthStore) DeleteOtherUserSessions(ctx context.Context, userID, keepTokenHash string) error {
	return s.q.DeleteOtherUserSessions(ctx, storedb.DeleteOtherUserSessionsParams{
		UserID:    userID,
		TokenHash: keepTokenHash,
	})
}

// ActivateTOTPAndRevokeOtherSessions promotes userID's pending secret and
// revokes every other session of that account in one transaction. ok is
// false when ActivateTOTP's conditional write matched no row.
func (s *AuthStore) ActivateTOTPAndRevokeOtherSessions(
	ctx context.Context,
	userID string,
	secret []byte,
	confirmedAt time.Time,
	step int64,
	pendingSecret []byte,
	keepSessionTokenHash string,
) (ok bool, err error) {
	sqlDB, okDB := s.db.(*sql.DB)
	if !okDB {
		if _, ok := s.db.(*sql.Tx); !ok {
			return false, fmt.Errorf("auth store: totp confirmation transaction requires *sql.DB or *sql.Tx")
		}
		activated, err := s.ActivateTOTP(ctx, userID, secret, confirmedAt, step, pendingSecret)
		if err != nil || !activated {
			return activated, err
		}
		return true, s.DeleteOtherUserSessions(ctx, userID, keepSessionTokenHash)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning totp confirm transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txStore := s.WithTx(tx)
	activated, err := txStore.ActivateTOTP(ctx, userID, secret, confirmedAt, step, pendingSecret)
	if err != nil || !activated {
		return activated, err
	}
	if err := txStore.DeleteOtherUserSessions(ctx, userID, keepSessionTokenHash); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing totp confirm transaction: %w", err)
	}
	return true, nil
}

// DeleteExpiredSessions removes every session whose expiry is at or
// before now.
func (s *AuthStore) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	return s.q.DeleteExpiredSessions(ctx, now.UTC().Format(timeFormat))
}

// UpdatePasswordHash replaces userID's password hash.
func (s *AuthStore) UpdatePasswordHash(ctx context.Context, userID, hash string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE users SET password_hash = ? WHERE id = ?", hash, userID)
	if err != nil {
		return fmt.Errorf("updating password: %w", err)
	}
	return nil
}

// UpdatePasswordHashAndMarkSMBCredential sets password_hash to hash and
// smb_credential_set_at to setAt in one transaction, so the two writes
// SetUserPassword makes before its Samba call can never separate — a
// marker-write failure can no longer leave a changed password hash with
// HasSMBCredential still false (#225 CodeRabbit finding on PR #228).
func (s *AuthStore) UpdatePasswordHashAndMarkSMBCredential(ctx context.Context, userID, hash string, setAt time.Time) error {
	return s.updatePasswordHashAndSMBCredentialMarker(ctx, userID, hash, sql.NullString{String: setAt.UTC().Format(timeFormat), Valid: true})
}

// RestorePasswordHashAndClearSMBCredential sets password_hash back to hash
// and clears smb_credential_set_at to NULL, in one transaction —
// SetUserPassword's compensation when the Samba write that followed a
// first-time UpdatePasswordHashAndMarkSMBCredential call fails.
func (s *AuthStore) RestorePasswordHashAndClearSMBCredential(ctx context.Context, userID, hash string) error {
	return s.updatePasswordHashAndSMBCredentialMarker(ctx, userID, hash, sql.NullString{})
}

func (s *AuthStore) updatePasswordHashAndSMBCredentialMarker(ctx context.Context, userID, hash string, smbCredentialSetAt sql.NullString) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: password transaction requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning password transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "UPDATE users SET password_hash = ? WHERE id = ?", hash, userID); err != nil {
		return fmt.Errorf("updating password: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE users SET smb_credential_set_at = ? WHERE id = ?", smbCredentialSetAt, userID); err != nil {
		return fmt.Errorf("recording smb credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing password transaction: %w", err)
	}
	return nil
}

// RecordLogin sets userID's last_login_at to at (#225, doc 03 §7) — called
// only from a completed Login, never from anything that merely checks
// whether a session is still live.
func (s *AuthStore) RecordLogin(ctx context.Context, userID string, at time.Time) error {
	if err := s.q.RecordUserLogin(ctx, storedb.RecordUserLoginParams{
		LastLoginAt: sql.NullString{String: at.UTC().Format(timeFormat), Valid: true},
		ID:          userID,
	}); err != nil {
		return fmt.Errorf("recording login: %w", err)
	}
	return nil
}

// RecordLoginAndCreateSession sets userID's last_login_at and inserts sess
// in one transaction, so a session-creation failure can never leave
// last_login_at reporting a completed sign-in with no session behind it.
func (s *AuthStore) RecordLoginAndCreateSession(ctx context.Context, userID string, at time.Time, sess *Session) error {
	sqlDB, ok := s.db.(*sql.DB)
	if !ok {
		return fmt.Errorf("auth store: login transaction requires *sql.DB")
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning login transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txStore := s.WithTx(tx)
	if err := txStore.RecordLogin(ctx, userID, at); err != nil {
		return err
	}
	if err := txStore.CreateSession(ctx, sess); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing login transaction: %w", err)
	}
	return nil
}

// ClearUserTOTP removes every TOTP credential from userID.
func (s *AuthStore) ClearUserTOTP(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE users SET totp_secret = NULL, totp_confirmed_at = NULL,
    totp_pending_secret = NULL, totp_last_step = 0 WHERE id = ?`, userID)
	if err != nil {
		return fmt.Errorf("clearing totp: %w", err)
	}
	return nil
}

// CreateRecoveryDelivery queues one credential-reset notification to ch.
func (s *AuthStore) CreateRecoveryDelivery(ctx context.Context, id, channelID, title, message, at string) error {
	return s.q.CreateDelivery(ctx, storedb.CreateDeliveryParams{
		ID:            id,
		ChannelID:     channelID,
		EventType:     "credential_reset",
		Severity:      "critical",
		Title:         title,
		Message:       message,
		Status:        "pending",
		Attempts:      0,
		CreatedAt:     at,
		NextAttemptAt: at,
	})
}

// InsertAuditLog records one audit-log entry (doc 01 §7).
func (s *AuthStore) InsertAuditLog(ctx context.Context, actor, action, detail string, at time.Time) error {
	var detailArg sql.NullString
	if detail != "" {
		detailArg = sql.NullString{String: detail, Valid: true}
	}
	return s.q.InsertAuditLogEntry(ctx, storedb.InsertAuditLogEntryParams{
		Actor:  actor,
		Action: action,
		Detail: detailArg,
		At:     at.UTC().Format(timeFormat),
	})
}

func optionalTimeToSQL(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: t.UTC().Format(timeFormat), Valid: true}
}

func userFromRow(row *storedb.User) (*User, error) {
	createdAt, err := time.Parse(timeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing user %s created_at: %w", row.ID, err)
	}
	u := &User{
		ID:                row.ID,
		Username:          row.Username,
		PasswordHash:      row.PasswordHash,
		Role:              row.Role,
		TOTPSecret:        row.TotpSecret,
		TOTPPendingSecret: row.TotpPendingSecret,
		TOTPLastStep:      row.TotpLastStep,
		CreatedAt:         createdAt,
	}
	if row.TotpConfirmedAt.Valid {
		t, err := time.Parse(timeFormat, row.TotpConfirmedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parsing user %s totp_confirmed_at: %w", row.ID, err)
		}
		u.TOTPConfirmedAt = &t
	}
	if row.LastLoginAt.Valid {
		t, err := time.Parse(timeFormat, row.LastLoginAt.String)
		if err != nil {
			return nil, fmt.Errorf("parsing user %s last_login_at: %w", row.ID, err)
		}
		u.LastLoginAt = &t
	}
	if row.SmbCredentialSetAt.Valid {
		t, err := time.Parse(timeFormat, row.SmbCredentialSetAt.String)
		if err != nil {
			return nil, fmt.Errorf("parsing user %s smb_credential_set_at: %w", row.ID, err)
		}
		u.SMBCredentialSetAt = &t
	}
	return u, nil
}

func sessionFromRow(row *storedb.Session) (*Session, error) {
	createdAt, err := time.Parse(timeFormat, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("parsing session created_at: %w", err)
	}
	expiresAt, err := time.Parse(timeFormat, row.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("parsing session expires_at: %w", err)
	}
	return &Session{
		TokenHash: row.TokenHash,
		UserID:    row.UserID,
		CreatedAt: createdAt,
		ExpiresAt: expiresAt,
	}, nil
}
