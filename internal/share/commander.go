package share

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// CommandResult is what a finished command left: its output and exit status.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Commander runs an external command to completion. It returns an error only
// when the command could not be run to an exit (not found, cancelled); a
// command that exited non-zero is a CommandResult with its ExitCode, so a
// caller can tell "that account does not exist" from "the tool is broken".
// The real one is ExecCommander; FakeCommander scripts it for tests.
type Commander interface {
	Run(ctx context.Context, stdin string, name string, args ...string) (CommandResult, error)
}

// ExecCommander is the real Commander. It execs name with args as its argv,
// never a shell (CLAUDE.md: "never interpolate user or template input into a
// shell command"), and feeds stdin to the command when it is not empty.
type ExecCommander struct{}

var _ Commander = ExecCommander{}

func (ExecCommander) Run(ctx context.Context, stdin string, name string, args ...string) (CommandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	res := CommandResult{Stdout: stdout.String(), Stderr: strings.TrimSpace(stderr.String())}
	if err == nil {
		return res, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, fmt.Errorf("%s: %w", name, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, fmt.Errorf("%s: %w", name, err)
}

// FakeCall is one command a FakeCommander was asked to run.
type FakeCall struct {
	Name  string
	Args  []string
	Stdin string
}

// FakeCommander is a scriptable simulator of Commander (doc 06 §2): it records
// every call and answers each from Script, or with exit 0 and no output when
// Script is nil.
type FakeCommander struct {
	mu     sync.Mutex
	calls  []FakeCall
	Script func(call FakeCall) (CommandResult, error)
}

var _ Commander = (*FakeCommander)(nil)

func (f *FakeCommander) Run(ctx context.Context, stdin string, name string, args ...string) (CommandResult, error) {
	call := FakeCall{Name: name, Args: append([]string(nil), args...), Stdin: stdin}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	script := f.Script
	f.mu.Unlock()
	if script == nil {
		return CommandResult{}, nil
	}
	return script(call)
}

// Calls returns every call made so far, in order.
func (f *FakeCommander) Calls() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeCall(nil), f.calls...)
}
