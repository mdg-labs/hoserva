//go:build linux

package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRenameNoReplaceFallback_ConflictNeverAutoResolved proves the EINVAL
// fallback renameNoReplace takes when Renameat2 can't carry the flag
// through the destination filesystem — true of every mergerfs mount
// (#243) — never silently replaces a target, matching doc 09 §2's
// guarantee for the direct Renameat2 path. The fallback is
// link(2)-then-unlink(2): link(2) is itself the atomic create-if-absent
// primitive, performing the no-clobber check and the claim as one kernel
// operation, so a target present at the time of this call and a target
// created by a concurrent writer (a live SMB/NFS client or a container
// writing directly through the pool mount) an instant before link(2)
// lands are the same case from link(2)'s point of view — both fail
// closed with EEXIST — rather than the fallback needing to observe the
// second case within a check-then-act window it doesn't have.
func TestRenameNoReplaceFallback_ConflictNeverAutoResolved(t *testing.T) {
	dir := t.TempDir()
	oldpath := filepath.Join(dir, "old")
	newpath := filepath.Join(dir, "new")

	if err := os.WriteFile(oldpath, []byte("source content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newpath, []byte("existing target content"), 0o640); err != nil {
		t.Fatal(err)
	}

	err := renameNoReplaceFallback(oldpath, newpath)
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("renameNoReplaceFallback: got %v, want os.ErrExist", err)
	}

	got, rerr := os.ReadFile(newpath)
	if rerr != nil || string(got) != "existing target content" {
		t.Fatalf("target must survive a conflict unmodified, got %q, %v", got, rerr)
	}
	if _, err := os.Stat(oldpath); err != nil {
		t.Fatalf("oldpath must survive a conflict: %v", err)
	}
}

// TestRenameNoReplaceFallback_RenamesWhenTargetAbsent proves the fallback
// still completes the move when there is no conflict — the case that was
// failing with EINVAL against a real mergerfs mount before this fix.
func TestRenameNoReplaceFallback_RenamesWhenTargetAbsent(t *testing.T) {
	dir := t.TempDir()
	oldpath := filepath.Join(dir, "old")
	newpath := filepath.Join(dir, "new")

	if err := os.WriteFile(oldpath, []byte("source content"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplaceFallback(oldpath, newpath); err != nil {
		t.Fatalf("renameNoReplaceFallback: %v", err)
	}
	got, err := os.ReadFile(newpath)
	if err != nil || string(got) != "source content" {
		t.Fatalf("target = %q, %v, want the source content", got, err)
	}
	if _, err := os.Stat(oldpath); !os.IsNotExist(err) {
		t.Fatalf("oldpath should be gone after rename: %v", err)
	}
}
