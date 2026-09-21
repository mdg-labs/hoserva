package share

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/mdg-labs/hoserva/internal/pool"
)

func TestAllowedSharePath_RefusesWrongShareAndEscapes(t *testing.T) {
	disk1 := filepath.Join(t.TempDir(), "disk1")
	media := filepath.Join(disk1, "media")
	appdata := filepath.Join(disk1, "appdata")
	allowed := []string{media}

	if !allowedSharePath(media, allowed) {
		t.Fatal("share's own directory must be allowed")
	}
	if allowedSharePath(appdata, allowed) {
		t.Fatal("a different share's directory must not be allowed")
	}
	escaped := filepath.Join(media, "..", "appdata")
	if allowedSharePath(escaped, allowed) {
		t.Fatalf("cleaned escape %q must not be allowed", escaped)
	}
	parity := filepath.Join(filepath.Dir(disk1), "parity1", "snapraid.parity")
	if allowedSharePath(parity, allowed) {
		t.Fatal("the parity file must not be allowed")
	}
}

func TestShareDataRoots_CacheOnlyOmitsDataDisks(t *testing.T) {
	roots, err := shareDataRoots("appdata", pool.CacheOnly, []string{"/mnt/disk1", "/mnt/disk2"}, "/mnt/cache")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || roots[0] != filepath.Join("/mnt/cache", "appdata") {
		t.Fatalf("cache-only roots = %v, want only cache", roots)
	}
}

func TestShareDataRoots_ArrayOnlyOmitsCache(t *testing.T) {
	roots, err := shareDataRoots("media", pool.ArrayOnly, []string{"/mnt/disk1", "/mnt/disk2"}, "/mnt/cache")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("array-only roots = %v", roots)
	}
	for _, r := range roots {
		if r == filepath.Join("/mnt/cache", "media") {
			t.Fatal("array-only roots must not include cache")
		}
	}
}

func TestConfineSharePath_RefusesDotDot(t *testing.T) {
	root := "/mnt/user/media"
	if _, err := confineSharePath(root, "../appdata"); err == nil {
		t.Fatal("expected ErrPathEscapes")
	}
	if _, err := confineSharePath(root, "/mnt/disk1/media"); err == nil {
		t.Fatal("absolute rel path must escape")
	}
	got, err := confineSharePath(root, "shows/../movies")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "movies") {
		t.Fatalf("got %q", got)
	}
}

func TestConfineSharePathOnFS_RefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	if _, err := confineSharePathOnFS(OSFS{}, root, "external"); !errors.Is(err, ErrPathEscapes) {
		t.Fatalf("symlink escape = %v, want ErrPathEscapes", err)
	}
	got, err := confineSharePathOnFS(OSFS{}, root, "")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Fatalf("root listing = %q, want %q", got, resolved)
	}
}

func TestDeleteData_LeavesOtherShareAndParity(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	writeFile(t, filepath.Join(layout.dataDisks[0], "media", "film.mkv"), "movie")
	writeFile(t, filepath.Join(layout.dataDisks[0], "appdata", "db.sqlite"), "keep")
	writeFile(t, filepath.Join(layout.parity, "snapraid.parity"), "parity")
	writeFile(t, filepath.Join(layout.cache, "appdata", "config.xml"), "keep-cache")

	createTestShare(t, svc, "media", pool.ArrayOnly)
	createTestShare(t, svc, "appdata", pool.CacheOnly)

	if err := svc.DeleteData(ctx, "media", "media"); err != nil {
		t.Fatalf("DeleteData(media): %v", err)
	}
	if _, err := os.Stat(filepath.Join(layout.dataDisks[0], "media")); !os.IsNotExist(err) {
		t.Fatalf("media directory still present: %v", err)
	}
	if got := readFile(t, filepath.Join(layout.dataDisks[0], "appdata", "db.sqlite")); got != "keep" {
		t.Fatalf("appdata on data disk = %q", got)
	}
	if got := readFile(t, filepath.Join(layout.parity, "snapraid.parity")); got != "parity" {
		t.Fatalf("parity file = %q", got)
	}
	if got := readFile(t, filepath.Join(layout.cache, "appdata", "config.xml")); got != "keep-cache" {
		t.Fatalf("appdata on cache = %q", got)
	}
}

func TestDeleteData_WrongConfirmationLeavesFiles(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	writeFile(t, filepath.Join(layout.dataDisks[0], "media", "film.mkv"), "movie")
	createTestShare(t, svc, "media", pool.ArrayOnly)

	if err := svc.DeleteData(ctx, "media", "nope"); err == nil {
		t.Fatal("wrong confirmation must be refused")
	}
	if got := readFile(t, filepath.Join(layout.dataDisks[0], "media", "film.mkv")); got != "movie" {
		t.Fatalf("file after refused delete = %q", got)
	}
}

func TestDeleteData_RefusesPathThatIsNotThisShare(t *testing.T) {
	disk1 := t.TempDir()
	media := filepath.Join(disk1, "media")
	appdata := filepath.Join(disk1, "appdata")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	allowed, err := shareDataRoots("media", pool.ArrayOnly, []string{disk1}, "")
	if err != nil {
		t.Fatal(err)
	}
	fs := OSFS{}
	if err := deleteSharePath(fs, appdata, allowed); err == nil {
		t.Fatal("deleting another share's directory must be refused")
	}
	if _, err := os.Stat(appdata); err != nil {
		t.Fatalf("other share directory was removed: %v", err)
	}
}

func TestDeleteFile_DeletesFile(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "film.mkv"), "movie")

	if err := svc.DeleteFile(ctx, "media", "film.mkv", true); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shareRoot, "film.mkv")); !os.IsNotExist(err) {
		t.Fatalf("film.mkv still present: %v", err)
	}
}

func TestDeleteFile_DeletesEmptyDirectory(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	if err := os.MkdirAll(filepath.Join(shareRoot, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteFile(ctx, "media", "empty", true); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shareRoot, "empty")); !os.IsNotExist(err) {
		t.Fatalf("empty directory still present: %v", err)
	}
}

func TestDeleteFile_RefusesNonEmptyDirectory(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "shows", "episode1.mkv"), "ep1")

	if err := svc.DeleteFile(ctx, "media", "shows", true); err == nil {
		t.Fatal("deleting a non-empty directory must be refused")
	}
	if got := readFile(t, filepath.Join(shareRoot, "shows", "episode1.mkv")); got != "ep1" {
		t.Fatalf("episode1.mkv = %q, want untouched", got)
	}
}

func TestDeleteFile_RequiresConfirm(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "film.mkv"), "movie")

	if err := svc.DeleteFile(ctx, "media", "film.mkv", false); !errors.Is(err, ErrConfirmation) {
		t.Fatalf("DeleteFile without confirm = %v, want ErrConfirmation", err)
	}
	if got := readFile(t, filepath.Join(shareRoot, "film.mkv")); got != "movie" {
		t.Fatalf("film.mkv = %q, want untouched", got)
	}
}

func TestDeleteFile_RefusesShareRoot(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "film.mkv"), "movie")

	for _, rel := range []string{"", "."} {
		if err := svc.DeleteFile(ctx, "media", rel, true); !errors.Is(err, ErrPathEscapes) {
			t.Fatalf("DeleteFile(%q) = %v, want ErrPathEscapes", rel, err)
		}
	}
	if _, err := os.Stat(shareRoot); err != nil {
		t.Fatalf("share root was removed: %v", err)
	}
}

func TestDeleteFile_RefusesTraversal(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	createTestShare(t, svc, "appdata", pool.ArrayOnly)
	writeFile(t, filepath.Join(layout.catchAll, "appdata", "db.sqlite"), "keep")

	if err := svc.DeleteFile(ctx, "media", "../appdata/db.sqlite", true); !errors.Is(err, ErrPathEscapes) {
		t.Fatalf("DeleteFile traversal = %v, want ErrPathEscapes", err)
	}
	if got := readFile(t, filepath.Join(layout.catchAll, "appdata", "db.sqlite")); got != "keep" {
		t.Fatalf("appdata/db.sqlite = %q, want untouched", got)
	}
}

func TestDeleteFile_RefusesSymlinkEscape(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	if err := os.MkdirAll(shareRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "no")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(shareRoot, "external")); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteFile(ctx, "media", "external", true); !errors.Is(err, ErrPathEscapes) {
		t.Fatalf("DeleteFile through symlink = %v, want ErrPathEscapes", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "secret")); err != nil {
		t.Fatalf("target outside the share was affected: %v", err)
	}
}

func TestDeleteFile_MissingFileIsRefused(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	if err := os.MkdirAll(filepath.Join(layout.catchAll, "media"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteFile(ctx, "media", "nope.mkv", true); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("DeleteFile missing file = %v, want ErrFileNotFound", err)
	}
}

// TestRemoveConfined_MissingFileUnderSymlinkedRootIsNotFound covers a root
// that is itself a symlink (e.g. a symlinked cache path): confineSharePathOnFS
// returns the missing leaf's path built from the raw root, not the resolved
// one, so removeConfined's own containment check must still recognize it as
// confined instead of misreporting a path escape.
func TestRemoveConfined_MissingFileUnderSymlinkedRootIsNotFound(t *testing.T) {
	real := t.TempDir()
	root := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(real, root); err != nil {
		t.Fatal(err)
	}

	if err := removeConfined(OSFS{}, root, "nope.mkv"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("removeConfined missing file under symlinked root = %v, want ErrFileNotFound", err)
	}
}

// swapParentAfterValidateFS embeds OSFS and, the first time EvalSymlinks is
// asked to resolve trigger, swaps victim for a symlink pointing at swapTo —
// simulating a concurrent writer replacing an in-share parent directory
// with a symlink at the exact moment between DeleteFile's path validation
// and its removal step.
type swapParentAfterValidateFS struct {
	OSFS
	trigger string
	victim  string
	swapTo  string
	swapped bool
}

func (f *swapParentAfterValidateFS) EvalSymlinks(path string) (string, error) {
	resolved, err := f.OSFS.EvalSymlinks(path)
	if !f.swapped && path == f.trigger {
		f.swapped = true
		if rmErr := os.RemoveAll(f.victim); rmErr != nil {
			return resolved, rmErr
		}
		if lnErr := os.Symlink(f.swapTo, f.victim); lnErr != nil {
			return resolved, lnErr
		}
	}
	return resolved, err
}

// RemoveConfined is overridden (rather than inherited from the embedded
// OSFS) so removeConfined's own path resolution dispatches back through
// this fake's EvalSymlinks — and so hits the swap — instead of a fresh,
// unswapped OSFS{}.
func (f *swapParentAfterValidateFS) RemoveConfined(root, rel string) error {
	return removeConfined(f, root, rel)
}

// TestDeleteFile_RefusesParentSwappedForSymlinkMidRace reproduces the
// CWE-367 TOCTOU race CodeRabbit flagged on PR #228: a directory holding
// the delete target is replaced with a symlink pointing outside the share
// after path validation but before the file is actually removed. Against
// the pre-fix implementation (separate confineSharePathOnFS, FS.Lstat,
// FS.Remove calls, each re-resolving the pathname), this swap causes the
// final os.Remove to walk through the new symlink and delete a file
// outside the share entirely. RemoveConfined must instead refuse the
// operation, never touching anything outside the share.
func TestDeleteFile_RefusesParentSwappedForSymlinkMidRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RemoveConfined's race protection is Linux-only (RESOLVE_IN_ROOT); production only runs on Debian (D2)")
	}
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "sub", "file.txt"), "inside")

	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "file.txt"), "outside-secret")

	svc.FS = &swapParentAfterValidateFS{
		trigger: filepath.Join(shareRoot, "sub", "file.txt"),
		victim:  filepath.Join(shareRoot, "sub"),
		swapTo:  outside,
	}

	if err := svc.DeleteFile(ctx, "media", "sub/file.txt", true); err == nil {
		t.Fatal("DeleteFile must refuse a target whose parent directory was swapped for a symlink mid-race")
	}
	if got := readFile(t, filepath.Join(outside, "file.txt")); got != "outside-secret" {
		t.Fatalf("file outside the share was affected by the race: got %q, want untouched", got)
	}
}

// TestDeleteFile_RefusesParentSwappedForInShareSymlinkMidRace is the same
// race with swapTo pointing at a different, legitimate directory inside
// the share rather than outside it. RESOLVE_IN_ROOT alone follows this
// without error, since the reopen never leaves root — it is
// RESOLVE_NO_SYMLINKS that must refuse it, since the swapped parent is now
// a symlink and the reopen fails outright rather than following it into a
// directory the caller never named. (unlinkConfined's identity check runs
// only against the leaf, once the reopen has already succeeded.)
func TestDeleteFile_RefusesParentSwappedForInShareSymlinkMidRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RemoveConfined's race protection is Linux-only (RESOLVE_IN_ROOT); production only runs on Debian (D2)")
	}
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "sub", "file.txt"), "inside")
	writeFile(t, filepath.Join(shareRoot, "other", "file.txt"), "other-data")

	svc.FS = &swapParentAfterValidateFS{
		trigger: filepath.Join(shareRoot, "sub", "file.txt"),
		victim:  filepath.Join(shareRoot, "sub"),
		// A relative target, not the host-absolute shareRoot/other: an
		// absolute swapTo is reinterpreted by RESOLVE_IN_ROOT as rooted at
		// the share (like the outside-the-share case above), so it would
		// not reach another in-share directory at all and this test would
		// pass for the wrong reason. The symlink is created at
		// shareRoot/sub, so it resolves relative to shareRoot itself —
		// "other" reaches shareRoot/other, the directory the assertion
		// below checks.
		swapTo: "other",
	}

	if err := svc.DeleteFile(ctx, "media", "sub/file.txt", true); err == nil {
		t.Fatal("DeleteFile must refuse a target whose parent directory was swapped for a symlink to another in-share directory mid-race")
	}
	if got := readFile(t, filepath.Join(shareRoot, "other", "file.txt")); got != "other-data" {
		t.Fatalf("file in a different in-share directory was affected by the race: got %q, want untouched", got)
	}
}

// TestDeleteFile_RefusesGrandparentSwappedForSymlinkMidRace is the same
// race again, but two levels up: the target's grandparent (not its
// immediate parent) is swapped for a symlink to a different in-share
// directory. RESOLVE_IN_ROOT alone follows this without error, and so
// does a check that only compares the immediate parent's identity against
// a fresh re-resolution of the same pathname, since both walks race the
// same swap identically and land on the same (wrong) directory. Only
// refusing to traverse a symlink at any depth (RESOLVE_NO_SYMLINKS) closes
// this: the reopen must fail outright, regardless of how deep the swapped
// component is.
func TestDeleteFile_RefusesGrandparentSwappedForSymlinkMidRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RemoveConfined's race protection is Linux-only (RESOLVE_IN_ROOT); production only runs on Debian (D2)")
	}
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "a", "b", "file.txt"), "inside")
	writeFile(t, filepath.Join(shareRoot, "other", "b", "file.txt"), "other-data")

	svc.FS = &swapParentAfterValidateFS{
		trigger: filepath.Join(shareRoot, "a", "b", "file.txt"),
		victim:  filepath.Join(shareRoot, "a"),
		// Relative, resolved from victim's own containing directory
		// (shareRoot itself, since victim is shareRoot/a), so this reaches
		// shareRoot/other — a different, legitimate in-share directory —
		// without an outer ".." that a chroot-like confined resolution and
		// an ordinary absolute-path resolution would disagree about.
		swapTo: "other",
	}

	if err := svc.DeleteFile(ctx, "media", "a/b/file.txt", true); err == nil {
		t.Fatal("DeleteFile must refuse a target whose grandparent directory was swapped for a symlink mid-race")
	}
	if got := readFile(t, filepath.Join(shareRoot, "other", "b", "file.txt")); got != "other-data" {
		t.Fatalf("file under a different in-share directory was affected by the race: got %q, want untouched", got)
	}
}

// swapLeafAfterValidateFS embeds OSFS and, the first time Lstat is asked to
// stat trigger, runs swap. removeConfined's own Lstat on the just-resolved
// target is the first (and, for an existing target, only) Lstat call it
// makes, so this fires at exactly the point where the leaf's identity is
// captured for later comparison — simulating a concurrent writer replacing
// the delete target itself, in an otherwise untouched parent directory,
// in the race window between that capture and unlinkConfined's removal.
type swapLeafAfterValidateFS struct {
	OSFS
	trigger string
	swap    func() error
	swapped bool
}

func (f *swapLeafAfterValidateFS) Lstat(path string) (os.FileInfo, error) {
	info, err := f.OSFS.Lstat(path)
	if !f.swapped && path == f.trigger {
		f.swapped = true
		if serr := f.swap(); serr != nil {
			return info, serr
		}
	}
	return info, err
}

// RemoveConfined is overridden (rather than inherited from the embedded
// OSFS) so removeConfined's own path resolution dispatches back through
// this fake's Lstat — and so hits the swap — instead of a fresh, unswapped
// OSFS{}.
func (f *swapLeafAfterValidateFS) RemoveConfined(root, rel string) error {
	return removeConfined(f, root, rel)
}

// TestDeleteFile_RefusesLeafSwappedForDifferentFileMidRace covers the gap
// neither RESOLVE_IN_ROOT nor RESOLVE_NO_SYMLINKS closes: the parent
// directory is never touched, but the target itself — a file an attacker
// has legitimate write access to, in a directory it also has legitimate
// write access to — is deleted and replaced with a different file of the
// same name between validation and removal. Nothing about the directory
// chain changes, so only a check of the leaf's own identity, captured
// before the swap, can refuse this.
func TestDeleteFile_RefusesLeafSwappedForDifferentFileMidRace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RemoveConfined's race protection is Linux-only (RESOLVE_IN_ROOT); production only runs on Debian (D2)")
	}
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	target := filepath.Join(shareRoot, "sub", "file.txt")
	writeFile(t, target, "original")

	// The replacement file is created before the swap fires, while
	// "original" still occupies its inode, so the allocator cannot hand
	// the replacement the same inode number — swapping it in with Rename
	// guarantees a different identity deterministically, rather than
	// hoping a remove-then-create at the same path happens not to reuse
	// the just-freed inode (filesystem-dependent, and not guaranteed).
	replacement := target + ".swapped"
	writeFile(t, replacement, "swapped-in")
	svc.FS = &swapLeafAfterValidateFS{
		trigger: target,
		swap: func() error {
			return os.Rename(replacement, target)
		},
	}

	if err := svc.DeleteFile(ctx, "media", "sub/file.txt", true); err == nil {
		t.Fatal("DeleteFile must refuse a target whose leaf was swapped for a different file mid-race")
	}
	if got := readFile(t, target); got != "swapped-in" {
		t.Fatalf("swapped-in file was affected by the refused delete: got %q, want untouched", got)
	}
}

func TestDeleteFile_AllowsSymlinkResolvingInsideShare(t *testing.T) {
	ctx, svc, layout, _ := testService(t)
	createTestShare(t, svc, "media", pool.ArrayOnly)
	shareRoot := filepath.Join(layout.catchAll, "media")
	writeFile(t, filepath.Join(shareRoot, "real.mkv"), "movie")
	if err := os.Symlink(filepath.Join(shareRoot, "real.mkv"), filepath.Join(shareRoot, "link.mkv")); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteFile(ctx, "media", "link.mkv", true); err != nil {
		t.Fatalf("DeleteFile(link.mkv): %v", err)
	}
	if _, err := os.Stat(filepath.Join(shareRoot, "real.mkv")); !os.IsNotExist(err) {
		t.Fatalf("target of an in-share symlink was not removed: %v", err)
	}
}
