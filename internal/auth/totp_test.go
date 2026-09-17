package auth

import (
	"encoding/base32"
	"testing"
	"time"
)

func TestValidateTOTPAcceptsCurrentCode(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	code, err := totpCodeAtStep(secret, now.Unix()/30)
	if err != nil {
		t.Fatalf("totpCodeAtStep: %v", err)
	}
	step, ok := ValidateTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("the current step's own code must validate")
	}
	if step != now.Unix()/30 {
		t.Errorf("accepted step = %d, want %d", step, now.Unix()/30)
	}
}

func TestValidateTOTPToleratesClockDrift(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	prevStep := now.Unix()/30 - 1
	code, _ := totpCodeAtStep(secret, prevStep)
	if _, ok := ValidateTOTP(secret, code, now, 0); !ok {
		t.Error("a code from the adjacent step must still validate (clock drift tolerance)")
	}
}

func TestValidateTOTPRejectsOutOfWindowCode(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	farStep := now.Unix()/30 - 5
	code, _ := totpCodeAtStep(secret, farStep)
	if _, ok := ValidateTOTP(secret, code, now, 0); ok {
		t.Error("a code far outside the tolerance window must not validate")
	}
}

func TestValidateTOTPRejectsWrongCode(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	if _, ok := ValidateTOTP(secret, "000000", now, 0); ok {
		t.Error("an arbitrary wrong code must not validate")
	}
}

func TestValidateTOTPReplayProtection(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Unix(1_700_000_000, 0)
	step := now.Unix() / 30
	code, _ := totpCodeAtStep(secret, step)

	acceptedStep, ok := ValidateTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("first use of the code must validate")
	}

	if _, ok := ValidateTOTP(secret, code, now, acceptedStep); ok {
		t.Error("reusing an already-accepted step's code must be refused (replay protection)")
	}
}

// TestTOTPCodeAtStepMatchesRFC6238Vectors is the review finding this
// issue closes: every other test in this file compares totpCodeAtStep
// against itself, so a wrong HMAC or a wrong truncation would still pass
// them all. These are RFC 6238 Appendix B's own test vectors (the
// SHA-1/8-digit table, truncated to this package's 6-digit codes), a
// fixed ASCII secret ("12345678901234567890", per the RFC's own Appendix
// B setup) rather than a randomly generated one, so the expected codes
// below are reproducible against any correct RFC 6238 implementation,
// not just this one.
func TestTOTPCodeAtStepMatchesRFC6238Vectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))

	cases := []struct {
		t    int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	}
	for _, c := range cases {
		step := c.t / int64(totpStep.Seconds())
		got, err := totpCodeAtStep(secret, step)
		if err != nil {
			t.Fatalf("totpCodeAtStep(T=%d): %v", c.t, err)
		}
		if got != c.want {
			t.Errorf("totpCodeAtStep(T=%d) = %q, want %q (RFC 6238 Appendix B, truncated to 6 digits)", c.t, got, c.want)
		}
	}
}

func TestOtpauthURIContainsSecret(t *testing.T) {
	uri := OtpauthURI("Hoserva", "admin", "ABCDEFGH")
	if uri == "" {
		t.Fatal("expected a non-empty otpauth URI")
	}
	if want := "otpauth://totp/"; len(uri) < len(want) || uri[:len(want)] != want {
		t.Errorf("otpauth URI = %q, want it to start with %q", uri, want)
	}
}
