package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/job"
)

func TestEventsRoleMatchesTheSpecsStreamEventsRole(t *testing.T) {
	got, ok := loadSpecRoles(t)["StreamEvents"]
	if !ok {
		t.Fatal("api/openapi.yaml declares no streamEvents operation")
	}
	if got != string(eventsRole) {
		t.Errorf("eventsRole = %q, the spec declares %q for streamEvents", eventsRole, got)
	}
}

func TestEnforceEventsRole(t *testing.T) {
	for _, role := range []Role{RoleViewer, RoleAdmin} {
		if err := EnforceEventsRole(role); err != nil {
			t.Errorf("EnforceEventsRole(%q) = %v, want nil", role, err)
		}
	}
	for _, role := range []Role{"share-only", "", "unknown"} {
		if err := EnforceEventsRole(role); !errors.Is(err, ErrRoleDenied) {
			t.Errorf("EnforceEventsRole(%q) = %v, want ErrRoleDenied", role, err)
		}
	}
}

func TestEventsHandlerAnswers403WhenAuthenticateReportsARoleDenial(t *testing.T) {
	h := &EventsHandler{
		Hub:          job.NewHub(),
		Authenticate: func(*http.Request) error { return fmt.Errorf("share-only: %w", ErrRoleDenied) },
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestEventsHandlerEndsTheStreamOnTheTickWhereAuthenticateStopsHolding(t *testing.T) {
	var calls atomic.Int32
	h := &EventsHandler{
		Hub: job.NewHub(),
		Authenticate: func(*http.Request) error {
			if calls.Add(1) > 3 {
				return ErrSessionInvalid
			}
			return nil
		},
		KeepAlive: 20 * time.Millisecond,
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("reading the stream until it ended: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("Authenticate ran %d times, want 4: once on connect and once per tick until it failed", got)
	}
}
