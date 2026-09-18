package pool

import (
	"errors"
	"fmt"
	"regexp"
)

// CacheMode is a share's cache mode (doc 02 §3, Q12): which branches its
// own per-share mergerfs mount unions, and in what order.
type CacheMode string

const (
	// CacheThenMove writes land on cache; the mover relocates them to
	// the array later. For media ingest, downloads.
	CacheThenMove CacheMode = "cache-then-move"
	// CacheOnly data lives on cache permanently and is never moved.
	// For appdata, databases — not covered by parity.
	CacheOnly CacheMode = "cache-only"
	// ArrayOnly writes bypass cache entirely. For bulk writes larger
	// than the cache.
	ArrayOnly CacheMode = "array-only"
)

// Share is one user-facing share (doc 02 §3): its cache mode picks its
// own mergerfs mount's branch list (Q12); its create policy (Q11) picks
// where a new file lands among those branches.
type Share struct {
	Name         string
	CacheMode    CacheMode
	CreatePolicy CreatePolicy
}

// shareNamePattern mirrors the convention scripts/devenv/lib.sh's own
// lab-id check uses (CLAUDE.md): a share name becomes a path segment in
// every branch this package builds, so it is validated once, here,
// rather than trusted from whatever called in.
var shareNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ErrInvalidShareName is ValidateShareName's refusal.
var ErrInvalidShareName = errors.New("pool: invalid share name")

// ValidateShareName refuses a share name that could turn a mergerfs
// branch or mount path into something other than what it looks like —
// a leading dot, a path separator, or "..".
func ValidateShareName(name string) error {
	if !shareNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidShareName, name)
	}
	return nil
}
