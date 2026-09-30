package disk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSetImmutable_RunsChattrWithCorrectFlag(t *testing.T) {
	r := NewFakeRunner()
	r.Script("chattr", []string{"+i", "/mnt/disk1"}, nil, nil)
	r.Script("chattr", []string{"-i", "/mnt/disk1"}, nil, nil)

	if err := SetImmutable(context.Background(), r, "/mnt/disk1", true); err != nil {
		t.Fatalf("SetImmutable(true): %v", err)
	}
	if err := SetImmutable(context.Background(), r, "/mnt/disk1", false); err != nil {
		t.Fatalf("SetImmutable(false): %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "chattr" || calls[0].Args[0] != "+i" {
		t.Fatalf("first call = %+v, want chattr +i", calls[0])
	}
	if calls[1].Name != "chattr" || calls[1].Args[0] != "-i" {
		t.Fatalf("second call = %+v, want chattr -i", calls[1])
	}
}

func TestSetImmutable_PropagatesError(t *testing.T) {
	r := NewFakeRunner()
	wantErr := errors.New("operation not permitted")
	r.Script("chattr", []string{"+i", "/mnt/disk1"}, nil, wantErr)

	if err := SetImmutable(context.Background(), r, "/mnt/disk1", true); !errors.Is(err, wantErr) {
		t.Fatalf("SetImmutable: got %v, want it to wrap %v", err, wantErr)
	}
}

func TestSetImmutable_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SetImmutable(ctx, NewFakeRunner(), "/mnt/disk1", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetImmutable: got %v, want context.Canceled", err)
	}
}

func TestEnsureEmptyMountpoint_CreatesDirectoryAndSetsImmutable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	r.Script("chattr", []string{"+i", dir}, nil, nil)

	if err := EnsureEmptyMountpoint(context.Background(), r, dir); err != nil {
		t.Fatalf("EnsureEmptyMountpoint: %v", err)
	}

	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("EnsureEmptyMountpoint: directory %s was not created: %v", dir, err)
	}

	calls := r.Calls()
	if len(calls) != 1 || calls[0].Name != "chattr" || calls[0].Args[0] != "+i" || calls[0].Args[1] != dir {
		t.Fatalf("got calls %+v, want exactly one chattr +i %s", calls, dir)
	}
}

func TestEnsureEmptyMountpoint_PropagatesChattrError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	wantErr := errors.New("operation not permitted")
	r.Script("chattr", []string{"+i", dir}, nil, wantErr)

	if err := EnsureEmptyMountpoint(context.Background(), r, dir); !errors.Is(err, wantErr) {
		t.Fatalf("EnsureEmptyMountpoint: got %v, want it to wrap %v", err, wantErr)
	}
}

func TestEnsureEmptyMountpoint_RefusesNonEmptyDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("data"), 0o644); err != nil {
		t.Fatalf("writing existing file: %v", err)
	}
	r := NewFakeRunner()

	if err := EnsureEmptyMountpoint(context.Background(), r, dir); !errors.Is(err, ErrMountpointNotEmpty) {
		t.Fatalf("EnsureEmptyMountpoint: got %v, want ErrMountpointNotEmpty", err)
	}
	if calls := r.Calls(); len(calls) != 0 {
		t.Fatalf("EnsureEmptyMountpoint on a non-empty directory made calls %+v, want none — it must never chattr +i a non-empty path", calls)
	}
}

func TestEnsureEmptyMountpoint_RefusesAlreadyMountedPath(t *testing.T) {
	// /proc is a real, already-mounted filesystem on every Linux host and
	// the lab container alike — reading its stat info, never mounting or
	// unmounting anything, is enough to exercise the "already mounted"
	// refusal without touching a real block device or mount (CLAUDE.md).
	if runtime.GOOS != "linux" {
		t.Skip("device-ID mount detection is Linux-specific")
	}
	if mounted, err := isMountpoint("/proc"); err != nil || !mounted {
		t.Skipf("/proc is not a separately mounted filesystem here (mounted=%v, err=%v), skipping", mounted, err)
	}
	r := NewFakeRunner()

	if err := EnsureEmptyMountpoint(context.Background(), r, "/proc"); !errors.Is(err, ErrMountpointMounted) {
		t.Fatalf("EnsureEmptyMountpoint(/proc): got %v, want ErrMountpointMounted", err)
	}
	if calls := r.Calls(); len(calls) != 0 {
		t.Fatalf("EnsureEmptyMountpoint(/proc) made calls %+v, want none — it must never chattr +i an already-mounted path", calls)
	}
}

func TestIsMountpoint_PlainDirectoryIsNotAMountpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	mounted, err := isMountpoint(dir)
	if err != nil {
		t.Fatalf("isMountpoint(%s): %v", dir, err)
	}
	if mounted {
		t.Fatalf("isMountpoint(%s) = true, want false for a plain, freshly created directory", dir)
	}
}

func requireProcMounted(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("device-ID mount detection is Linux-specific")
	}
	if mounted, err := isMountpoint("/proc"); err != nil || !mounted {
		t.Skipf("/proc is not a separately mounted filesystem here (mounted=%v, err=%v), skipping", mounted, err)
	}
}

func chattrCalls(r *FakeRunner) []RunCall {
	var out []RunCall
	for _, c := range r.Calls() {
		if c.Name == "chattr" {
			out = append(out, c)
		}
	}
	return out
}

func TestGuardMountpoint_MountedPathIsSkippedWithoutError(t *testing.T) {
	requireProcMounted(t)
	r := NewFakeRunner()

	if err := GuardMountpoint(context.Background(), r, "/proc"); err != nil {
		t.Fatalf("GuardMountpoint(/proc): got %v, want nil — a mounted slot is skipped, not a failure", err)
	}
	if calls := r.Calls(); len(calls) != 0 {
		t.Fatalf("GuardMountpoint on a mounted path made calls %+v, want none", calls)
	}
}

func TestGuardMountpoint_EmptyUnmountedDirectoryIsMadeImmutable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()

	if err := GuardMountpoint(context.Background(), r, dir); err != nil {
		t.Fatalf("GuardMountpoint: %v", err)
	}
	calls := chattrCalls(r)
	if len(calls) != 1 || calls[0].Args[0] != "+i" || calls[0].Args[1] != dir {
		t.Fatalf("got chattr calls %+v, want exactly chattr +i %s", calls, dir)
	}
}

func TestGuardMountpoint_NonEmptyDirectoryIsReportedNeverChattred(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()

	if err := GuardMountpoint(context.Background(), r, dir); !errors.Is(err, ErrMountpointNotEmpty) {
		t.Fatalf("GuardMountpoint: got %v, want ErrMountpointNotEmpty", err)
	}
	if calls := chattrCalls(r); len(calls) != 0 {
		t.Fatalf("chattr issued for a non-empty mountpoint: %+v", calls)
	}
}

func TestGuardMountpoint_ChattrFailureIsReported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	wantErr := errors.New("inappropriate ioctl for device")
	r.Script("chattr", []string{"+i", dir}, nil, wantErr)

	if err := GuardMountpoint(context.Background(), r, dir); !errors.Is(err, wantErr) {
		t.Fatalf("GuardMountpoint: got %v, want it to wrap %v", err, wantErr)
	}
}

type orderRecordingMounter struct {
	runner *FakeRunner
	err    error
	// chattrsBeforeMount is how many chattr calls had already been made
	// when Mount was reached.
	chattrsBeforeMount int
	mounts             []MountUnit
}

func (m *orderRecordingMounter) Mount(_ context.Context, unit MountUnit) error {
	m.chattrsBeforeMount = len(chattrCalls(m.runner))
	m.mounts = append(m.mounts, unit)
	return m.err
}

func (m *orderRecordingMounter) Unmount(context.Context, MountUnit) error { return nil }

func TestGuardedMounter_GuardsTheMountpointBeforeMounting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	inner := &orderRecordingMounter{runner: r}
	m := GuardedMounter{Mounter: inner, Runner: r}

	if err := m.Mount(context.Background(), MountUnit{Where: dir, UUID: "u1"}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	calls := chattrCalls(r)
	if len(calls) != 1 || calls[0].Args[0] != "+i" || calls[0].Args[1] != dir {
		t.Fatalf("got chattr calls %+v, want exactly chattr +i %s", calls, dir)
	}
	if inner.chattrsBeforeMount != 1 || len(inner.mounts) != 1 {
		t.Fatalf("chattr must precede the mount: chattrs before mount = %d, mounts = %d", inner.chattrsBeforeMount, len(inner.mounts))
	}
}

func TestGuardedMounter_NeverChattrsAMountedOrNonEmptyPathButStillMounts(t *testing.T) {
	requireProcMounted(t)
	nonEmpty := filepath.Join(t.TempDir(), "disk2")
	if err := os.MkdirAll(nonEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmpty, "stray"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{"/proc", nonEmpty} {
		r := NewFakeRunner()
		inner := &orderRecordingMounter{runner: r}
		m := GuardedMounter{Mounter: inner, Runner: r}

		if err := m.Mount(context.Background(), MountUnit{Where: where, UUID: "u"}); err != nil {
			t.Fatalf("Mount(%s): %v — a guard finding must not fail the mount", where, err)
		}
		if calls := chattrCalls(r); len(calls) != 0 {
			t.Fatalf("Mount(%s) issued chattr %+v, want none", where, calls)
		}
		if len(inner.mounts) != 1 {
			t.Fatalf("Mount(%s) did not reach the wrapped mounter", where)
		}
	}
}

func TestGuardedMounter_ChattrFailureDoesNotFailTheMount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	r.Script("chattr", []string{"+i", dir}, nil, errors.New("operation not supported"))
	inner := &orderRecordingMounter{runner: r}
	m := GuardedMounter{Mounter: inner, Runner: r}

	if err := m.Mount(context.Background(), MountUnit{Where: dir, UUID: "u1"}); err != nil {
		t.Fatalf("Mount: %v", err)
	}
	if len(inner.mounts) != 1 {
		t.Fatalf("the wrapped mounter was not reached after a chattr failure")
	}
}

func TestGuardedMounter_MountFailureIsReturnedAndTheDirectoryStaysGuarded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	mountErr := errors.New("device never appeared")
	inner := &orderRecordingMounter{runner: r, err: mountErr}
	m := GuardedMounter{Mounter: inner, Runner: r}

	if err := m.Mount(context.Background(), MountUnit{Where: dir, UUID: "u1"}); !errors.Is(err, mountErr) {
		t.Fatalf("Mount: got %v, want %v", err, mountErr)
	}
	if calls := chattrCalls(r); len(calls) != 1 || calls[0].Args[0] != "+i" {
		t.Fatalf("got chattr calls %+v, want the single chattr +i issued before the failed mount", calls)
	}
}

func TestGuardedMounter_CancelledContextMountsNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	inner := &orderRecordingMounter{runner: r}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := (GuardedMounter{Mounter: inner, Runner: r}).Mount(ctx, MountUnit{Where: dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Mount: got %v, want context.Canceled", err)
	}
	if len(inner.mounts) != 0 {
		t.Fatalf("a cancelled Mount reached the wrapped mounter")
	}
}

func TestGuardedMounter_UnmountDelegatesUntouched(t *testing.T) {
	r := NewFakeRunner()
	fm := NewFakeMounter()
	m := GuardedMounter{Mounter: fm, Runner: r}

	if err := m.Unmount(context.Background(), MountUnit{Where: "/mnt/disk1"}); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if len(fm.Unmounts) != 1 || len(r.Calls()) != 0 {
		t.Fatalf("Unmount must delegate only: unmounts=%d runner calls=%+v", len(fm.Unmounts), r.Calls())
	}
}

// A disk's own nofail mount unit can activate between EnsureEmptyMountpoint's
// mounted check and its chattr, which would then land on the mounted
// filesystem's root instead of the shadowed directory.
func TestEnsureEmptyMountpoint_MountAppearingBeforeChattrIsRolledBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "disk1")
	r := NewFakeRunner()
	checks := 0
	prev := mountCheck
	mountCheck = func(string) (bool, error) {
		checks++
		return checks > 1, nil
	}
	t.Cleanup(func() { mountCheck = prev })

	err := EnsureEmptyMountpoint(context.Background(), r, dir)
	if !errors.Is(err, ErrMountpointMounted) {
		t.Fatalf("EnsureEmptyMountpoint: got %v, want ErrMountpointMounted", err)
	}
	calls := chattrCalls(r)
	if len(calls) != 2 || calls[0].Args[0] != "+i" || calls[1].Args[0] != "-i" || calls[1].Args[1] != dir {
		t.Fatalf("got chattr calls %+v, want +i then -i %s to undo the flag on the mounted root", calls, dir)
	}
}
