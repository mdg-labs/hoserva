//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host — see internal/pool/mount_lab_test.go's own header for the
// build/run mechanics this file follows exactly.
//
// It proves ArraySequence's Stop/Start drive real mergerfs mounts in doc
// 02 §4's own order — share mount before catch-all on the way down,
// catch-all before share mount on the way up — using internal/pool's
// MountController against the standing lab's own data and cache disks
// (created by create-array.sh). Physical disk unmount/remount and every
// service (VM, container, Samba, NFS) stop/start step are not exercised
// here: they need, respectively, systemd unit activation and a real VM
// or Docker runtime, neither available inside this capability-narrowed
// container (no init system, doc 08 §6) — both are L3 work, tracked as
// blocked in this issue's own report rather than simulated here.

package job

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

func arrayLabDir(t *testing.T) string {
	t.Helper()
	id := os.Getenv("HOSERVA_LAB_ID")
	if id == "" {
		t.Skip("HOSERVA_LAB_ID not set — this test only runs inside its own lab container")
	}
	return filepath.Join("/lab", id)
}

func arrayMustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func arrayMustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func arrayMustReadFile(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(got)
}

func TestLabArraySequence_StopThenStartRealMounts(t *testing.T) {
	lab := arrayLabDir(t)
	ctx := context.Background()
	mounter := pool.Mounter{Runner: disk.CommandRunner{}}

	dataDisks := []string{
		filepath.Join(lab, "mnt", "disk1"),
		filepath.Join(lab, "mnt", "disk2"),
		filepath.Join(lab, "mnt", "disk3"),
	}
	for _, d := range dataDisks {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("data disk %s not present — expected create-array.sh to have mounted it: %v", d, err)
		}
	}

	const shareName = "arrayseq"
	share := pool.Share{Name: shareName, CacheMode: pool.ArrayOnly, CreatePolicy: pool.KeepFoldersTogether}
	opts := pool.Options{MinFreeSpace: "50M", Responsiveness: pool.Responsive}

	for _, d := range dataDisks {
		arrayMustMkdirAll(t, filepath.Join(d, shareName))
	}

	catchAllWhere := filepath.Join(lab, "mnt", "array-seq-user")
	arrayMustMkdirAll(t, catchAllWhere)
	catchAll, err := pool.CatchAllMount(dataDisks, opts)
	if err != nil {
		t.Fatalf("CatchAllMount: %v", err)
	}
	catchAll.Where = catchAllWhere

	shareMount, err := pool.ShareMount(share, dataDisks, "", opts)
	if err != nil {
		t.Fatalf("ShareMount: %v", err)
	}
	shareMount.Where = filepath.Join(catchAllWhere, shareName)

	catchAllCtl := pool.MountController{Mnt: catchAll, Mounter: mounter}
	shareCtl := pool.MountController{Mnt: shareMount, Mounter: mounter}

	t.Cleanup(func() {
		_ = mounter.Unmount(context.Background(), shareMount.Where)
		_ = mounter.Unmount(context.Background(), catchAll.Where)
	})

	seq := ArraySequence{
		ShareMounts: []ArrayMount{shareCtl},
		CatchAll:    catchAllCtl,
	}

	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	arrayMustWriteFile(t, filepath.Join(shareMount.Where, "hello.txt"), "hello from the array sequence")

	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Once Stop has run, the catch-all's own mountpoint is the plain,
	// pre-mount directory again (doc 02 §1's exposed-when-unmounted
	// shape) — empty, since nothing was ever placed there directly; the
	// share path nested inside it (only ever reachable through the now-
	// torn-down mergerfs stack) is gone with it. This is exactly what
	// makes Q69's immutable-mountpoint mechanism meaningful: an unmounted
	// mountpoint is a real, plain directory a stray write could land in.
	if _, err := os.Stat(shareMount.Where); err == nil {
		t.Fatalf("after Stop, %s still resolves — want it gone along with the torn-down mergerfs stack", shareMount.Where)
	}
	entries, err := os.ReadDir(catchAllWhere)
	if err != nil {
		t.Fatalf("reading %s after Stop: %v", catchAllWhere, err)
	}
	if len(entries) != 0 {
		t.Fatalf("after Stop, %s contains %v, want empty — confirming the mergerfs mount was really torn down, not just hidden", catchAllWhere, entries)
	}

	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start (remount): %v", err)
	}
	if got := arrayMustReadFile(t, filepath.Join(shareMount.Where, "hello.txt")); got != "hello from the array sequence" {
		t.Fatalf("after remount, hello.txt = %q, want the byte-identical original", got)
	}
}
