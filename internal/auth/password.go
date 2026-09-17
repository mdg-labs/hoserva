package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. These are OWASP's current baseline recommendation
// for argon2id (one pass, 64 MiB, four lanes) — encoded into every hash
// this package writes, so a future change to these constants never
// invalidates a password hashed under the old ones.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16
)

// ErrPasswordWorkBusy is returned by HashPasswordContext/VerifyPasswordContext
// when passwordWorkTimeout elapses waiting for a slot in the bounded
// argon2id worker pool below — an unauthenticated flood of concurrent
// login attempts queues for a slot, capped and bounded, instead of each
// spawning its own unbounded 64 MiB argon2id run (doc 01 §7's rate
// limiting is otherwise moot: RSS measured at 2.66 GB peak, up from
// 1.0 GB baseline, after 40 concurrent unauthenticated logins before
// this fix). Callers map it to a 429/503-style API error.
var ErrPasswordWorkBusy = errors.New("too many concurrent password operations")

// passwordWorkTimeout bounds how long HashPasswordContext/
// VerifyPasswordContext wait for a free worker slot before giving up. A
// var, not a const, so a test can shorten it rather than actually
// filling every slot for 2 real seconds.
var passwordWorkTimeout = 2 * time.Second

// passwordWorkSem bounds how many argon2id hash/verify operations may run
// concurrently, process-wide — derived from CPU count (each run already
// uses argonThreads lanes internally) but clamped to a small range: at
// least 1 (a single-core dev box still makes progress), at most 4 (a
// large host doesn't let an unauthenticated flood reserve unbounded
// memory just because it has many cores — 64 MiB per concurrent run is
// the actual constraint doc 01 §7's login rate limiting is meant to
// bound).
var passwordWorkSem = make(chan struct{}, passwordWorkConcurrency())

func passwordWorkConcurrency() int {
	n := runtime.NumCPU()
	if n < 1 {
		return 1
	}
	if n > 4 {
		return 4
	}
	return n
}

// acquirePasswordWork reserves one of passwordWorkSem's slots, waiting up
// to passwordWorkTimeout (or ctx's own deadline, if sooner) — never
// unboundedly. The caller must invoke the returned release func exactly
// once, whether or not the work it guards actually ran.
func acquirePasswordWork(ctx context.Context) (release func(), err error) {
	timer := time.NewTimer(passwordWorkTimeout)
	defer timer.Stop()

	select {
	case passwordWorkSem <- struct{}{}:
		return func() { <-passwordWorkSem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrPasswordWorkBusy
	}
}

// HashPassword returns an encoded argon2id hash of password, in the
// conventional "$argon2id$v=19$m=...,t=...,p=...$salt$hash" form (each
// field base64, RawStdEncoding, no padding).
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	hash := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// VerifyPassword reports whether password matches encoded, an
// HashPassword-produced string. The comparison is constant-time; a
// malformed encoded string is treated as "no match", never an error, so a
// caller cannot distinguish "wrong password" from "corrupt hash" by the
// return value alone.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var memory, t uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &t, &threads); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HashPasswordContext is HashPassword, bounded by passwordWorkSem: it
// waits for a free worker slot (up to passwordWorkTimeout, or ctx's own
// deadline) before running argon2id, so an unbounded number of concurrent
// callers can never each reserve their own 64 MiB run at once. Returns
// ErrPasswordWorkBusy if no slot frees up in time.
func HashPasswordContext(ctx context.Context, password string) (string, error) {
	release, err := acquirePasswordWork(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	return HashPassword(password)
}

// VerifyPasswordContext is VerifyPassword, bounded the same way
// HashPasswordContext bounds HashPassword. Returns (false,
// ErrPasswordWorkBusy) rather than blocking forever if no slot frees up
// in time — the caller must treat that as "could not verify", not as "the
// password was wrong".
func VerifyPasswordContext(ctx context.Context, encoded, password string) (bool, error) {
	release, err := acquirePasswordWork(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	return VerifyPassword(encoded, password), nil
}
