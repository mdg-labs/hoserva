package disk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Runner runs an external command and returns its stdout. Every
// system-touching call the real Provider makes — smartctl, hdparm — goes
// through Runner, so it sits behind a package interface with a
// scriptable fake (CLAUDE.md). CommandRunner is the only Runner that
// ever execs a real binary, and it always execs an argv, never a shell
// (CLAUDE.md: "never interpolate user or template input into a shell
// command").
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandRunner is the real Runner.
type CommandRunner struct{}

// Run execs name with args as its argv and returns its stdout. A
// non-zero exit is returned as an error, but the stdout already captured
// is still returned alongside it — smartctl's own exit status encodes
// SMART health bits, not "the command failed", so callers must not
// discard output on a non-nil error. On a non-zero exit, the returned
// error's message includes the command's captured stderr, since that is
// usually the only diagnostic the failing tool gave.
func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := bytes.TrimSpace(exitErr.Stderr); len(stderr) > 0 {
				return out, fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return out, err
	}
	return out, nil
}

var _ Runner = CommandRunner{}
