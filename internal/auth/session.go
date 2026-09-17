package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// sessionTokenBytes is 256 bits (doc 01 §7: "session ids are 256-bit
// random").
const sessionTokenBytes = 32

// NewSessionToken returns a fresh random session token: raw is the value
// set in the cookie and never stored; hash is what sessions.token_hash
// records, so a stolen database dump cannot be replayed as a session
// (mirroring password hashing's own reasoning).
func NewSessionToken() (raw, hash string, err error) {
	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating session token: %w", err)
	}
	raw = hex.EncodeToString(buf)
	return raw, HashSessionToken(raw), nil
}

// HashSessionToken hashes a raw session token exactly as NewSessionToken
// does, for looking up a presented cookie value against sessions.token_hash.
func HashSessionToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
