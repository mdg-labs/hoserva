package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const newEnv = "TOKEN=changed\nPORT=9000\n"

func TestStackUpdateEnv_SealsTheRowAndRewritesOnlyTheEnvFile(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	calls := len(r.runner.Calls())

	got, err := r.svc.UpdateEnv(context.Background(), "nginx", newEnv)
	if err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}
	row := r.store.rows["nginx"]
	if string(xorAll(row.SealedEnv)) != newEnv {
		t.Fatalf("the row's .env opens to %q, want the new text", xorAll(row.SealedEnv))
	}
	if strings.Contains(string(row.SealedEnv), "changed") {
		t.Fatal("the row holds the .env in the clear")
	}
	if row.Compose != oldRow.Compose || row.ManuallyEdited != oldRow.ManuallyEdited || row.TemplateRevision != oldRow.TemplateRevision {
		t.Fatalf("UpdateEnv changed another column: %+v", row)
	}
	files := snapshotFiles(t, dir)
	if files[stackEnvFile] != newEnv {
		t.Fatalf(".env = %q, want the new text", files[stackEnvFile])
	}
	if files[stackComposeFile] != oldFiles[stackComposeFile] || files[stackMetaFile] != oldFiles[stackMetaFile] {
		t.Fatal("an .env update rewrote docker-compose.yml or meta.json")
	}
	if info, err := os.Stat(filepath.Join(dir, stackEnvFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %v, %v; want 0600", info, err)
	}
	if got.Name != "nginx" || got.Compose != oldRow.Compose {
		t.Fatalf("returned stack = %+v", got)
	}
	if len(r.runner.Calls()) != calls {
		t.Fatalf("an .env update ran docker: %v", r.runner.Calls()[calls:])
	}
}

func TestStackUpdateEnv_KeepsAManuallyEditedComposeFileAsItIs(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	if _, err := r.svc.Update(context.Background(), "nginx", editedCompose, false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.root, "nginx")
	before := snapshotFiles(t, dir)

	if _, err := r.svc.UpdateEnv(context.Background(), "nginx", newEnv); err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}
	if got := string(readFile(t, filepath.Join(dir, stackComposeFile))); got != before[stackComposeFile] || got != editedCompose {
		t.Fatalf("docker-compose.yml = %q after an .env update", got)
	}
	if row := r.store.rows["nginx"]; row.Compose != editedCompose || !row.ManuallyEdited {
		t.Fatalf("row = compose %q, manuallyEdited %v", row.Compose, row.ManuallyEdited)
	}
}

func TestStackUpdateEnv_RefusedEnvLeavesTheRowAndEveryFileByteIdentical(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]

	for _, env := range []string{"PATH=/tmp\n", "TOKEN=x\nDOCKER_HOST=tcp://evil\n", "export HOME=/x\n"} {
		_, err := r.svc.UpdateEnv(context.Background(), "nginx", env)
		if !errors.Is(err, ErrReservedEnvName) {
			t.Fatalf("UpdateEnv(%q) = %v, want ErrReservedEnvName", env, err)
		}
		if string(r.store.rows["nginx"].SealedEnv) != string(oldRow.SealedEnv) {
			t.Fatalf("a refused .env changed the row")
		}
		if !equalFiles(snapshotFiles(t, dir), oldFiles) {
			t.Fatal("a refused .env changed a file")
		}
	}
}

func TestStackUpdateEnv_UnknownStackAndInvalidName(t *testing.T) {
	r := newStackRig(t)
	if _, err := r.svc.UpdateEnv(context.Background(), "nope", newEnv); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("unknown stack: error = %v, want ErrStackNotFound", err)
	}
	for _, name := range []string{"", "../x", "-x", "A"} {
		if _, err := r.svc.UpdateEnv(context.Background(), name, newEnv); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("UpdateEnv(%q) error = %v, want ErrInvalidStackName", name, err)
		}
	}
}

func TestStackUpdateEnv_RowUpdateFailureWritesNothing(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	r.store.envErr = errors.New("database is locked")

	if _, err := r.svc.UpdateEnv(context.Background(), "nginx", newEnv); err == nil {
		t.Fatal("UpdateEnv succeeded although the row could not be updated")
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatal("a file changed although the row was not updated")
	}
}

func TestStackUpdateEnv_FailedWriteRestoresTheRowAndTheFile(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	oldFiles := snapshotFiles(t, dir)
	oldRow := r.store.rows["nginx"]
	ctx, cancel := context.WithCancel(context.Background())
	r.svc.syncDir = func(string) error {
		cancel()
		return errors.New("input/output error")
	}

	if _, err := r.svc.UpdateEnv(ctx, "nginx", newEnv); err == nil {
		t.Fatal("UpdateEnv succeeded although the directory sync failed")
	}
	if got := r.store.rows["nginx"]; string(got.SealedEnv) != string(oldRow.SealedEnv) {
		t.Fatalf("a request that disconnected left the row's .env at %q", xorAll(got.SealedEnv))
	}
	if !equalFiles(snapshotFiles(t, dir), oldFiles) {
		t.Fatalf("the .env on disk was left as %q", readFile(t, filepath.Join(dir, stackEnvFile)))
	}
}

func TestStackUpdateEnv_RegeneratesAMissingDirectoryFromTheRow(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.UpdateEnv(context.Background(), "nginx", newEnv); err != nil {
		t.Fatalf("UpdateEnv: %v", err)
	}
	files := snapshotFiles(t, dir)
	if files[stackEnvFile] != newEnv || files[stackComposeFile] != r.store.rows["nginx"].Compose || !strings.Contains(files[stackMetaFile], `"nginx"`) {
		t.Fatalf("regenerated files = %v", files)
	}
}

func TestStackEnv_OpensTheRowsEnv(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	got, err := r.svc.Env(context.Background(), "nginx")
	if err != nil || got != "TOKEN=s3cret\n" {
		t.Fatalf("Env = %q, %v", got, err)
	}
	if _, err := r.svc.Env(context.Background(), "nope"); !errors.Is(err, ErrStackNotFound) {
		t.Errorf("Env(unknown) = %v, want ErrStackNotFound", err)
	}
	if _, err := r.svc.Env(context.Background(), "../x"); !errors.Is(err, ErrInvalidStackName) {
		t.Errorf("Env(bad name) = %v, want ErrInvalidStackName", err)
	}
	r.svc.Cipher = unopenableCipher{}
	if _, err := r.svc.Env(context.Background(), "nginx"); err == nil {
		t.Error("Env with an unopenable row succeeded")
	}
}
