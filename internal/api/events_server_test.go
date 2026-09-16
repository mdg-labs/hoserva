package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/job"
)

// TestEventsHandler_RefusesWithNoAuthenticateSeamWired confirms events.go's
// documented default: until #22 wires Authenticate, every request is
// refused, exactly like SecurityHandler's own default for every generated
// operation.
func TestEventsHandler_RefusesWithNoAuthenticateSeamWired(t *testing.T) {
	h := &api.EventsHandler{Hub: job.NewHub()}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestEventsHandler_RefusesWhenAuthenticateErrors exercises the seam
// itself, standing in for what #22 will wire it to.
func TestEventsHandler_RefusesWhenAuthenticateErrors(t *testing.T) {
	h := &api.EventsHandler{
		Hub:          job.NewHub(),
		Authenticate: func(r *http.Request) error { return context.DeadlineExceeded },
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestEventsHandler_StreamsPublishedJobProgress confirms the whole path: a
// Publish on the Hub a running job's scheduler calls reaches a subscribed
// client as one framed job_progress SSE event, decodable only through the
// generated events package (D18).
func TestEventsHandler_StreamsPublishedJobProgress(t *testing.T) {
	hub := job.NewHub()
	h := &api.EventsHandler{
		Hub:          hub,
		Authenticate: func(r *http.Request) error { return nil },
		KeepAlive:    time.Hour, // long enough that this test never sees one
	}

	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	progress := 10
	published := &job.Job{
		ID:          "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		Type:        job.TypeSync,
		Class:       job.ClassParity,
		Status:      job.StatusRunning,
		Progress:    &progress,
		Cancellable: true,
		CreatedAt:   time.Now().UTC(),
	}

	// Publish can race Subscribe (the handler subscribes inside ServeHTTP,
	// invoked asynchronously by the HTTP server on Do above) — retry until
	// the frame is actually seen or the test's own context times out.
	frameCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				frameCh <- line
				return
			}
		}
	}()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		hub.Publish(published)
		select {
		case line := <-frameCh:
			if !strings.Contains(line, `"job_progress"`) {
				t.Fatalf("frame = %q, want a job_progress event", line)
			}
			if !strings.Contains(line, published.ID) {
				t.Fatalf("frame = %q, want it to carry the published job's id", line)
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("never received a job_progress frame")
}
