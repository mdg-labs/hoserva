package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestAppdataSnapshot_WritesAVerifiedPreUpdateArchiveOfOneContainer(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "lscr.io/linuxserver/sonarr", "running", map[string]string{"config.xml": "alpha-config"})
	rig.addApp(t, "beta", "lscr.io/linuxserver/radarr", "running", map[string]string{"db": "beta-data"})
	// A container left out of the nightly backup is still snapshotted
	// before it is updated.
	if _, err := rig.svc.SetPolicy(context.Background(), "alpha", true, false); err != nil {
		t.Fatal(err)
	}

	ref, err := rig.svc.Snapshot(context.Background(), "alpha", ReasonPreUpdate, rig.out)
	if err != nil {
		t.Fatalf("Snapshot: %v\n%s", err, rig.out)
	}
	if ref.DestinationID != DefaultPoolID {
		t.Fatalf("ref = %+v, want the pool destination", ref)
	}
	if _, c, reason, _, ok := parseAppdataName(ref.Archive); !ok || c != "alpha" || reason != ReasonPreUpdate {
		t.Fatalf("archive name %q is not a pre-update archive of alpha", ref.Archive)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 || got[0] != ref.Archive {
		t.Fatalf("pool archives = %v, want only %s: beta was not snapshotted", got, ref.Archive)
	}
	hdr, trailer, err := verifyAppdata(filepath.Join(rig.poolDir, ref.Archive))
	if err != nil {
		t.Fatalf("verifying the snapshot: %v", err)
	}
	if hdr.Container != "alpha" || hdr.Reason != string(ReasonPreUpdate) || !hdr.Stopped || trailer.Files != 1 {
		t.Fatalf("header %+v trailer %+v", hdr, trailer)
	}
	if got, want := strings.Join(rig.containers.Events(), ","), "stop alpha,start alpha"; got != want {
		t.Fatalf("events = %s, want alpha stopped for the copy and started again", got)
	}
	if err := rig.svc.FindArchive(context.Background(), "alpha", ref.Archive, ref.DestinationID); err != nil {
		t.Fatalf("FindArchive of the snapshot: %v", err)
	}
	listed, _, err := rig.svc.ListArchives(context.Background(), "alpha")
	if err != nil || len(listed) != 1 || listed[0].Reason != ReasonPreUpdate {
		t.Fatalf("ListArchives = %+v, %v, want the pre-update reason shown", listed, err)
	}
}

// Nothing is stopped, archived or reported as written when no destination
// can hold the snapshot: the update that asked for it does not go ahead.
func TestAppdataSnapshot_NoDestinationFailsBeforeStoppingAnything(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	if err := rig.store.DeleteDestination(context.Background(), DefaultPoolID); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.svc.Snapshot(context.Background(), "alpha", ReasonPreUpdate, rig.out); !errors.Is(err, ErrAppdataNoDestination) {
		t.Fatalf("Snapshot = %v, want ErrAppdataNoDestination", err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 {
		t.Fatalf("containers were touched: %v", ev)
	}
}

func TestAppdataSnapshot_RefusalsTouchNothing(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	if _, err := rig.svc.Snapshot(context.Background(), "nope", ReasonPreUpdate, rig.out); !errors.Is(err, ErrAppdataContainerNotFound) {
		t.Fatalf("Snapshot of a container with no appdata = %v, want ErrAppdataContainerNotFound", err)
	}
	rig.halted = true
	if _, err := rig.svc.Snapshot(context.Background(), "alpha", ReasonPreUpdate, rig.out); !errors.Is(err, container.ErrArrayStopped) {
		t.Fatalf("Snapshot with the array stopped = %v, want ErrArrayStopped", err)
	}
	if ev := rig.containers.Events(); len(ev) != 0 || len(rig.archives(t, rig.poolDir)) != 0 {
		t.Fatalf("a refused snapshot touched containers %v or wrote %v", ev, rig.archives(t, rig.poolDir))
	}
}

// A container whose archive could not be taken (it would not stop, so its
// copy would not be consistent) is not reported as snapshotted, and is
// started again.
func TestAppdataSnapshot_AContainerThatWillNotStopIsNotSnapshotted(t *testing.T) {
	rig := newAppdataRig(t)
	rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"a": "1"})
	rig.engine.FailOn("stop", "alpha", errors.New("did not stop in time"))

	_, err := rig.svc.Snapshot(context.Background(), "alpha", ReasonPreUpdate, rig.out)
	if err == nil || !strings.Contains(err.Error(), "did not stop in time") {
		t.Fatalf("Snapshot = %v, want the stop failure", err)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 0 {
		t.Fatalf("an archive of a container that was still running was written: %v", got)
	}
}

// The round trip a revert depends on: the snapshot taken before an update
// brings the appdata back after it, and the restore writes a copy of what it
// replaces first.
func TestUpdateSnapshots_RestoreBringsBackTheSnapshottedAppdata(t *testing.T) {
	rig := newAppdataRig(t)
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "before-the-update"})
	snaps := UpdateSnapshots{Appdata: rig.svc}
	ctx := context.Background()

	scope, err := snaps.Scope(ctx, "alpha")
	if err != nil || !scope.Has || len(scope.Sharers) != 0 || len(scope.Resources) != 1 || scope.Resources[0] != AppdataJobResource {
		t.Fatalf("Scope(alpha) = %+v, %v, want appdata held through the backup's job resource", scope, err)
	}
	ref, err := snaps.Snapshot(ctx, "alpha", rig.out)
	if err != nil {
		t.Fatalf("Snapshot: %v\n%s", err, rig.out)
	}
	if err := snaps.Find(ctx, "alpha", ref); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("migrated-by-the-new-version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := snaps.Restore(ctx, "alpha", ref, nil, rig.out); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(dir, "config")); got != "before-the-update" {
		t.Fatalf("config = %q, want the snapshotted content", got)
	}
	var preRestore bool
	for _, n := range rig.archives(t, rig.poolDir) {
		if _, _, reason, _, ok := parseAppdataName(n); ok && reason == ReasonPreRestore {
			preRestore = true
		}
	}
	if !preRestore {
		t.Fatalf("no pre-restore snapshot of the migrated appdata: %v", rig.archives(t, rig.poolDir))
	}

	if err := snaps.Find(ctx, "alpha", container.SnapshotRef{Archive: "hoserva-appdata-0123456789ab-alpha-2020-01-01T00-00-00.pre-update.tar.zst", DestinationID: ref.DestinationID}); err == nil {
		t.Fatal("Find reported an archive the destination does not hold")
	}
}

func TestUpdateSnapshots_ScopeOfAContainerOutsideAppdataAndOfSharers(t *testing.T) {
	rig := newAppdataRig(t)
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"sub/a": "1"})
	rig.engine.AddContainer(container.Container{ID: "id-media", Name: "media", Image: "plex", State: "running", Mounts: []container.Mount{{Source: t.TempDir(), Destination: "/media"}}})
	rig.engine.AddContainer(container.Container{
		ID: "id-transcoder", Name: "transcoder", Image: "example/transcoder", State: "running",
		Mounts: []container.Mount{{Source: filepath.Join(dir, "sub"), Destination: "/transcode", ReadWrite: true}},
	})
	snaps := UpdateSnapshots{Appdata: rig.svc}

	scope, err := snaps.Scope(context.Background(), "media")
	if err != nil || scope.Has || len(scope.Resources) != 0 {
		t.Fatalf("Scope(media) = %+v, %v, want no appdata and nothing to hold", scope, err)
	}
	scope, err = snaps.Scope(context.Background(), "alpha")
	if err != nil || !scope.Has || strings.Join(scope.Sharers, ",") != "transcoder" {
		t.Fatalf("Scope(alpha) = %+v, %v, want transcoder as a sharer", scope, err)
	}
}

// updateHistory is the update record, in memory.
type updateHistory struct {
	rows []store.ImageHistory
}

func (h *updateHistory) InsertImageHistory(_ context.Context, r store.ImageHistory) (store.ImageHistory, error) {
	r.ID = int64(len(h.rows) + 1)
	h.rows = append(h.rows, r)
	return r, nil
}

func (h *updateHistory) LatestImageHistory(_ context.Context, name string) (store.ImageHistory, bool, error) {
	for i := len(h.rows) - 1; i >= 0; i-- {
		if h.rows[i].Container == name {
			return h.rows[i], true, nil
		}
	}
	return store.ImageHistory{}, false, nil
}

func (h *updateHistory) ListImageHistory(context.Context) ([]store.ImageHistory, error) {
	return h.rows, nil
}

func (h *updateHistory) MarkImageHistoryReverted(_ context.Context, id int64, at time.Time) error {
	h.rows[id-1].RevertedAt = at
	return nil
}

func (h *updateHistory) MarkImageHistorySnapshotRestored(_ context.Context, id int64, at time.Time) error {
	h.rows[id-1].SnapshotRestoredAt = at
	return nil
}

func (h *updateHistory) DeleteImageHistory(context.Context, int64) error { return nil }

func (h *updateHistory) SetBulkExcluded(context.Context, string, bool) error { return nil }

func (h *updateHistory) BulkExcluded(context.Context) ([]string, error) { return nil, nil }

func (h *updateHistory) ImageKeepDays(context.Context) (int, error) { return 7, nil }

func (h *updateHistory) SetImageKeepDays(context.Context, int) error { return nil }

// A revert that fails after it restored the snapshot says "revert again to
// finish", so the snapshot has to survive its own restore. The restore writes
// a pre-restore archive of what it replaces, and with four restores since the
// update that is the sixth pre-change archive: retention keeps five, and the
// pre-update archive, the oldest, would be the one it pruned, leaving the
// retry with revert_unavailable and the container stopped on the updated
// image.
func TestRevert_ARetryAfterAFailedSwapStillFindsTheSnapshotItsRestorePruned(t *testing.T) {
	rig := newAppdataRig(t)
	ctx := context.Background()
	dir := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "before-the-update"})
	rig.engine.RemoveContainer("id-alpha")
	rig.engine.AddContainer(container.Container{
		ID: "id-alpha", Name: "alpha", Image: "sonarr", Tag: "4.0", ImageID: "sha256:old", State: "running",
		Mounts: []container.Mount{{Source: dir, Destination: "/config", ReadWrite: true}},
	})
	parsed, err := container.ParseImageRef("sonarr", "4.0")
	if err != nil {
		t.Fatal(err)
	}
	ref := parsed.String()
	rig.engine.AddImage(container.Image{ID: "sha256:old", RepoTags: []string{ref}})
	rig.engine.AddImage(container.Image{ID: "sha256:new"})
	rig.engine.SetPull(ref, "sha256:new")
	updater := &container.Updater{
		Lifecycle: &container.Lifecycle{Provider: rig.engine, Halted: func() bool { return false }, StorageReady: func() bool { return true }},
		History:   &updateHistory{},
		Snapshots: UpdateSnapshots{Appdata: rig.svc},
		Now:       func() time.Time { return rig.now },
	}

	// Archive modification times decide which pre-change archives are the
	// newest, so each archive written gets the next minute.
	stamped := map[string]bool{}
	tick := 0
	stamp := func() {
		for _, n := range rig.archives(t, rig.poolDir) {
			if stamped[n] {
				continue
			}
			stamped[n] = true
			tick++
			at := rig.now.Add(time.Duration(tick) * time.Minute)
			if err := os.Chtimes(filepath.Join(rig.poolDir, n), at, at); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := rig.run(t, "alpha"); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	stamp()
	if err := updater.Update(ctx, "alpha", rig.out); err != nil {
		t.Fatalf("Update: %v\n%s", err, rig.out)
	}
	stamp()
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("migrated-by-the-new-version"), 0o644); err != nil {
		t.Fatal(err)
	}

	archives, _, err := rig.svc.ListArchives(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	var ordinary AppdataArchive
	for _, a := range archives {
		if a.Reason == ReasonNone && a.DestinationID == DefaultPoolID {
			ordinary = a
		}
	}
	if ordinary.Name == "" {
		t.Fatalf("no ordinary archive on the pool destination: %+v", archives)
	}
	for i := 0; i < 4; i++ {
		if err := rig.svc.Restore(ctx, AppdataRestoreRequest{Container: "alpha", Archive: ordinary.Name, DestinationID: ordinary.DestinationID}, rig.out); err != nil {
			t.Fatalf("restore %d: %v\n%s", i+1, err, rig.out)
		}
		stamp()
	}

	rig.engine.FailOn("tag-image", ref, errors.New("engine busy"))
	rig.out.Reset()
	err = updater.Revert(ctx, "alpha", nil, rig.out)
	if err == nil || !strings.Contains(err.Error(), "revert again to finish") || !strings.Contains(err.Error(), "engine busy") {
		t.Fatalf("Revert = %v, want the swap failure and how to finish\n%s", err, rig.out)
	}
	if err := updater.CheckRevert(ctx, "alpha"); err != nil {
		t.Fatalf("the pre-update snapshot was pruned by the restore that used it, so the revert cannot be retried: %v", err)
	}

	rig.engine.FailOn("tag-image", ref, nil)
	rig.out.Reset()
	if err := updater.Revert(ctx, "alpha", nil, rig.out); err != nil {
		t.Fatalf("retried Revert: %v\n%s", err, rig.out)
	}
	if c, _ := rig.engine.Inspect(ctx, "alpha"); c.ImageID != "sha256:old" {
		t.Fatalf("alpha runs %s after the retry, want its previous image", c.ImageID)
	}
	if got := readFile(t, filepath.Join(dir, "config")); got != "before-the-update" {
		t.Fatalf("config = %q, want the snapshotted content", got)
	}
}

func TestPruneAppdata_NeverPrunesTheArchiveBeingRestored(t *testing.T) {
	dir := t.TempDir()
	target := localTarget{dest: Destination{Path: dir}}
	inst := "aaaaaaaaaaaa"
	now := time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC)
	write := func(name string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	oldest := appdataArchiveName(inst, "alpha", now.Add(-6*time.Hour), ReasonPreUpdate, 0)
	write(oldest+".age", 6*time.Hour)
	write(oldest+".age"+identitySidecarSuffix, 6*time.Hour)
	for i := 0; i < 5; i++ {
		write(appdataArchiveName(inst, "alpha", now.Add(-time.Duration(i)*time.Hour), ReasonPreRestore, 0), time.Duration(i)*time.Hour)
	}
	newest := appdataArchiveName(inst, "alpha", now, ReasonPreRestore, 0)
	write(newest, 0)

	if err := pruneAppdata(context.Background(), target, inst, Retention{Daily: 1}, now, "alpha", newest, oldest); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{oldest + ".age", oldest + ".age" + identitySidecarSuffix} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("%s was pruned although it is the archive being restored: %v", n, err)
		}
	}
	if err := pruneAppdata(context.Background(), target, inst, Retention{Daily: 1}, now, "alpha", newest, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldest+".age")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("with nothing being restored the sixth-newest pre-change archive stays: %v", err)
	}
}
