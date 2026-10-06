package job

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

// fixRunner stands in for the snapraid binary under a real SnapraidEngine: it
// writes logBody to the path the -l flag names and exits with exitCode, so a
// fix job runs the engine's own reading of SnapRAID's log end to end.
type fixRunner struct {
	logBody  string
	exitCode int
}

func (r fixRunner) Start(_ context.Context, _ string, args ...string) (parity.Process, error) {
	for i, a := range args {
		if a == "-l" && i+1 < len(args) {
			if err := os.WriteFile(args[i+1], []byte(r.logBody), 0o644); err != nil {
				return nil, err
			}
		}
	}
	lines := make(chan string)
	close(lines)
	var err error
	if r.exitCode != 0 {
		err = exitCodeError(r.exitCode)
	}
	return fixProcess{lines: lines, err: err}, nil
}

type fixProcess struct {
	lines chan string
	err   error
}

func (p fixProcess) Lines() <-chan string { return p.lines }
func (p fixProcess) Wait() error          { return p.err }

type exitCodeError int

func (e exitCodeError) Error() string { return "exit status" }
func (e exitCodeError) ExitCode() int { return int(e) }

// A whole-array or one-disk fix log SnapRAID wrote after it could not rebuild
// one file: the shape captured from a real snapraid 12.4-1 in
// internal/parity's fixUnrecoverableLog, without -f.
const wholeFixUnrecoverableLog = `command:fix
argv:5:fix
msg:progress: Fixing...
unrecoverable:0:d3:pr/unrec.bin: Unrecoverable error at position 0
status:unrecoverable:d3:pr/unrec.bin
summary:error:2
summary:error_recovered:0
summary:error_unrecoverable:1
summary:exit:unrecoverable
`

const wholeFixRecoveredLog = `command:fix
argv:5:fix
data:d3:/nonexistent/disk3/
msg:progress: Fixing...
status:recovered:d3:backup/weekly.bin
summary:error:3
summary:error_recovered:3
summary:error_unrecoverable:0
summary:exit:recovered
`

func TestRunFix_UnrecoverableBlocksEndTheJobFailed(t *testing.T) {
	for name, params := range map[string]FixParams{
		"whole array": {Confirm: true},
		"one disk":    {Confirm: true, Disk: intPtr(3)},
	} {
		s := newTestScheduler(t)
		engine := &parity.SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: fixRunner{logBody: wholeFixUnrecoverableLog, exitCode: 1}}
		s.registry.Register(TypeFix, false, RunFix(engine))

		j, err := s.Submit(context.Background(), TypeFix, nil, mustJSON(t, params))
		if err != nil {
			t.Fatalf("%s: Submit: %v", name, err)
		}
		finished := await(t, s, j.ID)
		if finished.Status != StatusFailed {
			t.Fatalf("%s: status = %s, want failed: a fix that left unrecoverable blocks restored only part of the data", name, finished.Status)
		}
		for _, want := range []string{"1 unrecoverable block", "last sync", "pr/unrec.bin.unrecoverable on disk d3"} {
			if !strings.Contains(finished.ErrorMessage, want) {
				t.Errorf("%s: error message %q, want it to contain %q", name, finished.ErrorMessage, want)
			}
		}
	}
}

func TestRunFix_FullyRecoveredFixStillSucceeds(t *testing.T) {
	s := newTestScheduler(t)
	engine := &parity.SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: fixRunner{logBody: wholeFixRecoveredLog}}
	s.registry.Register(TypeFix, false, RunFix(engine))

	j, err := s.Submit(context.Background(), TypeFix, nil, mustJSON(t, FixParams{Confirm: true}))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if finished := await(t, s, j.ID); finished.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
}

func TestScrubAndCheck_ExitOneStaysANormalCompletion(t *testing.T) {
	log := strings.Replace(wholeFixUnrecoverableLog, "command:fix", "command:scrub", 1)
	engine := &parity.SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: fixRunner{logBody: log, exitCode: 1}}

	for name, run := range map[string]func() (<-chan parity.Progress, error){
		"scrub": func() (<-chan parity.Progress, error) { return engine.Scrub(context.Background(), 5, 10) },
		"check": func() (<-chan parity.Progress, error) { return engine.Check(context.Background(), parity.CheckOpts{}) },
	} {
		ch, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := drainProgress(ch, nil); err != nil {
			t.Fatalf("%s: exit 1 (data errors found) is the run's normal completion, got %v", name, err)
		}
	}
}
