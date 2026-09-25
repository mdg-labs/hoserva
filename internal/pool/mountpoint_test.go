package pool

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

// TestIsMountedConfirmed_NotExist proves the ENOENT tolerance is
// preserved: a path that genuinely does not exist is confirmed not
// mounted, exactly like IsMounted's own collapse — this issue (#365) must
// not change behavior for a stopped array whose catch-all was never
// mounted or has already been cleanly torn down.
func TestIsMountedConfirmed_NotExist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-mounted")
	mounted, err := IsMountedConfirmed(path)
	if err != nil {
		t.Fatalf("IsMountedConfirmed(%s) error = %v, want nil for a confirmed-absent path", path, err)
	}
	if mounted {
		t.Fatalf("IsMountedConfirmed(%s) = true, want false for a path that does not exist", path)
	}
}

// TestIsMountedConfirmed_UnknownError proves a stat failure that is not
// ENOENT is reported, not silently collapsed to "not mounted" the way
// IsMounted's single bool would — the exact gap #365 is about, where a
// dead FUSE endpoint's ENOTCONN was indistinguishable from a stopped
// pool. A path with an embedded NUL byte fails os.Stat's argument
// validation (EINVAL) before any syscall runs, reliably reproducing "stat
// failed for a reason other than the path being absent" without a real
// mount.
func TestIsMountedConfirmed_UnknownError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad\x00name")
	mounted, err := IsMountedConfirmed(path)
	if err == nil {
		t.Fatalf("IsMountedConfirmed(%s) error = nil, want a non-nil error for an unconfirmable stat failure", path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("IsMountedConfirmed(%s) error = %v classified as ErrNotExist, want an unconfirmed, non-ENOENT error", path, err)
	}
	if mounted {
		t.Fatalf("IsMountedConfirmed(%s) = (true, %v), want (false, err) when the state cannot be confirmed", path, err)
	}
}

// TestIsMounted_UnknownErrorStillCollapsesToFalse documents that IsMounted
// itself is unchanged: it keeps its existing "any stat error means not
// mounted" behavior, since it still backs Mounter.Mount/Unmount and the
// logging (non-strict) topology hook, both intentionally untouched by
// #365.
func TestIsMounted_UnknownErrorStillCollapsesToFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad\x00name")
	if IsMounted(path) {
		t.Fatalf("IsMounted(%s) = true, want false", path)
	}
}
