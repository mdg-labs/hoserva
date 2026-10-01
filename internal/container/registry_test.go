package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// registryServer is a loopback registry that asks for a bearer token, the
// way Docker Hub and ghcr.io do, and records every request it gets.
type registryServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	// manifestStatus, when set, is the status the manifest answers with.
	manifestStatus int
	tokenStatus    int
	realm          string
	challenge      string
	tagPageCount   int
	redirectTo     string
	tagPages       map[string]string
}

func newRegistryServer(t *testing.T) *registryServer {
	t.Helper()
	rs := &registryServer{tagPages: map[string]string{}}
	rs.Server = httptest.NewServer(http.HandlerFunc(rs.serve))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *registryServer) log(r *http.Request) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.requests = append(rs.requests, r.Method+" "+r.URL.Path)
}

func (rs *registryServer) seen() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.requests...)
}

func (rs *registryServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.log(r)
	if r.URL.Path == "/token" {
		if rs.tokenStatus != 0 {
			w.WriteHeader(rs.tokenStatus)
			return
		}
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("service") != "registry.test" || r.URL.Query().Get("scope") != "repository:acme/app:pull" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"tok-anon"}`)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok-") {
		realm := rs.realm
		if realm == "" {
			realm = rs.URL + "/token"
		}
		challenge := rs.challenge
		if challenge == "" {
			challenge = `Bearer realm="` + realm + `",service="registry.test",scope="repository:acme/app:pull"`
		}
		w.Header().Set("WWW-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case strings.Contains(r.URL.Path, "/manifests/"):
		if rs.redirectTo != "" {
			http.Redirect(w, r, rs.redirectTo+r.URL.Path, rs.manifestStatus)
			return
		}
		if rs.manifestStatus != 0 {
			w.WriteHeader(rs.manifestStatus)
			return
		}
		if r.Method != http.MethodHead || !strings.Contains(r.Header.Get("Accept"), "manifest.list") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Docker-Content-Digest", digestNew)
	case strings.HasSuffix(r.URL.Path, "/tags/list"):
		page := r.URL.Query().Get("page")
		if rs.tagPageCount > 0 {
			n, _ := strconv.Atoi(page)
			n = max(n, 1)
			if n < rs.tagPageCount {
				w.Header().Set("Link", fmt.Sprintf(`</v2/acme/app/tags/list?page=%d>; rel="next"`, n+1))
			}
			_, _ = fmt.Fprintf(w, `{"tags":["tag-%d"]}`, n)
			return
		}
		switch page {
		case "":
			w.Header().Set("Link", `</v2/acme/app/tags/list?page=2>; rel="next"`)
			_, _ = fmt.Fprint(w, `{"tags":["1.0","1.1"]}`)
		case "2":
			_, _ = fmt.Fprint(w, `{"tags":["1.2"]}`)
		case "evil":
			w.Header().Set("Link", `<http://evil.example/v2/x?page=3>; rel="next"`)
			_, _ = fmt.Fprint(w, `{"tags":["9"]}`)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (rs *registryServer) ref() ImageRef {
	return ImageRef{Registry: strings.TrimPrefix(rs.URL, "http://"), Repository: "acme/app", Tag: "latest"}
}

func TestHTTPRegistry_ManifestDigestIsAManifestHeadOnly(t *testing.T) {
	rs := newRegistryServer(t)
	got, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
	if err != nil || got != digestNew {
		t.Fatalf("ManifestDigest = %q, %v, want %s", got, err, digestNew)
	}
	want := []string{"HEAD /v2/acme/app/manifests/latest", "GET /token", "HEAD /v2/acme/app/manifests/latest"}
	if fmt.Sprint(rs.seen()) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v: a HEAD of the manifest, never a blob or a GET of the manifest body", rs.seen(), want)
	}
}

func TestHTTPRegistry_ARegistryThatWantsALoginIsDenied(t *testing.T) {
	for name, set := range map[string]func(*registryServer){
		"a basic challenge":           func(rs *registryServer) { rs.challenge = `Basic realm="registry"` },
		"a token service that is 401": func(rs *registryServer) { rs.tokenStatus = http.StatusUnauthorized },
		"a token service that is 403": func(rs *registryServer) { rs.tokenStatus = http.StatusForbidden },
		"a manifest that is 403":      func(rs *registryServer) { rs.manifestStatus = http.StatusForbidden },
	} {
		t.Run(name, func(t *testing.T) {
			rs := newRegistryServer(t)
			set(rs)
			_, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
			if !errors.Is(err, ErrRegistryDenied) {
				t.Fatalf("ManifestDigest = %v, want ErrRegistryDenied", err)
			}
		})
	}
}

func TestHTTPRegistry_RateLimitIsReportedAsRateLimited(t *testing.T) {
	for name, set := range map[string]func(*registryServer){
		"on the manifest":      func(rs *registryServer) { rs.manifestStatus = http.StatusTooManyRequests },
		"on the token service": func(rs *registryServer) { rs.tokenStatus = http.StatusTooManyRequests },
	} {
		t.Run(name, func(t *testing.T) {
			rs := newRegistryServer(t)
			set(rs)
			_, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("ManifestDigest = %v, want ErrRateLimited", err)
			}
		})
	}
}

func TestHTTPRegistry_UnknownTagAndOtherErrors(t *testing.T) {
	rs := newRegistryServer(t)
	rs.manifestStatus = http.StatusNotFound
	if _, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref()); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("404 = %v, want ErrManifestNotFound", err)
	}
	rs.manifestStatus = http.StatusBadGateway
	_, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
	if err == nil || errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("502 = %v, want a plain failure naming the status", err)
	}
}

func TestHTTPRegistry_TokenRealmMustBeHTTPS(t *testing.T) {
	rs := newRegistryServer(t)
	rs.realm = "http://tokens.example.com/token"
	_, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
	if err == nil || !strings.Contains(err.Error(), "not usable") {
		t.Fatalf("ManifestDigest = %v, want the plain-HTTP realm refused", err)
	}
}

func TestHTTPRegistry_TagsFollowsPagesOnTheSameRegistryOnly(t *testing.T) {
	rs := newRegistryServer(t)
	tags, err := (&HTTPRegistry{}).Tags(context.Background(), rs.ref())
	if err != nil || fmt.Sprint(tags) != "[1.0 1.1 1.2]" {
		t.Fatalf("Tags = %v, %v, want every page", tags, err)
	}

	evil := &http.Client{Transport: rewriteFirstPage{rs: rs}}
	if _, err := (&HTTPRegistry{Client: evil}).Tags(context.Background(), rs.ref()); err == nil || !strings.Contains(err.Error(), "not on the registry") {
		t.Fatalf("Tags with a next page on another host = %v, want it refused", err)
	}
}

// rewriteFirstPage sends the first tag page request to the "evil" page,
// whose Link header names another host.
type rewriteFirstPage struct{ rs *registryServer }

func (r rewriteFirstPage) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Path, "/tags/list") && req.URL.Query().Get("page") == "" {
		q := req.URL.Query()
		q.Set("page", "evil")
		req.URL.RawQuery = q.Encode()
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestHTTPRegistry_TagsReadsPastTenPages(t *testing.T) {
	rs := newRegistryServer(t)
	rs.tagPageCount = 11
	tags, err := (&HTTPRegistry{}).Tags(context.Background(), rs.ref())
	if err != nil || len(tags) != 11 || tags[10] != "tag-11" {
		t.Fatalf("Tags = %v, %v, want all 11 pages: a newer tag may sit on page 11", tags, err)
	}
}

func TestHTTPRegistry_ACutOffTagListIsAnErrorNotAShortList(t *testing.T) {
	rs := newRegistryServer(t)
	rs.tagPageCount = maxTagPages + 5
	tags, err := (&HTTPRegistry{}).Tags(context.Background(), rs.ref())
	if err == nil || tags != nil {
		t.Fatalf("Tags = %v, %v, want an error and no partial list when the registry still offers a next page", tags, err)
	}
}

// tlsRegistry is an https registry reached under the name example.com (the
// certificate httptest serves is valid for it), dialled to the test server.
type tlsRegistry struct {
	*httptest.Server
	client *http.Client
}

func newTLSRegistry(t *testing.T, h http.Handler) *tlsRegistry {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return &tlsRegistry{Server: srv, client: &http.Client{Transport: transport}}
}

func TestHTTPRegistry_ARedirectNeverCarriesTheTokenToAnotherSchemeOrPort(t *testing.T) {
	var mu sync.Mutex
	var elsewhere []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		elsewhere = append(elsewhere, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		w.Header().Set("Docker-Content-Digest", digestNew)
	}))
	t.Cleanup(plain.Close)

	for name, target := range map[string]string{
		"https to plain http on another port": plain.URL,
		"https to https on another port":      "https://example.com:1",
	} {
		t.Run(name, func(t *testing.T) {
			reg := newTLSRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/token":
					_, _ = fmt.Fprint(w, `{"token":"tok-anon"}`)
				case r.Header.Get("Authorization") == "":
					w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com/token",service="s",scope="repository:acme/app:pull"`)
					w.WriteHeader(http.StatusUnauthorized)
				default:
					http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
				}
			}))
			ref := ImageRef{Registry: "example.com", Repository: "acme/app", Tag: "latest"}
			_, err := (&HTTPRegistry{Client: reg.client}).ManifestDigest(context.Background(), ref)
			if err == nil || !strings.Contains(err.Error(), "same scheme, host and port") {
				t.Fatalf("ManifestDigest = %v, want the redirect refused", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(elsewhere) != 0 {
				t.Fatalf("the redirect target was asked: %v", elsewhere)
			}
		})
	}
}

func TestHTTPRegistry_ASameOriginRedirectIsFollowed(t *testing.T) {
	reg := newTLSRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/alias"):
			http.Redirect(w, r, "https://example.com/v2/acme/app/manifests/real", http.StatusTemporaryRedirect)
		case r.URL.Path == "/token":
			_, _ = fmt.Fprint(w, `{"token":"tok-anon"}`)
		case r.Header.Get("Authorization") == "":
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://example.com/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Docker-Content-Digest", digestNew)
		}
	}))
	ref := ImageRef{Registry: "example.com", Repository: "acme/app", Tag: "alias"}
	if got, err := (&HTTPRegistry{Client: reg.client}).ManifestDigest(context.Background(), ref); err != nil || got != digestNew {
		t.Fatalf("ManifestDigest = %q, %v, want the redirect on the same origin followed", got, err)
	}
}

func TestHTTPRegistry_ARedirectOnLoopbackToAnotherPortIsRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect target was asked: %s %s", r.Method, r.URL)
	}))
	t.Cleanup(other.Close)
	rs := newRegistryServer(t)
	rs.manifestStatus = http.StatusTemporaryRedirect
	rs.redirectTo = other.URL
	_, err := (&HTTPRegistry{}).ManifestDigest(context.Background(), rs.ref())
	if err == nil || !strings.Contains(err.Error(), "same scheme, host and port") {
		t.Fatalf("ManifestDigest = %v, want the redirect refused", err)
	}
}
