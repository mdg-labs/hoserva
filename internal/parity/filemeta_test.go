package parity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A file SnapRAID creates while fixing is the daemon's user with mode 0600 and
// a directory it creates 0755, whatever the lost one was (SnapRAID's
// cmdline/handle.c and cmdline/support.c); the modes below differ from both.
const (
	recordedFileMode = 0o640
	recordedTopMode  = 0o750
	recordedDirMode  = 0o770
)

const reportRel = "docs/Reports/Q3.txt"

func listLogFor(mount string, rels ...string) string {
	body := "data:d1:" + mount + "/\n"
	for _, rel := range rels {
		body += fmt.Sprintf("file:d1:%s:17:1790006330:712259445:132\n", rel)
	}
	return body + fmt.Sprintf("summary:file_count:%d\nsummary:exit:ok\n", len(rels))
}

func fixLogFor(mount string, recovered ...string) string {
	body := "command:fix\ndata:d1:" + mount + "/\n"
	for _, rel := range recovered {
		body += "status:recovered:d1:" + rel + "\n"
	}
	return body + "summary:error:0\nsummary:error_recovered:0\nsummary:error_unrecoverable:0\nsummary:exit:ok\n"
}

func seedReport(t *testing.T, mount string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(mount, "docs/Reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, reportRel), []byte("quarterly numbers"), 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, mode := range map[string]os.FileMode{reportRel: recordedFileMode, "docs": recordedTopMode, "docs/Reports": recordedDirMode} {
		if err := os.Chmod(filepath.Join(mount, rel), mode); err != nil {
			t.Fatal(err)
		}
	}
}

// loseReport is what a delete followed by `snapraid fix` leaves: the file and
// the directories above it, created the way SnapRAID creates them.
func loseReport(t *testing.T, mount string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(mount, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mount, "docs/Reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"docs", "docs/Reports"} {
		if err := os.Chmod(filepath.Join(mount, rel), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mount, reportRel), []byte("quarterly numbers"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(mount, reportRel), 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireBirthTime skips a test on a filesystem that does not report
// creation times, which the restore needs to tell what a fix created. The
// lab's XFS data disks do.
func requireBirthTime(t *testing.T, dir string) {
	t.Helper()
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, 0, unix.STATX_BTIME, &stx); err != nil || stx.Mask&unix.STATX_BTIME == 0 {
		t.Skipf("%s reports no creation times", dir)
	}
}

func syncScript(t *testing.T, list string) []scriptedResult {
	return []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
		{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
		{logBody: list},
	}
}

func runSync(t *testing.T, e *SnapraidEngine) {
	t.Helper()
	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drain(t, ch); final.Err != nil {
		t.Fatalf("Sync final Progress.Err = %v, want nil", final.Err)
	}
}

// runFix runs a fix whose log is log; during, if set, does to the disks what
// snapraid does while it runs, so what it creates is newer than the fix.
func runFix(t *testing.T, e *SnapraidEngine, log string, during ...func()) error {
	t.Helper()
	res := scriptedResult{logBody: log}
	if len(during) > 0 {
		res.before = during[0]
	}
	e.Runner = &scriptedRunner{t: t, script: []scriptedResult{res}}
	ch, err := e.Fix(context.Background(), FixOpts{})
	if err != nil {
		t.Fatalf("Fix: %v", err)
	}
	return drain(t, ch).Err
}

func modeOf(t *testing.T, path string) (mode uint32, uid, gid uint32) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return st.Mode & 0o7777, st.Uid, st.Gid
}

func assertMode(t *testing.T, path string, want uint32) {
	t.Helper()
	if got, _, _ := modeOf(t, path); got != want {
		t.Errorf("%s: mode %04o, want %04o", path, got, want)
	}
}

func TestSnapraidEngine_SyncRecordsMetadataFixRestoresIt(t *testing.T) {
	mount := t.TempDir()
	seedReport(t, mount)
	usage := NewUsageStore(newTestDB(t))
	r := &scriptedRunner{t: t, script: syncScript(t, listLogFor(mount, reportRel))}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r, Usage: usage}
	runSync(t, e)

	got, ok, err := usage.FileMeta().Get(context.Background(), mount, reportRel)
	if err != nil || !ok {
		t.Fatalf("Get(%s) = %+v, %v, %v, want the recorded file", reportRel, got, ok, err)
	}
	_, uid, gid := modeOf(t, filepath.Join(mount, reportRel))
	if want := (FileMeta{Disk: mount, RelPath: reportRel, UID: uid, GID: gid, Mode: recordedFileMode}); got != want {
		t.Fatalf("recorded %+v, want %+v", got, want)
	}

	requireBirthTime(t, mount)
	if err := runFix(t, e, fixLogFor(mount, reportRel), func() { loseReport(t, mount) }); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	assertMode(t, filepath.Join(mount, reportRel), recordedFileMode)
	assertMode(t, filepath.Join(mount, "docs"), recordedTopMode)
	assertMode(t, filepath.Join(mount, "docs/Reports"), recordedDirMode)
}

func TestSnapraidEngine_FixWithNothingRecordedFallsBackToTheShareDefaults(t *testing.T) {
	mount := t.TempDir()
	requireBirthTime(t, mount)
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: NewUsageStore(newTestDB(t))}

	if err := runFix(t, e, fixLogFor(mount, reportRel), func() { loseReport(t, mount) }); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	assertMode(t, filepath.Join(mount, reportRel), 0o664)
	assertMode(t, filepath.Join(mount, "docs/Reports"), 0o2775)
	assertMode(t, filepath.Join(mount, "docs"), 0o755)
}

// What a fix rewrites in place, or finds already there, matches the owner and
// mode SnapRAID creates with as well as what it created does; only its
// creation time tells them apart. They keep what they have, whether or not
// anything is recorded for them (a share's defaults would loosen a 0600 file).
func TestSnapraidEngine_FixLeavesWhatWasAlreadyThereAlone(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%v", recorded), func(t *testing.T) {
			mount := t.TempDir()
			requireBirthTime(t, mount)
			loseReport(t, mount)
			usage := NewUsageStore(newTestDB(t))
			if recorded {
				err := usage.FileMeta().Replace(context.Background(), func(emit func(FileMeta) error) error {
					for rel, mode := range map[string]uint32{reportRel: recordedFileMode, "docs": recordedTopMode, "docs/Reports": recordedDirMode} {
						if err := emit(FileMeta{Disk: mount, RelPath: rel, Dir: rel != reportRel, UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()), Mode: mode}); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			time.Sleep(1100 * time.Millisecond)
			e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage}

			if err := runFix(t, e, fixLogFor(mount, reportRel)); err != nil {
				t.Fatalf("Fix: %v", err)
			}
			assertMode(t, filepath.Join(mount, reportRel), 0o600)
			assertMode(t, filepath.Join(mount, "docs"), 0o755)
			assertMode(t, filepath.Join(mount, "docs/Reports"), 0o755)
		})
	}
}

// A share user who swaps a directory above a recovered file for a symlink
// while the fix runs must not get the file the link now leads to changed:
// not by the recorded mode and not by the defaults. The directory outside the
// mount is one this test made.
func TestSnapraidEngine_FixDoesNotFollowASymlinkedAncestor(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%v", recorded), func(t *testing.T) {
			mount, outside := t.TempDir(), t.TempDir()
			requireBirthTime(t, mount)
			victim := filepath.Join(outside, "Reports", "Q3.txt")
			if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(victim, []byte("host secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			usage := NewUsageStore(newTestDB(t))
			if recorded {
				err := usage.FileMeta().Replace(context.Background(), func(emit func(FileMeta) error) error {
					return emit(FileMeta{Disk: mount, RelPath: reportRel, UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()), Mode: 0o666})
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage}

			err := runFix(t, e, fixLogFor(mount, reportRel), func() {
				if err := os.Symlink(outside, filepath.Join(mount, "docs")); err != nil {
					t.Fatal(err)
				}
			})
			if err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("Fix error = %v, want it to report the symbolic link", err)
			}
			assertMode(t, victim, 0o600)
			assertMode(t, filepath.Dir(victim), 0o755)
		})
	}
}

func TestSnapraidEngine_FixFailsWhenOwnershipCannotBeRestored(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can give a file to anyone")
	}
	mount := t.TempDir()
	requireBirthTime(t, mount)
	usage := NewUsageStore(newTestDB(t))
	other := uint32(os.Geteuid()) + 1
	err := usage.FileMeta().Replace(context.Background(), func(emit func(FileMeta) error) error {
		return emit(FileMeta{Disk: mount, RelPath: reportRel, UID: other, GID: uint32(os.Getegid()), Mode: recordedFileMode})
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage}

	if err := runFix(t, e, fixLogFor(mount, reportRel), func() { loseReport(t, mount) }); err == nil {
		t.Fatal("Fix succeeded although the restored file could not be given back to its owner")
	}
}

func TestSnapraidEngine_FixKeepsTheUnrecoverableErrorBesideAnOwnershipFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can give a file to anyone")
	}
	mount := t.TempDir()
	requireBirthTime(t, mount)
	usage := NewUsageStore(newTestDB(t))
	err := usage.FileMeta().Replace(context.Background(), func(emit func(FileMeta) error) error {
		return emit(FileMeta{Disk: mount, RelPath: reportRel, UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid()), Mode: recordedFileMode})
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage}
	log := "data:d1:" + mount + "/\nstatus:recovered:d1:" + reportRel + "\nstatus:unrecoverable:d1:other.bin\nsummary:error:2\nsummary:error_recovered:1\nsummary:error_unrecoverable:1\nsummary:exit:unrecoverable\n"

	got := runFix(t, e, log, func() { loseReport(t, mount) })
	if !errors.Is(got, ErrFixUnrecoverable) {
		t.Fatalf("Fix error = %v, want it to still be ErrFixUnrecoverable", got)
	}
	if got == nil || !strings.Contains(got.Error(), "owner and mode") {
		t.Fatalf("Fix error = %v, want it to name the owner and mode failure too", got)
	}
}

func TestSnapraidEngine_SyncWithNothingChangedKeepsWhatIsRecorded(t *testing.T) {
	mount := t.TempDir()
	seedReport(t, mount)
	usage := NewUsageStore(newTestDB(t))
	runSync(t, &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage, Runner: &scriptedRunner{t: t, script: syncScript(t, listLogFor(mount, reportRel))}})

	equal := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
		{logBody: string(readCorpus(t, "snapraid_sync_equal.log"))},
	}}
	runSync(t, &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage, Runner: equal})
	if len(equal.calls) != 3 {
		t.Fatalf("a sync with nothing to do ran %d snapraid commands, want 3 (no list to re-record): %v", len(equal.calls), equal.calls)
	}
}

func TestSnapraidEngine_SyncWithNothingChangedRecordsWhenNothingIsRecorded(t *testing.T) {
	mount := t.TempDir()
	seedReport(t, mount)
	usage := NewUsageStore(newTestDB(t))
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
		{logBody: string(readCorpus(t, "snapraid_sync_equal.log"))},
		{logBody: listLogFor(mount, reportRel)},
	}}
	runSync(t, &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage, Runner: r})

	if _, ok, err := usage.FileMeta().Get(context.Background(), mount, reportRel); err != nil || !ok {
		t.Fatalf("Get = %v, %v, want the file recorded by the first sync that had nothing recorded", ok, err)
	}
}

func TestSnapraidEngine_SyncKeepsThePreviousRecordWhenTheWalkFails(t *testing.T) {
	mount := t.TempDir()
	seedReport(t, mount)
	usage := NewUsageStore(newTestDB(t))
	runSync(t, &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage, Runner: &scriptedRunner{t: t, script: syncScript(t, listLogFor(mount, reportRel))}})

	if os.Geteuid() == 0 {
		t.Skip("root reads a directory with mode 000")
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(other, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(other, 0o700) })
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Usage: usage, Runner: &scriptedRunner{t: t, script: syncScript(t, listLogFor(other, "f"))}}
	ch, err := e.Sync(context.Background(), SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drain(t, ch); final.Err == nil {
		t.Fatal("Sync succeeded although it could not read the ownership of a tracked file")
	}
	if _, ok, _ := usage.FileMeta().Get(context.Background(), mount, reportRel); !ok {
		t.Fatal("a failed walk emptied the previous record")
	}
}
