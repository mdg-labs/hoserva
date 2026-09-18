package parity

import (
	"context"
	"errors"
	"os"
	"testing"
)

// scriptedRunner is a fake Runner (CLAUDE.md's scriptable-fake rule) that
// plays back one canned result per call, in order, and writes each
// result's own log body to whatever path the call's "-l" flag names —
// exactly what SnapraidEngine itself expects to read back. It lets
// SnapraidEngine's own wiring (which command runs when, in what order,
// with what flags, and how a result is classified) be tested without a
// real snapraid binary; the real binary's own output shapes are covered
// separately, against real corpus (status_parse_test.go, diff_parse_test.go,
// run_parse_test.go) and in the lab (snapraid_lab_test.go).
type scriptedRunner struct {
	t      *testing.T
	script []scriptedResult
	calls  [][]string
	i      int
}

type scriptedResult struct {
	logBody string
	err     error
}

func (r *scriptedRunner) Start(ctx context.Context, name string, args ...string) (Process, error) {
	r.t.Helper()
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.i >= len(r.script) {
		r.t.Fatalf("unexpected extra snapraid invocation: %v", args)
	}
	res := r.script[r.i]
	r.i++

	for i, a := range args {
		if a == "-l" && i+1 < len(args) && res.logBody != "" {
			if err := os.WriteFile(args[i+1], []byte(res.logBody), 0o644); err != nil {
				r.t.Fatalf("writing scripted log: %v", err)
			}
		}
	}

	ch := make(chan string)
	close(ch)
	return &fakeProcess{lines: ch, err: res.err}, nil
}

type fakeProcess struct {
	lines chan string
	err   error
}

func (p *fakeProcess) Lines() <-chan string { return p.lines }
func (p *fakeProcess) Wait() error          { return p.err }

// fakeExitError scripts a Wait error with a given exit code, without
// spawning a real process — SnapraidEngine only ever reads a wait
// error's ExitCode() (exitCoder, snapraid_engine.go), so this stands in
// for a real *exec.ExitError exactly as far as this package's own code
// looks at it.
type fakeExitError struct{ code int }

func (e *fakeExitError) Error() string { return "exit status" }
func (e *fakeExitError) ExitCode() int { return e.code }

func drain(t *testing.T, ch <-chan Progress) Progress {
	t.Helper()
	var last Progress
	for p := range ch {
		last = p
	}
	return last
}

func TestSnapraidEngine_Sync_SkipsTouchWhenNotNeeded(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // status (touch check)
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},      // sync
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("got %d snapraid calls, want 2 (status, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[1][len(r.calls[1])-1]; got != "sync" {
		t.Fatalf("second call's own operation = %q, want %q (no touch call in between)", got, "sync")
	}
}

func TestSnapraidEngine_Sync_RunsTouchWhenZeroSubsecondFilesExist(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_zerosubsecond.log"))}, // status: 1 zero-subsecond file
		{logBody: string(readCorpus(t, "snapraid_touch.log"))},                // touch
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},              // sync
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d snapraid calls, want 3 (status, touch, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[1][len(r.calls[1])-1]; got != "touch" {
		t.Fatalf("second call = %v, want its own tail to be touch (Q17)", r.calls[1])
	}
	if got := r.calls[2][len(r.calls[2])-1]; got != "sync" {
		t.Fatalf("third call = %v, want its own tail to be sync", r.calls[2])
	}
}

func TestSnapraidEngine_Sync_ForceMapsToForceEmptyFlag(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{Force: true})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	drain(t, ch)

	found := false
	for _, a := range r.calls[1] {
		if a == "-E" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sync call %v does not carry -E for SyncOpts.Force", r.calls[1])
	}
}

func TestSnapraidEngine_Sync_FailsWhenProcessDidNotRun(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("boom")
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log")), err: boom},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err == nil {
		t.Fatal("Sync final Progress.Err = nil, want a wrapped failure")
	}
}

func TestSnapraidEngine_Status(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_bad_blocks.log"))},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	got, err := e.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Freshness != FreshnessRed {
		t.Fatalf("Freshness = %v, want FreshnessRed", got.Freshness)
	}
	if got.DataDisks != 3 || got.ParityDisks != 1 {
		t.Fatalf("DataDisks/ParityDisks = %d/%d, want 3/1", got.DataDisks, got.ParityDisks)
	}
}

// TestSnapraidEngine_Diff_CombinesStatusAndDiff wires status (the
// "before" snapshot) and diff together the same way the real Diff does,
// checked against the same real corpus diff_parse_test.go already
// verifies BuildDiffReport against directly — this test is about
// SnapraidEngine calling both operations in the right order and handing
// the real DiffLog.exit-2 result through as success, not as an error.
func TestSnapraidEngine_Diff_CombinesStatusAndDiff(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_new_array.log"))},
		{logBody: string(readCorpus(t, "snapraid_diff_all_added.log")), err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	report, err := e.Diff(context.Background())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if report.Added != 3 {
		t.Fatalf("Added = %d, want 3", report.Added)
	}
	if len(r.calls) != 2 {
		t.Fatalf("got %d snapraid calls, want 2 (status, diff): %v", len(r.calls), r.calls)
	}
}
