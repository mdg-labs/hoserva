package update

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

func TestEngine_DefaultPublicKeyIsEmbedded(t *testing.T) {
	e := &Engine{}
	got := e.publicKey()
	if len(got) != ed25519.PublicKeySize {
		t.Fatalf("default public key length = %d, want %d", len(got), ed25519.PublicKeySize)
	}
	if !bytes.Equal(got, EmbeddedPublicKey) {
		t.Fatal("Engine.publicKey without override is not EmbeddedPublicKey")
	}
}
