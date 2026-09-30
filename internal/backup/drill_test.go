package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type drillRig struct {
	svc    *Service
	drills *FakeDrillStore
	dir    string
	alerts []DrillResult
	alert  DrillAlert
}

func newDrillRig(t *testing.T, encrypt bool) *drillRig {
	t.Helper()
	dir := t.TempDir()
	svc := newSameDestinationService(t, newTestRecipient(t), dir, "drill-host", time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC))
	svc.Destinations[0].Encrypt = encrypt
	svc.Destinations[0].Name = "Local"
	rig := &drillRig{svc: svc, drills: &FakeDrillStore{}, dir: dir}
	svc.Drills = rig.drills
	rig.alert = func(_ context.Context, r DrillResult) error {
		rig.alerts = append(rig.alerts, r)
		return nil
	}
	return rig
}

// backupAt writes an archive stamped at, to every destination.
func (r *drillRig) backupAt(t *testing.T, at time.Time) string {
	t.Helper()
	r.svc.Now = func() time.Time { return at }
	if err := r.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run at %s: %v", at, err)
	}
	name := archiveName(r.svc.installationID(), at, ReasonNone, 0)
	if r.svc.Destinations[0].Encrypt {
		name += ".age"
	}
	if err := os.Chtimes(filepath.Join(r.dir, name), at, at); err != nil {
		t.Fatal(err)
	}
	return name
}

// drill runs a drill with the process temp directory confined to a fresh
// directory, and fails the test if the drill left anything in it.
func (r *drillRig) drill(t *testing.T) (string, error) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	var out bytes.Buffer
	err := r.svc.RunDrill(context.Background(), &out, r.alert)
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("the drill left %d entries in its temporary directory: %v", len(left), left)
	}
	return out.String(), err
}

func (r *drillRig) last(t *testing.T) DrillResult {
	t.Helper()
	got, err := r.drills.LastDrill(context.Background())
	if err != nil || got == nil {
		t.Fatalf("LastDrill = %v, %v; want a recorded result", got, err)
	}
	return *got
}

type fileState struct {
	data []byte
	mod  time.Time
}

func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fileState{}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		info, _ := e.Info()
		out[e.Name()] = fileState{data: data, mod: info.ModTime()}
	}
	return out
}

func corrupt(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
}

func TestRunDrill_VerifiesTheNewestArchiveAndChangesNothingOnTheDestination(t *testing.T) {
	rig := newDrillRig(t, false)
	rig.backupAt(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC))
	newest := rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
	before := snapshotDir(t, rig.dir)

	if _, err := rig.drill(t); err != nil {
		t.Fatalf("RunDrill: %v", err)
	}
	if after := snapshotDir(t, rig.dir); !reflect.DeepEqual(before, after) {
		t.Fatal("the drill changed the destination's files")
	}
	got := rig.last(t)
	if !got.Passed || len(got.Destinations) != 1 || got.Destinations[0].Archive != newest || !got.Destinations[0].Passed {
		t.Fatalf("result = %+v, want a pass on the newest archive %s", got, newest)
	}
	if len(rig.alerts) != 0 {
		t.Fatalf("a passing drill alerted: %+v", rig.alerts)
	}
}

func TestRunDrill_TestsTheNewestArchiveNotAnOlderOne(t *testing.T) {
	t.Run("a bad newest fails the drill", func(t *testing.T) {
		rig := newDrillRig(t, false)
		rig.backupAt(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC))
		newest := rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		corrupt(t, filepath.Join(rig.dir, newest))

		_, err := rig.drill(t)
		if err == nil {
			t.Fatal("RunDrill = nil for a corrupt newest archive")
		}
		got := rig.last(t)
		if got.Passed || got.Destinations[0].Archive != newest || got.Destinations[0].Error == "" {
			t.Fatalf("result = %+v, want a failure naming %s", got, newest)
		}
		if len(rig.alerts) != 1 {
			t.Fatalf("alerts = %d, want one", len(rig.alerts))
		}
	})
	t.Run("a bad older archive does not", func(t *testing.T) {
		rig := newDrillRig(t, false)
		older := rig.backupAt(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC))
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		corrupt(t, filepath.Join(rig.dir, older))

		if _, err := rig.drill(t); err != nil {
			t.Fatalf("RunDrill: %v", err)
		}
	})
}

func TestRunDrill_ReadsTheDestinationNotTheCopyItWroteAtBackupTime(t *testing.T) {
	rig := newDrillRig(t, false)
	name := rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
	// The archive verified when it was written. What sits on the destination
	// now is what a restore would get.
	corrupt(t, filepath.Join(rig.dir, name))
	if _, err := rig.drill(t); err == nil {
		t.Fatal("RunDrill passed an archive that no longer verifies on the destination")
	}
}

func TestRunDrill_OpensAnEncryptedArchiveThroughItsSidecarAndThePassphrase(t *testing.T) {
	t.Run("passes", func(t *testing.T) {
		rig := newDrillRig(t, true)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		if _, err := rig.drill(t); err != nil {
			t.Fatalf("RunDrill: %v", err)
		}
		if got := rig.last(t); !got.Passed || !strings.HasSuffix(got.Destinations[0].Archive, ".tar.zst.age") {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("a missing sidecar fails it although the box could still decrypt it itself", func(t *testing.T) {
		rig := newDrillRig(t, true)
		name := rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		if err := os.Remove(filepath.Join(rig.dir, name+identitySidecarSuffix)); err != nil {
			t.Fatal(err)
		}
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed an encrypted archive whose sidecar is gone")
		}
		if got := rig.last(t); got.Passed || !strings.Contains(got.Destinations[0].Error, "sidecar") {
			t.Fatalf("result = %+v, want a failure naming the sidecar", got)
		}
	})
	t.Run("a passphrase that no longer opens it fails it", func(t *testing.T) {
		rig := newDrillRig(t, true)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		rig.svc.Secrets = &FakeSecretSource{Passphrase: "another-passphrase", HasPass: true}
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed with a passphrase the archive was not encrypted under")
		}
	})
	t.Run("no passphrase at all fails it", func(t *testing.T) {
		rig := newDrillRig(t, true)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		rig.svc.Secrets = &FakeSecretSource{}
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed an encrypted archive with no backup passphrase")
		}
	})
}

func TestRunDrill_ADrillThatCannotFindAnArchiveFails(t *testing.T) {
	t.Run("an empty destination", func(t *testing.T) {
		rig := newDrillRig(t, false)
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed a destination with no archive")
		}
		got := rig.last(t)
		if got.Passed || got.Destinations[0].Archive != "" || !strings.Contains(got.Destinations[0].Error, "no config archive") {
			t.Fatalf("result = %+v", got)
		}
		if len(rig.alerts) != 1 {
			t.Fatalf("alerts = %d, want one", len(rig.alerts))
		}
	})
	t.Run("a destination that cannot be listed", func(t *testing.T) {
		rig := newDrillRig(t, false)
		file := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		rig.svc.Destinations[0].Path = file
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed a destination that cannot be listed")
		}
		if len(rig.alerts) != 1 {
			t.Fatalf("alerts = %d, want one", len(rig.alerts))
		}
	})
	t.Run("no enabled destination", func(t *testing.T) {
		rig := newDrillRig(t, false)
		rig.svc.Destinations[0].Enabled = false
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed with nothing to test")
		}
		got := rig.last(t)
		if got.Passed || got.Error == "" || len(got.Destinations) != 0 {
			t.Fatalf("result = %+v", got)
		}
		if len(rig.alerts) != 1 {
			t.Fatalf("alerts = %d, want one", len(rig.alerts))
		}
	})
	t.Run("a pool destination while the pool is not mounted", func(t *testing.T) {
		rig := newDrillRig(t, false)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		rig.svc.PoolRoot = filepath.Dir(rig.dir)
		rig.svc.PoolMounted = func(string) (bool, error) { return false, nil }
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill passed a destination it was not allowed to read")
		}
		if got := rig.last(t); got.Passed || !strings.Contains(got.Destinations[0].Error, "not mounted") {
			t.Fatalf("result = %+v", got)
		}
	})
}

func TestRunDrill_OnlyTestsArchivesThisInstallationWrote(t *testing.T) {
	rig := newDrillRig(t, false)
	mine := rig.backupAt(t, time.Date(2026, 9, 10, 3, 0, 0, 0, time.UTC))
	other := "hoserva-config-ffffffffffff-2026-09-14T03-00-00.tar.zst"
	if err := os.WriteFile(filepath.Join(rig.dir, other), []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	newer := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(rig.dir, other), newer, newer); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.drill(t); err != nil {
		t.Fatalf("RunDrill: %v", err)
	}
	if got := rig.last(t); got.Destinations[0].Archive != mine {
		t.Fatalf("tested %q, want this installation's %q", got.Destinations[0].Archive, mine)
	}
}

func TestRunDrill_EveryEnabledDestinationIsTested(t *testing.T) {
	rig := newDrillRig(t, false)
	rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
	second := t.TempDir()
	rig.svc.Destinations = append(rig.svc.Destinations, Destination{ID: "second", Name: "Second", Path: second, Enabled: true})
	// The second destination is enabled but has nothing on it.
	if _, err := rig.drill(t); err == nil {
		t.Fatal("RunDrill passed although one destination could not be tested")
	}
	got := rig.last(t)
	if got.Passed || len(got.Destinations) != 2 || !got.Destinations[0].Passed || got.Destinations[1].Passed {
		t.Fatalf("result = %+v, want the first destination passed and the second failed", got)
	}
	if len(rig.alerts) != 1 {
		t.Fatalf("alerts = %d, want one", len(rig.alerts))
	}
}

func TestRunDrill_ADisabledDestinationIsNotTested(t *testing.T) {
	rig := newDrillRig(t, false)
	rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
	rig.svc.Destinations = append(rig.svc.Destinations, Destination{ID: "off", Name: "Off", Path: t.TempDir(), Enabled: false})
	if _, err := rig.drill(t); err != nil {
		t.Fatalf("RunDrill: %v", err)
	}
	if got := rig.last(t); len(got.Destinations) != 1 {
		t.Fatalf("result = %+v, want only the enabled destination", got)
	}
}

func TestRunDrill_FetchesFromARemoteDestination(t *testing.T) {
	rig := newRemoteRig(t)
	ctx := context.Background()
	drills := &FakeDrillStore{}
	rig.svc.Drills = drills
	if _, err := rig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	if err := rig.svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	filesBefore := rig.rclone.Files()
	callsBefore := len(rig.rclone.Calls())

	t.Setenv("TMPDIR", t.TempDir())
	if err := rig.svc.RunDrill(ctx, nil, nil); err != nil {
		t.Fatalf("RunDrill: %v", err)
	}
	got, _ := drills.LastDrill(ctx)
	if got == nil || !got.Passed || !strings.HasSuffix(got.Destinations[0].Archive, ".tar.zst.age") {
		t.Fatalf("result = %+v", got)
	}
	if !reflect.DeepEqual(filesBefore, rig.rclone.Files()) {
		t.Fatal("the drill changed the remote")
	}
	for _, c := range rig.rclone.Calls()[callsBefore:] {
		switch c.Args[0] {
		case "obscure", "lsjson", "copyto":
		default:
			t.Fatalf("the drill ran rclone %q, want only listing and downloading", c.Args)
		}
	}

	for key := range rig.rclone.Files() {
		if strings.HasSuffix(key, ".tar.zst.age") {
			rig.rclone.Put("HOSERVADEST:bucket/hoserva", filepath.Base(key), []byte("garbage"), rig.now.Add(time.Hour))
		}
	}
	if err := rig.svc.RunDrill(ctx, nil, nil); err == nil {
		t.Fatal("RunDrill passed a remote archive that no longer decrypts")
	}
}

func TestRunDrill_FailureIsRecordedAndAlertedWhicheverPartFails(t *testing.T) {
	failing := func(t *testing.T) *drillRig {
		rig := newDrillRig(t, false)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		corrupt(t, filepath.Join(rig.dir, archiveName(rig.svc.installationID(), time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC), ReasonNone, 0)))
		return rig
	}

	t.Run("the result cannot be recorded, the alert is still sent", func(t *testing.T) {
		rig := failing(t)
		rig.drills.RecordErr = errors.New("database is locked")
		_, err := rig.drill(t)
		if err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "restore drill failed") {
			t.Fatalf("err = %v, want both the drill failure and the recording failure", err)
		}
		if len(rig.alerts) != 1 {
			t.Fatalf("alerts = %d, want one", len(rig.alerts))
		}
	})
	t.Run("the alert cannot be sent, the result is still recorded", func(t *testing.T) {
		rig := failing(t)
		rig.alert = func(context.Context, DrillResult) error { return errors.New("smtp down") }
		_, err := rig.drill(t)
		if err == nil || !strings.Contains(err.Error(), "smtp down") {
			t.Fatalf("err = %v, want the alert failure reported", err)
		}
		if got := rig.last(t); got.Passed {
			t.Fatalf("result = %+v, want the failure recorded", got)
		}
	})
	t.Run("a pass whose result cannot be recorded is an error but no alert", func(t *testing.T) {
		rig := newDrillRig(t, false)
		rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
		rig.drills.RecordErr = errors.New("database is locked")
		if _, err := rig.drill(t); err == nil {
			t.Fatal("RunDrill = nil although its result was not recorded")
		}
		if len(rig.alerts) != 0 {
			t.Fatalf("a passing drill alerted: %+v", rig.alerts)
		}
	})
}

func TestRunDrill_ACancelledDrillConcludesNothing(t *testing.T) {
	rig := newDrillRig(t, false)
	rig.backupAt(t, time.Date(2026, 9, 12, 3, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := rig.svc.RunDrill(ctx, nil, rig.alert)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunDrill = %v, want the cancellation", err)
	}
	if got, _ := rig.drills.LastDrill(context.Background()); got != nil {
		t.Fatalf("a cancelled drill recorded %+v", got)
	}
	if len(rig.alerts) != 0 {
		t.Fatalf("a cancelled drill alerted: %+v", rig.alerts)
	}
}

func TestFailDrill_RecordsAndAlertsOnADrillThatNeverStarted(t *testing.T) {
	rig := newDrillRig(t, false)
	err := rig.svc.FailDrill(context.Background(), errors.New("the job scheduler refused it"), rig.alert)
	if err == nil {
		t.Fatal("FailDrill = nil")
	}
	got := rig.last(t)
	if got.Passed || !strings.Contains(got.Error, "refused") || len(rig.alerts) != 1 {
		t.Fatalf("result = %+v, alerts = %d", got, len(rig.alerts))
	}
}

func TestLastDrill_NilBeforeTheFirstDrill(t *testing.T) {
	rig := newDrillRig(t, false)
	if got, err := rig.svc.LastDrill(context.Background()); err != nil || got != nil {
		t.Fatalf("LastDrill = %v, %v; want nil, nil", got, err)
	}
}

// betweenListAndFetch runs hook once, at the drill's first download from
// the remote, after the destination was listed and before the archive is
// fetched.
//
// It also reports, once armed, when a config backup deletes an archive, and
// records how long the rig's first backup took.
type betweenListAndFetch struct {
	inner RcloneRunner
	once  sync.Once
	hook  func(ctx context.Context)

	backupTook time.Duration
	armed      atomic.Bool
	pruneOnce  sync.Once
	pruned     chan struct{}
}

func (r *betweenListAndFetch) Run(ctx context.Context, c RcloneCommand) ([]byte, error) {
	if len(c.Args) > 0 {
		switch {
		case c.Args[0] == "copyto":
			r.once.Do(func() { r.hook(ctx) })
		case c.Args[0] == "deletefile" && r.armed.Load():
			r.pruneOnce.Do(func() { close(r.pruned) })
		}
	}
	return r.inner.Run(ctx, c)
}

func remoteDrillRig(t *testing.T, retention Retention) (*remoteRig, *FakeDrillStore, *betweenListAndFetch, *[]DrillResult) {
	t.Helper()
	rig := newRemoteRig(t)
	rig.now = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	rig.rclone.Now = func() time.Time { return rig.now }
	drills := &FakeDrillStore{}
	rig.svc.Drills = drills
	gate := &betweenListAndFetch{inner: rig.rclone, pruned: make(chan struct{})}
	rig.svc.Rclone = gate
	req := s3Request()
	req.Retention = &retention
	if _, err := rig.svc.AddDestination(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := rig.svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gate.backupTook = time.Since(start)
	t.Setenv("TMPDIR", t.TempDir())
	return rig, drills, gate, &[]DrillResult{}
}

func remoteArchives(rig *remoteRig) map[string]bool {
	out := map[string]bool{}
	for key := range rig.rclone.Files() {
		if strings.HasSuffix(key, ".tar.zst.age") {
			out[filepath.Base(key)] = true
		}
	}
	return out
}

func TestRunDrill_AConfigBackupPruningTheArchiveItPickedDoesNotFailTheDrill(t *testing.T) {
	rig, drills, gate, alerts := remoteDrillRig(t, Retention{Daily: 1})
	ctx := context.Background()
	picked := remoteArchives(rig)
	if len(picked) != 1 {
		t.Fatalf("archives after the first backup = %v, want one", picked)
	}

	backupDone := make(chan error, 1)
	gate.hook = func(context.Context) {
		rig.now = rig.now.Add(24 * time.Hour)
		gate.armed.Store(true)
		go func() { backupDone <- rig.svc.Run(context.Background()) }()
		// A backup that is not held back prunes about as soon as the rig's
		// first backup took to finish; give it twice that.
		select {
		case <-gate.pruned:
		case <-time.After(2*gate.backupTook + time.Second):
		}
	}

	err := rig.svc.RunDrill(ctx, nil, func(_ context.Context, r DrillResult) error {
		*alerts = append(*alerts, r)
		return nil
	})
	if err != nil {
		t.Fatalf("RunDrill: %v", err)
	}
	got, _ := drills.LastDrill(ctx)
	if got == nil || !got.Passed || !picked[got.Destinations[0].Archive] {
		t.Fatalf("result = %+v, want a pass on the archive the drill picked (%v)", got, picked)
	}
	if len(*alerts) != 0 {
		t.Fatalf("the drill alerted although the archive was only pruned: %+v", *alerts)
	}
	if err := <-backupDone; err != nil {
		t.Fatalf("the concurrent config backup: %v", err)
	}
	after := remoteArchives(rig)
	if len(after) != 1 || after[got.Destinations[0].Archive] {
		t.Fatalf("archives after the concurrent backup = %v, want only the new one (retention keeps one)", after)
	}
}

func TestRunDrill_AnArchiveGoneFromTheDestinationStillFailsTheDrill(t *testing.T) {
	rig, drills, gate, alerts := remoteDrillRig(t, Retention{Daily: 7})
	ctx := context.Background()
	gate.hook = func(ctx context.Context) {
		for name := range remoteArchives(rig) {
			if _, err := rig.rclone.Run(ctx, RcloneCommand{Args: []string{"deletefile", "HOSERVADEST:bucket/hoserva/" + name}}); err != nil {
				t.Errorf("deleting %s: %v", name, err)
			}
		}
	}

	err := rig.svc.RunDrill(ctx, nil, func(_ context.Context, r DrillResult) error {
		*alerts = append(*alerts, r)
		return nil
	})
	if err == nil {
		t.Fatal("RunDrill passed although the archive it picked was removed by something other than a backup")
	}
	got, _ := drills.LastDrill(ctx)
	if got == nil || got.Passed || !strings.Contains(got.Destinations[0].Error, "fetching") {
		t.Fatalf("result = %+v, want a failed drill naming the fetch", got)
	}
	if len(*alerts) != 1 {
		t.Fatalf("alerts = %d, want the failed drill alerted", len(*alerts))
	}
}
