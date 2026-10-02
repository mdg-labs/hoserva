package template

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCatalogURL is the compiled-in address of the signed catalog
	// (doc 04 §7, Q65): the latest archive and its detached signature, served
	// as static files. The archive is trusted through its signature, never its
	// host, and nothing here ever contacts api.github.com.
	DefaultCatalogURL = "https://catalog.hoserva.dev/"

	catalogArchiveName   = "catalog.tar.zst"
	catalogSignatureName = "catalog.tar.zst.sig"

	maxFetchArchiveBytes = 64 << 20
	maxFetchSigBytes     = 4 << 10
	maxFetchRedirects    = 3
	// checkTimeout bounds one whole check: the archive and the signature.
	checkTimeout = 2 * time.Minute
)

// Outcome is how a catalog check ended.
type Outcome string

const (
	OutcomeUpdated   Outcome = "updated"
	OutcomeUnchanged Outcome = "unchanged"
	OutcomeFailed    Outcome = "failed"
)

// FailReason is why a check failed. The first three, and BadArchive, are
// verification failures: the server answered with something Hoserva refuses
// to trust, and the check raises a notification. FetchFailed and
// InstallFailed are not.
type FailReason string

const (
	ReasonFetchFailed   FailReason = "fetch_failed"
	ReasonBadSignature  FailReason = "bad_signature"
	ReasonNotNewer      FailReason = "not_newer"
	ReasonBadArchive    FailReason = "bad_archive"
	ReasonInstallFailed FailReason = "install_failed"
)

func (r FailReason) verification() bool {
	switch r {
	case ReasonBadSignature, ReasonNotNewer, ReasonBadArchive:
		return true
	}
	return false
}

// CheckResult is one check's outcome. New and Updated count the templates
// the check added and changed, by id and revision against the index it
// replaced, and are set only when Outcome is OutcomeUpdated; Reason and
// Message are set only when it is OutcomeFailed.
type CheckResult struct {
	CheckedAt time.Time
	Outcome   Outcome
	New       int
	Updated   int
	Reason    FailReason
	Message   string
}

// Trigger is what started a check. A manual check always reports a
// verification failure and always fetches the archive in full; an automatic
// one (the interval, check-on-open) does not repeat a notification for a
// failure already raised, and does not download an archive again that already
// failed verification.
type Trigger int

const (
	TriggerManual Trigger = iota
	TriggerInterval
	TriggerOpen
)

// Refresher fetches the latest signed catalog and installs it into Store
// (doc 04 §7). One check is one conditional request for the archive and, only
// after a 200, one for its signature; a signature that fails verification
// makes the check fetch the pair once more before it reports the failure. An
// automatic check that remembers a bad_signature failure also asks whether the
// signature changed when the archive answers 304.
type Refresher struct {
	Store CatalogStore
	// URL is the directory the archive and signature are served from; empty
	// means DefaultCatalogURL.
	URL string
	// Client makes the requests; nil means a plain http.Client. Its redirect
	// policy is replaced: a redirect must stay on the host asked.
	Client *http.Client
	// Notify raises the notification for a verification failure. Nil raises
	// none.
	Notify func(ctx context.Context, r CheckResult) error
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Finished is called once with the result of every finished check,
	// whatever started it. Nil calls nothing.
	Finished func(r CheckResult)

	mu       sync.Mutex
	flight   *checkFlight
	last     *CheckResult
	reported string
	rejected *rejection
}

// served is what the last download of the archive and its signature brought:
// the hex SHA-256 of the archive and of the signature, and the validators the
// host sent with each.
type served struct {
	archive           string
	archiveValidators Validators
	sig               string
	sigValidators     Validators
}

// rejection is the last archive that failed verification: what the host
// served and the failure it produced. An automatic check sends the archive's
// validators, so a host still serving that archive answers 304 and the check
// reports the same failure without downloading it again. A bad_signature
// failure may lie in the signature alone, so the check first asks whether the
// signature is still the one that failed.
type rejection struct {
	served served
	result CheckResult
}

type checkFlight struct {
	done   chan struct{}
	result CheckResult
}

// Refresh runs one manual check now. A call made while another check is
// running shares that check's request and result, and the result is returned
// to every caller. The error is only the caller's own context ending while it
// waited.
func (r *Refresher) Refresh(ctx context.Context) (CheckResult, error) {
	return r.Check(ctx, TriggerManual)
}

// Check is Refresh for a check started by trigger. Joining a check already
// running leaves that check's own trigger in force.
func (r *Refresher) Check(ctx context.Context, trigger Trigger) (CheckResult, error) {
	f, leader := r.begin()
	if !leader {
		select {
		case <-f.done:
			return f.result, nil
		case <-ctx.Done():
			return CheckResult{}, ctx.Err()
		}
	}
	r.run(ctx, f, trigger)
	return f.result, nil
}

// StartBackground starts a check of trigger without waiting for it, unless
// one is already running, and reports whether it started one. The check does
// not end with ctx.
func (r *Refresher) StartBackground(ctx context.Context, trigger Trigger) bool {
	f, leader := r.begin()
	if !leader {
		return false
	}
	go r.run(ctx, f, trigger)
	return true
}

// begin registers a new check, or returns the running one.
func (r *Refresher) begin() (f *checkFlight, leader bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.flight != nil {
		return r.flight, false
	}
	r.flight = &checkFlight{done: make(chan struct{})}
	return r.flight, true
}

func (r *Refresher) run(ctx context.Context, f *checkFlight, trigger Trigger) {
	defer close(f.done)
	// A caller that goes away must not abort a check others are waiting on.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
	defer cancel()
	f.result = r.check(runCtx, trigger)
	// The check is over, and Last and the next Check see that, before
	// Finished announces it: whoever reacts to the announcement by asking
	// for the last result or by starting another check must not find this
	// one still in flight.
	r.mu.Lock()
	r.flight = nil
	if f.result.Outcome != "" {
		res := f.result
		r.last = &res
	}
	r.mu.Unlock()
	if r.Finished != nil {
		r.Finished(f.result)
	}
}

// Last is the most recent check's result since the daemon started, and false
// before any check has run.
func (r *Refresher) Last() (CheckResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		return CheckResult{}, false
	}
	return *r.last, true
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Refresher) baseURL() string {
	if r.URL != "" {
		return r.URL
	}
	return DefaultCatalogURL
}

func (r *Refresher) check(ctx context.Context, trigger Trigger) CheckResult {
	res, got := r.fetchAndInstall(ctx, trigger)
	res.CheckedAt = r.now().UTC()
	switch {
	case res.Outcome != OutcomeFailed:
		r.setReported("")
		r.setRejected(nil)
		return res
	case !res.Reason.verification():
		return res
	}
	if got.archiveValidators.empty() {
		r.setRejected(nil)
	} else {
		r.setRejected(&rejection{served: got, result: res})
	}
	if r.Notify == nil {
		return res
	}
	key := string(res.Reason) + "/" + got.archive
	if trigger == TriggerManual || !r.isReported(key) {
		if err := r.Notify(ctx, res); err != nil {
			res.Message = fmt.Sprintf("%s (the notification could not be raised: %v)", res.Message, err)
		} else {
			r.setReported(key)
		}
	}
	return res
}

// getRejected and setRejected keep the archive that last failed verification.
// A check that installs or confirms the catalog clears it; a check that could
// not reach the host, or could not install, leaves it.
func (r *Refresher) getRejected() *rejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rejected
}

func (r *Refresher) setRejected(rj *rejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejected = rj
}

// isReported and setReported keep the failure the last notification was
// raised for: its reason and the digest of the archive the host served. A
// check that installs or confirms the catalog clears it, so the same failure
// reported again after a good check is a new notification.
func (r *Refresher) isReported(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reported == key
}

func (r *Refresher) setReported(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reported = key
}

func failure(reason FailReason, err error) CheckResult {
	return CheckResult{Outcome: OutcomeFailed, Reason: reason, Message: err.Error()}
}

// fetchAndInstall runs the check and returns its result and, when an archive
// was downloaded, what the last download brought. An automatic check that
// remembers an archive that failed verification asks for the catalog
// conditioned on that archive's validators instead of the installed catalog's,
// so the host answering 304 means the same archive is still being served and
// the same failure is the result, unless the failure was a bad signature and
// the signature has changed since: then the pair is fetched in full.
func (r *Refresher) fetchAndInstall(ctx context.Context, trigger Trigger) (CheckResult, served) {
	archiveURL, err := catalogURL(r.baseURL(), catalogArchiveName)
	if err != nil {
		return failure(ReasonFetchFailed, err), served{}
	}
	sigURL, err := catalogURL(r.baseURL(), catalogSignatureName)
	if err != nil {
		return failure(ReasonFetchFailed, err), served{}
	}
	client := r.httpClient()
	prior := r.Store.Validators()
	var rejected *rejection
	if trigger != TriggerManual {
		if rejected = r.getRejected(); rejected != nil {
			prior = rejected.served.archiveValidators
		}
	}

	var got served
	// The archive and its signature are two files a static host caches
	// separately, so during a publish one can be new and the other old. A
	// signature that does not verify is fetched, with its archive, once more
	// before it is reported.
	retried := false
	for {
		archive, archiveValidators, notModified, err := fetch(ctx, client, archiveURL, maxFetchArchiveBytes, prior)
		if err != nil {
			return failure(ReasonFetchFailed, err), got
		}
		if notModified {
			if rejected == nil {
				return CheckResult{Outcome: OutcomeUnchanged}, got
			}
			changed, err := r.signatureChanged(ctx, client, sigURL, rejected)
			if err != nil {
				return failure(ReasonFetchFailed, err), got
			}
			if !changed {
				return rejected.result, rejected.served
			}
			rejected, prior = nil, Validators{}
			continue
		}
		sum := sha256.Sum256(archive)
		got = served{archive: hex.EncodeToString(sum[:]), archiveValidators: archiveValidators}
		var sig []byte
		if !r.Store.Unsigned {
			var sigValidators Validators
			sig, sigValidators, _, err = fetch(ctx, client, sigURL, maxFetchSigBytes, Validators{})
			if err != nil {
				return failure(ReasonFetchFailed, err), got
			}
			sigSum := sha256.Sum256(sig)
			got.sig, got.sigValidators = hex.EncodeToString(sigSum[:]), sigValidators
		}

		fetched, err := r.Store.InstallFetched(archive, sig, archiveValidators)
		switch {
		case errors.Is(err, ErrBadSignature) && !retried:
			retried = true
			continue
		case errors.Is(err, ErrBadSignature):
			return failure(ReasonBadSignature, err), got
		case errors.Is(err, ErrNotNewer):
			return failure(ReasonNotNewer, err), got
		case errors.Is(err, ErrBadArchive):
			return failure(ReasonBadArchive, err), got
		case err != nil:
			return failure(ReasonInstallFailed, err), got
		}
		if !fetched.Installed {
			return CheckResult{Outcome: OutcomeUnchanged}, got
		}
		added, changed := diffIndexes(fetched.Previous, fetched.Current)
		return CheckResult{Outcome: OutcomeUpdated, New: added, Updated: changed}, got
	}
}

// signatureChanged reports whether the signature the host serves is no longer
// the one that failed verification, once the archive that failed with it is
// unchanged. Only a bad_signature failure can be cured by the signature alone;
// the other reasons are properties of the archive. The signature is asked for
// conditioned on the validators it was served with; a host that sent none, or
// that answers 200 anyway, is told apart by comparing the body's digest.
func (r *Refresher) signatureChanged(ctx context.Context, client *http.Client, sigURL string, rejected *rejection) (bool, error) {
	if rejected.result.Reason != ReasonBadSignature {
		return false, nil
	}
	sig, _, notModified, err := fetch(ctx, client, sigURL, maxFetchSigBytes, rejected.served.sigValidators)
	if err != nil {
		return false, err
	}
	if notModified {
		return false, nil
	}
	sum := sha256.Sum256(sig)
	return hex.EncodeToString(sum[:]) != rejected.served.sig, nil
}

// diffIndexes counts the templates of current that previous does not list,
// and those both list under a different revision. A previous index that is
// missing or unreadable lists nothing.
func diffIndexes(previous, current []byte) (added, changed int) {
	revisions := map[string]int{}
	if prev, err := parseIndex(previous); err == nil {
		for _, t := range prev.Templates {
			revisions[t.ID] = t.Revision
		}
	}
	cur, err := parseIndex(current)
	if err != nil {
		return 0, 0
	}
	for _, t := range cur.Templates {
		rev, ok := revisions[t.ID]
		switch {
		case !ok:
			added++
		case rev != t.Revision:
			changed++
		}
	}
	return added, changed
}

// catalogURL is base with name as its last path element. Only http and https
// are fetched, and the GitHub API is refused by its parsed host, never by a
// substring.
func catalogURL(base, name string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", fmt.Errorf("the catalog URL %q is not an http or https address", base)
	}
	if strings.EqualFold(u.Hostname(), "api.github.com") {
		return "", fmt.Errorf("the catalog is never fetched from the GitHub API: %q", base)
	}
	u.Path = path.Join("/", u.Path, name)
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func (r *Refresher) httpClient() *http.Client {
	c := &http.Client{}
	if r.Client != nil {
		cp := *r.Client
		c = &cp
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxFetchRedirects {
			return fmt.Errorf("stopped after %d redirects", maxFetchRedirects)
		}
		first := via[0].URL
		if !strings.EqualFold(req.URL.Hostname(), first.Hostname()) || (req.URL.Scheme != first.Scheme && req.URL.Scheme != "https") {
			return fmt.Errorf("the catalog host redirected to %s, which is not the host asked", req.URL.Host)
		}
		return nil
	}
	return c
}

// fetch GETs target, sending prior as the conditional headers, and returns
// the body (at most limit bytes) and the validators the response carries. A
// 304 answers notModified and reads no body.
func fetch(ctx context.Context, client *http.Client, target string, limit int64, prior Validators) (body []byte, got Validators, notModified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, Validators{}, false, fmt.Errorf("building the request for %s: %w", target, err)
	}
	// The signature covers the archive's own bytes, so no content coding may
	// transform them in transit.
	req.Header.Set("Accept-Encoding", "identity")
	if prior.ETag != "" {
		req.Header.Set("If-None-Match", prior.ETag)
	}
	if prior.LastModified != "" {
		req.Header.Set("If-Modified-Since", prior.LastModified)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Validators{}, false, fmt.Errorf("fetching %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified && !prior.empty():
		return nil, Validators{}, true, nil
	case resp.StatusCode != http.StatusOK:
		return nil, Validators{}, false, fmt.Errorf("fetching %s: the server answered %s", target, resp.Status)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, Validators{}, false, fmt.Errorf("fetching %s: the server applied a %q content coding", target, enc)
	}
	if resp.ContentLength > limit {
		return nil, Validators{}, false, fmt.Errorf("fetching %s: the response is larger than %d bytes", target, limit)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, Validators{}, false, fmt.Errorf("reading %s: %w", target, err)
	}
	if int64(len(body)) > limit {
		return nil, Validators{}, false, fmt.Errorf("fetching %s: the response is larger than %d bytes", target, limit)
	}
	return body, Validators{ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified")}, false, nil
}
