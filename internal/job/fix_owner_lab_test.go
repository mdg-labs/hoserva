//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host. It drives the real `snapraid` through submitted TypeSync and
// TypeFix jobs, the way startSync and startFix do, and checks who owns what
// a fix restores: SnapRAID creates it as root with mode 0600 (#626).

package job

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

const (
	labShareUID = 99
	labShareGID = 100
)

func labOwn(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chown(path, labShareUID, labShareGID); err != nil {
		t.Fatalf("chown %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func labAssertOwned(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st := info.Sys().(*syscall.Stat_t)
	if st.Uid != labShareUID || st.Gid != labShareGID || info.Mode() != mode {
		t.Errorf("%s: owner %d:%d mode %v, want %d:%d mode %v", path, st.Uid, st.Gid, info.Mode(), labShareUID, labShareGID, mode)
	}
}

func submitSyncAndWait(t *testing.T, s *Scheduler) {
	t.Helper()
	j, err := s.Submit(context.Background(), TypeSync, nil, nil)
	if err != nil {
		t.Fatalf("Submit(sync): %v", err)
	}
	finished, err := s.Await(context.Background(), j.ID)
	if err != nil {
		t.Fatalf("Await(sync): %v", err)
	}
	if finished.Status != StatusSucceeded {
		t.Fatalf("sync status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}
}

// TestLabFix_RestoredFileKeepsItsOwnerGroupAndMode is #626's scenario: a
// UID 99, GID 100 file in 99:100 directories is deleted after a sync, then
// restored by a fix job. The file comes back 99:100 with its mode, whether
// only the file was deleted or its whole directory tree was.
func TestLabFix_RestoredFileKeepsItsOwnerGroupAndMode(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngineAt(t, lab, "fixowner", "fixowner")
	engine.Usage = parity.NewUsageStore(newTestDB(t))
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, false, RunSync(engine))
	s.registry.Register(TypeFix, false, RunFix(engine))

	reports := filepath.Join(mounts[1], "documents/Reports")
	file := filepath.Join(reports, "Q3.txt")
	original := writeRandomLabFile(t, file, 250_000)
	labOwn(t, file, 0o664)
	labOwn(t, reports, os.ModeSetgid|0o775)
	labOwn(t, filepath.Join(mounts[1], "documents"), os.ModeSetgid|0o775)
	submitSyncAndWait(t, s)

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	submitFixAndWait(t, s, FixParams{Confirm: true})
	if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("fix did not restore %s (err %v)", file, err)
	}
	labAssertOwned(t, file, 0o664)

	if err := os.RemoveAll(filepath.Join(mounts[1], "documents")); err != nil {
		t.Fatal(err)
	}
	submitFixAndWait(t, s, FixParams{Confirm: true})
	labAssertOwned(t, file, 0o664)
	labAssertOwned(t, reports, os.ModeDir|os.ModeSetgid|0o775)
	labAssertOwned(t, filepath.Join(mounts[1], "documents"), os.ModeDir|os.ModeSetgid|0o775)
}

// TestLabFix_NothingRecordedFallsBackToTheDirectoryOwnerAndShareMode is the
// documented fallback: with no sync that recorded ownership (an engine
// without the store), a restored file takes its directory's owner and group
// and the share default mode 0664, rather than root and 0600.
func TestLabFix_NothingRecordedFallsBackToTheDirectoryOwnerAndShareMode(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngineAt(t, lab, "fixownerfallback", "fixownerfallback")
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, false, RunSync(engine))
	s.registry.Register(TypeFix, false, RunFix(engine))

	reports := filepath.Join(mounts[1], "documents/Reports")
	file := filepath.Join(reports, "Q3.txt")
	writeRandomLabFile(t, file, 250_000)
	labOwn(t, reports, os.ModeSetgid|0o775)
	submitSyncAndWait(t, s)

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	submitFixAndWait(t, s, FixParams{Confirm: true})
	labAssertOwned(t, file, 0o664)
}

// TestLabFix_AFileRewrittenInPlaceKeepsItsOwnerAndMode: a root-owned 0600 file
// that fix rewrites where it stands matches what SnapRAID creates with, but it
// was not created by the fix, so it keeps its mode instead of taking the share
// default 0664 of a path nothing was recorded for. Its existing 0755 directory
// is left alone for the same reason.
func TestLabFix_AFileRewrittenInPlaceKeepsItsOwnerAndMode(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngineAt(t, lab, "fixownerinplace", "fixownerinplace")
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, false, RunSync(engine))
	s.registry.Register(TypeFix, false, RunFix(engine))

	reports := filepath.Join(mounts[1], "documents/Reports")
	file := filepath.Join(reports, "secret.bin")
	original := writeRandomLabFile(t, file, 250_000)
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(mounts[1], "documents"), 0o755); err != nil {
		t.Fatal(err)
	}
	submitSyncAndWait(t, s)

	time.Sleep(1100 * time.Millisecond)
	damaged := bytes.Repeat([]byte{0xAA}, len(original))
	if err := os.WriteFile(file, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	submitFixAndWait(t, s, FixParams{Confirm: true})
	if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, original) {
		t.Fatalf("fix did not rewrite %s in place (err %v)", file, err)
	}
	for path, want := range map[string]os.FileMode{file: 0o600, reports: os.ModeDir | 0o755} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || info.Mode() != want {
			t.Errorf("%s: owner %d mode %v, want root and %v", path, st.Uid, info.Mode(), want)
		}
	}
}

// TestLabFix_DoesNotGiveAFileBehindASymlinkedDirectoryToTheShareUser: the
// directory above a recovered file is a symlink to a directory outside the
// data disk, as it is when a container swaps it for one during a fix. A
// root-owned 0600 file there, a stand-in for a host secret, keeps its owner
// and mode although the record says 99:100 and 0664, and the fix fails
// naming the symlink instead of following it.
func TestLabFix_DoesNotGiveAFileBehindASymlinkedDirectoryToTheShareUser(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngineAt(t, lab, "fixownersymlink", "fixownersymlink")
	engine.Usage = parity.NewUsageStore(newTestDB(t))
	s := newTestScheduler(t)
	s.registry.Register(TypeSync, false, RunSync(engine))
	s.registry.Register(TypeFix, false, RunFix(engine))

	file := filepath.Join(mounts[1], "documents/Q3.txt")
	writeRandomLabFile(t, file, 250_000)
	labOwn(t, file, 0o664)
	submitSyncAndWait(t, s)

	outside := t.TempDir()
	secret := filepath.Join(outside, "Q3.txt")
	if err := os.WriteFile(secret, []byte("host secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(mounts[1], "documents")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mounts[1], "documents")); err != nil {
		t.Fatal(err)
	}
	finished := awaitFix(t, s, FixParams{Confirm: true})
	if finished.Status == StatusSucceeded || !strings.Contains(finished.ErrorMessage, "symbolic link") {
		t.Fatalf("fix status = %s (%s), want a failure naming the symbolic link", finished.Status, finished.ErrorMessage)
	}
	info, err := os.Lstat(secret)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); st.Uid != 0 || st.Gid != 0 || info.Mode() != 0o600 {
		t.Errorf("%s: owner %d:%d mode %v, want root:root 0600", secret, st.Uid, st.Gid, info.Mode())
	}
}
