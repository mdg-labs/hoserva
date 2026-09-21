package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/api"
)

// TestListSessionsAndRevoke is #49's fifth acceptance criterion: an
// admin can see every active session and revoke one, ending it
// immediately.
func TestListSessionsAndRevoke(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	_, token, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple")
	if err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	sessions, err := svc.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Username != "admin" {
		t.Fatalf("ListSessions = %+v, want exactly one admin session", sessions)
	}

	if err := svc.RevokeSession(ctx, sessions[0].TokenHash); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := svc.ValidateSession(ctx, token); !errors.Is(err, api.ErrSessionInvalid) {
		t.Errorf("ValidateSession after RevokeSession = %v, want ErrSessionInvalid", err)
	}

	if err := svc.RevokeSession(ctx, sessions[0].TokenHash); !errors.Is(err, api.ErrSessionNotFound) {
		t.Errorf("RevokeSession(already revoked) = %v, want ErrSessionNotFound", err)
	}
}

// TestListSessionsExcludesExpired guards against an expired-but-not-yet-swept
// session row appearing as "active" in the admin session list.
func TestListSessionsExcludesExpired(t *testing.T) {
	svc, _ := newAuthTestService(t)
	ctx := context.Background()
	start := time.Now()
	svc.Now = func() time.Time { return start }

	if _, _, err := svc.CreateFirstAdmin(ctx, "admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateFirstAdmin: %v", err)
	}

	svc.Now = func() time.Time { return start.Add(31 * 24 * time.Hour) }
	sessions, err := svc.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("ListSessions after the session's TTL elapsed = %+v, want none", sessions)
	}
}
