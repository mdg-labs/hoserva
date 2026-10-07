package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestExecutePrintsAnErrorOnceAndEscaped(t *testing.T) {
	root := rootCmd()
	root.SetArgs([]string{"--x\x1b[2K"})
	var stderr bytes.Buffer
	if code := execute(root, &stderr); code != exitError {
		t.Errorf("execute = %d, want %d", code, exitError)
	}
	got := stderr.String()
	assertNoTerminalControl(t, strings.ReplaceAll(got, "\n", ""))
	if n := strings.Count(got, `x\x1b[2K`); n != 1 {
		t.Errorf("stderr holds the error %d times, want once: %q", n, got)
	}
	if !strings.HasPrefix(got, "hoserva: unknown flag: --x") {
		t.Errorf("stderr = %q, want it to start with the escaped error", got)
	}
}
