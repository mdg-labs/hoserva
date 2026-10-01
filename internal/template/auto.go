package template

import (
	"context"
	"log"
	"math/rand/v2"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	// openStaleAfter is how old the last check must be before opening the
	// catalog starts another (Q65).
	openStaleAfter = 15 * time.Minute
	// settingsPoll is how often the background loop reads the settings again
	// while waiting, so a changed interval applies without a restart. It is
	// a database read, never a request to the catalog host.
	settingsPoll = time.Minute
	// maxJitter is the most a wait is lengthened by, as a fraction of the
	// interval, so installs that started together do not check together.
	maxJitter = 0.10
)

// CatalogSettingsReader is the catalog settings the automatic checks follow.
type CatalogSettingsReader interface {
	CatalogSettings(ctx context.Context) (store.CatalogSettings, error)
}

// AutoRefresher starts the catalog checks nobody asked for by hand (doc 04
// §7, Q65): one per user-set interval, and one when the catalog is opened
// while the last is stale. It reads the settings on every decision, and with
// the interval off and check-on-open off it never starts a check.
type AutoRefresher struct {
	Refresher *Refresher
	Settings  CatalogSettingsReader
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Jitter returns a fraction in [0, 1) of the maximum jitter; nil means a
	// uniform random one.
	Jitter func() float64
	// Sleep waits d or until ctx ends and reports whether the wait ran to
	// the end; nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) bool
	// Logf reports a settings read that failed; nil means log.Printf.
	Logf func(format string, args ...any)
}

func (a *AutoRefresher) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *AutoRefresher) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (a *AutoRefresher) sleep(ctx context.Context, d time.Duration) bool {
	if a.Sleep != nil {
		return a.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *AutoRefresher) jitter() float64 {
	if a.Jitter != nil {
		return a.Jitter()
	}
	return rand.Float64()
}

// IntervalDuration is how long the interval setting waits between checks,
// and false for off.
func IntervalDuration(interval string) (time.Duration, bool) {
	switch interval {
	case store.CatalogInterval1h:
		return time.Hour, true
	case store.CatalogInterval6h:
		return 6 * time.Hour, true
	case store.CatalogInterval12h:
		return 12 * time.Hour, true
	case store.CatalogInterval24h:
		return 24 * time.Hour, true
	}
	return 0, false
}

// Run is the background loop: it returns only when ctx ends. A check is due
// one interval, plus its jitter, after the later of the daemon's start and
// the last finished check, whatever started that one, so a manual check or a
// check-on-open postpones it. While the interval is off, or the settings
// cannot be read, it waits and sends nothing.
func (a *AutoRefresher) Run(ctx context.Context) {
	start := a.now()
	var (
		planLast     time.Time
		planInterval time.Duration
		due          time.Time
	)
	for ctx.Err() == nil {
		s, err := a.Settings.CatalogSettings(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			a.logf("hoservad: reading the catalog settings: %v — no automatic catalog check until they can be read", err)
			a.sleep(ctx, settingsPoll)
			continue
		}
		interval, on := IntervalDuration(s.RefreshInterval)
		if !on {
			planInterval = 0
			a.sleep(ctx, settingsPoll)
			continue
		}
		last := start
		if res, ok := a.Refresher.Last(); ok && res.CheckedAt.After(last) {
			last = res.CheckedAt
		}
		if !last.Equal(planLast) || interval != planInterval {
			planLast, planInterval = last, interval
			due = last.Add(interval + time.Duration(a.jitter()*maxJitter*float64(interval)))
		}
		wait := due.Sub(a.now())
		if wait > 0 {
			a.sleep(ctx, min(wait, settingsPoll))
			continue
		}
		if _, err := a.Refresher.Check(ctx, TriggerInterval); err != nil {
			return
		}
	}
}

// CheckOnOpen is what listing the catalog calls: with check-on-open on and
// the last check older than 15 minutes (or none since the daemon started), it
// starts one check in the background and returns at once. While a check runs
// it starts none. A settings read that fails starts none.
func (a *AutoRefresher) CheckOnOpen(ctx context.Context) {
	s, err := a.Settings.CatalogSettings(ctx)
	if err != nil {
		a.logf("hoservad: reading the catalog settings: %v — the catalog was not checked on open", err)
		return
	}
	if !s.CheckOnOpen {
		return
	}
	if res, ok := a.Refresher.Last(); ok && a.now().Sub(res.CheckedAt) < openStaleAfter {
		return
	}
	a.Refresher.StartBackground(ctx, TriggerOpen)
}
