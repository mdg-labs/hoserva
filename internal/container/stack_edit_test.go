package container

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const editedCompose = "services:\n  web:\n    image: nginx:1.28\n    ports:\n      - \"8081:80\"\n"

// editHookRunner calls onRun before each docker call; a non-nil error from it
// is what the call returns, and the call never reaches the wrapped Runner.
type editHookRunner struct {
	Runner
	onRun func(args []string) error
}

func (h editHookRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	if h.onRun != nil {
		if err := h.onRun(args); err != nil {
			return nil, err
		}
	}
	return h.Runner.Run(ctx, env, name, args...)
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// snapshotFiles returns the bytes of the stack's three files.
func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range stackFileNames {
		out[name] = string(readFile(t, filepath.Join(dir, name)))
	}
	return out
}

func equalFiles(a, b map[string]string) bool {
	for _, name := range stackFileNames {
		if a[name] != b[name] {
			return false
		}
	}
	return true
}

func TestStackUpdate_ChecksTheTextBeforeStoringAndSetsTheFlag(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	if oldRow.ManuallyEdited {
		t.Fatal("a template install is already marked manually edited")
	}

	var checked, tmpCompose string
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if !hasArg(args, "config") {
			return nil
		}
		checked = "ran"
		tmpCompose = flagValue(args, "--file")
		if strings.HasPrefix(tmpCompose, r.root) {
			t.Errorf("the text was checked from %s, inside the stacks directory", tmpCompose)
		}
		if got := string(readFile(t, tmpCompose)); got != editedCompose {
			t.Errorf("the checked file = %q, want the text being saved", got)
		}
		envFile := flagValue(args, "--env-file")
		if envFile == filepath.Join(dir, ".env") || string(readFile(t, envFile)) != "TOKEN=s3cret\n" {
			t.Errorf("--env-file %s is not a copy of the stack's .env", envFile)
		}
		if flagValue(args, "--project-name") != "nginx" || flagValue(args, "--project-directory") != dir {
			t.Errorf("project name/directory = %q / %q", flagValue(args, "--project-name"), flagValue(args, "--project-directory"))
		}
		if r.store.rows["nginx"].Compose != oldRow.Compose || r.store.rows["nginx"].ManuallyEdited {
			t.Error("the row changed before the text was checked")
		}
		if !equalFiles(snapshotFiles(t, dir), oldFiles) {
			t.Error("a file changed before the text was checked")
		}
		return nil
	}}

	got, err := r.svc.Update(context.Background(), "nginx", editedCompose, false)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if checked == "" {
		t.Fatal("docker compose config never ran")
	}
	if _, err := os.Stat(filepath.Dir(tmpCompose)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the temporary directory %s was left behind: %v", filepath.Dir(tmpCompose), err)
	}
	row := r.store.rows["nginx"]
	if row.Compose != editedCompose || !row.ManuallyEdited {
		t.Fatalf("row = compose %q, manuallyEdited %v; want the new text and the flag", row.Compose, row.ManuallyEdited)
	}
	if got.Compose != editedCompose || !got.ManuallyEdited {
		t.Fatalf("returned stack = %+v", got)
	}
	newFiles := snapshotFiles(t, dir)
	if newFiles[stackComposeFile] != editedCompose {
		t.Fatalf("docker-compose.yml = %q, want the new text", newFiles[stackComposeFile])
	}
	if newFiles[stackEnvFile] != oldFiles[stackEnvFile] || newFiles[stackMetaFile] != oldFiles[stackMetaFile] {
		t.Fatal("an edit of the compose file rewrote the .env or meta.json")
	}
	for _, c := range r.runner.Calls() {
		if hasArg(c.Args, "up") || hasArg(c.Args, "down") {
			t.Fatalf("an edit ran %v: nothing may be restarted", c.Args)
		}
	}
}

func TestStackUpdate_RefusedTextLeavesTheRowAndEveryFileByteIdentical(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "config") {
			return &exec.ExitError{}
		}
		return nil
	}}

	_, err := r.svc.Update(context.Background(), "nginx", "services: [\n", false)
	if !errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Update() error = %v, want ErrInvalidStack", err)
	}
	if got := r.store.rows["nginx"]; got.Compose != oldRow.Compose || got.ManuallyEdited != oldRow.ManuallyEdited {
		t.Fatalf("a refused edit changed the row: %+v", got)
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatal("a refused edit changed a file")
	}
}

func TestStackUpdate_EmptyTextIsRefusedWithoutRunningDocker(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	before := len(r.runner.Calls())
	for _, text := range []string{"", " \n\t"} {
		if _, err := r.svc.Update(context.Background(), "nginx", text, false); !errors.Is(err, ErrInvalidStack) {
			t.Errorf("Update(%q) error = %v, want ErrInvalidStack", text, err)
		}
	}
	if len(r.runner.Calls()) != before || r.store.rows["nginx"].ManuallyEdited {
		t.Fatal("an empty file ran docker or changed the row")
	}
}

func TestStackUpdate_ComposePluginMissingIsNotAnInvalidFile(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "config") {
			return errors.New("docker: unknown command: docker compose")
		}
		return nil
	}}
	_, err := r.svc.Update(context.Background(), "nginx", editedCompose, false)
	if !errors.Is(err, ErrComposeUnavailable) || errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Update() error = %v, want ErrComposeUnavailable", err)
	}
	if r.store.rows["nginx"].ManuallyEdited {
		t.Fatal("the row changed although the text could not be checked")
	}
}

func TestStackUpdate_DryRunValidatesAndChangesNothing(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	checks := 0
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "config") {
			checks++
		}
		return nil
	}}

	got, err := r.svc.Update(context.Background(), "nginx", editedCompose, true)
	if err != nil {
		t.Fatalf("Update dry run: %v", err)
	}
	if checks != 1 {
		t.Fatalf("docker compose config ran %d times, want 1", checks)
	}
	if got.Compose != oldRow.Compose || got.ManuallyEdited {
		t.Fatalf("a dry run returned %+v, want the stored stack", got)
	}
	if row := r.store.rows["nginx"]; row.Compose != oldRow.Compose || row.ManuallyEdited {
		t.Fatalf("a dry run changed the row: %+v", row)
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatal("a dry run changed a file")
	}

	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "config") {
			return &exec.ExitError{}
		}
		return nil
	}}
	if _, err := r.svc.Update(context.Background(), "nginx", "services: [\n", true); !errors.Is(err, ErrInvalidStack) {
		t.Fatalf("a dry run of an invalid file: error = %v, want ErrInvalidStack", err)
	}
}

func TestStackUpdate_RowIsPutBackWhenTheFileCannotBeWritten(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldRow := r.store.rows["nginx"]
	// A non-empty directory where the file belongs: the rename onto it fails.
	path := filepath.Join(dir, stackComposeFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := r.svc.Update(context.Background(), "nginx", editedCompose, false)
	if err == nil {
		t.Fatal("Update succeeded although docker-compose.yml could not be written")
	}
	if got := r.store.rows["nginx"]; got.Compose != oldRow.Compose || got.ManuallyEdited {
		t.Fatalf("row after a failed write = compose %q, manuallyEdited %v; want the old row", got.Compose, got.ManuallyEdited)
	}
}

func TestStackUpdate_FailedDirectorySyncPutsTheOldTextBackOnDisk(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	syncErr := errors.New("input/output error")
	r.svc.syncDir = func(string) error { return syncErr }

	_, err := r.svc.Update(context.Background(), "nginx", editedCompose, false)
	if !errors.Is(err, syncErr) {
		t.Fatalf("Update() error = %v, want the sync failure", err)
	}
	if got := r.store.rows["nginx"]; got.Compose != oldRow.Compose || got.ManuallyEdited {
		t.Fatalf("row after a failed sync = %+v; want the old row", got)
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatalf("docker-compose.yml is %q after the failed edit; the old text must be back so the row and the file agree", readFile(t, filepath.Join(dir, stackComposeFile)))
	}
}

func TestStackUpdate_RowRollbackIsNotCancelledWithTheRequest(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	oldRow := r.store.rows["nginx"]
	ctx, cancel := context.WithCancel(context.Background())
	r.svc.syncDir = func(string) error {
		cancel()
		return errors.New("input/output error")
	}

	if _, err := r.svc.Update(ctx, "nginx", editedCompose, false); err == nil {
		t.Fatal("Update succeeded although the directory sync failed")
	}
	if got := r.store.rows["nginx"]; got.Compose != oldRow.Compose || got.ManuallyEdited {
		t.Fatalf("a request that disconnected left the row at %+v", got)
	}
}

func TestStackUpdate_RowUpdateFailureWritesNothing(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	r.store.updateErr = errors.New("database is locked")

	if _, err := r.svc.Update(context.Background(), "nginx", editedCompose, false); err == nil {
		t.Fatal("Update succeeded although the row could not be updated")
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatal("a file changed although the row was not updated")
	}
}

func TestStackUpdate_UnknownStackInvalidNameAndUnopenableEnv(t *testing.T) {
	r := newStackRig(t)
	if _, err := r.svc.Update(context.Background(), "nope", editedCompose, false); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("unknown stack: error = %v, want ErrStackNotFound", err)
	}
	for _, name := range []string{"", "../x", "-x", "A"} {
		if _, err := r.svc.Update(context.Background(), name, editedCompose, false); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("Update(%q) error = %v, want ErrInvalidStackName", name, err)
		}
	}
	if len(r.runner.Calls()) != 0 {
		t.Fatal("a refused request ran docker")
	}

	r.create(t, "nginx")
	r.svc.Cipher = unopenableCipher{}
	before := len(r.runner.Calls())
	if _, err := r.svc.Update(context.Background(), "nginx", editedCompose, false); err == nil || errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Update with an unopenable .env: error = %v, want a plain failure", err)
	}
	if len(r.runner.Calls()) != before || r.store.rows["nginx"].ManuallyEdited {
		t.Fatal("an edit that could not be checked ran docker or changed the row")
	}
}

func TestStackUpdate_ReusesTheMissingDirectoryAndFilesFromTheRow(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "config") && flagValue(args, "--project-directory") == dir {
			t.Error("the checked project directory does not exist")
		}
		return nil
	}}

	if _, err := r.svc.Update(context.Background(), "nginx", editedCompose, false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	files := snapshotFiles(t, dir)
	if files[stackComposeFile] != editedCompose || files[stackEnvFile] != "TOKEN=s3cret\n" || !strings.Contains(files[stackMetaFile], `"nginx"`) {
		t.Fatalf("regenerated files = %v", files)
	}
}

func TestStackUp_StartsTheEditedFileNotTheOldOne(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	if _, err := r.svc.Update(context.Background(), "nginx", editedCompose, false); err != nil {
		t.Fatalf("Update: %v", err)
	}
	var seen string
	r.svc.Runner = editHookRunner{Runner: r.svc.Runner, onRun: func(args []string) error {
		if hasArg(args, "up") {
			seen = string(readFile(t, filepath.Join(dir, stackComposeFile)))
		}
		return nil
	}}
	if err := r.svc.Up(context.Background(), "nginx"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if seen != editedCompose {
		t.Fatalf("compose up read %q, want the edited text", seen)
	}
}

func TestStackRequireRunning(t *testing.T) {
	r := newStackRig(t)
	if err := r.svc.RequireRunning(); err != nil {
		t.Fatalf("RequireRunning with a running array: %v", err)
	}
	r.svc.RequireArrayRunning = func() error { return ErrArrayStopped }
	if err := r.svc.RequireRunning(); !errors.Is(err, ErrArrayStopped) {
		t.Fatalf("RequireRunning = %v, want ErrArrayStopped", err)
	}
	r.svc.RequireArrayRunning = nil
	if err := r.svc.RequireRunning(); !errors.Is(err, ErrArrayStateUnknown) {
		t.Fatalf("RequireRunning with no array check = %v, want ErrArrayStateUnknown", err)
	}
}
