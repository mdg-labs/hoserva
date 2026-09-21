package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// apiTokenBytes is 256 bits, matching NewSessionToken's own budget (doc 01
// §7 says only "session ids are 256-bit random", but a personal API token
// is a bearer credential with the same replay stakes, so it gets the same
// entropy).
const apiTokenBytes = 32

// apiTokenPrefix marks a raw token as a Hoserva personal API token
// (Q43) — never part of the hash, only prepended to the value a caller
// sees and pastes into a script or --token flag. It exists so a token is
// recognizable at a glance (in a diff, a log line, a secret scanner) the
// same way GitHub's own `ghp_`-style prefixes are, without making the
// token any easier to guess (the random suffix carries all the entropy).
const apiTokenPrefix = "hspat_"

// NewAPIToken returns a fresh random personal API token: raw is the value
// shown to the caller once and never stored; hash is what api_tokens.
// token_hash records, so a leaked database file cannot be replayed as a
// live token (mirroring NewSessionToken's own reasoning).
func NewAPIToken() (raw, hash string, err error) {
	buf := make([]byte, apiTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating api token: %w", err)
	}
	raw = apiTokenPrefix + hex.EncodeToString(buf)
	return raw, HashAPIToken(raw), nil
}

// HashAPIToken hashes a raw API token exactly as NewAPIToken does, for
// looking up a presented bearer value against api_tokens.token_hash.
func HashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
