package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/notify"
)

const eventsTestKeepAlive = 40 * time.Millisecond

// viewerSession is a viewer account signed in the way a browser is: the raw
// session cookie and the session's second secret.
type viewerSession struct {
	userID, token, secret string
}

func newViewerSession(t *testing.T, w *wiredInstall) viewerSession {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	viewer := &api.User{ID: "6a3f1c52-0f4e-4a3e-9c53-0d0c9d2a7b11", Username: "viewer", Role: "viewer", PasswordHash: hash, CreatedAt: time.Now()}
	if err := w.authStore.CreateUser(ctx, viewer); err != nil {
		t.Fatal(err)
	}
	_, token, err := w.authService.Login(ctx, "viewer", "correct horse battery staple", "", "")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := w.authService.SessionSecret(token)
	if err != nil {
		t.Fatal(err)
	}
	return viewerSession{userID: viewer.ID, token: token, secret: secret}
}

func (s viewerSession) open(t *testing.T, ts *httptest.Server) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: s.token})
	req.Header.Set(api.SessionSecretHeader, s.secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// streamEnds reports whether the stream body reaches its end within d.
func streamEnds(body io.Reader, d time.Duration) bool {
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, bufio.NewReader(body))
		close(ended)
	}()
	select {
	case <-ended:
		return true
	case <-time.After(d):
		return false
	}
}

func newWiredEventsServer(t *testing.T) (*wiredInstall, *httptest.Server) {
	t.Helper()
	w := newWiredInstall(t)
	w.handler.Auth = w.authService
	if _, _, err := w.authService.CreateFirstAdmin(context.Background(), "alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	events := &api.EventsHandler{
		Hub:          job.NewHub(),
		NotifyHub:    notify.NewHub(),
		Authenticate: tcpEventsAuthenticate(w.authService),
		KeepAlive:    eventsTestKeepAlive,
	}
	mux := http.NewServeMux()
	mux.Handle(apiPathPrefix+"/events", events)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return w, ts
}

func TestTCPEvents_ARoleTheContractRefusesGetsNoStream(t *testing.T) {
	w := newWiredInstall(t)
	w.handler.Auth = w.authService
	if _, _, err := w.authService.CreateFirstAdmin(context.Background(), "alice", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	tcp, err := buildTCPServer(w.handler, w.authStore, w.authService, job.NewHub(), notify.NewHub(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(tcp.Handler)
	t.Cleanup(ts.Close)

	session := newViewerSession(t, w)
	if resp := session.open(t, ts); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events as a viewer = %d, want 200", resp.StatusCode)
	}

	if err := w.authStore.UpdateUserRole(context.Background(), session.userID, "share-only"); err != nil {
		t.Fatal(err)
	}
	resp := session.open(t, ts)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), `"forbidden"`) {
		t.Errorf("GET /events as a demoted share-only account = %d %s, want 403 forbidden", resp.StatusCode, body)
	}
}

func TestTCPEvents_AnOpenStreamEndsWithinAKeepAliveOfItsSessionBeingRevoked(t *testing.T) {
	w, ts := newWiredEventsServer(t)
	session := newViewerSession(t, w)

	resp := session.open(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d, want 200", resp.StatusCode)
	}
	if streamEnds(resp.Body, 4*eventsTestKeepAlive) {
		t.Fatal("the stream ended while its session was still valid")
	}

	if err := w.authStore.DeleteSession(context.Background(), auth.HashSessionToken(session.token)); err != nil {
		t.Fatal(err)
	}
	if !streamEnds(resp.Body, 20*eventsTestKeepAlive) {
		t.Error("the stream stayed open after its session was revoked")
	}
}

func TestTCPEvents_AnOpenStreamEndsWithinAKeepAliveOfItsAccountBeingDemoted(t *testing.T) {
	w, ts := newWiredEventsServer(t)
	session := newViewerSession(t, w)

	resp := session.open(t, ts)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events = %d, want 200", resp.StatusCode)
	}
	if err := w.authStore.UpdateUserRole(context.Background(), session.userID, "share-only"); err != nil {
		t.Fatal(err)
	}
	if !streamEnds(resp.Body, 20*eventsTestKeepAlive) {
		t.Error("the stream stayed open after its account was demoted to share-only")
	}
}
