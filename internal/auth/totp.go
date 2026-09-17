package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 specifies HMAC-SHA1 for TOTP; this is not used for anything else.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"
)

// TOTP follows RFC 6238: HMAC-SHA1, a 30-second step and 6-digit codes —
// the parameters every authenticator app assumes when it has no other
// hint (Google Authenticator, Authy, etc.).
const (
	totpSecretLen = 20 // 160 bits, RFC 4226's recommended HMAC-SHA1 key size
	totpStep      = 30 * time.Second
	totpDigits    = 6
	// totpWindow is the tolerance either side of the current step (doc 01
	// §7): a code from the previous or next step is still accepted, for
	// clock drift between the server and the user's phone.
	totpWindow = 1
)

// GenerateTOTPSecret returns a new random secret, base32-encoded (no
// padding) for storage and display.
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretLen)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating TOTP secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// OtpauthURI builds the otpauth:// URI an authenticator app's QR-code
// scanner expects (doc 03 §1).
func OtpauthURI(issuer, accountName, secretBase32 string) string {
	label := url.PathEscape(fmt.Sprintf("%s:%s", issuer, accountName))
	v := url.Values{}
	v.Set("secret", secretBase32)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprintf("%d", totpDigits))
	v.Set("period", fmt.Sprintf("%d", int(totpStep.Seconds())))
	return fmt.Sprintf("otpauth://totp/%s?%s", label, v.Encode())
}

func totpCodeAtStep(secretBase32 string, step int64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secretBase32))
	if err != nil {
		return "", fmt.Errorf("decoding TOTP secret: %w", err)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))

	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	code := truncated % uint32(math.Pow10(totpDigits))
	return fmt.Sprintf("%0*d", totpDigits, code), nil
}

// CurrentCode returns the TOTP code for secretBase32 at now. The daemon
// itself never generates a code — only ValidateTOTP, below, matters in
// production; this is exported only so a caller outside this package
// (internal/api's own tests) can drive a TOTP flow end to end without
// reimplementing RFC 6238.
func CurrentCode(secretBase32 string, now time.Time) (string, error) {
	return totpCodeAtStep(secretBase32, now.Unix()/int64(totpStep.Seconds()))
}

// ValidateTOTP checks code against secretBase32 at now, within
// totpWindow steps either side, and against replay: a step at or before
// lastAcceptedStep never validates again, even if it's still within the
// clock-drift window (doc 01 §7's "replay protection within a step").
// lastAcceptedStep is 0 (the Unix epoch's own step) before any code has
// ever been accepted. On success it returns the step that matched, for
// the caller to persist as the new lastAcceptedStep.
func ValidateTOTP(secretBase32, code string, now time.Time, lastAcceptedStep int64) (acceptedStep int64, ok bool) {
	current := now.Unix() / int64(totpStep.Seconds())
	for offset := -totpWindow; offset <= totpWindow; offset++ {
		step := current + int64(offset)
		if step <= lastAcceptedStep {
			continue
		}
		want, err := totpCodeAtStep(secretBase32, step)
		if err != nil {
			return 0, false
		}
		if hmac.Equal([]byte(want), []byte(code)) {
			return step, true
		}
	}
	return 0, false
}
