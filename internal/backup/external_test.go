package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestExternalDestination_IsLocalMountPath(t *testing.T) {
	got, err := ExternalDestination("backup")
	if err != nil {
		t.Fatalf("ExternalDestination: %v", err)
	}
	if got.Path != "/mnt/disks/backup" {
		t.Fatalf("Path = %q, want /mnt/disks/backup", got.Path)
	}
	if !disk.IsExternalMountpoint(got.Path) {
		t.Fatalf("Path %q is not an external mountpoint", got.Path)
	}
	if got.ID != "external:backup" || !got.Enabled {
		t.Fatalf("got %+v", got)
	}
}

func TestWriteArchive_AcceptsExternalDestinationDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "hoserva-config-2026-09-20T03-00.tar.zst")
	if err := os.WriteFile(src, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := Destination{ID: "external:backup", Path: dir, Enabled: true}
	if err := writeArchive(dest, src); err != nil {
		t.Fatalf("writeArchive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.Base(src))); err != nil {
		t.Fatalf("archive missing at destination: %v", err)
	}
}

func TestExternalDestination_RejectsInvalidLabel(t *testing.T) {
	if _, err := ExternalDestination("../etc"); err == nil {
		t.Fatal("ExternalDestination(../etc): expected an error")
	}
}

type externalRig struct {
	svc     *Service
	store   *FakeDestinationStore
	root    string
	extRoot string
	now     time.Time
	logs    []string
	mounted map[string]bool
}

func newExternalRig(t *testing.T) *externalRig {
	t.Helper()
	db := openTestDB(t)
	paths, root := testLayout(t)
	rig := &externalRig{
		store:   &FakeDestinationStore{},
		root:    root,
		extRoot: filepath.Join(root, "mnt", "disks"),
		now:     time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC),
		mounted: map[string]bool{},
	}
	rig.svc = &Service{
		DB:           db,
		Paths:        paths,
		Store:        rig.store,
		Secrets:      &FakeSecretSource{Passphrase: "backup-pass", HasPass: true},
		Cipher:       FakeSecretCipher{},
		Hostname:     "test-host",
		Version:      "0.0.0-test",
		Now:          func() time.Time { return rig.now },
		ExternalRoot: rig.extRoot,
		ExternalMounted: func(_ context.Context, path string) (bool, error) {
			return rig.mounted[path], nil
		},
		Log: func(format string, args ...any) { rig.logs = append(rig.logs, fmt.Sprintf(format, args...)) },
	}
	return rig
}

func (r *externalRig) registerFlagged(t *testing.T, label string) Destination {
	t.Helper()
	ctx := context.Background()
	if err := r.svc.RegisterExternalDisk(ctx, store.ExternalDisk{Label: label, BackupDestination: true}); err != nil {
		t.Fatalf("RegisterExternalDisk: %v", err)
	}
	if _, err := r.store.GetDestination(ctx, "external:"+label); err != nil {
		t.Fatalf("the flag created no destination: %v", err)
	}
	// ExternalDestination points at the real /mnt/disks; the rig's disks
	// mount under a directory it controls.
	for i := range r.store.dests {
		if r.store.dests[i].ID == "external:"+label {
			r.store.dests[i].Path = filepath.Join(r.extRoot, label)
			return r.store.dests[i]
		}
	}
	t.Fatal("unreachable")
	return Destination{}
}

func (r *externalRig) addBootDestination(t *testing.T) string {
	t.Helper()
	path := filepath.Join(r.root, "boot-backups")
	err := r.store.CreateDestination(context.Background(), Destination{
		ID: "boot", Name: "Boot", Type: TypeLocal, Path: path, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6}, CreatedAt: r.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExternalMountPoint(t *testing.T) {
	tests := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"/mnt/disks/usb", "/mnt/disks/usb", true},
		{"/mnt/disks/usb/backups/deep", "/mnt/disks/usb", true},
		{"/mnt/disks/usb/", "/mnt/disks/usb", true},
		{"/mnt/disks", "/mnt/disks", true},
		{"/mnt/disksx/usb", "", false},
		{"/mnt/user/hoserva-backups", "", false},
		{"/var/lib/hoserva/backups", "", false},
	}
	for _, tt := range tests {
		got, ok := externalMountPoint(tt.path, "/mnt/disks")
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("externalMountPoint(%q) = %q, %v; want %q, %v", tt.path, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestPrepareExternalDestination(t *testing.T) {
	got, err := PrepareExternalDestination("usb", nil)
	if err != nil || got.ID != "external:usb" || got.Path != "/mnt/disks/usb" {
		t.Fatalf("PrepareExternalDestination = %+v, %v", got, err)
	}
	if _, err := PrepareExternalDestination("../etc", nil); err == nil {
		t.Fatal("an invalid label was accepted")
	}

	own := Destination{ID: "external:usb", Name: "usb", Type: TypeLocal, Path: "/mnt/disks/usb", Retention: Retention{Daily: 1}}
	got, err = PrepareExternalDestination("usb", []Destination{own})
	if err != nil || got.Retention.Daily != 1 {
		t.Fatalf("the disk's own destination = %+v, %v; want it returned untouched", got, err)
	}

	byName := Destination{ID: "dest-1", Name: "USB", Type: TypeLocal, Path: "/srv/x"}
	if _, err := PrepareExternalDestination("usb", []Destination{byName}); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("name clash = %v, want ErrDestinationExists", err)
	}
	byPath := Destination{ID: "dest-2", Name: "Elsewhere", Type: TypeLocal, Path: "/mnt/disks/usb"}
	if _, err := PrepareExternalDestination("usb", []Destination{byPath}); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("path clash = %v, want ErrDestinationExists", err)
	}
}

func TestService_RunWritesToMountedExternalDestination(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	dest := rig.registerFlagged(t, "usb")
	rig.mounted[dest.Path] = true

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	name := archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(dest.Path, name)); err != nil {
		t.Fatalf("archive missing on the mounted external disk: %v", err)
	}
	got, err := rig.store.GetDestination(ctx, dest.ID)
	if err != nil || got.LastSuccessfulBackupAt == nil {
		t.Fatalf("success not recorded on the external destination: %+v, %v", got, err)
	}
}

func TestService_RunChecksTheDiskMountForADestinationBelowIt(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	var asked []string
	rig.svc.ExternalMounted = func(_ context.Context, path string) (bool, error) {
		asked = append(asked, path)
		return true, nil
	}
	sub := filepath.Join(rig.extRoot, "usb", "hoserva", "backups")
	err := rig.store.CreateDestination(ctx, Destination{
		ID: "sub", Name: "Sub", Type: TypeLocal, Path: sub, Enabled: true,
		Retention: Retention{Daily: 1}, CreatedAt: rig.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(asked) != 1 || asked[0] != filepath.Join(rig.extRoot, "usb") {
		t.Fatalf("mount checked at %v, want the disk's own mount point %s", asked, filepath.Join(rig.extRoot, "usb"))
	}
}

func TestService_RunSkipsEjectedExternalDestinationAndWritesTheRest(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	boot := rig.addBootDestination(t)
	dest := rig.registerFlagged(t, "usb")

	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run with an ejected external disk and a working boot destination: %v", err)
	}
	name := archiveName(rig.svc.installationID(), rig.now, ReasonNone, 0)
	if _, err := os.Stat(filepath.Join(boot, name)); err != nil {
		t.Fatalf("boot archive missing: %v", err)
	}
	if _, err := os.Stat(dest.Path); !os.IsNotExist(err) {
		t.Fatalf("%q was created on the root filesystem while the disk was ejected: stat error = %v", dest.Path, err)
	}
	if _, err := os.Stat(rig.extRoot); !os.IsNotExist(err) {
		t.Fatalf("%q was created on the root filesystem while the disk was ejected: stat error = %v", rig.extRoot, err)
	}
	if len(rig.logs) != 1 || !strings.Contains(rig.logs[0], "external:usb") || !strings.Contains(rig.logs[0], "not mounted") {
		t.Fatalf("logs = %q, want one line naming the skipped destination and why", rig.logs)
	}
	got, err := rig.store.GetDestination(ctx, dest.ID)
	if err != nil || got.LastSuccessfulBackupAt != nil {
		t.Fatalf("a skipped destination recorded a success: %+v, %v", got, err)
	}
}

func TestService_RunFailsWhenTheOnlyDestinationIsAnEjectedExternalDisk(t *testing.T) {
	rig := newExternalRig(t)
	dest := rig.registerFlagged(t, "usb")

	if err := rig.svc.Run(context.Background()); err == nil {
		t.Fatal("Run reported success with its only destination skipped")
	}
	if _, err := os.Stat(dest.Path); !os.IsNotExist(err) {
		t.Fatalf("%q was created while the disk was ejected: %v", dest.Path, err)
	}
}

func TestService_RunTreatsUnreadableMountTableAsNotMounted(t *testing.T) {
	rig := newExternalRig(t)
	dest := rig.registerFlagged(t, "usb")
	rig.svc.ExternalMounted = func(context.Context, string) (bool, error) {
		return false, errors.New("open /proc/self/mountinfo: permission denied")
	}

	if err := rig.svc.Run(context.Background()); err == nil {
		t.Fatal("Run reported success although the mount table could not be read")
	}
	if _, err := os.Stat(dest.Path); !os.IsNotExist(err) {
		t.Fatalf("%q was created although the mount state was unknown: %v", dest.Path, err)
	}
}

func TestService_ExternalDestinationDefaultsToTheKernelMountTable(t *testing.T) {
	rig := newExternalRig(t)
	rig.svc.ExternalMounted = nil
	rig.svc.ExternalRoot = filepath.Join(t.TempDir(), "disks")
	dest := Destination{ID: "external:usb", Name: "usb", Type: TypeLocal, Path: filepath.Join(rig.svc.ExternalRoot, "usb"), Enabled: true}

	release, why := rig.svc.admitDestination(context.Background(), dest)
	release()
	if !strings.Contains(why, "not mounted") {
		t.Fatalf("a directory that is not in /proc/self/mountinfo was admitted: reason %q", why)
	}
	if _, err := os.Stat(rig.svc.ExternalRoot); !os.IsNotExist(err) {
		t.Fatalf("the mount check created %q: %v", rig.svc.ExternalRoot, err)
	}
}

func TestTestDestination_ExternalDiskIsNotWrittenWhileEjected(t *testing.T) {
	rig := newExternalRig(t)
	ctx := context.Background()
	dest := rig.registerFlagged(t, "usb")

	res, err := rig.svc.TestDestination(ctx, dest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Success || !strings.Contains(res.Error, "not mounted") {
		t.Fatalf("result = %+v, want a refusal because the disk is not mounted", res)
	}
	if _, err := os.Stat(rig.extRoot); !os.IsNotExist(err) {
		t.Fatalf("the test created %q on the boot device: %v", rig.extRoot, err)
	}

	rig.mounted[dest.Path] = true
	if err := os.MkdirAll(dest.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err = rig.svc.TestDestination(ctx, dest.ID); err != nil || !res.Success {
		t.Fatalf("result on a mounted disk = %+v, %v", res, err)
	}
}

func TestService_EjectedExternalDestinationBecomesStale(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	dest := rig.registerFlagged(t, "usb")
	rig.addBootDestination(t)

	rig.now = rig.now.Add(72 * time.Hour)
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var alerted []string
	alert := func(_ context.Context, name string, _ *time.Time) error {
		alerted = append(alerted, name)
		return nil
	}
	if err := rig.svc.CheckStaleDestinations(ctx, rig.now, alert); err != nil {
		t.Fatal(err)
	}
	if len(alerted) != 1 || alerted[0] != dest.Name {
		t.Fatalf("stale alerts = %v, want exactly the ejected external destination %q", alerted, dest.Name)
	}
}

func TestService_SetExternalDestinationCreatesAndRemovesTheDestination(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	if err := rig.svc.RegisterExternalDisk(ctx, store.ExternalDisk{Label: "usb"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.GetDestination(ctx, "external:usb"); !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("an unflagged disk has a destination: %v", err)
	}

	if err := rig.svc.SetExternalDestination(ctx, "usb", true); err != nil {
		t.Fatalf("flag on: %v", err)
	}
	got, err := rig.store.GetDestination(ctx, "external:usb")
	if err != nil {
		t.Fatalf("flag on created no destination: %v", err)
	}
	if got.Path != "/mnt/disks/usb" || !got.Enabled || got.Retention.Daily != DefaultRetentionDaily || !got.CreatedAt.Equal(rig.now) {
		t.Fatalf("destination = %+v", got)
	}
	if !rig.store.ExternalFlags()["usb"] {
		t.Fatal("flag on left the flag off")
	}

	rig.store.dests[0].Retention.Daily = 99
	if err := rig.svc.SetExternalDestination(ctx, "usb", true); err != nil {
		t.Fatalf("flag on again: %v", err)
	}
	dests, _ := rig.store.ListDestinations(ctx)
	if len(dests) != 1 || dests[0].Retention.Daily != 99 {
		t.Fatalf("flag on again = %+v, want the existing destination kept as it is", dests)
	}

	if err := rig.svc.SetExternalDestination(ctx, "usb", false); err != nil {
		t.Fatalf("flag off: %v", err)
	}
	if dests, _ = rig.store.ListDestinations(ctx); len(dests) != 0 || rig.store.ExternalFlags()["usb"] {
		t.Fatalf("flag off left destinations %+v, flag %v", dests, rig.store.ExternalFlags()["usb"])
	}
}

func TestService_SetExternalDestinationRefusalChangesNothing(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	if err := rig.svc.RegisterExternalDisk(ctx, store.ExternalDisk{Label: "usb"}); err != nil {
		t.Fatal(err)
	}
	taken := Destination{ID: "dest-1", Name: "usb", Type: TypeLocal, Path: "/srv/backups", Enabled: true, CreatedAt: rig.now}
	if err := rig.store.CreateDestination(ctx, taken); err != nil {
		t.Fatal(err)
	}

	if err := rig.svc.SetExternalDestination(ctx, "usb", true); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("err = %v, want ErrDestinationExists", err)
	}
	if rig.store.ExternalFlags()["usb"] {
		t.Fatal("a refused flag was set")
	}
	if err := rig.svc.RegisterExternalDisk(ctx, store.ExternalDisk{Label: "other", BackupDestination: true}); err != nil {
		t.Fatalf("registering a disk whose name is free: %v", err)
	}
	if err := rig.svc.SetExternalDestination(ctx, "gone", true); !errors.Is(err, store.ErrExternalNotFound) {
		t.Fatalf("unknown label = %v, want ErrExternalNotFound", err)
	}
}

func TestService_RegisterExternalDiskRefusalRegistersNothing(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	taken := Destination{ID: "dest-1", Name: "USB", Type: TypeLocal, Path: "/srv/backups", Enabled: true, CreatedAt: rig.now}
	if err := rig.store.CreateDestination(ctx, taken); err != nil {
		t.Fatal(err)
	}

	err := rig.svc.RegisterExternalDisk(ctx, store.ExternalDisk{Label: "usb", BackupDestination: true})
	if !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("err = %v, want ErrDestinationExists", err)
	}
	if _, registered := rig.store.ExternalFlags()["usb"]; registered {
		t.Fatal("the disk was registered although its destination was refused")
	}
}

func TestService_SetExternalDestinationNeedsAnExternalCapableStore(t *testing.T) {
	svc := &Service{Store: plainDestinationStore{&FakeDestinationStore{}}}
	if err := svc.SetExternalDestination(context.Background(), "usb", true); err == nil {
		t.Fatal("a store that cannot track external disks accepted the flag")
	}
}

type plainDestinationStore struct{ DestinationStore }

func TestService_ReconcileExternalDestinationsCreatesTheMissingRow(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	rig.store.flags = map[string]bool{"usb": true, "off": false}

	if err := rig.svc.ReconcileExternalDestinations(ctx); err != nil {
		t.Fatalf("ReconcileExternalDestinations: %v", err)
	}
	dests, _ := rig.store.ListDestinations(ctx)
	if len(dests) != 1 || dests[0].ID != "external:usb" {
		t.Fatalf("destinations = %+v, want only external:usb", dests)
	}
	if err := rig.svc.ReconcileExternalDestinations(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if dests, _ = rig.store.ListDestinations(ctx); len(dests) != 1 {
		t.Fatalf("a second reconcile changed the destinations: %+v", dests)
	}
}

func TestService_ReconcileExternalDestinationsReportsAConflictAndContinues(t *testing.T) {
	ctx := context.Background()
	rig := newExternalRig(t)
	rig.store.flags = map[string]bool{"clash": true, "usb": true}
	taken := Destination{ID: "dest-1", Name: "clash", Type: TypeLocal, Path: "/srv/backups", Enabled: true, CreatedAt: rig.now}
	if err := rig.store.CreateDestination(ctx, taken); err != nil {
		t.Fatal(err)
	}

	err := rig.svc.ReconcileExternalDestinations(ctx)
	if !errors.Is(err, ErrDestinationExists) || !strings.Contains(err.Error(), "clash") {
		t.Fatalf("err = %v, want the conflict on \"clash\"", err)
	}
	if _, gerr := rig.store.GetDestination(ctx, "external:usb"); gerr != nil {
		t.Fatalf("the disk after the conflicting one was not reconciled: %v", gerr)
	}
}
