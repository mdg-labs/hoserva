package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestVerifySHA256SUMS_RejectsTamperedSums(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("deadbeef  hoserva_0.1.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)
	if err := VerifySHA256SUMS(pub, sums, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	tampered := append([]byte{}, sums...)
	tampered[0] = 'c'
	if err := VerifySHA256SUMS(pub, tampered, sig); err == nil {
		t.Fatal("tampered SHA256SUMS still verified")
	}
}

func TestChecksumFor(t *testing.T) {
	sums := []byte("abc  hoserva_0.1.0_amd64.deb\ndef  other.deb\n")
	if _, err := ChecksumFor(sums, "hoserva_0.1.0_amd64.deb"); err == nil {
		t.Fatal("short checksum should fail")
	}
	good := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  hoserva_0.1.0_amd64.deb\n")
	got, err := ChecksumFor(good, "hoserva_0.1.0_amd64.deb")
	if err != nil {
		t.Fatal(err)
	}
	if got != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("got %s", got)
	}
}
