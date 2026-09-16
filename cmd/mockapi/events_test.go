package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEventsRequiresCredential is the events.go counterpart of
// TestNoCredentialGetsHonest401: /api/v1/events has no generated security
// gate to lean on (ogen generates no server side for it, Q63), so
// hasCredential/writeUnauthorized reproduce the same 401 by hand, and this
// proves they actually run.
func TestEventsRequiresCredential(t *testing.T) {
	h, err := newEventsHandler("healthy")
	if err != nil {
		t.Fatalf("newEventsHandler: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestEventsSessionCookieIsAccepted matches TestSessionCookieOnlyIsAccepted:
// a bare cookie, no Authorization header, is what a browser sends.
func TestEventsSessionCookieIsAccepted(t *testing.T) {
	h, err := newEventsHandler("healthy")
	if err != nil {
		t.Fatalf("newEventsHandler: %v", err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: "any-value"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestEventsKeepAlive proves the periodic SSE comment actually arrives, at
// the configured interval, after the fixture's own events are exhausted.
func TestEventsKeepAlive(t *testing.T) {
	h, err := newEventsHandler("fresh-install")
	if err != nil {
		t.Fatalf("newEventsHandler: %v", err)
	}
	h.keepAlive = 10 * time.Millisecond

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: "any-value"})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v (never saw a keep-alive comment)", err)
		}
		if strings.HasPrefix(line, ": keep-alive") {
			return
		}
	}
}

// TestEventsServeHTTPReturnsOnDisconnect proves ServeHTTP's keep-alive loop
// exits promptly once the client goes away, rather than leaking a goroutine
// per connection for the process's lifetime.
func TestEventsServeHTTPReturnsOnDisconnect(t *testing.T) {
	h, err := newEventsHandler("healthy")
	if err != nil {
		t.Fatalf("newEventsHandler: %v", err)
	}
	h.keepAlive = time.Hour // only the disconnect below should end this call

	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "hoserva_session", Value: "any-value"})

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Give ServeHTTP time to replay the fixture's own frames before the
	// client disconnects, so this exercises the post-replay wait loop.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeHTTP did not return after the client disconnected")
	}
}
