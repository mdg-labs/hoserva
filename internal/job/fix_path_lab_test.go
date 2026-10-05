//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on
// the host: it is built with `go test -tags lab -c` and the binary is run
// inside the lab container by `make test-lab`. It drives the real `snapraid`
// against the lab's standing array through a submitted TypeFix job.

package job

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func labFixEngine(t *testing.T, lab string) (*parity.SnapraidEngine, [3]string) {
	t.Helper()
	return labFixEngineAt(t, lab, "snapraid", "")
}

// labFixEngineAt builds an engine over the lab's three data disks. With an
// empty subdir it is the standing array on whole disks and the shared content
// files. With a subdir it is a separate array of its own: each data disk is
// that directory on the disk, the content and parity files carry name, and
// anything left from an earlier run is removed first. That array holds only
// what the test writes into it, so SnapRAID assigns every disk's first file
// parity position 0 whatever other tests synced on the same disks.
func labFixEngineAt(t *testing.T, lab, name, subdir string) (*parity.SnapraidEngine, [3]string) {
	t.Helper()
	var mounts [3]string
	var conf strings.Builder
	parityFile := filepath.Join(lab, "mnt/parity1", name+".parity")
	contentFiles := []string{filepath.Join(lab, "mnt/parity1", name+".content"), filepath.Join(lab, "mnt/cache", name+".content")}
	if subdir != "" {
		for _, p := range append([]string{parityFile}, contentFiles...) {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				t.Fatalf("removing %s from an earlier run: %v", p, err)
			}
		}
	}
	fmt.Fprintf(&conf, "parity %s\n", parityFile)
	for _, c := range contentFiles {
		fmt.Fprintf(&conf, "content %s\n", c)
	}
	for i := range mounts {
		mounts[i] = filepath.Join(lab, fmt.Sprintf("mnt/disk%d", i+1), subdir)
		if subdir != "" {
			if err := os.RemoveAll(mounts[i]); err != nil {
				t.Fatalf("removing %s from an earlier run: %v", mounts[i], err)
			}
			if err := os.MkdirAll(mounts[i], 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", mounts[i], err)
			}
		}
		fmt.Fprintf(&conf, "data d%d %s/\n", i+1, mounts[i])
	}
	workDir := filepath.Join(lab, "fix-path-lab-test", name)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", workDir, err)
	}
	confPath := filepath.Join(workDir, "snapraid.conf")
	if err := os.WriteFile(confPath, []byte(conf.String()), 0o644); err != nil {
		t.Fatalf("writing %s: %v", confPath, err)
	}
	return &parity.SnapraidEngine{
		ConfPath: confPath,
		LogDir:   filepath.Join(workDir, "logs"),
		Runner:   parity.CommandRunner{},
		Guard:    parity.Guard{Config: parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100}},
	}, mounts
}

func writeRandomLabFile(t *testing.T, path string, size int) []byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("reading random data: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return data
}

func submitFixAndWait(t *testing.T, s *Scheduler, params FixParams) *Job {
	t.Helper()
	finished := awaitFix(t, s, params)
	if finished.Status != StatusSucceeded {
		t.Fatalf("fix %+v status = %s (%s), want succeeded", params, finished.Status, finished.ErrorMessage)
	}
	return finished
}

func awaitFix(t *testing.T, s *Scheduler, params FixParams) *Job {
	t.Helper()
	j, err := s.Submit(context.Background(), TypeFix, nil, mustJSON(t, params))
	if err != nil {
		t.Fatalf("Submit(fix %+v): %v", params, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	finished, err := s.Await(ctx, j.ID)
	if err != nil {
		t.Fatalf("Await(fix %+v): %v", params, err)
	}
	return finished
}

// TestLabFixPath_RestoresOneFileAndKeepsEveryOtherChange is #619's data-loss
// scenario: after a sync, file A is edited and file C is deleted on purpose,
// and file B is deleted by accident. A fix job given B's path restores B and
// leaves A's edited bytes and C's deletion alone. A fix job without a path
// is the whole-array fix, and still reverts both (the behaviour a path
// exists to avoid).
func TestLabFixPath_RestoresOneFileAndKeepsEveryOtherChange(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngine(t, lab)
	s := newTestScheduler(t)
	s.registry.Register(TypeFix, false, RunFix(engine))

	pathA := filepath.Join(mounts[1], "docs/a.bin")
	pathB := filepath.Join(mounts[1], "docs/b.bin")
	pathC := filepath.Join(mounts[1], "docs/c.bin")
	originalA := writeRandomLabFile(t, pathA, 250_000)
	originalB := writeRandomLabFile(t, pathB, 250_000)
	originalC := writeRandomLabFile(t, pathC, 250_000)

	ctx := context.Background()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, ch); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	editedA := make([]byte, len(originalA))
	copy(editedA, originalA)
	for i := range 4096 {
		editedA[i] ^= 0xff
	}
	if err := os.WriteFile(pathA, editedA, 0o644); err != nil {
		t.Fatalf("editing %s: %v", pathA, err)
	}
	for _, p := range []string{pathB, pathC} {
		if err := os.Remove(p); err != nil {
			t.Fatalf("removing %s: %v", p, err)
		}
	}

	bPool := "/mnt/user/docs/b.bin"
	submitFixAndWait(t, s, FixParams{Confirm: true, Path: &bPool})

	gotB, err := os.ReadFile(pathB)
	if err != nil {
		t.Fatalf("reading restored %s: %v", pathB, err)
	}
	if !bytes.Equal(gotB, originalB) {
		t.Fatalf("%s was not restored to its synced bytes", pathB)
	}
	gotA, err := os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("reading %s: %v", pathA, err)
	}
	if !bytes.Equal(gotA, editedA) {
		t.Fatalf("%s no longer holds the edit made after the sync: the path-scoped fix reverted a file it was not asked to restore", pathA)
	}
	if _, err := os.Stat(pathC); !os.IsNotExist(err) {
		t.Fatalf("%s exists again (stat err %v): the path-scoped fix brought back a file it was not asked to restore", pathC, err)
	}

	submitFixAndWait(t, s, FixParams{Confirm: true})

	gotA, err = os.ReadFile(pathA)
	if err != nil {
		t.Fatalf("reading %s after the whole-array fix: %v", pathA, err)
	}
	if !bytes.Equal(gotA, originalA) {
		t.Fatalf("the whole-array fix left %s with the edit: its behaviour is no longer a fix of every change since the sync", pathA)
	}
	gotC, err := os.ReadFile(pathC)
	if err != nil || !bytes.Equal(gotC, originalC) {
		t.Fatalf("the whole-array fix did not bring %s back (err %v)", pathC, err)
	}
}

// TestLabFixPath_NothingRestoredIsNotSuccess is the other half of #619: a
// path SnapRAID's -f matches nothing for (a misspelt name, the wrong case, a
// file created after the last sync, a file that is intact and needs no
// restoring) makes `snapraid fix` exit 0 with "Nothing to do". The fix job
// must not end succeeded for it, because a user who trusts it lets the next
// sync write the deletion into parity, and the restore drill would record a
// recovery that never happened.
func TestLabFixPath_NothingRestoredIsNotSuccess(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngine(t, lab)
	s := newTestScheduler(t)
	s.registry.Register(TypeFix, false, RunFix(engine))

	pathB := filepath.Join(mounts[1], "keep/b.bin")
	pathIntact := filepath.Join(mounts[1], "keep/intact.bin")
	originalB := writeRandomLabFile(t, pathB, 250_000)
	writeRandomLabFile(t, pathIntact, 250_000)

	ctx := context.Background()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, ch); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	pathNew := filepath.Join(mounts[1], "keep/new.bin")
	writeRandomLabFile(t, pathNew, 250_000)
	if err := os.Remove(pathNew); err != nil {
		t.Fatalf("removing %s: %v", pathNew, err)
	}
	if err := os.Remove(pathB); err != nil {
		t.Fatalf("removing %s: %v", pathB, err)
	}

	for name, p := range map[string]string{
		"misspelt":        "/mnt/user/keep/bb.bin",
		"wrong case":      "/mnt/user/keep/B.bin",
		"never synced":    "/mnt/user/keep/new.bin",
		"intact":          "/mnt/user/keep/intact.bin",
		"unknown share":   "/mnt/user/nowhere/b.bin",
		"directory only":  "/mnt/user/keep",
		"wrong dir. case": "/mnt/user/Keep/b.bin",
	} {
		path := p
		finished := awaitFix(t, s, FixParams{Confirm: true, Path: &path})
		if finished.Status == StatusSucceeded {
			t.Errorf("%s: fix of %s recovered nothing but the job succeeded", name, path)
			continue
		}
		if !strings.Contains(finished.ErrorMessage, "restored nothing") {
			t.Errorf("%s: fix of %s failed with %q, want a message that it restored nothing", name, path, finished.ErrorMessage)
		}
	}
	if _, err := os.Stat(pathB); !os.IsNotExist(err) {
		t.Fatalf("%s exists after fixes for other paths (stat err %v)", pathB, err)
	}

	bPool := "/mnt/user/keep/b.bin"
	submitFixAndWait(t, s, FixParams{Confirm: true, Path: &bPool})
	gotB, err := os.ReadFile(pathB)
	if err != nil || !bytes.Equal(gotB, originalB) {
		t.Fatalf("the correct path did not restore %s (err %v)", pathB, err)
	}
}

// TestLabFixPath_UnrecoverableFileIsNotSuccess is the case where the path is
// right and in parity but SnapRAID cannot rebuild the file: B is deleted from
// disk 3, and a file on disk 1 that shares B's parity position is rewritten
// after the sync, so the parity block no longer matches what disk 1 holds.
// SnapRAID exits 1, leaves a partial B.unrecoverable behind and writes no
// recovered file. The job must fail, and say that parity could not rebuild
// the file and where the partial copy is, not that the path was wrong.
//
// SnapRAID assigns parity positions per data disk, each disk's first file
// taking position 0. The test therefore runs in an array of its own (one
// directory on each disk, its own content and parity files) that holds only
// B on disk 3 and the other file on disk 1: both are first on their disk, so
// both sit at position 0 whatever the other tests synced on these disks.
// If B nonetheless comes back with its synced bytes, the premise is gone
// and the test says so instead of blaming the product.
func TestLabFixPath_UnrecoverableFileIsNotSuccess(t *testing.T) {
	lab := labDir(t)
	engine, mounts := labFixEngineAt(t, lab, "snapraid-unrec", "fix-unrec")
	s := newTestScheduler(t)
	s.registry.Register(TypeFix, false, RunFix(engine))

	pathB := filepath.Join(mounts[2], "pr/unrec.bin")
	pathOther := filepath.Join(mounts[0], "pr/other.bin")
	const otherSize = 2_000_000
	originalB := writeRandomLabFile(t, pathB, 250_000)
	writeRandomLabFile(t, pathOther, otherSize)

	ctx := context.Background()
	ch, err := engine.Sync(ctx, parity.SyncOpts{})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if final := drainRealProgress(t, ch); final.Err != nil {
		t.Fatalf("Sync failed: %v", final.Err)
	}

	if err := os.Remove(pathB); err != nil {
		t.Fatalf("removing %s: %v", pathB, err)
	}
	writeRandomLabFile(t, pathOther, otherSize)

	bPool := "/mnt/user/pr/unrec.bin"
	finished := awaitFix(t, s, FixParams{Confirm: true, Path: &bPool})
	if finished.Status == StatusSucceeded {
		if gotB, err := os.ReadFile(pathB); err == nil && bytes.Equal(gotB, originalB) {
			t.Fatalf("precondition lost: %s was rebuilt from parity, so the file rewritten on disk 1 no longer shares its parity position (this test's array is meant to hold only these two files, each first on its disk); this is not a product failure", bPool)
		}
		t.Fatalf("fix of %s succeeded although parity could not rebuild it", bPool)
	}
	leftover := pathB + ".unrecoverable"
	for _, want := range []string{"could not rebuild", "pr/unrec.bin.unrecoverable on disk d3"} {
		if !strings.Contains(finished.ErrorMessage, want) {
			t.Errorf("fix of %s failed with %q, want it to contain %q", bPool, finished.ErrorMessage, want)
		}
	}
	if strings.Contains(finished.ErrorMessage, "restored nothing") {
		t.Errorf("fix of %s failed with %q: the path is in parity, the message must not blame it", bPool, finished.ErrorMessage)
	}
	if _, err := os.Stat(leftover); err != nil {
		t.Errorf("the partial copy %s is missing (%v): the test did not reach SnapRAID's unrecoverable case", leftover, err)
	}
}

// TestLabFix_UnrecoverableBlocksAreNotSuccess is #646: a fix without a path,
// whole-array or of one disk, that SnapRAID cannot finish exits 1 with
// unrecoverable blocks and used to end succeeded. The array is the one
// TestLabFixPath_UnrecoverableFileIsNotSuccess builds (B deleted from disk 3, a
// file at the same parity position on disk 1 rewritten after the sync), built
// fresh for each variant because a fix leaves its own partial copies behind.
// The job must fail, say that blocks were not restored and name the partial
// copy; as in that test, a B that comes back with its synced bytes means the
// premise is gone, not that the product is wrong.
func TestLabFix_UnrecoverableBlocksAreNotSuccess(t *testing.T) {
	lab := labDir(t)
	for name, params := range map[string]FixParams{
		"whole array": {Confirm: true},
		"one disk":    {Confirm: true, Disk: intPtr(3)},
	} {
		engine, mounts := labFixEngineAt(t, lab, "snapraid-unrec-all", "fix-unrec-all")
		s := newTestScheduler(t)
		s.registry.Register(TypeFix, false, RunFix(engine))

		pathB := filepath.Join(mounts[2], "pr/unrec.bin")
		pathOther := filepath.Join(mounts[0], "pr/other.bin")
		const otherSize = 2_000_000
		originalB := writeRandomLabFile(t, pathB, 250_000)
		writeRandomLabFile(t, pathOther, otherSize)

		ch, err := engine.Sync(context.Background(), parity.SyncOpts{})
		if err != nil {
			t.Fatalf("%s: Sync: %v", name, err)
		}
		if final := drainRealProgress(t, ch); final.Err != nil {
			t.Fatalf("%s: Sync failed: %v", name, final.Err)
		}
		if err := os.Remove(pathB); err != nil {
			t.Fatalf("%s: removing %s: %v", name, pathB, err)
		}
		writeRandomLabFile(t, pathOther, otherSize)

		finished := awaitFix(t, s, params)
		if finished.Status == StatusSucceeded {
			if gotB, err := os.ReadFile(pathB); err == nil && bytes.Equal(gotB, originalB) {
				t.Fatalf("%s: precondition lost: %s was rebuilt from parity, so the file rewritten on disk 1 no longer shares its parity position; this is not a product failure", name, pathB)
			}
			t.Fatalf("%s: fix %+v succeeded although parity could not rebuild every block", name, params)
		}
		for _, want := range []string{"unrecoverable block", "last sync", "pr/unrec.bin.unrecoverable on disk d3"} {
			if !strings.Contains(finished.ErrorMessage, want) {
				t.Errorf("%s: fix failed with %q, want it to contain %q", name, finished.ErrorMessage, want)
			}
		}
		if _, err := os.Stat(pathB + ".unrecoverable"); err != nil {
			t.Errorf("%s: the partial copy %s.unrecoverable is missing (%v): the test did not reach SnapRAID's unrecoverable case", name, pathB, err)
		}
	}
}
