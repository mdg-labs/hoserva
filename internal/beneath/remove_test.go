package beneath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func openTestRoot(t *testing.T, dir string) int {
	t.Helper()
	fd, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	t.Fatal(err)
	return false
}

func TestRemoveAll_RemovesANestedTreeAndLeavesItsSiblings(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/config/deep/er", "app/other", "keep")
	for _, f := range []string{"app/config/a", "app/config/deep/b", "app/config/deep/er/c", "app/other/d"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/", filepath.Join(root, "app/config/deep/link")); err != nil {
		t.Fatal(err)
	}

	if err := RemoveAll(openTestRoot(t, root), "app/config"); err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
	if exists(t, filepath.Join(root, "app/config")) {
		t.Error("app/config is still there")
	}
	for _, kept := range []string{"app", "app/other/d", "keep"} {
		if !exists(t, filepath.Join(root, kept)) {
			t.Errorf("%s was removed", kept)
		}
	}
}

func TestRemoveAll_RemovesAFile(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app")
	if err := os.WriteFile(filepath.Join(root, "app/f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(openTestRoot(t, root), "app/f"); err != nil || exists(t, filepath.Join(root, "app/f")) {
		t.Fatalf("RemoveAll(file) = %v", err)
	}
}

func TestRemoveAll_MissingPathIsNotExist(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app")
	rfd := openTestRoot(t, root)
	for _, rel := range []string{"app/gone", "gone/deeper"} {
		if err := RemoveAll(rfd, rel); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("RemoveAll(%s) = %v, want ErrNotExist", rel, err)
		}
	}
}

func TestRemoveAll_RefusesASymlinkedComponentAndLeavesTheTargetAlone(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, outside, "config/sub")
	if err := os.Symlink(outside, filepath.Join(root, "app")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(openTestRoot(t, root), "app/config"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("RemoveAll through a symlinked parent = %v, want ErrSymlink", err)
	}
	if !exists(t, filepath.Join(outside, "config/sub")) {
		t.Error("the tree behind the symlink was removed")
	}
}

func TestRemoveAll_RefusesASymlinkAsTheEntry(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, outside, "sub")
	if err := os.Symlink(outside, filepath.Join(root, "config")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(openTestRoot(t, root), "config"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("RemoveAll(symlink) = %v, want ErrSymlink", err)
	}
	if !exists(t, filepath.Join(outside, "sub")) || !exists(t, filepath.Join(root, "config")) {
		t.Error("the symlink or its target was removed")
	}
}

func TestRemoveAll_NeverFollowsALinkInsideTheTree(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	mkdirs(t, root, "config")
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "config/link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(openTestRoot(t, root), "config"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, filepath.Join(outside, "f")) {
		t.Error("a file behind a link inside the tree was removed")
	}
}

func TestRemoveAll_RefusesAPathNotBeneathTheRoot(t *testing.T) {
	root := t.TempDir()
	rfd := openTestRoot(t, root)
	for _, rel := range []string{"", "/a", "a/../b", ".."} {
		if err := RemoveAll(rfd, rel); err == nil {
			t.Errorf("RemoveAll(%q) succeeded", rel)
		}
	}
}

func TestRemoveAll_ASubdirectoryThatVanishesMidwayIsSkippedAndTheRestIsRemoved(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "config/a/x", "config/b/y", "config/c")
	for _, f := range []string{"config/a/x/f", "config/b/y/f", "config/top"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	beforeDescend = func(name string) {
		if name == "a" {
			if err := os.RemoveAll(filepath.Join(root, "config/a")); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { beforeDescend = nil })

	if err := RemoveAll(openTestRoot(t, root), "config"); err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
	if exists(t, filepath.Join(root, "config")) {
		t.Error("config is still there")
	}
}

func TestRemoveAll_APlannedDirectoryThatVanishesBeforeItIsOpenedIsSkipped(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/config")
	beforeDescend = func(string) {
		if err := os.Remove(filepath.Join(root, "app/config")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDescend = nil })

	if err := RemoveAll(openTestRoot(t, root), "app/config"); err != nil {
		t.Fatalf("RemoveAll = %v", err)
	}
}

func identityOfDir(t *testing.T, path string) (dev, ino uint64) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Dev), uint64(st.Ino)
}

func TestRemoveDirIf_RemovesTheTreeWhoseIdentityMatches(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/deep", "keep")
	if err := os.WriteFile(filepath.Join(root, "app/deep/f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dev, ino := identityOfDir(t, filepath.Join(root, "app"))

	err := RemoveDirIf(openTestRoot(t, root), "app", func(d, i uint64) bool { return d == dev && i == ino })
	if err != nil {
		t.Fatalf("RemoveDirIf = %v", err)
	}
	if exists(t, filepath.Join(root, "app")) || !exists(t, filepath.Join(root, "keep")) {
		t.Fatal("RemoveDirIf did not remove exactly the tree")
	}
}

func TestRemoveDirIf_LeavesADirectoryWhoseIdentityDiffers(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/deep", "other")
	if err := os.WriteFile(filepath.Join(root, "app/deep/f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dev, ino := identityOfDir(t, filepath.Join(root, "other"))

	err := RemoveDirIf(openTestRoot(t, root), "app", func(d, i uint64) bool { return d == dev && i == ino })
	if !errors.Is(err, ErrNotExpected) {
		t.Fatalf("RemoveDirIf = %v, want ErrNotExpected", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "app/deep/f")); string(got) != "x" {
		t.Fatalf("the directory with another identity was changed: %q", got)
	}
}

func TestRemoveDirIf_DoesNotFollowALinkAtTheName(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "target")
	if err := os.WriteFile(filepath.Join(root, "target/f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "app")); err != nil {
		t.Fatal(err)
	}

	err := RemoveDirIf(openTestRoot(t, root), "app", func(uint64, uint64) bool { return true })
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("RemoveDirIf = %v, want ErrSymlink", err)
	}
	if !exists(t, filepath.Join(root, "target/f")) {
		t.Fatal("the link's target was removed")
	}
}

func TestRemoveDirIf_ReportsAMissingDirectory(t *testing.T) {
	root := t.TempDir()
	err := RemoveDirIf(openTestRoot(t, root), "app", func(uint64, uint64) bool { return true })
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("RemoveDirIf = %v, want a not-exist error", err)
	}
}

func TestRemoveDirIf_DoesNotFollowASubdirectorySwappedForALinkWhileRemoving(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, root, "app/sub")
	dev, ino := identityOfDir(t, filepath.Join(root, "app"))
	beforeDescend = func(name string) {
		if name != "sub" {
			return
		}
		beforeDescend = nil
		if err := os.Remove(filepath.Join(root, "app/sub")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "app/sub")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDescend = nil })

	_ = RemoveDirIf(openTestRoot(t, root), "app", func(d, i uint64) bool { return d == dev && i == ino })
	if !exists(t, filepath.Join(outside, "f")) {
		t.Fatal("RemoveDirIf followed a link out of the tree")
	}
}

func TestRemoveContents_ReportsADirectoryThatWasRemovedWhileOpen(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "gone")
	fd, err := Open(openTestRoot(t, root), "gone", unix.O_RDONLY|unix.O_DIRECTORY)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := os.Remove(filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := removeContents(fd, "gone"); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("removeContents = %v, want an error wrapping ENOENT", err)
	}
}
