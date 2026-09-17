package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHashPasswordVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("VerifyPassword should accept the correct password")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Error("VerifyPassword should reject a wrong password")
	}
}

func TestHashPasswordUniqueSalt(t *testing.T) {
	h1, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Error("two hashes of the same password must differ (distinct salts)")
	}
}

func TestVerifyPasswordMalformedHash(t *testing.T) {
	if VerifyPassword("not a valid hash", "anything") {
		t.Error("a malformed hash must never verify")
	}
}

func TestHashAndVerifyPasswordContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	hash, err := HashPasswordContext(ctx, "correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPasswordContext: %v", err)
	}
	ok, err := VerifyPasswordContext(ctx, hash, "correct horse battery staple")
	if err != nil {
		t.Fatalf("VerifyPasswordContext: %v", err)
	}
	if !ok {
		t.Error("VerifyPasswordContext should accept the correct password")
	}
	ok, err = VerifyPasswordContext(ctx, hash, "wrong password")
	if err != nil {
		t.Fatalf("VerifyPasswordContext: %v", err)
	}
	if ok {
		t.Error("VerifyPasswordContext should reject a wrong password")
	}
}

// TestAcquirePasswordWorkCapsConcurrency is the review finding this issue
// closes: an unbounded number of concurrent login attempts must never
// each get to run their own argon2id work at once (measured before this
// fix: daemon RSS went from 1.0 GB to 2.66 GB after 40 concurrent
// unauthenticated logins) — acquirePasswordWork's channel-backed
// semaphore is what bounds that, derived from CPU count but clamped to
// [1, 4].
func TestAcquirePasswordWorkCapsConcurrency(t *testing.T) {
	capacity := passwordWorkConcurrency()
	if capacity < 1 || capacity > 4 {
		t.Fatalf("passwordWorkConcurrency() = %d, want a value in [1, 4]", capacity)
	}

	releases := make([]func(), 0, capacity)
	for i := 0; i < capacity; i++ {
		release, err := acquirePasswordWork(context.Background())
		if err != nil {
			t.Fatalf("acquire %d/%d: %v", i+1, capacity, err)
		}
		releases = append(releases, release)
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})

	original := passwordWorkTimeout
	passwordWorkTimeout = 50 * time.Millisecond
	t.Cleanup(func() { passwordWorkTimeout = original })

	if _, err := acquirePasswordWork(context.Background()); !errors.Is(err, ErrPasswordWorkBusy) {
		t.Errorf("acquiring past capacity = %v, want ErrPasswordWorkBusy", err)
	}

	release := releases[len(releases)-1]
	releases = releases[:len(releases)-1]
	release()

	freed, err := acquirePasswordWork(context.Background())
	if err != nil {
		t.Fatalf("acquire after a release: %v", err)
	}
	freed()
}

func TestAcquirePasswordWorkRespectsContextCancellation(t *testing.T) {
	capacity := passwordWorkConcurrency()
	releases := make([]func(), 0, capacity)
	for i := 0; i < capacity; i++ {
		release, err := acquirePasswordWork(context.Background())
		if err != nil {
			t.Fatalf("acquire %d/%d: %v", i+1, capacity, err)
		}
		releases = append(releases, release)
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquirePasswordWork(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("acquiring with an already-cancelled context = %v, want context.Canceled", err)
	}
}

// TestPasswordWorkNeverExceedsConcurrencyCap drives real concurrent
// password work and confirms the number actually running at once never
// exceeds passwordWorkConcurrency(), no matter how many callers arrive
// together.
func TestPasswordWorkNeverExceedsConcurrencyCap(t *testing.T) {
	const goroutines = 20
	var current, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquirePasswordWork(context.Background())
			if err != nil {
				t.Errorf("acquirePasswordWork: %v", err)
				return
			}
			defer release()

			n := current.Add(1)
			defer current.Add(-1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
		}()
	}
	wg.Wait()

	if got, want := peak.Load(), int32(passwordWorkConcurrency()); got > want {
		t.Errorf("peak concurrent password-work holders = %d, want at most %d", got, want)
	}
}
