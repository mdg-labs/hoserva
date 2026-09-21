package auth

import (
	"strings"
	"testing"
)

func TestNewAPITokenHashMatches(t *testing.T) {
	raw, hash, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("expected a non-empty raw token and hash")
	}
	if hash != HashAPIToken(raw) {
		t.Error("HashAPIToken(raw) must reproduce the stored hash")
	}
}

func TestNewAPITokenHasRecognizablePrefix(t *testing.T) {
	raw, _, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		t.Errorf("token %q must start with %q", raw, apiTokenPrefix)
	}
}

func TestNewAPITokenUnique(t *testing.T) {
	_, h1, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	_, h2, err := NewAPIToken()
	if err != nil {
		t.Fatalf("NewAPIToken: %v", err)
	}
	if h1 == h2 {
		t.Error("two generated api tokens must not collide")
	}
}
