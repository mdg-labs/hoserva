package share

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOSFS_ChownSwallowsPermissionDenied(t *testing.T) {
	dir := t.TempDir()
	// GID 100 ("users", Q26) is out of reach for an unprivileged test
	// process unless it happens to already be a member of it — chown's
	// permission error is swallowed rather than failing share creation
	// over something only hoservad's own root process needs (doc 01
	// §7). As root this chown just succeeds, which is also nil.
	if err := (OSFS{}).Chown(dir, -1, ShareGID); err != nil {
		t.Fatalf("Chown = %v, want nil (a permission error is swallowed)", err)
	}
}

func TestOSFS_ChownSurfacesOtherErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err := (OSFS{}).Chown(missing, -1, os.Getgid())
	if err == nil {
		t.Fatal("Chown on a missing path = nil, want an error")
	}
	if errors.Is(err, os.ErrPermission) {
		t.Fatalf("Chown on a missing path = %v, want something other than a permission error", err)
	}
}
