//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern internal/pool's own mount_lab_test.go uses — this file
// reuses that package's own Mount/Mounter to bring up a real cache-then-
// move share and its own array-write target, exactly as production
// would, rather than duplicating that topology logic here.
//
// It proves what an in-process fake OpenChecker and a simulated kill
// cannot: that Run, writing through a real array-only mergerfs mount,
// lands a file where mergerfs's own create policy puts it (doc 09 §2's
// "one placement algorithm"); that a file held open by a real process
// shows up as open whether that hold is direct on the cache disk or
// reached through mergerfs at /mnt/user/<share> — where the real holder
// of the file descriptor is mergerfs itself, not the client (doc 09 §2's
// own worked example, and this issue's acceptance criterion for the
// lab); and that a real SIGKILL sent to a real, separate mover process
// mid-copy leaves a duplicate, never a gap, once resumed (doc 09 §6).

package cache

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

func labDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// labMoverTopology brings up a real cache-then-move share, nested inside
// a real catch-all (mirroring pool.Mount's own ordering requirement), and
// that share's own array-only mover write target — the same three mounts
// internal/pool's own lab test builds, reused here rather than
// reimplemented.
type labMoverTopology struct {
	mounter    pool.Mounter
	catchAll   pool.Mount
	shareMount pool.Mount
	moverMount pool.Mount
	cachePath  string
	dataDisks  []string
}

func bringUpLabMoverTopology(t *testing.T, name string) labMoverTopology {
	t.Helper()
	lab := labDir(t)
	ctx := context.Background()
	mounter := pool.Mounter{Runner: disk.CommandRunner{}}

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	cachePath := filepath.Join(lab, "mnt", "cache")
	for _, d := range dataDisks {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("data disk %s not present — expected create-array.sh to have mounted it: %v", d, err)
		}
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache disk %s not present: %v", cachePath, err)
	}

	share := pool.Share{Name: name, CacheMode: pool.CacheThenMove, CreatePolicy: pool.KeepFoldersTogether}
	opts := pool.Options{MinFreeSpace: "10M", Responsiveness: pool.Responsive}

	for _, d := range dataDisks {
		mustMkdirAll(t, filepath.Join(d, name))
	}
	mustMkdirAll(t, filepath.Join(cachePath, name))

	catchAllWhere := filepath.Join(lab, "mnt", "cache-test-user-"+name)
	mustMkdirAll(t, catchAllWhere)
	catchAll, err := pool.CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAll.Where = catchAllWhere

	shareMount, err := pool.ShareMount(share, dataDisks, cachePath, opts)
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	shareMount.Where = filepath.Join(catchAllWhere, name)

	arrayRootWhere := filepath.Join(lab, "mnt", "cache-test-array-"+name)
	mustMkdirAll(t, arrayRootWhere)
	moverMount, err := pool.MoverTargetMount(share, dataDisks, opts)
	if err != nil {
		t.Fatalf("MoverTargetMount: %v", err)
	}
	moverMount.Where = filepath.Join(arrayRootWhere, name)
	mustMkdirAll(t, moverMount.Where)

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), shareMount.Where)
		_ = mounter.Unmount(context.Background(), catchAll.Where)
		_ = mounter.Unmount(context.Background(), moverMount.Where)
	})

	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("mounting catch-all: %v", err)
	}
	if err := mounter.Mount(ctx, shareMount); err != nil {
		t.Fatalf("mounting share: %v", err)
	}
	if err := mounter.Mount(ctx, moverMount); err != nil {
		t.Fatalf("mounting mover target: %v", err)
	}

	return labMoverTopology{
		mounter:    mounter,
		catchAll:   catchAll,
		shareMount: shareMount,
		moverMount: moverMount,
		cachePath:  cachePath,
		dataDisks:  dataDisks,
	}
}

func (top labMoverTopology) cacheShare() Share {
	branches := make([]string, len(top.dataDisks))
	for i, d := range top.dataDisks {
		branches[i] = filepath.Join(d, top.shareName())
	}
	return Share{
		Name:      top.shareName(),
		CachePath: filepath.Join(top.cachePath, top.shareName()),
		ArrayPath: top.moverMount.Where,
		Branches:  branches,
	}
}

func (top labMoverTopology) shareName() string {
	return filepath.Base(top.shareMount.Where)
}

// TestLabMover_WritesThroughRealMergerfsCreatePolicy proves Run's copy
// step, writing through the real array-only mount, lets mergerfs place
// the file — it never appears on the cache disk, and it does appear on
// exactly one real data disk (doc 09 §2's "one placement algorithm").
func TestLabMover_WritesThroughRealMergerfsCreatePolicy(t *testing.T) {
	top := bringUpLabMoverTopology(t, "movercreatepolicy")
	share := top.cacheShare()

	old := time.Now().Add(-time.Hour)
	src := filepath.Join(share.CachePath, "episode.mkv")
	if err := os.WriteFile(src, []byte("real mergerfs bytes"), 0o640); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), []Share{share}, Config{}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected one moved file, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be gone: err=%v", err)
	}

	found := 0
	var landedOn string
	for _, d := range top.dataDisks {
		p := filepath.Join(d, share.Name, "episode.mkv")
		if _, err := os.Stat(p); err == nil {
			found++
			landedOn = p
		}
	}
	if found != 1 {
		t.Fatalf("expected the file on exactly one data disk, found it on %d (last: %s)", found, landedOn)
	}
	got, err := os.ReadFile(landedOn)
	if err != nil || string(got) != "real mergerfs bytes" {
		t.Fatalf("content on %s = %q, %v, want the source bytes", landedOn, got, err)
	}
}

// waitUntilOpen polls checker until it reports path open, bounded by
// timeout — starting a holder process does not itself mean it has
// gotten as far as open(2)-ing the file yet.
func waitUntilOpen(t *testing.T, checker OpenChecker, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		open, err := checker.IsOpen(context.Background(), path)
		if err != nil {
			t.Fatalf("IsOpen(%s): %v", path, err)
		}
		if open {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was not reported open within %s", path, timeout)
}

// TestLabMover_SkipsFileHeldOpen_DirectAndThroughUnion is this issue's
// acceptance-required lab test (#53): a real process holding a file open
// directly on the cache disk, and a real process holding a file open
// through /mnt/user/<share> — where the actual holder of the file
// descriptor is mergerfs, not the client process — are both detected,
// and both files are left alone until closed.
func TestLabMover_SkipsFileHeldOpen_DirectAndThroughUnion(t *testing.T) {
	top := bringUpLabMoverTopology(t, "moveropenfiles")
	share := top.cacheShare()
	old := time.Now().Add(-time.Hour)

	directPath := filepath.Join(share.CachePath, "direct.bin")
	if err := os.WriteFile(directPath, []byte("held open directly on cache"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(directPath, old, old); err != nil {
		t.Fatal(err)
	}

	unionPath := filepath.Join(top.shareMount.Where, "union.bin")
	if err := os.WriteFile(unionPath, []byte("held open through the union mount"), 0o640); err != nil {
		t.Fatal(err)
	}
	cacheUnionPath := filepath.Join(share.CachePath, "union.bin")
	if err := os.Chtimes(cacheUnionPath, old, old); err != nil {
		t.Fatal(err)
	}

	directHold := exec.Command("tail", "-f", directPath)
	if err := directHold.Start(); err != nil {
		t.Fatalf("starting tail -f on %s: %v", directPath, err)
	}
	t.Cleanup(func() { _ = directHold.Process.Kill() })

	unionHold := exec.Command("tail", "-f", unionPath)
	if err := unionHold.Start(); err != nil {
		t.Fatalf("starting tail -f on %s: %v", unionPath, err)
	}
	t.Cleanup(func() { _ = unionHold.Process.Kill() })

	checker := ProcOpenChecker{}
	waitUntilOpen(t, checker, directPath, 5*time.Second)
	// The client (tail) opened the file through the fuse mount; the real
	// holder of a file descriptor on the underlying cache path is
	// mergerfs itself, confirmed here against the cache-side path, not
	// the union path tail actually used.
	waitUntilOpen(t, checker, cacheUnionPath, 5*time.Second)

	report, err := Run(context.Background(), []Share{share}, Config{}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Moved()) != 0 {
		t.Fatalf("expected nothing moved while both files are held open, got %+v", report.Entries)
	}
	skipped := report.Skipped()
	if len(skipped) != 2 {
		t.Fatalf("expected both files skipped as open, got %+v", report.Entries)
	}
	for _, e := range skipped {
		if e.Result != ResultSkippedOpen {
			t.Errorf("entry %+v: want skipped_open", e)
		}
	}

	if err := directHold.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = directHold.Wait()
	if err := unionHold.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = unionHold.Wait()

	// mergerfs releases its own fd asynchronously with the client's
	// close; give it a moment before asserting the check clears.
	deadline := time.Now().Add(5 * time.Second)
	for {
		openDirect, _ := checker.IsOpen(context.Background(), directPath)
		openUnion, _ := checker.IsOpen(context.Background(), cacheUnionPath)
		if !openDirect && !openUnion {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("files still reported open 5s after their holders were killed (direct=%v, union=%v)", openDirect, openUnion)
		}
		time.Sleep(20 * time.Millisecond)
	}

	report, err = Run(context.Background(), []Share{share}, Config{}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("Run (after close): %v", err)
	}
	if len(report.Moved()) != 2 {
		t.Fatalf("expected both files to move once closed, got %+v", report.Entries)
	}
	if _, err := os.Stat(directPath); !os.IsNotExist(err) {
		t.Fatalf("direct source should be gone: err=%v", err)
	}
	if _, err := os.Stat(cacheUnionPath); !os.IsNotExist(err) {
		t.Fatalf("union source should be gone: err=%v", err)
	}
}

// TestLabMover_SIGKILLMidCopyLeavesNoGap is doc 09 §6's own "Mover
// interrupted (SIGKILL) mid-copy leaves a duplicate, never a gap" lab
// test. This test's own binary re-executes itself as a second, genuinely
// separate OS process running TestLabMover_SIGKILLHelper below — the real
// mover, writing through this file's own real mergerfs/loop-device
// topology — and sends that process a real SIGKILL the moment its
// temp-suffixed partial copy appears on the array side, which is always
// well before the rename a real kill needs to land ahead of. The killed
// process leaves the cache-side source untouched and the stray temp file
// behind; a fresh Run back in this test's own process must then resume
// cleanly, leaving exactly one correct copy on the array and no gap.
func TestLabMover_SIGKILLMidCopyLeavesNoGap(t *testing.T) {
	top := bringUpLabMoverTopology(t, "moversigkill")
	share := top.cacheShare()

	old := time.Now().Add(-time.Hour)
	src := filepath.Join(share.CachePath, "big.bin")
	// Large enough that the copy loop is still running well after the
	// temp file is created, giving the kill below a comfortable margin.
	payload := make([]byte, 128<<20)
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := os.WriteFile(src, payload, 0o640); err != nil {
		t.Fatalf("writing source: %v", err)
	}
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestLabMover_SIGKILLHelper$", "-test.v")
	helper.Env = append(os.Environ(),
		"HOSERVA_LAB_MOVER_HELPER=1",
		"HOSERVA_LAB_MOVER_HELPER_SHARE_NAME="+share.Name,
		"HOSERVA_LAB_MOVER_HELPER_CACHE_PATH="+share.CachePath,
		"HOSERVA_LAB_MOVER_HELPER_ARRAY_PATH="+share.ArrayPath,
	)
	var stderr bytes.Buffer
	helper.Stderr = &stderr
	if err := helper.Start(); err != nil {
		t.Fatalf("starting the real mover subprocess: %v", err)
	}

	tmpGlob := filepath.Join(share.ArrayPath, "big.bin"+tempSuffix+"*")
	deadline := time.Now().Add(10 * time.Second)
	for {
		matches, _ := filepath.Glob(tmpGlob)
		if len(matches) > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = helper.Process.Kill()
			_ = helper.Wait()
			t.Fatalf("the mover subprocess never created its temp copy: stderr=%s", stderr.String())
		}
		time.Sleep(2 * time.Millisecond)
	}

	if err := helper.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL the mover subprocess: %v", err)
	}
	_ = helper.Wait()

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a SIGKILL mid-copy: %v", err)
	}
	if matches, _ := filepath.Glob(tmpGlob); len(matches) == 0 {
		t.Fatal("expected the killed subprocess to have left a stray temp copy behind")
	}

	report, err := Run(context.Background(), []Share{share}, Config{}, Deps{}, RunHooks{}, nil)
	if err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if len(report.Moved()) != 1 {
		t.Fatalf("expected exactly one moved entry on resume, got %+v", report.Entries)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source should be removed after the resumed run completes: err=%v", err)
	}
	dst := filepath.Join(share.ArrayPath, "big.bin")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading the final file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("final file content does not match the source: got %d bytes, want %d", len(got), len(payload))
	}
	if matches, _ := filepath.Glob(tmpGlob); len(matches) != 0 {
		t.Fatalf("the stray temp copy should have been swept by the resumed run, found %v", matches)
	}
}

// TestLabMover_SIGKILLHelper only does anything when
// TestLabMover_SIGKILLMidCopyLeavesNoGap re-executes this same binary
// with HOSERVA_LAB_MOVER_HELPER=1, naming exactly this test via
// -test.run: that re-exec is what makes the SIGKILL above land on a
// genuine, separate OS process actually running the mover, rather than a
// goroutine or fabricated on-disk state in the calling process.
func TestLabMover_SIGKILLHelper(t *testing.T) {
	if os.Getenv("HOSERVA_LAB_MOVER_HELPER") != "1" {
		t.Skip("only runs as TestLabMover_SIGKILLMidCopyLeavesNoGap's own subprocess")
	}
	share := Share{
		Name:      os.Getenv("HOSERVA_LAB_MOVER_HELPER_SHARE_NAME"),
		CachePath: os.Getenv("HOSERVA_LAB_MOVER_HELPER_CACHE_PATH"),
		ArrayPath: os.Getenv("HOSERVA_LAB_MOVER_HELPER_ARRAY_PATH"),
	}
	_, _ = Run(context.Background(), []Share{share}, Config{}, Deps{}, RunHooks{}, nil)
}
