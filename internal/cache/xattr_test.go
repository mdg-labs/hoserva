package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCopyXattrs_CopiesUserAttribute proves a user.* xattr set on the
// source shows up on the target with the same value (doc 09 §2:
// "preserving ... xattrs, ACLs"). Skipped when the temp filesystem
// backing t.TempDir() does not support xattrs at all, which some CI
// tmpfs configurations do not.
func TestCopyXattrs_CopiesUserAttribute(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := unix.Setxattr(src, "user.hoserva_test", []byte("value"), 0); err != nil {
		if isXattrUnsupported(err) || errors.Is(err, unix.EPERM) {
			t.Skipf("xattrs unsupported on this filesystem: %v", err)
		}
		t.Fatal(err)
	}

	if err := copyXattrs(src, dst); err != nil {
		t.Fatalf("copyXattrs: %v", err)
	}

	got, err := getXattr(dst, "user.hoserva_test")
	if err != nil {
		t.Fatalf("getXattr on target: %v", err)
	}
	if string(got) != "value" {
		t.Fatalf("target xattr = %q, want %q", got, "value")
	}
}

func TestCopyXattrs_NoAttributesIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := copyXattrs(src, dst); err != nil {
		t.Fatalf("copyXattrs on a file with no xattrs: %v", err)
	}
}
