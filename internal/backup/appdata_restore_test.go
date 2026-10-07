package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	rig := newAppdataRig(t)
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
	}); err != nil {
		t.Fatal(err)
	}
	err := rig.svc.Restore(context.Background(), AppdataRestoreRequest{Container: "alpha", Archive: name, DestinationID: "local"}, rig.out)
	if !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("Restore = %v, want ErrAppdataArchiveInvalid", err)
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
	afterAppdataParents = func() { moved = rig.swapParentForLink(t, outside) }
	t.Cleanup(func() { afterAppdataParents = nil })

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
