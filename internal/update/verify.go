package update

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// VerifySHA256SUMS reports whether sig is a valid Ed25519 signature of
// sums under pub — the same check openssl pkeyutl -verify -rawin performs
// for scripts/release/lib.sh's hoserva_sign_sha256sums (Q66).
func VerifySHA256SUMS(pub ed25519.PublicKey, sums, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("update: compiled-in public key is not a valid Ed25519 key")
	}
	if !ed25519.Verify(pub, sums, sig) {
		return fmt.Errorf("update: SHA256SUMS.sig does not verify")
	}
	return nil
}

// ChecksumFor returns the hex SHA-256 of filename recorded in a
// SHA256SUMS file (coreutils sha256sum format).
func ChecksumFor(sums []byte, filename string) (string, error) {
	want := path.Base(filename)
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok {
			sum, name, ok = strings.Cut(line, " *")
		}
		if !ok {
			continue
		}
		if path.Base(strings.TrimSpace(name)) == want {
			sum = strings.ToLower(strings.TrimSpace(sum))
			if len(sum) != sha256.Size*2 {
				return "", fmt.Errorf("update: SHA256SUMS entry for %s is not a SHA-256", want)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("update: SHA256SUMS has no entry for %s", want)
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func checksumsMatch(got, want string) bool {
	return strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want))
}
