package disk

import (
	"context"
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
// discard output on a non-nil error.
func (CommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.Output()
}

var _ Runner = CommandRunner{}
