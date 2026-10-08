package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestProcOpenChecker_DetectsOwnOpenFile proves the real /proc-scanning
// implementation finds a file this very test process holds open — the
// same mechanism that finds mergerfs's own descriptor when a client
// reaches a cache file through /mnt/user/<share> (doc 09 §2), exercised
// here against the test binary's own PID rather than a real mergerfs
// process (the lab test exercises that).
func TestProcOpenChecker_DetectsOwnOpenFile(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd on this platform")
	}
	path := filepath.Join(t.TempDir(), "held-open.bin")
	if err := os.WriteFile(path, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	checker := ProcOpenChecker{}
	open, err := checker.IsOpen(context.Background(), path)
	if err != nil {
		t.Fatalf("IsOpen: %v", err)
	}
	if !open {
		t.Fatal("expected the held-open file to be reported open")
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	open, err = checker.IsOpen(context.Background(), path)
	if err != nil {
		t.Fatalf("IsOpen after close: %v", err)
	}
	if open {
		t.Fatal("expected the file to be reported closed after Close")
	}
}

func TestProcOpenChecker_MissingFileIsNotOpen(t *testing.T) {
	checker := ProcOpenChecker{}
	open, err := checker.IsOpen(context.Background(), filepath.Join(t.TempDir(), "never-existed"))
	if err != nil {
		t.Fatalf("IsOpen: %v", err)
	}
	if open {
		t.Fatal("a nonexistent file cannot be open")
	}
}

func TestFakeOpenChecker_DefaultsClosed(t *testing.T) {
	f := NewFakeOpenChecker()
	open, err := f.IsOpen(context.Background(), "/anything")
	if err != nil || open {
		t.Fatalf("IsOpen = %v, %v, want false, nil", open, err)
	}
}

func TestFakeOpenChecker_SetOpenToggles(t *testing.T) {
	f := NewFakeOpenChecker()
	f.SetOpen("/a", true)
	open, _ := f.IsOpen(context.Background(), "/a")
	if !open {
		t.Fatal("expected /a to be open")
	}
	f.SetOpen("/a", false)
	open, _ = f.IsOpen(context.Background(), "/a")
	if open {
		t.Fatal("expected /a to be closed")
	}
}

// TestDeps_ZeroValueSelectsProcOpenChecker pins what production relies on:
// hoservad's mover, relocation, rebalance and evacuation wiring leaves the
// open-file checker unset, so a zero Deps must resolve to the real
// /proc-scanning ProcOpenChecker, never to a checker that reports nothing
// open.
func TestDeps_ZeroValueSelectsProcOpenChecker(t *testing.T) {
	got := Deps{}.withDefaults().Open
	if _, ok := got.(ProcOpenChecker); !ok {
		t.Fatalf("Deps{}.withDefaults().Open = %T, want ProcOpenChecker", got)
	}

	fake := NewFakeOpenChecker()
	if got := (Deps{Open: fake}).withDefaults().Open; got != OpenChecker(fake) {
		t.Fatalf("an injected Open was replaced: got %T", got)
	}
}

// The real snapshot answers for the held entry by its identity: the path
// replaced by a link to another file does not change the answer, and an entry
// whose identity cannot be read is an error, never "not open".
func TestIsOpenSource_RealSnapshotAnswersByIdentityNotPath(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd on this platform")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "held-open.bin")
	other := filepath.Join(dir, "closed.bin")
	for _, p := range []string{path, other} {
		if err := os.WriteFile(p, []byte("x"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	open, err := shareOpenChecker(context.Background(), ProcOpenChecker{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := isOpenSource(context.Background(), open, info, other); err != nil || !got {
		t.Fatalf("isOpenSource(held entry, path of a closed file) = %v, %v, want true", got, err)
	}
	otherInfo, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := isOpenSource(context.Background(), open, otherInfo, path); err != nil || got {
		t.Fatalf("isOpenSource(closed entry, path of the open file) = %v, %v, want false", got, err)
	}
	if _, err := isOpenSource(context.Background(), open, fakeInfo{}, path); err == nil {
		t.Fatal("isOpenSource of an entry with no device and inode succeeded, want an error")
	}
}

type fakeInfo struct{ os.FileInfo }

func (fakeInfo) Sys() any { return nil }
