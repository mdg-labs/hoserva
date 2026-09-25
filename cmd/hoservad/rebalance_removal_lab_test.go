//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never
// on the host: it is built with `go test -tags lab -c` from the host
// (compiling touches no device) and the resulting binary is run with
// `docker compose exec -T lab <binary>` inside the lab container, the
// same pattern evacuation_removal_state_lab_test.go uses in this package.
//
// It covers #366: a rebalance must never move a file onto a data disk
// that is leaving the array. The daemon side is p359StartDaemon's, so the
// rebalance job and Handler.RebalanceShares are the ones
// parityRegistrar.register wires in production (rebalanceSharesFromStore,
// shareRelocationSyncFunc, rebalanceTrackedFileCount), and the plan comes
// from Handler.PlanRebalance/StartRebalance. The disk's removal state is
// written through the same store.ArrayStore calls the evacuation and
// disk_remove jobs make. The snapraid engine lists only the disks still
// in snapraid.conf, as it would once the disk is "unlisted".

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/cache"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

const p366Share = "p366share"

// p366WriteFile writes size bytes derived from name to path and returns
// their SHA-256.
func p366WriteFile(t *testing.T, path string, size int) [32]byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	seed := []byte(filepath.Base(path))
	data := bytes.Repeat(seed, size/len(seed)+1)[:size]
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return sha256.Sum256(data)
}

// p366FilesUnder returns every regular file under root, relative to it.
func p366FilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return filepath.SkipAll
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return out
}

func p366UsedPercent(t *testing.T, mountpoint string) float64 {
	t.Helper()
	u, err := cache.UsageBytes(mountpoint)
	if err != nil {
		t.Fatalf("usage of %s: %v", mountpoint, err)
	}
	return u.UsedPercent()
}

// TestLabRebalance_NeverMovesOntoAnUnlistedDisk is #366's data-loss
// scenario. disk2 was left "unlisted" by a disk_remove job that failed
// at its last step: it is out of every pool mount and out of
// snapraid.conf, but still an array row, mounted, and the least full disk.
// disk3 is the most full. A rebalance started through the API must plan
// and run moves only between disk1 and disk3. Without the exclusion the
// plan moves files from disk3 onto disk2 and the job deletes their
// sources, so they leave both the pool and parity protection.
func TestLabRebalance_NeverMovesOntoAnUnlistedDisk(t *testing.T) {
	ctx := context.Background()
	lab := shareRelocLabDir(t)
	exec := disk.CommandRunner{}
	var disks []string
	for i := 1; i <= 3; i++ {
		label := fmt.Sprintf("p366-disk%d", i)
		mountpoint := filepath.Join(lab, label)
		p359CreateLoopDisk(t, exec, lab, label, 300, mountpoint)
		disks = append(disks, mountpoint)
	}
	disk1, disk2, disk3 := disks[0], disks[1], disks[2]

	engine := shareRelocLabEngine(t, lab, "p366-work", "p366", []string{disk1, disk3},
		parity.GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100})
	d := p359StartDaemon(t, t.TempDir(), t.TempDir(), engine)

	var rows []store.ArrayDisk
	for i, m := range disks {
		label := filepath.Base(m)
		rows = append(rows, store.ArrayDisk{
			Role: store.ArrayRoleData, RoleIndex: i + 1, Mountpoint: m,
			Device: "/dev/" + label, Filesystem: "xfs", FSUUID: label,
		})
	}
	now := time.Now().UTC()
	if err := d.arrays.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: now}, rows); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := d.shares.Insert(ctx, store.Share{Name: p366Share, CacheMode: "array-only", CreatePolicy: "mfs", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("inserting share: %v", err)
	}

	// disk1 about a quarter full, disk3 about half, disk2 empty. Small
	// files outside the share give the guard's percent rule enough
	// tracked files for a batch of several moves.
	const size = 12 << 20
	want := map[string][32]byte{}
	for i := 0; i < 6; i++ {
		rel := fmt.Sprintf("d1-%02d.bin", i)
		want[rel] = p366WriteFile(t, filepath.Join(disk1, p366Share, rel), size)
	}
	for i := 0; i < 12; i++ {
		rel := fmt.Sprintf("d3-%02d.bin", i)
		want[rel] = p366WriteFile(t, filepath.Join(disk3, p366Share, rel), size)
	}
	for i := 0; i < 100; i++ {
		p366WriteFile(t, filepath.Join(disk1, "filler", fmt.Sprintf("f%03d.bin", i)), 1024)
	}
	shareRelocLabSyncOnce(t, ctx, engine)

	if !(p366UsedPercent(t, disk2) < p366UsedPercent(t, disk1) && p366UsedPercent(t, disk1) < p366UsedPercent(t, disk3)) {
		t.Fatalf("used %% disk1=%.1f disk2=%.1f disk3=%.1f: the scenario needs disk2 least full and disk3 most full",
			p366UsedPercent(t, disk1), p366UsedPercent(t, disk2), p366UsedPercent(t, disk3))
	}

	if err := d.arrays.SetRemovalState(ctx, disk2, store.RemovalStateEvacuated, "p366-evacuation"); err != nil {
		t.Fatalf("marking disk2 evacuated: %v", err)
	}
	if err := d.arrays.AdvanceRemovalState(ctx, disk2, store.RemovalStateEvacuated, store.RemovalStateUnpooled, "p366-remove"); err != nil {
		t.Fatalf("marking disk2 unpooled: %v", err)
	}
	if err := d.arrays.AdvanceRemovalState(ctx, disk2, store.RemovalStateUnpooled, store.RemovalStateUnlisted, "p366-remove"); err != nil {
		t.Fatalf("marking disk2 unlisted: %v", err)
	}

	plan, err := d.handler.PlanRebalance(ctx)
	if err != nil {
		t.Fatalf("PlanRebalance: %v", err)
	}
	if len(plan.Moves) == 0 {
		t.Fatal("PlanRebalance planned no move; the scenario needs disk3 to be rebalanced")
	}
	for _, m := range plan.Moves {
		if filepath.Dir(m.TargetBranch) == disk2 || filepath.Dir(m.SourceBranch) == disk2 {
			t.Fatalf("PlanRebalance move %s: %s -> %s involves disk2, which is unlisted", m.RelPath, m.SourceBranch, m.TargetBranch)
		}
	}

	j, err := d.handler.StartRebalance(ctx, &apiv1.StartRebalanceRequest{Confirmation: job.RebalanceConfirmation()})
	if err != nil {
		t.Fatalf("StartRebalance: %v", err)
	}
	finished, err := d.scheduler.Await(ctx, j.ID.String())
	if err != nil {
		t.Fatalf("Await: %v", err)
	}
	if finished.Status != job.StatusSucceeded {
		t.Fatalf("rebalance status = %s (%s), want succeeded", finished.Status, finished.ErrorMessage)
	}

	if got := p366FilesUnder(t, disk2); len(got) != 0 {
		t.Fatalf("disk2 (unlisted) holds %v after the rebalance, want nothing", got)
	}
	found := map[string]int{}
	var onDisk1FromDisk3 int
	for _, m := range []string{disk1, disk3} {
		for _, rel := range p366FilesUnder(t, filepath.Join(m, p366Share)) {
			data, err := os.ReadFile(filepath.Join(m, p366Share, rel))
			if err != nil {
				t.Fatalf("reading %s: %v", rel, err)
			}
			if sum, ok := want[rel]; !ok || sha256.Sum256(data) != sum {
				t.Fatalf("%s on %s is not a file this test wrote, or its content changed", rel, m)
			}
			found[rel]++
			if m == disk1 && strings.HasPrefix(rel, "d3-") {
				onDisk1FromDisk3++
			}
		}
	}
	for rel := range want {
		if found[rel] != 1 {
			t.Fatalf("%s is on %d of disk1/disk3, want exactly 1", rel, found[rel])
		}
	}
	if onDisk1FromDisk3 == 0 {
		t.Fatal("no file moved from disk3 to disk1; the rebalance did not run between the disks staying in the array")
	}
}
