package parity

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// SnapraidEngine is the real Engine (doc 01 §4): it drives a real
// `snapraid` binary against a config file it never generates itself
// (Layout.Render, #26 — D1: Hoserva orchestrates SnapRAID, it never
// reimplements it). Every invocation carries its own `-l <log>` file —
// SnapRAID's own structured, machine-readable log format, confirmed
// against a real snapraid 12.4-1 binary — which every parser in this
// package reads after the run finishes; a caller never has to scrape
// SnapRAID's human-formatted text.
type SnapraidEngine struct {
	// Binary is the snapraid executable to run. Defaults to "snapraid".
	Binary string
	// ConfPath is the rendered snapraid.conf this engine drives.
	ConfPath string
	// LogDir is where each invocation's own -l log is written; created if
	// missing. Defaults to os.TempDir(). A dev daemon or a lab test points
	// this at a workspace-local state directory (CLAUDE.md) — never
	// /var/lib/hoserva.
	LogDir string
	// Runner execs the real snapraid process. Defaults to CommandRunner{}.
	Runner Runner
}

func (e *SnapraidEngine) binary() string {
	if e.Binary != "" {
		return e.Binary
	}
	return "snapraid"
}

func (e *SnapraidEngine) runner() Runner {
	if e.Runner != nil {
		return e.Runner
	}
	return CommandRunner{}
}

// newLog creates a fresh, empty, uniquely named log file under LogDir for
// one invocation to write its own `-l` output to, and returns its path
// with a cleanup func that removes it once the caller is done reading it.
func (e *SnapraidEngine) newLog(op string) (path string, cleanup func(), err error) {
	dir := e.LogDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("parity: creating log dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "snapraid-"+op+"-*.log")
	if err != nil {
		return "", nil, fmt.Errorf("parity: creating log file: %w", err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("parity: closing log file: %w", err)
	}
	return name, func() { _ = os.Remove(name) }, nil
}

func (e *SnapraidEngine) argv(logPath string, tail []string) []string {
	return append([]string{"-c", e.ConfPath, "-l", logPath}, tail...)
}

// runToCompletion execs tail's operation, draining and discarding its
// live output (used for the short, synchronous operations — diff,
// status, touch — that never report a live percentage), and returns the
// process's own wait error (nil on a normal exit, whatever its code).
func (e *SnapraidEngine) runToCompletion(ctx context.Context, logPath string, tail []string) error {
	proc, err := e.runner().Start(ctx, e.binary(), e.argv(logPath, tail)...)
	if err != nil {
		return err
	}
	for range proc.Lines() {
	}
	return proc.Wait()
}

// exitCoder is the method os/exec's own *exec.ExitError provides
// (ExitCode() int) — matched by interface here, rather than against the
// concrete *exec.ExitError type, so SnapraidEngine's own exit-code
// classification can be exercised against a scripted error in a test
// without spawning a real process just to obtain one.
type exitCoder interface{ ExitCode() int }

func exitCode(err error) (code int, ok bool) {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode(), true
	}
	return 0, false
}

// runStatus runs a real `snapraid status` to completion and parses it —
// the shared "before" snapshot Diff needs, and the pure engine of the
// public Status.
func (e *SnapraidEngine) runStatus(ctx context.Context) (StatusReport, error) {
	logPath, cleanup, err := e.newLog("status")
	if err != nil {
		return StatusReport{}, err
	}
	defer cleanup()

	if err := e.runToCompletion(ctx, logPath, statusArgv()); err != nil {
		return StatusReport{}, fmt.Errorf("parity: snapraid status: %w", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		return StatusReport{}, fmt.Errorf("parity: reading status log: %w", err)
	}
	return ParseStatus(data)
}

func (e *SnapraidEngine) Status(ctx context.Context) (ParityStatus, error) {
	r, err := e.runStatus(ctx)
	if err != nil {
		return ParityStatus{}, err
	}
	return r.ToParityStatus(), nil
}

// Diff runs `snapraid status` (for the before-counts BuildDiffReport
// needs) and then `snapraid diff`, and combines them into the public
// DiffReport (doc 02 §2). Diff's own exit code 2 ("There are
// differences!") is diff's normal, expected result, never an error; only
// an exit code diff itself never documents is treated as one.
func (e *SnapraidEngine) Diff(ctx context.Context) (DiffReport, error) {
	before, err := e.runStatus(ctx)
	if err != nil {
		return DiffReport{}, err
	}

	logPath, cleanup, err := e.newLog("diff")
	if err != nil {
		return DiffReport{}, err
	}
	defer cleanup()

	runErr := e.runToCompletion(ctx, logPath, diffArgv())
	if runErr != nil {
		if code, ok := exitCode(runErr); !ok || code != 2 {
			return DiffReport{}, fmt.Errorf("parity: snapraid diff: %w", runErr)
		}
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		return DiffReport{}, fmt.Errorf("parity: reading diff log: %w", err)
	}
	d, err := ParseDiff(data)
	if err != nil {
		return DiffReport{}, err
	}
	return BuildDiffReport(before, d), nil
}

// touchIfNeeded is Q17's own rule: run `snapraid touch` before a sync
// only when `status` reports files with a zero sub-second timestamp.
func (e *SnapraidEngine) touchIfNeeded(ctx context.Context) error {
	status, err := e.runStatus(ctx)
	if err != nil {
		return err
	}
	if status.ZeroSubsecondFiles == 0 {
		return nil
	}
	logPath, cleanup, err := e.newLog("touch")
	if err != nil {
		return err
	}
	defer cleanup()
	if err := e.runToCompletion(ctx, logPath, touchArgv()); err != nil {
		return fmt.Errorf("parity: snapraid touch: %w", err)
	}
	return nil
}

// runStream execs tail's operation, streaming SnapRAID's own progress
// updates (progress_parse.go) as Progress over the returned channel,
// which always finishes with exactly one final Progress carrying either
// a nil or non-nil Err (doc 02 §6). accept classifies the finished run —
// what counts as success differs by operation (a completed scrub that
// found data errors is not a failure; a sync that didn't fully write is).
//
// The line-draining goroutine below never stops early on ctx
// cancellation: Process.Lines must be drained to its own close before
// Wait is safe to call (Process's own doc comment), so a cancelled run
// still finishes draining in the background — cancellation only stops
// forwarding further ticks to the caller.
func (e *SnapraidEngine) runStream(ctx context.Context, logPath string, tail []string, accept func(RunSummary, error) error) (<-chan Progress, error) {
	proc, err := e.runner().Start(ctx, e.binary(), e.argv(logPath, tail)...)
	if err != nil {
		return nil, err
	}

	ch := make(chan Progress)
	go func() {
		defer close(ch)

		cancelled := false
		for line := range proc.Lines() {
			p, ok := parseProgressLine(line)
			if !ok {
				p = Progress{Output: line}
			}
			if cancelled {
				continue
			}
			select {
			case ch <- p:
			case <-ctx.Done():
				cancelled = true
			}
		}

		waitErr := proc.Wait()
		data, readErr := os.ReadFile(logPath)
		var finalErr error
		if readErr != nil {
			finalErr = fmt.Errorf("parity: reading run log: %w", readErr)
		} else {
			summary, parseErr := ParseRunSummary(data)
			if parseErr != nil {
				finalErr = parseErr
			} else {
				finalErr = accept(summary, waitErr)
			}
		}
		if finalErr == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				finalErr = ctxErr
			}
		}

		final := Progress{Percent: 100, Err: finalErr}
		if cancelled {
			select {
			case ch <- final:
			default:
			}
			return
		}
		ch <- final
	}()

	return ch, nil
}

// Sync runs `snapraid touch` first when Q17 calls for it, then a real
// sync (doc 02 §2). opts.Force maps to `-E` (syncArgv's own doc
// comment); opts.DryRun runs `diff` instead of `sync` — diff never
// writes parity by construction, so it satisfies "the diff SnapRAID
// would sync against without writing parity" (SyncOpts's own doc
// comment) exactly, rather than needing a separate no-op mode.
func (e *SnapraidEngine) Sync(ctx context.Context, opts SyncOpts) (<-chan Progress, error) {
	if opts.DryRun {
		logPath, cleanup, err := e.newLog("sync-dryrun")
		if err != nil {
			return nil, err
		}
		return e.runStream(ctx, logPath, diffArgv(), func(s RunSummary, waitErr error) error {
			defer cleanup()
			if s.Exit == "ok" || s.Exit == "diff" {
				return nil
			}
			return fmt.Errorf("parity: snapraid diff (dry-run sync): unexpected exit %q: %w", s.Exit, waitErr)
		})
	}

	if err := e.touchIfNeeded(ctx); err != nil {
		return nil, err
	}

	logPath, cleanup, err := e.newLog("sync")
	if err != nil {
		return nil, err
	}
	return e.runStream(ctx, logPath, syncArgv(opts.Force), func(s RunSummary, waitErr error) error {
		defer cleanup()
		if s.Exit == "ok" && waitErr == nil {
			return nil
		}
		return fmt.Errorf("parity: snapraid sync: exit %q: %w", s.Exit, waitErr)
	})
}

// Scrub runs a real scrub (doc 02 §2). Finding data errors (SnapRAID's
// own exit code 1, `summary:exit:error`) is scrub's whole purpose and a
// normal completion, not a failure — only a process that didn't run to
// completion at all is.
func (e *SnapraidEngine) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan Progress, error) {
	logPath, cleanup, err := e.newLog("scrub")
	if err != nil {
		return nil, err
	}
	return e.runStream(ctx, logPath, scrubArgv(pct, olderThanDays), func(s RunSummary, waitErr error) error {
		defer cleanup()
		if completedNormally(waitErr) {
			return nil
		}
		return fmt.Errorf("parity: snapraid scrub: exit %q: %w", s.Exit, waitErr)
	})
}

// Fix runs a real fix (doc 02 §2, §4). Reporting some unrecoverable
// blocks is fix's own honest result (doc 02 §4's "reconstruction can only
// restore data present at the last successful sync"), not a failure of
// the fix run itself — RunSummary.Unrecoverable carries that count for
// the caller. Only a process that didn't run to completion is an error
// here.
func (e *SnapraidEngine) Fix(ctx context.Context, opts FixOpts) (<-chan Progress, error) {
	logPath, cleanup, err := e.newLog("fix")
	if err != nil {
		return nil, err
	}
	return e.runStream(ctx, logPath, fixArgv(opts), func(s RunSummary, waitErr error) error {
		defer cleanup()
		if completedNormally(waitErr) {
			return nil
		}
		return fmt.Errorf("parity: snapraid fix: exit %q: %w", s.Exit, waitErr)
	})
}

// Check runs a real check (doc 02 §2). Like Scrub, finding a mismatch is
// check's own purpose, not a failure of the run.
func (e *SnapraidEngine) Check(ctx context.Context, opts CheckOpts) (<-chan Progress, error) {
	logPath, cleanup, err := e.newLog("check")
	if err != nil {
		return nil, err
	}
	return e.runStream(ctx, logPath, checkArgv(opts), func(s RunSummary, waitErr error) error {
		defer cleanup()
		if completedNormally(waitErr) {
			return nil
		}
		return fmt.Errorf("parity: snapraid check: exit %q: %w", s.Exit, waitErr)
	})
}

// completedNormally reports whether waitErr is SnapRAID's own "ran to
// completion" outcome: a clean exit (0), or exit 1 — SnapRAID's own code
// for "found something to report", confirmed for scrub (data errors),
// fix (unrecoverable blocks) and check (mismatches) against a real
// snapraid 12.4-1 binary. Anything else (killed by cancellation, or an
// exit code SnapRAID itself never documents) is a genuine failure.
func completedNormally(waitErr error) bool {
	if waitErr == nil {
		return true
	}
	code, ok := exitCode(waitErr)
	return ok && code == 1
}

var _ Engine = (*SnapraidEngine)(nil)
