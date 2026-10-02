package template

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/store"
)

func TestRefresh_ASignatureThatDoesNotVerifyIsFetchedOnceMoreBeforeItIsReported(t *testing.T) {
	g := newRefreshRig(t)
	seed, seedSig := signed(t, g.priv, catalogEntries(10, "seed", map[string]int{"jellyfin": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	newArchive, newSig := signed(t, g.priv, catalogEntries(11, "published", map[string]int{"jellyfin": 1, "plex": 1}))
	g.host.serve(newArchive, newSig, `"v11"`)
	g.host.mu.Lock()
	g.host.sigQueue = [][]byte{seedSig}
	g.host.mu.Unlock()

	res := g.refresh(t)
	if res.Outcome != OutcomeUpdated || res.New != 1 {
		t.Fatalf("result = %+v, want updated: the second pair verifies", res)
	}
	if len(*g.notified) != 0 {
		t.Fatalf("a signature that verified on the second try notified: %v", *g.notified)
	}
	if reqs, _ := g.host.log(); len(reqs) != 4 {
		t.Fatalf("requests = %v, want the archive and signature twice", reqs)
	}
	if serial, _, _ := g.store.Serial(); serial != 11 {
		t.Fatalf("installed serial = %d", serial)
	}
}

func TestRefresh_ASignatureThatStillFailsAfterTheSecondFetchIsReportedAndNothingIsInstalled(t *testing.T) {
	g := newRefreshRig(t)
	seed, seedSig := signed(t, g.priv, catalogEntries(10, "seed", map[string]int{"jellyfin": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	before := tree(t, g.store.Dir)
	newArchive, _ := signed(t, g.priv, catalogEntries(11, "published", map[string]int{"jellyfin": 1}))
	g.host.serve(newArchive, seedSig, `"v11"`)

	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v, want failed bad_signature", res)
	}
	if reqs, _ := g.host.log(); len(reqs) != 4 {
		t.Fatalf("requests = %v, want exactly one more fetch of the pair and no third", reqs)
	}
	if len(*g.notified) != 1 {
		t.Fatalf("notifications = %v, want one", *g.notified)
	}
	equalTrees(t, tree(t, g.store.Dir), before)
}

type autoRig struct {
	*refreshRig
	finished chan CheckResult
}

func newAutoRig(t *testing.T) *autoRig {
	t.Helper()
	g := &autoRig{refreshRig: newRefreshRig(t), finished: make(chan CheckResult, 64)}
	g.refresher.Finished = func(r CheckResult) { g.finished <- r }
	return g
}

// badServed publishes an archive whose signature is another archive's, and
// the unsigned archive's digest tells two of them apart.
func (g *autoRig) badServed(t *testing.T, marker string) {
	t.Helper()
	archive, _ := signed(t, g.priv, catalogEntries(50, marker, map[string]int{"a": 1}))
	_, otherSig := signed(t, g.priv, catalogEntries(50, marker+"-other", map[string]int{"a": 1}))
	g.host.serve(archive, otherSig, `"bad-`+marker+`"`)
}

func TestRefresh_AnAutomaticCheckDoesNotRepeatANotificationForTheSameServedFailure(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	check := func(trigger Trigger) CheckResult {
		t.Helper()
		res, err := g.refresher.Check(ctx, trigger)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeFailed || res.Reason != ReasonBadSignature {
			t.Fatalf("result = %+v, want failed bad_signature", res)
		}
		return res
	}

	g.badServed(t, "one")
	check(TriggerInterval)
	check(TriggerOpen)
	check(TriggerInterval)
	if n := len(*g.notified); n != 1 {
		t.Fatalf("three automatic checks of the same bad archive raised %d notifications, want 1", n)
	}
	check(TriggerManual)
	if n := len(*g.notified); n != 2 {
		t.Fatalf("a manual check raised %d notifications in total, want it to report again (2)", n)
	}

	g.badServed(t, "two")
	check(TriggerOpen)
	if n := len(*g.notified); n != 3 {
		t.Fatalf("a different bad archive raised %d notifications in total, want 3", n)
	}

	g.host.mu.Lock()
	g.host.status = 503
	g.host.mu.Unlock()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonFetchFailed {
		t.Fatalf("result = %+v, want fetch_failed", res)
	}
	g.badServed(t, "two")
	check(TriggerInterval)
	if n := len(*g.notified); n != 3 {
		t.Fatalf("a network failure between two checks of the same bad archive made %d notifications in total, want 3", n)
	}

	g.publish(t, `"good"`, catalogEntries(60, "good", map[string]int{"a": 1}))
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Outcome != OutcomeUpdated {
		t.Fatalf("good check = %+v", res)
	}
	g.badServed(t, "two")
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonNotNewer && res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}
	if n := len(*g.notified); n != 4 {
		t.Fatalf("the same failure after a good check made %d notifications in total, want 4", n)
	}
}

func TestRefresh_AutomaticChecksDoNotDownloadAnArchiveThatAlreadyFailedVerification(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	g.badServed(t, "one")

	first, err := g.refresher.Check(ctx, TriggerInterval)
	if err != nil || first.Outcome != OutcomeFailed || first.Reason != ReasonBadSignature {
		t.Fatalf("first check = %+v, %v, want failed bad_signature", first, err)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 4 || bytes == 0 {
		t.Fatalf("first check: requests = %v, bytes = %d, want the pair fetched twice", reqs, bytes)
	}

	g.host.reset()
	for _, trigger := range []Trigger{TriggerInterval, TriggerOpen, TriggerInterval} {
		res, err := g.refresher.Check(ctx, trigger)
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeFailed || res.Reason != ReasonBadSignature || res.Message != first.Message {
			t.Fatalf("repeat check = %+v, want the first check's failure", res)
		}
	}
	reqs, bytes := g.host.log()
	if len(reqs) != 6 || bytes != 0 {
		t.Fatalf("requests = %v, bytes = %d, want a conditional archive and signature request per check and no body", reqs, bytes)
	}
	for i, req := range reqs {
		if i%2 == 0 && req != "/catalog.tar.zst inm=\"bad-one\"" {
			t.Fatalf("request %q, want the archive asked with the bad archive's ETag", req)
		}
		if i%2 == 1 && !strings.HasPrefix(req, "/catalog.tar.zst.sig inm=\"sig-") {
			t.Fatalf("request %q, want the signature asked with the ETag it failed with", req)
		}
	}
	if n := len(*g.notified); n != 1 {
		t.Fatalf("notifications = %d, want the one from the first check", n)
	}

	g.host.mu.Lock()
	g.host.status = 503
	g.host.mu.Unlock()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonFetchFailed {
		t.Fatalf("result = %+v, want fetch_failed", res)
	}
	g.badServed(t, "one")
	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v, want bad_signature", res)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 2 || bytes != 0 {
		t.Fatalf("after a network failure: requests = %v, bytes = %d, want a conditional archive and signature request and no body", reqs, bytes)
	}
}

func TestRefresh_AnAutomaticCheckFetchesAndVerifiesADifferentArchiveAfterAFailure(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	g.badServed(t, "one")
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}

	g.badServed(t, "two")
	g.host.reset()
	res, _ := g.refresher.Check(ctx, TriggerInterval)
	if res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v, want the new archive verified and refused", res)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 4 || bytes == 0 {
		t.Fatalf("requests = %v, bytes = %d, want the new pair fetched twice", reqs, bytes)
	}
	if n := len(*g.notified); n != 2 {
		t.Fatalf("notifications = %d, want one per distinct bad archive", n)
	}

	g.host.reset()
	_, _ = g.refresher.Check(ctx, TriggerInterval)
	if reqs, bytes := g.host.log(); len(reqs) != 2 || bytes != 0 {
		t.Fatalf("requests = %v, bytes = %d, want the new bad archive remembered in place of the first", reqs, bytes)
	}

	g.publish(t, `"good"`, catalogEntries(60, "good", map[string]int{"a": 1}))
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Outcome != OutcomeUpdated {
		t.Fatalf("result = %+v, want the fixed archive installed", res)
	}
}

// pair is an archive, its signature, and a signature over another archive.
func (g *autoRig) pair(t *testing.T, serial int64, marker string) (archive, sig, wrongSig []byte) {
	t.Helper()
	archive, sig = signed(t, g.priv, catalogEntries(serial, marker, map[string]int{"a": 1}))
	_, wrongSig = signed(t, g.priv, catalogEntries(serial, marker+"-other", map[string]int{"a": 1}))
	return archive, sig, wrongSig
}

func TestRefresh_AFixedSignatureOverAnUnchangedArchiveIsFetchedAndInstalledByTheNextAutomaticCheck(t *testing.T) {
	for _, tc := range []struct {
		name         string
		noValidators bool
	}{
		{"signature with an ETag", false},
		{"signature without validators", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newAutoRig(t)
			ctx := context.Background()
			g.host.noSigValidators = tc.noValidators
			archive, sig, wrongSig := g.pair(t, 50, "one")
			g.host.serve(archive, wrongSig, `"v50"`)
			if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
				t.Fatalf("result = %+v, want bad_signature", res)
			}

			g.host.reset()
			if res, _ := g.refresher.Check(ctx, TriggerOpen); res.Reason != ReasonBadSignature {
				t.Fatalf("unchanged archive and signature: result = %+v, want the remembered bad_signature", res)
			}
			reqs, bytes := g.host.log()
			if tc.noValidators {
				if len(reqs) != 2 || bytes == 0 || bytes > int64(len(wrongSig)) {
					t.Fatalf("requests = %v, bytes = %d, want the archive refused by 304 and only the signature body fetched", reqs, bytes)
				}
			} else if len(reqs) != 2 || bytes != 0 {
				t.Fatalf("requests = %v, bytes = %d, want two conditional requests and no body", reqs, bytes)
			}

			g.host.serve(archive, sig, `"v50"`)
			g.host.reset()
			res, err := g.refresher.Check(ctx, TriggerInterval)
			if err != nil || res.Outcome != OutcomeUpdated {
				t.Fatalf("result = %+v, %v, want the archive installed once its signature verifies", res, err)
			}
			reqs, bytes = g.host.log()
			if len(reqs) != 4 || bytes < int64(len(archive)) {
				t.Fatalf("requests = %v, bytes = %d, want the archive refused by 304, then the pair fetched in full", reqs, bytes)
			}
			if reqs[0] != "/catalog.tar.zst inm=\"v50\"" || reqs[2] != "/catalog.tar.zst inm=" {
				t.Fatalf("requests = %v, want the archive fetched unconditionally after the signature changed", reqs)
			}
			if serial, _, _ := g.store.Serial(); serial != 50 {
				t.Fatalf("installed serial = %d, want 50", serial)
			}

			g.host.reset()
			if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Outcome != OutcomeUnchanged {
				t.Fatalf("result = %+v, want unchanged", res)
			}
		})
	}
}

func TestRefresh_AChangedSignatureThatStillFailsIsFetchedWithItsArchiveOnceMoreAndRemembered(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	archive, _, wrongSig := g.pair(t, 50, "one")
	_, _, otherWrongSig := g.pair(t, 50, "two")
	g.host.serve(archive, wrongSig, `"v50"`)
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}

	g.host.serve(archive, otherWrongSig, `"v50"`)
	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v, want bad_signature", res)
	}
	if reqs, _ := g.host.log(); len(reqs) != 6 {
		t.Fatalf("requests = %v, want the 304 pair, then the pair fetched twice", reqs)
	}

	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 2 || bytes != 0 {
		t.Fatalf("requests = %v, bytes = %d, want the new signature remembered in place of the first", reqs, bytes)
	}
}

func TestRefresh_ARejectionThatIsAPropertyOfTheArchiveDoesNotAskForTheSignature(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	seed, seedSig := signed(t, g.priv, catalogEntries(10, "seed", map[string]int{"a": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	g.publish(t, `"old"`, catalogEntries(9, "older", map[string]int{"a": 1}))
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonNotNewer {
		t.Fatalf("result = %+v, want not_newer", res)
	}

	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonNotNewer {
		t.Fatalf("result = %+v, want the remembered not_newer", res)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 1 || bytes != 0 {
		t.Fatalf("requests = %v, bytes = %d, want one conditional archive request", reqs, bytes)
	}
}

func TestRefresh_AManualCheckAfterAFailureFetchesTheArchiveInFullAndReportsItAgain(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	g.badServed(t, "one")
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}

	g.host.reset()
	res, err := g.refresher.Refresh(ctx)
	if err != nil || res.Outcome != OutcomeFailed || res.Reason != ReasonBadSignature {
		t.Fatalf("manual check = %+v, %v, want failed bad_signature", res, err)
	}
	reqs, bytes := g.host.log()
	if len(reqs) != 4 || bytes == 0 {
		t.Fatalf("requests = %v, bytes = %d, want the archive and signature fetched in full, twice", reqs, bytes)
	}
	for _, req := range reqs {
		if strings.Contains(req, "bad-one") {
			t.Fatalf("a manual check sent the bad archive's validators: %v", reqs)
		}
	}
	if n := len(*g.notified); n != 2 {
		t.Fatalf("notifications = %d, want the manual check to report again", n)
	}

	g.host.reset()
	_, _ = g.refresher.Check(ctx, TriggerInterval)
	if reqs, bytes := g.host.log(); len(reqs) != 2 || bytes != 0 {
		t.Fatalf("automatic check after the manual one: requests = %v, bytes = %d, want conditional requests and no body", reqs, bytes)
	}
}

func TestRefresh_AGoodCheckForgetsTheArchiveThatFailedVerification(t *testing.T) {
	g := newAutoRig(t)
	ctx := context.Background()
	seed, seedSig := signed(t, g.priv, catalogEntries(10, "seed", map[string]int{"a": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	g.publish(t, `"v11"`, catalogEntries(11, "installed", map[string]int{"a": 1}))
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Outcome != OutcomeUpdated {
		t.Fatalf("result = %+v", res)
	}

	g.badServed(t, "one")
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}

	g.publish(t, `"v11"`, catalogEntries(11, "installed", map[string]int{"a": 1}))
	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Outcome != OutcomeUnchanged {
		t.Fatalf("result = %+v, want unchanged once the host serves the installed archive again", res)
	}

	g.badServed(t, "one")
	g.host.reset()
	if res, _ := g.refresher.Check(ctx, TriggerInterval); res.Reason != ReasonBadSignature {
		t.Fatalf("result = %+v", res)
	}
	if reqs, bytes := g.host.log(); len(reqs) != 4 || bytes == 0 {
		t.Fatalf("requests = %v, bytes = %d, want the same bad archive downloaded again after a good check", reqs, bytes)
	}
}

func TestRefresh_ANotificationThatCouldNotBeRaisedIsRaisedAgainByTheNextAutomaticCheck(t *testing.T) {
	g := newAutoRig(t)
	var calls int
	g.refresher.Notify = func(context.Context, CheckResult) error {
		calls++
		if calls == 1 {
			return errors.New("queue is full")
		}
		return nil
	}
	g.badServed(t, "one")
	for i := 0; i < 3; i++ {
		if _, err := g.refresher.Check(context.Background(), TriggerInterval); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("Notify was called %d times over three checks, want 2: retried once after failing, then suppressed", calls)
	}
}

func TestRefresh_FinishedIsCalledOnceForEveryFinishedCheckWhateverStartedIt(t *testing.T) {
	g := newAutoRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	ctx := context.Background()
	if _, err := g.refresher.Check(ctx, TriggerInterval); err != nil {
		t.Fatal(err)
	}
	if _, err := g.refresher.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if !g.refresher.StartBackground(ctx, TriggerOpen) {
		t.Fatal("StartBackground started nothing while no check was running")
	}
	got := []Outcome{(<-g.finished).Outcome, (<-g.finished).Outcome, (<-g.finished).Outcome}
	if got[0] != OutcomeUpdated || got[1] != OutcomeUnchanged || got[2] != OutcomeUnchanged {
		t.Fatalf("outcomes = %v", got)
	}
	select {
	case extra := <-g.finished:
		t.Fatalf("a fourth result: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRefresh_AFinishedCheckIsLastAndNoLongerRunningWhenFinishedAnnouncesIt(t *testing.T) {
	g := newAutoRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	type seen struct {
		last    CheckResult
		hasLast bool
		started bool
	}
	got := make(chan seen, 1)
	var once sync.Once
	g.refresher.Finished = func(r CheckResult) {
		once.Do(func() {
			last, ok := g.refresher.Last()
			got <- seen{last: last, hasLast: ok, started: g.refresher.StartBackground(context.Background(), TriggerOpen)}
		})
	}
	res, err := g.refresher.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := <-got
	if !s.hasLast || s.last.Outcome != res.Outcome || !s.last.CheckedAt.Equal(res.CheckedAt) {
		t.Fatalf("Last inside Finished = %+v, %v, want the finished check %+v", s.last, s.hasLast, res)
	}
	if !s.started {
		t.Fatal("a check asked for inside Finished joined the one that had just finished instead of starting its own")
	}
	if _, err := g.refresher.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRefresh_StartBackgroundStartsNothingWhileACheckIsRunning(t *testing.T) {
	g := newAutoRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	g.host.mu.Lock()
	g.host.hold = func() {
		once.Do(func() { close(started) })
		<-release
	}
	g.host.mu.Unlock()

	ctx := context.Background()
	if !g.refresher.StartBackground(ctx, TriggerOpen) {
		t.Fatal("the first StartBackground started nothing")
	}
	<-started
	if g.refresher.StartBackground(ctx, TriggerOpen) {
		t.Fatal("StartBackground started a second check while one was running")
	}
	close(release)
	<-g.finished
	if reqs, _ := g.host.log(); len(reqs) != 2 {
		t.Fatalf("requests = %v, want one check's archive and signature", reqs)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeSettings struct {
	mu  sync.Mutex
	s   store.CatalogSettings
	err error
}

func (f *fakeSettings) CatalogSettings(context.Context) (store.CatalogSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s, f.err
}

func (f *fakeSettings) set(interval string, onOpen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.s = store.CatalogSettings{RefreshInterval: interval, CheckOnOpen: onOpen}
}

type autoSetup struct {
	*autoRig
	clock    *fakeClock
	settings *fakeSettings
	auto     *AutoRefresher
	start    time.Time
}

func newAutoSetup(t *testing.T, interval string, onOpen bool) *autoSetup {
	t.Helper()
	g := newAutoRig(t)
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: start}
	g.refresher.Now = clock.Now
	settings := &fakeSettings{}
	settings.set(interval, onOpen)
	a := &AutoRefresher{
		Refresher: g.refresher,
		Settings:  settings,
		Now:       clock.Now,
		Jitter:    func() float64 { return 0.5 },
		Logf:      func(string, ...any) {},
	}
	return &autoSetup{autoRig: g, clock: clock, settings: settings, auto: a, start: start}
}

// run runs the loop with a clock that only moves when it sleeps. before is
// called at every wait, with the time elapsed since start, and the loop ends
// when the elapsed time passes until.
func (s *autoSetup) run(until time.Duration, before func(elapsed time.Duration)) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.auto.Sleep = func(ctx context.Context, d time.Duration) bool {
		elapsed := s.clock.Now().Sub(s.start)
		if elapsed >= until {
			cancel()
			return false
		}
		if before != nil {
			before(elapsed)
		}
		s.clock.Advance(d)
		return true
	}
	s.auto.Run(ctx)
}

func (s *autoSetup) checkTimes() []time.Duration {
	var out []time.Duration
	for {
		select {
		case r := <-s.finished:
			out = append(out, r.CheckedAt.Sub(s.start))
		default:
			return out
		}
	}
}

func TestAutoRefresher_WithTheIntervalAndCheckOnOpenOffNoRequestReachesTheHostExceptTheManualCheck(t *testing.T) {
	s := newAutoSetup(t, "off", false)
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	if res := s.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatalf("first manual check = %+v", res)
	}
	<-s.finished
	s.host.reset()

	s.run(200*time.Hour, func(elapsed time.Duration) {
		s.auto.CheckOnOpen(context.Background())
	})
	if reqs, _ := s.host.log(); len(reqs) != 0 {
		t.Fatalf("over 200 simulated hours with both off, the host saw %v", reqs)
	}

	if res := s.refresh(t); res.Outcome != OutcomeUnchanged {
		t.Fatalf("manual check = %+v", res)
	}
	if reqs, _ := s.host.log(); len(reqs) != 1 {
		t.Fatalf("one manual check sent %v, want exactly one request", reqs)
	}
}

func TestAutoRefresher_ChecksOncePerIntervalPlusJitterAndNeverMoreOftenThanTheInterval(t *testing.T) {
	s := newAutoSetup(t, "6h", false)
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))

	s.run(25*time.Hour, nil)
	times := s.checkTimes()
	step := 6*time.Hour + 18*time.Minute
	if len(times) != 3 || times[0] != step || times[1] != 2*step || times[2] != 3*step {
		t.Fatalf("checks at %v, want %v, %v, %v after the start (the interval plus half of the 10%% jitter each time)", times, step, 2*step, 3*step)
	}
}

func TestAutoRefresher_JitterIsBetweenNoneAndTenPercentOfTheInterval(t *testing.T) {
	for _, tc := range []struct {
		jitter float64
		want   time.Duration
	}{{0, time.Hour}, {0.999999, time.Hour + 6*time.Minute - time.Millisecond}} {
		s := newAutoSetup(t, "1h", false)
		s.auto.Jitter = func() float64 { return tc.jitter }
		s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
		s.run(80*time.Minute, nil)
		times := s.checkTimes()
		if len(times) != 1 || times[0] < tc.want-time.Second || times[0] > tc.want+time.Second {
			t.Fatalf("jitter %v: checks at %v, want one at about %v", tc.jitter, times, tc.want)
		}
	}
}

func TestAutoRefresher_PicksUpAChangedIntervalWithoutARestart(t *testing.T) {
	s := newAutoSetup(t, "24h", false)
	s.auto.Jitter = func() float64 { return 0 }
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))

	s.run(10*time.Hour, func(elapsed time.Duration) {
		switch {
		case elapsed >= 150*time.Minute && elapsed < 151*time.Minute:
			s.settings.set("1h", false)
		case elapsed >= 5*time.Hour && elapsed < 5*time.Hour+time.Minute:
			s.settings.set("off", false)
		}
	})
	times := s.checkTimes()
	want := []time.Duration{151 * time.Minute, 211 * time.Minute, 271 * time.Minute}
	if len(times) != len(want) || times[0] != want[0] || times[1] != want[1] || times[2] != want[2] {
		t.Fatalf("checks at %v, want %v: none while 24h was set, hourly once 1h was set 2h30m in (the first at once, as it was overdue), and none after off at 5h", times, want)
	}
}

func TestAutoRefresher_ACheckByAnyTriggerPostponesTheIntervalCheck(t *testing.T) {
	s := newAutoSetup(t, "6h", false)
	s.auto.Jitter = func() float64 { return 0 }
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))

	var manual bool
	s.run(7*time.Hour, func(elapsed time.Duration) {
		if !manual && elapsed >= 5*time.Hour {
			manual = true
			s.refresh(t)
		}
	})
	times := s.checkTimes()
	if len(times) != 1 {
		t.Fatalf("checks at %v, want only the manual one: the interval check moved to 6h after it", times)
	}
}

func TestAutoRefresher_ASettingsReadThatFailsSendsNothing(t *testing.T) {
	s := newAutoSetup(t, "1h", true)
	s.settings.mu.Lock()
	s.settings.err = fmt.Errorf("database is locked")
	s.settings.mu.Unlock()
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	s.run(30*time.Hour, func(time.Duration) { s.auto.CheckOnOpen(context.Background()) })
	if reqs, _ := s.host.log(); len(reqs) != 0 {
		t.Fatalf("with unreadable settings the host saw %v", reqs)
	}
}

func TestAutoRefresher_CheckOnOpenStartsABackgroundCheckOnlyWhenTheLastIsOlderThanFifteenMinutes(t *testing.T) {
	s := newAutoSetup(t, "off", true)
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	ctx := context.Background()

	s.auto.CheckOnOpen(ctx)
	if r := <-s.finished; r.Outcome != OutcomeUpdated {
		t.Fatalf("the first open's check = %+v", r)
	}
	s.host.reset()

	s.clock.Advance(14 * time.Minute)
	s.auto.CheckOnOpen(ctx)
	time.Sleep(50 * time.Millisecond)
	if reqs, _ := s.host.log(); len(reqs) != 0 {
		t.Fatalf("an open 14 minutes after the last check sent %v", reqs)
	}

	s.clock.Advance(2 * time.Minute)
	s.auto.CheckOnOpen(ctx)
	if r := <-s.finished; r.Outcome != OutcomeUnchanged {
		t.Fatalf("the check 16 minutes after the last = %+v", r)
	}
	if reqs, _ := s.host.log(); len(reqs) != 1 {
		t.Fatalf("requests = %v, want one conditional request", reqs)
	}
}

func TestAutoRefresher_CheckOnOpenAnswersWithoutWaitingAndStartsNoSecondCheck(t *testing.T) {
	s := newAutoSetup(t, "off", true)
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.host.mu.Lock()
	s.host.hold = func() {
		once.Do(func() { close(started) })
		<-release
	}
	s.host.mu.Unlock()

	returned := make(chan struct{})
	go func() {
		s.auto.CheckOnOpen(context.Background())
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("CheckOnOpen waited for the check it started")
	}
	<-started
	for i := 0; i < 5; i++ {
		s.auto.CheckOnOpen(context.Background())
	}
	close(release)
	<-s.finished
	select {
	case extra := <-s.finished:
		t.Fatalf("a second check finished: %+v", extra)
	case <-time.After(50 * time.Millisecond):
	}
	if reqs, _ := s.host.log(); len(reqs) != 2 {
		t.Fatalf("requests = %v, want one check's archive and signature", reqs)
	}
}

func TestAutoRefresher_CheckOnOpenOffStartsNothing(t *testing.T) {
	s := newAutoSetup(t, "1h", false)
	s.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"a": 1}))
	for i := 0; i < 10; i++ {
		s.clock.Advance(time.Hour)
		s.auto.CheckOnOpen(context.Background())
	}
	time.Sleep(50 * time.Millisecond)
	if reqs, _ := s.host.log(); len(reqs) != 0 {
		t.Fatalf("with check-on-open off the host saw %v", reqs)
	}
}

func TestIntervalDuration_NamesEveryAllowedIntervalAndOffIsNone(t *testing.T) {
	for in, want := range map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour} {
		if got, ok := IntervalDuration(in); !ok || got != want {
			t.Errorf("IntervalDuration(%q) = %v, %v", in, got, ok)
		}
	}
	for _, in := range []string{"off", "", "2h"} {
		if _, ok := IntervalDuration(in); ok {
			t.Errorf("IntervalDuration(%q) is an interval", in)
		}
	}
}
