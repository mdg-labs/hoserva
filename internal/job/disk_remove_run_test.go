package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	hoservastore "github.com/mdg-labs/hoserva/internal/store"
)

const diskRemoveTestShare = "media"

// recordingParity is a parity.FakeEngine that also records every Sync
// call's options and whether it succeeded, and whose inner fake can be
// swapped between runs — FakeEngine's guard block stays scripted until a
// fresh one replaces it.
type recordingParity struct {
	mu    sync.Mutex
	inner *parity.FakeEngine
	syncs []parity.SyncOpts
	ok    int
	// onSync, when set, runs at the start of every Sync call.
	onSync func()
}

func (r *recordingParity) engine() *parity.FakeEngine {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner
}

func (r *recordingParity) Sync(ctx context.Context, opts parity.SyncOpts) (<-chan parity.Progress, error) {
	r.mu.Lock()
	r.syncs = append(r.syncs, opts)
	hook := r.onSync
	inner := r.inner
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	ch, err := inner.Sync(ctx, opts)
	if err != nil {
		return nil, err
	}
	out := make(chan parity.Progress, 1)
	go func() {
		defer close(out)
		var failed bool
		for p := range ch {
			if p.Err != nil {
				failed = true
			}
			out <- p
		}
		if !failed {
			r.mu.Lock()
			r.ok++
			r.mu.Unlock()
		}
	}()
	return out, nil
}

func (r *recordingParity) Diff(ctx context.Context) (parity.DiffReport, error) {
	return r.engine().Diff(ctx)
}
func (r *recordingParity) Scrub(ctx context.Context, pct, olderThanDays int) (<-chan parity.Progress, error) {
	return r.engine().Scrub(ctx, pct, olderThanDays)
}
func (r *recordingParity) Status(ctx context.Context) (parity.ParityStatus, error) {
	return r.engine().Status(ctx)
}
func (r *recordingParity) Fix(ctx context.Context, opts parity.FixOpts) (<-chan parity.Progress, error) {
	return r.engine().Fix(ctx, opts)
}
func (r *recordingParity) Check(ctx context.Context, opts parity.CheckOpts) (<-chan parity.Progress, error) {
	return r.engine().Check(ctx, opts)
}
func (r *recordingParity) List(ctx context.Context) (parity.ListReport, error) {
	return r.engine().List(ctx)
}

func (r *recordingParity) counts() (calls, succeeded int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.syncs), r.ok
}

// tableUnmounter stops a disk's mount unit by popping it off a
// FakeMountTable, the way systemctl stop takes a real mount away.
type tableUnmounter struct {
	table *FakeMountTable
	mu    sync.Mutex
	err   error
	calls []string
	// onUnmount, when set, runs at the start of every Unmount call.
	onUnmount func()
}

func (u *tableUnmounter) Mount(context.Context, disk.MountUnit) error {
	return errors.New("tableUnmounter: Mount is not used by disk_remove")
}

func (u *tableUnmounter) Unmount(ctx context.Context, unit disk.MountUnit) error {
	u.mu.Lock()
	u.calls = append(u.calls, unit.Where)
	err := u.err
	hook := u.onUnmount
	u.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	return u.table.UnmountOnce(ctx, unit.Where)
}

// diskRemoveHarness is one scheduler over a real SQLite array (a parity
// disk and three data disks, no cache), a real Generator whose
// snapraid.conf and unit files were generated from it, a fake mount
// table with every disk mounted, and a fake parity engine whose diff
// reports disk2 empty.
type diskRemoveHarness struct {
	s         *Scheduler
	arrays    *hoservastore.ArrayStore
	gen       *config.Generator
	genRoot   string
	mounts    *FakeMountTable
	unmounter *tableUnmounter
	parity    *recordingParity
	ready     *countingHook
	disks     []string
	parity1   string
	uuids     map[string]string
}

func newDiskRemoveHarness(t *testing.T, dataDisks int) *diskRemoveHarness {
	t.Helper()
	return newDiskRemoveHarnessWithCache(t, dataDisks, false)
}

// newDiskRemoveHarnessWithCache adds a cache disk when withCache is set:
// with one parity disk, Q18 needs three content-file copies, and a
// single data disk only reaches three with the boot device and a cache.
func newDiskRemoveHarnessWithCache(t *testing.T, dataDisks int, withCache bool) *diskRemoveHarness {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)
	base := t.TempDir()
	h := &diskRemoveHarness{
		arrays:  hoservastore.NewArrayStore(db),
		genRoot: t.TempDir(),
		mounts:  NewFakeMountTable(),
		ready:   &countingHook{},
		parity1: filepath.Join(base, "parity1"),
		uuids:   map[string]string{},
	}
	h.gen = config.NewGenerator(h.genRoot)
	h.unmounter = &tableUnmounter{table: h.mounts}
	rows := []hoservastore.ArrayDisk{{Role: hoservastore.ArrayRoleParity, RoleIndex: 1, Device: "/dev/sda", Filesystem: "xfs", FSUUID: "uuid-p1", Mountpoint: h.parity1}}
	h.mounts.Preload(h.parity1, "uuid-p1")
	if withCache {
		cachePath := filepath.Join(base, "cache")
		rows = append(rows, hoservastore.ArrayDisk{Role: hoservastore.ArrayRoleCache, RoleIndex: 1, Device: "/dev/sdz", Filesystem: "xfs", FSUUID: "uuid-c", Mountpoint: cachePath})
		h.mounts.Preload(cachePath, "uuid-c")
	}
	for i := 1; i <= dataDisks; i++ {
		mp := filepath.Join(base, fmt.Sprintf("disk%d", i))
		uuid := fmt.Sprintf("uuid-d%d", i)
		if err := os.MkdirAll(filepath.Join(mp, diskRemoveTestShare), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		h.disks = append(h.disks, mp)
		h.uuids[mp] = uuid
		h.mounts.Preload(mp, uuid)
		rows = append(rows, hoservastore.ArrayDisk{Role: hoservastore.ArrayRoleData, RoleIndex: i, Device: fmt.Sprintf("/dev/sd%c", 'b'+i-1), Filesystem: "xfs", FSUUID: uuid, Serial: fmt.Sprintf("SERIAL-%d", i), Mountpoint: mp})
	}
	if err := h.arrays.PutArray(ctx, hoservastore.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "1M", CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}, rows); err != nil {
		t.Fatalf("PutArray: %v", err)
	}
	if err := regenerateArrayFromStore(ctx, h.arrays, h.gen, time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("generating the array's files: %v", err)
	}
	fake := parity.NewFakeEngine()
	fake.Sleep = func(time.Duration) {}
	h.parity = &recordingParity{inner: fake}
	if dataDisks >= 2 {
		h.setTrackedFiles(0, 0)
	}
	h.s = NewScheduler(NewStore(db), NewLogStore(t.TempDir()), NewHub(), NewRegistry())
	h.s.registry.Register(TypeDiskRemove, false, RunDiskRemove(h.deps()))
	return h
}

func (h *diskRemoveHarness) deps() DiskRemoveDeps {
	return DiskRemoveDeps{
		Store:      h.arrays,
		Generator:  h.gen,
		Mounts:     h.mounts,
		Unmounter:  h.unmounter,
		Parity:     h.parity,
		ShareNames: func(context.Context) ([]string, error) { return []string{diskRemoveTestShare}, nil },
		ArrayReady: h.ready.hook,
		Now:        func() time.Time { return time.Date(2026, 9, 25, 1, 0, 0, 0, time.UTC) },
	}
}

// setTrackedFiles scripts the diff SnapRAID reports for disk2 after the
// step-8 sync: files it still tracks there, and files it now sees.
func (h *diskRemoveHarness) setTrackedFiles(before, after int) {
	per := map[string]parity.DiskDiff{}
	for _, d := range h.disks {
		per[filepath.Clean(d)] = parity.DiskDiff{FilesBefore: 10, FilesAfter: 10}
	}
	per[filepath.Clean(h.disks[1])] = parity.DiskDiff{FilesBefore: before, FilesAfter: after}
	h.parity.engine().SetDiff(parity.DiffReport{PerDisk: per})
}

func (h *diskRemoveHarness) setState(t *testing.T, mountpoint, state string) {
	t.Helper()
	ctx := context.Background()
	switch state {
	case hoservastore.RemovalStateEvacuating, hoservastore.RemovalStateEvacuated:
		if err := h.arrays.SetRemovalState(ctx, mountpoint, state, "evac-1"); err != nil {
			t.Fatalf("SetRemovalState(%s): %v", state, err)
		}
	case hoservastore.RemovalStateUnpooled:
		h.setState(t, mountpoint, hoservastore.RemovalStateEvacuated)
		if err := h.arrays.AdvanceRemovalState(ctx, mountpoint, hoservastore.RemovalStateEvacuated, state, "rm-0"); err != nil {
			t.Fatalf("AdvanceRemovalState(%s): %v", state, err)
		}
	case hoservastore.RemovalStateUnlisted:
		h.setState(t, mountpoint, hoservastore.RemovalStateUnpooled)
		if err := h.arrays.AdvanceRemovalState(ctx, mountpoint, hoservastore.RemovalStateUnpooled, state, "rm-0"); err != nil {
			t.Fatalf("AdvanceRemovalState(%s): %v", state, err)
		}
	}
}

func (h *diskRemoveHarness) row(t *testing.T, mountpoint string) (hoservastore.ArrayDisk, bool) {
	t.Helper()
	_, disks, err := h.arrays.GetArray(context.Background())
	if err != nil {
		t.Fatalf("GetArray: %v", err)
	}
	for _, d := range disks {
		if d.Mountpoint == mountpoint {
			return d, true
		}
	}
	return hoservastore.ArrayDisk{}, false
}

func (h *diskRemoveHarness) snapraidConf(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(h.genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("reading snapraid.conf: %v", err)
	}
	return string(body)
}

func (h *diskRemoveHarness) unitPath(mountpoint string) string {
	return filepath.Join(h.genRoot, "systemd/system", disk.UnitFileName(mountpoint))
}

// generated returns every file under the generator root, by path.
func (h *diskRemoveHarness) generated(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(h.genRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", h.genRoot, err)
	}
	return files
}

func (h *diskRemoveHarness) run(t *testing.T, mountpoint, confirmation string) *Job {
	t.Helper()
	j, err := h.s.Submit(context.Background(), TypeDiskRemove, nil, mustJSON(t, DiskRemoveParams{Mountpoint: mountpoint, Confirmation: confirmation}))
	if err != nil {
		t.Fatalf("Submit(disk_remove): %v", err)
	}
	return await(t, h.s, j.ID)
}

func (h *diskRemoveHarness) output(t *testing.T, id string) string {
	t.Helper()
	rc, err := h.s.logs.Open(id)
	if err != nil {
		t.Fatalf("opening job %s's log: %v", id, err)
	}
	defer func() { _ = rc.Close() }()
	gz, err := gzip.NewReader(rc)
	if err != nil {
		t.Fatalf("decompressing job %s's log: %v", id, err)
	}
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("reading job %s's log: %v", id, err)
	}
	return string(b)
}

// TestRunDiskRemove_RefusesBeforeAnyChange is the acceptance criterion's
// precondition list: each refusal leaves the store, every generated
// file, the mounts and parity exactly as they were — no removal state
// advanced, no live update, no sync, no unmount.
func TestRunDiskRemove_RefusesBeforeAnyChange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dataDisks int
		withCache bool
		// target picks the mountpoint, from the harness's data disks.
		target func(h *diskRemoveHarness) string
		setup  func(t *testing.T, h *diskRemoveHarness, target string)
		// confirmation, when set, replaces the correct phrase.
		confirmation string
		want         string
	}{
		{
			name: "not in removal", dataDisks: 3,
			setup: func(*testing.T, *diskRemoveHarness, string) {},
			want:  "has not been evacuated",
		},
		{
			name: "still evacuating", dataDisks: 3,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuating)
			},
			want: "has not been evacuated",
		},
		{
			name: "no data disk at the slot", dataDisks: 3,
			target: func(h *diskRemoveHarness) string { return h.parity1 },
			setup:  func(*testing.T, *diskRemoveHarness, string) {},
			want:   hoservastore.ErrArrayDiskNotFound.Error(),
		},
		{
			name: "a file is still on the disk", dataDisks: 3,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuated)
				if err := os.WriteFile(filepath.Join(target, diskRemoveTestShare, "late.bin"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "still has content",
		},
		{
			name: "a file outside every share", dataDisks: 3,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuated)
				if err := os.MkdirAll(filepath.Join(target, "not-a-share"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "not-a-share", "notes.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "not-a-share/notes.txt",
		},
		{
			name: "another filesystem is mounted at the slot", dataDisks: 3,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateUnpooled)
				h.mounts.ReportUUID[target] = "uuid-someone-else"
			},
			want: "uuid-someone-else",
		},
		{
			name: "the last data disk", dataDisks: 1, withCache: true,
			target: func(h *diskRemoveHarness) string { return h.disks[0] },
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuated)
			},
			want: parity.ErrNoDataDisks.Error(),
		},
		{
			name: "too few devices left for the content files (Q18)", dataDisks: 2,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuated)
			},
			want: parity.ErrContentPlacement.Error(),
		},
		{
			name: "wrong confirmation", dataDisks: 3,
			setup: func(t *testing.T, h *diskRemoveHarness, target string) {
				h.setState(t, target, hoservastore.RemovalStateEvacuated)
			},
			confirmation: "REMOVE /mnt/somewhere-else",
			want:         disk.ErrConfirmationMismatch.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDiskRemoveHarnessWithCache(t, tc.dataDisks, tc.withCache)
			target := h.disks[len(h.disks)-1]
			if len(h.disks) >= 2 {
				target = h.disks[1]
			}
			if tc.target != nil {
				target = tc.target(h)
			}
			tc.setup(t, h, target)
			beforeRow, _ := h.row(t, target)
			beforeFiles := h.generated(t)
			beforeOps := h.mounts.Ops()
			beforeMounted := h.mounts.MountedPaths()

			confirmation := EvacuationConfirmation(target)
			if tc.confirmation != "" {
				confirmation = tc.confirmation
			}
			j := h.run(t, target, confirmation)
			if j.Status != StatusFailed {
				t.Fatalf("status = %s, want failed", j.Status)
			}
			if !strings.Contains(j.ErrorMessage, tc.want) {
				t.Fatalf("ErrorMessage = %q, want it to mention %q", j.ErrorMessage, tc.want)
			}

			afterRow, _ := h.row(t, target)
			if afterRow != beforeRow {
				t.Fatalf("row changed: %+v -> %+v", beforeRow, afterRow)
			}
			if after := h.generated(t); fmt.Sprint(after) != fmt.Sprint(beforeFiles) {
				t.Fatalf("generated files changed on a refusal")
			}
			if calls := h.ready.calls.Load(); calls != 0 {
				t.Fatalf("ArrayReady ran %d times on a refusal", calls)
			}
			if calls, _ := h.parity.counts(); calls != 0 {
				t.Fatalf("Sync ran %d times on a refusal", calls)
			}
			if len(h.unmounter.calls) != 0 || len(h.mounts.Ops()) != len(beforeOps) {
				t.Fatalf("mount table touched on a refusal: unmounts %v, ops %v", h.unmounter.calls, h.mounts.Ops())
			}
			afterMounted := h.mounts.MountedPaths()
			sort.Strings(beforeMounted)
			sort.Strings(afterMounted)
			if fmt.Sprint(afterMounted) != fmt.Sprint(beforeMounted) {
				t.Fatalf("mounted paths changed on a refusal: %v -> %v", beforeMounted, afterMounted)
			}
		})
	}
}

// TestRunDiskRemove_UnmountedAfterEvacuation_FinishesFromDiffAlone is
// #369's recovery path: a data disk whose removal state is "evacuated" or
// "unpooled" and which is no longer mounted — it failed after being
// proven empty, before finishDiskRemoval ever ran or ever got to finish —
// still finishes, using only a fresh SnapRAID diff to confirm it is
// empty. A leftover file that every mounted-only check (postCheck,
// diskLeftover, removeEmptyDirs) would refuse on is left in place to
// prove none of them ran: the job succeeds anyway, because the disk is
// unmounted and they are skipped, not passed.
func TestRunDiskRemove_UnmountedAfterEvacuation_FinishesFromDiffAlone(t *testing.T) {
	for _, state := range []string{hoservastore.RemovalStateEvacuated, hoservastore.RemovalStateUnpooled} {
		t.Run(state, func(t *testing.T) {
			h := newDiskRemoveHarness(t, 3)
			disk2 := h.disks[1]
			h.setState(t, disk2, state)
			if err := os.WriteFile(filepath.Join(disk2, diskRemoveTestShare, "leftover.bin"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := h.mounts.UnmountOnce(context.Background(), disk2); err != nil {
				t.Fatal(err)
			}

			j := h.run(t, disk2, EvacuationConfirmation(disk2))
			if j.Status != StatusSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", j.Status, j.ErrorMessage)
			}
			if strings.Contains(h.snapraidConf(t), disk2) {
				t.Fatal("snapraid.conf still names disk2")
			}
			if _, present := h.row(t, disk2); present {
				t.Fatal("disk2's row survived")
			}
			if len(h.unmounter.calls) != 0 {
				t.Fatalf("unmounted an already unmounted disk: %v", h.unmounter.calls)
			}
			if calls, ok := h.parity.counts(); calls != 1 || ok != 1 {
				t.Fatalf("sync calls = %d, succeeded = %d, want exactly one successful sync", calls, ok)
			}
		})
	}
}

// TestRunDiskRemove_UnmountedWithDirtyDiff_StillRefused proves the
// recovery path never advances an unmounted disk past "unpooled" on
// anything but a clean, fresh SnapRAID diff: unmounted alone is not
// proof of emptiness, exactly as it is not for a mounted disk. It also
// proves that diff is read before step 8's own sync ever runs: a sync
// that ran first would rewrite what "before" means (a real
// --force-empty sync always makes its own next diff read 0/0, #369), so
// Sync must never be called at all once the pre-sync diff is already
// dirty.
func TestRunDiskRemove_UnmountedWithDirtyDiff_StillRefused(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	h.setTrackedFiles(3, 3)
	if err := h.mounts.UnmountOnce(context.Background(), disk2); err != nil {
		t.Fatal(err)
	}
	confBefore := h.snapraidConf(t)

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, "still records") {
		t.Fatalf("status = %s (%s), want failed on SnapRAID still recording files", j.Status, j.ErrorMessage)
	}
	if row, _ := h.row(t, disk2); row.RemovalState != hoservastore.RemovalStateUnpooled {
		t.Fatalf("removal state = %q, want unpooled", row.RemovalState)
	}
	if h.snapraidConf(t) != confBefore {
		t.Fatal("snapraid.conf dropped disk2 while SnapRAID still tracks files on it")
	}
	if len(h.unmounter.calls) != 0 {
		t.Fatal("disk2 was unmounted")
	}
	if calls, _ := h.parity.counts(); calls != 0 {
		t.Fatalf("Sync ran %d times before the pre-sync diff refused — the dirty diff must be caught before syncing, never laundered by it", calls)
	}
}

// TestRunDiskRemove_UnmountedDisk_RemovesStrayContentFile proves the
// second half of #369: while disk2 was still listed in snapraid.conf,
// its mountpoint named one of SnapRAID's own content-file copies (Q18),
// and a real step-8 sync writes one there like any other data disk's —
// onto what is really just an ordinary directory on the boot filesystem
// once the disk itself is gone. A finish must not leave that file
// behind once snapraid.conf no longer names it there.
func TestRunDiskRemove_UnmountedDisk_RemovesStrayContentFile(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	if err := h.mounts.UnmountOnce(context.Background(), disk2); err != nil {
		t.Fatal(err)
	}
	strayPath := filepath.Join(disk2, "snapraid.content")
	if err := os.WriteFile(strayPath, []byte("stray"), 0o644); err != nil {
		t.Fatal(err)
	}

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", j.Status, j.ErrorMessage)
	}
	if _, err := os.Stat(strayPath); !os.IsNotExist(err) {
		t.Fatalf("stray content file at %s survived the finish, err=%v", strayPath, err)
	}
}

// TestRunDiskRemove_StepsRunInOrder is the ordering criterion: unpooled
// is persisted and applied live before anything touches parity; the
// guarded sync runs with only this disk exempt while snapraid.conf still
// lists it; only then is it unlisted and snapraid.conf regenerated
// without it (the surviving disk keeping its own "d3"); then the unit is
// stopped, its unit file removed and the row deleted, and the daemon's
// view rebuilt last.
func TestRunDiskRemove_StepsRunInOrder(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	if _, err := os.Stat(h.unitPath(disk2)); err != nil {
		t.Fatalf("disk2's unit file was never generated: %v", err)
	}
	// What an evacuation leaves: the share's own directory tree, empty,
	// plus SnapRAID's content file and the filesystem's lost+found.
	for _, dir := range []string{filepath.Join(disk2, diskRemoveTestShare, "tv", "show"), filepath.Join(disk2, "lost+found")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(disk2, "snapraid.content"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}

	var events []string
	var mu sync.Mutex
	record := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	h.ready.onCall = func(ctx context.Context, n int32) error {
		row, present := h.row(t, disk2)
		record(fmt.Sprintf("ready(state=%s present=%v)", row.RemovalState, present))
		return nil
	}
	h.parity.onSync = func() {
		row, _ := h.row(t, disk2)
		listed := strings.Contains(h.snapraidConf(t), "data d2 "+disk2+"/")
		mounted, _ := h.mounts.IsMounted(context.Background(), disk2)
		_, shareErr := os.Stat(filepath.Join(disk2, diskRemoveTestShare))
		record(fmt.Sprintf("sync(state=%s listed=%v mounted=%v shareDirGone=%v)", row.RemovalState, listed, mounted, os.IsNotExist(shareErr)))
	}

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", j.Status, j.ErrorMessage)
	}

	want := []string{
		"ready(state=unpooled present=true)",
		"sync(state=unpooled listed=true mounted=true shareDirGone=true)",
		"ready(state= present=false)",
	}
	if fmt.Sprint(events) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}

	h.parity.mu.Lock()
	opts := h.parity.syncs[0]
	h.parity.mu.Unlock()
	if opts.Confirm {
		t.Fatal("the step-8 sync was confirmed past the guard")
	}
	if len(opts.RemovingDisks) != 1 || !opts.RemovingDisks[filepath.Clean(disk2)] {
		t.Fatalf("RemovingDisks = %v, want exactly {%s}", opts.RemovingDisks, disk2)
	}

	conf := h.snapraidConf(t)
	if strings.Contains(conf, disk2) {
		t.Fatalf("snapraid.conf still names disk2:\n%s", conf)
	}
	for _, want := range []string{"data d1 " + h.disks[0] + "/\n", "data d3 " + h.disks[2] + "/\n"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("snapraid.conf lacks %q:\n%s", want, conf)
		}
	}
	if mounted, _ := h.mounts.IsMounted(context.Background(), disk2); mounted {
		t.Fatal("disk2 is still mounted")
	}
	if fmt.Sprint(h.unmounter.calls) != fmt.Sprint([]string{disk2}) {
		t.Fatalf("unmounts = %v, want only disk2", h.unmounter.calls)
	}
	if _, err := os.Stat(h.unitPath(disk2)); !os.IsNotExist(err) {
		t.Fatalf("disk2's unit file is still there (%v) — the next boot would mount it again", err)
	}
	if status, err := h.gen.Check(context.Background(), "systemd/system/"+disk.UnitFileName(disk2)); err != nil || status != config.StatusUnknown {
		t.Fatalf("disk2's unit manifest record = (%v, %v), want gone", status, err)
	}
	for _, other := range []string{h.disks[0], h.disks[2], h.parity1} {
		if _, err := os.Stat(h.unitPath(other)); err != nil {
			t.Fatalf("%s's unit file was removed: %v", other, err)
		}
	}
	if _, present := h.row(t, disk2); present {
		t.Fatal("disk2's row is still in the array")
	}
	for _, kept := range []string{filepath.Join(disk2, "lost+found"), filepath.Join(disk2, "snapraid.content")} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was removed: %v", kept, err)
		}
	}
	out := h.output(t, j.ID)
	if !strings.Contains(out, "safe to physically remove") || !strings.Contains(out, "SERIAL-2") || !strings.Contains(out, "/dev/sdc") {
		t.Fatalf("job output does not name the disk as safe to remove:\n%s", out)
	}
}

// TestRunDiskRemove_FailedLiveUpdate_StopsInUnpooled is step 7's own
// criterion: when the running pool does not take the disk out of its
// branches, the job fails in unpooled and parity is never touched.
func TestRunDiskRemove_FailedLiveUpdate_StopsInUnpooled(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	confBefore := h.snapraidConf(t)
	h.ready.onCall = func(context.Context, int32) error {
		return errors.New("applying the new pool mounts to the running pool: mergerfs refused")
	}

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, "mergerfs refused") {
		t.Fatalf("status = %s (%s), want failed with the live-update error", j.Status, j.ErrorMessage)
	}
	if row, _ := h.row(t, disk2); row.RemovalState != hoservastore.RemovalStateUnpooled {
		t.Fatalf("removal state = %q, want unpooled", row.RemovalState)
	}
	if calls, _ := h.parity.counts(); calls != 0 {
		t.Fatalf("Sync ran %d times after a failed live update", calls)
	}
	if h.snapraidConf(t) != confBefore {
		t.Fatal("snapraid.conf changed after a failed live update")
	}
	if mounted, _ := h.mounts.IsMounted(context.Background(), disk2); !mounted {
		t.Fatal("disk2 was unmounted after a failed live update")
	}
}

// TestRunDiskRemove_GuardBlock_StopsInUnpooled_RerunSyncsOnce is the
// guard criterion against the fake engine: a tripped guard fails the job
// with disk2 still listed and mounted, nothing synced; once the pending
// change is reviewed, a re-run completes with exactly one successful
// step-8 sync across both runs.
func TestRunDiskRemove_GuardBlock_StopsInUnpooled_RerunSyncsOnce(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	h.parity.engine().ScriptGuardBlock(trippedGuard())

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusFailed {
		t.Fatalf("status = %s, want failed on the tripped guard", j.Status)
	}
	if !strings.Contains(j.ErrorMessage, "threshold guard blocked the sync") {
		t.Fatalf("ErrorMessage = %q, want the guard's own block", j.ErrorMessage)
	}
	if row, _ := h.row(t, disk2); row.RemovalState != hoservastore.RemovalStateUnpooled {
		t.Fatalf("removal state = %q, want unpooled", row.RemovalState)
	}
	if !strings.Contains(h.snapraidConf(t), "data d2 "+disk2+"/") {
		t.Fatal("disk2 left snapraid.conf past a tripped guard")
	}
	if mounted, _ := h.mounts.IsMounted(context.Background(), disk2); !mounted {
		t.Fatal("disk2 was unmounted past a tripped guard")
	}
	if _, ok := h.parity.counts(); ok != 0 {
		t.Fatalf("%d syncs succeeded past a tripped guard", ok)
	}

	fresh := parity.NewFakeEngine()
	fresh.Sleep = func(time.Duration) {}
	h.parity.mu.Lock()
	h.parity.inner = fresh
	h.parity.mu.Unlock()
	h.setTrackedFiles(0, 0)

	again := h.run(t, disk2, EvacuationConfirmation(disk2))
	if again.Status != StatusSucceeded {
		t.Fatalf("re-run status = %s (%s), want succeeded", again.Status, again.ErrorMessage)
	}
	if _, ok := h.parity.counts(); ok != 1 {
		t.Fatalf("%d successful step-8 syncs across both runs, want exactly 1", ok)
	}
	if _, present := h.row(t, disk2); present {
		t.Fatal("disk2's row survived the re-run")
	}
}

// TestRunDiskRemove_SnapraidStillTracksFiles_StopsBeforeUnlisting proves
// the data line is never dropped while SnapRAID itself still records
// files on the disk after the step-8 sync — files outside every share
// root that the post-check cannot see.
func TestRunDiskRemove_SnapraidStillTracksFiles_StopsBeforeUnlisting(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after int
		omit          bool
	}{
		{name: "tracked files", before: 3, after: 3},
		{name: "a new file since the sync", before: 0, after: 1},
		{name: "the disk is missing from the diff", omit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDiskRemoveHarness(t, 3)
			disk2 := h.disks[1]
			h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
			h.setTrackedFiles(tc.before, tc.after)
			if tc.omit {
				h.parity.engine().SetDiff(parity.DiffReport{PerDisk: map[string]parity.DiskDiff{filepath.Clean(h.disks[0]): {FilesBefore: 1, FilesAfter: 1}}})
			}
			confBefore := h.snapraidConf(t)

			j := h.run(t, disk2, EvacuationConfirmation(disk2))
			if j.Status != StatusFailed {
				t.Fatalf("status = %s, want failed", j.Status)
			}
			if row, _ := h.row(t, disk2); row.RemovalState != hoservastore.RemovalStateUnpooled {
				t.Fatalf("removal state = %q, want unpooled", row.RemovalState)
			}
			if h.snapraidConf(t) != confBefore {
				t.Fatal("snapraid.conf dropped disk2 while SnapRAID still tracks files on it")
			}
			if len(h.unmounter.calls) != 0 {
				t.Fatal("disk2 was unmounted")
			}
		})
	}
}

// TestRunDiskRemove_RerunAfterTheSync_NeverSyncsAgain covers a job
// stopped once the step-8 sync had been recorded (unlisted): before
// snapraid.conf was regenerated, and after step 9's unmount. Each re-run
// finishes without another sync, and one that finds the disk already
// unmounted does not unmount again.
func TestRunDiskRemove_RerunAfterTheSync_NeverSyncsAgain(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unmounted bool
	}{
		{name: "stopped before regenerating snapraid.conf"},
		{name: "stopped after the unmount", unmounted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDiskRemoveHarness(t, 3)
			disk2 := h.disks[1]
			h.setState(t, disk2, hoservastore.RemovalStateUnlisted)
			if tc.unmounted {
				if err := h.mounts.UnmountOnce(context.Background(), disk2); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(h.snapraidConf(t), disk2) {
				t.Fatal("setup: snapraid.conf should still list disk2 (the regeneration never ran)")
			}

			j := h.run(t, disk2, EvacuationConfirmation(disk2))
			if j.Status != StatusSucceeded {
				t.Fatalf("status = %s (%s), want succeeded", j.Status, j.ErrorMessage)
			}
			if calls, _ := h.parity.counts(); calls != 0 {
				t.Fatalf("the re-run synced %d times — the step-8 sync already ran", calls)
			}
			if strings.Contains(h.snapraidConf(t), disk2) {
				t.Fatal("snapraid.conf still names disk2")
			}
			if tc.unmounted && len(h.unmounter.calls) != 0 {
				t.Fatalf("unmounted an already unmounted disk: %v", h.unmounter.calls)
			}
			if _, present := h.row(t, disk2); present {
				t.Fatal("disk2's row survived")
			}
		})
	}
}

// TestRunDiskRemove_Step9Failures_StopInUnlisted covers step 9 failing
// after the sync: a busy unmount, and a unit file someone edited by
// hand. Each leaves the row "unlisted" for a re-run — never deleted
// while its unit could still mount it at the next boot.
func TestRunDiskRemove_Step9Failures_StopInUnlisted(t *testing.T) {
	t.Run("unmount fails", func(t *testing.T) {
		h := newDiskRemoveHarness(t, 3)
		disk2 := h.disks[1]
		h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
		h.unmounter.err = errors.New("target is busy")

		j := h.run(t, disk2, EvacuationConfirmation(disk2))
		if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, "target is busy") {
			t.Fatalf("status = %s (%s), want failed on the busy unmount", j.Status, j.ErrorMessage)
		}
		if row, present := h.row(t, disk2); !present || row.RemovalState != hoservastore.RemovalStateUnlisted {
			t.Fatalf("row = %+v (present %v), want unlisted", row, present)
		}
		if _, err := os.Stat(h.unitPath(disk2)); err != nil {
			t.Fatalf("unit file removed while the disk is still mounted: %v", err)
		}

		h.unmounter.mu.Lock()
		h.unmounter.err = nil
		h.unmounter.mu.Unlock()
		again := h.run(t, disk2, EvacuationConfirmation(disk2))
		if again.Status != StatusSucceeded {
			t.Fatalf("re-run status = %s (%s), want succeeded", again.Status, again.ErrorMessage)
		}
		if _, ok := h.parity.counts(); ok != 1 {
			t.Fatalf("%d successful syncs across both runs, want 1", ok)
		}
	})
	t.Run("unit file edited by hand", func(t *testing.T) {
		h := newDiskRemoveHarness(t, 3)
		disk2 := h.disks[1]
		h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
		h.unmounter.onUnmount = func() {
			if err := os.WriteFile(h.unitPath(disk2), []byte("# edited by hand\n"), 0o644); err != nil {
				t.Error(err)
			}
		}

		j := h.run(t, disk2, EvacuationConfirmation(disk2))
		if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, "changed by hand") {
			t.Fatalf("status = %s (%s), want failed on the hand-edited unit", j.Status, j.ErrorMessage)
		}
		if body, err := os.ReadFile(h.unitPath(disk2)); err != nil || !bytes.Equal(body, []byte("# edited by hand\n")) {
			t.Fatalf("hand-edited unit = (%q, %v), want it left alone", body, err)
		}
		if row, present := h.row(t, disk2); !present || row.RemovalState != hoservastore.RemovalStateUnlisted {
			t.Fatalf("row = %+v (present %v), want unlisted", row, present)
		}
	})
}

// TestRunDiskRemove_SomethingLeftAfterTheSync_StopsBeforeUnlisting proves
// the data line stays while anything SnapRAID would still record — here
// a directory created on the disk behind the job's back — is on it: the
// job stops "unpooled", and a re-run removes it and finishes.
func TestRunDiskRemove_SomethingLeftAfterTheSync_StopsBeforeUnlisting(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	stray := filepath.Join(disk2, "created-meanwhile")
	var once sync.Once
	h.parity.onSync = func() {
		once.Do(func() {
			if err := os.Mkdir(stray, 0o755); err != nil {
				t.Error(err)
			}
		})
	}
	confBefore := h.snapraidConf(t)

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, stray) {
		t.Fatalf("status = %s (%s), want failed naming %s", j.Status, j.ErrorMessage, stray)
	}
	if row, _ := h.row(t, disk2); row.RemovalState != hoservastore.RemovalStateUnpooled {
		t.Fatalf("removal state = %q, want unpooled", row.RemovalState)
	}
	if h.snapraidConf(t) != confBefore {
		t.Fatal("snapraid.conf dropped disk2 while a directory was left on it")
	}

	again := h.run(t, disk2, EvacuationConfirmation(disk2))
	if again.Status != StatusSucceeded {
		t.Fatalf("re-run = %s (%s), want succeeded", again.Status, again.ErrorMessage)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("%s survived the re-run: %v", stray, err)
	}
}

// TestRunDiskRemove_SnapraidRefusesTheNewConfig_FailsLoud proves the job
// never reports a removal as done when SnapRAID will not run against the
// regenerated snapraid.conf: it fails in "unlisted", disk still mounted
// and in the array.
func TestRunDiskRemove_SnapraidRefusesTheNewConfig_FailsLoud(t *testing.T) {
	h := newDiskRemoveHarness(t, 3)
	disk2 := h.disks[1]
	h.setState(t, disk2, hoservastore.RemovalStateEvacuated)
	h.parity.engine().FailStatus(errors.New("Disk 'd2' not present in the configuration file"))

	j := h.run(t, disk2, EvacuationConfirmation(disk2))
	if j.Status != StatusFailed || !strings.Contains(j.ErrorMessage, "does not accept the configuration") {
		t.Fatalf("status = %s (%s), want failed on SnapRAID's refusal", j.Status, j.ErrorMessage)
	}
	if row, present := h.row(t, disk2); !present || row.RemovalState != hoservastore.RemovalStateUnlisted {
		t.Fatalf("row = %+v (present %v), want unlisted", row, present)
	}
	if len(h.unmounter.calls) != 0 {
		t.Fatal("disk2 was unmounted although SnapRAID refuses the new configuration")
	}
}
