package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const testBackupPassphrase = "correct horse"

// secretsTree is filesTree plus a secrets.age holding the given stack .env
// bodies, sealed under passphrase the way BuildArchive seals it.
func secretsTree(t *testing.T, files map[string]string, passphrase string, envs map[string]string) string {
	t.Helper()
	tree := filesTree(t, files)
	var stackEnvs []StackEnv
	for stack, body := range envs {
		stackEnvs = append(stackEnvs, StackEnv{Stack: stack, Body: []byte(body)})
	}
	sealed, err := buildSecretsAge(context.Background(), &FakeSecretSource{Passphrase: passphrase, HasPass: true}, FakeSecretCipher{}, stackEnvs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "secrets.age"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return tree
}

func envsOf(m map[string]string) []StackEnv {
	var out []StackEnv
	for stack, body := range m {
		out = append(out, StackEnv{Stack: stack, Body: []byte(body)})
	}
	slices.SortFunc(out, func(a, b StackEnv) int { return strings.Compare(a.Stack, b.Stack) })
	return out
}

func TestReadSecrets(t *testing.T) {
	tree := secretsTree(t, archivedContent, testBackupPassphrase, map[string]string{"web": "SECRET=2"})

	s, err := ReadSecrets(tree, testBackupPassphrase)
	if err != nil {
		t.Fatalf("ReadSecrets: %v", err)
	}
	if got := s.StackEnvs(); len(got) != 1 || got[0].Stack != "web" || string(got[0].Body) != "SECRET=2" {
		t.Fatalf("StackEnvs = %+v", got)
	}

	if _, err := ReadSecrets(tree, "wrong"); !errors.Is(err, ErrPassphraseIncorrect) {
		t.Fatalf("ReadSecrets(wrong passphrase) = %v, want ErrPassphraseIncorrect", err)
	}
	if _, err := ReadSecrets(filesTree(t, archivedContent), testBackupPassphrase); !errors.Is(err, ErrNoSecrets) {
		t.Fatalf("ReadSecrets(no secrets.age) = %v, want ErrNoSecrets", err)
	}

	if err := os.WriteFile(filepath.Join(tree, "secrets.age"), []byte("not an age file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ReadSecrets(tree, testBackupPassphrase)
	if !errors.Is(err, ErrSecretsUnreadable) || errors.Is(err, ErrPassphraseIncorrect) {
		t.Fatalf("ReadSecrets(corrupt secrets.age) = %v, want ErrSecretsUnreadable", err)
	}
}

func TestRestoreFiles_StackEnvsAreWrittenForTheArchivesStacksOnly(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	putFiles(t, e.paths.StacksDir, map[string]string{"old/.env": "KEEP=1"})
	tree := filesTree(t, archivedContent)

	staged, err := StageFiles(context.Background(), tree, e.paths, WithStackEnvs(envsOf(map[string]string{
		"web":   "SECRET=2",
		"new":   "NEW=1",
		"ghost": "NOT_IN_ARCHIVE=1",
	})))
	if err != nil {
		t.Fatalf("StageFiles: %v", err)
	}
	if err := staged.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	staged.Discard()

	stacks := e.paths.StacksDir
	for rel, want := range map[string]string{"web/.env": "SECRET=2", "new/.env": "NEW=1", "old/.env": "KEEP=1"} {
		if got := slurp(t, filepath.Join(stacks, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{"web/.env", "new/.env"} {
		fi, err := os.Stat(filepath.Join(stacks, rel))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", rel, fi.Mode().Perm())
		}
	}
	if pathExists(filepath.Join(stacks, "ghost")) {
		t.Error("an .env for a stack the archive does not contain created a directory")
	}
	if pathExists(filepath.Join(stacks, "old", "compose.yaml")) {
		t.Error("the compose file of a stack the archive lacks was kept")
	}

	want := []FileChanges{
		{Category: FilesCustomConfig, Replaced: []string{}, Added: []string{}, Removed: []string{}},
		{Category: FilesTemplates, Replaced: []string{}, Added: []string{}, Removed: []string{}},
		{Category: FilesStacks, Replaced: []string{"web/docker-compose.yml"}, Added: []string{"new/compose.yml"}, Removed: []string{"old/compose.yaml"}},
	}
	got := staged.Changes()
	for i := range got[:2] {
		got[i] = FileChanges{Category: got[i].Category, Replaced: []string{}, Added: []string{}, Removed: []string{}}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Changes = %+v\nwant      %+v", got, want)
	}
	env := staged.StackEnvChanges()
	if !slices.Equal(env.Added, []string{"new"}) || !slices.Equal(env.Replaced, []string{"web"}) {
		t.Fatalf("StackEnvChanges = %+v", env)
	}
}

func TestRestoreFiles_AnIdenticalStackEnvIsNotRewritten(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)

	staged, err := StageFiles(context.Background(), tree, e.paths, WithStackEnvs(envsOf(map[string]string{"web": "SECRET=1"})))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Discard()
	if env := staged.StackEnvChanges(); len(env.Added)+len(env.Replaced) != 0 {
		t.Fatalf("StackEnvChanges = %+v, want none", env)
	}
}

func TestRestoreFiles_WithoutStackEnvsEveryEnvIsLeftAlone(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	if _, err := RestoreFiles(context.Background(), tree, e.paths); err != nil {
		t.Fatal(err)
	}
	if got := slurp(t, filepath.Join(e.paths.StacksDir, "web", ".env")); got != "SECRET=1" {
		t.Fatalf("web/.env = %q, want it untouched", got)
	}
	if pathExists(filepath.Join(e.paths.StacksDir, "new", ".env")) {
		t.Fatal("an .env appeared with no secrets to restore")
	}
}

// The data-loss scenario for the .env files: a failure injected while they
// are put in place. Every stack's .env, and every other stack file, ends up
// entirely as it was.
func TestApply_AFailureWhileStackEnvsAreWrittenLeavesThemAllAsTheyWere(t *testing.T) {
	envs := envsOf(map[string]string{"web": "SECRET=2", "new": "NEW=1"})
	for step := range 5 {
		t.Run(fmt.Sprintf("step%d", step), func(t *testing.T) {
			e := newRestoreEnv(t)
			seedRuntime(t, e)
			tree := filesTree(t, archivedContent)
			before := treeSnapshot(t, e.paths.StacksDir)
			injected := errors.New("injected failure")

			staged, err := StageFiles(context.Background(), tree, e.paths, WithStackEnvs(envs), func(c *filesConfig) {
				c.afterStep = func(category string, n int) error {
					if category == FilesStacks && n == step {
						return injected
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
			if !errors.As(err, &ae) || !errors.Is(err, injected) || !slices.Contains(ae.Unchanged, FilesStacks) || len(ae.Indeterminate) != 0 {
				t.Fatalf("Apply = %v, want the stacks left as they were", err)
			}
			assertTreeUnchanged(t, before, treeSnapshot(t, e.paths.StacksDir))
			if got := slurp(t, filepath.Join(e.paths.StacksDir, "web", ".env")); got != "SECRET=1" {
				t.Errorf("web/.env = %q, want SECRET=1", got)
			}
		})
	}

	t.Run("no failure", func(t *testing.T) {
		e := newRestoreEnv(t)
		seedRuntime(t, e)
		tree := filesTree(t, archivedContent)
		staged, err := StageFiles(context.Background(), tree, e.paths, WithStackEnvs(envs))
		if err != nil {
			t.Fatal(err)
		}
		if err := staged.Apply(); err != nil {
			t.Fatal(err)
		}
		staged.Discard()
		if got := slurp(t, filepath.Join(e.paths.StacksDir, "web", ".env")); got != "SECRET=2" {
			t.Errorf("web/.env = %q, want SECRET=2", got)
		}
		if got := slurp(t, filepath.Join(e.paths.StacksDir, "new", ".env")); got != "NEW=1" {
			t.Errorf("new/.env = %q, want NEW=1", got)
		}
	})
}

func TestStageFiles_AFailureWhileStagingStackEnvsChangesNothing(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	tree := filesTree(t, archivedContent)
	before := treeSnapshot(t, e.root)

	_, err := StageFiles(context.Background(), tree, e.paths, WithStackEnvs(envsOf(map[string]string{"web": "SECRET=2"})), func(c *filesConfig) {
		c.beforeStage = func(dest string) error {
			if filepath.Base(dest) == ".env" {
				return errors.New("injected failure")
			}
			return nil
		}
	})
	if err == nil {
		t.Fatal("StageFiles = nil, want the injected failure")
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
}

func TestRestoreFiles_RefusesToWriteAStackEnvThroughASymbolicLink(t *testing.T) {
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	outside := filepath.Join(e.root, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(e.paths.StacksDir, "web", ".env")
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, envPath); err != nil {
		t.Fatal(err)
	}
	tree := filesTree(t, archivedContent)
	before := treeSnapshot(t, e.root)

	opt := WithStackEnvs(envsOf(map[string]string{"web": "SECRET=2"}))
	var unsafe *UnsafeRestorePathError
	if _, err := PlanFiles(tree, e.paths, opt); !errors.As(err, &unsafe) {
		t.Fatalf("PlanFiles = %v, want an *UnsafeRestorePathError", err)
	}
	if _, err := RestoreFiles(context.Background(), tree, e.paths, opt); !errors.As(err, &unsafe) {
		t.Fatalf("RestoreFiles = %v, want an *UnsafeRestorePathError", err)
	}
	assertTreeUnchanged(t, before, treeSnapshot(t, e.root))
	if got := slurp(t, outside); got != "outside" {
		t.Fatalf("the link's target = %q, was written through", got)
	}
}

func resolveTree(t *testing.T) string {
	return secretsTree(t, archivedContent, testBackupPassphrase, map[string]string{"web": "SECRET=2", "ghost": "X=1"})
}

func configured(pass string, ok bool) SecretSource {
	return &FakeSecretSource{Passphrase: pass, HasPass: ok}
}

func strPtr(s string) *string { return &s }

func TestResolveSecrets(t *testing.T) {
	ctx := context.Background()
	archiveStacks := []string{"new", "web"}

	tests := []struct {
		name       string
		tree       func(t *testing.T) string
		src        SecretSource
		explicit   *string
		wantStatus string
		wantStacks []string
		wantErr    error
	}{
		{"configured passphrase opens it", resolveTree, configured(testBackupPassphrase, true), nil, SecretsOpened, nil, nil},
		{"explicit passphrase opens it over a wrong configured one", resolveTree, configured("wrong", true), strPtr(testBackupPassphrase), SecretsOpened, nil, nil},
		{"explicit passphrase opens it with none configured", resolveTree, configured("", false), strPtr(testBackupPassphrase), SecretsOpened, nil, nil},
		{"no passphrase at all", resolveTree, configured("", false), nil, SecretsNoPassphrase, archiveStacks, nil},
		{"a nil source is no passphrase", resolveTree, nil, nil, SecretsNoPassphrase, archiveStacks, nil},
		{"a wrong configured passphrase restores the rest", resolveTree, configured("wrong", true), nil, SecretsPassphraseIncorrect, archiveStacks, nil},
		{"a wrong explicit passphrase is refused", resolveTree, configured(testBackupPassphrase, true), strPtr("wrong"), "", nil, ErrPassphraseIncorrect},
		{"an empty explicit passphrase is refused", resolveTree, configured(testBackupPassphrase, true), strPtr(""), "", nil, ErrPassphraseIncorrect},
		{"no secrets section", func(t *testing.T) string { return filesTree(t, archivedContent) }, configured(testBackupPassphrase, true), nil, SecretsNone, archiveStacks, nil},
		{"an explicit passphrase and no secrets section", func(t *testing.T) string { return filesTree(t, archivedContent) }, nil, strPtr("anything"), SecretsNone, archiveStacks, nil},
		{"an unreadable secrets section", func(t *testing.T) string {
			tree := filesTree(t, archivedContent)
			if err := os.WriteFile(filepath.Join(tree, "secrets.age"), []byte("garbage"), 0o600); err != nil {
				t.Fatal(err)
			}
			return tree
		}, configured(testBackupPassphrase, true), nil, "", nil, ErrSecretsUnreadable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ResolveSecrets(ctx, tc.tree(t), tc.src, tc.explicit)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolveSecrets = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSecrets: %v", err)
			}
			if out.Status != tc.wantStatus || !slices.Equal(out.Stacks, tc.wantStacks) {
				t.Fatalf("outcome = %q %v, want %q %v", out.Status, out.Stacks, tc.wantStatus, tc.wantStacks)
			}
			if opened := len(out.StackEnvs()) > 0; opened != (tc.wantStatus == SecretsOpened) {
				t.Fatalf("StackEnvs = %+v with status %s", out.StackEnvs(), out.Status)
			}
		})
	}

	t.Run("a failure reading the configured passphrase refuses", func(t *testing.T) {
		boom := errors.New("machine key unavailable")
		src := &ServiceSecretSource{BackupPassphraseFn: func(context.Context) (string, bool, error) { return "", false, boom }}
		_, err := ResolveSecrets(ctx, resolveTree(t), src, nil)
		if !errors.Is(err, boom) {
			t.Fatalf("ResolveSecrets = %v, want the read failure", err)
		}
	})
}

func TestStackEnvsNotRestored(t *testing.T) {
	ctx := context.Background()
	e := newRestoreEnv(t)
	seedRuntime(t, e)
	putFiles(t, e.paths.StacksDir, map[string]string{"old/.env": "KEEP=1", "new/notes": "no .env here"})

	type item struct{ name, reason string }
	summarize := func(ns []NotRestored) []item {
		var out []item
		for _, n := range ns {
			if n.Kind != NotRestoredStackEnv || n.Message == "" {
				t.Errorf("item %+v has no kind or message", n)
			}
			out = append(out, item{n.Name, n.Reason})
		}
		return out
	}

	t.Run("opened", func(t *testing.T) {
		out, err := ResolveSecrets(ctx, resolveTree(t), configured(testBackupPassphrase, true), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := StackEnvsNotRestored(out, e.paths)
		if err != nil {
			t.Fatal(err)
		}
		want := []item{{"ghost", NotRestoredStackNotInArchive}, {"old", NotRestoredLeftInPlace}}
		if !reflect.DeepEqual(summarize(got), want) {
			t.Fatalf("not restored = %v, want %v", summarize(got), want)
		}
	})

	for status, src := range map[string]SecretSource{
		NotRestoredNoPassphrase:        configured("", false),
		NotRestoredPassphraseIncorrect: configured("wrong", true),
	} {
		t.Run(status, func(t *testing.T) {
			out, err := ResolveSecrets(ctx, resolveTree(t), src, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := StackEnvsNotRestored(out, e.paths)
			if err != nil {
				t.Fatal(err)
			}
			want := []item{{"new", status}, {"web", status}, {"old", NotRestoredLeftInPlace}}
			if !reflect.DeepEqual(summarize(got), want) {
				t.Fatalf("not restored = %v, want %v", summarize(got), want)
			}
		})
	}

	t.Run("no secrets section", func(t *testing.T) {
		out, err := ResolveSecrets(ctx, filesTree(t, archivedContent), configured(testBackupPassphrase, true), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := StackEnvsNotRestored(out, e.paths)
		if err != nil {
			t.Fatal(err)
		}
		want := []item{{"new", NotRestoredNoSecrets}, {"web", NotRestoredNoSecrets}, {"old", NotRestoredLeftInPlace}}
		if !reflect.DeepEqual(summarize(got), want) {
			t.Fatalf("not restored = %v, want %v", summarize(got), want)
		}
	})

	t.Run("a directory that is not there", func(t *testing.T) {
		out, err := ResolveSecrets(ctx, resolveTree(t), configured(testBackupPassphrase, true), nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := StackEnvsNotRestored(out, Paths{StacksDir: filepath.Join(e.root, "missing")})
		if err != nil {
			t.Fatal(err)
		}
		if want := []item{{"ghost", NotRestoredStackNotInArchive}}; !reflect.DeepEqual(summarize(got), want) {
			t.Fatalf("not restored = %v, want %v", summarize(got), want)
		}
	})
}
