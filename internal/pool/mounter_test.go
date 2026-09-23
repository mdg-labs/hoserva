package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// scriptedResponse is one queued reply for scriptedRunner.
type scriptedResponse struct {
	err error
}

// scriptedRunner is a disk.Runner that answers a given argv from an
// ordered, per-argv queue of scripted responses, consuming one entry per
// call — unlike disk.FakeRunner, which always returns the same response
// for a given argv. This is what a busy-then-success (or busy-then-a
// different-error) sequence needs: the same "fusermount -u <where>"
// call answered differently across successive retries.
type scriptedRunner struct {
	mu     sync.Mutex
	queues map[string][]scriptedResponse
	calls  []disk.RunCall
}

func newScriptedRunner() *scriptedRunner {
	return &scriptedRunner{queues: make(map[string][]scriptedResponse)}
}

func scriptedRunnerKey(name string, args []string) string {
	return strings.Join(append([]string{name}, args...), " ")
}

// Script appends err to the queue of responses this exact argv will
// return, in the order Script was called.
func (r *scriptedRunner) Script(name string, args []string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := scriptedRunnerKey(name, args)
	r.queues[key] = append(r.queues[key], scriptedResponse{err: err})
}

func (r *scriptedRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, disk.RunCall{Name: name, Args: append([]string(nil), args...)})
	key := scriptedRunnerKey(name, args)
	q := r.queues[key]
	if len(q) == 0 {
		return nil, fmt.Errorf("scriptedRunner: no more scripted responses for %q", key)
	}
	r.queues[key] = q[1:]
	return nil, q[0].err
}

func (r *scriptedRunner) Calls() []disk.RunCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]disk.RunCall, len(r.calls))
	copy(out, r.calls)
	return out
}

var _ disk.Runner = (*scriptedRunner)(nil)

// fakeUnmountClock is a deterministic Now/Sleep pair for Unmount's
// busy-retry loop: Sleep advances the clock by exactly d rather than
// waiting in real time, so a test can observe the retry window's bound
// without the test suite actually taking unmountRetryWindow to run.
type fakeUnmountClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeUnmountClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeUnmountClock) Sleep(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func testWhere(t *testing.T) string {
	return filepath.Join(t.TempDir(), "mnt", "user")
}

func TestMounter_Mount_CreatesWhere(t *testing.T) {
	where := testWhere(t)
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r := disk.NewFakeRunner()
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if _, err := os.Stat(where); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Where= %s should not exist before Mount", where)
	}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	info, err := os.Stat(where)
	if err != nil {
		t.Fatalf("after Mount, Stat(%s): %v", where, err)
	}
	if !info.IsDir() {
		t.Fatalf("after Mount, %s is not a directory", where)
	}
}

func TestMounter_Mount(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "mergerfs" {
		t.Fatalf("Mount: got calls %+v, want exactly one mergerfs call", calls)
	}
}

func TestMounter_Mount_PropagatesError(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	argv := m.Argv()
	wantErr := errors.New("mount failed")
	r.Script(argv[0], argv[1:], nil, wantErr)

	mounter := Mounter{Runner: r}
	if err := mounter.Mount(context.Background(), m); !errors.Is(err, wantErr) {
		t.Fatalf("Mount: got %v, want it to wrap %v", err, wantErr)
	}
}

func TestMounter_Unmount(t *testing.T) {
	r := disk.NewFakeRunner()
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Unmount(context.Background(), "/mnt/user"); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "fusermount" {
		t.Fatalf("Unmount: got calls %+v, want exactly one fusermount call", calls)
	}
}

// TestMounter_Unmount_RetriesBusyThenSucceeds is this issue's central
// case: after Samba/NFS stop, fusermount can briefly report "busy"
// while their just-closed file handles are released, and a bounded
// retry rides that out rather than failing array stop outright. The
// current, non-retrying Unmount fails this test (it returns the first
// busy error after a single call).
func TestMounter_Unmount_RetriesBusyThenSucceeds(t *testing.T) {
	where := "/mnt/user"
	r := newScriptedRunner()
	r.Script("fusermount", []string{"-u", where}, errors.New("fusermount: failed to unmount /mnt/user: Device or resource busy"))
	r.Script("fusermount", []string{"-u", where}, errors.New("fusermount: failed to unmount /mnt/user: Device or resource busy"))
	r.Script("fusermount", []string{"-u", where}, nil)

	clock := &fakeUnmountClock{now: time.Unix(0, 0)}
	mounter := Mounter{Runner: r, Now: clock.Now, Sleep: clock.Sleep}
	if err := mounter.Unmount(context.Background(), where); err != nil {
		t.Fatalf("Unmount: got %v, want nil after busy settles", err)
	}

	calls := r.Calls()
	if len(calls) != 3 {
		t.Fatalf("Unmount: got %d fusermount calls %+v, want 3 (two busy, then success)", len(calls), calls)
	}
}

// TestMounter_Unmount_FailsAfterBoundWhenBusyThroughout proves a
// genuinely stuck pool still fails array stop: Unmount must not retry
// forever, and must return the busy error once unmountRetryWindow
// elapses. The current, non-retrying Unmount fails this test (it makes
// exactly one call, never observing the retry-then-bound behavior this
// test asserts on).
func TestMounter_Unmount_FailsAfterBoundWhenBusyThroughout(t *testing.T) {
	where := "/mnt/user"
	r := newScriptedRunner()
	busyErr := errors.New("fusermount: failed to unmount /mnt/user: Device or resource busy")
	// More responses than the bounded loop can ever consume, so a bug
	// that fails to stop retrying is caught as "ran out of script"
	// rather than looping unboundedly.
	wantCalls := int(unmountRetryWindow/unmountRetryDelay) + 1
	for i := 0; i < wantCalls+5; i++ {
		r.Script("fusermount", []string{"-u", where}, busyErr)
	}

	clock := &fakeUnmountClock{now: time.Unix(0, 0)}
	// IsMountpoint: where is a literal path this test never creates, so
	// the production check (os.Stat) would itself see ENOENT and, since
	// #268, treat that as "already gone" — this test is about the busy-
	// retry bound, not that tolerance, so it fakes "still mounted".
	mounter := Mounter{Runner: r, Now: clock.Now, Sleep: clock.Sleep, IsMountpoint: func(string) (bool, error) { return true, nil }}
	err := mounter.Unmount(context.Background(), where)
	if !errors.Is(err, busyErr) {
		t.Fatalf("Unmount: got %v, want it to wrap the last busy error %v", err, busyErr)
	}

	calls := r.Calls()
	if len(calls) != wantCalls {
		t.Fatalf("Unmount: got %d calls, want exactly %d (unmountRetryWindow/unmountRetryDelay + 1 attempts, i.e. bounded)", len(calls), wantCalls)
	}
}

// TestMounter_Unmount_StopsRetryingOnNonBusyError proves a non-busy
// error is never retried, including mid-retry: a busy response followed
// by a distinct, non-busy error must stop immediately on the second
// call and return that second error, not keep retrying and not return
// the first (busy) error. The current, non-retrying Unmount fails this
// test — it returns the first (busy) error after one call, never
// reaching the second, distinct error this test asserts on.
func TestMounter_Unmount_StopsRetryingOnNonBusyError(t *testing.T) {
	where := "/mnt/user"
	r := newScriptedRunner()
	r.Script("fusermount", []string{"-u", where}, errors.New("fusermount: failed to unmount /mnt/user: Device or resource busy"))
	nonBusyErr := errors.New("fusermount: failed to unmount /mnt/user: Operation not permitted")
	r.Script("fusermount", []string{"-u", where}, nonBusyErr)

	clock := &fakeUnmountClock{now: time.Unix(0, 0)}
	// IsMountpoint: see TestMounter_Unmount_FailsAfterBoundWhenBusyThroughout
	// — where is never created, so the real check would treat it as
	// already gone rather than exercising the non-busy-error path.
	mounter := Mounter{Runner: r, Now: clock.Now, Sleep: clock.Sleep, IsMountpoint: func(string) (bool, error) { return true, nil }}
	err := mounter.Unmount(context.Background(), where)
	if !errors.Is(err, nonBusyErr) {
		t.Fatalf("Unmount: got %v, want it to wrap the non-busy error %v", err, nonBusyErr)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("Unmount: got %d calls %+v, want exactly 2 (one busy retry, then stop on the non-busy error)", len(calls), calls)
	}
}

// TestMounter_Unmount_DoesNotRetryOnErrorMentioningBusyPath proves the
// busy check looks only at fusermount's own strerror text, not at
// whether "busy" appears anywhere in the error — a share name may
// itself contain "busy" (internal/pool/share.go's shareNamePattern
// allows it), and CommandRunner's error text always includes the mount
// path, so a non-busy failure against a path like /mnt/user/busybox
// must still return on the first call. Against the pre-fix
// isBusyUnmountError (strings.Contains on the whole, lowercased error),
// this test fails: it retries for the full unmountRetryWindow instead
// of stopping after one call.
func TestMounter_Unmount_DoesNotRetryOnErrorMentioningBusyPath(t *testing.T) {
	where := "/mnt/user/busybox"
	r := newScriptedRunner()
	nonBusyErr := errors.New("fusermount: failed to unmount /mnt/user/busybox: Invalid argument")
	r.Script("fusermount", []string{"-u", where}, nonBusyErr)

	clock := &fakeUnmountClock{now: time.Unix(0, 0)}
	// IsMountpoint: see TestMounter_Unmount_FailsAfterBoundWhenBusyThroughout
	// — where is never created, so the real check would treat it as
	// already gone rather than exercising the busy-path-name path.
	mounter := Mounter{Runner: r, Now: clock.Now, Sleep: clock.Sleep, IsMountpoint: func(string) (bool, error) { return true, nil }}
	err := mounter.Unmount(context.Background(), where)
	if !errors.Is(err, nonBusyErr) {
		t.Fatalf("Unmount: got %v, want it to wrap %v", err, nonBusyErr)
	}

	calls := r.Calls()
	if len(calls) != 1 {
		t.Fatalf("Unmount: got %d fusermount calls %+v, want exactly 1 (a non-busy error against a busy-looking path must not retry)", len(calls), calls)
	}
}

// TestMounter_Mount_NoOpWhenAlreadyMountedWithMatchingFSName is #268's
// central start-side case: a Start called against a path that already
// has mnt's own mount up (a retried apply, or Start run without an
// intervening Stop) must not stack a second mergerfs on top of the
// first. Against the pre-fix Mount, which never checks, this test fails
// — it would issue a second mergerfs call.
func TestMounter_Mount_NoOpWhenAlreadyMountedWithMatchingFSName(t *testing.T) {
	where := testWhere(t)
	if err := os.MkdirAll(where, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	r := disk.NewFakeRunner()
	r.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte("hoserva-pool\n"), nil)

	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	mounter := Mounter{Runner: r, IsMountpoint: func(string) (bool, error) { return true, nil }}
	if err := mounter.Mount(context.Background(), m); err != nil {
		t.Fatalf("Mount: got %v, want nil (already mounted with matching fsname)", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "findmnt" {
		t.Fatalf("Mount: got calls %+v, want exactly one findmnt call and no mergerfs call", calls)
	}
}

// TestMounter_Mount_FailsWhenAlreadyMountedByDifferentFSName proves Mount
// refuses, rather than silently accepting, a path that is already
// mounted by something other than mnt itself — a wrong mount
// masquerading as "already there" would be worse than a start that
// refuses loudly.
func TestMounter_Mount_FailsWhenAlreadyMountedByDifferentFSName(t *testing.T) {
	where := testWhere(t)
	if err := os.MkdirAll(where, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	r := disk.NewFakeRunner()
	r.Script("findmnt", []string{"-n", "-o", "SOURCE", where}, []byte("hoserva-appdata\n"), nil)

	m := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	mounter := Mounter{Runner: r, IsMountpoint: func(string) (bool, error) { return true, nil }}
	err := mounter.Mount(context.Background(), m)
	if err == nil {
		t.Fatal("Mount: got nil, want an error — the mounted fsname does not match mnt's own")
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "findmnt" {
		t.Fatalf("Mount: got calls %+v, want exactly one findmnt call and no mergerfs call", calls)
	}
}

// TestMounter_Unmount_NoOpWhenAlreadyGone is #268's own deleteShare
// scenario: a share whose per-share mount is already gone for any
// reason must unmount as a clean no-op, not 500 on fusermount's own
// "No such file or directory" for a mount that was never there. Against
// the pre-fix Unmount, which returns fusermount's raw error, this test
// fails.
func TestMounter_Unmount_NoOpWhenAlreadyGone(t *testing.T) {
	where := "/mnt/user/gone-share"
	r := disk.NewFakeRunner()
	r.Script("fusermount", []string{"-u", where}, nil, errors.New("fusermount: failed to unmount /mnt/user/gone-share: No such file or directory"))

	mounter := Mounter{Runner: r, IsMountpoint: func(string) (bool, error) { return false, nil }}
	if err := mounter.Unmount(context.Background(), where); err != nil {
		t.Fatalf("Unmount: got %v, want nil (already gone)", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "fusermount" {
		t.Fatalf("Unmount: got calls %+v, want exactly one fusermount call and no busy retry", calls)
	}
}

// TestMounter_Unmount_NoOpWhenPathDoesNotExist is
// TestMounter_Unmount_NoOpWhenAlreadyGone's own scenario against the
// production isMountpoint (IsMountpoint left nil), not a fake that
// already answers "not mounted": where is never created at all, so a
// real os.Stat of it fails with ENOENT, not the (false, nil)
// TestMounter_Unmount_NoOpWhenAlreadyGone's fake injects directly. A
// deleted share's own /mnt/user/<name> after the catch-all's own
// remount recreated everything but that one branch directory, or a
// mover write target under a fresh /run that was never recreated,
// reproduces exactly this: fusermount reports "No such file or
// directory" and isMountpoint's own os.Stat of the missing path fails
// the same way, which is still "not a mount point", not an error to
// propagate.
func TestMounter_Unmount_NoOpWhenPathDoesNotExist(t *testing.T) {
	where := filepath.Join(t.TempDir(), "mnt", "user", "gone-share")
	r := disk.NewFakeRunner()
	r.Script("fusermount", []string{"-u", where}, nil, errors.New("fusermount: failed to unmount "+where+": No such file or directory"))

	mounter := Mounter{Runner: r}
	if err := mounter.Unmount(context.Background(), where); err != nil {
		t.Fatalf("Unmount: got %v, want nil (path never existed)", err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "fusermount" {
		t.Fatalf("Unmount: got calls %+v, want exactly one fusermount call and no busy retry", calls)
	}
}

// TestMounter_Unmount_StillFailsWhenStillMounted proves the
// already-gone tolerance does not swallow a genuine unmount failure
// against a path that is, in fact, still mounted.
func TestMounter_Unmount_StillFailsWhenStillMounted(t *testing.T) {
	where := "/mnt/user"
	r := disk.NewFakeRunner()
	wantErr := errors.New("permission denied")
	r.Script("fusermount", []string{"-u", where}, nil, wantErr)

	mounter := Mounter{Runner: r, IsMountpoint: func(string) (bool, error) { return true, nil }}
	if err := mounter.Unmount(context.Background(), where); !errors.Is(err, wantErr) {
		t.Fatalf("Unmount: got %v, want it to wrap %v", err, wantErr)
	}
}

// TestMounter_Unmount_StillFailsWhenMountpointCheckErrorsForOtherReason
// proves the ENOENT tolerance is narrow, not "any isMountpoint error
// means already gone": a dead FUSE mount's own endpoint reports
// ENOTCONN, not ENOENT, from a stat against it (doc 08 §6's own
// "unreachable" mount state) — a live-but-broken mount, not an absent
// one — so Unmount must still surface fusermount's own error rather than
// swallow it.
func TestMounter_Unmount_StillFailsWhenMountpointCheckErrorsForOtherReason(t *testing.T) {
	where := "/mnt/user"
	r := disk.NewFakeRunner()
	wantErr := errors.New("fusermount: failed to unmount /mnt/user: Transport endpoint is not connected")
	r.Script("fusermount", []string{"-u", where}, nil, wantErr)

	mounter := Mounter{
		Runner:       r,
		IsMountpoint: func(string) (bool, error) { return false, syscall.ENOTCONN },
	}
	if err := mounter.Unmount(context.Background(), where); !errors.Is(err, wantErr) {
		t.Fatalf("Unmount: got %v, want it to wrap %v", err, wantErr)
	}
}

// TestMounter_Remount is doc 02 §4 "Adding a disk" step 6: unmount, then
// mount the grown Mount — in that order, and both against the real
// mount point.
func TestMounter_Remount(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	m := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	r.Script("fusermount", []string{"-u", where}, nil, nil)
	argv := m.Argv()
	r.Script(argv[0], argv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), previous, m); err != nil {
		t.Fatalf("Remount: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 || calls[0].Name != "fusermount" || calls[1].Name != "mergerfs" {
		t.Fatalf("Remount: got calls %+v, want [fusermount mergerfs] in that order", calls)
	}
}

func TestMounter_Remount_PropagatesUnmountError(t *testing.T) {
	r := disk.NewFakeRunner()
	// Deliberately not a "busy" error: this test is about Remount never
	// attempting Mount after a failed Unmount, not about Unmount's own
	// busy-retry (covered separately below), and disk.FakeRunner always
	// returns the same scripted response for a given argv, so a "busy"
	// error here would make Unmount retry for the real wall-clock
	// unmountRetryWindow before this test could observe the result.
	wantErr := errors.New("permission denied")
	r.Script("fusermount", []string{"-u", "/mnt/user"}, nil, wantErr)

	previous := Mount{Where: "/mnt/user", What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	m := previous
	// IsMountpoint: /mnt/user is never created here, so the real check
	// would treat it as already gone (#268) and Unmount would return nil
	// instead of wantErr — this test is about Remount stopping after a
	// genuine Unmount failure, so it fakes "still mounted".
	mounter := Mounter{Runner: r, IsMountpoint: func(string) (bool, error) { return true, nil }}
	if err := mounter.Remount(context.Background(), previous, m); !errors.Is(err, wantErr) {
		t.Fatalf("Remount: got %v, want it to wrap %v", err, wantErr)
	}
	if len(r.Calls()) != 1 {
		t.Fatalf("Remount: got calls %+v, want mount never attempted after a failed unmount", r.Calls())
	}
}

// TestMounter_Remount_RollsBackOnMountFailure is this issue's central
// safety property: if the grown mount fails, Remount must not leave
// mnt.Where unmounted — it re-mounts previous so a malformed branch list
// degrades to "the expansion didn't take" rather than a storage outage.
func TestMounter_Remount_RollsBackOnMountFailure(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	grown := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r.Script("fusermount", []string{"-u", where}, nil, nil)
	wantErr := errors.New("mergerfs: invalid branch")
	grownArgv := grown.Argv()
	r.Script(grownArgv[0], grownArgv[1:], nil, wantErr)
	previousArgv := previous.Argv()
	r.Script(previousArgv[0], previousArgv[1:], nil, nil)

	mounter := Mounter{Runner: r}
	if err := mounter.Remount(context.Background(), previous, grown); !errors.Is(err, wantErr) {
		t.Fatalf("Remount: got %v, want it to wrap %v", err, wantErr)
	}

	calls := r.Calls()
	if len(calls) != 3 {
		t.Fatalf("Remount: got calls %+v, want [fusermount, failed mergerfs, rollback mergerfs]", calls)
	}
	if calls[0].Name != "fusermount" || calls[1].Name != "mergerfs" || calls[2].Name != "mergerfs" {
		t.Fatalf("Remount: got calls %+v, want [fusermount mergerfs mergerfs]", calls)
	}
	if calls[2].Args[len(calls[2].Args)-2] != "/mnt/disk1=RW" {
		t.Fatalf("Remount: rollback call args %+v, want the previous (ungrown) branch list", calls[2].Args)
	}
}

// TestMounter_Remount_ReportsRollbackFailureAlongsideTheOriginalError
// covers the case where the pool can't even go back to how it was: both
// errors must reach the caller, since losing the original failure would
// hide why Remount stopped short, and losing the rollback failure would
// hide that mnt.Where is now unmounted.
func TestMounter_Remount_ReportsRollbackFailureAlongsideTheOriginalError(t *testing.T) {
	where := testWhere(t)
	r := disk.NewFakeRunner()
	previous := Mount{Where: where, What: "/mnt/disk1=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}
	grown := Mount{Where: where, What: "/mnt/disk1=RW:/mnt/disk2=RW", FSName: "hoserva-pool", CreatePolicy: DefaultCreatePolicy, Options: DefaultOptions()}

	r.Script("fusermount", []string{"-u", where}, nil, nil)
	mountErr := errors.New("mergerfs: invalid branch")
	grownArgv := grown.Argv()
	r.Script(grownArgv[0], grownArgv[1:], nil, mountErr)
	rollbackErr := errors.New("mergerfs: still invalid")
	previousArgv := previous.Argv()
	r.Script(previousArgv[0], previousArgv[1:], nil, rollbackErr)

	mounter := Mounter{Runner: r}
	err := mounter.Remount(context.Background(), previous, grown)
	if !errors.Is(err, mountErr) {
		t.Fatalf("Remount: got %v, want it to wrap the original mount error %v", err, mountErr)
	}
	if err == nil || !strings.Contains(err.Error(), rollbackErr.Error()) {
		t.Fatalf("Remount: got %v, want it to also mention the rollback failure %v", err, rollbackErr)
	}
}
