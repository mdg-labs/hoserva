package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestVerifySHA256SUMS_RejectsWrongKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("deadbeef  hoserva_0.1.0_amd64.deb\n")
	sig := ed25519.Sign(priv, sums)
	if err := VerifySHA256SUMS(otherPub, sums, sig); err == nil {
		t.Fatal("signature from a different key verified")
	}
}

func TestVerifySHA256SUMS_AcceptsOpenSSLSignature(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "priv.pem")
	pub := filepath.Join(dir, "pub.pem")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	sigPath := filepath.Join(dir, "SHA256SUMS.sig")

	if out, err := exec.Command("openssl", "genpkey", "-algorithm", "ed25519", "-out", priv).CombinedOutput(); err != nil {
		t.Skipf("openssl not available: %v\n%s", err, out)
	}
	if out, err := exec.Command("openssl", "pkey", "-in", priv, "-pubout", "-out", pub).CombinedOutput(); err != nil {
		t.Fatalf("export public key: %v\n%s", err, out)
	}
	sums := []byte("deadbeef  hoserva_0.1.0_amd64.deb\n")
	if err := os.WriteFile(sumsPath, sums, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("openssl", "pkeyutl", "-sign", "-inkey", priv, "-rawin", "-in", sumsPath, "-out", sigPath).CombinedOutput(); err != nil {
		t.Fatalf("openssl sign: %v\n%s", err, out)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	derOut, err := exec.Command("openssl", "pkey", "-in", pub, "-pubin", "-outform", "DER").Output()
	if err != nil {
		t.Fatalf("export DER public key: %v", err)
	}
	if len(derOut) < ed25519.PublicKeySize {
		t.Fatalf("DER public key too short: %d bytes", len(derOut))
	}
	rawPub := ed25519.PublicKey(derOut[len(derOut)-ed25519.PublicKeySize:])
	if err := VerifySHA256SUMS(rawPub, sums, sig); err != nil {
		t.Fatalf("openssl-produced signature rejected: %v", err)
	}
}

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
