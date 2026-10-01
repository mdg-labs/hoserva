package template

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// catalogEntries is a catalog archive's files: index.json listing each
// template at its revision, and a compose.yaml per template carrying marker,
// so two archives of one serial can differ.
func catalogEntries(serial int64, marker string, templates map[string]int) []tarEntry {
	ids := make([]string, 0, len(templates))
	for id := range templates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var listed []string
	entries := []tarEntry{{Name: "index.json"}}
	for _, id := range ids {
		listed = append(listed, fmt.Sprintf(`{"id":%q,"revision":%d,"title":%q,"categories":[],"docs":"https://example.com"}`, id, templates[id], id))
		entries = append(entries,
			tarEntry{Name: id + "/", Type: tar.TypeDir},
			reg(id+"/compose.yaml", fmt.Sprintf("name: %s # %s rev %d\n", id, marker, templates[id])))
	}
	entries[0] = reg("index.json", fmt.Sprintf(`{"schema":1,"serial":%d,"templates":[%s]}`, serial, strings.Join(listed, ",")))
	return entries
}

// fakeCatalogHost serves the catalog archive and signature the way a static
// host does, honouring If-None-Match, and counts requests and the bytes it
// sends.
type fakeCatalogHost struct {
	mu       sync.Mutex
	archive  []byte
	sig      []byte
	etag     string
	status   int
	requests []string
	bytes    int64
	hold     func()
}

func (f *fakeCatalogHost) serve(archive, sig []byte, etag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archive, f.sig, f.etag, f.status = archive, sig, etag, 0
}

func (f *fakeCatalogHost) log() ([]string, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), f.bytes
}

func (f *fakeCatalogHost) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests, f.bytes = nil, 0
}

func (f *fakeCatalogHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		hold()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.URL.Path+" inm="+r.Header.Get("If-None-Match"))
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	var body []byte
	switch r.URL.Path {
	case "/catalog.tar.zst":
		if f.etag != "" && r.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body = f.archive
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
	case "/catalog.tar.zst.sig":
		body = f.sig
	default:
		http.NotFound(w, r)
		return
	}
	n, _ := w.Write(body)
	f.bytes += int64(n)
}

type refreshRig struct {
	host      *fakeCatalogHost
	refresher *Refresher
	store     CatalogStore
	priv      ed25519.PrivateKey
	notified  *[]CheckResult
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	pub, priv := newKey(t)
	host := &fakeCatalogHost{}
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	store := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog"), Key: pub}
	var (
		mu       sync.Mutex
		notified []CheckResult
	)
	r := &Refresher{
		Store: store,
		URL:   srv.URL,
		Notify: func(_ context.Context, res CheckResult) error {
			mu.Lock()
			defer mu.Unlock()
			notified = append(notified, res)
			return nil
		},
	}
	return &refreshRig{host: host, refresher: r, store: store, priv: priv, notified: &notified}
}

func (g *refreshRig) publish(t *testing.T, etag string, entries []tarEntry) {
	t.Helper()
	archive, sig := signed(t, g.priv, entries)
	g.host.serve(archive, sig, etag)
}

func (g *refreshRig) refresh(t *testing.T) CheckResult {
	t.Helper()
	res, err := g.refresher.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return res
}

func TestDefaultCatalogURL_IsTheCatalogHostAndNeverTheGitHubAPI(t *testing.T) {
	if DefaultCatalogURL != "https://catalog.hoserva.dev/" {
		t.Fatalf("DefaultCatalogURL = %q", DefaultCatalogURL)
	}
	got, err := catalogURL((&Refresher{}).baseURL(), catalogArchiveName)
	if err != nil || got != "https://catalog.hoserva.dev/catalog.tar.zst" {
		t.Fatalf("archive URL = %q, %v", got, err)
	}
	got, err = catalogURL((&Refresher{}).baseURL(), catalogSignatureName)
	if err != nil || got != "https://catalog.hoserva.dev/catalog.tar.zst.sig" {
		t.Fatalf("signature URL = %q, %v", got, err)
	}
	for _, base := range []string{"https://api.github.com/repos/mdg-labs/hoserva-catalog/releases", "https://API.GITHUB.COM/", "ftp://catalog.hoserva.dev/", "catalog.hoserva.dev", ""} {
		if _, err := catalogURL(base, catalogArchiveName); err == nil {
			t.Errorf("catalogURL(%q) = nil error, want a refusal", base)
		}
	}
	if _, err := catalogURL("https://api.github.com.example.org/catalog/", catalogArchiveName); err != nil {
		t.Errorf("a host that only starts with api.github.com was refused: %v", err)
	}
}

func TestRefresh_InstallsANewerCatalogAndReportsWhatChanged(t *testing.T) {
	g := newRefreshRig(t)
	seed, seedSig := signed(t, g.priv, catalogEntries(10, "seed", map[string]int{"jellyfin": 1, "plex": 2, "gone": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	g.publish(t, `"v11"`, catalogEntries(11, "fetched", map[string]int{"jellyfin": 1, "plex": 3, "sonarr": 1, "radarr": 1}))

	res := g.refresh(t)
	if res.Outcome != OutcomeUpdated || res.New != 2 || res.Updated != 1 {
		t.Fatalf("result = %+v, want updated with 2 new and 1 updated (a template that only left the index counts as neither)", res)
	}
	if res.CheckedAt.IsZero() || time.Since(res.CheckedAt) > time.Minute {
		t.Fatalf("CheckedAt = %v", res.CheckedAt)
	}
	if len(*g.notified) != 0 {
		t.Fatalf("a successful check notified: %v", *g.notified)
	}
	serial, ok, err := g.store.Serial()
	if err != nil || !ok || serial != 11 {
		t.Fatalf("installed serial = %d, %v, %v", serial, ok, err)
	}
	if v := g.store.Validators(); v.ETag != `"v11"` {
		t.Fatalf("stored validators = %+v", v)
	}
	reqs, _ := g.host.log()
	if len(reqs) != 2 {
		t.Fatalf("requests = %v, want the archive and its signature", reqs)
	}
}

func TestRefresh_AFreshInstallCountsEveryTemplateAsNew(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, "", catalogEntries(5, "first", map[string]int{"a": 1, "b": 1}))
	res := g.refresh(t)
	if res.Outcome != OutcomeUpdated || res.New != 2 || res.Updated != 0 {
		t.Fatalf("result = %+v", res)
	}
	if v := g.store.Validators(); !v.empty() {
		t.Fatalf("validators stored although the server sent none: %+v", v)
	}
}

func TestRefresh_AnUnchangedCatalogDownloadsNothing(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(7, "x", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatalf("first check = %+v", res)
	}
	before := tree(t, g.store.Dir)
	g.host.reset()

	res := g.refresh(t)
	if res.Outcome != OutcomeUnchanged || res.New != 0 || res.Updated != 0 {
		t.Fatalf("second check = %+v, want unchanged", res)
	}
	reqs, bytes := g.host.log()
	if len(reqs) != 1 || reqs[0] != `/catalog.tar.zst inm="v1"` || bytes != 0 {
		t.Fatalf("requests = %v with %d bytes, want one conditional request for the archive and no body, and no signature request", reqs, bytes)
	}
	equalTrees(t, tree(t, g.store.Dir), before)
	if len(*g.notified) != 0 {
		t.Fatalf("an unchanged catalog notified: %v", *g.notified)
	}
}

func TestRefresh_ASeededCatalogTheServerStillServesIsUnchangedAndLearnsItsValidators(t *testing.T) {
	g := newRefreshRig(t)
	entries := catalogEntries(20, "same", map[string]int{"jellyfin": 1})
	archive, sig := signed(t, g.priv, entries)
	if _, err := g.store.Seed(archive, sig); err != nil {
		t.Fatal(err)
	}
	g.host.serve(archive, sig, `"v20"`)

	res := g.refresh(t)
	if res.Outcome != OutcomeUnchanged {
		t.Fatalf("result = %+v, want unchanged: the server holds the catalog already installed", res)
	}
	if len(*g.notified) != 0 {
		t.Fatalf("notified: %v", *g.notified)
	}
	if v := g.store.Validators(); v.ETag != `"v20"` {
		t.Fatalf("validators = %+v, want the response's", v)
	}
	g.host.reset()
	if res := g.refresh(t); res.Outcome != OutcomeUnchanged {
		t.Fatalf("next check = %+v", res)
	}
	reqs, bytes := g.host.log()
	if len(reqs) != 1 || bytes != 0 {
		t.Fatalf("the check after learning the validators sent %v with %d bytes", reqs, bytes)
	}
}

func TestRefresh_NeverReplacesTheCatalogWithAReplayedOlderOrForgedArchive(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v100"`, catalogEntries(100, "installed", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatalf("installing the starting catalog: %+v", res)
	}
	before := tree(t, g.store.Dir)
	beforeValidators := g.store.Validators()

	_, otherPriv := newKey(t)
	goodArchive, goodSig := signed(t, g.priv, catalogEntries(100, "forged-same-serial", map[string]int{"jellyfin": 9, "evil": 1}))
	olderArchive, olderSig := signed(t, g.priv, catalogEntries(99, "older", map[string]int{"jellyfin": 1}))
	tampered, _ := signed(t, g.priv, catalogEntries(101, "tampered", map[string]int{"jellyfin": 1}))
	_, tamperedSig := signed(t, g.priv, catalogEntries(101, "other-content", map[string]int{"jellyfin": 1}))
	foreignArchive, foreignSig := signed(t, otherPriv, catalogEntries(500, "foreign", map[string]int{"jellyfin": 1}))
	corruptArchive, corruptSig := signed(t, g.priv, []tarEntry{reg("index.json", `{"serial":200,"templates":[]}`), reg("../escape", "x")})
	badIndexArchive, badIndexSig := signed(t, g.priv, []tarEntry{reg("index.json", `{"serial":201,"templates":[{"id":"Not A Valid Id","revision":1}]}`)})

	cases := []struct {
		name    string
		archive []byte
		sig     []byte
		reason  FailReason
	}{
		{"the same serial with different content", goodArchive, goodSig, ReasonNotNewer},
		{"an older serial", olderArchive, olderSig, ReasonNotNewer},
		{"a bad signature", tampered, tamperedSig, ReasonBadSignature},
		{"a signature of another key", foreignArchive, foreignSig, ReasonBadSignature},
		{"a signed archive that is not a catalog", corruptArchive, corruptSig, ReasonBadArchive},
		{"a signed index no catalog list could read", badIndexArchive, badIndexSig, ReasonBadArchive},
	}
	for i, c := range cases {
		g.host.serve(c.archive, c.sig, fmt.Sprintf(`"replay-%d"`, i))
		res := g.refresh(t)
		if res.Outcome != OutcomeFailed || res.Reason != c.reason || res.Message == "" {
			t.Fatalf("%s: result = %+v, want failed with %s", c.name, res, c.reason)
		}
		if len(*g.notified) != i+1 || (*g.notified)[i].Reason != c.reason {
			t.Fatalf("%s: notifications = %+v, want one more with %s", c.name, *g.notified, c.reason)
		}
		equalTrees(t, tree(t, g.store.Dir), before)
		if got := g.store.Validators(); got != beforeValidators {
			t.Fatalf("%s: validators = %+v, want %+v", c.name, got, beforeValidators)
		}
		if names := listing(t, filepath.Dir(g.store.Dir)); len(names) != 1 || names[0] != "catalog" {
			t.Fatalf("%s: the state directory holds %v, want only catalog", c.name, names)
		}
	}
}

func TestRefresh_ANetworkFailureIsReportedAndNeverNotified(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatal(res)
	}
	before := tree(t, g.store.Dir)

	g.host.mu.Lock()
	g.host.status = http.StatusInternalServerError
	g.host.mu.Unlock()
	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed || !strings.Contains(res.Message, "500") {
		t.Fatalf("result = %+v", res)
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	g.refresher.URL = dead.URL
	if res := g.refresh(t); res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed {
		t.Fatalf("an unreachable host: %+v", res)
	}
	if len(*g.notified) != 0 {
		t.Fatalf("a network failure notified: %v", *g.notified)
	}
	equalTrees(t, tree(t, g.store.Dir), before)
}

func TestRefresh_ASignatureThatCannotBeFetchedKeepsTheCatalog(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatal(res)
	}
	before := tree(t, g.store.Dir)
	g.publish(t, `"v2"`, catalogEntries(4, "y", map[string]int{"jellyfin": 2}))
	g.host.mu.Lock()
	g.host.sig = nil
	g.host.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			http.NotFound(w, r)
			return
		}
		g.host.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	g.refresher.URL = srv.URL

	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed {
		t.Fatalf("result = %+v", res)
	}
	equalTrees(t, tree(t, g.store.Dir), before)
}

func TestRefresh_RefusesAnOversizedResponseAndAContentCodedOne(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatal(res)
	}
	before := tree(t, g.store.Dir)

	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Transfer-Encoding", "chunked")
		chunk := make([]byte, 1<<20)
		for i := 0; i <= maxFetchArchiveBytes>>20; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(big.Close)
	g.refresher.URL = big.URL
	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed || !strings.Contains(res.Message, "larger than") {
		t.Fatalf("an oversized archive: %+v", res)
	}

	coded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(coded.Close)
	g.refresher.URL = coded.URL
	res = g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed || !strings.Contains(res.Message, "content coding") {
		t.Fatalf("a content-coded archive: %+v", res)
	}
	if len(*g.notified) != 0 {
		t.Fatalf("notified: %v", *g.notified)
	}
	equalTrees(t, tree(t, g.store.Dir), before)
}

func TestRefresh_FollowsNoRedirectToAnotherHost(t *testing.T) {
	g := newRefreshRig(t)
	other := httptest.NewServer(g.host)
	t.Cleanup(other.Close)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	g.refresher.URL = redirecting.URL

	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonFetchFailed || !strings.Contains(res.Message, "not the host asked") {
		t.Fatalf("result = %+v", res)
	}
	if reqs, _ := g.host.log(); len(reqs) != 0 {
		t.Fatalf("the other host was contacted: %v", reqs)
	}
}

func TestRefresh_ANotificationThatCannotBeRaisedIsNamedInTheResult(t *testing.T) {
	g := newRefreshRig(t)
	g.refresher.Notify = func(context.Context, CheckResult) error { return errors.New("queue is full") }
	older, olderSig := signed(t, g.priv, catalogEntries(1, "old", map[string]int{"a": 1}))
	g.host.serve(older, olderSig, `"a"`)
	seed, seedSig := signed(t, g.priv, catalogEntries(2, "new", map[string]int{"a": 1}))
	if _, err := g.store.Seed(seed, seedSig); err != nil {
		t.Fatal(err)
	}
	res := g.refresh(t)
	if res.Outcome != OutcomeFailed || res.Reason != ReasonNotNewer || !strings.Contains(res.Message, "queue is full") {
		t.Fatalf("result = %+v", res)
	}
}

func TestRefresh_ConcurrentCallsShareOneRequestAndItsResult(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	g.host.mu.Lock()
	g.host.hold = func() {
		once.Do(func() { close(started) })
		<-release
	}
	g.host.mu.Unlock()

	const callers = 6
	results := make([]CheckResult, callers)
	var wg sync.WaitGroup
	var ready atomic.Int32
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready.Add(1)
			res, err := g.refresher.Refresh(context.Background())
			if err != nil {
				t.Errorf("Refresh: %v", err)
			}
			results[i] = res
		}()
	}
	<-started
	for ready.Load() < callers {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, res := range results {
		if res != results[0] || res.Outcome != OutcomeUpdated {
			t.Fatalf("caller %d got %+v, caller 0 got %+v", i, res, results[0])
		}
	}
	if reqs, _ := g.host.log(); len(reqs) != 2 {
		t.Fatalf("%d callers made %v, want one archive and one signature request", callers, reqs)
	}
	if last, ok := g.refresher.Last(); !ok || last != results[0] {
		t.Fatalf("Last = %+v, %v", last, ok)
	}

	g.host.mu.Lock()
	g.host.hold = nil
	g.host.mu.Unlock()
	g.host.reset()
	if res := g.refresh(t); res.Outcome != OutcomeUnchanged {
		t.Fatalf("a later call after the shared check finished = %+v, want a new check", res)
	}
	if reqs, _ := g.host.log(); len(reqs) != 1 {
		t.Fatalf("the later call made %v", reqs)
	}
}

func TestRefresh_ACallerThatGivesUpWhileWaitingDoesNotCancelTheSharedCheck(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(3, "x", map[string]int{"jellyfin": 1}))
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	g.host.mu.Lock()
	g.host.hold = func() {
		once.Do(func() { close(started) })
		<-release
	}
	g.host.mu.Unlock()

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan CheckResult, 1)
	go func() {
		res, _ := g.refresher.Refresh(leaderCtx)
		leaderDone <- res
	}()
	<-started
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterErr := make(chan error, 1)
	go func() {
		_, err := g.refresher.Refresh(waiterCtx)
		waiterErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelWaiter()
	if err := <-waiterErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("the waiter's error = %v, want its own cancellation", err)
	}
	cancelLeader()
	close(release)
	if res := <-leaderDone; res.Outcome != OutcomeUpdated {
		t.Fatalf("the check ended %+v after its caller went away", res)
	}
}

func TestRefresh_LastIsAbsentBeforeAnyCheckAndRecordsFailuresToo(t *testing.T) {
	g := newRefreshRig(t)
	if _, ok := g.refresher.Last(); ok {
		t.Fatal("Last reported a check before any ran")
	}
	g.host.mu.Lock()
	g.host.status = http.StatusBadGateway
	g.host.mu.Unlock()
	res := g.refresh(t)
	last, ok := g.refresher.Last()
	if !ok || last != res || last.Outcome != OutcomeFailed {
		t.Fatalf("Last = %+v, %v; the check returned %+v", last, ok, res)
	}
}

func TestCatalogStore_ConcurrentInstallsOverOneCatalogAreSerialised(t *testing.T) {
	pub, priv := newKey(t)
	dir := filepath.Join(t.TempDir(), "catalog")
	base := CatalogStore{Dir: dir, Key: pub}
	start, startSig := signed(t, priv, catalogEntries(1, "start", map[string]int{"a": 1}))
	if err := base.Install(start, startSig); err != nil {
		t.Fatal(err)
	}

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers*2)
	for w := 0; w < writers; w++ {
		serial := int64(w + 2)
		archive, sig := signed(t, priv, catalogEntries(serial, fmt.Sprintf("writer %d", w), map[string]int{"a": int(serial)}))
		for _, install := range []func() error{
			func() error { return CatalogStore{Dir: dir, Key: pub}.Install(archive, sig) },
			func() error {
				_, err := CatalogStore{Dir: dir, Key: pub}.InstallFetched(archive, sig, Validators{ETag: fmt.Sprintf(`"%d"`, serial)})
				return err
			},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := install(); err != nil && !errors.Is(err, ErrNotNewer) {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent install failed other than as a replay: %v", err)
	}

	serial, ok, err := base.Serial()
	if err != nil || !ok || serial != writers+1 {
		t.Fatalf("installed serial = %d, %v, %v, want the highest written, %d", serial, ok, err, writers+1)
	}
	if names := listing(t, filepath.Dir(dir)); len(names) != 1 || names[0] != "catalog" {
		t.Fatalf("the state directory holds %v, want only catalog", names)
	}
	if _, err := (DirCatalog{Root: dir}).Index(context.Background()); err != nil {
		t.Fatalf("the catalog left on disk cannot be listed: %v", err)
	}
	if v := base.Validators(); !v.empty() && v.ETag != fmt.Sprintf(`"%d"`, writers+1) {
		t.Fatalf("the validators %+v do not belong to the installed catalog (serial %d)", v, writers+1)
	}
}

func TestRefresh_AReaderDuringAReplacementSeesOnlyAWholeCatalog(t *testing.T) {
	g := newRefreshRig(t)
	g.publish(t, `"v1"`, catalogEntries(1, "old", map[string]int{"jellyfin": 1}))
	if res := g.refresh(t); res.Outcome != OutcomeUpdated {
		t.Fatal(res)
	}
	g.publish(t, `"v2"`, catalogEntries(2, "new", map[string]int{"jellyfin": 2, "plex": 1}))

	inSwap := make(chan struct{})
	release := make(chan struct{})
	var first sync.Once
	g.refresher.Store.rename = func(oldpath, newpath string) error {
		first.Do(func() {
			close(inSwap)
			<-release
		})
		return os.Rename(oldpath, newpath)
	}
	done := make(chan CheckResult, 1)
	go func() {
		res, _ := g.refresher.Refresh(context.Background())
		done <- res
	}()
	<-inSwap

	cat := DirCatalog{Root: g.store.Dir, Source: SourceCurated}
	idx, err := cat.Index(context.Background())
	if err != nil || idx.Serial != 1 || len(idx.Templates) != 1 {
		t.Fatalf("a reader before the swap saw %+v, %v, want the old catalog whole", idx, err)
	}
	entry, err := cat.Entry(context.Background(), "jellyfin")
	if err != nil || !strings.Contains(string(entry.Data), "old rev 1") {
		t.Fatalf("an installer before the swap read %q, %v", entry.Data, err)
	}
	if _, err := cat.Entry(context.Background(), "plex"); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("a template of the new catalog is visible before the swap: %v", err)
	}
	close(release)
	if res := <-done; res.Outcome != OutcomeUpdated {
		t.Fatalf("result = %+v", res)
	}
	if idx, err := cat.Index(context.Background()); err != nil || idx.Serial != 2 || len(idx.Templates) != 2 {
		t.Fatalf("after the swap: %+v, %v", idx, err)
	}
}

func TestCatalogStore_FetchedValidatorsAreReplacedWithTheCatalogTheyDescribe(t *testing.T) {
	pub, priv := newKey(t)
	store := CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog"), Key: pub}
	a, aSig := signed(t, priv, catalogEntries(1, "a", map[string]int{"x": 1}))
	if _, err := store.InstallFetched(a, aSig, Validators{ETag: `"a"`, LastModified: "Mon, 01 Jan 2026 00:00:00 GMT"}); err != nil {
		t.Fatal(err)
	}
	if v := store.Validators(); v.ETag != `"a"` || v.LastModified == "" {
		t.Fatalf("validators = %+v", v)
	}
	b, bSig := signed(t, priv, catalogEntries(2, "b", map[string]int{"x": 1}))
	if _, err := store.Seed(b, bSig); err != nil {
		t.Fatal(err)
	}
	if v := store.Validators(); !v.empty() {
		t.Fatalf("validators %+v survived the replacement of the catalog they described", v)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, validatorFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := store.Validators(); !v.empty() {
		t.Fatalf("an unreadable validators file gave %+v", v)
	}
}
