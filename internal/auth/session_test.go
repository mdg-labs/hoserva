package auth

import "testing"

func TestNewSessionTokenHashMatches(t *testing.T) {
	raw, hash, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("expected a non-empty raw token and hash")
	}
	if hash != HashSessionToken(raw) {
		t.Error("HashSessionToken(raw) must reproduce the stored hash")
	}
}

func TestNewSessionTokenUnique(t *testing.T) {
	_, h1, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	_, h2, err := NewSessionToken()
	if err != nil {
		t.Fatalf("NewSessionToken: %v", err)
	}
	if h1 == h2 {
		t.Error("two generated session tokens must not collide")
	}
}
