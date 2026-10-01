package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/api/gen/go/events"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/template"
)

// countingCatalogHost serves one archive and signature the way a static
// host does, answering 304 to a matching If-None-Match, and counts every
// request that reaches it.
type countingCatalogHost struct {
	mu       sync.Mutex
	archive  []byte
	sig      []byte
	requests int
	hold     chan struct{}
	status   int
}

func (c *countingCatalogHost) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func (c *countingCatalogHost) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = 0
}

func (c *countingCatalogHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.requests++
	hold, archive, sig, status := c.hold, c.archive, c.sig, c.status
	c.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	switch r.URL.Path {
	case "/catalog.tar.zst":
		const etag = `"served"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = w.Write(archive)
	case "/catalog.tar.zst.sig":
		_, _ = w.Write(sig)
	default:
		http.NotFound(w, r)
	}
}

type autoCatalog struct {
	h       *api.Handler
	host    *countingCatalogHost
	auto    *template.AutoRefresher
	clock   *testClock
	checked <-chan template.CheckResult
	serial  int64
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newAutoCatalog builds the handler as main.go does, with the settings store
// wired, a catalog host serving a catalog newer than the embedded one, and a
// clock the automatic checks read.
func newAutoCatalog(t *testing.T) *autoCatalog {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := startedTemplatesWithSettings(t, t.TempDir(), nil)
	auto, ok := h.CatalogOpen.(*template.AutoRefresher)
	if !ok {
		t.Fatalf("startTemplates left Handler.CatalogOpen = %T, so listing the catalog could never check on open", h.CatalogOpen)
	}
	if h.CatalogSettings == nil || h.CatalogChecks == nil {
		t.Fatal("startTemplates left the catalog settings store or the check hub nil")
	}
	// Listing the catalog checks on open by default, so the host is pointed
	// away from the real one before anything lists it.
	refresher := h.CatalogRefresh.(*template.Refresher)
	refresher.URL = "http://127.0.0.1:1"
	installed, err := h.Catalog.Index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	host := &countingCatalogHost{}
	host.archive = catalogArchive(t, installed.Serial+1)
	host.sig = ed25519.Sign(priv, host.archive)
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)

	refresher.URL = srv.URL
	refresher.Store.Key = pub
	clock := &testClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	refresher.Now = clock.Now
	auto.Now = clock.Now
	ch, unsubscribe := h.CatalogChecks.Subscribe()
	t.Cleanup(unsubscribe)
	return &autoCatalog{h: h, host: host, auto: auto, clock: clock, checked: ch, serial: installed.Serial}
}

func (a *autoCatalog) waitCheck(t *testing.T) template.CheckResult {
	t.Helper()
	select {
	case r := <-a.checked:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no catalog check finished")
		return template.CheckResult{}
	}
}

func TestStartTemplates_CatalogSettingsStartAtDailyAndOnAndChangeOneFieldAtATime(t *testing.T) {
	ctx := context.Background()
	a := newAutoCatalog(t)

	got, err := a.h.GetCatalogSettings(ctx)
	if err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval24h || !got.CheckOnOpen {
		t.Fatalf("default settings = %+v, %v, want 24h and on", got, err)
	}
	got, err = a.h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshInterval6h)})
	if err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval6h || !got.CheckOnOpen {
		t.Fatalf("after setting only the interval = %+v, %v", got, err)
	}
	got, err = a.h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{CheckOnOpen: apiv1.NewOptBool(false)})
	if err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval6h || got.CheckOnOpen {
		t.Fatalf("after setting only check-on-open = %+v, %v", got, err)
	}
	got, err = a.h.GetCatalogSettings(ctx)
	if err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval6h || got.CheckOnOpen {
		t.Fatalf("read back = %+v, %v", got, err)
	}

	_, err = a.h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval("2h")})
	if st := a.h.NewError(ctx, err); st.StatusCode != http.StatusBadRequest || st.Response.Code != "invalid_catalog_interval" {
		t.Fatalf("an unknown interval = %d %q, want 400 invalid_catalog_interval", st.StatusCode, st.Response.Code)
	}
	if got, _ := a.h.GetCatalogSettings(ctx); got.RefreshInterval != apiv1.CatalogRefreshInterval6h {
		t.Fatalf("a refused interval changed the settings: %+v", got)
	}
}

func TestStartTemplates_WithTheIntervalAndCheckOnOpenOffListCatalogSendsNoRequestAndOneRefreshSendsOne(t *testing.T) {
	ctx := context.Background()
	a := newAutoCatalog(t)
	if res, err := a.h.RefreshCatalog(ctx); err != nil || res.Outcome != apiv1.CatalogCheckOutcomeUpdated {
		t.Fatalf("first refresh = %+v, %v", res, err)
	}
	a.waitCheck(t)
	if _, err := a.h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{
		RefreshInterval: apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshIntervalOff),
		CheckOnOpen:     apiv1.NewOptBool(false),
	}); err != nil {
		t.Fatal(err)
	}
	a.host.reset()

	for i := 0; i < 100; i++ {
		a.clock.Advance(3 * time.Hour)
		if _, err := a.h.ListCatalog(ctx); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := a.host.count(); n != 0 {
		t.Fatalf("100 listings over 300 simulated hours with both settings off sent %d requests", n)
	}

	res, err := a.h.RefreshCatalog(ctx)
	if err != nil || res.Outcome != apiv1.CatalogCheckOutcomeUnchanged {
		t.Fatalf("manual refresh = %+v, %v", res, err)
	}
	if n := a.host.count(); n != 1 {
		t.Fatalf("one refreshCatalog sent %d requests, want exactly 1", n)
	}
}

func TestStartTemplates_ListCatalogChecksOnOpenInTheBackgroundAndAnswersFromTheOnDiskCopy(t *testing.T) {
	ctx := context.Background()
	a := newAutoCatalog(t)
	hold := make(chan struct{})
	a.host.mu.Lock()
	a.host.hold = hold
	a.host.mu.Unlock()

	list, err := a.h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Serial != a.serial {
		t.Fatalf("the list waited for the check: serial %d, want the on-disk %d", list.Serial, a.serial)
	}
	if _, err := a.h.ListCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	close(hold)
	res := a.waitCheck(t)
	if res.Outcome != template.OutcomeUpdated {
		t.Fatalf("the on-open check = %+v, want updated", res)
	}
	if n := a.host.count(); n != 2 {
		t.Fatalf("two listings while one check ran sent %d requests, want one check's archive and signature", n)
	}
	list, err = a.h.ListCatalog(ctx)
	if err != nil || list.Serial != a.serial+1 {
		t.Fatalf("the list after the check = serial %d, %v, want %d", list.Serial, err, a.serial+1)
	}
	if o, ok := list.LastOutcome.Get(); !ok || o != apiv1.CatalogCheckOutcomeUpdated {
		t.Fatalf("lastOutcome = %v, %v", o, ok)
	}

	a.host.reset()
	a.clock.Advance(14 * time.Minute)
	if _, err := a.h.ListCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := a.host.count(); n != 0 {
		t.Fatalf("a listing 14 minutes after the last check sent %d requests", n)
	}
}

func TestStartTemplates_EveryFinishedCheckIsAnnouncedAsACatalogEventOnTheEventsStream(t *testing.T) {
	a := newAutoCatalog(t)
	stream := httptest.NewServer(&api.EventsHandler{
		Hub:          job.NewHub(),
		CatalogHub:   a.h.CatalogChecks,
		Authenticate: func(*http.Request) error { return nil },
		KeepAlive:    time.Hour,
	})
	t.Cleanup(stream.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	frames := bufio.NewScanner(resp.Body)

	next := func() events.CatalogEvent {
		t.Helper()
		for frames.Scan() {
			line := frames.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev events.Event
			if err := ev.UnmarshalJSON([]byte(strings.TrimPrefix(line, "data: "))); err != nil {
				t.Fatalf("decoding %q: %v", line, err)
			}
			if ev.Type != events.CatalogEventEvent {
				t.Fatalf("event %q, want a catalog event", ev.Type)
			}
			return ev.CatalogEvent
		}
		t.Fatalf("the stream ended: %v", frames.Err())
		return events.CatalogEvent{}
	}

	if _, err := a.h.ListCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened := next()
	if opened.Event != "catalog" || opened.Data.Outcome != events.CatalogCheckOutcomeUpdated || opened.Data.CheckedAt.IsZero() {
		t.Fatalf("the on-open check was announced as %+v", opened)
	}

	if _, err := a.h.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	manual := next()
	if manual.Data.Outcome != events.CatalogCheckOutcomeUnchanged || manual.Data.CheckedAt.IsZero() {
		t.Fatalf("the manual check was announced as %+v", manual)
	}

	a.host.mu.Lock()
	a.host.status = http.StatusServiceUnavailable
	a.host.mu.Unlock()
	if _, err := a.h.RefreshCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := next()
	if failed.Data.Outcome != events.CatalogCheckOutcomeFailed || !failed.Data.Reason.IsSet() || failed.Data.Message.Or("") == "" {
		t.Fatalf("a failed check was announced as %+v", failed)
	}
}

func TestStartTemplates_ReturnsTheBackgroundLoopOnlyWithASettingsStore(t *testing.T) {
	h := startedTemplates(t, t.TempDir())
	if h.CatalogOpen != nil || h.CatalogSettings != nil {
		t.Fatal("startTemplates with no settings store wired automatic checks")
	}
	if h.CatalogChecks == nil {
		t.Fatal("startTemplates left no hub for catalog events even with no settings store")
	}
	if _, err := h.GetCatalogSettings(context.Background()); err == nil {
		t.Fatal("GetCatalogSettings with no store answered, want 501")
	}
}
