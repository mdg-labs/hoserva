package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	parityNewUUID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	cacheNewUUID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// parityHarness is the point of no return's dependencies over fakes: the
// adopted array of the import harness (two data disks mounted read-only, a
// recorded parity disk and cache), a provider that knows the disks, a mount table
// the mounter keeps up to date, and an event log every step writes to so a test
// can say what happened before what.
type parityHarness struct {
	t        *testing.T
	s        *Scheduler
	st       *store.ArrayStore
	genRoot  string
	runner   *disk.FakeRunner
	provider *loggingProvider
	plan     disk.AdoptionPlan
	dir      string

	mu        sync.Mutex
	log       []string
	mounted   map[string]bool
	failMount map[string]error
	failUnmnt map[string]error

	services []*fakeArrayService
	seq      *ArraySequence

	// planErr is what the plan hook returns instead of the plan; fresh, when set,
	// is the plan it resolves.
	planErr   error
	fresh     *disk.AdoptionPlan
	sharesErr error
	readyErr  error
	syncErr   error
	engine    *parity.FakeEngine
	syncJobs  []string
	// queueProbe, when set, runs just before the sync is queued.
	queueProbe func(ctx context.Context)
}

// loggingProvider is the fake provider that notes each accepted format in the
// harness's event log.
type loggingProvider struct {
	*disk.FakeProvider
	h *parityHarness
	// failFormat, when set, refuses the format of the device whose by-id path
	// ends with it.
	failFormat string
}

func (p *loggingProvider) Format(ctx context.Context, dev string, fs disk.FilesystemType) error {
	if p.failFormat != "" && strings.HasSuffix(dev, p.failFormat) {
		return errors.New("injected: mkfs failed")
	}
	if err := p.FakeProvider.Format(ctx, dev, fs); err != nil {
		return err
	}
	p.h.note("format:" + dev)
	return nil
}

func (h *parityHarness) note(event string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, event)
}

func (h *parityHarness) events() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

// indexOf is where event first appears in the log, or -1.
func (h *parityHarness) indexOf(event string) int {
	for i, e := range h.events() {
		if e == event {
			return i
		}
	}
	return -1
}

func (h *parityHarness) has(prefix string) bool {
	for _, e := range h.events() {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func (h *parityHarness) formats() []string {
	var out []string
	for _, e := range h.events() {
		if dev, ok := strings.CutPrefix(e, "format:"); ok {
			out = append(out, dev)
		}
	}
	return out
}

// mountTable is the unit mounter and the kernel's mount table in one: a mount
// scripts what findmnt then reports for it, as the real kernel would.
type mountTable struct{ h *parityHarness }

func (m mountTable) Mount(_ context.Context, u disk.MountUnit) error {
	h := m.h
	mode := "rw"
	if u.ReadOnly {
		mode = "ro"
	}
	h.note("mount:" + u.Where + ":" + mode)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.failMount[u.Where+":"+mode]; err != nil {
		return err
	}
	h.mounted[u.Where] = true
	h.runner.Script("findmnt", []string{"-n", "-o", "UUID", u.Where}, []byte(u.UUID+"\n"), nil)
	if u.What != "" {
		if real, err := filepath.EvalSymlinks(u.What); err == nil {
			h.runner.Script("findmnt", []string{"-n", "-o", "SOURCE", u.Where}, []byte(real+"\n"), nil)
		}
	}
	opts := "rw,relatime"
	if u.ReadOnly {
		opts = "ro,nosuid,nodev,noexec,noatime"
	}
	h.runner.Script("findmnt", []string{"-n", "-o", "OPTIONS", u.Where}, []byte(opts+"\n"), nil)
	return nil
}

func (m mountTable) Unmount(_ context.Context, u disk.MountUnit) error {
	h := m.h
	h.note("unmount:" + u.Where)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.failUnmnt[u.Where]; err != nil {
		return err
	}
	delete(h.mounted, u.Where)
	return nil
}

func (h *parityHarness) isMounted(where string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mounted[where], nil
}

// parityPool is the catch-all mount of the rebuilt array sequence: read-only
// while the migration is pending, read-write after.
type parityPool struct{ h *parityHarness }

func (p parityPool) Where() string { return impCatchAt }

func (p parityPool) Mount(ctx context.Context) error {
	pending, _ := p.h.st.MigrationPending(ctx)
	return mountTable(p).Mount(ctx, disk.MountUnit{Where: impCatchAt, ReadOnly: pending, UUID: "pool"})
}

func (p parityPool) Unmount(ctx context.Context) error {
	return mountTable(p).Unmount(ctx, disk.MountUnit{Where: impCatchAt})
}

func newParityHarness(t *testing.T) *parityHarness {
	t.Helper()
	imp := newImportHarness(t)
	h := &parityHarness{
		t: t, s: newTestScheduler(t), st: store.NewArrayStore(newTestDB(t)), genRoot: t.TempDir(), runner: disk.NewFakeRunner(),
		plan: imp.plan, dir: imp.dir, mounted: map[string]bool{}, failMount: map[string]error{}, failUnmnt: map[string]error{},
		engine: parity.NewFakeEngine(),
	}
	// The plan's data disks are reached through by-id links in imp's directory.
	h.provider = &loggingProvider{FakeProvider: disk.NewFakeProvider(), h: h}
	h.provider.AddDisk("/dev/sdb", disk.Disk{Serial: "PAR1", ByIDName: "ata-EX_PAR1", Size: 8 << 40, Filesystem: "xfs", FSUUID: impUUID1})
	h.provider.AddDisk("/dev/sdc", disk.Disk{Serial: "DAT1", ByIDName: "ata-EX_DAT1", Size: 4 << 40, Filesystem: "xfs", FSUUID: impUUID1})
	h.provider.AddDisk("/dev/sdd", disk.Disk{Serial: "DAT2", ByIDName: "ata-EX_DAT2", Size: 2 << 40, Filesystem: "ext4", FSUUID: impUUID2})
	h.provider.AddDisk("/dev/nvme0n1", disk.Disk{Serial: "CAC1", ByIDName: "nvme-EX_CAC1", Size: 500 << 30, Filesystem: "btrfs"})

	h.runner.Script("blkid", []string{"-p", "-s", "UUID", "-o", "value", "/dev/disk/by-id/ata-EX_PAR1"}, []byte(parityNewUUID+"\n"), nil)
	h.runner.Script("blkid", []string{"-p", "-s", "UUID", "-o", "value", "/dev/disk/by-id/nvme-EX_CAC1"}, []byte(cacheNewUUID+"\n"), nil)

	ctx := context.Background()
	disks, recorded := pendingRows(h.plan)
	if err := h.st.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Date(2026, 10, 4, 11, 0, 0, 0, time.UTC)}, disks, recorded); err != nil {
		t.Fatal(err)
	}
	// The adoption is mounted read-only, as the import left it.
	for _, u := range h.plan.MountUnits() {
		if err := (mountTable{h}).Mount(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	h.seq = &ArraySequence{}
	h.services = []*fakeArrayService{{name: "samba", log: &h.log}, {name: "nfs", log: &h.log}}
	h.buildSequence()
	if err := (parityPool{h}).Mount(ctx); err != nil {
		t.Fatal(err)
	}
	h.log = nil

	// A sync the point of no return queued is waited for before the database goes.
	t.Cleanup(func() {
		for _, id := range h.syncJobs {
			_, _ = h.s.Await(context.Background(), id)
		}
	})
	h.s.SetMigrationPending(h.st.MigrationUnfinished)
	h.s.registry.Register(TypeSync, false, RunSync(h.engine))
	h.s.registry.Register(TypeMigrationParity, false, RunMigrationParity(h.deps()))
	return h
}

func (h *parityHarness) buildSequence() {
	var svcs []ArrayService
	for _, s := range h.services {
		svcs = append(svcs, s)
	}
	h.mu.Lock()
	h.seq = &ArraySequence{Services: svcs, CatchAll: parityPool{h}}
	h.mu.Unlock()
}

func (h *parityHarness) deps() MigrationParityDeps {
	return MigrationParityDeps{
		Plan: func(ctx context.Context) (disk.AdoptionPlan, error) {
			if h.planErr != nil {
				return disk.AdoptionPlan{}, h.planErr
			}
			if h.fresh != nil {
				return *h.fresh, nil
			}
			return h.plan, nil
		},
		Provider:  h.provider,
		Probe:     disk.NewFakeBlankProber(),
		Runner:    h.runner,
		Store:     h.st,
		Generator: config.NewGenerator(h.genRoot),
		Mounter:   mountTable{h},
		ArrayReady: func(ctx context.Context) error {
			h.note("ready")
			if h.readyErr != nil {
				return h.readyErr
			}
			h.buildSequence()
			return nil
		},
		Array: func() *ArraySequence {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.seq
		},
		Shares: func(ctx context.Context, out io.Writer) error {
			h.note("shares")
			for _, w := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/parity1", "/mnt/cache"} {
				if mounted, _ := h.isMounted(w); !mounted {
					h.note("shares-without:" + w)
				}
			}
			return h.sharesErr
		},
		QueueSync: func(ctx context.Context) (string, error) {
			h.note("queue-sync")
			if h.queueProbe != nil {
				h.queueProbe(ctx)
			}
			if h.syncErr != nil {
				return "", h.syncErr
			}
			id, err := QueueInitialSync(h.s)(ctx)
			if err == nil {
				h.syncJobs = append(h.syncJobs, id)
			}
			return id, err
		},
		IsMounted: h.isMounted,
	}
}

func (h *parityHarness) confirmation() string { return h.plan.ParityInitConfirmation() }

func (h *parityHarness) run(confirmation string) *Job {
	h.t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(MigrationParityParams{Confirmation: confirmation})
	if err != nil {
		h.t.Fatal(err)
	}
	j, err := h.s.Submit(ctx, TypeMigrationParity, []string{"migration"}, body)
	if err != nil {
		h.t.Fatalf("Submit: %v", err)
	}
	done, err := h.s.Await(ctx, j.ID)
	if err != nil {
		h.t.Fatalf("Await: %v", err)
	}
	return done
}

// assertUntouched is the state a refusal or a failure before the first format
// leaves: still pending, nothing recorded, no snapraid.conf, every format call
// absent, and the adoption mounted read-only with the pool over it.
func (h *parityHarness) assertUntouched(when string) {
	h.t.Helper()
	ctx := context.Background()
	if pending, err := h.st.MigrationPending(ctx); err != nil || !pending {
		h.t.Errorf("%s: MigrationPending = %v, %v, want the pending state intact", when, pending, err)
	}
	if finishing, _ := h.st.MigrationFinishing(ctx); finishing {
		h.t.Errorf("%s: the migration is finishing", when)
	}
	if _, disks, err := h.st.GetArray(ctx); err != nil || len(disks) != 2 {
		h.t.Errorf("%s: array disks = %+v, %v, want only the two adopted data disks", when, disks, err)
	}
	if rec, err := h.st.RecordedDisks(ctx); err != nil || len(rec) != 2 {
		h.t.Errorf("%s: recorded = %+v, %v", when, rec, err)
	}
	if _, err := os.Stat(filepath.Join(h.genRoot, "snapraid.conf")); err == nil {
		h.t.Errorf("%s: snapraid.conf was generated", when)
	}
	for _, w := range []string{"/mnt/disk1", "/mnt/disk2", impCatchAt} {
		if mounted, _ := h.isMounted(w); !mounted {
			h.t.Errorf("%s: %s is not mounted", when, w)
		}
		if ro, err := disk.MountedReadOnly(ctx, h.runner, w); err != nil || !ro {
			h.t.Errorf("%s: %s is not mounted read-only (%v, %v)", when, w, ro, err)
		}
	}
	if len(h.syncJobs) != 0 {
		h.t.Errorf("%s: a sync was queued", when)
	}
}

func (h *parityHarness) assertNoFormat(when string) {
	h.t.Helper()
	if f := h.formats(); len(f) != 0 {
		h.t.Errorf("%s: formatted %v, want nothing erased", when, f)
	}
}

func TestMigrationParity_FormatsOnlyParityAndCacheThenFinishesTheArray(t *testing.T) {
	h := newParityHarness(t)
	done := h.run(h.confirmation())
	if done.Status != StatusSucceeded {
		t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()

	// Exactly the confirmed parity disk and the cache were formatted, through
	// their by-id paths and never a data disk.
	want := []string{"/dev/disk/by-id/ata-EX_PAR1", "/dev/disk/by-id/nvme-EX_CAC1"}
	if got := h.formats(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("formatted %v, want %v", got, want)
	}
	for _, dev := range []string{"/dev/sdc", "/dev/sdd"} {
		if _, ok := h.provider.FormattedAs(dev); ok {
			t.Errorf("data disk %s was formatted", dev)
		}
	}

	// The record: parity and cache are array disks with the filesystems just
	// made, the migration is neither pending nor finishing, the data disks keep
	// their identity binding.
	settings, disks, err := h.st.GetArray(ctx)
	if err != nil || settings.MigrationPending || len(disks) != 4 {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	byMount := map[string]store.ArrayDisk{}
	for _, d := range disks {
		byMount[d.Mountpoint] = d
	}
	if p := byMount["/mnt/parity1"]; p.Role != store.ArrayRoleParity || p.FSUUID != parityNewUUID || p.Filesystem != "xfs" || p.Serial != "PAR1" {
		t.Errorf("parity row = %+v", p)
	}
	if c := byMount["/mnt/cache"]; c.Role != store.ArrayRoleCache || c.FSUUID != cacheNewUUID || c.Filesystem != "xfs" || c.Serial != "CAC1" {
		t.Errorf("cache row = %+v", c)
	}
	if d := byMount["/mnt/disk1"]; d.MountSource != h.plan.Data[0].MountSource || d.FSUUID != impUUID1 {
		t.Errorf("data disk 1 = %+v, want its adoption record kept", d)
	}
	if unfinished, err := h.st.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished = %v, %v after the job", unfinished, err)
	}

	// The order that makes it safe: what holds the read-only mounts is stopped
	// and the mounts released before anything is erased; the disks come back
	// read-write only after the formatted ones are recorded; the shares' deferred
	// settings are applied once every disk is up; the sync is queued last.
	before := func(a, b string) {
		t.Helper()
		ia, ib := h.indexOf(a), h.indexOf(b)
		if ia < 0 || ib < 0 || ia > ib {
			t.Errorf("%q (at %d) must come before %q (at %d); log:\n%s", a, ia, b, ib, strings.Join(h.events(), "\n"))
		}
	}
	before("stop:samba", "unmount:"+impCatchAt)
	before("unmount:"+impCatchAt, "unmount:/mnt/disk1")
	before("unmount:/mnt/disk2", "format:"+want[0])
	before("format:"+want[1], "mount:/mnt/disk1:rw")
	before("mount:/mnt/disk1:rw", "shares")
	before("mount:/mnt/parity1:rw", "shares")
	before("mount:/mnt/cache:rw", "shares")
	before("shares", "ready")
	before("ready", "start:samba")
	before("start:samba", "queue-sync")
	if h.has("shares-without:") {
		t.Errorf("the shares step ran before every disk was mounted:\n%s", strings.Join(h.events(), "\n"))
	}
	for _, w := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/parity1", "/mnt/cache", impCatchAt} {
		if mounted, _ := h.isMounted(w); !mounted {
			t.Errorf("%s is not mounted at the end", w)
		}
		if ro, err := disk.MountedReadOnly(ctx, h.runner, w); err != nil || ro {
			t.Errorf("%s is read-only after the point of no return (%v, %v)", w, ro, err)
		}
	}

	// Generated files: read-write data units, parity and cache units, and a
	// snapraid.conf with the parity file and content files placed per doc 02 §2.
	unit1, err := os.ReadFile(filepath.Join(h.genRoot, "systemd", "system", "mnt-disk1.mount"))
	if err != nil || strings.Contains(string(unit1), "ro,") || !strings.Contains(string(unit1), "What="+h.plan.Data[0].MountSource) {
		t.Errorf("mnt-disk1.mount = %s, %v, want a read-write unit bound to the disk's device", unit1, err)
	}
	for _, name := range []string{"mnt-parity1.mount", "mnt-cache.mount"} {
		if _, err := os.Stat(filepath.Join(h.genRoot, "systemd", "system", name)); err != nil {
			t.Errorf("%s was not generated: %v", name, err)
		}
	}
	conf, err := os.ReadFile(filepath.Join(h.genRoot, "snapraid.conf"))
	if err != nil {
		t.Fatalf("snapraid.conf: %v", err)
	}
	for _, line := range []string{"parity /mnt/parity1/snapraid.parity", "content /var/lib/hoserva/snapraid.content", "content /mnt/cache/snapraid.content", "data d1 /mnt/disk1/", "data d2 /mnt/disk2/"} {
		if !strings.Contains(string(conf), line+"\n") {
			t.Errorf("snapraid.conf lacks %q:\n%s", line, conf)
		}
	}

	// The initial sync is queued through the scheduler, an ordinary sync with no
	// confirmation of a guard block.
	if len(h.syncJobs) != 1 {
		t.Fatalf("queued syncs = %v, want one", h.syncJobs)
	}
	sj, err := h.s.Await(ctx, h.syncJobs[0])
	if err != nil || sj.Type != TypeSync || sj.Status != StatusSucceeded {
		t.Fatalf("the sync = %+v, %v", sj, err)
	}
	if opts, err := SyncOptsFromParams(sj.Params); err != nil || opts.Confirm || opts.DryRun {
		t.Errorf("sync params = %+v, %v, want a real sync that confirms no guard block", opts, err)
	}
}

// The initial sync is an ordinary sync: when the threshold guard blocks it, the
// sync does not run and the job that queued it has not confirmed anything past
// the block.
func TestMigrationParity_TheInitialSyncGoesThroughTheGuardNeverAroundIt(t *testing.T) {
	h := newParityHarness(t)
	h.engine.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerZeroFiles}})
	done := h.run(h.confirmation())
	if done.Status != StatusSucceeded || len(h.syncJobs) != 1 {
		t.Fatalf("job ended %s (%s), syncs %v", done.Status, done.ErrorMessage, h.syncJobs)
	}
	sj, err := h.s.Await(context.Background(), h.syncJobs[0])
	if err != nil {
		t.Fatal(err)
	}
	if sj.Status != StatusFailed || !strings.Contains(sj.ErrorMessage, "threshold guard blocked the sync") {
		t.Errorf("the sync ended %s: %q, want it stopped by the threshold guard", sj.Status, sj.ErrorMessage)
	}
	opts, err := SyncOptsFromParams(sj.Params)
	if err != nil || opts.Confirm {
		t.Errorf("sync params = %+v, %v: the initial sync must never confirm past the guard", opts, err)
	}
}

func TestMigrationParity_RefusesBeforeTouchingAnything(t *testing.T) {
	for name, tc := range map[string]struct {
		setup        func(h *parityHarness)
		confirmation func(h *parityHarness) string
		want         string
	}{
		"the gate refuses (no passing verify)": {
			setup:        func(h *parityHarness) { h.planErr = errors.New("verify_required: the adopted array was not verified") },
			confirmation: func(h *parityHarness) string { return h.confirmation() },
			want:         "verify_required",
		},
		"a wrong confirmation": {
			confirmation: func(h *parityHarness) string { return "ERASE /dev/sdb" },
			want:         "typed confirmation",
		},
		"a data disk named in the confirmation": {
			confirmation: func(h *parityHarness) string { return "ERASE /dev/nvme0n1, /dev/sdb, /dev/sdc" },
			want:         "typed confirmation",
		},
		"a data disk that changed since the import": {
			setup: func(h *parityHarness) {
				fresh := h.plan
				fresh.Data = append([]disk.AdoptedDisk(nil), h.plan.Data...)
				fresh.Data[1].FSUUID = "99999999-9999-4999-8999-999999999999"
				h.fresh = &fresh
			},
			confirmation: func(h *parityHarness) string { return h.confirmation() },
			want:         "the adoption recorded",
		},
		"a parity disk swapped for another": {
			setup: func(h *parityHarness) {
				fresh := h.plan
				fresh.Parity = []disk.RecordedDisk{h.plan.Parity[0]}
				fresh.Parity[0].Serial = "OTHER"
				h.fresh = &fresh
			},
			confirmation: func(h *parityHarness) string { return h.confirmation() },
			want:         "the adoption recorded",
		},
		"a parity disk of another size": {
			setup: func(h *parityHarness) {
				fresh := h.plan
				fresh.Parity = []disk.RecordedDisk{h.plan.Parity[0]}
				fresh.Parity[0].Size = 9 << 40
				h.fresh = &fresh
			},
			confirmation: func(h *parityHarness) string { return h.confirmation() },
			want:         "size",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newParityHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			done := h.run(tc.confirmation(h))
			if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, tc.want) {
				t.Fatalf("job ended %s: %q, want it refused with %q", done.Status, done.ErrorMessage, tc.want)
			}
			h.assertNoFormat("a refusal")
			h.assertUntouched("a refusal")
			if h.has("unmount:") || h.has("stop:") {
				t.Errorf("a refusal stopped or unmounted something:\n%s", strings.Join(h.events(), "\n"))
			}
		})
	}
}

func TestMigrationParity_ARunWithNothingPendingIsRefused(t *testing.T) {
	h := newParityHarness(t)
	// An ordinary array: the adoption's record is deleted and one made by hand
	// takes its place.
	ctx := context.Background()
	if _, err := h.st.DeletePendingArray(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.st.PutArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, []store.ArrayDisk{
		{Role: store.ArrayRoleData, RoleIndex: 1, Device: "/dev/sdc", Filesystem: "xfs", FSUUID: "u", Mountpoint: "/mnt/disk1"},
	}); err != nil {
		t.Fatal(err)
	}
	done := h.run(h.confirmation())
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, ErrMigrationParityNotPending.Error()) {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	h.assertNoFormat("an array that is not a pending migration's")
}

// A failure before the first disk is formatted leaves the pending state as it
// was and the adoption mounted read-only again, with what was stopped started.
func TestMigrationParity_AFailureBeforeTheFirstFormatLeavesThePendingStateIntact(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(h *parityHarness)
		want  string
	}{
		"a service will not stop": {
			setup: func(h *parityHarness) { h.services[1].stopErr = errors.New("injected: nfs would not stop") },
			want:  "injected: nfs would not stop",
		},
		"a data disk will not unmount": {
			setup: func(h *parityHarness) { h.failUnmnt["/mnt/disk2"] = errors.New("injected: target is busy") },
			want:  "injected: target is busy",
		},
		"the cache disk is gone when the formats are resolved": {
			setup: func(h *parityHarness) {
				p := disk.NewFakeProvider()
				p.AddDisk("/dev/sdb", disk.Disk{Serial: "PAR1", ByIDName: "ata-EX_PAR1", Size: 8 << 40})
				h.provider.FakeProvider = p
			},
			want: "no disk on this machine has the identity given",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newParityHarness(t)
			tc.setup(h)
			done := h.run(h.confirmation())
			if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, tc.want) {
				t.Fatalf("job ended %s: %q, want it failed with %q", done.Status, done.ErrorMessage, tc.want)
			}
			h.assertNoFormat("a failure before the first format")
			h.assertUntouched("a failure before the first format")
			// What was stopped is started again, and the pool is read-only again.
			for _, s := range h.services {
				if !h.has("start:" + s.name) {
					t.Errorf("%s was stopped and never started again:\n%s", s.name, strings.Join(h.events(), "\n"))
				}
			}
		})
	}
}

// A format that fails part-way leaves the migration pending: the disks are read
// only again, nothing is recorded, and running it again formats them again, but
// never a data disk.
func TestMigrationParity_AFailedFormatLeavesItPendingAndARetryNeverFormatsADataDisk(t *testing.T) {
	h := newParityHarness(t)
	h.provider.failFormat = "nvme-EX_CAC1"
	done := h.run(h.confirmation())
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "injected: mkfs failed") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	if got := h.formats(); len(got) != 1 || got[0] != "/dev/disk/by-id/ata-EX_PAR1" {
		t.Fatalf("formatted %v, want only the parity disk, which was formatted before the cache failed", got)
	}
	h.assertUntouched("a format that failed after the first")

	h.provider.failFormat = ""
	done = h.run(h.confirmation())
	if done.Status != StatusSucceeded {
		t.Fatalf("the retry ended %s: %s", done.Status, done.ErrorMessage)
	}
	for _, dev := range h.formats() {
		if strings.Contains(dev, "DAT") {
			t.Errorf("a data disk was formatted: %s", dev)
		}
	}
	if pending, _ := h.st.MigrationPending(context.Background()); pending {
		t.Error("still pending after the retry")
	}
}

// A failure after the formatted disks are recorded is not "migration complete":
// the job says it did not finish, the migration stays unfinished (every parity,
// array-write and topology job but this one is refused), and running it again
// finishes it without formatting anything.
func TestMigrationParity_AFailureAfterTheRecordIsFinishedByRunningItAgainWithoutFormatting(t *testing.T) {
	h := newParityHarness(t)
	h.sharesErr = errors.New("injected: the shares could not be prepared")
	done := h.run(h.confirmation())
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "injected: the shares could not be prepared") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	formatted := h.formats()
	if len(formatted) != 2 {
		t.Fatalf("formatted %v, want the parity disk and the cache", formatted)
	}
	if finishing, err := h.st.MigrationFinishing(ctx); err != nil || !finishing {
		t.Fatalf("MigrationFinishing = %v, %v, want the migration kept unfinished", finishing, err)
	}
	if len(h.syncJobs) != 0 {
		t.Error("a sync was queued by a job that did not finish")
	}
	if _, err := h.s.Submit(ctx, TypeSync, nil, nil); !errors.Is(err, ErrMigrationInProgress) {
		t.Errorf("a sync submitted while the initialisation is unfinished = %v, want the migration_in_progress refusal", err)
	}

	// The erase confirmation of the first run is not the one that finishes it, and
	// a run that is not asked for the finishing is refused.
	if done := h.run(h.confirmation()); done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "typed confirmation") {
		t.Fatalf("a retry with the erase confirmation ended %s: %q", done.Status, done.ErrorMessage)
	}
	if len(h.formats()) != 2 {
		t.Fatalf("a refused retry formatted: %v", h.formats())
	}

	h.sharesErr = nil
	done = h.run(disk.ParityInitFinishConfirmation)
	if done.Status != StatusSucceeded {
		t.Fatalf("the finishing run ended %s: %s", done.Status, done.ErrorMessage)
	}
	if got := h.formats(); len(got) != 2 {
		t.Errorf("finishing formatted again: %v", got)
	}
	if finishing, _ := h.st.MigrationFinishing(ctx); finishing {
		t.Error("still finishing")
	}
	if len(h.syncJobs) != 1 {
		t.Errorf("queued syncs = %v, want one", h.syncJobs)
	}
	for _, w := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/parity1", "/mnt/cache"} {
		if ro, err := disk.MountedReadOnly(ctx, h.runner, w); err != nil || ro {
			t.Errorf("%s is read-only after the finish (%v, %v)", w, ro, err)
		}
	}

	// Once finished it is not run again.
	if done := h.run(disk.ParityInitFinishConfirmation); done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, ErrMigrationParityNotPending.Error()) {
		t.Errorf("a run after the migration finished ended %s: %q", done.Status, done.ErrorMessage)
	}
}

func TestMigrationParity_ASyncThatCannotBeQueuedIsSaidAfterTheArrayIsFinished(t *testing.T) {
	h := newParityHarness(t)
	h.syncErr = errors.New("injected: job_type_not_registered")
	done := h.run(h.confirmation())
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "the initial sync could not be queued") || !strings.Contains(done.ErrorMessage, "injected: job_type_not_registered") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	if unfinished, _ := h.st.MigrationUnfinished(ctx); unfinished {
		t.Error("the array is finished: a sync that cannot be queued is not a reason to hold every job back")
	}
}

// The layout snapraid.conf needs is checked before anything is erased: content
// files that cannot be placed on enough distinct devices refuse the run.
func TestMigrationParity_ALayoutThatCannotBeRenderedIsRefusedBeforeAnythingIsErased(t *testing.T) {
	h := newParityHarness(t)
	// One data disk, one parity disk and no cache: boot and that data disk are two
	// distinct devices, one fewer than the three Q18 asks for.
	fresh := h.plan
	fresh.Cache = nil
	ctx := context.Background()
	if _, err := h.st.DeletePendingArray(ctx); err != nil {
		t.Fatal(err)
	}
	disks, recorded := pendingRows(h.plan)
	if err := h.st.PutPendingArray(ctx, store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "50G", CreatedAt: time.Now()}, disks[:1], recorded[:1]); err != nil {
		t.Fatal(err)
	}
	fresh.Data = fresh.Data[:1]
	h.fresh = &fresh
	conf := fresh.ParityInitConfirmation()
	done := h.run(conf)
	if done.Status != StatusFailed || !strings.Contains(done.ErrorMessage, "cannot be configured for SnapRAID") {
		t.Fatalf("job ended %s: %q", done.Status, done.ErrorMessage)
	}
	h.assertNoFormat("a layout that cannot be rendered")
}

// restart is the daemon after a stop: a scheduler with nothing in memory over
// the same array record, with the sync job registered as main.go registers it
// once snapraid.conf exists and the migration gate set.
func (h *parityHarness) restart() *Scheduler {
	h.t.Helper()
	s := newTestScheduler(h.t)
	if err := s.RecoverFromRestart(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	s.SetMigrationPending(h.st.MigrationUnfinished)
	s.registry.Register(TypeSync, false, RunSync(h.engine))
	h.t.Cleanup(func() {
		jobs, _ := s.store.List(context.Background(), ListFilter{})
		for _, j := range jobs {
			_, _ = s.Await(context.Background(), j.ID)
		}
	})
	return s
}

func (h *parityHarness) assertOwed(when string, want bool) {
	h.t.Helper()
	if owed, err := h.st.InitialSyncOwed(context.Background()); err != nil || owed != want {
		h.t.Errorf("%s: InitialSyncOwed = %v, %v, want %v", when, owed, err, want)
	}
}

// The initial sync is owed from the moment the migration reads finished until it
// is queued, and the job that queues it clears what is owed.
func TestMigrationParity_TheInitialSyncIsOwedUntilItIsQueued(t *testing.T) {
	h := newParityHarness(t)
	var owedWhenQueued bool
	h.queueProbe = func(ctx context.Context) {
		owed, err := h.st.InitialSyncOwed(ctx)
		owedWhenQueued = err == nil && owed
	}
	h.assertOwed("before the point of no return", false)
	if done := h.run(h.confirmation()); done.Status != StatusSucceeded {
		t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
	}
	if !owedWhenQueued {
		t.Error("the sync was queued while nothing recorded it as owed: a stop right there would lose it")
	}
	h.assertOwed("after the sync was queued", false)
}

// A stop between FinishMigration and the queued sync (an error from the queue
// leaves exactly the state a stopped daemon would, as nothing after
// FinishMigration writes the record) leaves the array without a sync queued and
// the migration finished, so the migration gate no longer holds anything back.
// The next start queues the initial sync: an ordinary sync that runs through the
// engine's threshold guard.
func TestMigrationParity_AStopBeforeTheSyncIsQueuedIsFinishedByTheNextStart(t *testing.T) {
	h := newParityHarness(t)
	h.syncErr = errors.New("injected: the daemon stopped before the sync was queued")
	if done := h.run(h.confirmation()); done.Status != StatusFailed {
		t.Fatalf("job ended %s: %s", done.Status, done.ErrorMessage)
	}
	ctx := context.Background()
	if unfinished, _ := h.st.MigrationUnfinished(ctx); unfinished {
		t.Fatal("the migration is unfinished: the scenario is a finished migration with no sync queued")
	}
	if len(h.syncJobs) != 0 {
		t.Fatalf("a sync was queued: %v", h.syncJobs)
	}
	h.assertOwed("after the stop", true)

	s := h.restart()
	h.engine.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerZeroFiles}})
	id, err := QueueOwedInitialSync(ctx, h.st, s)
	if err != nil || id == "" {
		t.Fatalf("QueueOwedInitialSync = %q, %v, want the sync queued", id, err)
	}
	h.assertOwed("after the next start queued it", false)
	sj, err := s.Await(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sj.Type != TypeSync {
		t.Fatalf("queued a %s, want a sync", sj.Type)
	}
	if opts, err := SyncOptsFromParams(sj.Params); err != nil || opts.Confirm || opts.DryRun {
		t.Errorf("sync params = %+v, %v, want a real sync that confirms no guard block", opts, err)
	}
	if sj.Status != StatusFailed || !strings.Contains(sj.ErrorMessage, "threshold guard blocked the sync") {
		t.Errorf("the sync ended %s: %q, want it stopped by the threshold guard, never run around it", sj.Status, sj.ErrorMessage)
	}

	if again, err := QueueOwedInitialSync(ctx, h.st, s); err != nil || again != "" {
		t.Errorf("a second start queued %q, %v: nothing is owed any more", again, err)
	}
}

// When the guard lets the sync through, the one the next start queued is a real
// sync of the engine.
func TestMigrationParity_TheSyncTheNextStartQueuesRunsTheEngine(t *testing.T) {
	h := newParityHarness(t)
	h.syncErr = errors.New("injected: stopped")
	h.run(h.confirmation())
	s := h.restart()
	ctx := context.Background()
	id, err := QueueOwedInitialSync(ctx, h.st, s)
	if err != nil || id == "" {
		t.Fatalf("QueueOwedInitialSync = %q, %v", id, err)
	}
	if sj, err := s.Await(ctx, id); err != nil || sj.Status != StatusSucceeded {
		t.Fatalf("the sync = %+v, %v", sj, err)
	}
}

// A start that cannot queue the sync keeps what is owed, says so, and a later
// start queues it.
func TestMigrationParity_AStartThatCannotQueueTheSyncKeepsItOwed(t *testing.T) {
	h := newParityHarness(t)
	h.syncErr = errors.New("injected: stopped")
	h.run(h.confirmation())
	ctx := context.Background()

	bare := newTestScheduler(t)
	id, err := QueueOwedInitialSync(ctx, h.st, bare)
	if err == nil || id != "" || !errors.Is(err, ErrJobTypeNotRegistered) {
		t.Fatalf("QueueOwedInitialSync with no sync job registered = %q, %v, want ErrJobTypeNotRegistered", id, err)
	}
	h.assertOwed("after a refused queue", true)

	s := h.restart()
	if err := s.EnterMaintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if id, err := QueueOwedInitialSync(ctx, h.st, s); !errors.Is(err, ErrMaintenanceMode) || id != "" {
		t.Fatalf("QueueOwedInitialSync in maintenance mode = %q, %v, want ErrMaintenanceMode", id, err)
	}
	h.assertOwed("after a start in maintenance mode", true)
	s.ExitMaintenance()

	id, err = QueueOwedInitialSync(ctx, h.st, s)
	if err != nil || id == "" {
		t.Fatalf("the next start: %q, %v", id, err)
	}
	h.assertOwed("after the retry", false)
}

// A record that cannot be read is not an array that owes nothing.
func TestMigrationParity_AnUnreadableRecordIsNotNothingOwed(t *testing.T) {
	h := newParityHarness(t)
	db := newTestDB(t)
	st := store.NewArrayStore(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := h.restart()
	if id, err := QueueOwedInitialSync(context.Background(), st, s); err == nil || id != "" {
		t.Errorf("QueueOwedInitialSync over a closed database = %q, %v, want an error and no sync", id, err)
	}
}

// Parity built another way ends what is owed with no second sync: a person who
// started the sync themselves after a failed queue is not given another by a
// later restart. A dry run, and a sync that did not succeed, build nothing.
func TestMigrationParity_ASyncAPersonRanEndsWhatIsOwed(t *testing.T) {
	ctx := context.Background()
	submit := func(s *Scheduler, p SyncParams) *Job {
		t.Helper()
		body, _ := json.Marshal(p)
		j, err := s.Submit(ctx, TypeSync, nil, body)
		if err != nil {
			t.Fatal(err)
		}
		done, err := s.Await(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		return done
	}
	owed := func(t *testing.T) (*parityHarness, *Scheduler) {
		h := newParityHarness(t)
		h.syncErr = errors.New("injected: stopped")
		h.run(h.confirmation())
		return h, h.restart()
	}

	t.Run("a succeeded sync", func(t *testing.T) {
		h, s := owed(t)
		if done := submit(s, SyncParams{}); done.Status != StatusSucceeded {
			t.Fatalf("sync ended %s", done.Status)
		}
		if id, err := QueueOwedInitialSync(ctx, h.st, s); err != nil || id != "" {
			t.Errorf("QueueOwedInitialSync = %q, %v, want no second sync", id, err)
		}
		h.assertOwed("after a sync parity was built by", false)
	})
	t.Run("a dry run", func(t *testing.T) {
		h, s := owed(t)
		if done := submit(s, SyncParams{DryRun: true}); done.Status != StatusSucceeded {
			t.Fatalf("dry run ended %s", done.Status)
		}
		if id, err := QueueOwedInitialSync(ctx, h.st, s); err != nil || id == "" {
			t.Errorf("QueueOwedInitialSync = %q, %v, want the sync queued: a dry run builds no parity", id, err)
		}
	})
	t.Run("a blocked sync", func(t *testing.T) {
		h, s := owed(t)
		h.engine.ScriptGuardBlock(parity.GuardResult{Blocked: true, Triggers: []parity.GuardTrigger{parity.TriggerZeroFiles}})
		if done := submit(s, SyncParams{}); done.Status != StatusFailed {
			t.Fatalf("sync ended %s", done.Status)
		}
		if id, err := QueueOwedInitialSync(ctx, h.st, s); err != nil || id == "" {
			t.Errorf("QueueOwedInitialSync = %q, %v, want the sync queued: a sync the guard stopped builds no parity", id, err)
		}
	})
}
