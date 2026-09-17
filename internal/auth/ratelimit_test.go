package auth

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock lets tests advance time deterministically instead of sleeping
// (CLAUDE.md's injectable-clock requirement for rate limiting).
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestLimiterAllowsFreeAttempts(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts; i++ {
		if ok, _ := l.Reserve("user:alice", SubjectAccount); !ok {
			t.Fatalf("attempt %d should be allowed (within the free budget)", i)
		}
	}
}

func TestLimiterLocksOutAfterFreeAttempts(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		if ok, _ := l.Reserve("user:alice", SubjectAccount); !ok {
			t.Fatalf("attempt %d should still be allowed", i)
		}
	}
	ok, retryAfter := l.Reserve("user:alice", SubjectAccount)
	if ok {
		t.Fatal("expected a lockout after exceeding the free attempt budget")
	}
	if retryAfter <= 0 {
		t.Error("expected a positive retry-after duration")
	}
}

func TestLimiterBackoffIncreases(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve("user:bob", SubjectAccount)
	}
	_, firstRetry := l.Reserve("user:bob", SubjectAccount)
	if firstRetry <= 0 {
		t.Fatal("expected to be locked out after the free attempt budget")
	}

	// Advancing past the first lockout and reserving once more both lifts
	// it and counts one further failure, extending the backoff for the
	// *next* lockout.
	clock.advance(firstRetry)
	if ok, _ := l.Reserve("user:bob", SubjectAccount); !ok {
		t.Fatal("expected to be allowed again once the first lockout expired")
	}
	_, secondRetry := l.Reserve("user:bob", SubjectAccount)

	if secondRetry <= firstRetry {
		t.Errorf("expected exponential backoff to increase: first=%v second=%v", firstRetry, secondRetry)
	}
}

func TestLimiterUnlocksAfterWaiting(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve("user:carol", SubjectAccount)
	}
	_, retryAfter := l.Reserve("user:carol", SubjectAccount)
	clock.advance(retryAfter)

	if ok, _ := l.Reserve("user:carol", SubjectAccount); !ok {
		t.Error("expected the lockout to have expired after waiting retryAfter")
	}
}

func TestLimiterReleaseResetsFailures(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve("user:dave", SubjectAccount)
	}
	if ok, _ := l.Reserve("user:dave", SubjectAccount); ok {
		t.Fatal("expected a lockout before Release")
	}
	l.Release("user:dave", SubjectAccount)
	if ok, _ := l.Reserve("user:dave", SubjectAccount); !ok {
		t.Error("Release must clear the failure history")
	}
}

func TestLimiterSubjectsAreIndependent(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve("addr:203.0.113.1", SubjectAddress)
	}
	if ok, _ := l.Reserve("user:eve", SubjectAccount); !ok {
		t.Error("locking out an address must not lock out an unrelated username")
	}
}

// TestLimiterReserveIsAtomicUnderConcurrency is the review finding this
// issue closes: Allowed-then-RecordFailure left a window where N
// concurrent attempts against the same subject could all pass the check
// before any of them was recorded, so the lockout never engaged no
// matter how many requests arrived together. Reserve folds the check and
// the count into one locked operation, so no more than
// limiterFreeAttempts+1 concurrent attempts can ever be admitted before
// the rest are refused outright.
func TestLimiterReserveIsAtomicUnderConcurrency(t *testing.T) {
	l := NewLimiter(nil)

	const attempts = 50
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Reserve("user:race", SubjectAccount); ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()

	if got, want := admitted.Load(), int32(limiterFreeAttempts+1); got != want {
		t.Errorf("%d of %d concurrent Reserve calls were admitted, want exactly %d (the free-attempt budget)", got, attempts, want)
	}
}

// TestLimiterEntriesExpire is the review finding this issue closes: the
// entries table must not grow without bound just because an attacker
// tries an unlimited number of distinct source addresses, each only once.
func TestLimiterEntriesExpire(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	l.Reserve("addr:temp", SubjectAddress)
	if got := l.Len(SubjectAddress); got != 1 {
		t.Fatalf("Len(SubjectAddress) = %d, want 1", got)
	}

	clock.advance(limiterEntryTTL + time.Minute)
	l.Reserve("addr:other", SubjectAddress) // any call opportunistically prunes.

	if got := l.Len(SubjectAddress); got != 1 {
		t.Errorf("Len(SubjectAddress) = %d after the TTL passed, want 1 (only addr:other, with addr:temp pruned)", got)
	}
}

// TestLimiterEntriesDoNotExpireWhileLockedOut confirms pruning never
// discards an entry that is still actively enforcing a lockout, even if
// its last failure happened long enough ago that a naive age-only check
// would remove it — limiterEntryTTL is chosen to exceed limiterMaxDelay
// precisely so this can't happen, but this test pins the behaviour
// directly rather than relying on the two constants' relative sizes.
func TestLimiterEntriesDoNotExpireWhileLockedOut(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := NewLimiter(clock.now)

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve("user:frank", SubjectAccount)
	}
	_, retryAfter := l.Reserve("user:frank", SubjectAccount)
	if retryAfter <= 0 {
		t.Fatal("expected a lockout")
	}

	clock.advance(retryAfter / 2)
	l.Reserve("user:unrelated", SubjectAccount) // triggers a prune sweep mid-lockout.

	ok, _ := l.Reserve("user:frank", SubjectAccount)
	if ok {
		t.Error("a still-locked-out entry must not be pruned away early")
	}
}

// TestLimiterAddressEntryCountStaysBoundedUnderManyDistinctAddresses is the
// review finding this issue closes: even before limiterEntryTTL or
// limiterPruneInterval's own sweep ever has a chance to retire anything,
// the address table itself must never grow past limiterMaxAddresses — a
// flood of distinct source addresses, each only ever tried once, must not
// cost unbounded memory.
func TestLimiterAddressEntryCountStaysBoundedUnderManyDistinctAddresses(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const distinctAddresses = limiterMaxAddresses + 500
	for i := 0; i < distinctAddresses; i++ {
		l.Reserve("addr:"+strconv.Itoa(i), SubjectAddress)
	}

	if got := l.Len(SubjectAddress); got > limiterMaxAddresses {
		t.Errorf("Len(SubjectAddress) = %d after %d distinct addresses, want at most %d", got, distinctAddresses, limiterMaxAddresses)
	}
}

// TestLimiterAccountTableIsNeverCapped is the account-side half of the
// same finding, the other way around: the account table has no cap at
// all (SubjectKind's own doc comment) — it never needs one, since a real
// AuthService never Reserves an account subject that doesn't correspond
// to either a real account or the one shared "unknown username" bucket.
// This test pins that the table itself doesn't enforce any cap, so a
// caller that did try many distinct account subjects wouldn't have any of
// them silently evicted.
func TestLimiterAccountTableIsNeverCapped(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const distinctAccounts = limiterMaxAddresses + 500
	for i := 0; i < distinctAccounts; i++ {
		l.Reserve("user:"+strconv.Itoa(i), SubjectAccount)
	}

	if got := l.Len(SubjectAccount); got != distinctAccounts {
		t.Errorf("Len(SubjectAccount) = %d after %d distinct accounts, want exactly %d (the account table is never capped)", got, distinctAccounts, distinctAccounts)
	}
}

// TestLimiterAddressEvictionPrefersFreshEntriesOverAPartialVictim is the
// review finding this issue closes: the previous eviction picked an
// arbitrary unlocked entry in map-iteration order, which could be a
// victim mid-way through its own free-attempt budget instead of one of
// the flood's own brand new entries. A fresh address subject always
// starts at one failure, strictly fewer than a victim that has already
// failed more than once, so it must always be evicted first — the
// victim's own partial count must survive the flood entirely.
func TestLimiterAddressEvictionPrefersFreshEntriesOverAPartialVictim(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const victim = "addr:victim"
	const victimFailures = 3
	for i := 0; i < victimFailures; i++ {
		if ok, _ := l.Reserve(victim, SubjectAddress); !ok {
			t.Fatalf("victim's own attempt %d should not be locked out yet", i)
		}
	}

	// Filling the table well past its cap with fresh, distinct addresses
	// must never evict the victim: every one of those fresh entries has
	// fewer recorded failures (one) than the victim (three), so
	// evictAddrLocked must always prefer them.
	for i := 0; i < limiterMaxAddresses+200; i++ {
		l.Reserve("addr:flood:"+strconv.Itoa(i), SubjectAddress)
	}

	if got := l.Len(SubjectAddress); got > limiterMaxAddresses {
		t.Fatalf("Len(SubjectAddress) = %d after the flood, want at most %d", got, limiterMaxAddresses)
	}

	// The victim's own partial count must be exactly where it was left —
	// still able to take its remaining free attempts, not reset to zero
	// by having been evicted and silently re-created.
	for i := victimFailures; i < limiterFreeAttempts+1; i++ {
		if ok, _ := l.Reserve(victim, SubjectAddress); !ok {
			t.Fatalf("victim should still have free attempts left at count %d, was evicted by the flood", i)
		}
	}
	if ok, _ := l.Reserve(victim, SubjectAddress); ok {
		t.Error("victim should now be locked out, having spent its own full free-attempt budget — its partial count must have survived the flood untouched")
	}
}

// TestLimiterAddressLockedVictimSurvivesATableFullOfLockedEntries is the
// other half of the same finding: once every address subject the table
// tracks is itself locked out, a further flood must be refused outright —
// treated as rate-limited — rather than evicting any of them, including
// the victim.
func TestLimiterAddressLockedVictimSurvivesATableFullOfLockedEntries(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const victim = "addr:locked-victim"
	lockAddress := func(subject string) {
		for i := 0; i < limiterFreeAttempts+1; i++ {
			l.Reserve(subject, SubjectAddress)
		}
	}
	lockAddress(victim)
	if ok, retryAfter := l.Reserve(victim, SubjectAddress); ok {
		t.Fatalf("expected the victim to already be locked out, retryAfter=%v", retryAfter)
	}

	// Fill the rest of the table with other addresses, every one of them
	// also locked out — leaving evictAddrLocked with nothing it is
	// willing to evict.
	for i := 0; i < limiterMaxAddresses-1; i++ {
		lockAddress("addr:locked:" + strconv.Itoa(i))
	}
	if got := l.Len(SubjectAddress); got != limiterMaxAddresses {
		t.Fatalf("Len(SubjectAddress) = %d, want exactly %d before the refused attempt", got, limiterMaxAddresses)
	}

	// A brand new address subject must now be refused, not admitted by
	// evicting a locked one.
	if ok, retryAfter := l.Reserve("addr:one-more", SubjectAddress); ok {
		t.Error("expected a new address subject to be refused once the table is full of locked entries")
	} else if retryAfter <= 0 {
		t.Error("expected a positive retry-after duration for the refusal")
	}

	// The victim's own lockout must be completely untouched.
	if ok, _ := l.Reserve(victim, SubjectAddress); ok {
		t.Error("the victim's lockout must survive a table full of locked entries, not be evicted to make room")
	}
}

// TestLimiterUnknownAccountsGetDistinctSubjects is the review finding
// this issue closes: an untried unknown username must reach the same
// lockout threshold as any other untried username — real or unknown —
// independently, not share a single bucket with every other unknown
// name.
func TestLimiterUnknownAccountsGetDistinctSubjects(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	alice := l.UnknownAccountSubject("alice")
	bob := l.UnknownAccountSubject("bob")
	if alice == bob {
		t.Fatalf("UnknownAccountSubject(alice) = UnknownAccountSubject(bob) = %q, want distinct subjects", alice)
	}

	for i := 0; i < limiterFreeAttempts+1; i++ {
		l.Reserve(alice, SubjectUnknownAccount)
	}
	if ok, _ := l.Reserve(alice, SubjectUnknownAccount); ok {
		t.Fatal("expected alice's own subject to be locked out")
	}
	if ok, _ := l.Reserve(bob, SubjectUnknownAccount); !ok {
		t.Error("locking out one unknown username must not lock out a different one")
	}
}

// TestLimiterUnknownAccountSubjectIsStableAndKeyed pins
// UnknownAccountSubject's own documented properties: the same username
// always maps to the same subject (so its failure count accumulates
// correctly across calls), and the username itself never appears in the
// returned subject.
func TestLimiterUnknownAccountSubjectIsStableAndKeyed(t *testing.T) {
	l := NewLimiter(nil)

	const name = "someone-trying-to-log-in"
	a := l.UnknownAccountSubject(name)
	b := l.UnknownAccountSubject(name)
	if a != b {
		t.Errorf("UnknownAccountSubject(%q) returned different subjects across calls: %q vs %q", name, a, b)
	}
	if a == "" || strings.Contains(a, name) {
		t.Errorf("UnknownAccountSubject(%q) = %q, must not contain the username verbatim", name, a)
	}

	other := NewLimiter(nil)
	if got := other.UnknownAccountSubject(name); got == a {
		t.Error("two different Limiters produced the same UnknownAccountSubject for the same name, want each Limiter's own random key to make them differ")
	}
}

// TestLimiterUnknownAccountTableIsCappedAndEvicts is the unknown-account
// half of TestLimiterAddressEntryCountStaysBoundedUnderManyDistinctAddresses:
// a flood of distinct nonexistent usernames must not grow this table
// without bound either.
func TestLimiterUnknownAccountTableIsCappedAndEvicts(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const distinctNames = limiterMaxUnknownAccounts + 500
	for i := 0; i < distinctNames; i++ {
		l.Reserve(l.UnknownAccountSubject("nonexistent-"+strconv.Itoa(i)), SubjectUnknownAccount)
	}

	if got := l.Len(SubjectUnknownAccount); got > limiterMaxUnknownAccounts {
		t.Errorf("Len(SubjectUnknownAccount) = %d after %d distinct usernames, want at most %d", got, distinctNames, limiterMaxUnknownAccounts)
	}
}

// TestLimiterEvictionCanEvictAPartialVictimGivenAPreloadedTable pins
// evictLocked's actual, narrower guarantee — a review finding against an
// earlier version of this comment, which claimed a victim's own partial
// failure count "always" survives eviction. That holds against a flood
// of brand new entries (TestLimiterAddressEvictionPrefersFreshEntriesOverAPartialVictim),
// each starting at one failure, but "fewest failures wins" cuts the other
// way once an attacker deliberately drives its own flood entries' own
// failure counts *above* a victim's: this test reproduces the review
// finding's own repro (9,999 addresses held at limiterFreeAttempts
// failures each, one victim address at fewer, then one more address)
// and confirms the victim — having the fewest failures of anything in
// the table — is the one evicted, not one of the higher-failure
// preloaded entries.
func TestLimiterEvictionCanEvictAPartialVictimGivenAPreloadedTable(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := NewLimiter(clock.now)

	const victim = "addr:preload-victim"
	const victimFailures = limiterFreeAttempts - 1
	for i := 0; i < victimFailures; i++ {
		if ok, _ := l.Reserve(victim, SubjectAddress); !ok {
			t.Fatalf("victim's own attempt %d should not be locked out yet", i)
		}
	}

	// Fill the rest of the table with entries at limiterFreeAttempts
	// failures each — still unlocked (lockout starts on the failure
	// *past* limiterFreeAttempts), but each with strictly more failures
	// than the victim, so the victim, not one of these, is the minimum
	// evictLocked finds.
	for i := 0; i < limiterMaxAddresses-1; i++ {
		subject := "addr:preload:" + strconv.Itoa(i)
		for j := 0; j < limiterFreeAttempts; j++ {
			l.Reserve(subject, SubjectAddress)
		}
	}
	if got := l.Len(SubjectAddress); got != limiterMaxAddresses {
		t.Fatalf("Len(SubjectAddress) = %d, want exactly %d before the triggering attempt", got, limiterMaxAddresses)
	}

	// One more new address subject must be admitted (by evicting
	// something), and the victim's own entry — having strictly fewer
	// failures than every preloaded entry — must be the one evicted: a
	// white-box check against the table itself, since Reserve's own
	// return value can't otherwise distinguish "the victim's entry is
	// gone" from "the victim's entry is still there, untouched".
	if ok, _ := l.Reserve("addr:one-more", SubjectAddress); !ok {
		t.Fatal("expected the table to admit one more subject by evicting the smallest entry")
	}
	if _, stillPresent := l.addrs[victim]; stillPresent {
		t.Error("expected the victim's own entry to have been evicted in favor of a preloaded entry with more failures, but it is still present")
	}
	if got := l.Len(SubjectAddress); got != limiterMaxAddresses {
		t.Errorf("Len(SubjectAddress) = %d after the triggering attempt, want unchanged at the cap (%d)", got, limiterMaxAddresses)
	}
}

func TestAddressSubjectSharesACounterAcrossOneIPv6SlashSixtyFour(t *testing.T) {
	a := AddressSubject("2001:db8::1")
	b := AddressSubject("2001:db8::ffff:ffff:ffff:ffff")
	if a != b {
		t.Errorf("AddressSubject(2001:db8::1) = %q, AddressSubject(2001:db8::ffff:ffff:ffff:ffff) = %q, want the same /64 subject", a, b)
	}

	c := AddressSubject("2001:db8:0:1::1")
	if a == c {
		t.Errorf("AddressSubject returned the same subject %q for two addresses in different /64s", a)
	}
}

func TestAddressSubjectTreatsAnIPv4MappedAddressAsIPv4(t *testing.T) {
	mapped := AddressSubject("::ffff:203.0.113.5")
	plain := AddressSubject("203.0.113.5")
	if mapped != plain {
		t.Errorf("AddressSubject(::ffff:203.0.113.5) = %q, AddressSubject(203.0.113.5) = %q, want the same subject", mapped, plain)
	}

	other := AddressSubject("203.0.113.6")
	if mapped == other {
		t.Errorf("AddressSubject returned the same subject %q for two different IPv4 addresses", mapped)
	}
}

// TestAddressSubjectFallsBackToTheRawStringWhenUnparseable pins
// AddressSubject's documented behaviour for input that can't happen in
// practice (withSourceAddrMiddleware always hands it a valid IP) but is
// still handled rather than assumed.
func TestAddressSubjectFallsBackToTheRawStringWhenUnparseable(t *testing.T) {
	if got, want := AddressSubject("not-an-ip"), "addr:raw:not-an-ip"; got != want {
		t.Errorf("AddressSubject(%q) = %q, want %q", "not-an-ip", got, want)
	}
}

// TestAddressSubjectIPv6PrefixMatchesNetCIDRMask is a sanity check that
// the /64 truncation above actually discards the host bits, using
// net.CIDRMask directly rather than duplicating AddressSubject's own
// logic.
func TestAddressSubjectIPv6PrefixMatchesNetCIDRMask(t *testing.T) {
	ip := net.ParseIP("2001:db8:1234:5678:9999:aaaa:bbbb:cccc")
	want := "addr:v6:" + ip.Mask(net.CIDRMask(64, 128)).String()
	if got := AddressSubject(ip.String()); got != want {
		t.Errorf("AddressSubject(%s) = %q, want %q", ip, got, want)
	}
}

// TestAddressSubjectTreatsAllLoopbackAddressesAsOneSubject is the review
// finding this issue closes: a non-blocking finding's own reproduction
// filled and mostly locked the address table using tens of thousands of
// distinct addresses within 127.0.0.0/8 alone (a local unprivileged
// caller can bind any of them), which then refused a legitimate login
// from a genuinely different, fresh remote address once the table had no
// unlocked entry left to evict. Every loopback address — anywhere in
// 127.0.0.0/8, ::1, or an IPv4-mapped ::ffff:127.x.x.x — now shares one
// fixed subject, so that flood can occupy at most one entry, exactly
// like a single non-loopback address would.
func TestAddressSubjectTreatsAllLoopbackAddressesAsOneSubject(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.0.0.2", "127.255.255.254", "::1", "::ffff:127.0.0.1"}
	first := AddressSubject(loopback[0])
	for _, addr := range loopback[1:] {
		if got := AddressSubject(addr); got != first {
			t.Errorf("AddressSubject(%q) = %q, AddressSubject(%q) = %q, want the same loopback subject", loopback[0], first, addr, got)
		}
	}

	if got := AddressSubject("203.0.113.1"); got == first {
		t.Errorf("AddressSubject(203.0.113.1) = %q, want a subject distinct from the loopback one (%q)", got, first)
	}
}
