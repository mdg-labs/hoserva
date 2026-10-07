package container

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func appdataTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeTree(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveAppdataDirs_RemovesANestedTreeCompletely(t *testing.T) {
	root := appdataTestRoot(t)
	writeTree(t, root, "app/config/a", "app/config/sub/deep/b", "app/other/c")
	plan := []string{filepath.Join(root, "app/config")}

	deleted, err := removeAppdataDirs(context.Background(), []string{root}, plan)
	if err != nil {
		t.Fatalf("removeAppdataDirs = %v", err)
	}
	if !reflect.DeepEqual(deleted, plan) {
		t.Fatalf("deleted = %v, want %v", deleted, plan)
	}
	if exists(t, plan[0]) {
		t.Error("the planned tree is still there")
	}
	if !exists(t, filepath.Join(root, "app/other/c")) {
		t.Error("a sibling of the planned tree was removed")
	}
}

func TestRemoveAppdataDirs_SkipsAPathThatIsAlreadyGone(t *testing.T) {
	root := appdataTestRoot(t)
	writeTree(t, root, "app/data/a")
	plan := []string{filepath.Join(root, "app/gone"), filepath.Join(root, "app/data")}

	deleted, err := removeAppdataDirs(context.Background(), []string{root}, plan)
	if err != nil {
		t.Fatalf("removeAppdataDirs = %v", err)
	}
	if want := []string{plan[1]}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
}

func TestRemoveAppdataDirs_RefusesAParentReplacedByALinkAfterPlanning(t *testing.T) {
	root := appdataTestRoot(t)
	outside := appdataTestRoot(t)
	writeTree(t, root, "app/config/a")
	writeTree(t, outside, "config/precious")
	plan := []string{filepath.Join(root, "app/config")}

	beforeAppdataRemove = func(string) {
		if err := os.Rename(filepath.Join(root, "app"), filepath.Join(root, "app.moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "app")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAppdataRemove = nil })

	deleted, err := removeAppdataDirs(context.Background(), []string{root}, plan)
	if err == nil {
		t.Fatal("removeAppdataDirs followed a link that replaced a parent")
	}
	if len(deleted) != 0 {
		t.Errorf("deleted = %v, want none", deleted)
	}
	if !exists(t, filepath.Join(outside, "config/precious")) {
		t.Error("the tree outside the appdata location was removed")
	}
}

func TestRemoveAppdataDirs_RefusesAPathOutsideEveryRoot(t *testing.T) {
	root := appdataTestRoot(t)
	other := appdataTestRoot(t)
	writeTree(t, other, "app/config/a")

	deleted, err := removeAppdataDirs(context.Background(), []string{root}, []string{filepath.Join(other, "app/config")})
	if err == nil || len(deleted) != 0 {
		t.Fatalf("removeAppdataDirs = %v, %v, want a refusal", deleted, err)
	}
	if !exists(t, filepath.Join(other, "app/config/a")) {
		t.Error("a tree outside the appdata location was removed")
	}
}

func TestRemoveAppdataDirs_RefusesARootReplacedByALinkAfterItWasResolved(t *testing.T) {
	disk := appdataTestRoot(t)
	outside := appdataTestRoot(t)
	root := filepath.Join(disk, "appdata")
	writeTree(t, root, "app/config/a")
	writeTree(t, outside, "app/config/precious")
	plan := []string{filepath.Join(root, "app/config")}

	beforeAppdataRootOpen = func(string) {
		if err := os.Rename(root, root+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAppdataRootOpen = nil })

	deleted, err := removeAppdataDirs(context.Background(), []string{root}, plan)
	if err == nil {
		t.Fatal("removeAppdataDirs followed a link that replaced the appdata location")
	}
	if len(deleted) != 0 {
		t.Errorf("deleted = %v, want none", deleted)
	}
	if !exists(t, filepath.Join(outside, "app/config/precious")) {
		t.Error("the tree outside the appdata location was removed")
	}
}
