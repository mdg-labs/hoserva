package share

import (
	"errors"
	"os"
	"path/filepath"
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
