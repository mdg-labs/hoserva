package backup

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// snapshotTree records every entry below root with its type, mode and, for
// a file, its content, so a test can prove a tree is byte-for-byte what it
// was.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		desc := info.Mode().String()
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			desc += ":" + string(b)
		}
		out[p] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (r *restoreRig) preview(t *testing.T) (AppdataRestorePreview, error) {
	t.Helper()
	return r.svc.PreviewRestore(context.Background(), AppdataRestoreRequest{
		Container: "alpha", Archive: r.archive.Name, DestinationID: r.archive.DestinationID,
	})
}

func (r *restoreRig) requireNothingChanged(t *testing.T, before map[string]string) {
	t.Helper()
	if after := snapshotTree(t, r.cache); !reflect.DeepEqual(before, after) {
		t.Fatalf("the cache disk changed during the preview:\nbefore %v\nafter  %v", before, after)
	}
	if ev := r.containers.Events(); len(ev) != 0 {
		t.Fatalf("a container was touched by the preview: %v", ev)
	}
	alpha, _ := r.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the preview, want it still running", alpha.State)
	}
	if _, err := os.Stat(r.svc.JournalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preview wrote the stopped-container journal: %v", err)
	}
}

func TestAppdataPreview_ReportsWhatARestoreWouldReplaceAddAndRemove(t *testing.T) {
	rig := newRestoreRig(t)
	// The archive holds config and sub/keep; live has a changed config, the
	// extra file, and no sub/keep.
	if err := os.Remove(filepath.Join(rig.dir, "sub", "keep")); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, rig.cache)

	got, err := rig.preview(t)
	if err != nil {
		t.Fatalf("PreviewRestore: %v", err)
	}
	rig.requireNothingChanged(t, before)

	if got.Container != "alpha" || got.Archive != rig.archive.Name || got.DestinationID != rig.archive.DestinationID || got.CreatedAt.IsZero() {
		t.Fatalf("preview header = %+v", got)
	}
	want := []AppdataDirPreview{{
		Directory: "alpha",
		Replaced:  AppdataPreviewGroup{Files: 1, Bytes: int64(len("v2-live")), Sample: []string{"config"}},
		Added:     AppdataPreviewGroup{Files: 1, Bytes: int64(len("kept")), Sample: []string{"sub/keep"}},
		Removed:   AppdataPreviewGroup{Files: 1, Bytes: int64(len("added after the backup")), Sample: []string{"extra"}},
	}}
	if !reflect.DeepEqual(got.Directories, want) {
		t.Fatalf("directories = %+v, want %+v", got.Directories, want)
	}
	if strings.Contains(fmt.Sprint(got), rig.appdata) {
		t.Fatalf("the preview discloses the appdata location %s: %+v", rig.appdata, got)
	}
}

func TestAppdataPreview_MatchesWhatTheRestoreThenDoes(t *testing.T) {
	rig := newRestoreRig(t)
	if err := rig.store.CreateDestination(context.Background(), Destination{
		ID: DefaultPoolID, Name: "Pool", Type: TypeLocal, Path: rig.poolDir, Enabled: true,
		Retention: Retention{Daily: 7, Weekly: 4, Monthly: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(rig.dir, "sub", "keep")); err != nil {
		t.Fatal(err)
	}
	previewed, err := rig.preview(t)
	if err != nil {
		t.Fatal(err)
	}
	d := previewed.Directories[0]
	if len(d.Replaced.Sample) == 0 || len(d.Added.Sample) == 0 || len(d.Removed.Sample) == 0 {
		t.Fatalf("the preview has an empty group, so this test would prove nothing: %+v", d)
	}

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	for _, rel := range d.Replaced.Sample {
		if got := readFile(t, filepath.Join(rig.dir, rel)); got != "v1" {
			t.Fatalf("replaced %s = %q after the restore, want the archived v1", rel, got)
		}
	}
	for _, rel := range d.Added.Sample {
		if got := readFile(t, filepath.Join(rig.dir, rel)); got != "kept" {
			t.Fatalf("added %s = %q after the restore", rel, got)
		}
	}
	for _, rel := range d.Removed.Sample {
		if _, err := os.Stat(filepath.Join(rig.dir, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was listed as removed but the restore left it: %v", rel, err)
		}
	}
}

func TestAppdataPreview_BoundsTheSampleAndSortsIt(t *testing.T) {
	rig := newRestoreRig(t)
	extra := AppdataPreviewSampleLimit + 10
	for i := 0; i < extra; i++ {
		if err := os.WriteFile(filepath.Join(rig.dir, fmt.Sprintf("live-%03d", i)), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := rig.preview(t)
	if err != nil {
		t.Fatal(err)
	}
	removed := got.Directories[0].Removed
	if removed.Files != int64(extra)+1 {
		t.Fatalf("removed files = %d, want every live-only file counted (%d and extra)", removed.Files, extra)
	}
	if removed.Bytes != int64(2*extra+len("added after the backup")) {
		t.Fatalf("removed bytes = %d", removed.Bytes)
	}
	if len(removed.Sample) != AppdataPreviewSampleLimit || removed.Sample[0] != "extra" || removed.Sample[1] != "live-000" {
		t.Fatalf("sample = %v, want the first %d paths in order", removed.Sample, AppdataPreviewSampleLimit)
	}
}

func TestAppdataPreview_AnAppdataDirectoryThatIsGoneIsAllAdded(t *testing.T) {
	rig := newRestoreRig(t)
	if err := os.RemoveAll(rig.dir); err != nil {
		t.Fatal(err)
	}
	got, err := rig.preview(t)
	if err != nil {
		t.Fatalf("PreviewRestore: %v", err)
	}
	d := got.Directories[0]
	if d.Replaced.Files != 0 || d.Removed.Files != 0 || d.Added.Files != 2 || !reflect.DeepEqual(d.Added.Sample, []string{"config", "sub/keep"}) {
		t.Fatalf("directory = %+v, want the two archived files added and nothing lost", d)
	}
	if _, err := os.Stat(rig.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the preview created %s: %v", rig.dir, err)
	}
}

func TestAppdataPreview_RefusesACorruptArchiveLikeTheRestoreAndLeavesNothingBehind(t *testing.T) {
	rig := newRestoreRig(t)
	for key, data := range rig.rclone.Files() {
		if strings.HasSuffix(key, ".tar.zst.age") {
			i := strings.LastIndex(key, "/")
			data[len(data)/2] ^= 0xff
			rig.rclone.Put(key[:i], key[i+1:], data, rig.now)
		}
	}
	before := snapshotTree(t, rig.cache)

	_, perr := rig.preview(t)
	if perr == nil {
		t.Fatal("PreviewRestore of a corrupt archive succeeded")
	}
	rig.requireNothingChanged(t, before)
	rerr := rig.restore(t)
	if rerr == nil || rerr.Error() != perr.Error() {
		t.Fatalf("the restore's refusal = %v, the preview's = %v; they must be the same", rerr, perr)
	}
}

func TestAppdataPreview_RefusesAnArchiveWhoseDirectoriesAreOutsideAppdataLikeTheRestore(t *testing.T) {
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
	req := AppdataRestoreRequest{Container: "alpha", Archive: name, DestinationID: "local"}
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
}

func TestAppdataPreview_RefusesTheRequestsTheRestoreRefuses(t *testing.T) {
	rig := newRestoreRig(t)
	ctx := context.Background()
	before := snapshotTree(t, rig.cache)
	cases := []struct {
		name string
		req  AppdataRestoreRequest
		want error
	}{
		{"another container's archive", AppdataRestoreRequest{Container: "beta", Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID}, ErrAppdataArchiveInvalid},
		{"an archive the destination does not hold", AppdataRestoreRequest{Container: "alpha", Archive: strings.Replace(rig.archive.Name, "-2026", "-2025", 1), DestinationID: rig.archive.DestinationID}, ErrAppdataArchiveNotFound},
		{"a path instead of a name", AppdataRestoreRequest{Container: "alpha", Archive: "../../etc/passwd", DestinationID: rig.archive.DestinationID}, ErrAppdataArchiveInvalid},
		{"an unknown destination", AppdataRestoreRequest{Container: "alpha", Archive: rig.archive.Name, DestinationID: "nope"}, ErrDestinationNotFound},
	}
	for _, c := range cases {
		_, err := rig.svc.PreviewRestore(ctx, c.req)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: PreviewRestore = %v, want %v", c.name, err, c.want)
		}
	}
	rig.requireNothingChanged(t, before)
}

func TestAppdataPreview_RefusesWhileTheArrayIsStopped(t *testing.T) {
	rig := newRestoreRig(t)
	rig.halted = true
	before := snapshotTree(t, rig.cache)
	if _, err := rig.preview(t); err == nil {
		t.Fatal("PreviewRestore with the array stopped succeeded")
	}
	rig.halted = false
	rig.requireNothingChanged(t, before)
}

// A preview is a job the scheduler runs behind a backup or restore of the
// same container and beside those of others, so it must neither refuse
// because one is running nor make one refuse: it takes no part in their
// lock.
func TestAppdataPreview_TakesNoPartInTheBackupAndRestoreLock(t *testing.T) {
	rig := newRestoreRig(t)
	rig.svc.runMu.Lock()
	got, err := rig.preview(t)
	rig.svc.runMu.Unlock()
	if err != nil || len(got.Directories) != 1 {
		t.Fatalf("PreviewRestore while a run holds the lock = %+v, %v; want it to go ahead", got, err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	rig.svc.Containers = &gatedContainers{AppdataContainers: rig.svc.Containers, entered: entered, release: release}
	done := make(chan error, 1)
	go func() { _, err := rig.preview(t); done <- err }()
	<-entered
	rig.now = rig.now.Add(time.Hour)
	if err := rig.run(t); err != nil {
		t.Fatalf("a backup started while a preview was running failed: %v\n%s", err, rig.out)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the preview: %v", err)
	}
}

// gatedContainers holds the first RequireArrayRunning until released, to
// keep a preview in flight while the test starts something else.
type gatedContainers struct {
	AppdataContainers
	held             atomic.Bool
	entered, release chan struct{}
}

func (g *gatedContainers) RequireArrayRunning() error {
	if g.held.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.release
	}
	return g.AppdataContainers.RequireArrayRunning()
}

func TestAppdataPreview_StagingOfOnePreviewSurvivesAnotherAndIsRemovedWithIt(t *testing.T) {
	appdata := filepath.Join(t.TempDir(), "cache", "appdata")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []string{appdata}
	base := filepath.Join(filepath.Dir(appdata), appdataPreviewStagingDir)
	stale := filepath.Join(base, "preview-left-by-a-dead-daemon")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	var p appdataPreviews

	first, releaseFirst, err := p.stage(roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a directory an earlier daemon left behind survived: %v", err)
	}
	second, releaseSecond, err := p.stage(roots)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two previews share the staging directory %s", first)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("starting a second preview removed the first one's staging: %v", err)
	}
	releaseFirst()
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the first staging directory survived its release: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("releasing the first preview removed the second one's staging: %v", err)
	}
	releaseSecond()
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staging base survived the last preview: %v", err)
	}
}

func TestAppdataPreview_LeavesTheBackupStagingDirectoryAlone(t *testing.T) {
	rig := newRestoreRig(t)
	backupStaging := filepath.Join(rig.cache, appdataStagingDir)
	if err := os.MkdirAll(backupStaging, 0o700); err != nil {
		t.Fatal(err)
	}
	inFlight := filepath.Join(backupStaging, "half-built.tar.zst")
	if err := os.WriteFile(inFlight, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.preview(t); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("the preview cleared the backup's staging directory: %v", err)
	}
}

func TestAppdataPreview_RunPreviewHoldsTheResultOnlyOnSuccess(t *testing.T) {
	rig := newRestoreRig(t)
	req := AppdataRestoreRequest{Container: "alpha", Archive: rig.archive.Name, DestinationID: rig.archive.DestinationID}
	if _, ok := rig.svc.Preview("job-1"); ok {
		t.Fatal("a result exists before any preview ran")
	}
	if err := rig.svc.RunPreview(context.Background(), "job-1", req, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, ok := rig.svc.Preview("job-1")
	if !ok || len(got.Directories) != 1 || got.Directories[0].Directory != "alpha" {
		t.Fatalf("Preview = %+v, %v", got, ok)
	}

	bad := req
	bad.Container = "beta"
	if err := rig.svc.RunPreview(context.Background(), "job-2", bad, io.Discard); !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("RunPreview of another container's archive = %v, want ErrAppdataArchiveInvalid", err)
	}
	if _, ok := rig.svc.Preview("job-2"); ok {
		t.Fatal("a failed preview left a result")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := snapshotTree(t, rig.cache)
	if err := rig.svc.RunPreview(ctx, "job-3", req, io.Discard); err == nil {
		t.Fatal("a cancelled preview succeeded")
	}
	if _, ok := rig.svc.Preview("job-3"); ok {
		t.Fatal("a cancelled preview left a result")
	}
	rig.requireNothingChanged(t, before)
}

func TestAppdataPreview_HoldsTheNewestResultsOnly(t *testing.T) {
	var p appdataPreviews
	for i := 0; i < AppdataPreviewKeep+3; i++ {
		p.put(fmt.Sprint("job-", i), AppdataRestorePreview{Container: fmt.Sprint(i)})
	}
	for i := 0; i < AppdataPreviewKeep+3; i++ {
		p.mu.Lock()
		_, held := p.results[fmt.Sprint("job-", i)]
		p.mu.Unlock()
		if want := i >= 3; held != want {
			t.Errorf("job-%d held = %v, want %v", i, held, want)
		}
	}
}

func TestAppdataPreview_RefusesWithoutAnAppdataLocation(t *testing.T) {
	rig := newRestoreRig(t)
	rig.svc.Roots = func(context.Context) ([]string, error) { return nil, nil }
	if _, err := rig.preview(t); !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("PreviewRestore without a cache disk = %v, want ErrAppdataArchiveInvalid", err)
	}
}

// writeTestArchive writes a zstd tar of entries and returns its path.
func writeTestArchive(t *testing.T, entries ...tar.Header) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.zst")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for _, h := range entries {
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// The preview refuses an archive exactly when extraction does, so a restore
// it lets through is not one that stops the container, spends a snapshot
// slot and then fails to unpack.
func TestReadArchivedFiles_AgreesWithExtractionOnEveryEntryLayout(t *testing.T) {
	dir := func(name string) tar.Header { return tar.Header{Name: "data/0/" + name, Typeflag: tar.TypeDir} }
	reg := func(name string) tar.Header { return tar.Header{Name: "data/0/" + name, Typeflag: tar.TypeReg} }
	link := func(name, to string) tar.Header {
		return tar.Header{Name: "data/0/" + name, Typeflag: tar.TypeSymlink, Linkname: to}
	}
	cases := []struct {
		name    string
		entries []tar.Header
		refused bool
	}{
		{"an entry that climbs out of its tree", []tar.Header{reg("../escape")}, true},
		{"a hard link", []tar.Header{{Name: "data/0/link", Typeflag: tar.TypeLink, Linkname: "data/0/x"}}, true},
		{"a path that appears twice", []tar.Header{reg("x"), reg("x")}, true},
		{"a tree the header does not name", []tar.Header{{Name: "data/1/x", Typeflag: tar.TypeReg}}, true},
		{"a file below a link that leaves the tree", []tar.Header{link("l", "/tmp"), reg("l/x")}, true},
		{"a file below a relative link that leaves the tree", []tar.Header{dir("d"), link("l", "../.."), reg("l/x")}, true},
		{"a file below a link to nothing", []tar.Header{link("l", "missing"), reg("l/x")}, true},
		{"a directory listed twice", []tar.Header{dir("d"), dir("d")}, true},
		{"a directory where a file is", []tar.Header{reg("d"), dir("d")}, true},
		{"a link where a directory is", []tar.Header{dir("d"), link("d", "e")}, true},
		{"a file below a file", []tar.Header{reg("a"), reg("a/b")}, true},
		{"a file whose directory is not listed first", []tar.Header{reg("d/f")}, true},
		{"a link with no target", []tar.Header{link("l", "")}, true},
		{"a link loop below a file", []tar.Header{link("a", "b"), link("b", "a"), reg("a/x")}, true},
		{"a link that names an existing file's place", []tar.Header{dir("d"), link("l", "d"), reg("d/x"), reg("l/x")}, true},
		{"files in listed directories", []tar.Header{dir("d"), reg("d/f"), dir("d/e"), reg("d/e/g")}, false},
		{"a link to a listed directory in the tree", []tar.Header{dir("d"), link("l", "d"), reg("l/x")}, false},
		{"a link back up to a listed directory in the tree", []tar.Header{dir("d"), dir("d/e"), link("d/e/up", ".."), reg("d/e/up/x")}, false},
	}
	for _, c := range cases {
		archive := writeTestArchive(t, c.entries...)
		_, rerr := readArchivedFiles(context.Background(), archive, 1)
		target := filepath.Join(t.TempDir(), "fresh")
		xerr := extractAppdata(context.Background(), archive, appdataHeader{Dirs: []string{"unused"}}, []string{target})
		if (xerr != nil) != c.refused {
			t.Errorf("%s: extraction refused = %v (%v), the case expects %v", c.name, xerr != nil, xerr, c.refused)
		}
		if (rerr != nil) != c.refused {
			t.Errorf("%s: readArchivedFiles refused = %v (%v), extraction's %v", c.name, rerr != nil, rerr, xerr != nil)
		}
		if rerr != nil && !errors.Is(rerr, ErrAppdataArchiveInvalid) {
			t.Errorf("%s: readArchivedFiles = %v, want ErrAppdataArchiveInvalid", c.name, rerr)
		}
	}
}

func TestReadArchivedFiles_ListsFilesWhereExtractionPutsThem(t *testing.T) {
	files, err := readArchivedFiles(context.Background(), writeTestArchive(t,
		tar.Header{Name: "data/0/", Typeflag: tar.TypeDir},
		tar.Header{Name: "data/0/d/", Typeflag: tar.TypeDir},
		tar.Header{Name: "data/0/d/f", Typeflag: tar.TypeReg, Size: 0},
		tar.Header{Name: "data/0/l", Typeflag: tar.TypeSymlink, Linkname: "d"},
		tar.Header{Name: "data/0/l/g", Typeflag: tar.TypeReg},
		tar.Header{Name: "data/0/s", Typeflag: tar.TypeSymlink, Linkname: "d/f"},
	), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"d/f": 0, "d/g": 0, "l": 0, "s": 0}
	if !reflect.DeepEqual(files[0], want) {
		t.Fatalf("files = %v, want %v: directories are not files, a link is, and l/g is d/g", files[0], want)
	}
}

func TestReadArchivedFiles_RefusesAFileWhereTheTreesRootIs(t *testing.T) {
	_, err := readArchivedFiles(context.Background(), writeTestArchive(t, tar.Header{Name: "data/0/.", Typeflag: tar.TypeReg}), 1)
	if !errors.Is(err, ErrAppdataArchiveInvalid) {
		t.Fatalf("readArchivedFiles = %v, want ErrAppdataArchiveInvalid", err)
	}
}

func TestAppdataPreview_ADirectorySwappedForALinkIsNotListed(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "appdata")
	dir := filepath.Join(root, "alpha")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(dir, "sub"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, content := range map[string]string{
		filepath.Join(dir, "sub", "inside"): "inside",
		filepath.Join(outside, "secret"):    "outside secret content",
	} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var swapped bool
	beforeOpen = func(rel string) {
		if rel != "sub" || swapped {
			return
		}
		swapped = true
		if err := os.RemoveAll(filepath.Join(dir, "sub")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeOpen = nil })

	got, err := previewDirectory(context.Background(), dir, []string{root}, map[string]int64{})
	if err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("the walk never reached the directory")
	}
	if fmt.Sprint(got) != fmt.Sprint(AppdataDirPreview{Directory: "alpha"}) {
		t.Fatalf("preview = %+v, want nothing listed for the swapped directory", got)
	}
}

func TestAppdataPreview_AnAppdataDirectoryThatIsALinkIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "appdata")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	if got, err := previewDirectory(context.Background(), filepath.Join(root, "alpha"), []string{root}, map[string]int64{}); err == nil {
		t.Fatalf("preview of a link = %+v, want it refused", got)
	}
}

func TestAppdataPreview_ADirectoryRemovedBeforeItIsListedDoesNotEndTheComparison(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "appdata")
	dir := filepath.Join(root, "alpha")
	for _, d := range []string{filepath.Join(dir, "a"), filepath.Join(dir, "b")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(dir, "a", "gone"), filepath.Join(dir, "b", "live"), filepath.Join(dir, "z")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var removed bool
	beforeList = func(rel string) {
		if rel != "a" || removed {
			return
		}
		removed = true
		if err := os.RemoveAll(filepath.Join(dir, "a")); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeList = nil })

	got, err := previewDirectory(context.Background(), dir, []string{root}, map[string]int64{"z": 1})
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("the walk never listed the directory")
	}
	if got.Replaced.Files != 1 || got.Replaced.Sample[0] != "z" || got.Removed.Files != 1 || got.Removed.Sample[0] != "b/live" || got.Added.Files != 0 {
		t.Fatalf("preview = %+v, want z replaced and b/live removed: the siblings after the vanished directory are still compared", got)
	}
}
