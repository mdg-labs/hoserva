package parity

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
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

// TestDeliverFinalAfterCancel_DiscardsAStaleTickToMakeRoom is this issue's
// own reproduction of the dropped-terminal-status bug: runStream's
// cancelled path once sent the final Progress with a non-blocking
// `select { default: }`, so it was silently discarded whenever ch's one
// buffered slot already held a tick nobody had received yet. This test
// primes ch with exactly that stale, unconsumed tick before calling
// deliverFinalAfterCancel — deterministic, no goroutine timing involved —
// and would hang forever (caught by the timeout below) were the fix to
// block on a full buffer instead of draining it first.
func TestDeliverFinalAfterCancel_DiscardsAStaleTickToMakeRoom(t *testing.T) {
	ch := make(chan Progress, 1)
	ch <- Progress{Percent: 42, Output: "a tick nobody received"}

	final := Progress{Percent: 100, Err: context.Canceled}
	done := make(chan struct{})
	go func() {
		deliverFinalAfterCancel(ch, final)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deliverFinalAfterCancel blocked forever with a stale tick occupying the buffer")
	}

	got, ok := <-ch
	if !ok {
		t.Fatal("deliverFinalAfterCancel: channel closed with no final Progress — the terminal status was dropped")
	}
	if !errors.Is(got.Err, context.Canceled) {
		t.Fatalf("got %+v, want the final status (Err = context.Canceled), not the stale tick", got)
	}
	// deliverFinalAfterCancel never closes ch (runStream's own goroutine
	// does that); a non-blocking check is the only correct way to confirm
	// nothing else is buffered.
	select {
	case v := <-ch:
		t.Fatalf("got a second value %+v, want only the final status", v)
	default:
	}
}

// TestDeliverFinalAfterCancel_SucceedsImmediatelyWhenBufferIsEmpty covers
// the ordinary case: no stale tick to drain, the final status still goes
// through.
func TestDeliverFinalAfterCancel_SucceedsImmediatelyWhenBufferIsEmpty(t *testing.T) {
	ch := make(chan Progress, 1)
	deliverFinalAfterCancel(ch, Progress{Percent: 100, Err: context.Canceled})

	got, ok := <-ch
	if !ok {
		t.Fatal("deliverFinalAfterCancel: channel closed with no final Progress")
	}
	if !errors.Is(got.Err, context.Canceled) {
		t.Fatalf("got %+v, want Err = context.Canceled", got)
	}
}

// TestSnapraidEngine_RunStream_KilledProcessErrorWrapsCtxErr is #379's own
// engine-level proof for scheduler.go's identity classification
// (isCancellationDerived): a process this ctx's own cancellation killed
// reports a real, non-nil exit error through accept — Scrub's own
// completedNormally check rejects a bare "signal: killed" the same way a
// real killed snapraid process's waitErr would — so runStream must still
// fold ctx.Err() into that error rather than only substituting it when
// accept returned nil, so errors.Is(finalErr, context.Canceled) stays
// true and runJob's own classification recognizes this as the RunFunc's
// own reaction to the cancellation it was asked to stop for, not a
// newly-surfaced genuine failure a raced Cancel must not erase.
func TestSnapraidEngine_RunStream_KilledProcessErrorWrapsCtxErr(t *testing.T) {
	dir := t.TempDir()
	killErr := errors.New("signal: killed")
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_scrub_data_errors.log")), err: killErr},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch, err := e.Scrub(ctx, 100, 0)
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	final := drain(t, ch)
	if final.Err == nil {
		t.Fatal("Scrub final Progress.Err = nil, want the killed process's own error wrapped with ctx.Err()")
	}
	if !errors.Is(final.Err, context.Canceled) {
		t.Fatalf("Scrub final Progress.Err = %v, want errors.Is(..., context.Canceled) so a raced Cancel classifies this as cancellation-derived", final.Err)
	}
	if !strings.Contains(final.Err.Error(), killErr.Error()) {
		t.Fatalf("Scrub final Progress.Err = %q, want it to still name the killed process's own error, not just context.Canceled", final.Err.Error())
	}
}

func drain(t *testing.T, ch <-chan Progress) Progress {
	t.Helper()
	var last Progress
	for p := range ch {
		last = p
	}
	return last
}

// noChangeDiffLog is a minimal, hand-written `snapraid diff -l <log>`
// body reporting no changes at all — every Sync call now runs the
// threshold guard on a fresh Diff first (this issue), so every test below
// that expects Sync to reach touch/sync needs a scripted diff step ahead
// of it. A real "nothing changed" diff always exits 0, never SnapRAID's
// own exit-2 ("there are differences"), so scriptedResult below leaves
// err nil to match.
const noChangeDiffLog = `data:d1:/lab/28-a1/mnt/disk1/
data:d2:/lab/28-a1/mnt/disk2/
data:d3:/lab/28-a1/mnt/disk3/
summary:equal:6
summary:added:0
summary:removed:0
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:ok
`

func TestSnapraidEngine_Sync_SkipsTouchWhenNotNeeded(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // status (guard's "before", and touch check)
		{logBody: noChangeDiffLog},                                    // diff (guard evaluation)
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
	if len(r.calls) != 3 {
		t.Fatalf("got %d snapraid calls, want 3 (status, diff, sync — one status shared between the guard's before-count and the touch check): %v", len(r.calls), r.calls)
	}
	if got := r.calls[2][len(r.calls[2])-1]; got != "sync" {
		t.Fatalf("third call's own operation = %q, want %q (no touch call in between)", got, "sync")
	}
}

func TestSnapraidEngine_Sync_RunsTouchWhenZeroSubsecondFilesExist(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_zerosubsecond.log"))}, // status (guard's "before", and touch check): 1 zero-subsecond file
		{logBody: noChangeDiffLog},                               // diff (guard evaluation)
		{logBody: string(readCorpus(t, "snapraid_touch.log"))},   // touch
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))}, // sync
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
	if len(r.calls) != 4 {
		t.Fatalf("got %d snapraid calls, want 4 (status, diff, touch, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[2][len(r.calls[2])-1]; got != "touch" {
		t.Fatalf("third call = %v, want its own tail to be touch (Q17)", r.calls[2])
	}
	if got := r.calls[3][len(r.calls[3])-1]; got != "sync" {
		t.Fatalf("fourth call = %v, want its own tail to be sync", r.calls[3])
	}
}

// TestSnapraidEngine_Sync_DryRunSucceedsWhenAlreadySynced is #389's own
// reproduction for the dry-run path: a real, unedited `snapraid diff -l`
// capture against an already-synced array (testdata/parsers/
// snapraid_diff_idle.log, captured on a real Debian 13 guest with real
// UUID support) reports `summary:exit:equal`, exactly like sync's own
// no-op case (#267) — but DryRun's own accept closure only recognized
// "ok" or "diff", so a dry-run request against an array with nothing to
// sync fell into its "unexpected exit" failure branch.
func TestSnapraidEngine_Sync_DryRunSucceedsWhenAlreadySynced(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_diff_idle.log"))},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{DryRun: true})
	if err != nil {
		t.Fatalf("Sync (dry-run): %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync (dry-run, already synced) final Progress.Err = %v, want nil", final.Err)
	}
}

func TestSnapraidEngine_Sync_FailsWhenProcessDidNotRun(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("boom")
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
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

// TestSnapraidEngine_Sync_SucceedsWhenNothingToSync is #267's own
// reproduction: a second sync run back-to-back with no changes in between
// gets `summary:exit:equal` from a real snapraid binary (a legitimate,
// undocumented outcome distinct from "ok") while the process itself still
// exits 0 (waitErr == nil). Before the fix, that combination fell into
// Sync's error branch and rendered a malformed `%!w(<nil>)` instead of
// succeeding.
func TestSnapraidEngine_Sync_SucceedsWhenNothingToSync(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // status (first sync)
		{logBody: noChangeDiffLog},                                    // diff (first sync)
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},      // sync (first sync)
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // status (second sync)
		{logBody: noChangeDiffLog},                                    // diff (second sync)
		{logBody: string(readCorpus(t, "snapraid_sync_equal.log"))},   // sync (second sync): summary:exit:equal
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if final := drain(t, ch); final.Err != nil {
		t.Fatalf("first Sync final Progress.Err = %v, want nil", final.Err)
	}

	ch, err = e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("second Sync (nothing to sync) final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 6 {
		t.Fatalf("got %d snapraid calls, want 6 (status, diff, sync twice over): %v", len(r.calls), r.calls)
	}
}

// TestSnapraidEngine_Sync_FailsLoudlyOnUnrecognizedExitWithNilWaitErr
// guards #267's general fix: an Exit value Sync doesn't recognize as
// success must still fail — never silently succeed — and its error must
// never render the malformed `%!w(<nil>)` %w-on-nil formatting produced
// before the fix.
func TestSnapraidEngine_Sync_FailsLoudlyOnUnrecognizedExitWithNilWaitErr(t *testing.T) {
	dir := t.TempDir()
	unrecognizedExitLog := string(readCorpus(t, "snapraid_sync_equal.log"))
	unrecognizedExitLog = replaceExit(unrecognizedExitLog, "bogus")
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
		{logBody: unrecognizedExitLog},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err == nil {
		t.Fatal("Sync final Progress.Err = nil, want a failure for an unrecognized exit value")
	}
	if got := final.Err.Error(); strings.Contains(got, "%!w") {
		t.Fatalf("Sync final Progress.Err = %q, contains malformed %%w-on-nil formatting", got)
	}
}

func replaceExit(log, exit string) string {
	return strings.Replace(log, "summary:exit:equal", "summary:exit:"+exit, 1)
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

// TestSnapraidEngine_Sync_BlocksOnZeroFilesTrigger is this issue's own
// central reproduction, at the engine level: disk3 dropping from 1 file
// to 0 (snapraid_diff_mixed.log, the exact scenario doc 06 §3's lab test
// drives for real by unmounting a disk) must stop Sync before it ever
// invokes `snapraid sync` — proving the guard sits structurally in front
// of it, not as an optional check a caller could have skipped.
func TestSnapraidEngine_Sync_BlocksOnZeroFilesTrigger(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_new_array.log"))},
		{logBody: string(readCorpus(t, "snapraid_diff_mixed.log")), err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err == nil {
		t.Fatal("Sync: got nil error, want a *GuardBlockedError")
	}
	if ch != nil {
		t.Fatal("Sync: got a non-nil channel for a blocked sync")
	}
	if !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("Sync error %v does not match ErrGuardBlocked", err)
	}
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync error %v is not a *GuardBlockedError", err)
	}
	if !blocked.Result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("GuardBlockedError.Result.Triggers = %v, want TriggerZeroFiles", blocked.Result.Triggers)
	}
	if len(r.calls) != 2 {
		t.Fatalf("got %d snapraid calls, want exactly 2 (status, diff) — sync must never run: %v", len(r.calls), r.calls)
	}
	for _, call := range r.calls {
		if call[len(call)-1] == "sync" {
			t.Fatalf("a blocked Sync still invoked snapraid sync: %v", r.calls)
		}
	}
}

// TestSnapraidEngine_Sync_ConfirmedZeroFilesProceedsWithForceEmpty is the
// human-decision half of the same scenario: once a caller has reviewed
// the blocked result and sets opts.Confirm (doc 02 §2's "sync anyway"),
// Sync proceeds and passes SnapRAID's own `-E` for the disk the guard
// found emptied.
func TestSnapraidEngine_Sync_ConfirmedZeroFilesProceedsWithForceEmpty(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_new_array.log"))}, // status (guard's "before", and touch check)
		{logBody: string(readCorpus(t, "snapraid_diff_mixed.log")), err: &fakeExitError{code: 2}},
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{Confirm: true})
	if err != nil {
		t.Fatalf("Sync with Confirm: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d snapraid calls, want 3: %v", len(r.calls), r.calls)
	}
	last := r.calls[2]
	if last[len(last)-1] != "sync" {
		t.Fatalf("last call = %v, want its own tail to be sync", last)
	}
	found := false
	for _, a := range last {
		if a == "-E" {
			found = true
		}
	}
	if !found {
		t.Fatalf("confirmed sync call %v does not carry -E for the diff's own emptied disk", last)
	}
}

// removedCountDiffLog and its matching "before" status isolate the
// removed-count trigger: 501 removed files (over the default max of 500)
// against a huge before-count so the removed+updated percentage stays
// far under the default 10% — only TriggerRemovedCount should fire.
const removedCountStatusLog = `data:d1:/lab/guard/mnt/disk1/
summary:disk_file_count:d1:100000
`
const removedCountDiffLog = `data:d1:/lab/guard/mnt/disk1/
summary:equal:0
summary:added:0
summary:removed:501
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

func TestSnapraidEngine_Sync_BlocksOnRemovedCountTrigger(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: removedCountStatusLog},
		{logBody: removedCountDiffLog, err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	_, err := e.Sync(context.Background(), SyncOpts{})
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync error %v is not a *GuardBlockedError", err)
	}
	if !blocked.Result.hasTrigger(TriggerRemovedCount) {
		t.Fatalf("Triggers = %v, want TriggerRemovedCount", blocked.Result.Triggers)
	}
	if blocked.Result.hasTrigger(TriggerRemovedUpdatedPercent) {
		t.Fatalf("Triggers = %v, want TriggerRemovedUpdatedPercent NOT to fire (isolating the count trigger)", blocked.Result.Triggers)
	}
	if len(r.calls) != 2 {
		t.Fatalf("got %d snapraid calls, want exactly 2 — sync must never run: %v", len(r.calls), r.calls)
	}
}

// removedUpdatedPercentDiffLog isolates the percent trigger: 150 removed
// against a before-count of 1000 (15%, over the default 10%), while
// staying under the default 500-file removed-count threshold.
const removedUpdatedPercentStatusLog = `data:d1:/lab/guard/mnt/disk1/
summary:disk_file_count:d1:1000
`
const removedUpdatedPercentDiffLog = `data:d1:/lab/guard/mnt/disk1/
summary:equal:0
summary:added:0
summary:removed:150
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

func TestSnapraidEngine_Sync_BlocksOnRemovedUpdatedPercentTrigger(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: removedUpdatedPercentStatusLog},
		{logBody: removedUpdatedPercentDiffLog, err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r}

	_, err := e.Sync(context.Background(), SyncOpts{})
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync error %v is not a *GuardBlockedError", err)
	}
	if !blocked.Result.hasTrigger(TriggerRemovedUpdatedPercent) {
		t.Fatalf("Triggers = %v, want TriggerRemovedUpdatedPercent", blocked.Result.Triggers)
	}
	if blocked.Result.hasTrigger(TriggerRemovedCount) {
		t.Fatalf("Triggers = %v, want TriggerRemovedCount NOT to fire (isolating the percent trigger)", blocked.Result.Triggers)
	}
	if len(r.calls) != 2 {
		t.Fatalf("got %d snapraid calls, want exactly 2 — sync must never run: %v", len(r.calls), r.calls)
	}
}

// manifestDiffLog: 6 files removed from d1, 3 of them reappearing added
// on d2 — the shape a relocation's own manifest (Q15) needs to match
// against, exercised here through the real diff parser rather than a
// hand-built DiffReport (guard_test.go already covers that directly).
const manifestStatusLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
summary:disk_file_count:d1:1000
summary:disk_file_count:d2:1000
`
const manifestDiffLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
scan:remove:d1:movies/f1.bin
scan:remove:d1:movies/f2.bin
scan:remove:d1:movies/f3.bin
scan:remove:d1:movies/f4.bin
scan:remove:d1:movies/f5.bin
scan:remove:d1:movies/f6.bin
scan:add:d2:movies/f1.bin
scan:add:d2:movies/f2.bin
scan:add:d2:movies/f3.bin
summary:equal:0
summary:added:3
summary:removed:6
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

func manifestEntries(relPaths ...string) []ManifestEntry {
	entries := make([]ManifestEntry, len(relPaths))
	for i, p := range relPaths {
		entries[i] = ManifestEntry{RelPath: p, SourceDisk: "/lab/guard/mnt/disk1", TargetDisk: "/lab/guard/mnt/disk2"}
	}
	return entries
}

// TestSnapraidEngine_Sync_UnaccountedRemovalsStillBlock confirms that,
// without a manifest, all 6 removed files count fully against the lowered
// threshold and Sync blocks — the baseline TestSnapraidEngine_Sync_
// AccountedRemovalsAllowSync below is contrasted against.
func TestSnapraidEngine_Sync_UnaccountedRemovalsStillBlock(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: manifestStatusLog},
		{logBody: manifestDiffLog, err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r, Guard: Guard{Config: GuardConfig{RemovedFilesMax: 3}}}

	_, err := e.Sync(context.Background(), SyncOpts{})
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync error %v is not a *GuardBlockedError", err)
	}
	if blocked.Result.RemovedCount != 6 {
		t.Fatalf("RemovedCount = %d, want 6 (no manifest supplied)", blocked.Result.RemovedCount)
	}
}

// TestSnapraidEngine_Sync_AccountedRemovalsAllowSync is Q15's own worked
// example at the engine level: the same diff as above, but with a
// manifest accounting for 3 of the 6 removals as "moved by Hoserva" —
// leaving 3 unaccounted, at (not over) the lowered threshold, so Sync
// proceeds.
func TestSnapraidEngine_Sync_AccountedRemovalsAllowSync(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: manifestStatusLog}, // status (guard's "before", and touch check)
		{logBody: manifestDiffLog, err: &fakeExitError{code: 2}},
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r, Guard: Guard{Config: GuardConfig{RemovedFilesMax: 3}}}

	ch, err := e.Sync(context.Background(), SyncOpts{
		Manifest: manifestEntries("movies/f1.bin", "movies/f2.bin", "movies/f3.bin"),
	})
	if err != nil {
		t.Fatalf("Sync with an accounting manifest: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d snapraid calls, want 3: %v", len(r.calls), r.calls)
	}
}

// trailingStatusLog/trailingDiffLog represent a Q14 two-phase relocation's
// *trailing* sync (#248): three files removed from d1, none of them
// showing as added on d2 — that addition was already committed by an
// earlier sync's own diff, not this one, so matchManifest's same-diff
// check alone cannot account for them.
const trailingStatusLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
summary:disk_file_count:d1:1000
summary:disk_file_count:d2:1003
`
const trailingDiffLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
scan:remove:d1:movies/f1.bin
scan:remove:d1:movies/f2.bin
scan:remove:d1:movies/f3.bin
summary:equal:0
summary:added:0
summary:removed:3
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

// trailingListLogAllTracked is the `snapraid list` a trailing sync's own
// confirmManifestTargets call reads: f1-f3 already tracked on d2, proving
// the earlier sync really did commit their addition.
const trailingListLogAllTracked = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
file:d2:movies/f1.bin:100:1000000000:1:1
file:d2:movies/f2.bin:100:1000000000:1:1
file:d2:movies/f3.bin:100:1000000000:1:1
summary:file_count:3
summary:exit:ok
`

// trailingListLogNoneTracked is the same shape, but d2 never actually
// received any of the files — the "stale/incorrect manifest" case: a
// relocation that claims to have moved files that were never really
// copied and synced.
const trailingListLogNoneTracked = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
summary:file_count:0
summary:exit:ok
`

// TestSnapraidEngine_Sync_TrailingSyncAccountsConfirmedTarget is this
// issue's (#248) own central lab-adjacent reproduction at the engine
// level: a trailing sync whose diff shows only the removal still proceeds
// once a real `snapraid list` confirms the files are already on their
// target disk — RemovedFilesMax is set low enough that all 3 removals
// would block without the exemption.
func TestSnapraidEngine_Sync_TrailingSyncAccountsConfirmedTarget(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: trailingStatusLog},                             // status (guard's "before")
		{logBody: trailingDiffLog, err: &fakeExitError{code: 2}}, // diff (guard evaluation)
		{logBody: trailingListLogAllTracked},                     // list (confirmManifestTargets)
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))}, // sync
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r, Guard: Guard{Config: GuardConfig{RemovedFilesMax: 2}}}

	ch, err := e.Sync(context.Background(), SyncOpts{
		Manifest: manifestEntries("movies/f1.bin", "movies/f2.bin", "movies/f3.bin"),
	})
	if err != nil {
		t.Fatalf("Sync for a trailing two-phase sync: %v", err)
	}
	final := drain(t, ch)
	if final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
	if len(r.calls) != 4 {
		t.Fatalf("got %d snapraid calls, want 4 (status, diff, list, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[2][len(r.calls[2])-1]; got != "list" {
		t.Fatalf("third call = %v, want its last arg to be \"list\"", r.calls[2])
	}
}

// TestSnapraidEngine_Sync_UnconfirmedTrailingRemovalStillBlocks is the
// other half of this issue's (#248) own acceptance criteria: the same
// trailing-sync diff shape, but `snapraid list` shows the files were
// never actually placed on their claimed target disk — a stale or
// incorrect manifest entry — so the guard must still block exactly as it
// would with no manifest at all.
func TestSnapraidEngine_Sync_UnconfirmedTrailingRemovalStillBlocks(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: trailingStatusLog},
		{logBody: trailingDiffLog, err: &fakeExitError{code: 2}},
		{logBody: trailingListLogNoneTracked},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: dir, Runner: r, Guard: Guard{Config: GuardConfig{RemovedFilesMax: 2}}}

	_, err := e.Sync(context.Background(), SyncOpts{
		Manifest: manifestEntries("movies/f1.bin", "movies/f2.bin", "movies/f3.bin"),
	})
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync error %v is not a *GuardBlockedError", err)
	}
	if blocked.Result.RemovedCount != 3 {
		t.Fatalf("RemovedCount = %d, want 3 — a manifest entry whose target was never actually confirmed must not be accounted", blocked.Result.RemovedCount)
	}
	if len(r.calls) != 3 {
		t.Fatalf("got %d snapraid calls, want exactly 3 — sync must never run: %v", len(r.calls), r.calls)
	}
}

// TestConfirmManifestTargets_AgreesWithSyncGuardVerdict is #252's own
// acceptance criterion: the diff-preview path (Diff, then
// ConfirmManifestTargets, then Guard.Evaluate directly — exactly what
// RunParityDiff, internal/api/parity_handler.go, does) must reach the same
// Blocked verdict Sync itself reaches for the identical manifest/diff/list
// state, both when a trailing sync's removal is confirmed on its target
// disk and when it is not.
func TestConfirmManifestTargets_AgreesWithSyncGuardVerdict(t *testing.T) {
	cases := []struct {
		name        string
		listLog     string
		wantBlocked bool
	}{
		{"targetConfirmed", trailingListLogAllTracked, false},
		{"targetUnconfirmed", trailingListLogNoneTracked, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guard := Guard{Config: GuardConfig{RemovedFilesMax: 2}}
			manifest := manifestEntries("movies/f1.bin", "movies/f2.bin", "movies/f3.bin")

			previewDir := t.TempDir()
			previewRunner := &scriptedRunner{t: t, script: []scriptedResult{
				{logBody: trailingStatusLog},
				{logBody: trailingDiffLog, err: &fakeExitError{code: 2}},
				{logBody: tc.listLog},
			}}
			previewEngine := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: previewDir, Runner: previewRunner, Guard: guard}
			diff, err := previewEngine.Diff(context.Background())
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			confirmed, err := ConfirmManifestTargets(context.Background(), previewEngine, diff, manifest)
			if err != nil {
				t.Fatalf("ConfirmManifestTargets: %v", err)
			}
			previewResult := guard.Evaluate(diff, confirmed, nil)

			syncDir := t.TempDir()
			syncRunner := &scriptedRunner{t: t, script: []scriptedResult{
				{logBody: trailingStatusLog},
				{logBody: trailingDiffLog, err: &fakeExitError{code: 2}},
				{logBody: tc.listLog},
				{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
			}}
			syncEngine := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: syncDir, Runner: syncRunner, Guard: guard}
			ch, syncErr := syncEngine.Sync(context.Background(), SyncOpts{Manifest: manifest})
			var blocked *GuardBlockedError
			syncBlocked := errors.As(syncErr, &blocked)
			if !syncBlocked {
				if syncErr != nil {
					t.Fatalf("Sync: %v", syncErr)
				}
				if final := drain(t, ch); final.Err != nil {
					t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
				}
			}

			if previewResult.Blocked != tc.wantBlocked {
				t.Fatalf("preview Blocked = %v, want %v", previewResult.Blocked, tc.wantBlocked)
			}
			if syncBlocked != tc.wantBlocked {
				t.Fatalf("Sync Blocked = %v, want %v", syncBlocked, tc.wantBlocked)
			}
			if previewResult.Blocked != syncBlocked {
				t.Fatalf("preview and Sync disagree on the same manifest/diff/list state: preview.Blocked=%v Sync.Blocked=%v", previewResult.Blocked, syncBlocked)
			}
		})
	}
}

// TestSnapraidEngine_CurrentRelocationManifest_UnwiredReturnsNil confirms
// a SnapraidEngine with no Relocation store (the zero value, exactly what
// every engine built before #194 has) reports no manifest — RunSync's own
// type-assertion still finds the method, and it must report "nothing to
// load" rather than panic on a nil Relocation.
func TestSnapraidEngine_CurrentRelocationManifest_UnwiredReturnsNil(t *testing.T) {
	e := &SnapraidEngine{}
	manifest, removingDisks, err := e.CurrentRelocationManifest(context.Background())
	if err != nil {
		t.Fatalf("CurrentRelocationManifest: %v", err)
	}
	if manifest != nil || removingDisks != nil {
		t.Fatalf("CurrentRelocationManifest = (%v, %v), want (nil, nil) with no Relocation store wired", manifest, removingDisks)
	}
}

// TestSnapraidEngine_CurrentRelocationManifest_ReadsWiredStore confirms
// CurrentRelocationManifest is a straight passthrough to Relocation.Current
// once a store is wired — the shape RunSync's own relocationManifestSource
// type-assertion depends on.
func TestSnapraidEngine_CurrentRelocationManifest_ReadsWiredStore(t *testing.T) {
	ctx := context.Background()
	relocationStore := NewRelocationManifestStore(newTestDB(t))
	entries := []ManifestEntry{{RelPath: "movies/a.mkv", SourceDisk: "/mnt/disk1", TargetDisk: "/mnt/disk2"}}
	if err := relocationStore.Replace(ctx, entries, map[string]bool{"/mnt/disk3": true}); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	e := &SnapraidEngine{Relocation: relocationStore}
	manifest, removingDisks, err := e.CurrentRelocationManifest(ctx)
	if err != nil {
		t.Fatalf("CurrentRelocationManifest: %v", err)
	}
	if len(manifest) != 1 || manifest[0].RelPath != "movies/a.mkv" {
		t.Fatalf("manifest = %+v, want the persisted entry", manifest)
	}
	if !removingDisks["/mnt/disk3"] {
		t.Fatalf("removingDisks = %+v, want /mnt/disk3", removingDisks)
	}
}
