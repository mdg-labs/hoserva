package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archivesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestRunConfigBackup_WritesOneVerifiedArchiveToEveryEnabledDestination(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	a := filepath.Join(rig.root, "a")
	b := filepath.Join(rig.root, "b")
	off := filepath.Join(rig.root, "off")
	for name, path := range map[string]string{"Alpha": a, "Beta": b} {
		if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: name, Type: TypeLocal, Path: path}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Off", Type: TypeLocal, Path: off, Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := rig.svc.RunConfigBackup(ctx, &out); err != nil {
		t.Fatalf("RunConfigBackup: %v", err)
	}
	na, nb := archivesIn(t, a), archivesIn(t, b)
	if len(na) != 1 || len(nb) != 1 || na[0] != nb[0] {
		t.Fatalf("archives = %v and %v, want the same single archive in both", na, nb)
	}
	if _, err := os.Stat(off); !os.IsNotExist(err) {
		t.Fatalf("a disabled destination was written to (stat: %v)", err)
	}
	if err := VerifyArchive(filepath.Join(a, na[0]), "backup-pass"); err != nil {
		t.Fatalf("the archive written does not verify: %v", err)
	}
	if got := out.String(); !strings.Contains(got, na[0]) || !strings.Contains(got, "Alpha") || !strings.Contains(got, "Beta") || strings.Contains(got, "Off") {
		t.Fatalf("output = %q, want the archive and the two destinations written, not the disabled one", got)
	}
	for _, id := range []string{"Alpha", "Beta"} {
		dests, _ := rig.store.ListDestinations(ctx)
		for _, d := range dests {
			if d.Name == id && d.LastSuccessfulBackupAt == nil {
				t.Fatalf("destination %s has no recorded success", id)
			}
		}
	}
}

func TestRunConfigBackup_PartialSuccessSucceedsAndNamesTheDestinationThatFailed(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if _, err := rig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(rig.root, "local")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: local}); err != nil {
		t.Fatal(err)
	}
	rig.rclone.Fail = map[string]error{"copy": &RcloneExitError{Code: 1, Output: "access denied"}}

	var out bytes.Buffer
	if err := rig.svc.RunConfigBackup(ctx, &out); err != nil {
		t.Fatalf("RunConfigBackup: %v — one destination written must make the run succeed, as the nightly chain's does", err)
	}
	if len(archivesIn(t, local)) != 1 {
		t.Fatal("the local destination was not written")
	}
	got := out.String()
	if !strings.Contains(got, "Local") || !strings.Contains(got, "Backblaze") || !strings.Contains(got, "access denied") {
		t.Fatalf("output = %q, want the destination written and the one that failed with its reason", got)
	}
}

func TestRunConfigBackup_FailsWhenNoDestinationWasWritten(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	if _, err := rig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	rig.rclone.Fail = map[string]error{"copy": &RcloneExitError{Code: 1, Output: "access denied"}}

	err := rig.svc.RunConfigBackup(ctx, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v, want the destination's failure", err)
	}
}

func TestRunConfigBackup_WithNothingEnabledFailsInsteadOfReportingSuccess(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dest, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: filepath.Join(rig.root, "local")})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.RequireEnabledDestination(ctx); err != nil {
		t.Fatalf("RequireEnabledDestination with one enabled: %v", err)
	}
	if _, err := rig.svc.UpdateDestination(ctx, dest.ID, DestinationUpdate{Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.RequireEnabledDestination(ctx); !errors.Is(err, ErrNoEnabledDestination) {
		t.Fatalf("RequireEnabledDestination = %v, want ErrNoEnabledDestination", err)
	}
	if err := rig.svc.RunConfigBackup(ctx, &bytes.Buffer{}); !errors.Is(err, ErrNoEnabledDestination) {
		t.Fatalf("RunConfigBackup with nothing enabled = %v, want ErrNoEnabledDestination", err)
	}
}

// An on-demand archive is an ordinary one: it takes the daily slot an older
// ordinary archive held, and never one of the pre-change archives kept on
// top of the tiers.
func TestRunConfigBackup_RetentionCountsItLikeAScheduledBackupAndKeepsPreChangeArchives(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dir := filepath.Join(rig.root, "kept")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: dir, Retention: &Retention{Daily: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	install := rig.svc.installationID()
	seed := func(reason Reason, daysAgo int) string {
		day := rig.now.AddDate(0, 0, -daysAgo)
		name := archiveName(install, day, reason, 0)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, name), day, day); err != nil {
			t.Fatal(err)
		}
		return name
	}
	preUpdate := seed(ReasonPreUpdate, 1)
	preImport := seed(ReasonPreImport, 2)
	ordinary := seed(ReasonNone, 3)

	if err := rig.svc.RunConfigBackup(ctx, &bytes.Buffer{}); err != nil {
		t.Fatalf("RunConfigBackup: %v", err)
	}
	names := archivesIn(t, dir)
	if !contains(names, preUpdate) || !contains(names, preImport) {
		t.Fatalf("archives = %v, want both pre-change archives kept", names)
	}
	if contains(names, ordinary) {
		t.Fatalf("archives = %v, want the older ordinary archive pruned by the on-demand one's daily slot", names)
	}
	if len(names) != 3 {
		t.Fatalf("archives = %v, want the two pre-change archives and the new one", names)
	}

	if err := rig.svc.RunConfigBackup(ctx, &bytes.Buffer{}); err != nil {
		t.Fatalf("second RunConfigBackup: %v", err)
	}
	names = archivesIn(t, dir)
	if len(names) != 3 || !contains(names, preUpdate) || !contains(names, preImport) {
		t.Fatalf("archives after a second on-demand backup = %v, want it to replace the first and leave the pre-change archives", names)
	}
}

func TestRunConfigBackup_ConcurrentWithAPreChangeBackupNeverLosesEither(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	dir := filepath.Join(rig.root, "shared")
	if _, err := rig.svc.AddDestination(ctx, NewDestination{Name: "Local", Type: TypeLocal, Path: dir, Retention: &Retention{Daily: 1}}); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 2)
	go func() { errs <- rig.svc.RunConfigBackup(ctx, &bytes.Buffer{}) }()
	go func() { errs <- rig.svc.RunReason(ctx, ReasonPreUpdate) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var preChange, ordinary int
	for _, name := range archivesIn(t, dir) {
		switch {
		case strings.Contains(name, fmt.Sprintf(".%s.", ReasonPreUpdate)):
			preChange++
		case strings.HasSuffix(name, ".tar.zst"):
			ordinary++
		}
	}
	if preChange != 1 || ordinary != 1 {
		t.Fatalf("archives = %v, want one pre-update and one ordinary", archivesIn(t, dir))
	}
}
