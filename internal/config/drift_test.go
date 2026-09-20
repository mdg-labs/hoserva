package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckUnknownForNeverWritten(t *testing.T) {
	g := NewGenerator(t.TempDir())

	status, err := g.Check(context.Background(), "snapraid.conf")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusUnknown {
		t.Fatalf("Check = %v, want StatusUnknown", status)
	}
}

func TestCheckManagedRightAfterWrite(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusManaged {
		t.Fatalf("Check = %v, want StatusManaged", status)
	}
}

func TestCheckDriftedAfterHandEdit(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	full := filepath.Join(g.Root, file.Path)
	if err := os.WriteFile(full, []byte("# hand-edited\nparity /mnt/parity/snapraid.parity\n"), 0o644); err != nil {
		t.Fatalf("hand-editing generated file: %v", err)
	}

	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusDrifted {
		t.Fatalf("Check = %v, want StatusDrifted", status)
	}
}

func TestCheckDriftedWhenFileRemoved(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.Remove(filepath.Join(g.Root, file.Path)); err != nil {
		t.Fatalf("removing generated file: %v", err)
	}

	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusDrifted {
		t.Fatalf("Check = %v, want StatusDrifted", status)
	}
}

func TestCheckAllReturnsEveryRecordedFile(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()

	snapraid := testFile()
	smb := File{Path: "samba/smb.conf", Command: "share create", Body: []byte("[global]\n")}

	if err := g.Write(ctx, snapraid, 1, time.Now()); err != nil {
		t.Fatalf("Write snapraid: %v", err)
	}
	if err := g.Write(ctx, smb, 1, time.Now()); err != nil {
		t.Fatalf("Write smb: %v", err)
	}
	if err := os.WriteFile(filepath.Join(g.Root, smb.Path), []byte("hand-edited\n"), 0o644); err != nil {
		t.Fatalf("hand-editing smb.conf: %v", err)
	}

	statuses, err := g.CheckAll(ctx)
	if err != nil {
		t.Fatalf("CheckAll: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("CheckAll returned %d entries, want 2: %v", len(statuses), statuses)
	}
	if statuses[snapraid.Path] != StatusManaged {
		t.Fatalf("CheckAll[%s] = %v, want StatusManaged", snapraid.Path, statuses[snapraid.Path])
	}
	if statuses[smb.Path] != StatusDrifted {
		t.Fatalf("CheckAll[%s] = %v, want StatusDrifted", smb.Path, statuses[smb.Path])
	}
}

func TestDiffShowsHandEditAgainstFreshRender(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()
	now := time.Date(2026, 9, 14, 10, 33, 12, 0, time.UTC)

	if err := g.Write(ctx, file, 1, now); err != nil {
		t.Fatalf("Write: %v", err)
	}
	full := filepath.Join(g.Root, file.Path)
	if err := os.WriteFile(full, []byte("# hand-edited\nparity /mnt/parity/snapraid.parity\n"), 0o644); err != nil {
		t.Fatalf("hand-editing generated file: %v", err)
	}

	diff, err := g.Diff(ctx, file, 1, now)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "-# hand-edited") {
		t.Fatalf("Diff did not show the removed hand edit:\n%s", diff)
	}
	if !strings.Contains(diff, "+"+headerRule) {
		t.Fatalf("Diff did not show the regenerated header:\n%s", diff)
	}
}

func TestDiffAgainstAMissingFile(t *testing.T) {
	g := NewGenerator(t.TempDir())
	file := testFile()

	diff, err := g.Diff(context.Background(), file, 1, time.Now())
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "+"+headerRule) {
		t.Fatalf("Diff of a missing file did not show the fresh render as additions:\n%s", diff)
	}
}

func TestWriteAfterHandEditRegeneratesAndClearsDrift(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(g.Root, file.Path), []byte("hand-edited\n"), 0o644); err != nil {
		t.Fatalf("hand-editing generated file: %v", err)
	}
	if status, _ := g.Check(ctx, file.Path); status != StatusDrifted {
		t.Fatalf("precondition failed: file was not drifted")
	}

	now := time.Now()
	if err := g.Write(ctx, file, 2, now); err != nil {
		t.Fatalf("regenerating Write: %v", err)
	}

	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusManaged {
		t.Fatalf("Check after regenerate = %v, want StatusManaged", status)
	}

	got, err := os.ReadFile(filepath.Join(g.Root, file.Path))
	if err != nil {
		t.Fatalf("reading regenerated file: %v", err)
	}
	want := Header(file.Command, 2, now) + string(file.Body)
	if string(got) != want {
		t.Fatalf("regenerated content mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestKeepUnmanagedStopsManaging(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(g.Root, file.Path), []byte("owned by the user now\n"), 0o644); err != nil {
		t.Fatalf("hand-editing generated file: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, file.Path); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}

	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusUnmanaged {
		t.Fatalf("Check = %v, want StatusUnmanaged", status)
	}

	got, err := os.ReadFile(filepath.Join(g.Root, file.Path))
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	if string(got) != "owned by the user now\n" {
		t.Fatalf("KeepUnmanaged changed the file's content: %q", got)
	}
}

func TestKeepUnmanagedErrorsForAPathNeverGenerated(t *testing.T) {
	g := NewGenerator(t.TempDir())

	if err := g.KeepUnmanaged(context.Background(), "never-written.conf"); err == nil {
		t.Fatal("KeepUnmanaged on a never-generated path did not error")
	}
}

func TestKeepUnmanagedRecordsAnExistingNeverGeneratedFile(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	ctx := context.Background()
	full := filepath.Join(root, PathNFS)
	original := "/export/media *(ro)\n"
	if err := os.WriteFile(full, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := g.KeepUnmanaged(ctx, PathNFS); err != nil {
		t.Fatalf("KeepUnmanaged on an existing host file: %v", err)
	}
	status, err := g.Check(ctx, PathNFS)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusUnmanaged {
		t.Fatalf("Check = %v, want StatusUnmanaged", status)
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("KeepUnmanaged changed the file: %q", got)
	}
	file := File{Path: PathNFS, Command: "share create", Body: []byte("/generated *(rw)\n")}
	if err := g.Write(ctx, file, 1, time.Now()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("Write after leave-unmanaged = %v, want ErrUnmanaged", err)
	}
	got, err = os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != original {
		t.Fatalf("Write after leave-unmanaged changed the file: %q", got)
	}
}

func TestApplyHostFileDecisionsDoesNotPartialCommit(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	ctx := context.Background()
	if err := os.WriteFile(filepath.Join(root, PathNFS), []byte("/export/media *(ro)\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := g.ApplyHostFileDecisions(ctx, []HostFileDecision{
		{Path: PathNFS, Decision: DecisionLeave},
		{Path: PathSamba, Decision: DecisionLeave},
	}); err == nil {
		t.Fatal("missing samba should fail the whole batch")
	}
	status, err := g.Check(ctx, PathNFS)
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusUnknown {
		t.Fatalf("Check(nfs) = %v, want StatusUnknown after a later file failed", status)
	}
}

func TestManageReversesKeepUnmanaged(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := g.KeepUnmanaged(ctx, file.Path); err != nil {
		t.Fatalf("KeepUnmanaged: %v", err)
	}
	if err := g.Write(ctx, file, 2, time.Now()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("Write on an unmanaged file err = %v, want ErrUnmanaged", err)
	}

	if err := g.Manage(ctx, file.Path); err != nil {
		t.Fatalf("Manage: %v", err)
	}

	now := time.Now()
	if err := g.Write(ctx, file, 2, now); err != nil {
		t.Fatalf("Write after Manage: %v", err)
	}
	status, err := g.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusManaged {
		t.Fatalf("Check after Manage and Write = %v, want StatusManaged", status)
	}
}

func TestManageErrorsForAPathNeverGenerated(t *testing.T) {
	g := NewGenerator(t.TempDir())

	if err := g.Manage(context.Background(), "never-written.conf"); err == nil {
		t.Fatal("Manage on a never-generated path did not error")
	}
}

func TestManageErrorsForAlreadyManagedPath(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	file := testFile()

	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := g.Manage(ctx, file.Path); err == nil {
		t.Fatal("Manage on an already-managed path did not error")
	}
}

func TestManifestPersistsAcrossGeneratorInstances(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	file := testFile()

	first := NewGenerator(root)
	if err := first.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	second := NewGenerator(root)
	status, err := second.Check(ctx, file.Path)
	if err != nil {
		t.Fatalf("Check on a fresh Generator instance: %v", err)
	}
	if status != StatusManaged {
		t.Fatalf("Check on a fresh Generator instance = %v, want StatusManaged", status)
	}
}
