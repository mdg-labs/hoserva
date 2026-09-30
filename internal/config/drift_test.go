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

func putHostFile(t *testing.T, g *Generator, rel, body string) string {
	t.Helper()
	full := filepath.Join(g.Root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func TestHostFileDecisionForIsOnlyTheFilesGeneratorWrites(t *testing.T) {
	for _, tc := range []struct {
		kind, decision string
		want           HostFileDecision
		ok             bool
	}{
		{KindSamba, DecisionImport, HostFileDecision{Path: PathSamba, Decision: DecisionImport}, true},
		{KindNFS, DecisionLeave, HostFileDecision{Path: PathNFS, Decision: DecisionLeave}, true},
		{KindFstab, DecisionImport, HostFileDecision{}, false},
		{KindDockerImages, DecisionImport, HostFileDecision{}, false},
		{KindSamba, "maybe", HostFileDecision{}, false},
	} {
		got, ok := HostFileDecisionFor(tc.kind, tc.decision)
		if got != tc.want || ok != tc.ok {
			t.Errorf("HostFileDecisionFor(%s, %s) = %+v, %v; want %+v, %v", tc.kind, tc.decision, got, ok, tc.want, tc.ok)
		}
	}
}

// The restored host_config says import, the host's manifest has never heard
// of smb.conf, and Write refuses it (ErrExistingHostFile, the 409
// unmanaged_config a share write returned, #471). PlanHostFiles names it as
// a file the restore replaces, and ReconcileHostFiles is what lets Write
// through, without touching the file itself.
func TestReconcileHostFilesLetsAWriteReplaceAnImportedFileAndOnlyThen(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	full := putHostFile(t, g, PathSamba, "[global]\n")
	file := File{Path: PathSamba, Command: "hoserva apply", Body: []byte("[media]\n")}
	if err := g.Write(ctx, file, 1, time.Now()); !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("Write before reconciling = %v, want ErrExistingHostFile", err)
	}

	plan, err := g.PlanHostFiles(ctx, []HostFileDecision{{Path: PathSamba, Decision: DecisionImport}})
	if err != nil {
		t.Fatalf("PlanHostFiles: %v", err)
	}
	if len(plan.Replace) != 1 || plan.Replace[0].Path != PathSamba || len(plan.Record) != 1 {
		t.Fatalf("plan = %+v, want smb.conf to be replaced and recorded", plan)
	}
	if got, _ := os.ReadFile(full); string(got) != "[global]\n" {
		t.Fatalf("planning changed the file: %q", got)
	}
	if err := g.Write(ctx, file, 1, time.Now()); !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("Write after planning only = %v, want ErrExistingHostFile", err)
	}

	if err := g.ReconcileHostFiles(ctx, plan.Record); err != nil {
		t.Fatalf("ReconcileHostFiles: %v", err)
	}
	if got, _ := os.ReadFile(full); string(got) != "[global]\n" {
		t.Fatalf("reconciling wrote the file: %q", got)
	}
	if st, err := g.Check(ctx, PathSamba); err != nil || st != StatusManaged {
		t.Fatalf("Check = %v, %v; want managed", st, err)
	}
	if err := g.Write(ctx, file, 1, time.Now()); err != nil {
		t.Fatalf("Write after reconciling: %v", err)
	}
	if got, _ := os.ReadFile(full); !strings.Contains(string(got), "[media]") {
		t.Fatalf("Write did not replace the file: %q", got)
	}
}

// A leave decision, a kind with no decision, and a file that is not there are
// none of them replaced; leave is recorded unmanaged so nothing writes it.
func TestPlanHostFilesReplacesOnlyWhatARestoredImportTakesOver(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	full := putHostFile(t, g, PathSamba, "[global]\n")

	plan, err := g.PlanHostFiles(ctx, []HostFileDecision{
		{Path: PathSamba, Decision: DecisionLeave},
		{Path: PathNFS, Decision: DecisionImport},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Replace) != 0 {
		t.Fatalf("Replace = %+v, want nothing: leave keeps the file and the exports file is absent", plan.Replace)
	}
	if len(plan.Record) != 1 || plan.Record[0] != (HostFileDecision{Path: PathSamba, Decision: DecisionLeave}) {
		t.Fatalf("Record = %+v, want smb.conf left", plan.Record)
	}
	if err := g.ReconcileHostFiles(ctx, plan.Record); err != nil {
		t.Fatal(err)
	}
	if st, err := g.Check(ctx, PathSamba); err != nil || st != StatusUnmanaged {
		t.Fatalf("Check = %v, %v; want unmanaged", st, err)
	}
	if err := g.Write(ctx, File{Path: PathSamba, Command: "c", Body: []byte("x")}, 1, time.Now()); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("Write of a left file = %v, want ErrUnmanaged", err)
	}
	if got, _ := os.ReadFile(full); string(got) != "[global]\n" {
		t.Fatalf("the left file changed: %q", got)
	}
}

// A file the manifest already records is this host's own history: one marked
// unmanaged is the user's decision and is never taken over by a restored
// import, and one managed is replaced but is not recorded again.
func TestPlanAndReconcileHostFilesKeepAnExistingManifestRecord(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	putHostFile(t, g, PathNFS, "/srv 10.0.0.0/8(rw)\n")
	if err := g.KeepUnmanaged(ctx, PathNFS); err != nil {
		t.Fatal(err)
	}
	if err := g.Write(ctx, File{Path: PathSamba, Command: "c", Body: []byte("[media]\n")}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}

	both := []HostFileDecision{{Path: PathSamba, Decision: DecisionImport}, {Path: PathNFS, Decision: DecisionImport}}
	plan, err := g.PlanHostFiles(ctx, both)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Replace) != 1 || plan.Replace[0].Path != PathSamba || len(plan.Record) != 0 {
		t.Fatalf("plan = %+v, want only the managed smb.conf replaced and nothing recorded", plan)
	}
	if err := g.ReconcileHostFiles(ctx, both); err != nil {
		t.Fatal(err)
	}
	if st, _ := g.Check(ctx, PathNFS); st != StatusUnmanaged {
		t.Fatalf("a restored import took over the exports file the user left: %v", st)
	}
}

func TestReconcileHostFilesSkipsAFileThatIsGone(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	if err := g.ReconcileHostFiles(ctx, []HostFileDecision{{Path: PathSamba, Decision: DecisionImport}}); err != nil {
		t.Fatalf("ReconcileHostFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(g.Root, ".hoserva", "manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a manifest was written for a file that does not exist: %v", err)
	}
}

// A manifest or file that cannot be read is a failure, never "nothing to do":
// the caller refuses the restore rather than overwrite what it could not see.
func TestPlanHostFilesFailsClosedOnAnUnreadableManifest(t *testing.T) {
	g := NewGenerator(t.TempDir())
	ctx := context.Background()
	putHostFile(t, g, PathSamba, "[global]\n")
	if err := os.MkdirAll(filepath.Join(g.Root, ".hoserva"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(g.Root, ".hoserva", "manifest.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.PlanHostFiles(ctx, []HostFileDecision{{Path: PathSamba, Decision: DecisionImport}}); err == nil {
		t.Fatal("PlanHostFiles read an unparseable manifest as empty")
	}
	if err := g.ReconcileHostFiles(ctx, []HostFileDecision{{Path: PathSamba, Decision: DecisionImport}}); err == nil {
		t.Fatal("ReconcileHostFiles read an unparseable manifest as empty")
	}
}
