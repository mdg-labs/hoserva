package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

const (
	sessionTestHost = "hoserva.lan:8008"
	sessionTestJob  = "/api/v1/jobs/3fa85f64-5717-4562-b3fc-2c963f66afa6/cancel"
)

// signedInSession is a login through the TCP server's own handler: the
// cookie the browser stores and the second secret it keeps for itself.
type signedInSession struct {
	cookie *http.Cookie
	secret string
}

func newSessionTCPServer(t *testing.T) *http.Server {
	t.Helper()
	w := newWiredInstall(t)
	w.handler.Auth = w.authService
	if _, _, err := w.authService.CreateFirstAdmin(context.Background(), "alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	srv, err := buildTCPServer(w.handler, w.authStore, w.authService, job.NewHub(), notify.NewHub(), fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("spa")}})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func loginThroughTCP(t *testing.T, srv *http.Server) signedInSession {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://"+sessionTestHost+"/api/v1/auth/login",
		strings.NewReader(`{"username":"alice","password":"correct horse battery staple"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s, want 200", rec.Code, rec.Body)
	}
	var s signedInSession
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hoserva_session" {
			s.cookie = c
		}
	}
	s.secret = rec.Header().Get(api.SessionSecretHeader)
	if s.cookie == nil || s.secret == "" {
		t.Fatalf("login set cookie %v and secret %q, want both", s.cookie, s.secret)
	}
	return s
}

func (s signedInSession) request(method, path string, withSecret bool) *http.Request {
	req := httptest.NewRequest(method, "https://"+sessionTestHost+path, nil)
	req.AddCookie(s.cookie)
	if withSecret {
		req.Header.Set(api.SessionSecretHeader, s.secret)
	}
	return req
}

func serve(srv *http.Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestTCPServer_ASessionCookieAloneDoesNotAuthenticate(t *testing.T) {
	srv := newSessionTCPServer(t)
	s := loginThroughTCP(t, srv)
	other := loginThroughTCP(t, srv)

	if rec := serve(srv, s.request(http.MethodGet, "/api/v1/jobs", true)); rec.Code != http.StatusOK {
		t.Fatalf("GET /jobs with cookie and secret = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := serve(srv, s.request(http.MethodGet, "/api/v1/jobs", false)); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /jobs with the cookie alone = %d, want 401", rec.Code)
	}
	if rec := serve(srv, s.request(http.MethodPost, sessionTestJob, false)); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST cancelJob (admin) with the cookie alone = %d, want 401", rec.Code)
	}
	if rec := serve(srv, s.request(http.MethodPost, sessionTestJob, true)); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Errorf("POST cancelJob with cookie and secret = %d %s, want it to reach the operation", rec.Code, rec.Body)
	}

	crossed := s.request(http.MethodGet, "/api/v1/jobs", false)
	crossed.Header.Set(api.SessionSecretHeader, other.secret)
	if rec := serve(srv, crossed); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /jobs with another session's secret = %d, want 401", rec.Code)
	}
	garbage := s.request(http.MethodGet, "/api/v1/jobs", false)
	garbage.Header.Set(api.SessionSecretHeader, "not-hex")
	if rec := serve(srv, garbage); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /jobs with a malformed secret = %d, want 401", rec.Code)
	}
}

func TestTCPServer_EventsStreamNeedsTheSessionSecret(t *testing.T) {
	srv := newSessionTCPServer(t)
	s := loginThroughTCP(t, srv)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	status := func(withSecret bool) int {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(s.cookie)
		if withSecret {
			req.Header.Set(api.SessionSecretHeader, s.secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /events: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := status(false); code != http.StatusUnauthorized {
		t.Errorf("GET /events with the cookie alone = %d, want 401", code)
	}
	if code := status(true); code != http.StatusOK {
		t.Errorf("GET /events with cookie and secret = %d, want 200", code)
	}
}

func TestTCPServer_RefusesAStateChangeFromAnotherOrigin(t *testing.T) {
	srv := newSessionTCPServer(t)
	s := loginThroughTCP(t, srv)

	post := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := s.request(http.MethodPost, sessionTestJob, true)
		mutate(req)
		return serve(srv, req)
	}
	for name, mutate := range map[string]func(*http.Request){
		"another port of the same host": func(r *http.Request) { r.Header.Set("Origin", "https://hoserva.lan:8443") },
		"another host":                  func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"the null origin":               func(r *http.Request) { r.Header.Set("Origin", "null") },
		"plain http on this host":       func(r *http.Request) { r.Header.Set("Origin", "http://"+sessionTestHost) },
		"a same-site fetch":             func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"a cross-site fetch":            func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		if rec := post(mutate); rec.Code != http.StatusForbidden {
			t.Errorf("POST from %s with a valid cookie and secret = %d %s, want 403", name, rec.Code, rec.Body)
		}
	}
	for name, mutate := range map[string]func(*http.Request){
		"this server's own origin":          func(r *http.Request) { r.Header.Set("Origin", "https://"+sessionTestHost) },
		"a same-origin fetch":               func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
		"a request with no browser headers": func(*http.Request) {},
	} {
		if rec := post(mutate); rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Errorf("POST from %s = %d %s, want it to reach the operation", name, rec.Code, rec.Body)
		}
	}
}

func TestTCPServer_LoginIgnoresAStaleCookieWithoutASecret(t *testing.T) {
	srv := newSessionTCPServer(t)
	req := httptest.NewRequest(http.MethodPost, "https://"+sessionTestHost+"/api/v1/auth/login",
		strings.NewReader(`{"username":"alice","password":"correct horse battery staple"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: "from-before-the-upgrade"})
	if rec := serve(srv, req); rec.Code != http.StatusOK {
		t.Errorf("login with a stale cookie and no secret = %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestTCPServer_CatalogImagesNeedOnlyTheCookie(t *testing.T) {
	srv := newSessionTCPServer(t)
	s := loginThroughTCP(t, srv)
	for _, path := range []string{"/api/v1/catalog/nginx/icon", "/api/v1/catalog/nginx/screenshots/0"} {
		if rec := serve(srv, s.request(http.MethodGet, path, false)); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("GET %s with the cookie alone = %d %s, want it to reach the operation", path, rec.Code, rec.Body)
		}
	}
}
