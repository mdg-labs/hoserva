package template

import (
	"context"
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

// Refresher fetches the latest signed catalog and installs it into Store
// (doc 04 §7). One check is one conditional request for the archive and, only
// after a 200, one for its signature.
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

	mu     sync.Mutex
	flight *checkFlight
	last   *CheckResult
}

type checkFlight struct {
	done   chan struct{}
	result CheckResult
}

// Refresh runs one check now. A call made while another check is running
// shares that check's request and result, and the result is returned to every
// caller. The error is only the caller's own context ending while it waited.
func (r *Refresher) Refresh(ctx context.Context) (CheckResult, error) {
	r.mu.Lock()
	if f := r.flight; f != nil {
		r.mu.Unlock()
		select {
		case <-f.done:
			return f.result, nil
		case <-ctx.Done():
			return CheckResult{}, ctx.Err()
		}
	}
	f := &checkFlight{done: make(chan struct{})}
	r.flight = f
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.flight = nil
		if f.result.Outcome != "" {
			res := f.result
			r.last = &res
		}
		r.mu.Unlock()
		close(f.done)
	}()
	// A caller that goes away must not abort a check others are waiting on.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkTimeout)
	defer cancel()
	f.result = r.check(runCtx)
	return f.result, nil
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

func (r *Refresher) check(ctx context.Context) CheckResult {
	res := r.fetchAndInstall(ctx)
	res.CheckedAt = r.now().UTC()
	if res.Outcome == OutcomeFailed && res.Reason.verification() && r.Notify != nil {
		if err := r.Notify(ctx, res); err != nil {
			res.Message = fmt.Sprintf("%s (the notification could not be raised: %v)", res.Message, err)
		}
	}
	return res
}

func failure(reason FailReason, err error) CheckResult {
	return CheckResult{Outcome: OutcomeFailed, Reason: reason, Message: err.Error()}
}

func (r *Refresher) fetchAndInstall(ctx context.Context) CheckResult {
	archiveURL, err := catalogURL(r.baseURL(), catalogArchiveName)
	if err != nil {
		return failure(ReasonFetchFailed, err)
	}
	sigURL, err := catalogURL(r.baseURL(), catalogSignatureName)
	if err != nil {
		return failure(ReasonFetchFailed, err)
	}
	client := r.httpClient()

	prior := r.Store.Validators()
	archive, got, notModified, err := fetch(ctx, client, archiveURL, maxFetchArchiveBytes, prior)
	if err != nil {
		return failure(ReasonFetchFailed, err)
	}
	if notModified {
		return CheckResult{Outcome: OutcomeUnchanged}
	}
	sig, _, _, err := fetch(ctx, client, sigURL, maxFetchSigBytes, Validators{})
	if err != nil {
		return failure(ReasonFetchFailed, err)
	}

	fetched, err := r.Store.InstallFetched(archive, sig, got)
	switch {
	case errors.Is(err, ErrBadSignature):
		return failure(ReasonBadSignature, err)
	case errors.Is(err, ErrNotNewer):
		return failure(ReasonNotNewer, err)
	case errors.Is(err, ErrBadArchive):
		return failure(ReasonBadArchive, err)
	case err != nil:
		return failure(ReasonInstallFailed, err)
	}
	if !fetched.Installed {
		return CheckResult{Outcome: OutcomeUnchanged}
	}
	added, changed := diffIndexes(fetched.Previous, fetched.Current)
	return CheckResult{Outcome: OutcomeUpdated, New: added, Updated: changed}
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
