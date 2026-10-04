package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	impUUID1   = "11111111-1111-4111-8111-111111111111"
	impUUID2   = "22222222-2222-4222-8222-222222222222"
	impCatchAt = "/mnt/user"
)

// importMounter is the disk unit mounter of an import test: it keeps what is
// mounted and can fail the mount, or the unmount, of one mountpoint.
type importMounter struct {
	mu         sync.Mutex
	mounted    map[string]bool
	mounts     []disk.MountUnit
	unmounts   []string
	failMount  string
	failUnmont string
	// mountedErr makes every question whether a path is mounted fail.
	mountedErr error
}

func (m *importMounter) Mount(_ context.Context, u disk.MountUnit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mounts = append(m.mounts, u)
	if u.Where == m.failMount {
		return errors.New("injected: the mount failed")
	}
	m.mounted[u.Where] = true
	return nil
}

func (m *importMounter) Unmount(_ context.Context, u disk.MountUnit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unmounts = append(m.unmounts, u.Where)
	if u.Where == m.failUnmont {
		return errors.New("injected: the unmount failed")
	}
	delete(m.mounted, u.Where)
	return nil
}

func (m *importMounter) isMounted(where string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mountedErr != nil {
		return false, m.mountedErr
	}
	return m.mounted[where], nil
}

// importPool is the catch-all mount of the rebuilt array sequence.
type importPool struct {
	m         *importMounter
	failMount bool
}

func (p importPool) Where() string { return impCatchAt }
func (p importPool) Mount(ctx context.Context) error {
	if p.failMount {
		return errors.New("injected: the pool did not mount")
	}
	return p.m.Mount(ctx, disk.MountUnit{Where: impCatchAt})
}
func (p importPool) Unmount(ctx context.Context) error {
	return p.m.Unmount(ctx, disk.MountUnit{Where: impCatchAt})
}

type importHarness struct {
	t       *testing.T
	s       *Scheduler
	st      *store.ArrayStore
	genRoot string
	runner  *disk.FakeRunner
	mounter *importMounter
	plan    disk.AdoptionPlan
	// freshPlan is what the plan hook resolves: plan unless a test changes it.
	freshPlan disk.AdoptionPlan
	poolFails bool
	readyErr  error
	ready     int
	seq       *ArraySequence
	// seeded counts the seed hook's calls; seedErr is its failure; mountedAtSeed
	// is what was mounted when it ran.
	seeded        int
	seedErr       error
	mountedAtSeed []string
	noSeed        bool
	dir           string
	links         []string
}

// newImportHarness builds the import's dependencies over fakes: a plan of two
// data disks, a parity disk whose filesystem UUID is the first data disk's (the
// one-data-disk case, where it carries a copy of it) and a cache, every
// data disk reached through a by-id link that exists in a temporary directory.
func newImportHarness(t *testing.T) *importHarness {
	t.Helper()
	h := &importHarness{t: t, dir: t.TempDir(), genRoot: t.TempDir(), runner: disk.NewFakeRunner(), mounter: &importMounter{mounted: map[string]bool{}}}
	h.s = newTestScheduler(t)
	h.st = store.NewArrayStore(newTestDB(t))

	link := func(name string) string {
		dev := filepath.Join(h.dir, name+"1")
		if err := os.WriteFile(dev, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		l := filepath.Join(h.dir, "by-id-"+name+"-part1")
		if err := os.Symlink(dev, l); err != nil {
			t.Fatal(err)
		}
		h.links = append(h.links, l)
		return l
	}
	l1, l2 := link("DAT1"), link("DAT2")
	h.plan = disk.AdoptionPlan{
		Data: []disk.AdoptedDisk{
			{AssignedDisk: disk.AssignedDisk{Device: "/dev/sdc", Filesystem: disk.XFS, Adopt: true, Serial: "DAT1", ByIDName: "ata-EX_DAT1", FSUUID: impUUID1}, Size: 4 << 40, FSDevice: "/dev/sdc1", MountSource: l1},
			{AssignedDisk: disk.AssignedDisk{Device: "/dev/sdd", Filesystem: disk.EXT4, Adopt: true, Serial: "DAT2", ByIDName: "ata-EX_DAT2", FSUUID: impUUID2}, Size: 2 << 40, FSDevice: "/dev/sdd1", MountSource: l2},
		},
		Parity: []disk.RecordedDisk{
			{AssignedDisk: disk.AssignedDisk{Device: "/dev/sdb", Filesystem: disk.XFS, Serial: "PAR1", ByIDName: "ata-EX_PAR1", FSUUID: impUUID1}, Size: 8 << 40},
		},
		Cache: &disk.RecordedDisk{AssignedDisk: disk.AssignedDisk{Device: "/dev/nvme0n1", Filesystem: disk.XFS, Serial: "CAC1", ByIDName: "nvme-EX_CAC1"}, Size: 500 << 30},
	}
	h.freshPlan = h.plan

	for i, u := range h.plan.MountUnits() {
		uuid := h.plan.Data[i].FSUUID
		h.runner.Script("findmnt", []string{"-n", "-o", "UUID", u.Where}, []byte(uuid+"\n"), nil)
		h.runner.Script("findmnt", []string{"-n", "-o", "SOURCE", u.Where}, []byte(filepath.Join(h.dir, []string{"DAT1", "DAT2"}[i]+"1")+"\n"), nil)
		h.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", u.Where}, []byte("ro,nosuid,nodev,noexec,noatime\n"), nil)
	}
	h.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", impCatchAt}, []byte("ro,nosuid,nodev,relatime\n"), nil)
	return h
}

func (h *importHarness) register() {
	h.t.Helper()
	gen := config.NewGenerator(h.genRoot)
	h.s.registry.Register(TypeMigrationImport, false, RunMigrationImport(MigrationImportDeps{
		Plan: func(context.Context, []disk.AdoptionAssignment) (disk.AdoptionPlan, error) {
			return h.freshPlan, nil
		},
		Runner:    h.runner,
		Store:     h.st,
		Generator: gen,
		Mounter:   h.mounter,
		ArrayReady: func(ctx context.Context) error {
			h.ready++
			if h.readyErr != nil {
				return h.readyErr
			}
			_, _, err := h.st.GetArray(ctx)
			if errors.Is(err, store.ErrNoArray) {
				h.seq = nil
				return nil
			}
			h.seq = &ArraySequence{CatchAll: importPool{m: h.mounter, failMount: h.poolFails}}
			return nil
		},
		Array:     func() *ArraySequence { return h.seq },
		Seed:      h.seedHook(),
		IsMounted: h.mounter.isMounted,
		Now:       func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) },
	}))
}

func (h *importHarness) seedHook() func(context.Context, io.Writer) error {
	if h.noSeed {
		return nil
	}
	return func(_ context.Context, out io.Writer) error {
		h.seeded++
		h.mounter.mu.Lock()
		h.mountedAtSeed = h.mountedAtSeed[:0]
		for w, up := range h.mounter.mounted {
			if up {
				h.mountedAtSeed = append(h.mountedAtSeed, w)
			}
		}
		h.mounter.mu.Unlock()
		_, _ = fmt.Fprintln(out, "seeded")
		return h.seedErr
	}
}

func (h *importHarness) params() []byte {
	h.t.Helper()
	body, err := json.Marshal(MigrationImportParams{
		Assignments: []disk.AdoptionAssignment{
			{Role: disk.AdoptParity, Serial: "PAR1"}, {Role: disk.AdoptData, Serial: "DAT1"},
			{Role: disk.AdoptData, Serial: "DAT2"}, {Role: disk.AdoptCache, Serial: "CAC1"},
		},
		Plan: h.plan,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return body
}

func (h *importHarness) run() *Job {
	h.t.Helper()
	h.register()
	ctx := context.Background()
	j, err := h.s.Submit(ctx, TypeMigrationImport, []string{"migration"}, h.params())
	if err != nil {
		h.t.Fatalf("Submit: %v", err)
	}
	done, err := h.s.Await(ctx, j.ID)
	if err != nil {
		h.t.Fatalf("Await: %v", err)
	}
	return done
}

func (h *importHarness) xfsChecks() []string {
	var out []string
	for _, c := range h.runner.Calls() {
		if strings.HasSuffix(c.Name, "_repair") || c.Name == "e2fsck" || c.Name == "btrfs" {
			out = append(out, c.Name+" "+strings.Join(c.Args, " "))
		}
	}
	return out
}

// unitFiles are the generated mount units under the generator's root.
func (h *importHarness) unitFiles() []string {
	var out []string
	_ = filepath.WalkDir(h.genRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mount") {
			out = append(out, filepath.Base(p))
		}
		return nil
	})
	return out
}

func (h *importHarness) assertNothingAdopted(when string) {
	h.t.Helper()
	ctx := context.Background()
	if exists, err := h.st.Exists(ctx); err != nil || exists {
		h.t.Errorf("%s: array record exists = %v, %v", when, exists, err)
	}
	if rec, err := h.st.RecordedDisks(ctx); err != nil || len(rec) != 0 {
		h.t.Errorf("%s: recorded disks = %+v, %v", when, rec, err)
	}
	if len(h.mounter.mounted) != 0 {
		h.t.Errorf("%s: still mounted: %v", when, h.mounter.mounted)
	}
	if units := h.unitFiles(); len(units) != 0 {
		h.t.Errorf("%s: generated units left: %v", when, units)
	}
	h.assertNoSnapraidConf(when)
}

func (h *importHarness) assertNoSnapraidConf(when string) {
	h.t.Helper()
	_ = filepath.WalkDir(h.genRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "snapraid") {
			h.t.Errorf("%s: %s was generated: no snapraid.conf may exist while a migration is pending", when, p)
		}
		return nil
	})
}

func TestMigrationImport_AdoptsReadOnlyAndRecordsParityAndCache(t *testing.T) {
	h := newImportHarness(t)
	done := h.run()
	if done.Status != StatusSucceeded {
		t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	settings, disks, err := h.st.GetArray(ctx)
	if err != nil || !settings.MigrationPending || len(disks) != 2 {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	if disks[0].Mountpoint != "/mnt/disk1" || disks[0].FSUUID != impUUID1 || disks[0].MountSource != h.plan.Data[0].MountSource || disks[1].Mountpoint != "/mnt/disk2" {
		t.Errorf("disks = %+v", disks)
	}
	recorded, err := h.st.RecordedDisks(ctx)
	if err != nil || len(recorded) != 2 || recorded[0].Role != store.ArrayRoleParity || recorded[0].Serial != "PAR1" || recorded[1].Role != store.ArrayRoleCache {
		t.Fatalf("recorded = %+v, %v", recorded, err)
	}

	// Only the data disks were checked and mounted, and the parity disk, which
	// shares the first data disk's UUID, appears in no command.
	if got := h.xfsChecks(); len(got) != 2 || !strings.HasPrefix(got[0], "xfs_repair -n "+h.plan.Data[0].MountSource) || !strings.HasPrefix(got[1], "e2fsck -n "+h.plan.Data[1].MountSource) {
		t.Errorf("filesystem checks = %v, want one read-only check per data disk, through its by-id link", got)
	}
	for _, c := range h.runner.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), "sdb") || strings.Contains(strings.Join(c.Args, " "), "nvme") || strings.HasPrefix(c.Name, "mkfs") {
			t.Errorf("the import ran %s %v", c.Name, c.Args)
		}
	}
	if len(h.mounter.mounts) != 3 || h.mounter.mounts[2].Where != impCatchAt {
		t.Fatalf("mounts = %+v, want the two data disks and then the pool", h.mounter.mounts)
	}
	for _, u := range h.mounter.mounts[:2] {
		if !u.ReadOnly || u.What == "" {
			t.Errorf("unit %+v was not a read-only mount of the disk's own device", u)
		}
	}
	if len(h.mounter.unmounts) != 0 {
		t.Errorf("unmounts = %v after a successful import", h.mounter.unmounts)
	}

	// Units are read-only; the parity and cache disks have none; no snapraid.conf.
	unit1, err := os.ReadFile(filepath.Join(h.genRoot, "systemd", "system", "mnt-disk1.mount"))
	if err != nil || !strings.Contains(string(unit1), "Options=ro,norecovery,") || !strings.Contains(string(unit1), "What="+h.plan.Data[0].MountSource) {
		t.Errorf("mnt-disk1.mount = %s, %v", unit1, err)
	}
	unit2, _ := os.ReadFile(filepath.Join(h.genRoot, "systemd", "system", "mnt-disk2.mount"))
	if !strings.Contains(string(unit2), "Options=ro,noload,") {
		t.Errorf("mnt-disk2.mount = %s", unit2)
	}
	catchAll, err := os.ReadFile(filepath.Join(h.genRoot, "systemd", "system", "mnt-user.mount"))
	if err != nil || !strings.Contains(string(catchAll), "What=/mnt/disk1=RO:/mnt/disk2=RO") || !strings.Contains(string(catchAll), ",ro\n") {
		t.Errorf("mnt-user.mount = %s, %v", catchAll, err)
	}
	if units := h.unitFiles(); len(units) != 3 {
		t.Errorf("units = %v, want the two data disks and the pool", units)
	}
	h.assertNoSnapraidConf("after the import")
	if pending, err := h.st.MigrationPending(ctx); err != nil || !pending {
		t.Errorf("MigrationPending = %v, %v", pending, err)
	}
}

// Before anything is written the job refuses a disk that is not the one the
// confirmed mapping named, and a data disk whose read-only check fails.
func TestMigrationImport_RefusesBeforeWritingAnything(t *testing.T) {
	t.Run("a disk that changed since the request", func(t *testing.T) {
		h := newImportHarness(t)
		h.freshPlan.Data = append([]disk.AdoptedDisk(nil), h.plan.Data...)
		h.freshPlan.Data[1].FSUUID = "99999999-9999-4999-8999-999999999999"
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "not the one the confirmed mapping named") {
			t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
		}
		if got := h.xfsChecks(); len(got) != 0 {
			t.Errorf("checks ran on a plan that did not match: %v", got)
		}
		if len(h.mounter.mounts) != 0 {
			t.Errorf("mounted %v", h.mounter.mounts)
		}
		h.assertNothingAdopted("after the refusal")
	})
	t.Run("a data disk that fails its read-only check", func(t *testing.T) {
		h := newImportHarness(t)
		h.runner.Script("e2fsck", []string{"-n", h.plan.Data[1].MountSource}, nil, errors.New("exit status 4"))
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "refusing to adopt") {
			t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
		}
		if len(h.mounter.mounts) != 0 {
			t.Errorf("mounted %v although a disk failed its check", h.mounter.mounts)
		}
		h.assertNothingAdopted("after the refusal")
	})
}

// A failure after the array is recorded undoes it: what was mounted is
// unmounted first, the record deleted, the units removed.
func TestMigrationImport_AFailureAfterTheRecordIsUndone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*importHarness)
		want  string
	}{
		{"the second disk does not mount", func(h *importHarness) { h.mounter.failMount = "/mnt/disk2" }, "injected: the mount failed"},
		{"a disk comes up read-write", func(h *importHarness) {
			h.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", "/mnt/disk2"}, []byte("rw,relatime\n"), nil)
		}, "mounted read-write"},
		{"a disk is mounted from another device with the UUID", func(h *importHarness) {
			other := filepath.Join(h.dir, "other")
			if err := os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			h.runner.Script("findmnt", []string{"-n", "-o", "SOURCE", "/mnt/disk1"}, []byte(other+"\n"), nil)
		}, "is mounted from"},
		{"a disk comes up with another filesystem", func(h *importHarness) {
			h.runner.Script("findmnt", []string{"-n", "-o", "UUID", "/mnt/disk1"}, []byte("other-uuid\n"), nil)
		}, "want "},
		{"the rebuilt array has no pool", func(h *importHarness) { h.readyErr = errors.New("injected: rebuild failed") }, "injected: rebuild failed"},
		{"the pool does not mount", func(h *importHarness) { h.poolFails = true }, "injected: the pool did not mount"},
		{"the pool comes up read-write", func(h *importHarness) {
			h.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", impCatchAt}, []byte("rw,relatime\n"), nil)
		}, "read-write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newImportHarness(t)
			tc.setup(h)
			done := h.run()
			if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, tc.want) {
				t.Fatalf("job ended %s: %q, want it failed with %q", done.Status, done.ErrorMessage, tc.want)
			}
			h.assertNothingAdopted("after the undo")
			if len(h.mounter.unmounts) == 0 && len(h.mounter.mounts) > 0 {
				t.Errorf("mounted %v and never unmounted", h.mounter.mounts)
			}
			// The pool goes down before the disks under it.
			poolAt, diskAt := -1, -1
			for i, w := range h.mounter.unmounts {
				if w == impCatchAt {
					poolAt = i
				} else if diskAt < 0 {
					diskAt = i
				}
			}
			if poolAt >= 0 && diskAt >= 0 && poolAt > diskAt {
				t.Errorf("unmount order = %v, want the pool before the disks", h.mounter.unmounts)
			}
		})
	}
}

// A mount that cannot be released keeps the record: it never claims fewer disks
// than are mounted, and the failure says so.
func TestMigrationImport_AnUndoThatCannotUnmountKeepsTheRecord(t *testing.T) {
	h := newImportHarness(t)
	h.mounter.failMount = "/mnt/disk2"
	h.mounter.failUnmont = "/mnt/disk1"
	done := h.run()
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "could not be undone") || !strings.Contains(done.ErrorMessage, "injected: the unmount failed") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	if exists, err := h.st.Exists(ctx); err != nil || !exists {
		t.Errorf("array record exists = %v, %v: the record of a disk that is still mounted was deleted", exists, err)
	}
	if !h.mounter.mounted["/mnt/disk1"] {
		t.Error("the fake did not keep /mnt/disk1 mounted")
	}
}

// The undo never takes a disk's mount for its own when it cannot tell whether it
// is mounted: an error reading the mount table is not "not mounted".
func TestMigrationImport_AnUndoThatCannotReadTheMountTableKeepsTheRecord(t *testing.T) {
	h := newImportHarness(t)
	h.mounter.failMount = "/mnt/disk2"
	h.mounter.mountedErr = errors.New("injected: the mount table is unreadable")
	done := h.run()
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "injected: the mount table is unreadable") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	if exists, err := h.st.Exists(context.Background()); err != nil || !exists {
		t.Errorf("array record exists = %v, %v: it was deleted without knowing the disks were unmounted", exists, err)
	}
	if len(h.mounter.unmounts) != 0 {
		t.Errorf("unmounted %v on a guess", h.mounter.unmounts)
	}
}

// A run after one that stopped part-way applies the recorded array again, checks
// and formats nothing, and keeps the record if it fails too; another mapping is
// refused.
func TestMigrationImport_ARetryOfARecordedImportAppliesItAgain(t *testing.T) {
	ctx := context.Background()
	seed := func(h *importHarness) {
		disks, recorded := pendingRows(h.plan)
		if err := h.st.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)}, disks, recorded); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the same plan", func(t *testing.T) {
		h := newImportHarness(t)
		seed(h)
		done := h.run()
		if done.Status != StatusSucceeded {
			t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
		}
		if got := h.xfsChecks(); len(got) != 0 {
			t.Errorf("a retry re-ran the filesystem checks on disks that may already be mounted: %v", got)
		}
		if len(h.mounter.mounts) != 3 {
			t.Errorf("mounts = %+v", h.mounter.mounts)
		}
	})
	t.Run("the same plan failing again keeps the record", func(t *testing.T) {
		h := newImportHarness(t)
		seed(h)
		h.mounter.failMount = "/mnt/disk2"
		done := h.run()
		if done.Status != StatusFailed {
			t.Fatalf("job ended %s", done.Status)
		}
		if exists, err := h.st.Exists(ctx); err != nil || !exists {
			t.Errorf("array record exists = %v, %v: a retry deleted a record it did not make", exists, err)
		}
		if len(h.mounter.unmounts) != 0 {
			t.Errorf("a retry unmounted %v: it undoes only what it recorded", h.mounter.unmounts)
		}
	})
	t.Run("another mapping", func(t *testing.T) {
		h := newImportHarness(t)
		seed(h)
		h.freshPlan.Data = append([]disk.AdoptedDisk(nil), h.plan.Data...)
		h.freshPlan.Data = h.freshPlan.Data[:1]
		h.plan = h.freshPlan
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, store.ErrArrayExists.Error()) {
			t.Fatalf("job ended %s: %q, want an existing array", done.Status, done.ErrorMessage)
		}
		if len(h.mounter.mounts) != 0 {
			t.Errorf("mounted %v", h.mounter.mounts)
		}
	})
	t.Run("an array that is not a pending import's", func(t *testing.T) {
		h := newImportHarness(t)
		if err := h.st.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
			{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: impUUID1, Mountpoint: "/mnt/disk1"},
		}); err != nil {
			t.Fatal(err)
		}
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, store.ErrArrayExists.Error()) {
			t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
		}
		if len(h.mounter.mounts) != 0 {
			t.Errorf("mounted %v", h.mounter.mounts)
		}
		if _, disks, err := h.st.GetArray(ctx); err != nil || len(disks) != 1 {
			t.Errorf("the existing array = %+v, %v: it was changed", disks, err)
		}
	})
}

func TestMigrationImport_ParamsNeedTheMappingAndAValidPlan(t *testing.T) {
	if err := ValidateParams(TypeMigrationImport, nil); err == nil {
		t.Error("no params were accepted")
	}
	if err := ValidateParams(TypeMigrationImport, []byte(`{"assignments":[],"plan":{}}`)); err == nil {
		t.Error("an empty mapping was accepted")
	}
	h := newImportHarness(t)
	bad := h.plan
	bad.Parity = nil
	body, _ := json.Marshal(MigrationImportParams{Assignments: []disk.AdoptionAssignment{{Role: disk.AdoptData, Serial: "DAT1"}}, Plan: bad})
	if err := ValidateParams(TypeMigrationImport, body); err == nil || !strings.Contains(err.Error(), "parity") {
		t.Errorf("a plan with no parity = %v", err)
	}
	if err := ValidateParams(TypeMigrationImport, h.params()); err != nil {
		t.Errorf("a valid mapping and plan = %v", err)
	}
	if class, _ := ClassOf(TypeMigrationImport); class != ClassTopology {
		t.Errorf("class = %s, want topology", class)
	}
	if Resumable(TypeMigrationImport) {
		t.Error("an import is not resumable: a retry is a new run")
	}
}

func TestScheduler_RefusesStorageJobsWhileAMigrationIsPending(t *testing.T) {
	ctx := context.Background()
	noop := func(context.Context, *RunContext) error { return nil }
	newScheduler := func(t *testing.T) *Scheduler {
		s := newTestScheduler(t)
		for _, typ := range []Type{TypeSync, TypeScrub, TypeMover, TypeRebalance, TypeDiskFormat, TypeDiskAdd, TypeMigrationScan, TypeConfigBackup, TypeMigrationImport} {
			s.registry.Register(typ, false, noop)
		}
		return s
	}
	h := newImportHarness(t)
	importParams := h.params()
	params := map[Type][]byte{
		TypeMigrationScan:   []byte(`{"upload":"upload-x.zip"}`),
		TypeMigrationImport: importParams,
		TypeDiskFormat:      nil,
		TypeDiskAdd:         nil,
		TypeRebalance:       nil,
	}
	delete(params, TypeDiskFormat)
	delete(params, TypeDiskAdd)
	delete(params, TypeRebalance)

	t.Run("pending", func(t *testing.T) {
		s := newScheduler(t)
		s.SetMigrationPending(func(context.Context) (bool, error) { return true, nil })
		for _, typ := range []Type{TypeSync, TypeScrub, TypeMover, TypeMigrationScan} {
			if _, err := s.Submit(ctx, typ, nil, params[typ]); !errors.Is(err, ErrMigrationInProgress) {
				t.Errorf("%s while pending = %v, want ErrMigrationInProgress", typ, err)
			}
		}
		for _, typ := range []Type{TypeConfigBackup, TypeMigrationImport} {
			j, err := s.Submit(ctx, typ, nil, params[typ])
			if err != nil {
				t.Errorf("%s was refused while pending: a service job and the import's own retry are admitted: %v", typ, err)
				continue
			}
			if _, err := s.Await(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("not pending", func(t *testing.T) {
		s := newScheduler(t)
		s.SetMigrationPending(func(context.Context) (bool, error) { return false, nil })
		j, err := s.Submit(ctx, TypeSync, nil, nil)
		if err != nil {
			t.Fatalf("a sync with nothing pending = %v", err)
		}
		if _, err := s.Await(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("no check configured", func(t *testing.T) {
		s := newScheduler(t)
		j, err := s.Submit(ctx, TypeScrub, nil, nil)
		if err != nil {
			t.Fatalf("a scrub with no migration check = %v", err)
		}
		if _, err := s.Await(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a check that fails refuses the job", func(t *testing.T) {
		s := newScheduler(t)
		s.SetMigrationPending(func(context.Context) (bool, error) { return false, errors.New("injected: the database is gone") })
		_, err := s.Submit(ctx, TypeSync, nil, nil)
		if err == nil || errors.Is(err, ErrMigrationInProgress) || !strings.Contains(err.Error(), "injected: the database is gone") {
			t.Errorf("a sync whose pending check failed = %v, want the check's error and no job", err)
		}
		if jobs, lerr := s.store.List(ctx, ListFilter{}); lerr != nil || len(jobs) != 0 {
			t.Errorf("jobs = %+v, %v: a job was created although the check failed", jobs, lerr)
		}
	})
}

type sourceTable struct {
	*FakeMountTable
	source string
}

func (s sourceTable) MountedSource(context.Context, string) (string, error) { return s.source, nil }

func TestArrayDiskUUIDCheck_ConfirmsTheSourceOfADeviceBoundDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dev, other := filepath.Join(dir, "sdc1"), filepath.Join(dir, "sdb1")
	for _, p := range []string{dev, other} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "by-id-part1")
	if err := os.Symlink(dev, link); err != nil {
		t.Fatal(err)
	}
	table := NewFakeMountTable()
	table.Preload("/mnt/disk1", impUUID1)
	units := []disk.MountUnit{{Where: "/mnt/disk1", UUID: impUUID1, What: link}}

	if err := (ArrayDiskUUIDCheck{Mounts: sourceTable{table, dev}, Disks: units}).ConfirmArrayDisks(ctx); err != nil {
		t.Errorf("the right device = %v", err)
	}
	// The former parity disk carries the same UUID: only the source tells.
	err := ArrayDiskUUIDCheck{Mounts: sourceTable{table, other}, Disks: units}.ConfirmArrayDisks(ctx)
	if !errors.Is(err, ErrArrayDiskMismatch) || !strings.Contains(err.Error(), "is mounted from") {
		t.Errorf("another device with the same UUID = %v, want ErrArrayDiskMismatch", err)
	}
	// A mount table that cannot report a source cannot confirm one.
	if err := (ArrayDiskUUIDCheck{Mounts: table, Disks: units}).ConfirmArrayDisks(ctx); err == nil {
		t.Error("a device-bound disk was confirmed by a table that cannot report its source")
	}
	// A disk with no device binding is confirmed by UUID as before.
	if err := (ArrayDiskUUIDCheck{Mounts: table, Disks: []disk.MountUnit{{Where: "/mnt/disk1", UUID: impUUID1}}}).ConfirmArrayDisks(ctx); err != nil {
		t.Errorf("an unbound disk = %v", err)
	}
}

func TestArrayMountUnits_AreReadOnlyOnlyWhileAMigrationIsPending(t *testing.T) {
	disks := []store.ArrayDisk{{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: impUUID1, Mountpoint: "/mnt/disk1", MountSource: "/dev/disk/by-id/x-part1"}}
	pending, err := ArrayMountUnits(store.ArraySettings{MigrationPending: true}, disks)
	if err != nil || !pending[0].ReadOnly || pending[0].What != "/dev/disk/by-id/x-part1" {
		t.Errorf("pending units = %+v, %v", pending, err)
	}
	plain, err := ArrayMountUnits(store.ArraySettings{}, disks)
	if err != nil || plain[0].ReadOnly {
		t.Errorf("units of an ordinary array = %+v, %v: they must stay read-write", plain, err)
	}
	if state := poolStateFromStore(store.ArraySettings{MigrationPending: true}, disks); !state.ReadOnly {
		t.Error("the pool state of a pending migration is not read-only")
	}
	if state := poolStateFromStore(store.ArraySettings{}, disks); state.ReadOnly {
		t.Error("the pool state of an ordinary array is read-only")
	}
}

// The shares and accounts are seeded once the disks and the pool are up, and a
// seed that fails undoes the adoption it follows: nothing is left recorded,
// mounted or generated.
func TestMigrationImport_SeedsAfterTheAdoptionAndUndoesItWhenTheSeedFails(t *testing.T) {
	t.Run("the seed runs with the disks and the pool mounted", func(t *testing.T) {
		h := newImportHarness(t)
		done := h.run()
		if done.Status != StatusSucceeded {
			t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
		}
		sort.Strings(h.mountedAtSeed)
		if h.seeded != 1 || strings.Join(h.mountedAtSeed, " ") != "/mnt/disk1 /mnt/disk2 /mnt/user" {
			t.Errorf("seed ran %d times with %v mounted, want once with the disks and the pool up", h.seeded, h.mountedAtSeed)
		}
	})
	t.Run("a failed seed is undone with the adoption", func(t *testing.T) {
		h := newImportHarness(t)
		h.seedErr = errors.New("injected: the seed failed")
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "injected: the seed failed") {
			t.Fatalf("job ended %s: %q, want it failed by the seed", done.Status, done.ErrorMessage)
		}
		h.assertNothingAdopted("after a failed seed")
	})
	t.Run("a retry of a recorded import seeds again, and a failing seed keeps the record", func(t *testing.T) {
		ctx := context.Background()
		for _, fail := range []bool{false, true} {
			h := newImportHarness(t)
			disks, recorded := pendingRows(h.plan)
			if err := h.st.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)}, disks, recorded); err != nil {
				t.Fatal(err)
			}
			if fail {
				h.seedErr = errors.New("injected: the seed failed")
			}
			done := h.run()
			if h.seeded != 1 {
				t.Errorf("fail=%v: seed ran %d times on a retry", fail, h.seeded)
			}
			if fail != (done.Status == StatusFailed) {
				t.Errorf("fail=%v: job ended %s: %s", fail, done.Status, done.ErrorMessage)
			}
			if exists, err := h.st.Exists(ctx); err != nil || !exists {
				t.Errorf("fail=%v: a retry deleted a record it did not make (%v, %v)", fail, exists, err)
			}
		}
	})
	t.Run("a daemon without the seed hook refuses before anything is written", func(t *testing.T) {
		h := newImportHarness(t)
		h.noSeed = true
		done := h.run()
		if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "missing its dependencies") {
			t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
		}
		h.assertNothingAdopted("without a seed hook")
	})
}
