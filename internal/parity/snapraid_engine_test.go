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
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // status (diff's own "before")
		{logBody: noChangeDiffLog},                                    // diff (guard evaluation)
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
	if len(r.calls) != 4 {
		t.Fatalf("got %d snapraid calls, want 4 (status, diff, status, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[3][len(r.calls[3])-1]; got != "sync" {
		t.Fatalf("fourth call's own operation = %q, want %q (no touch call in between)", got, "sync")
	}
}

func TestSnapraidEngine_Sync_RunsTouchWhenZeroSubsecondFilesExist(t *testing.T) {
	dir := t.TempDir()
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},         // status (diff's own "before")
		{logBody: noChangeDiffLog},                                            // diff (guard evaluation)
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
	if len(r.calls) != 5 {
		t.Fatalf("got %d snapraid calls, want 5 (status, diff, status, touch, sync): %v", len(r.calls), r.calls)
	}
	if got := r.calls[3][len(r.calls[3])-1]; got != "touch" {
		t.Fatalf("fourth call = %v, want its own tail to be touch (Q17)", r.calls[3])
	}
	if got := r.calls[4][len(r.calls[4])-1]; got != "sync" {
		t.Fatalf("fifth call = %v, want its own tail to be sync", r.calls[4])
	}
}

func TestSnapraidEngine_Sync_FailsWhenProcessDidNotRun(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("boom")
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
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
		{logBody: string(readCorpus(t, "snapraid_status_new_array.log"))},
		{logBody: string(readCorpus(t, "snapraid_diff_mixed.log")), err: &fakeExitError{code: 2}},
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // touch check
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
	if len(r.calls) != 4 {
		t.Fatalf("got %d snapraid calls, want 4: %v", len(r.calls), r.calls)
	}
	last := r.calls[3]
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
		{logBody: manifestStatusLog},
		{logBody: manifestDiffLog, err: &fakeExitError{code: 2}},
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))}, // touch check
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
	if len(r.calls) != 4 {
		t.Fatalf("got %d snapraid calls, want 4: %v", len(r.calls), r.calls)
	}
}
