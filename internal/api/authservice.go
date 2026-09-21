package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/share"
)

// sessionTTL is how long a session cookie stays valid without the user
// interacting again. Not specified in doc 01 §7 beyond "an expiry"; 30
// days matches a typical "remember me" browser session and is short
// enough that a stolen, unrevoked cookie doesn't stay useful forever.
const sessionTTL = 30 * 24 * time.Hour

var (
	// ErrSetupComplete is returned by CreateFirstAdmin once an admin
	// already exists — reachable both from the ordinary count check and
	// from a lost race against a concurrent request (the
	// users_one_admin_idx partial unique index is what actually
	// guarantees only one caller ever wins that race).
	ErrSetupComplete = errors.New("an admin account already exists")
	// ErrInvalidCredentials covers both an unknown username and a wrong
	// password, deliberately indistinguishable (doc 01 §7).
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrTOTPRequired       = errors.New("a TOTP code is required")
	ErrTOTPInvalid        = errors.New("invalid TOTP code")
	ErrSessionInvalid     = errors.New("invalid or expired session")
	ErrTOTPNotPending     = errors.New("no pending TOTP enrolment to confirm")
	// ErrTOTPReverifyRequired is returned by EnrollTOTP when TOTP is
	// already active on the account and the caller supplied neither the
	// current password nor a current TOTP code — enrollTotp must not let
	// a bare session cookie alone replace (and thereby turn off, until a
	// confirmTotp that never happens) an account's existing second
	// factor.
	ErrTOTPReverifyRequired = errors.New("re-enrolling an active TOTP credential requires the current password or a current TOTP code")
	// ErrTOTPReverifyAmbiguous is returned by EnrollTOTP when a
	// re-enrolment supplies both the current password and a current TOTP
	// code: each is one guess against the active credential, and honouring
	// both would let a single call spend two guesses against the one
	// Limiter.Reserve this issue's own rate limiting is built on — the
	// caller must supply exactly one, never both.
	ErrTOTPReverifyAmbiguous = errors.New("re-enrolling an active TOTP credential accepts the current password or a current TOTP code, not both")
	// ErrShareOnlyNoLogin is Login's refusal of an otherwise-correct
	// credential once the account's role is share-only (Q27): "no UI
	// login" is enforced here, at the point credentials would otherwise
	// succeed, not only left to fall out of every later operation's own
	// role check.
	ErrShareOnlyNoLogin = errors.New("this account has SMB/NFS access only — it has no UI login")
)

// Account roles (Q27, doc 03 §7). These are the users.role column's own
// values, distinct from Role (roles.go), which is an operation's required
// *capability level* — share-only is never a capability level (it never
// satisfies any operation's declared role, public ones excepted), only an
// account's own row can hold it.
const (
	roleAdmin     = "admin"
	roleViewer    = "viewer"
	roleShareOnly = "share-only"
)

// normalizeUsername folds username to the one form every account row,
// lookup and rate-limiter subject uses — AuthStore.CreateUser normalizes
// it before every row is written, so a lookup or a limiter subject built
// from the same fold always agrees with what's on record. This is the
// fix for a review finding: without it, "Admin" and "admin" looked up as
// two different accounts (GetUserByUsername's own column comparison is
// case-sensitive) despite accountSubject already folding a real
// account's own Username to lower case for the limiter, so the two forms
// of the same login shared a limiter budget but not an account.
func normalizeUsername(username string) string {
	return strings.ToLower(username)
}

// accountSubject returns the rate-limiter subject and SubjectKind for a
// login or re-enrolment attempt: a real account's own subject and
// auth.SubjectAccount once u is non-nil (looked up by AuthService.Store
// first, never merely assumed from the caller's input), or
// limiter.UnknownAccountSubject(normalizedUsername) and
// auth.SubjectUnknownAccount when it's nil — normalizedUsername is
// unused and may be "" when u is non-nil.
//
// Giving every nonexistent username its own subject, rather than one
// shared bucket, is the fix for a review finding: a shared bucket meant
// six failed logins against *any* unknown names locked out every unknown
// name at once, after which a fresh, never-tried unknown name got 429 in
// under a millisecond (no argon2id work — Reserve refused it outright)
// while a real, never-tried username still took a full password check
// before its own 401 — letting a caller distinguish "this name is real"
// from "this name isn't" well before ever guessing a password. Each
// unknown name's own subject, in its own capped, evictable table
// (auth.SubjectUnknownAccount), reaches the same threshold independently
// of every other unknown name, exactly like two different real accounts
// already did — see Login's own doc comment for why the dummy
// password-verification branch also stays unconditional either way.
func accountSubject(limiter *auth.Limiter, u *User, normalizedUsername string) (subject string, kind auth.SubjectKind) {
	if u != nil {
		return "user:" + normalizeUsername(u.Username), auth.SubjectAccount
	}
	return limiter.UnknownAccountSubject(normalizedUsername), auth.SubjectUnknownAccount
}

// LockoutError is returned by Login when subject (the account, the source
// address, or both) is currently locked out (doc 01 §7).
type LockoutError struct {
	RetryAfter time.Duration
}

func (e *LockoutError) Error() string {
	return fmt.Sprintf("too many attempts — try again in %s", e.RetryAfter.Round(time.Second))
}

// dummyPasswordHash is verified against on every login attempt for a
// username that doesn't exist, so that path costs the same argon2id work
// as a real one — doc 01 §7's rate-limiting bullet: no signal, including
// timing, distinguishes an unknown username from a wrong password.
// Computed once, from an arbitrary fixed input; its actual value carries
// no meaning and is never a real account's hash.
var dummyPasswordHash = mustHashPassword("hoserva-dummy-password-for-timing-only")

func mustHashPassword(password string) string {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(fmt.Sprintf("api: hashing dummy password: %v", err))
	}
	return hash
}

// AuthService implements #22's setup, login, session and TOTP business
// logic against AuthStore and internal/auth's primitives (doc 01 §7,
// Q28, Q44). Handler (auth_handler.go) and SessionSecurityHandler
// (security.go) both call through here rather than touching AuthStore or
// internal/auth directly, so there is exactly one place that decides what
// a valid session or a successful login means.
type AuthService struct {
	Store      *AuthStore
	MachineKey *auth.MachineKey
	Limiter    *auth.Limiter
	Now        func() time.Time
	// Issuer is the TOTP issuer name shown in an authenticator app
	// (doc 03 §1).
	Issuer string
	// SambaAccounts writes and removes Samba passdb entries (#49, Q27) —
	// the system-touching half of SetUserPassword and DeleteUser.
	// Defaulted to the real, smbpasswd-exec'ing implementation by
	// NewAuthService; tests replace it with share.NewFakeSambaAccounts()
	// so nothing here ever execs a real binary the host doesn't have
	// installed.
	SambaAccounts share.SambaAccounts
}

// NewAuthService wires an AuthService with the real clock and a fresh
// rate limiter.
func NewAuthService(store *AuthStore, key *auth.MachineKey) *AuthService {
	return &AuthService{
		Store:         store,
		MachineKey:    key,
		Limiter:       auth.NewLimiter(nil),
		Now:           time.Now,
		Issuer:        "Hoserva",
		SambaAccounts: share.NewSmbpasswdAccounts(),
	}
}

func isUniqueConstraintError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint")
}

// CreateFirstAdmin creates the sole admin account and signs it in,
// returning the raw session token for the caller to set as a cookie.
// Refuses once an admin already exists — atomically: the
// users_one_admin_idx partial unique index (schema.sql) makes two
// concurrent callers race the database itself, not this method's own
// CountAdmins check, so exactly one of them ever succeeds regardless of
// how their goroutines interleave.
func (s *AuthService) CreateFirstAdmin(ctx context.Context, username, password string) (*User, string, error) {
	n, err := s.Store.CountAdmins(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("counting admins: %w", err)
	}
	if n > 0 {
		return nil, "", ErrSetupComplete
	}

	hash, err := auth.HashPasswordContext(ctx, password)
	if err != nil {
		return nil, "", fmt.Errorf("hashing password: %w", err)
	}

	// Username is passed through as-typed; CreateUser normalizes it.
	u := &User{
		ID:           uuid.NewString(),
		Username:     username,
		PasswordHash: hash,
		Role:         "admin",
		TOTPLastStep: 0,
		CreatedAt:    s.Now(),
	}
	if err := s.Store.CreateUser(ctx, u); err != nil {
		if isUniqueConstraintError(err) {
			return nil, "", ErrSetupComplete
		}
		return nil, "", fmt.Errorf("creating admin: %w", err)
	}

	token, err := s.createSession(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// Login verifies username/password (and a TOTP code, once enrolled), rate
// limited and lockout-protected per account and per source address
// (sourceAddr may be "" when the caller has none — the Unix socket never
// calls this at all, since it never authenticates this way). On success
// it returns the signed-in user and a raw session token.
//
// Both the account and address limiter subjects are reserved (Limiter.
// Reserve) before any password-verification work runs, not merely
// checked: an ordinary check-then-record pair leaves a window where N
// concurrent attempts all pass the check before any of them is recorded,
// so a flood of parallel guesses never trips the lockout no matter how
// many arrive together. Password verification itself runs through
// auth.VerifyPasswordContext, bounded by its own small, CPU-derived
// worker pool — closing the same flood's other half, unauthenticated
// memory exhaustion: without a cap, each concurrent attempt reserves its
// own 64 MiB argon2id run at once.
func (s *AuthService) Login(ctx context.Context, username, password, totpCode, sourceAddr string) (*User, string, error) {
	addrKey := ""
	if sourceAddr != "" {
		addrKey = auth.AddressSubject(sourceAddr)
	}

	// The address is checked and reserved before the account, not after:
	// an already-locked-out address must never cause a new account entry
	// to be created at all. With the reverse order, a flood from one
	// locked-out address still reserved (and thereby created) a fresh
	// account entry on every single request — no password work, but an
	// unbounded number of limiter entries, each costing a lock-held map
	// insert — since only the *account* reservation was ever refused.
	// Checking the address first means that once it's locked, nothing
	// below this ever runs.
	if addrKey != "" {
		if ok, retryAfter := s.Limiter.Reserve(addrKey, auth.SubjectAddress); !ok {
			return nil, "", &LockoutError{RetryAfter: retryAfter}
		}
	}

	// The lookup runs before the account's own limiter reservation, not
	// after: whether username names a real account decides which of the
	// two per-username tables (accountSubject's own doc comment) this
	// attempt is reserved against, and either way it is reserved — a real
	// account's own subject, or that unknown username's own subject in
	// the separate, capped SubjectUnknownAccount table — never the old
	// shared bucket a review finding closed. The lookup itself is one
	// indexed row read, nowhere near argon2id's own cost, so doing it
	// before the reservation doesn't reopen the unauthenticated-work
	// flood this method's own doc comment already closes for password
	// verification.
	normalizedUsername := normalizeUsername(username)
	u, err := s.Store.GetUserByUsername(ctx, normalizedUsername)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("looking up user: %w", err)
	}

	userKey, userKind := accountSubject(s.Limiter, u, normalizedUsername)
	ok, retryAfter := s.Limiter.Reserve(userKey, userKind)
	if !ok {
		// The address's own reservation above already counted this
		// attempt against its budget; nothing to undo — a genuine
		// failure (which this is, from the address's point of view) is
		// meant to stay counted.
		return nil, "", &LockoutError{RetryAfter: retryAfter}
	}

	release := func() {
		s.Limiter.Release(userKey, userKind)
		if addrKey != "" {
			s.Limiter.Release(addrKey, auth.SubjectAddress)
		}
	}

	// Both branches run a real, bounded argon2id comparison, under the
	// same passwordWorkSem, so a wrong password and an unknown username
	// take the same shape and similar timing regardless of which
	// subject above admitted them (doc 01 §7). The dummy branch's own
	// result is discarded, not used as validPassword: an unknown
	// username is always ErrInvalidCredentials, regardless of what the
	// caller happened to type.
	var validPassword bool
	if u != nil {
		validPassword, err = auth.VerifyPasswordContext(ctx, u.PasswordHash, password)
	} else {
		_, err = auth.VerifyPasswordContext(ctx, dummyPasswordHash, password)
	}
	if err != nil {
		return nil, "", err
	}
	if u == nil || !validPassword {
		return nil, "", ErrInvalidCredentials
	}

	if u.TOTPEnrolled() {
		if totpCode == "" {
			return nil, "", ErrTOTPRequired
		}
		secret, err := s.decryptTOTPSecret(u.TOTPSecret)
		if err != nil {
			return nil, "", fmt.Errorf("decrypting totp secret: %w", err)
		}
		step, ok := auth.ValidateTOTP(secret, totpCode, s.Now(), u.TOTPLastStep)
		if !ok {
			return nil, "", ErrTOTPInvalid
		}
		// Conditional on totp_last_step still being older than step, not
		// a read-validate-write pair: two concurrent logins presenting
		// the same code can never both get past this, since at most one
		// UPDATE's WHERE clause still matches by the time it runs
		// (doc 01 §7's replay protection).
		accepted, err := s.Store.UpdateTOTPLastStepIfNewer(ctx, u.ID, step)
		if err != nil {
			return nil, "", fmt.Errorf("recording totp step: %w", err)
		}
		if !accepted {
			return nil, "", ErrTOTPInvalid
		}
	}

	release()

	// The role check runs after the credential is fully verified, not
	// before: these are real, correct credentials, not a guess, so the
	// rate limiter's reservation is released exactly as it would be for
	// any other successful login — what's refused here is policy
	// (share-only has no UI login, Q27), not authentication.
	if u.Role == roleShareOnly {
		return nil, "", ErrShareOnlyNoLogin
	}

	loginAt := s.Now()
	if err := s.Store.RecordLogin(ctx, u.ID, loginAt); err != nil {
		return nil, "", err
	}
	u.LastLoginAt = &loginAt

	token, err := s.createSession(ctx, u.ID)
	if err != nil {
		return nil, "", err
	}
	return u, token, nil
}

// Logout revokes rawToken's session server-side. Revoking a token that
// doesn't exist (already logged out, expired, or bogus) is not an error —
// the end state ("this token no longer works") is identical either way.
func (s *AuthService) Logout(ctx context.Context, rawToken string) error {
	if rawToken == "" {
		return nil
	}
	if err := s.Store.DeleteSession(ctx, auth.HashSessionToken(rawToken)); err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// LogoutByTokenHash revokes a session identified by its already-hashed
// token — SecurityHandler already computed this hash while validating the
// request, and the generated Logout operation itself takes no parameters
// (D18: it's a bare POST /auth/logout), so this is what the handler calls
// instead of re-deriving or re-hashing anything.
func (s *AuthService) LogoutByTokenHash(ctx context.Context, tokenHash string) error {
	if tokenHash == "" {
		return nil
	}
	if err := s.Store.DeleteSession(ctx, tokenHash); err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

// ValidateSession returns the user a raw session token belongs to, or
// ErrSessionInvalid for a missing, unknown or expired one — deliberately
// one error for all three, so nothing downstream can distinguish them.
func (s *AuthService) ValidateSession(ctx context.Context, rawToken string) (*User, error) {
	if rawToken == "" {
		return nil, ErrSessionInvalid
	}
	sess, err := s.Store.GetSession(ctx, auth.HashSessionToken(rawToken))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, fmt.Errorf("looking up session: %w", err)
	}
	if !s.Now().Before(sess.ExpiresAt) {
		_ = s.Store.DeleteSession(ctx, sess.TokenHash)
		return nil, ErrSessionInvalid
	}
	u, err := s.Store.GetUserByID(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionInvalid
		}
		return nil, fmt.Errorf("looking up session user: %w", err)
	}
	return u, nil
}

// EnrollTOTP generates a new secret for userID, storing it encrypted with
// the machine key (Q28) in its own pending column — entirely separate
// from any already-active credential, which this never touches — until
// ConfirmTOTP activates it. Calling this again before confirming replaces
// the still-pending secret.
//
// Once TOTP is already active on the account, replacing it is refused
// (ErrTOTPReverifyRequired) unless password or code proves the caller
// still holds the account: a bare session cookie must never be enough by
// itself to remove an account's second factor, which is what starting an
// enrolment used to do the instant it overwrote the active secret with
// an unconfirmed one — this is the fix. Neither is required for a first
// enrolment (u.TOTPEnrolled() is false).
func (s *AuthService) EnrollTOTP(ctx context.Context, userID, password, code string) (secret, otpauthURI string, err error) {
	u, err := s.Store.GetUserByID(ctx, userID)
	if err != nil {
		return "", "", fmt.Errorf("looking up user: %w", err)
	}

	if u.TOTPEnrolled() {
		if err := s.verifyTOTPReenrolment(ctx, u, password, code); err != nil {
			return "", "", err
		}
	}

	secret, err = auth.GenerateTOTPSecret()
	if err != nil {
		return "", "", err
	}
	encrypted, err := s.MachineKey.Encrypt([]byte(secret))
	if err != nil {
		return "", "", fmt.Errorf("encrypting totp secret: %w", err)
	}
	if err := s.Store.SetPendingTOTPSecret(ctx, userID, encrypted); err != nil {
		return "", "", fmt.Errorf("storing pending totp secret: %w", err)
	}
	return secret, auth.OtpauthURI(s.Issuer, u.Username, secret), nil
}

// verifyTOTPReenrolment proves the caller still holds u's account before
// EnrollTOTP is allowed to replace an already-active TOTP credential —
// either the current password, or a current code against the active
// secret (checked exactly like Login's own TOTP step, including replay
// protection: a code this call accepts can never be reused for a login
// or another re-enrolment). Exactly one of password or code must be
// supplied, never both (ErrTOTPReverifyAmbiguous) — honouring both would
// spend two guesses against the single Reserve call below, doubling the
// effective rate an attacker could try passwords or codes at.
//
// Every attempt is reserved against — and, on failure, stays counted
// against — the exact same account subject Login uses (Reserve before
// checking, Release only once the check actually succeeds), so this can
// never be an unlimited password/TOTP-code oracle against an account's
// second factor: without it, any bare session cookie (RoleViewer is all
// EnrollTOTP requires) could try passwords or codes against the *active*
// credential at full request rate, with no logging and no lockout, and a
// successful guess would hand the attacker a brand new pending secret to
// confirm as their own. Sharing Login's subject also means a caller
// locked out here is locked out of login too, and vice versa — one
// failure budget per account, not two independent ones an attacker could
// exhaust separately.
func (s *AuthService) verifyTOTPReenrolment(ctx context.Context, u *User, password, code string) error {
	if password != "" && code != "" {
		return ErrTOTPReverifyAmbiguous
	}

	// u is always a real, looked-up account here (EnrollTOTP only reaches
	// this once u.TOTPEnrolled() is true), so accountSubject always
	// returns auth.SubjectAccount — kept as a variable, not the constant,
	// so this stays the same call Login makes.
	userKey, userKind := accountSubject(s.Limiter, u, "")
	ok, retryAfter := s.Limiter.Reserve(userKey, userKind)
	if !ok {
		return &LockoutError{RetryAfter: retryAfter}
	}

	if password != "" {
		if ok, err := auth.VerifyPasswordContext(ctx, u.PasswordHash, password); err != nil {
			return err
		} else if ok {
			s.Limiter.Release(userKey, userKind)
			return nil
		}
	}
	if code != "" {
		secret, err := s.decryptTOTPSecret(u.TOTPSecret)
		if err != nil {
			return fmt.Errorf("decrypting totp secret: %w", err)
		}
		// accepted's result is honoured, not discarded: ValidateTOTP's
		// own ok can be true from a stale read of u.TOTPLastStep (a
		// concurrent Login or re-enrolment already advanced the
		// persisted step by the time this call's own conditional UPDATE
		// runs), in which case accepted is false and this call must not
		// be told its code proved anything — mirroring Login's identical
		// TOTP-step handling.
		if step, ok := auth.ValidateTOTP(secret, code, s.Now(), u.TOTPLastStep); ok {
			accepted, err := s.Store.UpdateTOTPLastStepIfNewer(ctx, u.ID, step)
			if err != nil {
				return fmt.Errorf("recording totp step: %w", err)
			}
			if accepted {
				s.Limiter.Release(userKey, userKind)
				return nil
			}
		}
	}
	return ErrTOTPReverifyRequired
}

// ConfirmTOTP activates userID's pending secret once code proves the user
// has it, and revokes every other session of that account atomically with
// the activation (Q84). keepSessionTokenHash is the caller's own session
// hash — empty revokes every session of the account.
//
// The activation itself is conditional, not a read-validate-write pair:
// AuthStore.ActivateTOTP's UPDATE only matches while totp_pending_secret
// still holds exactly the secret this call read, so two concurrent
// confirmTotp calls for the same pending secret can never both activate it
// (doc 01 §7's replay protection, mirroring Login's own TOTP-step check).
func (s *AuthService) ConfirmTOTP(ctx context.Context, userID, code, keepSessionTokenHash string) error {
	u, err := s.Store.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("looking up user: %w", err)
	}
	if len(u.TOTPPendingSecret) == 0 {
		return ErrTOTPNotPending
	}

	secret, err := s.decryptTOTPSecret(u.TOTPPendingSecret)
	if err != nil {
		return fmt.Errorf("decrypting pending totp secret: %w", err)
	}
	// A pending secret has no prior accepted step of its own yet, so
	// lastAcceptedStep is always 0 here, regardless of the active
	// credential's own totp_last_step.
	step, ok := auth.ValidateTOTP(secret, code, s.Now(), 0)
	if !ok {
		return ErrTOTPInvalid
	}
	activated, err := s.Store.ActivateTOTPAndRevokeOtherSessions(ctx, userID, u.TOTPPendingSecret, s.Now(), step, u.TOTPPendingSecret, keepSessionTokenHash)
	if err != nil {
		return fmt.Errorf("confirming totp: %w", err)
	}
	if !activated {
		return ErrTOTPInvalid
	}
	return nil
}

func (s *AuthService) decryptTOTPSecret(ciphertext []byte) (string, error) {
	plaintext, err := s.MachineKey.Decrypt(ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *AuthService) createSession(ctx context.Context, userID string) (string, error) {
	raw, hash, err := auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	now := s.Now()
	sess := &Session{
		TokenHash: hash,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
	}
	if err := s.Store.CreateSession(ctx, sess); err != nil {
		return "", fmt.Errorf("creating session: %w", err)
	}
	return raw, nil
}
