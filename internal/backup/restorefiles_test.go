package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type restoreEnv struct {
	root  string
	paths Paths
}

// newRestoreEnv lays out the three runtime directories the way production
// does: the config root under one parent, templates and stacks under
// another, with appdata beside them, none of which a restore may touch.
func newRestoreEnv(t *testing.T) restoreEnv {
	t.Helper()
	root := t.TempDir()
	e := restoreEnv{root: root, paths: Paths{
		ConfigRoot:   filepath.Join(root, "etc", "hoserva"),
		TemplatesDir: filepath.Join(root, "state", "templates"),
		StacksDir:    filepath.Join(root, "state", "stacks"),
	}}
	for _, d := range []string{filepath.Dir(e.paths.ConfigRoot), filepath.Dir(e.paths.TemplatesDir), filepath.Join(root, "appdata")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func putFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// filesTree writes a verified-archive tree: the files, keyed by their
// archive path, and a manifest checksumming each.
func filesTree(t *testing.T, files map[string]string) string {
	t.Helper()
	tree := t.TempDir()
	sums := map[string]string{}
	for rel, body := range files {
		sum := sha256.Sum256([]byte(body))
		sums[rel] = hex.EncodeToString(sum[:])
	}
	putFiles(t, tree, files)
	if err := writeManifest(filepath.Join(tree, "manifest.json"), buildManifest("host", "1", time.Now(), sums)); err != nil {
		t.Fatal(err)
	}
	return tree
}

// treeSnapshot records every entry under dir: its kind, mode and content, or a
// symbolic link's target.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link->" + target
		case fi.IsDir():
			out[rel] = fmt.Sprintf("dir %o", fi.Mode().Perm())
		default:
			b, _ := os.ReadFile(p)
			out[rel] = fmt.Sprintf("file %o %s", fi.Mode().Perm(), b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertTreeUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the tree changed:\nbefore %v\nafter  %v", before, after)
	}
}

func slurp(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// The live files and the archive of most tests: each category has a file
// that differs, one only in the archive and one only live, and the live
// side holds files a restore must never touch.
var (
	seedCustom = map[string]string{
		"smb.custom.conf":  "old smb custom",
		"gone.custom.conf": "only live",
		"secret.key":       "not a custom file",
		"snapraid.conf":    "generated, regenerated later",
	}
	seedTemplates = map[string]string{
		"app/template.json": "old template",
		"stale.json":        "only live",
	}
	seedStacks = map[string]string{
		"web/docker-compose.yml": "old compose",
		"web/meta.json":          "old meta",
		"web/.env":               "SECRET=1",
		"old/compose.yaml":       "only live",
		"old/notes.txt":          "not a stack file",
		"loose.txt":              "not in a stack",
	}
	archivedContent = map[string]string{
		"state.db":                      "database",
		"generated/smb.conf":            "generated content that is never restored",
		"custom/smb.custom.conf":        "archived smb custom",
		"custom/sub/new.custom.conf":    "new nested",
		"templates/app/template.json":   "archived template",
		"templates/added/other.json":    "added template",
		"stacks/web/docker-compose.yml": "archived compose",
		"stacks/web/meta.json":          "old meta",
		"stacks/new/compose.yml":        "new stack",
	}
)

func seedRuntime(t *testing.T, e restoreEnv) {
	t.Helper()
	putFiles(t, e.paths.ConfigRoot, seedCustom)
	putFiles(t, e.paths.TemplatesDir, seedTemplates)
	putFiles(t, e.paths.StacksDir, seedStacks)
	putFiles(t, e.root, map[string]string{"appdata/db/data": "appdata"})
}

func TestRestoreFiles_EachCategoryMatchesTheArchive(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	appdataBefore := treeSnapshot(t, filepath.Join(e.root, "appdata"))

	changes, err := RestoreFiles(context.Background(), tree, e.paths)
	if err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}

	want := []FileChanges{
		{Category: FilesCustomConfig, Replaced: []string{"smb.custom.conf"}, Added: []string{"sub/new.custom.conf"}, Removed: []string{"gone.custom.conf"}},
		{Category: FilesTemplates, Replaced: []string{"app/template.json"}, Added: []string{"added/other.json"}, Removed: []string{"stale.json"}},
		{Category: FilesStacks, Replaced: []string{"web/docker-compose.yml"}, Added: []string{"new/compose.yml"}, Removed: []string{"old/compose.yaml"}},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %+v\nwant     %+v", changes, want)
	}

	cfg := e.paths.ConfigRoot
	if got := slurp(t, filepath.Join(cfg, "smb.custom.conf")); got != "archived smb custom" {
		t.Errorf("smb.custom.conf = %q", got)
	}
	if got := slurp(t, filepath.Join(cfg, "sub", "new.custom.conf")); got != "new nested" {
		t.Errorf("sub/new.custom.conf = %q", got)
	}
	if pathExists(filepath.Join(cfg, "gone.custom.conf")) {
		t.Error("a *.custom.conf the archive lacks was kept")
	}
	if slurp(t, filepath.Join(cfg, "secret.key")) != "not a custom file" || slurp(t, filepath.Join(cfg, "snapraid.conf")) != "generated, regenerated later" {
		t.Error("a file under ConfigRoot that is not *.custom.conf was changed")
	}
	if pathExists(filepath.Join(cfg, "smb.conf")) {
		t.Error("the archive's generated/ was copied into ConfigRoot")
	}

	tpl := e.paths.TemplatesDir
	if slurp(t, filepath.Join(tpl, "app", "template.json")) != "archived template" || slurp(t, filepath.Join(tpl, "added", "other.json")) != "added template" || pathExists(filepath.Join(tpl, "stale.json")) {
		t.Errorf("templates do not match the archive: %v", treeSnapshot(t, tpl))
	}

	stk := e.paths.StacksDir
	if slurp(t, filepath.Join(stk, "web", "docker-compose.yml")) != "archived compose" || slurp(t, filepath.Join(stk, "new", "compose.yml")) != "new stack" {
		t.Errorf("stacks do not match the archive: %v", treeSnapshot(t, stk))
	}
	if pathExists(filepath.Join(stk, "old", "compose.yaml")) {
		t.Error("a compose file the archive lacks was kept")
	}
	if slurp(t, filepath.Join(stk, "web", ".env")) != "SECRET=1" || slurp(t, filepath.Join(stk, "old", "notes.txt")) != "not a stack file" || slurp(t, filepath.Join(stk, "loose.txt")) != "not in a stack" {
		t.Errorf("a file the restore must not touch changed: %v", treeSnapshot(t, stk))
	}
	assertTreeUnchanged(t, appdataBefore, treeSnapshot(t, filepath.Join(e.root, "appdata")))

	for _, parent := range []string{filepath.Dir(cfg), filepath.Dir(tpl)} {
		entries, _ := os.ReadDir(parent)
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), ".hoserva-restore-") {
				t.Errorf("staging directory %s left behind", en.Name())
			}
		}
	}

	again, err := RestoreFiles(context.Background(), tree, e.paths)
	if err != nil {
		t.Fatalf("second RestoreFiles: %v", err)
	}
	for _, c := range again {
		if !c.empty() {
			t.Errorf("a restore of an already restored archive still reports %+v", c)
		}
	}
}

func TestRestoreFiles_ReplacedFileKeepsItsMode(t *testing.T) {
	e := newRestoreEnv(t)
	putFiles(t, e.paths.ConfigRoot, map[string]string{"smb.custom.conf": "old"})
	if err := os.Chmod(filepath.Join(e.paths.ConfigRoot, "smb.custom.conf"), 0o640); err != nil {
		t.Fatal(err)
	}
	tree := filesTree(t, map[string]string{"custom/smb.custom.conf": "new"})
	if _, err := RestoreFiles(context.Background(), tree, e.paths); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(e.paths.ConfigRoot, "smb.custom.conf"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v (%v), want 0640", fi.Mode().Perm(), err)
	}
}

// The data-loss scenario for staging: a write error partway through
// copying the archive's files. Nothing may have changed — not a runtime
// directory, and not the directories beside them the staging used.
func TestStageFiles_AFailureWhileStagingChangesNothing(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	before := treeSnapshot(t, e.root)

	calls := 0
	injected := errors.New("injected write error")
	staged, err := StageFiles(context.Background(), tree, e.paths, func(c *filesConfig) {
		c.beforeStage = func(string) error {
			calls++
			if calls == 5 {
				return injected
			}
			return nil
		}
	})
	if !errors.Is(err, injected) || staged != nil {
		t.Fatalf("StageFiles = %v, %v, want the injected error", staged, err)
	}
	if calls < 5 {
		t.Fatalf("the failure was injected after only %d files, before any category was staged", calls)
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
}

func TestStageFiles_StagedButNotAppliedChangesNothingAndDiscardCleansUp(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	dirs := []string{e.paths.ConfigRoot, e.paths.TemplatesDir, e.paths.StacksDir}
	var before []map[string]string
	for _, d := range dirs {
		before = append(before, treeSnapshot(t, d))
	}

	staged, err := StageFiles(context.Background(), tree, e.paths)
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range dirs {
		assertTreeUnchanged(t, before[i], treeSnapshot(t, d))
	}
	if got := staged.Changes(); len(got) != 3 || got[0].Category != FilesCustomConfig {
		t.Fatalf("Changes() = %+v", got)
	}
	staged.Discard()
	staged.Discard()
	if entries, _ := filepath.Glob(filepath.Join(e.root, "*", ".hoserva-restore-*")); len(entries) != 0 {
		t.Fatalf("Discard left %v", entries)
	}
}

// entirely reports whether every file of every entry in want holds its
// wanted content, and none of the paths in gone pathExists.
func assertFilesHold(t *testing.T, dir string, want map[string]string, gone ...string) {
	t.Helper()
	for rel, body := range want {
		if got := slurp(t, filepath.Join(dir, filepath.FromSlash(rel))); got != body {
			t.Errorf("%s = %q, want %q", rel, got, body)
		}
	}
	for _, rel := range gone {
		if pathExists(filepath.Join(dir, filepath.FromSlash(rel))) {
			t.Errorf("%s pathExists", rel)
		}
	}
}

// The data-loss scenario for applying: a failure after the first category
// has been swapped. Every category ends wholly as it was or wholly as in
// the archive, and the error says which.
func TestApply_AFailureAfterTheFirstCategoryLeavesEveryCategoryWhollyOldOrNew(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	injected := errors.New("injected failure")

	staged, err := StageFiles(context.Background(), tree, e.paths, func(c *filesConfig) {
		c.afterStep = func(category string, step int) error {
			if category == FilesTemplates && step == 0 {
				return injected
			}
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	err = staged.Apply()

	var ae *ApplyError
	if !errors.As(err, &ae) || !errors.Is(err, injected) {
		t.Fatalf("Apply = %v, want an *ApplyError wrapping the injected failure", err)
	}
	if !slices.Equal(ae.Restored, []string{FilesCustomConfig}) || !slices.Equal(ae.Unchanged, []string{FilesTemplates, FilesStacks}) || len(ae.Indeterminate) != 0 {
		t.Fatalf("ApplyError = %+v", ae)
	}
	for _, want := range []string{"restored: custom config files", "left as they were: app templates, app stacks"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}

	assertFilesHold(t, e.paths.ConfigRoot, map[string]string{"smb.custom.conf": "archived smb custom", "sub/new.custom.conf": "new nested", "secret.key": "not a custom file"}, "gone.custom.conf")
	assertFilesHold(t, e.paths.TemplatesDir, seedTemplates)
	assertFilesHold(t, e.paths.StacksDir, seedStacks, "new/compose.yml")
	if got := treeSnapshot(t, e.paths.TemplatesDir); len(got) != 4 { // the dir, app/, and two files
		t.Errorf("templates = %v, want exactly the old ones", got)
	}
}

func TestApply_AFailureInsideACategoryPutsEveryFileOfItBack(t *testing.T) {
	for _, category := range filesCategories {
		for _, step := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/step%d", category, step), func(t *testing.T) {
				e := newRestoreEnv(t)
				seedRuntime(t, e)
				tree := filesTree(t, archivedContent)
				before := treeSnapshot(t, e.root)

				staged, err := StageFiles(context.Background(), tree, e.paths, func(c *filesConfig) {
					c.afterStep = func(cat string, n int) error {
						if cat == category && n == step {
							return errors.New("injected failure")
						}
						return nil
					}
				})
				if err != nil {
					t.Fatal(err)
				}
				err = staged.Apply()
				staged.Discard()
				var ae *ApplyError
				if !errors.As(err, &ae) || len(ae.Indeterminate) != 0 || !slices.Contains(ae.Unchanged, category) {
					t.Fatalf("Apply = %v, want the category left as it was", err)
				}

				after := treeSnapshot(t, e.root)
				dir := map[string]string{FilesCustomConfig: "etc", FilesTemplates: "state/templates", FilesStacks: "state/stacks"}[category]
				for k, v := range before {
					if strings.HasPrefix(k, filepath.FromSlash(dir)) && after[k] != v {
						t.Errorf("%s = %q after the failed apply, was %q", k, after[k], v)
					}
				}
				for k := range after {
					if strings.HasPrefix(k, filepath.FromSlash(dir)) && before[k] == "" {
						t.Errorf("%s appeared and stayed", k)
					}
				}
			})
		}
	}
}

func TestApply_APutBackThatFailsKeepsTheSavedFilesAndSaysWhere(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)

	staged, err := StageFiles(context.Background(), tree, e.paths, func(c *filesConfig) {
		c.afterStep = func(cat string, n int) error {
			if cat != FilesCustomConfig || n != 1 {
				return nil
			}
			saved, _ := filepath.Glob(filepath.Join(filepath.Dir(e.paths.ConfigRoot), ".hoserva-restore-*", "old", "*"))
			for _, p := range saved {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			return errors.New("injected failure")
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	err = staged.Apply()
	staged.Discard()

	var ae *ApplyError
	if !errors.As(err, &ae) || !slices.Equal(ae.Indeterminate, []string{FilesCustomConfig}) || len(ae.SavedAt) != 1 {
		t.Fatalf("Apply = %v, want the custom category indeterminate with a place the old files are", err)
	}
	if !strings.Contains(err.Error(), ae.SavedAt[0]) {
		t.Errorf("the error %q does not name %s", err, ae.SavedAt[0])
	}
	if !pathExists(ae.SavedAt[0]) {
		t.Error("Discard removed the directory the old files are in")
	}
}

// The data-loss scenario for symbolic links: a *.custom.conf that is a link
// to a file outside ConfigRoot. Restoring must refuse it, not write through.
func TestRestoreFiles_RefusesToFollowASymbolicLink(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "target.conf")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, setup := range map[string]func(t *testing.T, e restoreEnv){
		"a custom file that is a link": func(t *testing.T, e restoreEnv) {
			if err := os.Symlink(secret, filepath.Join(e.paths.ConfigRoot, "smb.custom.conf")); err != nil {
				t.Fatal(err)
			}
		},
		"a custom file the archive lacks that is a link": func(t *testing.T, e restoreEnv) {
			if err := os.Symlink(secret, filepath.Join(e.paths.ConfigRoot, "other.custom.conf")); err != nil {
				t.Fatal(err)
			}
		},
		"a directory on the way that is a link": func(t *testing.T, e restoreEnv) {
			if err := os.Symlink(outside, filepath.Join(e.paths.ConfigRoot, "sub")); err != nil {
				t.Fatal(err)
			}
		},
		"a stack directory that is a link": func(t *testing.T, e restoreEnv) {
			if err := os.MkdirAll(e.paths.StacksDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(e.paths.StacksDir, "web")); err != nil {
				t.Fatal(err)
			}
		},
		"a stack file that is a link": func(t *testing.T, e restoreEnv) {
			if err := os.MkdirAll(filepath.Join(e.paths.StacksDir, "web"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, filepath.Join(e.paths.StacksDir, "web", "docker-compose.yml")); err != nil {
				t.Fatal(err)
			}
		},
		"the templates directory is a link": func(t *testing.T, e restoreEnv) {
			if err := os.Symlink(outside, e.paths.TemplatesDir); err != nil {
				t.Fatal(err)
			}
		},
		"the config root is a link": func(t *testing.T, e restoreEnv) {
			if err := os.RemoveAll(e.paths.ConfigRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, e.paths.ConfigRoot); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRestoreEnv(t)
			if err := os.MkdirAll(e.paths.ConfigRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			setup(t, e)
			tree := filesTree(t, archivedContent)
			before, outsideBefore := treeSnapshot(t, e.root), treeSnapshot(t, outside)

			if _, err := PlanFiles(tree, e.paths); !isUnsafePath(err) {
				t.Fatalf("PlanFiles = %v, want an *UnsafeRestorePathError", err)
			}
			if _, err := RestoreFiles(context.Background(), tree, e.paths); !isUnsafePath(err) {
				t.Fatalf("RestoreFiles = %v, want an *UnsafeRestorePathError", err)
			}
			assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
			assertTreeUnchanged(t, outsideBefore, treeSnapshot(t, outside))
			if got := slurp(t, secret); got != "outside" {
				t.Fatalf("the file outside was written: %q", got)
			}
		})
	}
}

func isUnsafePath(err error) bool {
	var u *UnsafeRestorePathError
	return errors.As(err, &u)
}

func TestRestoreFiles_RefusesAnArchivePathThatIsNotOneOfItsFiles(t *testing.T) {
	for name, key := range map[string]string{
		"a custom file that is not *.custom.conf": "custom/secret.key",
		"a path that leaves the archive":          "custom/../state.db",
		"a stack file of another name":            "stacks/web/.env",
		"a stack file too deep":                   "stacks/web/deep/compose.yml",
		"a file directly in stacks":               "stacks/compose.yml",
	} {
		t.Run(name, func(t *testing.T) {
			e := newRestoreEnv(t)
			seedRuntime(t, e)
			tree := filesTree(t, map[string]string{"state.db": "x"})
			if err := os.MkdirAll(filepath.Join(tree, "stacks", "web", "deep"), 0o755); err != nil {
				t.Fatal(err)
			}
			manifest, err := readManifest(filepath.Join(tree, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			manifest.Checksums[key] = "00"
			if err := writeManifest(filepath.Join(tree, "manifest.json"), manifest); err != nil {
				t.Fatal(err)
			}
			before := treeSnapshot(t, e.root)
			if _, err := RestoreFiles(context.Background(), tree, e.paths); !isUnsafePath(err) {
				t.Fatalf("RestoreFiles = %v, want an *UnsafeRestorePathError", err)
			}
			assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
		})
	}
}

// A file changed in the extracted tree between verification and restore is
// never written, and neither is one the manifest does not list.
func TestStageFiles_OnlyWhatTheManifestChecksIsWritten(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	before := treeSnapshot(t, e.root)

	if err := os.WriteFile(filepath.Join(tree, "templates", "added", "other.json"), []byte("changed after verification"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StageFiles(context.Background(), tree, e.paths); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("StageFiles = %v, want a manifest mismatch", err)
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))

	if err := os.WriteFile(filepath.Join(tree, "templates", "added", "other.json"), []byte("added template"), 0o600); err != nil {
		t.Fatal(err)
	}
	putFiles(t, tree, map[string]string{"custom/unlisted.custom.conf": "not in the manifest", "stacks/web/compose.yml": "not in the manifest"})
	if _, err := RestoreFiles(context.Background(), tree, e.paths); err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if pathExists(filepath.Join(e.paths.ConfigRoot, "unlisted.custom.conf")) || pathExists(filepath.Join(e.paths.StacksDir, "web", "compose.yml")) {
		t.Fatal("a file the manifest does not list was restored")
	}
}

func TestRestoreFiles_UnconfiguredDirectoriesAreLeftAlone(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	changes, err := RestoreFiles(context.Background(), tree, Paths{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if !c.empty() {
			t.Errorf("%+v", c)
		}
	}
}

func TestRestoreFiles_ARestoreOntoAFreshSystemCreatesTheDirectories(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		ConfigRoot:   filepath.Join(root, "etc", "hoserva"),
		TemplatesDir: filepath.Join(root, "state", "templates"),
		StacksDir:    filepath.Join(root, "state", "stacks"),
	}
	for _, d := range []string{"etc", "state"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tree := filesTree(t, archivedContent)
	if _, err := RestoreFiles(context.Background(), tree, paths); err != nil {
		t.Fatal(err)
	}
	assertFilesHold(t, paths.ConfigRoot, map[string]string{"smb.custom.conf": "archived smb custom", "sub/new.custom.conf": "new nested"})
	assertFilesHold(t, paths.TemplatesDir, map[string]string{"app/template.json": "archived template"})
	assertFilesHold(t, paths.StacksDir, map[string]string{"web/docker-compose.yml": "archived compose", "new/compose.yml": "new stack"})
}

func TestApply_DoesNotObserveCancellation(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	ctx, cancel := context.WithCancel(context.Background())
	staged, err := StageFiles(ctx, tree, e.paths)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	cancel()
	if err := staged.Apply(); err != nil {
		t.Fatalf("Apply after the request was cancelled: %v", err)
	}
	assertFilesHold(t, e.paths.ConfigRoot, map[string]string{"smb.custom.conf": "archived smb custom"})
}

func TestStageFiles_ACancelledContextStagesNothing(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	before := treeSnapshot(t, e.root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := StageFiles(ctx, tree, e.paths); !errors.Is(err, context.Canceled) {
		t.Fatalf("StageFiles = %v", err)
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
}

func TestExtractVerifiedArchive(t *testing.T) {
	staging := t.TempDir()
	db := openMigratedDB(t, filepath.Join(staging, "state.db"))
	_ = db.Close()
	putFiles(t, staging, map[string]string{"custom/a.custom.conf": "a"})
	sums := map[string]string{}
	for _, rel := range []string{"state.db", "custom/a.custom.conf"} {
		sum, err := hashFile(filepath.Join(staging, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		sums[rel] = sum
	}
	if err := writeManifest(filepath.Join(staging, "manifest.json"), buildManifest("h", "1", time.Now(), sums)); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "a.tar.zst")
	if err := packArchive(staging, archive); err != nil {
		t.Fatal(err)
	}

	tree, err := ExtractVerifiedArchive(archive)
	if err != nil {
		t.Fatalf("ExtractVerifiedArchive: %v", err)
	}
	defer func() { _ = os.RemoveAll(tree) }()
	if got := slurp(t, filepath.Join(tree, "custom", "a.custom.conf")); got != "a" {
		t.Fatalf("extracted file = %q", got)
	}

	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if err := os.WriteFile(filepath.Join(staging, "custom", "a.custom.conf"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.tar.zst")
	if err := packArchive(staging, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractVerifiedArchive(bad); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("ExtractVerifiedArchive of a tampered archive = %v", err)
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Fatalf("a failed extraction left %d entries behind", len(entries))
	}
}

// stageArchiveFiles adds files to a staged preview archive and lists them,
// with their checksums, in its manifest.
func stageArchiveFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	putFiles(t, dir, files)
	m, err := readManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		sum := sha256.Sum256([]byte(body))
		m.Checksums[rel] = hex.EncodeToString(sum[:])
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), m); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewImport_ListsWhatRestoringTheFilesWouldReplaceAddAndRemove(t *testing.T) {
	ctx := context.Background()
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arcPath := filepath.Join(t.TempDir(), "state.db")
	dir := stagePreviewArchive(t, openMigratedDB(t, arcPath), arcPath)
	stageArchiveFiles(t, dir, map[string]string{
		"custom/smb.custom.conf":        "archived smb custom",
		"templates/added/other.json":    "added template",
		"stacks/web/docker-compose.yml": "archived compose",
	})
	before := treeSnapshot(t, e.root)

	p, err := PreviewImport(ctx, live, e.paths, dir)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(p.Blockers) != 0 {
		t.Fatalf("blockers = %+v", p.Blockers)
	}
	var categories []string
	for _, g := range p.Groups {
		categories = append(categories, g.Category)
	}
	if !slices.Equal(categories, ImportCategories()) {
		t.Fatalf("categories = %v, want %v", categories, ImportCategories())
	}
	custom := groupByCategory(t, p.Groups, FilesCustomConfig)
	if !reflect.DeepEqual(custom.Changed, changesOf("custom_config_file", "smb.custom.conf")) ||
		len(custom.Added) != 0 || !reflect.DeepEqual(custom.Removed, changesOf("custom_config_file", "gone.custom.conf")) {
		t.Errorf("custom config = %+v", custom)
	}
	templates := groupByCategory(t, p.Groups, FilesTemplates)
	if !reflect.DeepEqual(templates.Added, changesOf("template_file", "added/other.json")) ||
		!reflect.DeepEqual(templates.Removed, changesOf("template_file", "app/template.json", "stale.json")) || len(templates.Changed) != 0 {
		t.Errorf("templates = %+v", templates)
	}
	stacks := groupByCategory(t, p.Groups, FilesStacks)
	if !reflect.DeepEqual(stacks.Changed, changesOf("stack_file", "web/docker-compose.yml")) ||
		!reflect.DeepEqual(stacks.Removed, changesOf("stack_file", "old/compose.yaml", "web/meta.json")) || len(stacks.Added) != 0 {
		t.Errorf("stacks = %+v", stacks)
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
}

func TestPreviewImport_APathTheRestoreWouldRefuseIsABlocker(t *testing.T) {
	ctx := context.Background()
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	outside := filepath.Join(t.TempDir(), "outside.conf")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.paths.ConfigRoot, "linked.custom.conf")); err != nil {
		t.Fatal(err)
	}
	live := openMigratedDB(t, filepath.Join(t.TempDir(), "live.db"))
	arcPath := filepath.Join(t.TempDir(), "state.db")
	dir := stagePreviewArchive(t, openMigratedDB(t, arcPath), arcPath)
	stageArchiveFiles(t, dir, map[string]string{"custom/smb.custom.conf": "archived"})

	p, err := PreviewImport(ctx, live, e.paths, dir)
	if err != nil {
		t.Fatalf("PreviewImport: %v", err)
	}
	if len(p.Blockers) != 1 || p.Blockers[0].Code != RefusalUnsafeRestorePath || !strings.Contains(p.Blockers[0].Message, "linked.custom.conf") {
		t.Fatalf("blockers = %+v, want the unsafe-path refusal naming the link", p.Blockers)
	}
	if len(p.Groups) != len(ImportCategories()) {
		t.Fatalf("groups = %d, want every category even when blocked", len(p.Groups))
	}
}
