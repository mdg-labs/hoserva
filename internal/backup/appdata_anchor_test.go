package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// fakeDirAttrs keeps attributes by the device and inode of the directory the
// descriptor names, so a rename carries them with the directory as a real
// filesystem does. GetErr and CreateErr, when set, are returned instead.
type fakeDirAttrs struct {
	mu        sync.Mutex
	vals      map[devIno]map[string][]byte
	GetErr    error
	CreateErr error
	Creates   int
}

func newFakeDirAttrs() *fakeDirAttrs {
	return &fakeDirAttrs{vals: map[devIno]map[string][]byte{}}
}

func idOfFd(fd int) (devIno, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return devIno{}, err
	}
	return devIno{uint64(st.Dev), uint64(st.Ino)}, nil
}

func (f *fakeDirAttrs) Get(fd int, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	id, err := idOfFd(fd)
	if err != nil {
		return nil, err
	}
	v, ok := f.vals[id][name]
	if !ok {
		return nil, unix.ENODATA
	}
	return append([]byte(nil), v...), nil
}

func (f *fakeDirAttrs) Create(fd int, name string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Creates++
	if f.CreateErr != nil {
		return f.CreateErr
	}
	id, err := idOfFd(fd)
	if err != nil {
		return err
	}
	if _, ok := f.vals[id][name]; ok {
		return unix.EEXIST
	}
	if f.vals[id] == nil {
		f.vals[id] = map[string][]byte{}
	}
	f.vals[id][name] = append([]byte(nil), value...)
	return nil
}

func devInoOf(t *testing.T, dir string) devIno {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		t.Fatal(err)
	}
	return devIno{uint64(st.Dev), uint64(st.Ino)}
}

// mark sets the anchor of dir to value, replacing any.
func (f *fakeDirAttrs) mark(t *testing.T, dir, value string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	id := devInoOf(t, dir)
	if f.vals[id] == nil {
		f.vals[id] = map[string][]byte{}
	}
	f.vals[id][appdataAnchorAttr] = []byte(value)
}

func (f *fakeDirAttrs) unmark(t *testing.T, dir string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, devInoOf(t, dir))
}

// anchor returns the anchor of dir and whether it has one.
func (f *fakeDirAttrs) anchor(t *testing.T, dir string) (string, bool) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vals[devInoOf(t, dir)][appdataAnchorAttr]
	return string(v), ok
}

func (f *fakeDirAttrs) requireAnchor(t *testing.T, dir, want string) {
	t.Helper()
	got, ok := f.anchor(t, dir)
	if !ok || got != want {
		t.Fatalf("anchor of %s = %q (set: %v), want %q", dir, got, ok, want)
	}
}

func TestFakeDirAttrs_KeepsAnAnchorWithTheDirectoryWhenItIsRenamed(t *testing.T) {
	root := t.TempDir()
	a, b := root+"/a", root+"/b"
	if err := os.Mkdir(a, 0o755); err != nil {
		t.Fatal(err)
	}
	f := newFakeDirAttrs()
	f.mark(t, a, "/appdata/a")
	if err := os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	f.requireAnchor(t, b, "/appdata/a")
}

func TestAppdataBackup_AnchorsEachDirectoryItArchivesToItsOwnPath(t *testing.T) {
	rig := newAppdataRig(t)
	alpha := rig.addApp(t, "alpha", "sonarr", "running", map[string]string{"config": "v1"})
	beta := rig.addApp(t, "beta", "radarr", "exited", map[string]string{"config": "v1"})

	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	rig.attrs.requireAnchor(t, alpha, resolved(t, alpha))
	rig.attrs.requireAnchor(t, beta, resolved(t, beta))
	if strings.Contains(rig.out.String(), "warning") {
		t.Fatalf("an undisturbed backup warned:\n%s", rig.out)
	}
}

func TestAppdataBackup_NeverReplacesAnAnchorNamingAnotherPath(t *testing.T) {
	rig := newAppdataRig(t)
	alpha := rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"config": "v1"})
	rig.attrs.mark(t, alpha, "/mnt/cache/appdata/elsewhere")

	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	rig.attrs.requireAnchor(t, alpha, "/mnt/cache/appdata/elsewhere")
	if !strings.Contains(rig.out.String(), "/mnt/cache/appdata/elsewhere") || !strings.Contains(rig.out.String(), "left unchanged") {
		t.Fatalf("the backup did not say it left another path's anchor alone:\n%s", rig.out)
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 1 {
		t.Fatalf("archives = %v, want the backup written", got)
	}
}

func TestAppdataBackup_KeepsAnAnchorThatAlreadyNamesItsPathWithoutAWarning(t *testing.T) {
	rig := newAppdataRig(t)
	alpha := rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"config": "v1"})
	rig.attrs.mark(t, alpha, resolved(t, alpha))

	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	if strings.Contains(rig.out.String(), "warning") {
		t.Fatalf("warned about an anchor that is right:\n%s", rig.out)
	}
}

func TestAppdataBackup_WarnsOnceWhenNoDirectoryCanBeAnchoredAndStillBacksUp(t *testing.T) {
	for _, cause := range []error{unix.ENOTSUP, unix.EPERM} {
		t.Run(cause.Error(), func(t *testing.T) {
			rig := newAppdataRig(t)
			rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"config": "v1"})
			rig.addApp(t, "beta", "radarr", "exited", map[string]string{"config": "v1"})
			rig.attrs.CreateErr = cause

			if err := rig.run(t); err != nil {
				t.Fatalf("backup: %v\n%s", err, rig.out)
			}
			if n := strings.Count(rig.out.String(), "cannot be anchored"); n != 1 {
				t.Fatalf("%d warnings that directories cannot be anchored, want 1:\n%s", n, rig.out)
			}
			if got := rig.archives(t, rig.poolDir); len(got) != 2 {
				t.Fatalf("archives = %v, want both written", got)
			}
		})
	}
}

func TestAppdataBackup_WarnsForEachDirectoryItCannotAnchorForAnotherReasonAndStillBacksUp(t *testing.T) {
	rig := newAppdataRig(t)
	alpha := rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"config": "v1"})
	beta := rig.addApp(t, "beta", "radarr", "exited", map[string]string{"config": "v1"})
	rig.attrs.CreateErr = unix.EIO

	if err := rig.run(t); err != nil {
		t.Fatalf("backup: %v\n%s", err, rig.out)
	}
	for _, d := range []string{alpha, beta} {
		if !strings.Contains(rig.out.String(), "warning: "+resolved(t, d)+" is not anchored") {
			t.Fatalf("no warning for %s:\n%s", d, rig.out)
		}
	}
	if got := rig.archives(t, rig.poolDir); len(got) != 2 {
		t.Fatalf("archives = %v, want both written", got)
	}
}

func TestAppdataSnapshot_BeforeAnUpdateAnchorsTheDirectory(t *testing.T) {
	rig := newAppdataRig(t)
	alpha := rig.addApp(t, "alpha", "sonarr", "exited", map[string]string{"config": "v1"})

	if _, err := rig.svc.Snapshot(context.Background(), "alpha", ReasonPreUpdate, rig.out); err != nil {
		t.Fatalf("Snapshot: %v\n%s", err, rig.out)
	}
	rig.attrs.requireAnchor(t, alpha, resolved(t, alpha))
}

func TestAppdataRestore_AcceptsADirectoryWithoutAnAnchorAndAnchorsWhatItRestores(t *testing.T) {
	rig := newRestoreRig(t)
	rig.attrs.unmark(t, rig.dir)

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	rig.attrs.requireAnchor(t, rig.dir, resolved(t, rig.dir))
	if strings.Contains(rig.out.String(), "warning") {
		t.Fatalf("an undisturbed restore warned:\n%s", rig.out)
	}
}

func TestAppdataRestore_AcceptsADirectoryAnchoredToItsOwnPathAndKeepsItAnchored(t *testing.T) {
	rig := newRestoreRig(t)
	rig.attrs.requireAnchor(t, rig.dir, resolved(t, rig.dir))

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	rig.attrs.requireAnchor(t, rig.dir, resolved(t, rig.dir))
}

func TestAppdataRestore_RefusesWhenTheAnchorCannotBeRead(t *testing.T) {
	rig := newRestoreRig(t)
	rig.withLocalSnapshotDestination(t)
	rig.attrs.GetErr = unix.EIO

	err := rig.restore(t)
	if !errors.Is(err, ErrPreRestoreSnapshot) || !errors.Is(err, unix.EIO) {
		t.Fatalf("Restore = %v, want ErrPreRestoreSnapshot wrapping the read error", err)
	}
	rig.requireLiveUntouched(t)
	rig.requireNoPreRestoreSnapshot(t)
	alpha, _ := rig.engine.Inspect(context.Background(), "alpha")
	if alpha.State != "running" {
		t.Fatalf("alpha = %s after the refused restore, want it started again", alpha.State)
	}
}

func TestAppdataRestore_TreatsAFilesystemWithoutAttributesAsHavingNoAnchor(t *testing.T) {
	rig := newRestoreRig(t)
	rig.attrs.GetErr = unix.ENOTSUP
	rig.attrs.CreateErr = unix.ENOTSUP

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	if n := strings.Count(rig.out.String(), "cannot be anchored"); n != 1 {
		t.Fatalf("%d warnings that directories cannot be anchored, want 1:\n%s", n, rig.out)
	}
}

func TestAppdataRestore_WarnsAndRestoresWhenTheRestoredTreeCannotBeAnchored(t *testing.T) {
	rig := newRestoreRig(t)
	rig.attrs.CreateErr = unix.EIO

	if err := rig.restore(t); err != nil {
		t.Fatalf("Restore: %v\n%s", err, rig.out)
	}
	if got := readFile(t, filepath.Join(rig.dir, "config")); got != "v1" {
		t.Fatalf("config = %q, want the archived v1", got)
	}
	if !strings.Contains(rig.out.String(), "is not anchored") {
		t.Fatalf("the restore did not say the tree is not anchored:\n%s", rig.out)
	}
}

func TestHeldDirsRequireAnchored_ReadsTheAnchorOfTheRecordedDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	dir, other := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{dir, other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	attrs := newFakeDirAttrs()
	attrs.mark(t, other, "/elsewhere")
	held := &heldDirs{}
	defer held.close()
	recorded, err := held.recordAll([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.requireAnchored(attrs, []string{dir}, recorded); err != nil {
		t.Fatalf("requireAnchored over an unanchored directory = %v", err)
	}
	if err := os.Rename(dir, dir+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(other, dir); err != nil {
		t.Fatal(err)
	}
	err = held.requireAnchored(attrs, []string{dir}, recorded)
	if err == nil || !strings.Contains(err.Error(), "not the directory the restore recorded") {
		t.Fatalf("requireAnchored over a directory other than the recorded one = %v", err)
	}
}
