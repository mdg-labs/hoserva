package beneath

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	if err := removeContents(fd, "gone", nil); !errors.Is(err, unix.ENOENT) {
		t.Fatalf("removeContents = %v, want an error wrapping ENOENT", err)
	}
}

// listedUnder returns a func accepting the identity of every entry under dir
// that is in the listed relative paths, and dir's own.
func listedUnder(t *testing.T, dir string, rels ...string) func(dev, ino uint64) bool {
	t.Helper()
	type id struct{ dev, ino uint64 }
	ids := map[id]bool{}
	for _, rel := range append([]string{"."}, rels...) {
		d, i := identityOfDir(t, filepath.Join(dir, rel))
		ids[id{d, i}] = true
	}
	return func(dev, ino uint64) bool { return ids[id{dev, ino}] }
}

func writeFiles(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(rel), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveDirIfListed_RemovesEverythingListedAndTheDirectory(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/a/b", "keep")
	writeFiles(t, root, "app/f", "app/a/g", "app/a/b/h")
	if err := os.Symlink("/", filepath.Join(root, "app/link")); err != nil {
		t.Fatal(err)
	}
	listed := listedUnder(t, filepath.Join(root, "app"), "f", "a", "a/g", "a/b", "a/b/h", "link")

	kept, err := RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if err != nil || len(kept) != 0 {
		t.Fatalf("RemoveDirIfListed = %v, %v", kept, err)
	}
	if exists(t, filepath.Join(root, "app")) || !exists(t, filepath.Join(root, "keep")) {
		t.Fatal("RemoveDirIfListed did not remove exactly the tree")
	}
}

func TestRemoveDirIfListed_KeepsWhatIsNotListedAndTheDirectoriesHoldingIt(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/a/b", "app/foreign/inner")
	writeFiles(t, root, "app/f", "app/a/g", "app/a/b/h", "app/a/stray", "app/foreign/x", "app/foreign/inner/y")
	listed := listedUnder(t, filepath.Join(root, "app"), "f", "a", "a/g", "a/b", "a/b/h")

	kept, err := RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if err != nil {
		t.Fatalf("RemoveDirIfListed = %v", err)
	}
	sort.Strings(kept)
	if got, want := strings.Join(kept, ","), "app/a/stray,app/foreign"; got != want {
		t.Fatalf("kept = %s, want %s", got, want)
	}
	for _, p := range []string{"app/a/stray", "app/foreign/x", "app/foreign/inner/y"} {
		if got, _ := os.ReadFile(filepath.Join(root, p)); string(got) != p {
			t.Fatalf("%s = %q, was changed", p, got)
		}
	}
	for _, p := range []string{"app/f", "app/a/g", "app/a/b"} {
		if exists(t, filepath.Join(root, p)) {
			t.Errorf("%s was listed and is still there", p)
		}
	}
}

func TestRemoveDirIfListed_DoesNotOpenAnUnlistedDirectory(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/foreign")
	opened := false
	beforeDescend = func(name string) {
		if name == "foreign" {
			opened = true
		}
	}
	t.Cleanup(func() { beforeDescend = nil })
	listed := listedUnder(t, filepath.Join(root, "app"))

	kept, err := RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if err != nil || len(kept) != 1 || kept[0] != "app/foreign" {
		t.Fatalf("RemoveDirIfListed = %v, %v", kept, err)
	}
	if opened {
		t.Fatal("an unlisted directory was opened")
	}
}

func TestRemoveDirIfListed_KeepsADirectorySwappedForAnUnlistedOneBeforeItIsOpened(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/a", "elsewhere")
	writeFiles(t, root, "elsewhere/f")
	listed := listedUnder(t, filepath.Join(root, "app"), "a")
	beforeDescend = func(name string) {
		if name != "a" {
			return
		}
		if err := os.Remove(filepath.Join(root, "app/a")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(root, "elsewhere"), filepath.Join(root, "app/a")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDescend = nil })

	kept, err := RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if err != nil || len(kept) != 1 || kept[0] != "app/a" {
		t.Fatalf("RemoveDirIfListed = %v, %v", kept, err)
	}
	if !exists(t, filepath.Join(root, "app/a/f")) {
		t.Fatal("the unlisted directory's content was removed")
	}
}

func TestRemoveDirIfListed_LeavesADirectoryWhoseIdentityDiffers(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app", "other")
	writeFiles(t, root, "app/f")
	other := listedUnder(t, filepath.Join(root, "other"))
	all := func(uint64, uint64) bool { return true }

	_, err := RemoveDirIfListed(openTestRoot(t, root), "app", other, all)
	if !errors.Is(err, ErrNotExpected) {
		t.Fatalf("RemoveDirIfListed = %v, want ErrNotExpected", err)
	}
	if !exists(t, filepath.Join(root, "app/f")) {
		t.Fatal("the directory with another identity was changed")
	}
}

func TestRemoveDirIfListed_ReportsAMissingDirectory(t *testing.T) {
	root := t.TempDir()
	all := func(uint64, uint64) bool { return true }
	_, err := RemoveDirIfListed(openTestRoot(t, root), "app", all, all)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("RemoveDirIfListed = %v, want a not-exist error", err)
	}
}

func TestRemoveDirIfListed_DoesNotFollowASubdirectorySwappedForALinkWhileRemoving(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, outside, "f")
	mkdirs(t, root, "app/a")
	listed := listedUnder(t, filepath.Join(root, "app"), "a")
	beforeDescend = func(name string) {
		if name != "a" {
			return
		}
		if err := os.Remove(filepath.Join(root, "app/a")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "app/a")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeDescend = nil })

	_, _ = RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if !exists(t, filepath.Join(outside, "f")) {
		t.Fatal("RemoveDirIfListed followed a link out of the tree")
	}
}

func TestRemoveDirIfListed_StopsAtAnErrorAndRemovesNothingByName(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "app/a")
	writeFiles(t, root, "app/a/f", "app/z")
	listed := listedUnder(t, filepath.Join(root, "app"), "a", "a/f", "z")
	beforeDescend = func(name string) {
		if name == "a" {
			_ = os.Chmod(filepath.Join(root, "app/a"), 0)
		}
	}
	t.Cleanup(func() { beforeDescend = nil })
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "app/a"), 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}

	_, err := RemoveDirIfListed(openTestRoot(t, root), "app", listed, listed)
	if err == nil {
		t.Fatal("RemoveDirIfListed = nil, want the error from the unreadable directory")
	}
	if !exists(t, filepath.Join(root, "app")) {
		t.Fatal("the directory was removed after an error")
	}
}
