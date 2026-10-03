package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

// wireDataDisk wires the migrator with one data disk, WIREDSERIAL, that the fake
// mounter shows holding the given top-level directories, and returns the pieces a
// test inspects.
func wireDataDisk(t *testing.T, w *containersWiringHarness, mount func(where string) error) (*disk.FakeReadOnlyMounter, *disk.FakeRunner) {
	t.Helper()
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40, Filesystem: "xfs", FSDevice: "/dev/sdb1", FSUUID: "11111111-2222-4333-8444-555555555555"})
	mounter := disk.NewFakeReadOnlyMounter()
	mounter.OnMount = mount
	runner := disk.NewFakeRunner()
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, mounter, runner, store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	return mounter, runner
}

func writeFiles(root string, files map[string]string) error {
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

type reportRows struct {
	Phase  string `json:"phase"`
	Report struct {
		Verdict string `json:"verdict"`
		Rows    []struct {
			Check   string `json:"check"`
			Status  string `json:"status"`
			Subject string `json:"subject"`
			Detail  string `json:"detail"`
		} `json:"rows"`
	} `json:"report"`
}

func (r reportRows) has(check, status, subject, contains string) bool {
	for _, row := range r.Report.Rows {
		if row.Check == check && row.Status == status && row.Subject == subject && strings.Contains(row.Detail, contains) {
			return true
		}
	}
	return false
}

// A scan queued over HTTP runs the real daemon's wiring: the data disk is checked
// through the runner, mounted read-only at a private mountpoint under the state
// directory and unmounted again, its baseline is a file beside the session, and
// the directories read from it tell a real share from the config of one that no
// longer exists.
func TestMigrationWiring_TheScanReadsTheDataDisksAndFindsOrphanShareConfigs(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounter, runner := wireDataDisk(t, w, func(where string) error {
		return writeFiles(where, map[string]string{"media/film.mkv": "film", "media/sub/x": "x"})
	})
	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/shares/media.cfg": "shareAllocator=\"mostfree\"\nshareUseCache=\"no\"\nshareExport=\"e\"\n",
		"config/shares/ghost.cfg": "shareAllocator=\"mostfree\"\nshareUseCache=\"no\"\nshareExport=\"e\"\n",
	})
	status, body := w.uploadScan(t, zipData, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct{ ID string }
	_ = json.Unmarshal(body, &queued)
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
	if done.Progress == nil || *done.Progress != 100 {
		t.Errorf("the job's progress = %v, want the scan to have reported 100", done.Progress)
	}

	status, body = w.do(t, http.MethodGet, "/migrate")
	var got reportRows
	if err := json.Unmarshal(body, &got); status != http.StatusOK || err != nil || got.Phase != "scanned" {
		t.Fatalf("GET /migrate = %d %s (%v)", status, body, err)
	}
	if !got.has("disk_integrity", "pass", "disk1", "is clean") {
		t.Errorf("no passing integrity row for disk1: %s", body)
	}
	if !got.has("shares", "info", "ghost", "is an orphan") {
		t.Errorf("the config with no directory on any disk was not called an orphan: %s", body)
	}
	if !got.has("shares", "info", "media", "directory on disk1") {
		t.Errorf("media was not found on disk1: %s", body)
	}
	if !got.has("baseline", "info", "disk1", "2 files") {
		t.Errorf("no baseline row for disk1: %s", body)
	}

	if !argvRan(runner, "xfs_repair", "-n", "/dev/sdb1") {
		t.Errorf("the filesystem check was not run on the data disk: %v", runner.Calls())
	}
	where := filepath.Join(w.root, "migrate", "mnt", "data")
	if len(mounter.Mounts) == 0 {
		t.Fatal("the data disk was never mounted")
	}
	for _, m := range mounter.Mounts {
		if m.FSType != "xfs" || m.Device != "/dev/sdb1" || m.Where != where {
			t.Errorf("mount %+v, want xfs of /dev/sdb1 at %s", m, where)
		}
	}
	if p := mounter.MountedPaths(); len(p) != 0 {
		t.Errorf("still mounted after the job: %v", p)
	}
	baselines, _ := filepath.Glob(filepath.Join(w.root, "migrate", "baseline-*.jsonl.gz"))
	if len(baselines) != 1 {
		t.Fatalf("baseline files = %v, want one", baselines)
	}
	if info, _ := os.Stat(baselines[0]); info.Mode().Perm()&0o077 != 0 {
		t.Errorf("the baseline is %v, want private", info.Mode().Perm())
	}
	if scratch, _ := filepath.Glob(filepath.Join(w.root, "migrate", "tmp-*")); len(scratch) != 0 {
		t.Errorf("scratch space left: %v", scratch)
	}

	if status, body := w.do(t, http.MethodDelete, "/migrate"); status != http.StatusNoContent {
		t.Fatalf("DELETE /migrate = %d %s", status, body)
	}
	if left, _ := filepath.Glob(filepath.Join(w.root, "migrate", "baseline-*")); len(left) != 0 {
		t.Errorf("forgetting the session left the baseline: %v", left)
	}
}

func argvRan(r *disk.FakeRunner, name string, args ...string) bool {
	for _, c := range r.Calls() {
		if c.Name == name && strings.Join(c.Args, " ") == strings.Join(args, " ") {
			return true
		}
	}
	return false
}

// A disk that fails its check is refused over HTTP too, and is never mounted.
func TestMigrationWiring_ADataDiskThatFailsItsCheckIsRefusedAndNeverMounted(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounter, runner := wireDataDisk(t, w, func(where string) error { return writeFiles(where, map[string]string{"media/a": "a"}) })
	runner.Script("xfs_repair", []string{"-n", "/dev/sdb1"}, nil, os.ErrInvalid)
	status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct{ ID string }
	_ = json.Unmarshal(body, &queued)
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}
	_, body = w.do(t, http.MethodGet, "/migrate")
	var got reportRows
	_ = json.Unmarshal(body, &got)
	if !got.has("disk_integrity", "refuse", "disk1", "disk1 is not adopted") || got.Report.Verdict != "no_go" {
		t.Errorf("the failed check was not refused: %s", body)
	}
	if len(mounter.Mounts) != 0 {
		t.Errorf("the refused disk was mounted: %v", mounter.Mounts)
	}
}

// The scan job is cancellable: cancelling it while a data disk is mounted stops
// the read and releases the mount.
func TestMigrationWiring_CancellingTheScanReleasesTheMountedDisk(t *testing.T) {
	w := newContainersWiringHarness(t)
	mounted := make(chan struct{})
	release := make(chan struct{})
	mounter, _ := wireDataDisk(t, w, func(where string) error {
		if err := writeFiles(where, map[string]string{"media/a": "a"}); err != nil {
			return err
		}
		close(mounted)
		<-release
		return nil
	})
	status, body := w.uploadScan(t, flashBackupZip(t, "7.3.2"), false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct{ ID string }
	_ = json.Unmarshal(body, &queued)
	select {
	case <-mounted:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan never mounted the data disk")
	}
	if _, err := w.scheduler.Cancel(context.Background(), queued.ID); err != nil {
		t.Fatalf("Cancel: %v (the scan job must be cancellable)", err)
	}
	close(release)
	done := w.awaitJobByID(t, queued.ID)
	if done.Status != job.StatusCancelled {
		t.Fatalf("the cancelled scan = %s %s, want cancelled", done.Status, done.ErrorMessage)
	}
	if p := mounter.MountedPaths(); len(p) != 0 {
		t.Errorf("still mounted after the scan was cancelled: %v", p)
	}
	if left, _ := filepath.Glob(filepath.Join(w.root, "migrate", "baseline-*")); len(left) != 0 {
		t.Errorf("a cancelled scan left %v", left)
	}
	if _, body := w.do(t, http.MethodGet, "/migrate"); !bytes.Contains(body, []byte(`"scan_failed"`)) {
		t.Errorf("GET /migrate after a cancelled scan = %s, want scan_failed", body)
	}
}
