package backup

import (
	"errors"
	"testing"
)

func TestValidateRemoteDestination(t *testing.T) {
	if err := ValidateRemoteDestination(true); err != nil {
		t.Fatalf("expected no error with a passphrase set, got %v", err)
	}
	err := ValidateRemoteDestination(false)
	if err == nil {
		t.Fatal("expected an error with no passphrase set")
	}
	if !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("expected ErrPassphraseRequired, got %v", err)
	}
}
