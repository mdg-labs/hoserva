package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Rate-limit tuning (doc 01 §7): the first few failures cost nothing —
// mistyping a password shouldn't lock anyone out — and backoff only
// starts past that, doubling each additional failure up to a one-hour
// ceiling.
const (
	limiterFreeAttempts = 5
	limiterBaseDelay    = 30 * time.Second
	limiterMaxDelay     = time.Hour
	// limiterEntryTTL bounds how long a subject's failure history is kept
	// once it stops being touched — comfortably past limiterMaxDelay, so
	// pruning never removes an entry that is still meaningfully locked
	// out, but still finite, so a subject nothing ever unlocks (an idle
	// address, an idle account) doesn't hold its entry forever.
	limiterEntryTTL = 2 * time.Hour
	// limiterPruneInterval throttles how often Reserve actually walks
	// both tables: a full scan on every single call makes Reserve's own
	// cost grow with however many subjects have ever been tried (measured
	// at 1.68ms per call at 100k entries, under the global lock), which
	// under a sustained flood turns every login — including unrelated
	// ones — quadratic in the flood's own size. Pruning at most once per
	// interval keeps that scan's amortized cost bounded regardless of
	// request rate; the address table still can't grow without limit
	// between sweeps because of limiterMaxAddresses below, and the account
	// table needs no such cap at all — see SubjectKind's own doc comment.
	limiterPruneInterval = time.Second
	// limiterMaxAddresses hard-caps how many source-address subjects the
	// limiter tracks at once. There is no equivalent cap for the real
	// account table (SubjectAccount): a real-account subject is only ever
	// created for a username AuthService has already looked up as an
	// actual account (see SubjectKind's own doc comment), so that table's
	// size is bounded by how many accounts actually exist — never by how
	// many login attempts arrive — and capping or evicting from it would
	// only ever risk lifting a real account's own lockout early for no
	// reason. A username that isn't a real account gets its own subject in
	// the separate, capped SubjectUnknownAccount table instead
	// (limiterMaxUnknownAccounts).
	limiterMaxAddresses = 10_000
	// limiterMaxUnknownAccounts hard-caps how many distinct nonexistent
	// usernames the limiter tracks at once, exactly like limiterMaxAddresses
	// does for source addresses: an unknown username costs a Login caller
	// nothing to generate, so without a cap and eviction here a flood of
	// distinct nonexistent names would grow this table without bound.
	limiterMaxUnknownAccounts = 10_000
	// limiterCapacityRetryAfter is handed back when a brand new address or
	// unknown-account subject arrives with its table already full and
	// every entry in it currently locked out (evictLocked finds nothing
	// safe to evict) — a short, fixed wait rather than a per-subject
	// lockout, since nothing about *this* subject's own history caused the
	// refusal; the table has room again well before this elapses, once any
	// of those lockouts itself expires.
	limiterCapacityRetryAfter = limiterBaseDelay
)

// SubjectKind tells Reserve, Release and Len which of the limiter's three
// independent tables a subject belongs to (doc 01 §7's own "per account
// and per source address" split, plus the unknown-username table below).
//
// This split, and the fact that only the address and unknown-account
// tables are ever capped or evicted from, is the fix for a review
// finding: filling one shared table with unlocked address subjects could
// evict a real account subject with a partial failure count, and filling
// it entirely with locked subjects could evict — and thereby lift — an
// active account lockout, in both cases from a caller that only ever
// rotates its own source address, at no password-verification cost.
// Keeping the real-account table separate makes an address-only, or
// unknown-username-only, flood structurally unable to touch a real
// account's own entry at all, not merely unlikely to.
type SubjectKind int

const (
	// SubjectAccount identifies a real account's own subject: never
	// evicted, and never capped — see limiterMaxAddresses's own doc
	// comment for why that is safe.
	SubjectAccount SubjectKind = iota
	// SubjectAddress identifies a per-source-address subject: capped at
	// limiterMaxAddresses, and one of the two kinds evictLocked ever
	// touches.
	SubjectAddress
	// SubjectUnknownAccount identifies a per-username subject for a login
	// attempt against a username that isn't a real account: capped at
	// limiterMaxUnknownAccounts, and evicted the same way SubjectAddress
	// is. Kept in its own table, rather than the real SubjectAccount
	// table, so a flood of nonexistent usernames can never grow or evict
	// from the real-account table — see UnknownAccountSubject's own doc
	// comment for why each distinct username still gets its own subject
	// here, rather than all of them sharing one.
	SubjectUnknownAccount
)

// limiterEntry is one subject's failure history — a real account (e.g.
// "user:alice"), an unknown username (UnknownAccountSubject's hashed
// form), or a source address (e.g. "addr:v4:203.0.113.4"): login checks
// the address and exactly one of the two account-shaped subjects, so a
// lockout on either blocks the attempt (doc 01 §7's "per account and per
// source address").
type limiterEntry struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// Limiter tracks login failures per subject, in three independent tables
// (SubjectKind), with an injectable clock so backoff and lockout are
// testable without a real sleep.
type Limiter struct {
	now func() time.Time

	mu              sync.Mutex
	accounts        map[string]*limiterEntry
	addrs           map[string]*limiterEntry
	unknownAccounts map[string]*limiterEntry
	// unknownKey is UnknownAccountSubject's HMAC key: generated once per
	// Limiter, from crypto/rand, and never persisted or exposed — its only
	// purpose is to keep a tried username out of the limiter's own subject
	// strings.
	unknownKey []byte
	lastPrune  time.Time
}

// NewLimiter builds a Limiter. now defaults to time.Now.
func NewLimiter(now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	unknownKey := make([]byte, 32)
	if _, err := rand.Read(unknownKey); err != nil {
		panic(fmt.Sprintf("auth: generating limiter unknown-account key: %v", err))
	}
	return &Limiter{
		now:             now,
		accounts:        make(map[string]*limiterEntry),
		addrs:           make(map[string]*limiterEntry),
		unknownAccounts: make(map[string]*limiterEntry),
		unknownKey:      unknownKey,
	}
}

// UnknownAccountSubject returns the SubjectUnknownAccount subject for a
// login attempt against username, which AuthService has already looked
// up and confirmed doesn't name a real account. It is HMAC-SHA256 of
// username under a key generated once per Limiter (never persisted,
// never logged) — not username itself — so the limiter's own state never
// holds a tried username verbatim, while two Reserve calls for the same
// username still always land on the same subject.
//
// Each distinct username gets its own subject, deliberately, rather than
// every unknown username sharing one: a review finding closed by this —
// a shared bucket meant a handful of failed logins against *any* unknown
// names locked out every other unknown name too, so a fresh unknown name
// then failed fast (locked out, no argon2id work) while a real username
// still failed slow (a full password check), letting a caller tell the
// two apart. Each username's own subject, capped and evicted exactly like
// SubjectAddress, keeps that from happening without reintroducing the
// unbounded growth the shared bucket existed to prevent — the cap
// (limiterMaxUnknownAccounts) does that instead.
func (l *Limiter) UnknownAccountSubject(username string) string {
	mac := hmac.New(sha256.New, l.unknownKey)
	mac.Write([]byte(username))
	return "user:unknown:" + hex.EncodeToString(mac.Sum(nil))
}

func (l *Limiter) tableFor(kind SubjectKind) map[string]*limiterEntry {
	switch kind {
	case SubjectAddress:
		return l.addrs
	case SubjectUnknownAccount:
		return l.unknownAccounts
	default:
		return l.accounts
	}
}

// cappedAt reports the table cap for kind, and whether it has one at all
// — SubjectAccount has none (limiterMaxAddresses's own doc comment).
func cappedAt(kind SubjectKind) (limit int, capped bool) {
	switch kind {
	case SubjectAddress:
		return limiterMaxAddresses, true
	case SubjectUnknownAccount:
		return limiterMaxUnknownAccounts, true
	default:
		return 0, false
	}
}

// Reserve atomically checks subject's lockout and, if it isn't currently
// locked out, counts this attempt against its failure budget before the
// caller does any password-verification work — closing the race a
// separate check-then-record pair leaves open: with that pair, N
// concurrent attempts against the same subject can all pass the check
// before any of them is recorded, so the lockout never engages no matter
// how many requests arrive together. ok is false, with retryAfter set,
// when subject is currently locked out, or when kind is capped
// (cappedAt) and its table is full with no entry this call is willing to
// evict; the caller must not do any password work in either case, and
// must not call Release either — Reserve only counts an attempt when it
// actually admits one.
//
// At most once per limiterPruneInterval, Reserve also prunes entries in
// every table whose failure history is both unlocked and older than
// limiterEntryTTL — throttled, rather than run on every call, so the
// full-table scan pruning needs never becomes each call's own cost under
// a sustained flood (limiterPruneInterval's own doc comment).
// Independently of that sweep, a capped table (cappedAt) is hard-capped
// at its own limit: once it is full, adding a new subject either evicts
// one unlocked entry (evictLocked) or, if every subject tracked in that
// table is currently locked out, refuses the new one outright rather
// than evicting a locked entry.
func (l *Limiter) Reserve(subject string, kind SubjectKind) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if now.Sub(l.lastPrune) >= limiterPruneInterval {
		l.pruneLocked(now)
		l.lastPrune = now
	}

	table := l.tableFor(kind)
	e, found := table[subject]
	if found && now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	if !found {
		if limit, capped := cappedAt(kind); capped && len(table) >= limit {
			if !l.evictLocked(table, now) {
				return false, limiterCapacityRetryAfter
			}
		}
		e = &limiterEntry{}
		table[subject] = e
	}
	e.failures++
	e.lastFailure = now
	if e.failures > limiterFreeAttempts {
		delay := limiterBaseDelay << uint(e.failures-limiterFreeAttempts-1)
		if delay <= 0 || delay > limiterMaxDelay { // overflow, or past the ceiling
			delay = limiterMaxDelay
		}
		e.lockedUntil = now.Add(delay)
	}
	return true, 0
}

// Release clears subject's failure history — call once the attempt
// Reserve admitted actually succeeds. A failed attempt needs no call at
// all: Reserve has already counted it.
func (l *Limiter) Release(subject string, kind SubjectKind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tableFor(kind), subject)
}

// Len reports how many subjects of kind the limiter is currently
// tracking — used only by tests, to confirm each table's own bound
// holds: the address and unknown-account tables never grow past their
// own cap (limiterMaxAddresses, limiterMaxUnknownAccounts), and the real
// account table never grows past however many accounts actually exist.
func (l *Limiter) Len(kind SubjectKind) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.tableFor(kind))
}

// pruneLocked removes every entry, in every table, that is both unlocked
// and older than limiterEntryTTL. Callers must already hold l.mu.
func (l *Limiter) pruneLocked(now time.Time) {
	for _, table := range [...]map[string]*limiterEntry{l.accounts, l.addrs, l.unknownAccounts} {
		for subject, e := range table {
			if now.Before(e.lockedUntil) {
				continue
			}
			if now.Sub(e.lastFailure) > limiterEntryTTL {
				delete(table, subject)
			}
		}
	}
}

// evictLocked drops one entry from table so a caller about to add a new
// subject to it (l.addrs or l.unknownAccounts, at its own cap) can do so
// without growing past it. It is never called for
// the real account table, and never touches it (SubjectKind's own doc
// comment: a real account's subject is never evicted, full stop).
//
// It only ever considers an entry that isn't currently enforcing a
// lockout — an attacker flooding fresh subjects to fill the table must
// never be able to use that to evict, and thereby lift, another
// subject's active lockout — and, among those, evicts the one with the
// fewest recorded failures, breaking ties by the oldest last-failure
// time. This means a flood of brand new subjects, which always start at
// one failure each, can never evict a genuine entry that has failed more
// than once: this is the fix for the review finding that the previous,
// map-iteration-order eviction picked an arbitrary unlocked entry —
// which could be a victim mid-way through its own free-attempt budget —
// instead of always preferring one of the flood's own fresh entries. It
// does not guarantee a victim's own count survives *every* possible
// table state: an attacker who pre-loads the table with entries already
// at or below the victim's own failure count (rather than flooding fresh
// ones) can still evict it — this only ever costs the attacker's own
// budget against those entries' addresses or usernames, never the
// victim's. Reports false, evicting nothing, only once every subject
// tracked in table is currently locked out; Reserve then refuses the new
// subject outright rather than evicting a locked one. Callers must
// already hold l.mu.
func (l *Limiter) evictLocked(table map[string]*limiterEntry, now time.Time) bool {
	var victimKey string
	var victim *limiterEntry
	for key, e := range table {
		if now.Before(e.lockedUntil) {
			continue
		}
		if victim == nil ||
			e.failures < victim.failures ||
			(e.failures == victim.failures && e.lastFailure.Before(victim.lastFailure)) {
			victimKey, victim = key, e
		}
	}
	if victim == nil {
		return false
	}
	delete(table, victimKey)
	return true
}
