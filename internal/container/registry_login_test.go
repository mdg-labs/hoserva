package container

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// credentialSource is a CredentialSource over a fixed table, recording the
// registries it was asked for.
type credentialSource struct {
	creds map[string]Credential
	err   error
	mu    sync.Mutex
	asked []string
}

func (c *credentialSource) Credential(_ context.Context, registry string) (Credential, bool, error) {
	c.mu.Lock()
	c.asked = append(c.asked, registry)
	c.mu.Unlock()
	if c.err != nil {
		return Credential{}, false, c.err
	}
	cred, ok := c.creds[registry]
	return cred, ok, nil
}

func basicHeader(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// loginRegistry is an https registry (example.com) that records every
// request with its Authorization header.
type loginRegistry struct {
	*tlsRegistry
	mu   sync.Mutex
	seen []string
}

func (lr *loginRegistry) requests() []string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return append([]string(nil), lr.seen...)
}

func (lr *loginRegistry) record(r *http.Request) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.seen = append(lr.seen, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
}

// newLoginRegistry serves the manifest only to the Authorization header
// want, challenging every other request with challenge.
func newLoginRegistry(t *testing.T, challenge, want string, token func(w http.ResponseWriter, r *http.Request)) *loginRegistry {
	t.Helper()
	lr := &loginRegistry{}
	lr.tlsRegistry = newTLSRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lr.record(r)
		switch {
		case r.URL.Path == "/token":
			token(w, r)
		case r.Header.Get("Authorization") == want:
			w.Header().Set("Docker-Content-Digest", digestNew)
		default:
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	return lr
}

var loginRef = ImageRef{Registry: "example.com", Repository: "acme/app", Tag: "latest"}

func TestHTTPRegistry_ABearerLoginSendsTheCredentialToTheTokenServiceOnly(t *testing.T) {
	var tokenAuth string
	lr := newLoginRegistry(t, `Bearer realm="https://example.com/token",service="s",scope="repository:acme/app:pull"`, "Bearer tok-user", func(w http.ResponseWriter, r *http.Request) {
		tokenAuth = r.Header.Get("Authorization")
		_, _ = fmt.Fprint(w, `{"token":"tok-user"}`)
	})
	src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "pa:ss"}}}
	got, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).ManifestDigest(context.Background(), loginRef)
	if err != nil || got != digestNew {
		t.Fatalf("ManifestDigest = %q, %v", got, err)
	}
	if tokenAuth != basicHeader("me", "pa:ss") {
		t.Errorf("the token service got Authorization %q, want the basic login", tokenAuth)
	}
	for _, req := range lr.requests() {
		if strings.Contains(req, "auth=Basic") && !strings.Contains(req, "/token") {
			t.Errorf("the credential was sent to the registry itself on a bearer challenge: %s", req)
		}
	}
	if len(src.asked) == 0 || src.asked[0] != "example.com" {
		t.Errorf("credentials asked for %v, want the image's registry host", src.asked)
	}
}

func TestHTTPRegistry_ABasicChallengeIsAnsweredWithTheCredentialOverHTTPS(t *testing.T) {
	want := basicHeader("me", "secret")
	lr := newLoginRegistry(t, `Basic realm="registry"`, want, nil)
	src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "secret"}}}
	if got, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).ManifestDigest(context.Background(), loginRef); err != nil || got != digestNew {
		t.Fatalf("ManifestDigest = %q, %v", got, err)
	}
	reqs := lr.requests()
	if len(reqs) != 2 || strings.HasSuffix(reqs[0], "auth=") == false || reqs[1] != "HEAD /v2/acme/app/manifests/latest auth="+want {
		t.Fatalf("requests = %v, want an anonymous request first and the login only after the challenge", reqs)
	}
}

func TestHTTPRegistry_AnotherRegistrysCredentialIsNeverSent(t *testing.T) {
	lr := newLoginRegistry(t, `Bearer realm="https://example.com/token",service="s"`, "Bearer tok-anon", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"token":"tok-anon"}`)
	})
	src := &credentialSource{creds: map[string]Credential{"ghcr.io": {Username: "me", Password: "other-registry-secret"}}}
	if _, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).ManifestDigest(context.Background(), loginRef); err != nil {
		t.Fatalf("ManifestDigest = %v, want the anonymous flow for a registry with no credential", err)
	}
	for _, req := range lr.requests() {
		if strings.Contains(req, "Basic") {
			t.Fatalf("a credential saved for another registry was sent: %s", req)
		}
	}
}

func TestHTTPRegistry_ADockerHubCredentialIsUsedForEveryNameOfDockerHub(t *testing.T) {
	for _, host := range []string{"registry-1.docker.io", "index.docker.io", "docker.io"} {
		t.Run(host, func(t *testing.T) {
			var tokenAuth string
			lr := newLoginRegistry(t, `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`, "Bearer tok-user", func(w http.ResponseWriter, r *http.Request) {
				tokenAuth = r.Header.Get("Authorization")
				_, _ = fmt.Fprint(w, `{"token":"tok-user"}`)
			})
			lr.client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
			creds, _ := newCredentials()
			if err := creds.PutCredential(context.Background(), "registry-1.docker.io", Credential{Username: "me", Password: "secret"}); err != nil {
				t.Fatal(err)
			}
			ref := ImageRef{Registry: host, Repository: "acme/private", Tag: "latest"}
			if got, err := (&HTTPRegistry{Client: lr.client, Credentials: creds}).ManifestDigest(context.Background(), ref); err != nil || got != digestNew {
				t.Fatalf("ManifestDigest = %q, %v", got, err)
			}
			if tokenAuth != basicHeader("me", "secret") {
				t.Errorf("the token service got Authorization %q, want the saved login", tokenAuth)
			}
		})
	}
}

func TestHTTPRegistry_ATokenServiceOnAnotherHostNeverGetsTheCredential(t *testing.T) {
	for name, realm := range map[string]string{
		"another host":                  "https://tokens.other.example/token",
		"the same host on another port": "https://example.com:8443/token",
	} {
		t.Run(name, func(t *testing.T) {
			lr := newLoginRegistry(t, fmt.Sprintf(`Bearer realm=%q,service="s"`, realm), "never", nil)
			src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "secret"}}}
			_, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).ManifestDigest(context.Background(), loginRef)
			if !errors.Is(err, ErrCredentialUnusable) {
				t.Fatalf("ManifestDigest = %v, want ErrCredentialUnusable, not an anonymous retry", err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the error carries the password: %v", err)
			}
			for _, req := range lr.requests() {
				if strings.Contains(req, "Basic") || strings.Contains(req, "/token") {
					t.Errorf("a request reached the token service or carried the login: %s", req)
				}
			}
		})
	}
}

func TestCredentialRealm(t *testing.T) {
	hub := "https://registry-1.docker.io/v2/library/nginx/manifests/latest"
	for name, tc := range map[string]struct {
		registry, request, realm string
		ok                       bool
	}{
		"the registry's own host":       {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "https://ghcr.io/token", true},
		"the same host in another case": {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "https://GHCR.io/token", true},
		"docker hub's token service":    {"docker.io", hub, "https://auth.docker.io/token", true},
		"docker hub's own host":         {"docker.io", hub, "https://registry-1.docker.io/token", true},
		"another host":                  {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "https://auth.docker.io/token", false},
		"docker hub's service for ghcr": {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "https://auth.docker.io/token", false},
		"a lookalike of docker hub's":   {"docker.io", hub, "https://auth.docker.io.evil.example/token", false},
		"another host for docker hub":   {"docker.io", hub, "https://evil.example/token", false},
		"the registry's host on a port": {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "https://ghcr.io:444/token", false},
		"plain http":                    {"ghcr.io", "https://ghcr.io/v2/x/manifests/y", "http://ghcr.io/token", false},
	} {
		t.Run(name, func(t *testing.T) {
			realm := mustParseURL(t, tc.realm)
			err := credentialRealm(tc.registry, tc.request, realm)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrCredentialUnusable)) {
				t.Fatalf("credentialRealm = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestHTTPRegistry_ACredentialIsNeverSentOverPlainHTTP(t *testing.T) {
	rs := newRegistryServer(t)
	ref := rs.ref()
	src := &credentialSource{creds: map[string]Credential{ref.Registry: {Username: "me", Password: "secret"}}}
	for name, call := range map[string]func(h *HTTPRegistry) error{
		"manifest": func(h *HTTPRegistry) error { _, err := h.ManifestDigest(context.Background(), ref); return err },
		"tags":     func(h *HTTPRegistry) error { _, err := h.Tags(context.Background(), ref); return err },
	} {
		err := call(&HTTPRegistry{Credentials: src})
		if !errors.Is(err, ErrCredentialUnusable) {
			t.Fatalf("%s = %v, want ErrCredentialUnusable: the registry is plain HTTP", name, err)
		}
	}
	if got := rs.seen(); len(got) != 0 {
		t.Fatalf("the plain-HTTP registry was asked %v, want nothing: not even anonymously", got)
	}
}

func TestHTTPRegistry_ACredentialThatCannotBeOpenedFailsWithoutAskingAnonymously(t *testing.T) {
	lr := newLoginRegistry(t, `Bearer realm="https://example.com/token"`, "Bearer tok-anon", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"token":"tok-anon"}`)
	})
	unusable := fmt.Errorf("%w: cleared by a restore", ErrCredentialUnusable)
	for name, call := range map[string]func(h *HTTPRegistry) error{
		"manifest": func(h *HTTPRegistry) error { _, err := h.ManifestDigest(context.Background(), loginRef); return err },
		"tags":     func(h *HTTPRegistry) error { _, err := h.Tags(context.Background(), loginRef); return err },
	} {
		err := call(&HTTPRegistry{Client: lr.client, Credentials: &credentialSource{err: unusable}})
		if !errors.Is(err, ErrCredentialUnusable) {
			t.Fatalf("%s = %v, want ErrCredentialUnusable", name, err)
		}
	}
	if got := lr.requests(); len(got) != 0 {
		t.Fatalf("the registry was asked %v after its credential failed to open, want nothing", got)
	}
}

func TestHTTPRegistry_ARefusedCredentialIsNotTheLoginWallOfAnonymousChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		challenge string
		token     func(w http.ResponseWriter, r *http.Request)
	}{
		"the token service refuses it": {
			`Bearer realm="https://example.com/token"`,
			func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
		},
		"the registry refuses the basic login": {`Basic realm="registry"`, nil},
		"the registry refuses the token":       {`Bearer realm="https://example.com/token"`, func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"token":"tok-no-access"}`) }},
	} {
		t.Run(name, func(t *testing.T) {
			lr := newLoginRegistry(t, tc.challenge, "never-matches", tc.token)
			src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "secret"}}}
			_, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).ManifestDigest(context.Background(), loginRef)
			if !errors.Is(err, ErrCredentialRejected) || errors.Is(err, ErrRegistryDenied) {
				t.Fatalf("ManifestDigest = %v, want ErrCredentialRejected and not the anonymous 'wants a login'", err)
			}
		})
	}
}

func TestHTTPRegistry_TheCredentialNeverFollowsARedirect(t *testing.T) {
	var mu sync.Mutex
	var elsewhere []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		elsewhere = append(elsewhere, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
	}))
	t.Cleanup(other.Close)
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		elsewhere = append(elsewhere, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
	}))
	t.Cleanup(secure.Close)

	for name, target := range map[string]string{
		"plain http on another host": other.URL,
		"https on another port":      "https://example.com:1",
		"https on another host":      secure.URL,
	} {
		t.Run(name, func(t *testing.T) {
			for stage, challenge := range map[string]string{
				"manifest request after a basic challenge": `Basic realm="registry"`,
				"token request": `Bearer realm="https://example.com/token"`,
			} {
				reg := newTLSRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/token":
						http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
					case r.Header.Get("Authorization") == "":
						w.Header().Set("WWW-Authenticate", challenge)
						w.WriteHeader(http.StatusUnauthorized)
					default:
						http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
					}
				}))
				src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "secret"}}}
				_, err := (&HTTPRegistry{Client: reg.client, Credentials: src}).ManifestDigest(context.Background(), loginRef)
				if err == nil || !strings.Contains(err.Error(), "same scheme, host and port") {
					t.Fatalf("%s: ManifestDigest = %v, want the redirect refused", stage, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(elsewhere) != 0 {
				t.Fatalf("the redirect target was asked: %v", elsewhere)
			}
		})
	}
}

func TestHTTPRegistry_TagPagesCarryTheLoginOnlyToTheRegistry(t *testing.T) {
	want := basicHeader("me", "secret")
	lr := &loginRegistry{}
	lr.tlsRegistry = newTLSRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lr.record(r)
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", `<https://evil.example/v2/acme/app/tags/list?page=2>; rel="next"`)
		}
		_, _ = fmt.Fprint(w, `{"tags":["1.0"]}`)
	}))
	src := &credentialSource{creds: map[string]Credential{"example.com": {Username: "me", Password: "secret"}}}
	_, err := (&HTTPRegistry{Client: lr.client, Credentials: src}).Tags(context.Background(), loginRef)
	if err == nil || !strings.Contains(err.Error(), "not on the registry") {
		t.Fatalf("Tags = %v, want the next page on another host refused", err)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
