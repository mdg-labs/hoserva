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
