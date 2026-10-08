package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"

	"github.com/mdg-labs/hoserva/internal/beneath"
	"github.com/mdg-labs/hoserva/internal/container"
)

// restoreRig is an appdataRig with one running container, alpha, whose
// backup already sits on the destination the test restores from.
type restoreRig struct {
	*appdataRig
	dir     string
	archive AppdataArchive
}

// newRestoreRig backs alpha up (config = "v1") to a single remote
// destination — a remote so a test can make uploads fail while the archive
// can still be fetched — then changes its live appdata so a restore has
// something to replace.
func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	return newRestoreRigOn(t, newAppdataRig(t))
}

func newRestoreRigOn(t *testing.T, rig *appdataRig) *restoreRig {
	t.Helper()
	ctx := context.Background()
	if err := rig.store.DeleteDestination(ctx, DefaultPoolID); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.remoteRig.svc.AddDestination(ctx, s3Request()); err != nil {
		t.Fatal(err)
	}
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "v1", "sub/keep": "kept"})
	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	archives, unavailable, err := rig.svc.ListArchives(ctx, "alpha")
	if err != nil || len(unavailable) != 0 || len(archives) != 1 {
		t.Fatalf("ListArchives = %v, %v, %v", archives, unavailable, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("v2-live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra"), []byte("added after the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	rig.containers.mu.Lock()
	rig.containers.events = nil
	rig.containers.mu.Unlock()
	return &restoreRig{appdataRig: rig, dir: dir, archive: archives[0]}
}

func (r *restoreRig) restore(t *testing.T) error {
	t.Helper()
	r.out.Reset()
	return r.svc.Restore(context.Background(), AppdataRestoreRequest{
		Container: "alpha", Archive: r.archive.Name, DestinationID: r.archive.DestinationID,
	}, r.out)
}

func (r *restoreRig) requireLiveUntouched(t *testing.T) {
	t.Helper()
	if got := readFile(t, filepath.Join(r.dir, "config")); got != "v2-live" {
		t.Fatalf("live config = %q, want the untouched v2-live", got)
	}
	if got := readFile(t, filepath.Join(r.dir, "extra")); got != "added after the backup" {
		t.Fatalf("live extra = %q", got)
	}
	entries, err := os.ReadDir(r.appdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "alpha" {
			t.Fatalf("restore left %s next to the live appdata", e.Name())
		}
	}
}

func TestAppdataRestore_RefusesToTouchLiveAppdataWhenTheSnapshotCannotBeWritten(t *testing.T) {
	rig := newRestoreRig(t)
	rig.rclone.Fail = map[string]error{"copy": errors.New("remote down")}

	err := rig.restore(t)
	if !errors.Is(err, ErrPreRestoreSnapshot) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot", err)
	}
	rig.requireLiveUntouched(t)
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alpha.State)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
}

func TestAppdataRestore_ReplacesAppdataAfterSnapshottingTheCurrentState(t *testing.T) {
	rig := newRestoreRig(t)
	// A second, working destination takes the snapshot.
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	if got := readFile(t, filepath.Join(rig.dir, "sub", "keep")); got != "kept" {
		t.Fatalf("sub/keep = %q", got)
	}
	if _, err := os.Stat(filepath.Join(rig.dir, "extra")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a file added after the backup survived the restore: %v", err)
	}
	entries, _ := os.ReadDir(rig.appdata)
	if len(entries) != 1 {
		t.Fatalf("appdata holds %d entries after the restore, want alpha only", len(entries))
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,start alpha"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}

	var snapshot string
	for _, n := range rig.archives(t, rig.poolDir) {
		if _, _, reason, _, ok := parseAppdataName(n); ok && reason == ReasonPreRestore {
			snapshot = filepath.Join(rig.poolDir, n)
		}
	}
	if snapshot == "" {
		t.Fatalf("no pre-restore snapshot on the pool destination: %v", rig.archives(t, rig.poolDir))
	}
	hdr, trailer, err := verifyAppdata(snapshot)
	if err != nil {
		t.Fatalf("verifying the snapshot: %v", err)
	}
	if hdr.Reason != string(ReasonPreRestore) || trailer.Files != 3 {
		t.Fatalf("snapshot header %+v trailer %+v, want the 3 live files", hdr, trailer)
	}
	extracted := filepath.Join(t.TempDir(), "x")
	if err := extractAppdata(context.Background(), snapshot, hdr, []string{extracted}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(extracted, "config")); got != "v2-live" {
		t.Fatalf("the snapshot holds config = %q, want the state before the restore", got)
	}
}

func TestAppdataRestore_RestoresIntoAnAbsentDirectoryWithoutWarning(t *testing.T) {
	rig := newRestoreRig(t)
	if err := os.RemoveAll(rig.dir); err != nil {
		t.Fatal(err)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	if got := readFile(t, filepath.Join(rig.dir, "sub", "keep")); got != "kept" {
		t.Fatalf("sub/keep = %q", got)
	}
	if strings.Contains(rig.out.String(), "warning:") {
		t.Fatalf("a restore into an absent directory warned:\n%s", rig.out)
	}
}

func TestAppdataRestore_CorruptArchiveIsRefusedBeforeAnythingIsStopped(t *testing.T) {
	rig := newRestoreRig(t)
	for key, data := range rig.rclone.Files() {
		if strings.HasSuffix(key, ".tar.zst.age") {
			i := strings.LastIndex(key, "/")
			data[len(data)/2] ^= 0xff
			rig.rclone.Put(key[:i], key[i+1:], data, rig.now)
		}
	}
	if err := rig.restore(t); err == nil {
		t.Fatal("Restore of a corrupt archive succeeded")
	}
	rig.requireLiveUntouched(t)
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("a container was stopped for an archive that never verified: %v", ev)
	}
}

func TestAppdataRestore_RefusesWhileTheArrayIsStopped(t *testing.T) {
	rig := newRestoreRig(t)
	rig.halted = true
	if err := rig.restore(t); err == nil {
		t.Fatal("Restore with the array stopped succeeded")
	}
	rig.requireLiveUntouched(t)
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched: %v", ev)
	}
}

func TestAppdataRestore_RefusesAnArchiveOfAnotherContainerOrAnUnknownOne(t *testing.T) {
	rig := newRestoreRig(t)
	ctx := context.Background()
	req := AppdataRestoreRequest{Container: "beta", Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID}
	if err := rig.svc.Restore(ctx, req, rig.out); !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore of alpha's archive as beta = %v, want ErrAppdataArchiveInvalid", err)
	}
	req = AppdataRestoreRequest{Container: "alpha", Archive: strings.Replace(rig.archive.Name, "-2026", "-2025", 1), DestinationID: rig.archive.DestinationID}
	if err := rig.svc.Restore(ctx, req, rig.out); !errors.Is(err, ErrAppdataArchiveNotFound) {
		t.Fatalf("Restore of a missing archive = %v, want ErrAppdataArchiveNotFound", err)
	}
	req = AppdataRestoreRequest{Container: "alpha", Archive: "../../etc/passwd", DestinationID: rig.archive.DestinationID}
	if err := rig.svc.Restore(ctx, req, rig.out); !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore of a path = %v, want ErrAppdataArchiveInvalid", err)
	}
	req = AppdataRestoreRequest{Container: "alpha", Archive: rig.archive.Name, DestinationID: "nope"}
	if err := rig.svc.Restore(ctx, req, rig.out); !errors.Is(err, ErrDestinationNotFound) {
		t.Fatalf("Restore from an unknown destination = %v, want ErrDestinationNotFound", err)
	}
	rig.requireLiveUntouched(t)
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched: %v", ev)
	}
}

func TestAppdataRestore_RefusesAnArchiveWhoseDirectoriesAreOutsideAppdata(t *testing.T) {
	rig := newRestoreRig(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "victim"), []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(rig.root, "local")
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: "local", Name: "Local", Type: TypeLocal, Path: dest, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	name := appdataArchiveName(rig.remoteRig.svc.installationID(), "alpha", rig.now, ReasonNone, 0)
	if _, err := packAppdata(context.Background(), filepath.Join(dest, name), appdataHeader{
		Container: "alpha", CreatedAt: rig.now, Dirs: []string{outside},
	}, rig.key(t)); err != nil {
		t.Fatal(err)
	}
	err := rig.svc.Restore(context.Background(), AppdataRestoreRequest{Container: "alpha", Archive: name, DestinationID: "local"}, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "not inside the appdata location") {
		t.Fatalf("Restore = %v, want ErrAppdataArchiveInvalid for a directory outside the appdata location", err)
	}
	if got := readFile(t, filepath.Join(outside, "victim")); got != "precious" {
		t.Fatalf("a directory outside appdata was changed: %q", got)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched: %v", ev)
	}
}

func TestAppdataRestore_RestartsTheContainerWhenExtractionFails(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	// A read-only appdata location: the archive verifies and the snapshot
	// is written, then unpacking fails creating the temporary tree next to
	// the live directory, before any swap is attempted.
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	if err := os.Chmod(rig.appdata, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(rig.appdata, 0o755) })

	if err := rig.restore(t); err == nil {
		t.Fatal("Restore into a read-only appdata location succeeded")
	}
	if err := os.Chmod(rig.appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	rig.requireLiveUntouched(t)
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the failed restore, want running", alpha.State)
	}
}

func swapAppdataDirs(swaps []appdataSwap) error {
	dirs := &heldDirs{}
	defer dirs.close()
	for i := range swaps {
		id, err := dirs.recordLive(swaps[i].live)
		if err != nil {
			return err
		}
		swaps[i].recorded = id
		if swaps[i].tree, err = dirs.recordLive(swaps[i].fresh); err != nil {
			return err
		}
	}
	return dirs.swap(swaps)
}

func TestSwapAppdataDirs_RollsBackWhatItAlreadySwappedWhenALaterOneFails(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	live1, fresh1 := filepath.Join(root, "one"), filepath.Join(root, "one.new")
	live2, fresh2 := filepath.Join(root, "two"), filepath.Join(root, "two.new")
	write(filepath.Join(live1, "f"), "live1")
	write(filepath.Join(live2, "f"), "live2")
	write(filepath.Join(fresh1, "f"), "fresh1")
	// fresh2 is missing, so the second swap fails.

	err := swapAppdataDirs([]appdataSwap{
		{live: live1, fresh: fresh1, old: filepath.Join(root, "one.old")},
		{live: live2, fresh: fresh2, old: filepath.Join(root, "two.old")},
	})
	if err == nil {
		t.Fatal("swapAppdataDirs succeeded although the second fresh tree is missing")
	}
	if got := readFile(t, filepath.Join(live1, "f")); got != "live1" {
		t.Fatalf("first directory = %q after the rollback, want live1", got)
	}
	if got := readFile(t, filepath.Join(live2, "f")); got != "live2" {
		t.Fatalf("second directory = %q, want live2", got)
	}
	if got := readFile(t, filepath.Join(fresh1, "f")); got != "fresh1" {
		t.Fatalf("the first fresh tree = %q, want it back where it was", got)
	}
}

func TestAppdataRestore_KeepsAnEarlierRunsStoppedContainerInTheJournal(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	rig.addApp(t, "beta", "radarr", "exited", map[string]string{"b": "1"})
	if err := rig.svc.writeJournal([]string{"beta"}); err != nil {
		t.Fatal(err)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, rig.svc.JournalPath); !strings.Contains(got, "beta") || strings.Contains(got, "alpha") {
		t.Fatalf("journal = %s, want beta (an earlier run's) and not alpha", got)
	}
	if err := rig.svc.RecoverStopped(context.Background()); err != nil {
		t.Fatalf("RecoverStopped: %v", err)
	}
	beta, _ := rig.engine.Inspect(context.Background(), "beta")
	if beta.State != "running" {
		t.Fatalf("beta = %s after recovery, want running", beta.State)
	}
}

// A backup of one container and a restore of another, arriving together in
// either order, both succeed: whichever comes second waits, leaves the
// first's staging alone, and neither loses the other's journal entry.
func TestAppdataRestore_ABackupOfAnotherContainerWaitsAndBothSucceed(t *testing.T) {
	for _, restoreFirst := range []bool{true, false} {
		rig := newRestoreRig(t)
		if err := rig.store.CreateDestination(context.Background(), Destination{
			ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
			Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
		}); err != nil {
			t.Fatal(err)
		}
		rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})
		heldName, otherName := "alpha", "beta"
		if !restoreFirst {
			heldName, otherName = "beta", "alpha"
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.containers.onStop = func(name string) {
			if name == heldName {
				close(entered)
				<-release
			}
		}
		backupOf := func() error {
			return runNamed(context.Background(), rig.svc, &bytes.Buffer{}, "beta")
		}
		restoreOf := func() error {
			return rig.svc.Restore(context.Background(), AppdataRestoreRequest{
				Container: "alpha", Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID,
			}, &bytes.Buffer{})
		}
		first, second := restoreOf, backupOf
		if !restoreFirst {
			first, second = backupOf, restoreOf
		}
		firstDone := make(chan error, 1)
		go func() { firstDone <- first() }()
		<-entered
		secondDone := make(chan error, 1)
		go func() { secondDone <- second() }()

		time.Sleep(200 * time.Millisecond)
		for _, ev := range rig.containers.Events() {
			if ev == "stop "+otherName {
				t.Fatalf("restoreFirst=%v: %s was stopped while the other job held the service", restoreFirst, otherName)
			}
		}
		if _, err := os.Stat(filepath.Join(rig.cache, appdataStagingDir)); err != nil {
			t.Fatalf("restoreFirst=%v: the running job's staging directory is gone: %v", restoreFirst, err)
		}
		close(release)
		if err := <-firstDone; err != nil {
			t.Fatalf("restoreFirst=%v: first job: %v", restoreFirst, err)
		}
		if err := <-secondDone; err != nil {
			t.Fatalf("restoreFirst=%v: second job: %v", restoreFirst, err)
		}
		if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
			t.Fatalf("restoreFirst=%v: config = %q, want the restored v1", restoreFirst, got)
		}
		rig.archiveFor(t, rig.poolDir, "beta")
		for _, n := range []string{"alpha", "beta"} {
			if c, _ := rig.engine.Inspect(context.Background(), n); c.State != "running" {
				t.Fatalf("restoreFirst=%v: %s = %s, want running", restoreFirst, n, c.State)
			}
		}
		if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restoreFirst=%v: journal left behind: %v", restoreFirst, err)
		}
	}
}

// addSharer adds a running container that bind-mounts alpha's sub
// directory, the way a transcoder mounts part of a media server's appdata.
func (r *restoreRig) addSharer(t *testing.T) {
	t.Helper()
	r.engine.AddContainer(container.Container{
		ID: "id-transcoder", Name: "transcoder", Image: "example/transcoder", State: "running",
		Mounts: []container.Mount{{Source: filepath.Join(r.dir, "sub"), Destination: "/transcode", ReadWrite: true}},
	})
}

func TestAppdataRestoreSharers_NamesEveryOtherContainerMountingTheSameAppdata(t *testing.T) {
	rig := newRestoreRig(t)
	rig.addSharer(t)
	rig.addApp(t, "beta", "radarr", "running", map[string]string{"b": "1"})

	got, err := rig.svc.RestoreSharers(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "transcoder" {
		t.Fatalf("RestoreSharers(alpha) = %v, want [transcoder]", got)
	}
	got, err = rig.svc.RestoreSharers(context.Background(), "transcoder")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alpha" {
		t.Fatalf("RestoreSharers(transcoder) = %v, want [alpha], whose mount holds transcoder's", got)
	}
}

// A container sharing the restored appdata keeps its bind mount on the tree
// the restore renames away and deletes, so it is stopped with the restored
// container and started again after it.
func TestAppdataRestore_StopsAContainerSharingTheRestoredAppdata(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	rig.addSharer(t)
	sharers, err := rig.svc.RestoreSharers(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	rig.out.Reset()
	if err := rig.svc.Restore(context.Background(), AppdataRestoreRequest{
		Container: "alpha", Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID, Sharers: sharers,
	}, rig.out); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop transcoder,stop alpha,start alpha,start transcoder"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
}

// A running container that shares the restored appdata but was not in the
// job's scope when it was queued could be recreated beside the restore, so
// the restore refuses before it stops or changes anything.
func TestAppdataRestore_RefusesWhenARunningSharerIsOutsideTheJobsScope(t *testing.T) {
	rig := newRestoreRig(t)
	rig.addSharer(t)

	err := rig.restore(t)
	if err == nil || !strings.Contains(err.Error(), "transcoder") {
		t.Fatalf("Restore = %v, want a refusal naming transcoder", err)
	}
	rig.requireLiveUntouched(t)
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("events = %v, want nothing stopped", ev)
	}
	if _, err := os.Stat(rig.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal written by a refused restore: %v", err)
	}
}

// outsideTree is a directory the restore has no business with, holding a
// copy of the container's directory name so that a restore which reaches it
// by name finds something to replace.
type outsideTree struct{ dir string }

func newOutsideTree(t *testing.T) outsideTree {
	t.Helper()
	o := outsideTree{dir: t.TempDir()}
	for rel, content := range map[string]string{"alpha/config": "outside", "alpha/sub/keep": "outside-kept", "other": "outside-other"} {
		p := filepath.Join(o.dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

func (o outsideTree) listing(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(o.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content := ""
		if d.Type().IsRegular() {
			content = readFile(t, p)
		}
		b.WriteString(p + " " + info.Mode().String() + " " + content + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func (o outsideTree) requireUntouched(t *testing.T, want string) {
	t.Helper()
	if got := o.listing(t); got != want {
		t.Fatalf("the directory outside the appdata location changed:\nbefore:\n%safter:\n%s", want, got)
	}
}

// swapParentForLink moves the appdata location aside and puts a link to
// outside where it was.
func (r *restoreRig) swapParentForLink(t *testing.T, outside outsideTree) string {
	t.Helper()
	moved := r.appdata + ".moved"
	if err := os.Rename(r.appdata, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside.dir, r.appdata); err != nil {
		t.Fatal(err)
	}
	return moved
}

func TestAppdataRestore_RefusesWhenTheParentBecomesALinkAfterValidation(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	outside := newOutsideTree(t)
	want := outside.listing(t)
	var moved string
	beforeAppdataParents = func() { moved = rig.swapParentForLink(t, outside) }
	t.Cleanup(func() { beforeAppdataParents = nil })

	err := rig.restore(t)
	if !errors.Is(err, beneath.ErrSymlink) {
		t.Fatalf("Restore = %v, want a refusal because the parent is a link\n%s", err, rig.out)
	}
	outside.requireUntouched(t, want)
	if got := readFile(t, filepath.Join(moved, "alpha", "config")); got != "v2-live" {
		t.Fatalf("the live config = %q, want it untouched", got)
	}
	entries, err := os.ReadDir(moved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the live appdata location holds %d entries, want alpha only", len(entries))
	}
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alpha.State)
	}
}

func TestAppdataRestore_WorksInTheDirectoriesItOpenedWhenAParentIsSwappedForALinkLater(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	outside := newOutsideTree(t)
	want := outside.listing(t)
	var moved string
	beforeAppdataUnpack = func() { moved = rig.swapParentForLink(t, outside) }
	t.Cleanup(func() { beforeAppdataUnpack = nil })

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	outside.requireUntouched(t, want)
	if got := readFile(t, filepath.Join(moved, "alpha", "config")); got != "v1" {
		t.Fatalf("config = %q in the directory the restore opened, want the archived v1", got)
	}
	entries, err := os.ReadDir(moved)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the directory the restore opened holds %d entries after the restore, want alpha only", len(entries))
	}
}

// otherAppdata is a directory of another application, with a nested file.
type otherAppdata struct{ files map[string]string }

func newOtherAppdata() otherAppdata {
	return otherAppdata{files: map[string]string{"data": "not part of this restore", "sub/state": "also not"}}
}

func (o otherAppdata) create(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range o.files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (o otherAppdata) requireIntactAt(t *testing.T, dir string) {
	t.Helper()
	for rel, content := range o.files {
		if got := readFile(t, filepath.Join(dir, rel)); got != content {
			t.Fatalf("%s = %q, want %q", filepath.Join(dir, rel), got, content)
		}
	}
}

// putOtherAppdataInPlaceOfLive moves the live directory aside and puts the
// other application's directory at its name.
func (r *restoreRig) putOtherAppdataInPlaceOfLive(t *testing.T, other otherAppdata) (aside string) {
	t.Helper()
	aside = r.dir + ".aside"
	if err := os.Rename(r.dir, aside); err != nil {
		t.Fatal(err)
	}
	other.create(t, r.dir)
	return aside
}

func (r *restoreRig) withLocalSnapshotDestination(t *testing.T) {
	t.Helper()
	if err := r.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: r.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
}

func (r *restoreRig) requireNothingLeftNextToLive(t *testing.T, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(r.appdata)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the appdata location holds %v, want %v", got, want)
	}
}

func TestAppdataRestore_SwapsOnlyTheDirectoryIdentityRecordedBeforeTheUnpack(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	beforeAppdataUnpack = func() { aside = rig.putOtherAppdataInPlaceOfLive(t, other) }
	t.Cleanup(func() { beforeAppdataUnpack = nil })

	if err := rig.restore(t); err == nil {
		t.Fatalf("Restore succeeded over a directory other than the one it recorded\n%s", rig.out)
	}
	other.requireIntactAt(t, rig.dir)
	if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
		t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
	}
	if got := readFile(t, filepath.Join(aside, "extra")); got != "added after the backup" {
		t.Fatalf("the directory the restore started from: extra = %q", got)
	}
	rig.requireNothingLeftNextToLive(t, "alpha", "alpha.aside")
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alpha.State)
	}
}

func TestAppdataRestore_DoesNotSwapOverADirectoryThatWasAbsentWhenRecorded(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	if err := os.RemoveAll(rig.dir); err != nil {
		t.Fatal(err)
	}
	other := newOtherAppdata()
	beforeAppdataUnpack = func() { other.create(t, rig.dir) }
	t.Cleanup(func() { beforeAppdataUnpack = nil })

	if err := rig.restore(t); err == nil {
		t.Fatalf("Restore succeeded over a directory that appeared after it recorded none\n%s", rig.out)
	}
	other.requireIntactAt(t, rig.dir)
	rig.requireNothingLeftNextToLive(t, "alpha")
}

func TestAppdataRestore_PutsBackAnEntryMovedAsideThatIsNotTheRecordedOne(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	beforeAppdataMoveAside = func() {
		beforeAppdataMoveAside = nil
		aside = rig.putOtherAppdataInPlaceOfLive(t, other)
	}
	t.Cleanup(func() { beforeAppdataMoveAside = nil })

	if err := rig.restore(t); err == nil {
		t.Fatalf("Restore succeeded over a directory other than the one it recorded\n%s", rig.out)
	}
	other.requireIntactAt(t, rig.dir)
	if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
		t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
	}
	rig.requireNothingLeftNextToLive(t, "alpha", "alpha.aside")
}

// replaceEveryRestoreNameWithOtherAppdata renames each entry the restore
// created next to the live directory out of the way and puts a different
// application's directory at its name.
func (r *restoreRig) replaceEveryRestoreNameWithOtherAppdata(t *testing.T, other otherAppdata) (moved []string) {
	t.Helper()
	entries, err := os.ReadDir(r.appdata)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.Contains(e.Name(), ".hoserva-") {
			continue
		}
		p := filepath.Join(r.appdata, e.Name())
		if err := os.Rename(p, p+".moved"); err != nil {
			t.Fatal(err)
		}
		other.create(t, p)
		moved = append(moved, p)
	}
	if len(moved) == 0 {
		t.Fatal("the restore had created nothing next to the live directory to replace")
	}
	return moved
}

func TestAppdataRestore_RemovesOnlyTheDirectoryItReplacedWhenTheNameChangesBeforeTheRemoval(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var replaced []string
	beforeAppdataCleanup = func() {
		beforeAppdataCleanup = nil
		replaced = rig.replaceEveryRestoreNameWithOtherAppdata(t, other)
	}
	t.Cleanup(func() { beforeAppdataCleanup = nil })

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	for _, p := range replaced {
		other.requireIntactAt(t, p)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
}

func TestAppdataRestore_RemovesOnlyTheTreeItUnpackedWhenARefusedRestoreFindsTheNameChanged(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	beforeAppdataUnpack = func() { aside = rig.putOtherAppdataInPlaceOfLive(t, other) }
	var replaced []string
	beforeAppdataCleanup = func() {
		beforeAppdataCleanup = nil
		replaced = rig.replaceEveryRestoreNameWithOtherAppdata(t, newOtherAppdata())
	}
	t.Cleanup(func() { beforeAppdataUnpack, beforeAppdataCleanup = nil, nil })

	if err := rig.restore(t); err == nil {
		t.Fatalf("Restore succeeded over a directory other than the one it recorded\n%s", rig.out)
	}
	other.requireIntactAt(t, rig.dir)
	if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
		t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
	}
	for _, p := range replaced {
		newOtherAppdata().requireIntactAt(t, p)
	}
}

func TestSwapAppdataDirs_PutsBackOnlyTheTreesItSwappedIn(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	live1, fresh1 := filepath.Join(root, "one"), filepath.Join(root, "one.new")
	live2, fresh2 := filepath.Join(root, "two"), filepath.Join(root, "two.new")
	write(filepath.Join(live1, "f"), "live1")
	write(filepath.Join(live2, "f"), "live2")
	write(filepath.Join(fresh1, "f"), "fresh1")
	other := newOtherAppdata()
	beforeAppdataRollback = func() {
		beforeAppdataRollback = nil
		if err := os.Rename(live1, live1+".moved"); err != nil {
			t.Fatal(err)
		}
		other.create(t, live1)
	}
	t.Cleanup(func() { beforeAppdataRollback = nil })

	err := swapAppdataDirs([]appdataSwap{
		{live: live1, fresh: fresh1, old: filepath.Join(root, "one.old")},
		{live: live2, fresh: fresh2, old: filepath.Join(root, "two.old")},
	})
	if err == nil {
		t.Fatal("swapAppdataDirs succeeded although the second fresh tree is missing")
	}
	other.requireIntactAt(t, live1)
	if got := readFile(t, filepath.Join(live1+".moved", "f")); got != "fresh1" {
		t.Fatalf("the tree that was swapped in = %q, want fresh1", got)
	}
	if got := readFile(t, filepath.Join(live2, "f")); got != "live2" {
		t.Fatalf("second directory = %q, want live2", got)
	}
}

func exchangeDirs(t *testing.T, a, b string) {
	t.Helper()
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		t.Fatalf("exchanging %s and %s: %v", a, b, err)
	}
}

// otherAppdataAsTree makes a directory that holds the other application's
// directory as "tree" and returns it.
func (r *restoreRig) otherAppdataAsTree(t *testing.T, other otherAppdata) string {
	t.Helper()
	dir := r.dir + ".foreign"
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other.create(t, filepath.Join(dir, "tree"))
	return dir
}

func (r *restoreRig) requireRestoreRefusedAndLiveUntouched(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Restore succeeded although a directory other than the one it created was in its place\n%s", r.out)
	}
	r.requireLiveContent(t)
}

func (r *restoreRig) requireLiveContent(t *testing.T) {
	t.Helper()
	if got := readFile(t, filepath.Join(r.dir, "config")); got != "v2-live" {
		t.Fatalf("live config = %q, want the untouched v2-live", got)
	}
	if got := readFile(t, filepath.Join(r.dir, "extra")); got != "added after the backup" {
		t.Fatalf("live extra = %q", got)
	}
}

func TestAppdataRestore_RefusesAWorkDirectoryThatHoldsAnotherDirectory(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	foreign := rig.otherAppdataAsTree(t, other)
	var work string
	beforeAppdataWorkOpen = func(path string) {
		beforeAppdataWorkOpen = nil
		work = path
		exchangeDirs(t, path, foreign)
	}
	t.Cleanup(func() { beforeAppdataWorkOpen = nil })

	err := rig.restore(t)

	rig.requireRestoreRefusedAndLiveUntouched(t, err)
	if !strings.Contains(err.Error(), work) {
		t.Fatalf("the error does not name the work directory %s: %v", work, err)
	}
	other.requireIntactAt(t, filepath.Join(work, "tree"))
}

func TestAppdataRestore_RefusesAnEmptyWorkDirectoryWithAnotherMode(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	foreign := rig.dir + ".foreign"
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	var work string
	beforeAppdataWorkOpen = func(path string) {
		beforeAppdataWorkOpen = nil
		work = path
		exchangeDirs(t, path, foreign)
	}
	t.Cleanup(func() { beforeAppdataWorkOpen = nil })

	err := rig.restore(t)

	rig.requireRestoreRefusedAndLiveUntouched(t, err)
	if !strings.Contains(err.Error(), work) {
		t.Fatalf("the error does not name the work directory %s: %v", work, err)
	}
	entries, rerr := os.ReadDir(work)
	if rerr != nil || len(entries) != 0 {
		t.Fatalf("the directory at the work name was changed: %v, %v", entries, rerr)
	}
}

func TestAppdataRestore_RefusesATreeThatIsNotTheEmptyDirectoryItCreated(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	foreign := rig.dir + ".foreign"
	other.create(t, foreign)
	var tree string
	beforeAppdataTreeOpen = func(path string) {
		beforeAppdataTreeOpen = nil
		tree = path
		exchangeDirs(t, path, foreign)
	}
	t.Cleanup(func() { beforeAppdataTreeOpen = nil })

	err := rig.restore(t)

	rig.requireRestoreRefusedAndLiveUntouched(t, err)
	other.requireIntactAt(t, tree)
}

func TestAppdataRestore_SwapsOnlyTheTreeItUnpacked(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	foreign := rig.dir + ".foreign"
	other.create(t, foreign)
	var tree string
	beforeAppdataMoveAside = func() {
		beforeAppdataMoveAside = nil
		entries, err := os.ReadDir(rig.appdata)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".hoserva-restore-") {
				tree = filepath.Join(rig.appdata, e.Name(), "tree")
				exchangeDirs(t, tree, foreign)
			}
		}
	}
	t.Cleanup(func() { beforeAppdataMoveAside = nil })

	err := rig.restore(t)

	rig.requireRestoreRefusedAndLiveUntouched(t, err)
	if tree == "" {
		t.Fatal("the hook did not find the restore's work directory")
	}
	other.requireIntactAt(t, tree)
}

func (r *restoreRig) requireNoPreRestoreSnapshot(t *testing.T) {
	t.Helper()
	for _, n := range r.archives(t, r.poolDir) {
		if _, _, reason, _, ok := parseAppdataName(n); ok && reason == ReasonPreRestore {
			t.Fatalf("a pre-restore snapshot was written to the pool destination: %s", n)
		}
	}
}

// requireRefusedBeforeAnyWrite checks that the restore failed in the snapshot
// step, that the other application's directory is intact at the restored
// directory's name and the directory the restore started from is intact where
// it was moved, and that nothing was unpacked, swapped or snapshotted.
func (r *restoreRig) requireRefusedBeforeAnyWrite(t *testing.T, err error, other otherAppdata, aside string) {
	t.Helper()
	if !errors.Is(err, ErrPreRestoreSnapshot) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot\n%s", err, r.out)
	}
	other.requireIntactAt(t, r.dir)
	if aside != "" {
		if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
			t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
		}
		if got := readFile(t, filepath.Join(aside, "sub", "keep")); got != "kept" {
			t.Fatalf("the directory the restore started from: sub/keep = %q", got)
		}
		r.requireNothingLeftNextToLive(t, "alpha", "alpha.aside")
	} else {
		r.requireNothingLeftNextToLive(t, "alpha")
	}
	r.requireNoPreRestoreSnapshot(t)
	alpha, _ := r.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alpha.State)
	}
}

func TestAppdataRestore_RefusesToSnapshotADirectoryOtherThanTheRecordedOne(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	afterAppdataRecord = func() { aside = rig.putOtherAppdataInPlaceOfLive(t, other) }
	t.Cleanup(func() { afterAppdataRecord = nil })

	err := rig.restore(t)
	rig.requireRefusedBeforeAnyWrite(t, err, other, aside)
}

func TestAppdataRestore_RefusesASnapshotWhoseDirectoryChangesWhileItIsPacked(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	afterAppdataPack = func() { aside = rig.putOtherAppdataInPlaceOfLive(t, other) }
	t.Cleanup(func() { afterAppdataPack = nil })

	err := rig.restore(t)
	rig.requireRefusedBeforeAnyWrite(t, err, other, aside)
}

func TestAppdataRestore_RefusesToSnapshotADirectoryThatWasAbsentWhenRecorded(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	if err := os.RemoveAll(rig.dir); err != nil {
		t.Fatal(err)
	}
	other := newOtherAppdata()
	afterAppdataRecord = func() { other.create(t, rig.dir) }
	t.Cleanup(func() { afterAppdataRecord = nil })

	err := rig.restore(t)
	rig.requireRefusedBeforeAnyWrite(t, err, other, "")
}

func TestAppdataRestore_RefusesToSnapshotADirectoryThatIsGoneSinceItWasRecorded(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	aside := rig.dir + ".aside"
	afterAppdataRecord = func() {
		if err := os.Rename(rig.dir, aside); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { afterAppdataRecord = nil })

	err := rig.restore(t)
	if !errors.Is(err, ErrPreRestoreSnapshot) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot\n%s", err, rig.out)
	}
	if _, err := os.Lstat(rig.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something was put at the name the restore found gone: %v", err)
	}
	if got := readFile(t, filepath.Join(aside, "config")); got != "v2-live" {
		t.Fatalf("the directory the restore started from: config = %q, want v2-live", got)
	}
	rig.requireNothingLeftNextToLive(t, "alpha.aside")
	rig.requireNoPreRestoreSnapshot(t)
}

func TestAppdataRestore_TheRevertBeforeAnUpdateRefusesADirectoryOtherThanTheRecordedOne(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	var aside string
	afterAppdataRecord = func() { aside = rig.putOtherAppdataInPlaceOfLive(t, other) }
	t.Cleanup(func() { afterAppdataRecord = nil })

	snaps := UpdateSnapshots{Appdata: rig.svc}
	ref := container.SnapshotRef{Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID}
	rig.out.Reset()
	err := snaps.Restore(context.Background(), "alpha", ref, nil, rig.out)
	rig.requireRefusedBeforeAnyWrite(t, err, other, aside)
}

// moveOtherAppdataIntoLive renames another application's directory into the
// live directory as name.
func (r *restoreRig) moveOtherAppdataIntoLive(t *testing.T, other otherAppdata, name string) {
	t.Helper()
	from := filepath.Join(r.appdata, "beta")
	other.create(t, from)
	if err := os.Rename(from, filepath.Join(r.dir, name)); err != nil {
		t.Fatal(err)
	}
}

// workDir returns the one work directory a restore left next to the live
// directory.
func (r *restoreRig) workDir(t *testing.T) string {
	t.Helper()
	work, err := filepath.Glob(r.dir + ".hoserva-restore-*")
	if err != nil || len(work) != 1 {
		t.Fatalf("work directories = %v, %v, want exactly one", work, err)
	}
	return work[0]
}

// filesUnder lists the paths of the files under dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func TestAppdataRestore_LeavesADirectoryMovedIntoTheLiveDirectoryAfterTheSnapshot(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	beforeAppdataUnpack = func() { rig.moveOtherAppdataIntoLive(t, other, "moved") }
	t.Cleanup(func() { beforeAppdataUnpack = nil })

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	old := filepath.Join(rig.workDir(t), "old")
	moved := filepath.Join(old, "moved")
	other.requireIntactAt(t, moved)
	if got, want := strings.Join(filesUnder(t, old), ","), "moved/data,moved/sub/state"; got != want {
		t.Fatalf("the replaced directory still holds %s, want only %s: what the snapshot archived is removed", got, want)
	}
	if !strings.Contains(rig.out.String(), "warning:") || !strings.Contains(rig.out.String(), strconv.Quote(moved)) {
		t.Fatalf("the output does not warn about %s:\n%s", moved, rig.out)
	}
}

func TestAppdataRestore_LeavesAFileMovedIntoTheLiveDirectoryAndTheDirectoryHoldingIt(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	var stray, nested string
	beforeAppdataUnpack = func() {
		stray = filepath.Join(rig.appdata, "stray")
		if err := os.WriteFile(stray, []byte("another app's file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(stray, filepath.Join(rig.dir, "stray")); err != nil {
			t.Fatal(err)
		}
		nested = filepath.Join(rig.dir, "sub", "nested")
		if err := os.WriteFile(nested, []byte("added to a directory the snapshot archived"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeAppdataUnpack = nil })

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	old := filepath.Join(rig.workDir(t), "old")
	if got, want := strings.Join(filesUnder(t, old), ","), "stray,sub/nested"; got != want {
		t.Fatalf("the replaced directory still holds %s, want %s", got, want)
	}
	if got := readFile(t, filepath.Join(old, "stray")); got != "another app's file" {
		t.Fatalf("stray = %q", got)
	}
	if got := readFile(t, filepath.Join(old, "sub", "nested")); got != "added to a directory the snapshot archived" {
		t.Fatalf("sub/nested = %q", got)
	}
	for _, p := range []string{"stray", "sub/nested"} {
		if !strings.Contains(rig.out.String(), strconv.Quote(filepath.Join(old, p))) {
			t.Fatalf("the output does not name %s:\n%s", p, rig.out)
		}
	}
}

func TestAppdataRestore_RemovesTheWholeReplacedTreeWhenNothingWasAddedAfterTheSnapshot(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	if err := unix.Mkfifo(filepath.Join(rig.dir, "sub", "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config", filepath.Join(rig.dir, "link")); err != nil {
		t.Fatal(err)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	rig.requireNothingLeftNextToLive(t, "alpha")
	if strings.Contains(rig.out.String(), "warning:") {
		t.Fatalf("a restore with nothing added after the snapshot warned:\n%s", rig.out)
	}
}

func TestAppdataRestore_KeepsTheReplacedTreeAndWarnsWhenItsRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	var sub string
	beforeAppdataCleanup = func() {
		beforeAppdataCleanup = nil
		work, _ := filepath.Glob(rig.dir + ".hoserva-restore-*")
		sub = filepath.Join(work[0], "old", "sub")
		if err := os.Chmod(sub, 0); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		beforeAppdataCleanup = nil
		if sub != "" {
			_ = os.Chmod(sub, 0o755)
		}
	})

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if err := os.Chmod(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(sub, "keep")); got != "kept" {
		t.Fatalf("sub/keep = %q: the removal went on after its error", got)
	}
	if !strings.Contains(rig.out.String(), "warning: the appdata that was replaced is still at") {
		t.Fatalf("no warning about the replaced appdata:\n%s", rig.out)
	}
}

func TestKeptMessage_NamesAtMostFivePathsAndCountsTheRest(t *testing.T) {
	var kept []string
	for _, n := range []string{"g", "f", "e", "d", "c", "b", "a"} {
		kept = append(kept, "old/"+n)
	}
	got := keptMessage("/w", kept)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if !strings.Contains(got, strconv.Quote("/w/old/"+n)) {
			t.Errorf("%q does not name %s", got, n)
		}
	}
	if strings.Contains(got, `"/w/old/f"`) || !strings.HasSuffix(got, " and 2 more") {
		t.Fatalf("message = %q", got)
	}
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// moveOtherAppInPlaceOfLive moves the restored container's directory aside
// and, before the restore starts, renames the directory of beta, another app
// whose anchor is its own path, to the name it had. It returns beta's anchor
// and where the restored container's directory went.
func (r *restoreRig) moveOtherAppInPlaceOfLive(t *testing.T, other otherAppdata) (anchor, aside string) {
	t.Helper()
	beta := filepath.Join(r.appdata, "beta")
	other.create(t, beta)
	anchor = resolved(t, beta)
	r.attrs.mark(t, beta, anchor)
	aside = r.dir + ".aside"
	if err := os.Rename(r.dir, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(beta, r.dir); err != nil {
		t.Fatal(err)
	}
	return anchor, aside
}

// requireRefusedAsAnotherAppsDirectory checks that the restore was refused
// before any write because the directory at the name carries another path,
// that the error names both paths, and that nothing about the directory
// changed: its files, its anchor, and what the restore started from.
func (r *restoreRig) requireRefusedAsAnotherAppsDirectory(t *testing.T, err error, other otherAppdata, anchor, aside string) {
	t.Helper()
	r.requireRefusedBeforeAnyWrite(t, err, other, aside)
	for _, want := range []string{anchor, resolved(t, r.dir)} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Restore error %q does not name %s", err, want)
		}
	}
	if got := filesUnder(t, r.dir); strings.Join(got, ",") != "data,sub/state" {
		t.Fatalf("files in the other app's directory = %v, want data and sub/state only", got)
	}
	r.attrs.requireAnchor(t, r.dir, anchor)
}

func TestAppdataRestore_RefusesADirectoryAnchoredToAnotherPathBeforeSnapshottingIt(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	anchor, aside := rig.moveOtherAppInPlaceOfLive(t, other)

	err := rig.restore(t)
	if !errors.Is(err, ErrPreRestoreSnapshot) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot\n%s", err, rig.out)
	}
	rig.requireRefusedAsAnotherAppsDirectory(t, err, other, anchor, aside)
}

func TestAppdataRestore_TheRevertBeforeAnUpdateRefusesADirectoryAnchoredToAnotherPath(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	other := newOtherAppdata()
	anchor, aside := rig.moveOtherAppInPlaceOfLive(t, other)

	snaps := UpdateSnapshots{Appdata: rig.svc}
	ref := container.SnapshotRef{Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID}
	rig.out.Reset()
	err := snaps.Restore(context.Background(), "alpha", ref, nil, rig.out)
	rig.requireRefusedAsAnotherAppsDirectory(t, err, other, anchor, aside)
}

// forgedEntry is one tar entry of an archive forgeAppdata writes.
type forgedEntry struct {
	name     string
	typeflag byte
	mode     int64
	uid, gid int
	body     string
}

// forgeAppdata writes a complete, self-consistent appdata archive the way
// someone who can write a destination would: a valid header, entries, and a
// trailer whose counts and SHA-256 match what precedes it. With a key it also
// carries the authentication tag the daemon's own archives carry.
func forgeAppdata(t *testing.T, dest string, hdr appdataHeader, entries []forgedEntry, key []byte) {
	t.Helper()
	out, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(out, zstd.WithEncoderCRC(true))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(zw, sum))
	hdr.Version = appdataFormatVersion
	if err := writeAppdataMeta(tw, appdataHeaderName, hdr, hdr.CreatedAt); err != nil {
		t.Fatal(err)
	}
	var trailer appdataTrailer
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Uid: e.uid, Gid: e.gid, Size: int64(len(e.body)), ModTime: hdr.CreatedAt}
		if e.typeflag == tar.TypeDir {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
			trailer.Files++
			trailer.Bytes += int64(len(e.body))
		}
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	trailer.SHA256 = hex.EncodeToString(sum.Sum(nil))
	signForgedTrailer(&trailer, key)
	if err := writeAppdataMeta(tw, appdataTrailerName, trailer, hdr.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// plantLocalDestination adds an unencrypted local destination to the rig and
// returns its directory.
func (r *restoreRig) plantLocalDestination(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(r.root, "local")
	if _, err := os.Stat(dest); err == nil {
		return dest
	}
	if err := r.store.CreateDestination(context.Background(), Destination{
		ID: "local", Name: "Local", Type: TypeLocal, Path: dest, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	return dest
}

// plantAppdata forges an archive of container alpha into the unencrypted
// local destination and returns the restore request for it.
func (r *restoreRig) plantAppdata(t *testing.T, suffix int, hdr appdataHeader, entries []forgedEntry, key []byte) AppdataRestoreRequest {
	t.Helper()
	dest := r.plantLocalDestination(t)
	name := appdataArchiveName(r.remoteRig.svc.installationID(), "alpha", r.now, ReasonNone, suffix)
	hdr.Container, hdr.CreatedAt = "alpha", r.now
	forgeAppdata(t, filepath.Join(dest, name), hdr, entries, key)
	return AppdataRestoreRequest{Container: "alpha", Archive: name, DestinationID: "local"}
}

// foreignEntries is an archive's content for Dirs [alpha's, beta's]: a file
// for alpha, and for beta a root-owned setuid binary.
func foreignEntries() []forgedEntry {
	return []forgedEntry{
		{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/0/config", typeflag: tar.TypeReg, mode: 0o644, body: "planted"},
		{name: "data/1/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/1/payload", typeflag: tar.TypeReg, mode: 0o4755, uid: 0, gid: 0, body: "planted"},
	}
}

// A self-consistent archive that was not produced by Run, found under a name
// this installation would use in an unencrypted local destination, names
// alpha's directory and beta's. A restore of alpha must not replace beta's
// appdata as root.
func TestAppdataRestore_RefusesAForgedArchiveNamingAnotherContainersAppdata(t *testing.T) {
	rig := newRestoreRig(t)
	betaDir := rig.addApp(t, "beta", "radarr", "exited", map[string]string{"secret": "beta-private"})
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir, betaDir}}, foreignEntries(), nil)
	before := snapshotTree(t, rig.cache)
	rig.containers.mu.Lock()
	rig.containers.events = nil
	rig.containers.mu.Unlock()

	err := rig.svc.Restore(context.Background(), req, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore = %v, want ErrAppdataArchiveInvalid", err)
	}
	if after := snapshotTree(t, rig.cache); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused restore changed appdata:\nbefore %v\nafter  %v", before, after)
	}
	if got := readFile(t, filepath.Join(betaDir, "secret")); got != "beta-private" {
		t.Fatalf("beta's appdata = %q after the refused restore", got)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched by a refused restore: %v", ev)
	}
}

// An archive whose directories are alpha's own is still refused when nothing
// proves the daemon wrote it.
func TestAppdataRestore_RefusesAnArchiveWithoutAValidTagFromAnUnencryptedDestination(t *testing.T) {
	rig := newRestoreRig(t)
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir}}, []forgedEntry{
		{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/0/config", typeflag: tar.TypeReg, mode: 0o644, body: "planted"},
	}, nil)
	before := snapshotTree(t, rig.cache)
	rig.containers.mu.Lock()
	rig.containers.events = nil
	rig.containers.mu.Unlock()

	err := rig.svc.Restore(context.Background(), req, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore = %v, want ErrAppdataArchiveInvalid", err)
	}
	if after := snapshotTree(t, rig.cache); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused restore changed appdata:\nbefore %v\nafter  %v", before, after)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched by a refused restore: %v", ev)
	}
}

func TestAppdataPreview_RefusesTheArchivesARestoreRefusesWithTheSameError(t *testing.T) {
	rig := newRestoreRig(t)
	betaDir := rig.addApp(t, "beta", "radarr", "exited", map[string]string{"secret": "beta-private"})
	own := []forgedEntry{
		{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/0/config", typeflag: tar.TypeReg, mode: 0o644, body: "planted"},
	}
	requests := map[string]AppdataRestoreRequest{
		"another container's directory": rig.plantAppdata(t, 2, appdataHeader{Dirs: []string{rig.dir, betaDir}}, foreignEntries(), nil),
		"no tag":                        rig.plantAppdata(t, 3, appdataHeader{Dirs: []string{rig.dir}}, own, nil),
	}
	rig.containers.mu.Lock()
	rig.containers.events = nil
	rig.containers.mu.Unlock()
	for name, req := range requests {
		t.Run(name, func(t *testing.T) {
			before := snapshotTree(t, rig.cache)
			_, perr := rig.svc.PreviewRestore(context.Background(), req)
			if !errors.Is(perr, ErrAppdataArchiveInvalid) {
				t.Fatalf("PreviewRestore = %v, want ErrAppdataArchiveInvalid", perr)
			}
			rig.requireNothingChanged(t, before)
			rerr := rig.svc.Restore(context.Background(), req, rig.out)
			if rerr == nil || rerr.Error() != perr.Error() {
				t.Fatalf("the restore's refusal = %v, the preview's = %v; they must be the same", rerr, perr)
			}
		})
	}
}

func signForgedTrailer(t *appdataTrailer, key []byte) {
	if key != nil {
		t.MAC = t.mac(key)
	}
}

// key is the key the rig's service authenticates its archives with.
func (r *appdataRig) key(t *testing.T) []byte {
	t.Helper()
	key, err := r.svc.archiveKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func ownEntries(body string) []forgedEntry {
	return []forgedEntry{
		{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/0/config", typeflag: tar.TypeReg, mode: 0o644, body: body},
	}
}

func (r *restoreRig) forgeEncrypted(t *testing.T, key []byte) AppdataRestoreRequest {
	t.Helper()
	dest := filepath.Join(r.root, "encrypted")
	if err := r.store.CreateDestination(context.Background(), Destination{
		ID: "encrypted", Name: "Encrypted", Type: TypeLocal, Path: dest, Enabled: true, Encrypt: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatal(err)
	}
	name := appdataArchiveName(r.remoteRig.svc.installationID(), "alpha", r.now, ReasonNone, 0)
	plain := filepath.Join(r.root, name)
	forgeAppdata(t, plain, appdataHeader{Container: "alpha", CreatedAt: r.now, Dirs: []string{r.dir}}, ownEntries("legacy"), key)
	sealed, err := encryptArchiveForDestination(plain, r.remoteRig.svc.Recipient.Public)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sealed, filepath.Join(dest, name+".age")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(plain, filepath.Join(dest, name)); err != nil {
		t.Fatal(err)
	}
	return AppdataRestoreRequest{Container: "alpha", Archive: name + ".age", DestinationID: "encrypted"}
}

func TestAppdataRestore_RefusesAnUntaggedArchiveFromAnUnencryptedDestinationAndSaysWhy(t *testing.T) {
	rig := newRestoreRig(t)
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir}}, ownEntries("legacy"), nil)
	for name, run := range map[string]func() error{
		"restore": func() error { return rig.svc.Restore(context.Background(), req, rig.out) },
		"preview": func() error { _, err := rig.svc.PreviewRestore(context.Background(), req); return err },
	} {
		err := run()
		if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "predates archive authentication") || !strings.Contains(err.Error(), "by hand") {
			t.Fatalf("%s = %v, want the archive refused as one that predates authentication", name, err)
		}
	}
	rig.requireLiveUntouched(t)
}

func TestAppdataRestore_RefusesAnArchiveWhoseTagIsAnotherKeys(t *testing.T) {
	rig := newRestoreRig(t)
	other, err := deriveAppdataKey("AGE-SECRET-KEY-SOMEONE-ELSE")
	if err != nil {
		t.Fatal(err)
	}
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir}}, ownEntries("forged"), other)
	err = rig.svc.Restore(context.Background(), req, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "authentication tag") {
		t.Fatalf("Restore = %v, want a refused tag", err)
	}
	rig.requireLiveUntouched(t)
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers touched by a refused restore: %v", ev)
	}
}

// Taking the tag off a genuine archive leaves a legacy one, which an
// unencrypted destination does not accept.
func TestAppdataRestore_RefusesAGenuineArchiveWithItsTagRemoved(t *testing.T) {
	rig := newAppdataRig(t)
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "v1"})
	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	genuine := rig.archiveFor(t, rig.poolDir, "alpha")
	_, trailer, err := verifyAppdata(genuine)
	if err != nil || !trailer.authentic(rig.key(t)) {
		t.Fatalf("the daemon's own archive: %v, authentic %v", err, trailer.authentic(rig.key(t)))
	}
	entries := readArchivedEntries(t, genuine)
	last := &entries[len(entries)-1]
	trailer.MAC = ""
	last.body, _ = json.Marshal(trailer)
	stripped := writeArchivedEntries(t, entries)
	name := appdataArchiveName(rig.svc.Backup.installationID(), "alpha", rig.now, ReasonNone, 2)
	raw, err := os.ReadFile(stripped)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.poolDir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err = rig.svc.Restore(context.Background(), AppdataRestoreRequest{Container: "alpha", Archive: name, DestinationID: DefaultPoolID}, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "predates archive authentication") {
		t.Fatalf("Restore = %v, want an archive without its tag refused as one that predates authentication", err)
	}
	if got := readFile(t, filepath.Join(dir, "config")); got != "v1" {
		t.Fatalf("config = %q", got)
	}
}

func TestAppdataRestore_AcceptsAnArchiveWithoutATagFromADestinationThatEncrypts(t *testing.T) {
	rig := newRestoreRig(t)
	req := rig.forgeEncrypted(t, nil)
	if err := rig.svc.Restore(context.Background(), req, rig.out); err != nil {
		t.Fatalf("Restore of a legacy archive from an encrypting destination: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "legacy" {
		t.Fatalf("config = %q, want the archived content", got)
	}
}

func TestAppdataRestore_RefusesAnUntaggedFileSittingInADestinationThatEncrypts(t *testing.T) {
	rig := newRestoreRig(t)
	req := rig.forgeEncrypted(t, nil)
	req.Archive = strings.TrimSuffix(req.Archive, ".age")
	err := rig.svc.Restore(context.Background(), req, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore = %v, want ErrAppdataArchiveInvalid for a plain file in an encrypting destination", err)
	}
	rig.requireLiveUntouched(t)
}

func TestAppdataRestore_RefusesATaggedArchiveNamingADirectoryThatIsNotTheContainers(t *testing.T) {
	rig := newRestoreRig(t)
	betaDir := rig.addApp(t, "beta", "radarr", "exited", map[string]string{"secret": "beta-private"})
	unowned := filepath.Join(rig.appdata, "nobody")
	if err := os.MkdirAll(unowned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unowned, "keep"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"another container's directory": {rig.dir, betaDir},
		"a directory no container has":  {rig.dir, unowned},
		"inside a directory of its own": {filepath.Join(rig.dir, "sub")},
	}
	suffix := 1
	for name, dirs := range cases {
		t.Run(name, func(t *testing.T) {
			entries := []forgedEntry{{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755}}
			for i := 1; i < len(dirs); i++ {
				entries = append(entries,
					forgedEntry{name: "data/" + strconv.Itoa(i) + "/", typeflag: tar.TypeDir, mode: 0o755},
					forgedEntry{name: "data/" + strconv.Itoa(i) + "/payload", typeflag: tar.TypeReg, mode: 0o4755, body: "planted"})
			}
			suffix++
			req := rig.plantAppdata(t, suffix, appdataHeader{Dirs: dirs}, entries, rig.key(t))
			before := snapshotTree(t, rig.cache)
			rig.containers.mu.Lock()
			rig.containers.events = nil
			rig.containers.mu.Unlock()

			err := rig.svc.Restore(context.Background(), req, rig.out)
			if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "appdata directories") {
				t.Fatalf("Restore = %v, want a directory that is not alpha's refused", err)
			}
			if after := snapshotTree(t, rig.cache); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused restore changed appdata:\nbefore %v\nafter  %v", before, after)
			}
			if ev := rig.containers.Events(); len(ev) != 0 {
				t.Fatalf("containers touched before the restore was refused: %v", ev)
			}
			if _, perr := rig.svc.PreviewRestore(context.Background(), req); !errors.Is(perr, ErrAppdataArchiveInvalid) || perr.Error() != err.Error() {
				t.Fatalf("PreviewRestore = %v, want the restore's refusal %v", perr, err)
			}
		})
	}
}

func TestAppdataRestore_RefusesAnArchiveOfAContainerThatNoLongerExists(t *testing.T) {
	rig := newRestoreRig(t)
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir}}, ownEntries("v1"), rig.key(t))
	rig.engine.RemoveContainer("id-alpha")
	err := rig.svc.Restore(context.Background(), req, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) || !strings.Contains(err.Error(), "not a container on this server") {
		t.Fatalf("Restore = %v, want a refusal because the container is gone", err)
	}
}

// A directory that is the container's by its mount but is not on disk now
// (a lost directory is what a restore is for) is restored.
func TestAppdataRestore_RestoresADirectoryThatIsMissingFromDisk(t *testing.T) {
	rig := newRestoreRig(t)
	if err := os.RemoveAll(rig.dir); err != nil {
		t.Fatal(err)
	}
	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
}

// The directory a mount names through a link is the one the archive names,
// since the archive holds the resolved path.
func TestAppdataRestore_AcceptsADirectoryTheContainerMountsThroughALink(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "v1"})
	link := filepath.Join(rig.appdata, "alpha-link")
	if err := os.Symlink(filepath.Join(rig.appdata, "alpha"), link); err != nil {
		t.Fatal(err)
	}
	rig.engine.RemoveContainer("id-alpha")
	rig.engine.AddContainer(container.Container{
		ID: "id-alpha", Name: "alpha", Image: "sonarr", State: "running",
		Mounts: []container.Mount{{Source: link, Destination: "/config", ReadWrite: true}},
	})
	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	archives, _, err := rig.svc.ListArchives(context.Background(), "alpha")
	if err != nil || len(archives) != 1 {
		t.Fatalf("ListArchives = %v, %v", archives, err)
	}
	rig.out.Reset()
	if err := rig.svc.Restore(context.Background(), AppdataRestoreRequest{Container: "alpha", Archive: archives[0].Name, DestinationID: archives[0].DestinationID}, rig.out); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
}

// A genuine archive written by the daemon restores from an unencrypted
// destination, and every archive a writer produces carries the tag.
func TestAppdataRestore_RestoresTheDaemonsOwnArchiveFromAnUnencryptedDestination(t *testing.T) {
	rig := newAppdataRig(t)
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "v1"})
	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	if _, trailer, err := verifyAppdata(rig.archiveFor(t, rig.poolDir, "alpha")); err != nil || !trailer.authentic(rig.key(t)) {
		t.Fatalf("a backup's archive: %v, authentic %v", err, trailer.authentic(rig.key(t)))
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	archives, _, err := rig.svc.ListArchives(context.Background(), "alpha")
	if err != nil || len(archives) != 1 {
		t.Fatalf("ListArchives = %v, %v", archives, err)
	}
	rig.out.Reset()
	req := AppdataRestoreRequest{Container: "alpha", Archive: archives[0].Name, DestinationID: archives[0].DestinationID}
	if _, err := rig.svc.PreviewRestore(context.Background(), req); err != nil {
		t.Fatalf("PreviewRestore: %v", err)
	}
	if err := rig.svc.Restore(context.Background(), req, rig.out); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want v1", got)
	}
	snapshots := 0
	for _, n := range rig.archives(t, rig.poolDir) {
		if _, _, reason, _, _ := parseAppdataName(n); reason != ReasonPreRestore {
			continue
		}
		snapshots++
		if _, trailer, err := verifyAppdata(filepath.Join(rig.poolDir, n)); err != nil || !trailer.authentic(rig.key(t)) {
			t.Fatalf("the pre-restore snapshot %s: %v, authentic %v", n, err, trailer.authentic(rig.key(t)))
		}
	}
	if snapshots != 1 {
		t.Fatalf("%d pre-restore snapshots in the pool, want 1", snapshots)
	}
}

func TestAppdataRestore_LeavesOffSetuidAndSetgidOnEntriesGivenToRoot(t *testing.T) {
	rig := newRestoreRig(t)
	const user = 1234
	req := rig.plantAppdata(t, 0, appdataHeader{Dirs: []string{rig.dir}}, []forgedEntry{
		{name: "data/0/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "data/0/rootsuid", typeflag: tar.TypeReg, mode: 0o4755, uid: 0, gid: user, body: "x"},
		{name: "data/0/rootsgid", typeflag: tar.TypeReg, mode: 0o2755, uid: user, gid: 0, body: "x"},
		{name: "data/0/rootboth", typeflag: tar.TypeReg, mode: 0o6755, uid: 0, gid: 0, body: "x"},
		{name: "data/0/usersuid", typeflag: tar.TypeReg, mode: 0o4755, uid: user, gid: user, body: "x"},
		{name: "data/0/plain", typeflag: tar.TypeReg, mode: 0o755, uid: 0, gid: 0, body: "x"},
	}, rig.key(t))
	if os.Geteuid() != 0 {
		// Without root a restore does not chown, so an archived owner other
		// than the daemon's own cannot be given back; the rule reads the
		// archived owner, which is what this checks.
		t.Log("running unprivileged: owners are not restored, the archived owner is still what decides")
	}

	if err := rig.svc.Restore(context.Background(), req, rig.out); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	mode := func(name string) os.FileMode {
		t.Helper()
		info, err := os.Lstat(filepath.Join(rig.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode()
	}
	for name, want := range map[string]os.FileMode{
		"rootsuid": 0o755, "rootsgid": 0o755, "rootboth": 0o755, "plain": 0o755,
	} {
		if got := mode(name); got != want {
			t.Errorf("%s restored with mode %v, want %v: no setuid or setgid bit on an entry given to root", name, got, want)
		}
	}
	if got := mode("usersuid"); got&os.ModeSetuid == 0 {
		t.Errorf("usersuid restored with mode %v, want its setuid bit kept: its owner is not root", got)
	}
	for _, name := range []string{"rootsuid", "rootsgid", "rootboth"} {
		if !strings.Contains(rig.out.String(), filepath.Join(rig.dir, name)) {
			t.Errorf("the job output does not name %s:\n%s", name, rig.out)
		}
	}
	for _, name := range []string{"usersuid", "plain"} {
		if strings.Contains(rig.out.String(), filepath.Join(rig.dir, name)) {
			t.Errorf("the job output names %s, which kept its bits:\n%s", name, rig.out)
		}
	}
}
