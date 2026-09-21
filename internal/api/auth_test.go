package api_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/store"

	_ "modernc.org/sqlite"
)

// newAuthTestService wires a real SQLite database (through the same
// migration runner the daemon uses) and a fresh machine key, exactly like
// handler_test.go's newTestHandler does for the job system — this
// package's auth tests exercise real persistence and real crypto, never a
// fixture map.
func newAuthTestService(t *testing.T) (*api.AuthService, *sql.DB) {
	t.Helper()

	migrations, err := store.Load()
	if err != nil {
		t.Fatalf("loading embedded migrations: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "auth-test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &store.Runner{DB: db, Migrations: migrations, SnapshotDir: t.TempDir()}
	if _, _, err := runner.Apply(context.Background()); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}

	authStore := api.NewAuthStore(db)
	key, err := auth.LoadOrGenerateMachineKey(context.Background(), filepath.Join(t.TempDir(), "secret.key"), authStore)
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}

	svc := api.NewAuthService(authStore, key)
	return svc, db
}

func TestCreateFirstAdminSucceedsOnce(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	u, token, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if u.Role != "admin" {
		t.Errorf("role = %q, want admin", u.Role)
	}
	if token == "" {
		t.Error("expected a non-empty session token")
	}

	if _, _, err := svc.CreateFirstAdmin(ctx, "someone-else", "another password entirely"); !errors.Is(err, api.ErrSetupComplete) {
		t.Errorf("second CreateFirstAdmin = %v, want ErrSetupComplete", err)
	}
}

// TestCreateFirstAdminRaceCreatesExactlyOneAdmin drives the exact race the
// acceptance criteria names: two concurrent requests must never both
// create an admin. The users_one_admin_idx partial unique index
// (schema.sql) is what actually guarantees this — this test proves it
// holds against a real database, not just against CreateFirstAdmin's own
// CountAdmins pre-check, which by itself has a race window.
func TestCreateFirstAdminRaceCreatesExactlyOneAdmin(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	const attempts = 8
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			username := "admin"
			if _, _, err := svc.CreateFirstAdmin(ctx, username, "correct horse battery staple"); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, api.ErrSetupComplete) {
				t.Errorf("attempt %d: unexpected error: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("%d of %d concurrent CreateFirstAdmin calls succeeded, want exactly 1", got, attempts)
	}
	n, err := svc.Store.CountAdmins(ctx)
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if n != 1 {
		t.Errorf("CountAdmins = %d, want 1", n)
	}
}

// TestUsernameIsCaseInsensitive is the review finding this issue closes:
// GetUserByUsername's own column comparison is case-sensitive, but
// accountSubject already folded a real account's own Username to lower
// case for the rate limiter — so "Admin" and "admin" shared one limiter
// budget while looking like two different accounts to everything else.
// Normalizing at CreateFirstAdmin and at lookup makes every caller agree:
// "Admin" and "admin" are the same account, everywhere.
func TestUsernameIsCaseInsensitive(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	created, _, err := svc.CreateFirstAdmin(ctx, "Admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	if created.Username != "admin" {
		t.Errorf("CreateFirstAdmin(%q).Username = %q, want the normalized lower-case form", "Admin", created.Username)
	}

	// Logging in with any other casing of the same username reaches the
	// same account.
	u, _, err := svc.Login(ctx, "aDMIN", "correct horse battery staple", "", "203.0.113.1")
	if err != nil {
		t.Fatalf("Login with a differently-cased username: %v", err)
	}
	if u.ID != created.ID {
		t.Errorf("Login(%q) returned a different account than CreateFirstAdmin(%q) created", "aDMIN", "Admin")
	}

	// A second createFirstAdmin, differently cased, is refused exactly
	// like an exact-case repeat would be — it's the same account, not a
	// new one.
	if _, _, err := svc.CreateFirstAdmin(ctx, "ADMIN", "another password entirely"); !errors.Is(err, api.ErrSetupComplete) {
		t.Errorf("CreateFirstAdmin(%q) after admin already exists = %v, want ErrSetupComplete", "ADMIN", err)
	}
}

// TestCreateUserNormalizesUsername is the review finding this issue
// closes: normalization used to happen only at CreateFirstAdmin's own
// call site, so any future caller of AuthStore.CreateUser that forgot to
// normalize first (there being none yet besides CreateFirstAdmin) would
// have written a row TestUsernameIsCaseInsensitive's own guarantees don't
// cover. Calling CreateUser directly, bypassing CreateFirstAdmin
// entirely, confirms the store itself — not just that one caller —
// normalizes.
func TestCreateUserNormalizesUsername(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u := &api.User{ID: "viewer-id", Username: "ViEwEr", Role: "viewer", PasswordHash: hash, CreatedAt: svc.Now()}
	if err := svc.Store.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if u.Username != "viewer" {
		t.Errorf("CreateUser mutated u.Username to %q, want the normalized lower-case form %q", u.Username, "viewer")
	}

	got, err := svc.Store.GetUserByUsername(ctx, "viewer")
	if err != nil {
		t.Fatalf("GetUserByUsername(%q): %v", "viewer", err)
	}
	if got.ID != u.ID {
		t.Errorf("GetUserByUsername(%q) found a different account than CreateUser(%q) created", "viewer", "ViEwEr")
	}
}

func TestLoginSuccess(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	u, token, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if u.Username != "admin" {
		t.Errorf("username = %q, want admin", u.Username)
	}
	if token == "" {
		t.Error("expected a non-empty session token")
	}

	got, err := svc.ValidateSession(ctx, token)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("ValidateSession returned a different user")
	}
}

// TestLoginRecordsLastLoginAtAuthenticationTime is #225's acceptance
// criterion: last-login is set by a completed Login, not derived from
// whether a session happens to still be live.
func TestLoginRecordsLastLoginAtAuthenticationTime(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	before, err := svc.Store.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if before.LastLoginAt != nil {
		t.Fatalf("LastLoginAt = %v before any Login call, want nil", before.LastLoginAt)
	}

	u, token, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.5")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if u.LastLoginAt == nil {
		t.Fatal("Login did not set LastLoginAt on the returned user")
	}

	after, err := svc.Store.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if after.LastLoginAt == nil {
		t.Fatal("LastLoginAt was not persisted")
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	stillThere, err := svc.Store.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if stillThere.LastLoginAt == nil {
		t.Fatal("LastLoginAt must survive session revocation, not be derived from session liveness")
	}
}

func TestLoginWrongPasswordAndUnknownUserSameError(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	_, _, err1 := svc.Login(ctx, "admin", "wrong password", "", "198.51.100.1")
	_, _, err2 := svc.Login(ctx, "no-such-user", "irrelevant password", "", "198.51.100.2")

	if !errors.Is(err1, api.ErrInvalidCredentials) {
		t.Errorf("wrong password: %v, want ErrInvalidCredentials", err1)
	}
	if !errors.Is(err2, api.ErrInvalidCredentials) {
		t.Errorf("unknown user: %v, want ErrInvalidCredentials", err2)
	}
}

// TestLoginUnknownUsernameTimingIsSimilarToARealUsername measures, rather
// than merely asserting, the "at similar timing" half of the login
// description (api/openapi.yaml): both an unknown username and a real
// one's wrong password run the same bounded argon2id-shaped comparison
// (Login's own doc comment), so this times each real, uncached login
// attempt (real system calls and disk-backed SQLite included, not a
// microbenchmark of the hash function alone) and checks the two medians
// land within a generous factor of each other — generous because CI
// hosts vary, and the point is to catch "one path skips the password
// check" (which would show up as an order-of-magnitude gap, not a small
// one), not to pin an exact ratio.
func TestLoginUnknownUsernameTimingIsSimilarToARealUsername(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	const samples = 15
	// Every real-account sample is its own account, and every unknown
	// sample its own username, each tried exactly once: reusing one
	// subject across samples would let limiterFreeAttempts's own lockout
	// (a real effect, not a timing artifact) shorten the later samples.
	for i := 0; i < samples; i++ {
		username := fmt.Sprintf("timing-user-%d", i)
		hash, err := auth.HashPassword("correct horse battery staple")
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		u := &api.User{ID: fmt.Sprintf("timing-user-id-%d", i), Username: username, Role: "viewer", PasswordHash: hash, CreatedAt: svc.Now()}
		if err := svc.Store.CreateUser(ctx, u); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}

	realDurations := make([]time.Duration, samples)
	unknownDurations := make([]time.Duration, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		_, _, _ = svc.Login(ctx, fmt.Sprintf("timing-user-%d", i), "wrong password", "", fmt.Sprintf("198.51.100.%d", i))
		realDurations[i] = time.Since(start)

		start = time.Now()
		_, _, _ = svc.Login(ctx, fmt.Sprintf("timing-unknown-user-%d", i), "wrong password", "", fmt.Sprintf("198.51.101.%d", i))
		unknownDurations[i] = time.Since(start)
	}

	realMedian := medianDuration(realDurations)
	unknownMedian := medianDuration(unknownDurations)
	t.Logf("median real-account login: %v, median unknown-username login: %v (%d samples each)", realMedian, unknownMedian, samples)

	if realMedian <= 0 || unknownMedian <= 0 {
		t.Fatalf("expected positive median durations, got real=%v unknown=%v", realMedian, unknownMedian)
	}
	const maxRatio = 5.0
	ratio := float64(realMedian) / float64(unknownMedian)
	if ratio > maxRatio || ratio < 1/maxRatio {
		t.Errorf("median real-account login %v vs median unknown-username login %v: ratio %.2f exceeds the %.0fx tolerance", realMedian, unknownMedian, ratio, maxRatio)
	}
}

func medianDuration(d []time.Duration) time.Duration {
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func TestLoginLockoutAfterRepeatedFailures(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	var lastErr error
	for i := 0; i < 10; i++ {
		_, _, lastErr = svc.Login(ctx, "admin", "wrong password", "", "203.0.113.9")
	}
	var lockout *api.LockoutError
	if !errors.As(lastErr, &lockout) {
		t.Fatalf("after repeated failures, Login = %v, want a *LockoutError", lastErr)
	}

	// The correct password still doesn't get through while locked out.
	_, _, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.9")
	if !errors.As(err, &lockout) {
		t.Errorf("locked out account should refuse even the correct password, got %v", err)
	}
}

func TestLoginTOTPFlow(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	// Login without a code is refused, distinctly from a wrong password.
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.5"); !errors.Is(err, api.ErrTOTPRequired) {
		t.Errorf("login with no TOTP code = %v, want ErrTOTPRequired", err)
	}

	// A fresh code, one step later, succeeds and can never be reused
	// (replay protection).
	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	loginCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", loginCode, "203.0.113.5"); err != nil {
		t.Fatalf("Login with a valid TOTP code: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", loginCode, "203.0.113.5"); !errors.Is(err, api.ErrTOTPInvalid) {
		t.Errorf("reusing an already-accepted TOTP code = %v, want ErrTOTPInvalid (replay protection)", err)
	}
}

// TestEnrollTOTPReplacingActiveRequiresReverification is the review
// finding this issue closes: re-enrolling while TOTP is already active
// must not, by itself, remove the account's existing second factor — a
// bare session cookie (which is all EnrollTOTP's caller is required to
// present) must never be enough.
func TestEnrollTOTPReplacingActiveRequiresReverification(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	// A bare re-enrolment attempt, with neither a password nor a code, is
	// refused.
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "", ""); !errors.Is(err, api.ErrTOTPReverifyRequired) {
		t.Errorf("bare re-enrolment while TOTP is active = %v, want ErrTOTPReverifyRequired", err)
	}
	// A wrong password and a wrong code are refused the same way.
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "wrong password", ""); !errors.Is(err, api.ErrTOTPReverifyRequired) {
		t.Errorf("re-enrolment with a wrong password = %v, want ErrTOTPReverifyRequired", err)
	}
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "", "000000"); !errors.Is(err, api.ErrTOTPReverifyRequired) {
		t.Errorf("re-enrolment with a wrong code = %v, want ErrTOTPReverifyRequired", err)
	}

	// The original credential must be entirely undisturbed by any of
	// those refused attempts: a login with it still works.
	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	stillValidCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", stillValidCode, ""); err != nil {
		t.Errorf("login with the original TOTP secret after refused re-enrolment attempts: %v", err)
	}
}

// TestEnrollTOTPReplacingActiveRefusesBothPasswordAndCode is the review
// finding this issue closes: supplying both the current password and a
// current TOTP code used to spend two guesses against the one Reserve
// call verifyTOTPReenrolment makes, doubling the effective rate an
// attacker could try either at. Supplying both is now refused outright,
// before either is ever checked and before the limiter subject is even
// reserved — so it costs nothing against the account's own budget either.
func TestEnrollTOTPReplacingActiveRefusesBothPasswordAndCode(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	reenrolCode, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "correct horse battery staple", reenrolCode); !errors.Is(err, api.ErrTOTPReverifyAmbiguous) {
		t.Errorf("re-enrolment with both a correct password and a correct code = %v, want ErrTOTPReverifyAmbiguous", err)
	}

	// The refusal must not have spent any of the account's own limiter
	// budget: a bare re-enrolment attempt right after still gets the
	// ordinary ErrTOTPReverifyRequired, not a LockoutError.
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "", ""); !errors.Is(err, api.ErrTOTPReverifyRequired) {
		t.Errorf("re-enrolment after the ambiguous refusal = %v, want ErrTOTPReverifyRequired (not locked out)", err)
	}

	// And a login with the still-active secret still works — a step
	// later, since the ambiguous attempt's own reenrolCode was never
	// actually validated (replay protection never advanced
	// totp_last_step past confirmTotp's own step), so this proves the
	// secret itself, not merely that this particular step was never
	// consumed.
	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	loginCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", loginCode, ""); err != nil {
		t.Errorf("login after the ambiguous re-enrolment refusal: %v", err)
	}
}

// TestEnrollTOTPReplacingActiveSucceedsWithPasswordOrCode covers the two
// ways EnrollTOTP accepts a re-enrolment while TOTP is already active —
// and confirms the previous secret only actually stops working once
// confirmTotp activates the new one, never earlier.
func TestEnrollTOTPReplacingActiveSucceedsWithPasswordOrCode(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	oldSecret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	oldCode, err := auth.CurrentCode(oldSecret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, oldCode, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	// Re-enrolling with the correct password succeeds.
	newSecret, _, err := svc.EnrollTOTP(ctx, u.ID, "correct horse battery staple", "")
	if err != nil {
		t.Fatalf("EnrollTOTP with the correct password: %v", err)
	}
	if newSecret == oldSecret {
		t.Fatal("expected a fresh secret, not the original one")
	}

	// The old secret is still the *active* one until confirmTotp
	// activates the new one — a login with it must still succeed.
	beforeActivation := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return beforeActivation }
	oldCodeBeforeActivation, err := auth.CurrentCode(oldSecret, beforeActivation)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", oldCodeBeforeActivation, ""); err != nil {
		t.Errorf("the original secret must remain active until confirmTotp activates the new one: %v", err)
	}

	// Confirming the new secret activates it.
	confirmCode, err := auth.CurrentCode(newSecret, beforeActivation)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, confirmCode, ""); err != nil {
		t.Fatalf("ConfirmTOTP with the new secret: %v", err)
	}

	// Now the old secret no longer works, and the new one does.
	afterActivation := beforeActivation.Add(60 * time.Second)
	svc.Now = func() time.Time { return afterActivation }
	oldCodeAfterActivation, err := auth.CurrentCode(oldSecret, afterActivation)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", oldCodeAfterActivation, ""); !errors.Is(err, api.ErrTOTPInvalid) {
		t.Errorf("the old secret should no longer validate once the new one is active: %v", err)
	}
	newCodeAfterActivation, err := auth.CurrentCode(newSecret, afterActivation)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", newCodeAfterActivation, ""); err != nil {
		t.Errorf("the newly activated secret should now work: %v", err)
	}
}

// TestEnrollTOTPReplacingActiveSucceedsWithCode is
// TestEnrollTOTPReplacingActiveSucceedsWithPasswordOrCode's other branch:
// proving a caller may also re-enrol with a current TOTP code instead of
// the password.
func TestEnrollTOTPReplacingActiveSucceedsWithCode(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	reenrolCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "", reenrolCode); err != nil {
		t.Errorf("EnrollTOTP with a valid current code: %v", err)
	}
}

// TestEnrollTOTPReenrolmentIsRateLimitedAndLocksOutLogin is the review
// finding this issue closes: verifyTOTPReenrolment used to run no
// reservation or lockout of its own at all, making it an unlimited
// password/TOTP-code oracle against an already-active second factor for
// anyone holding a bare session cookie (RoleViewer is enough). It must
// share Login's own per-account limiter subject, so repeated wrong
// re-enrolment attempts lock the account out of login too, not merely
// refuse the re-enrolment itself.
func TestEnrollTOTPReenrolmentIsRateLimitedAndLocksOutLogin(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	var lastErr error
	for i := 0; i < 10; i++ {
		_, _, lastErr = svc.EnrollTOTP(ctx, u.ID, "wrong password", "")
	}
	var lockout *api.LockoutError
	if !errors.As(lastErr, &lockout) {
		t.Fatalf("after repeated wrong re-enrolment passwords, EnrollTOTP = %v, want a *LockoutError", lastErr)
	}

	// The very same account is now locked out of an ordinary login too —
	// re-enrolment and login share one limiter subject by design, not two
	// independent budgets an attacker could exhaust separately.
	loginCode, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", loginCode, ""); !errors.As(err, &lockout) {
		t.Errorf("Login while re-enrolment attempts have locked the account out = %v, want a *LockoutError", err)
	}
}

// TestEnrollTOTPReenrolmentConcurrentSameCodeAcceptedOnlyOnce is the
// review finding this issue closes: verifyTOTPReenrolment discarded
// UpdateTOTPLastStepIfNewer's own accepted result, so two concurrent
// re-enrolment attempts that both read the same not-yet-advanced
// totp_last_step before either write landed could both be told their code
// was valid, even though only one of their conditional writes could still
// match by the time it ran.
func TestEnrollTOTPReenrolmentConcurrentSameCodeAcceptedOnlyOnce(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	reenrolCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}

	const attempts = 10
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.EnrollTOTP(ctx, u.ID, "", reenrolCode)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, api.ErrTOTPReverifyRequired):
				// expected for every loser of the race.
			default:
				var lockout *api.LockoutError
				if !errors.As(err, &lockout) {
					t.Errorf("unexpected EnrollTOTP error: %v", err)
				}
				// A concurrent burst against the same account can also
				// trip the shared login/re-enrolment rate limiter —
				// this test's own property (never more than one
				// success) holds either way.
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("%d of %d concurrent re-enrolment attempts with the same code succeeded, want exactly 1", got, attempts)
	}
}

// TestLoginLockedAddressDoesNotAccumulateUsernameEntries is the review
// finding this issue closes: Login used to reserve the username subject
// before the address subject, so an address that was already locked out
// still created a brand new per-username limiter entry on every further
// attempt — no password work, but an unbounded number of map entries, one
// per distinct username an attacker cared to try. Checking the address
// first means a locked-out address is refused before any username
// reservation is ever made.
func TestLoginLockedAddressDoesNotAccumulateUsernameEntries(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	const addr = "203.0.113.9"
	var lastErr error
	for i := 0; i < 10; i++ {
		_, _, lastErr = svc.Login(ctx, "admin", "wrong password", "", addr)
	}
	var lockout *api.LockoutError
	if !errors.As(lastErr, &lockout) {
		t.Fatalf("expected the address to be locked out, got %v", lastErr)
	}
	lockedAccountEntries := svc.Limiter.Len(auth.SubjectAccount)

	// A flood of distinct, nonexistent usernames from the now-locked
	// address must be refused by the address's own lockout before ever
	// reaching a per-account reservation, so the account table's entry
	// count must not grow with each new username tried.
	for i := 0; i < 500; i++ {
		_, _, err := svc.Login(ctx, fmt.Sprintf("nonexistent-user-%d", i), "wrong password", "", addr)
		if !errors.As(err, &lockout) {
			t.Fatalf("attempt %d: expected a LockoutError from the locked address, got %v", i, err)
		}
	}

	if got := svc.Limiter.Len(auth.SubjectAccount); got != lockedAccountEntries {
		t.Errorf("Limiter.Len(SubjectAccount) = %d after 500 distinct usernames from a locked address, want unchanged at %d (no per-account entries created)", got, lockedAccountEntries)
	}
}

// TestLoginUnknownUsernameFloodNeverTouchesTheRealAccountTable is the
// address-independent half of TestLoginLockedAddressDoesNotAccumulateUsernameEntries:
// a flood of distinct, nonexistent usernames, each from its own never
// locked out address, must still never create an entry in the real
// account table (auth.SubjectAccount) — every one of them lands in the
// separate, capped auth.SubjectUnknownAccount table instead (this
// issue's own fix for a review finding: a shared bucket used to let a
// caller distinguish a real username from an unknown one by how fast it
// locked out and whether it cost any argon2id work — see accountSubject's
// own doc comment). The real "admin" account's own entry, once actually
// tried, is unaffected by any of it.
func TestLoginUnknownUsernameFloodNeverTouchesTheRealAccountTable(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	const flood = 2_000
	for i := 0; i < flood; i++ {
		addr := fmt.Sprintf("10.0.%d.%d", i/250, i%250)
		_, _, _ = svc.Login(ctx, fmt.Sprintf("nonexistent-user-%d", i), "wrong password", "", addr)
	}

	if got := svc.Limiter.Len(auth.SubjectAccount); got != 0 {
		t.Errorf("Limiter.Len(SubjectAccount) = %d after %d distinct nonexistent usernames, want 0 (the real account table, never touched by an unknown username)", got, flood)
	}
	if got := svc.Limiter.Len(auth.SubjectUnknownAccount); got != flood {
		t.Errorf("Limiter.Len(SubjectUnknownAccount) = %d after %d distinct nonexistent usernames, want %d (one subject per distinct unknown username, under the table's own cap)", got, flood, flood)
	}

	// The real admin account still has its own, untouched budget: a
	// correct login still works normally after the flood.
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "198.51.100.9"); err != nil {
		t.Errorf("Login for the real account after the unknown-username flood: %v", err)
	}
}

// TestLoginUnknownUsernameReachesTheSameLockoutThresholdAsARealUsername is
// the review finding this issue closes, taken directly: locking out a
// handful of unknown usernames used to lock out every other unknown
// username at once, so a never-tried unknown name then got 429 far
// faster than a never-tried real username's own 401 (no argon2id work —
// Reserve refused it outright). This drives exactly that: five failures
// against unrelated unknown names (never the one under test), then walks
// both an untried unknown name and an untried real name through every one
// of the seven attempts limiterFreeAttempts (doc 01 §7, five) takes to
// reach lockout, asserting each attempt individually — not only the
// last — so a mutation that moves either name's own threshold in either
// direction (including one that starts locking an unknown name out
// earlier than a real one, the exact shape of the original finding)
// fails this test. Timing is covered separately, by
// TestLoginUnknownUsernameTimingIsSimilarToARealUsername; this test's own
// name no longer claims to measure it.
func TestLoginUnknownUsernameReachesTheSameLockoutThresholdAsARealUsername(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	// Noise: failures against five *other* unknown names, from their own
	// addresses, must never affect either name under test below.
	for i := 0; i < 5; i++ {
		_, _, _ = svc.Login(ctx, fmt.Sprintf("noise-user-%d", i), "wrong password", "", fmt.Sprintf("198.51.100.%d", 50+i))
	}

	// Seven attempts each, from addresses reused at most six times apiece
	// (one short of the address table's own five-free-attempt budget, so
	// the source-address subject never locks out inside this loop — only
	// the account-level subject under test ever does, which is the one
	// this test means to observe).
	const attempts = 7
	unknownErrs := make([]error, attempts)
	realErrs := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		unknownAddr, realAddr := "198.51.100.101", "198.51.100.102"
		if i > 0 {
			unknownAddr, realAddr = "198.51.100.103", "198.51.100.104"
		}
		_, _, unknownErrs[i] = svc.Login(ctx, "never-tried-unknown-user", "wrong password", "", unknownAddr)
		_, _, realErrs[i] = svc.Login(ctx, "admin", "wrong password", "", realAddr)
	}

	for i := 0; i < attempts; i++ {
		attempt := i + 1
		if attempt < attempts {
			if !errors.Is(unknownErrs[i], api.ErrInvalidCredentials) {
				t.Errorf("unknown username, attempt %d = %v, want ErrInvalidCredentials", attempt, unknownErrs[i])
			}
			if !errors.Is(realErrs[i], api.ErrInvalidCredentials) {
				t.Errorf("real username, attempt %d = %v, want ErrInvalidCredentials", attempt, realErrs[i])
			}
			continue
		}
		var unknownLockout, realLockout *api.LockoutError
		if !errors.As(unknownErrs[i], &unknownLockout) {
			t.Errorf("unknown username, attempt %d = %v, want *LockoutError", attempt, unknownErrs[i])
		}
		if !errors.As(realErrs[i], &realLockout) {
			t.Errorf("real username, attempt %d = %v, want *LockoutError", attempt, realErrs[i])
		}
	}
}

// TestLoginLockingOneUnknownUsernameDoesNotAffectAnother is the other
// direction of the same finding: unlike the old shared bucket, locking
// out one unknown username must leave a second, distinct unknown
// username with its own full budget.
func TestLoginLockingOneUnknownUsernameDoesNotAffectAnother(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }

	var lastErr error
	for i := 0; i < 10; i++ {
		_, _, lastErr = svc.Login(ctx, "locked-out-unknown-user", "wrong password", "", fmt.Sprintf("198.51.100.%d", 10+i))
	}
	var lockout *api.LockoutError
	if !errors.As(lastErr, &lockout) {
		t.Fatalf("expected locked-out-unknown-user to be locked out, got %v", lastErr)
	}

	_, _, err := svc.Login(ctx, "a-different-unknown-user", "wrong password", "", "198.51.100.200")
	if !errors.Is(err, api.ErrInvalidCredentials) {
		t.Errorf("a different, never-tried unknown username after another's lockout = %v, want ErrInvalidCredentials (unaffected)", err)
	}

	// And a real account is unaffected too.
	if _, _, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "198.51.100.201"); err != nil {
		t.Errorf("Login for the real account after an unrelated unknown username's lockout: %v", err)
	}
}

// TestActivateTOTPTwiceForSameSecretActivatesOnlyOnce is a deterministic
// counterpart to TestConfirmTOTPConcurrentRaceActivatesOnlyOnce below,
// which only catches a regression here probabilistically — removing the
// "if !activated" guard in ConfirmTOTP was caught in just 1 of 3 runs of
// that goroutine-based test. Calling AuthStore.ActivateTOTP twice,
// sequentially, for the exact same pending secret must accept the first
// and refuse the second every single time, not just most of the time.
func TestActivateTOTPTwiceForSameSecretActivatesOnlyOnce(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	pending := []byte("a pending secret, already machine-key-encrypted")
	if err := svc.Store.SetPendingTOTPSecret(ctx, u.ID, pending); err != nil {
		t.Fatalf("SetPendingTOTPSecret: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	first, err := svc.Store.ActivateTOTP(ctx, u.ID, pending, now, 42, pending)
	if err != nil {
		t.Fatalf("first ActivateTOTP: %v", err)
	}
	if !first {
		t.Fatal("expected the first ActivateTOTP for a still-pending secret to succeed")
	}

	second, err := svc.Store.ActivateTOTP(ctx, u.ID, pending, now, 42, pending)
	if err != nil {
		t.Fatalf("second ActivateTOTP: %v", err)
	}
	if second {
		t.Error("a second ActivateTOTP for the same already-activated pending secret must return false, deterministically — not just usually")
	}
}

// TestConfirmTOTPRevokesOtherSessionsKeepsCaller is #137's acceptance
// test: confirming TOTP from one session leaves that session valid and
// revokes every other session of the same account immediately; other
// accounts' sessions are untouched.
func TestConfirmTOTPRevokesOtherSessionsKeepsCaller(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	u, tokenA, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	_, tokenB, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatalf("Login for second session: %v", err)
	}

	viewerHash, err := auth.HashPassword("viewer password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	viewer := &api.User{ID: "viewer-id", Username: "viewer", Role: "viewer", PasswordHash: viewerHash, CreatedAt: svc.Now()}
	if err := svc.Store.CreateUser(ctx, viewer); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	_, viewerToken, err := svc.Login(ctx, "viewer", "viewer password", "", "")
	if err != nil {
		t.Fatalf("Login viewer: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, auth.HashSessionToken(tokenA)); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, tokenA); err != nil {
		t.Errorf("session A should remain valid: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, tokenB); !errors.Is(err, api.ErrSessionInvalid) {
		t.Errorf("session B = %v, want ErrSessionInvalid", err)
	}
	if _, err := svc.ValidateSession(ctx, viewerToken); err != nil {
		t.Errorf("viewer session should be untouched: %v", err)
	}
}

// TestConfirmTOTPInvalidCodeLeavesOtherSessions verifies revocation is
// tied to a successful activation: a failed confirmTotp leaves every
// session of the account unchanged.
func TestConfirmTOTPInvalidCodeLeavesOtherSessions(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	u, tokenA, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	_, tokenB, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatalf("Login for second session: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	if _, _, err := svc.EnrollTOTP(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, "000000", auth.HashSessionToken(tokenA)); !errors.Is(err, api.ErrTOTPInvalid) {
		t.Fatalf("ConfirmTOTP with a wrong code = %v, want ErrTOTPInvalid", err)
	}

	if _, err := svc.ValidateSession(ctx, tokenA); err != nil {
		t.Errorf("session A should remain valid after a failed confirm: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, tokenB); err != nil {
		t.Errorf("session B should remain valid after a failed confirm: %v", err)
	}
}

// TestConfirmTOTPConcurrentRaceActivatesOnlyOnce is the review finding
// this issue closes: confirmTotp's activation is a conditional write, not
// a read-validate-write pair, so two concurrent calls presenting the same
// still-valid code can never both activate it.
func TestConfirmTOTPConcurrentRaceActivatesOnlyOnce(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}

	const attempts = 10
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := svc.ConfirmTOTP(ctx, u.ID, code, "")
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, api.ErrTOTPInvalid):
				// A genuine race against ActivateTOTP's conditional
				// write: this call read the same pending secret the
				// winner did, but its own UPDATE's WHERE clause no
				// longer matched by the time it ran.
			case errors.Is(err, api.ErrTOTPNotPending):
				// The far more common outcome in practice: by the time
				// this call's own GetUserByID read the row, the winner
				// had already cleared totp_pending_secret entirely, so
				// there was nothing left even to attempt activating.
				// Either way, activation itself never ran twice.
			default:
				t.Errorf("unexpected ConfirmTOTP error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("%d of %d concurrent ConfirmTOTP calls for the same pending secret succeeded, want exactly 1", got, attempts)
	}
}

// TestLoginConcurrentSameTOTPCodeAcceptedOnlyOnce is the review finding
// this issue closes: two concurrent logins presenting the same TOTP code
// must never both succeed — the totp_last_step write is conditional, not
// a read-validate-write pair.
func TestLoginConcurrentSameTOTPCodeAcceptedOnlyOnce(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	u, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	secret, _, err := svc.EnrollTOTP(ctx, u.ID, "", "")
	if err != nil {
		t.Fatalf("EnrollTOTP: %v", err)
	}
	code, err := auth.CurrentCode(secret, now)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}
	if err := svc.ConfirmTOTP(ctx, u.ID, code, ""); err != nil {
		t.Fatalf("ConfirmTOTP: %v", err)
	}

	later := now.Add(30 * time.Second)
	svc.Now = func() time.Time { return later }
	loginCode, err := auth.CurrentCode(secret, later)
	if err != nil {
		t.Fatalf("CurrentCode: %v", err)
	}

	const attempts = 10
	var wg sync.WaitGroup
	var successes atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Login(ctx, "admin", "correct horse battery staple", loginCode, "")
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, api.ErrTOTPInvalid):
				// expected for every loser of the race.
			default:
				var lockout *api.LockoutError
				if !errors.As(err, &lockout) {
					t.Errorf("unexpected Login error: %v", err)
				}
				// A concurrent burst against the same account can also
				// trip the ordinary login rate limiter — this test's own
				// property (never more than one success) holds either
				// way.
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("%d of %d concurrent logins with the same TOTP code succeeded, want exactly 1", got, attempts)
	}
}

// TestLoginConcurrentFailuresLockOutAtomically is the review finding this
// issue closes: Login's rate limiting must reserve each attempt before
// doing any password work, atomically, so a burst of concurrent guesses
// can never all pass the check before any of them is counted — closing
// the race that otherwise lets an unlimited number of concurrent attempts
// through regardless of how many arrive together.
func TestLoginConcurrentFailuresLockOutAtomically(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	const (
		attempts = 30
		// mirrors internal/auth's own limiterFreeAttempts (5) + 1 — not
		// exported, so pinned here as a plain literal with this comment
		// rather than an import-cycle-inducing reference.
		maxAdmittedBeforeLockout = 6
	)
	var wg sync.WaitGroup
	var invalidCreds, lockouts atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Login(ctx, "admin", "wrong password", "", "203.0.113.99")
			switch {
			case errors.Is(err, api.ErrInvalidCredentials):
				invalidCreds.Add(1)
			default:
				var lockout *api.LockoutError
				if errors.As(err, &lockout) {
					lockouts.Add(1)
				} else {
					t.Errorf("unexpected Login error: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if got := invalidCreds.Load(); got > int32(maxAdmittedBeforeLockout) {
		t.Errorf("%d concurrent attempts ran real password verification before the lockout engaged, want at most %d", got, maxAdmittedBeforeLockout)
	}
	if got := invalidCreds.Load() + lockouts.Load(); got != int32(attempts) {
		t.Errorf("invalidCreds(%d)+lockouts(%d) = %d, want %d (every attempt accounted for)", invalidCreds.Load(), lockouts.Load(), got, attempts)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	_, token, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.7")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, api.ErrSessionInvalid) {
		t.Errorf("ValidateSession after logout = %v, want ErrSessionInvalid", err)
	}
}

func TestValidateSessionExpired(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	svc.Now = func() time.Time { return now }
	_, token, err := svc.Login(ctx, "admin", "correct horse battery staple", "", "203.0.113.8")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	svc.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, api.ErrSessionInvalid) {
		t.Errorf("ValidateSession after expiry = %v, want ErrSessionInvalid", err)
	}
}

func TestSetupGateBlocksUntilAdminExists(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()

	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	gated := api.SetupGate(inner, svc.Store, "/api/v1")

	req := httptest.NewRequest("GET", "/api/v1/jobs", nil)
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	if called {
		t.Error("an ordinary operation must not reach the inner handler before an admin exists")
	}
	if rec.Code != 409 {
		t.Errorf("status = %d, want 409", rec.Code)
	}

	for _, path := range []string{"/api/v1/setup/status", "/api/v1/setup/admin"} {
		called = false
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, req)
		if !called {
			t.Errorf("exempt path %s must reach the inner handler even before an admin exists", path)
		}
	}

	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	called = false
	req = httptest.NewRequest("GET", "/api/v1/jobs", nil)
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	if !called {
		t.Error("once an admin exists, an ordinary operation must reach the inner handler")
	}
}
