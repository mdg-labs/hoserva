package parity

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
)

// Runner execs a real snapraid process — always as its own argv, never a
// shell (CLAUDE.md: "never interpolate user or template input into a
// shell command"). It sits behind this package interface so
// SnapraidEngine can be tested against a scriptable fake instead of a
// real binary (CLAUDE.md), the same shape internal/disk's own Runner
// uses for smartctl/hdparm.
type Runner interface {
	Start(ctx context.Context, name string, args ...string) (Process, error)
}

// Process is one running snapraid invocation. Lines streams its combined
// stdout/stderr, split at every '\n' *and* every '\r' — SnapRAID rewrites
// its own progress line with a bare '\r' rather than a newline per
// update (progress_parse.go), so treating '\r' as a line terminator too
// is what turns that one rewritten line into the individual updates a
// caller can parse. Lines always runs to completion (closes once the
// process's output reaches EOF) regardless of whether a caller stops
// reading Progress ticks early — a caller must still drain it fully
// before calling Wait, or the process's own writes can block forever
// with nothing reading them.
type Process interface {
	Lines() <-chan string
	Wait() error
}

// CommandRunner is the real Runner.
type CommandRunner struct{}

func (CommandRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	p := &process{lines: make(chan string), done: make(chan struct{})}

	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}

	go func() {
		defer close(p.lines)
		scanner := bufio.NewScanner(pr)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		scanner.Split(scanLinesOrCR)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
	}()

	go func() {
		p.waitErr = cmd.Wait()
		_ = pw.Close()
		close(p.done)
	}()

	return p, nil
}

type process struct {
	lines   chan string
	done    chan struct{}
	waitErr error
}

func (p *process) Lines() <-chan string { return p.lines }

// Wait blocks until the process has exited and its stdout/stderr copy
// goroutine (inside os/exec) has finished — which itself only happens
// once nothing is left unread on the other end of the pipe, so Wait must
// not be called until Lines has been drained to its own close.
func (p *process) Wait() error {
	<-p.done
	return p.waitErr
}

var _ Runner = CommandRunner{}

// scanLinesOrCR is bufio.ScanLines, extended to also break a line at a
// bare '\r' (SnapRAID's own progress-line rewrite, see Process's doc
// comment), not just '\r\n' or '\n'.
func scanLinesOrCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
