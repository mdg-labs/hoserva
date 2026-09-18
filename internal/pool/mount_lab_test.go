//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern internal/disk's own format_lab_test.go uses. It proves
// this package's Mount/Mounter bring up the real doc 02 §1 topology
// against real loop devices and a real mergerfs: a catch-all, a
// cache-then-move share nested inside it, and that share's own mover
// write target — survive an unmount/remount cycle, enforce the
// catch-all-before-share ordering the kernel itself provides (S6, doc
// 08 §6), and route a stray top-level write to a data disk rather than
// leaving it on the pre-mount directory that stands in for the boot
// device.
//
// Every mount point here lives under $LAB (doc 06 §3's own convention),
// never at the production /mnt/user or /run/hoserva/array paths this
// package's constructors return — so each Mount's Where is rebuilt
// under $LAB after the constructor computes its branches, options and
// create policy exactly as production would.

package pool

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
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

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(got)
}

// TestLabPoolTopology_MountsInOrderAndSurvivesRemount is this issue's
// central lab acceptance test (issue #27): a real catch-all plus a real
// cache-then-move share plus that share's own mover write target, all
// driven by this package's own Mount/Mounter, against the standing
// lab's own data and cache disks (created by create-array.sh).
func TestLabPoolTopology_MountsInOrderAndSurvivesRemount(t *testing.T) {
	lab := labDir(t)
	ctx := context.Background()
	mounter := Mounter{Runner: disk.CommandRunner{}}

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

	const shareName = "labshare"
	share := Share{Name: shareName, CacheMode: CacheThenMove, CreatePolicy: KeepFoldersTogether}
	// 50M, not doc 02 §1's own 50G production default — the same
	// convention S6 and S8 used (doc 08 §6, §8) to fit the lab's
	// 1 GiB loop-device disks; at 50G every branch here would be
	// filtered out and every write would fail ENOSPC.
	opts := Options{MinFreeSpace: "50M", Responsiveness: Responsive}

	// The share's own directory must exist on every branch *before* the
	// pools mount over them — creating it afterwards, through a pool,
	// risks landing only on the branch that pool's own mkdir policy
	// picks, and creating it inside an already-mounted catch-all's own
	// directory would place it out of reach of a separately-mounted
	// array-only pool over the same branches.
	for _, d := range dataDisks {
		mustMkdirAll(t, filepath.Join(d, shareName))
	}
	mustMkdirAll(t, filepath.Join(cachePath, shareName))

	catchAllWhere := filepath.Join(lab, "mnt", "pool-test-user")
	mustMkdirAll(t, catchAllWhere)
	catchAll, err := CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAll.Where = catchAllWhere

	shareMount, err := ShareMount(share, dataDisks, cachePath, opts)
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	shareMount.Where = filepath.Join(catchAllWhere, shareName)

	arrayRootWhere := filepath.Join(lab, "mnt", "pool-test-array")
	mustMkdirAll(t, arrayRootWhere)
	moverMount, err := MoverTargetMount(share, dataDisks, opts)
	if err != nil {
		t.Fatalf("MoverTargetMount: %v", err)
	}
	moverMount.Where = filepath.Join(arrayRootWhere, shareName)
	mustMkdirAll(t, moverMount.Where)

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), shareMount.Where)
		_ = mounter.Unmount(context.Background(), catchAll.Where)
		_ = mounter.Unmount(context.Background(), moverMount.Where)
	})

	// --- Ordering: the share mount only appears once the catch-all is
	// up, and the catch-all refuses to unmount while the share is
	// nested inside it (S6, doc 08 §6). ---

	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("mounting catch-all: %v", err)
	}
	if _, err := os.Stat(shareMount.Where); err != nil {
		t.Fatalf("share directory %s not visible through the mounted catch-all: %v", shareMount.Where, err)
	}

	if err := mounter.Mount(ctx, shareMount); err != nil {
		t.Fatalf("mounting share: %v", err)
	}
	if err := mounter.Mount(ctx, moverMount); err != nil {
		t.Fatalf("mounting mover target: %v", err)
	}

	if err := mounter.Unmount(ctx, catchAll.Where); err == nil {
		t.Fatal("unmounting the catch-all while the share is still nested inside it: got nil error, want busy")
	}

	// --- A write through the cache-then-move share lands on cache,
	// never on an array branch. ---
	mustWriteFile(t, filepath.Join(shareMount.Where, "hello.txt"), "hello from the lab")
	if got := mustReadFile(t, filepath.Join(cachePath, shareName, "hello.txt")); got != "hello from the lab" {
		t.Fatalf("cache branch content = %q, want the file written through the share", got)
	}
	for _, d := range dataDisks {
		if _, err := os.Stat(filepath.Join(d, shareName, "hello.txt")); err == nil {
			t.Fatalf("hello.txt landed on data branch %s — a cache-then-move write must land on cache", d)
		}
	}

	// --- A write through the mover's own array-only target lands on a
	// data disk, with the share's own create policy placing it (doc 09
	// §2). ---
	mustWriteFile(t, filepath.Join(moverMount.Where, "moved.txt"), "moved by the mover")
	foundOnArray := false
	for _, d := range dataDisks {
		if _, err := os.Stat(filepath.Join(d, shareName, "moved.txt")); err == nil {
			foundOnArray = true
		}
	}
	if !foundOnArray {
		t.Fatal("moved.txt did not land on any data disk through the mover target")
	}

	// --- A stray top-level write (no per-share mount) lands on a data
	// disk through the catch-all's own default policy, never left
	// stranded on the pre-mount directory that stands in for the boot
	// device. ---
	mustWriteFile(t, filepath.Join(catchAll.Where, "stray.txt"), "stray top-level write")
	strayOnArray := false
	for _, d := range dataDisks {
		if _, err := os.Stat(filepath.Join(d, "stray.txt")); err == nil {
			strayOnArray = true
		}
	}
	if !strayOnArray {
		t.Fatal("stray.txt did not land on any data disk through the catch-all")
	}

	// --- Remount cycle: unmount in dependency order, remount, and the
	// earlier cache write is still there, byte-identical. ---
	if err := mounter.Unmount(ctx, moverMount.Where); err != nil {
		t.Fatalf("unmounting mover target: %v", err)
	}
	if err := mounter.Unmount(ctx, shareMount.Where); err != nil {
		t.Fatalf("unmounting share: %v", err)
	}
	if err := mounter.Unmount(ctx, catchAll.Where); err != nil {
		t.Fatalf("unmounting catch-all after its share was unmounted: %v", err)
	}

	if err := mounter.Mount(ctx, catchAll); err != nil {
		t.Fatalf("remounting catch-all: %v", err)
	}
	if err := mounter.Mount(ctx, shareMount); err != nil {
		t.Fatalf("remounting share: %v", err)
	}
	if got := mustReadFile(t, filepath.Join(shareMount.Where, "hello.txt")); got != "hello from the lab" {
		t.Fatalf("after remount, hello.txt = %q, want the byte-identical original", got)
	}
}
