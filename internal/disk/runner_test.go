package disk

import (
	"context"
	"strings"
	"testing"
)

// TestCommandRunner_Run_FailureIncludesStderr is this issue's central
// property (#140): a non-zero exit must surface the tool's own stderr in
// the returned error, not just "exit status 1". The fake command here is
// a shell script, never a real storage tool (CLAUDE.md).
func TestCommandRunner_Run_FailureIncludesStderr(t *testing.T) {
	r := CommandRunner{}
	_, err := r.Run(context.Background(), "sh", "-c", "echo failmsg >&2; exit 1")
	if err == nil {
		t.Fatal("Run: got nil error, want a non-nil error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "failmsg") {
		t.Fatalf("Run error = %q, want it to contain the command's stderr %q", err.Error(), "failmsg")
	}
}

// TestCommandRunner_Run_Success asserts stdout capture is unchanged by
// this issue's fix.
func TestCommandRunner_Run_Success(t *testing.T) {
	r := CommandRunner{}
	out, err := r.Run(context.Background(), "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "hello" {
		t.Fatalf("Run stdout = %q, want %q", got, "hello")
	}
}

// TestCommandRunner_Run_FailureWithoutStderr asserts a non-zero exit with
// no stderr output still returns a non-nil error, without panicking or
// producing a malformed message.
func TestCommandRunner_Run_FailureWithoutStderr(t *testing.T) {
	r := CommandRunner{}
	_, err := r.Run(context.Background(), "sh", "-c", "exit 1")
	if err == nil {
		t.Fatal("Run: got nil error, want a non-nil error for a non-zero exit")
	}
}
