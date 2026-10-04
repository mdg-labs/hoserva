//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host: it is built with `go test -tags lab -c` from the host (compiling touches
// no device) and the binary is run inside the lab container.
//
// It is the data-loss scenario of the Unraid import (doc 05 §5): the data disks
// are the user's data and, with the Unraid stick, the rollback. "Re-attach the
// stick and the array returns unchanged" rests on nothing having written to any
// source disk, and no agent runs Unraid (D20), so the test takes the whole-device
// sha256 of every source disk (data, parity and cache), runs the scan and the
// import through the job scheduler as hoservad does, reads every file through
// /mnt/user against the fixture's manifest, stops and starts the array through
// ArraySequence, and asserts every device is byte-identical. Control arms show
// the harness sees what it must: a read-write mount of a copy changes the device
// and is reported read-write.
//
// The lab's device cgroup lets a container open only loop devices, never the
// partition nodes the kernel makes of one, so each filesystem sits on a second
// loop device over the same image at the partition's offset (attachFixture), and
// a /dev/disk/by-id link for it is made here by hand, as udev makes one for a
// real disk's partition: the import binds each mount to that link, never to the
// filesystem UUID.

package migrate

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	cfggen "github.com/mdg-labs/hoserva/internal/config"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// recordingRunner is the real runner, remembering every argv it ran.
type recordingRunner struct {
	mu    sync.Mutex
	inner disk.Runner
	calls [][]string
}

func (r *recordingRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	return r.inner.Run(ctx, name, args...)
}

// mentioning returns every command that names dev (or a by-id link resolving to
// it) in its argv.
func (r *recordingRunner) mentioning(dev string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		for _, a := range c[1:] {
			if a == dev {
				out = append(out, c)
				break
			}
			if strings.HasPrefix(a, "/dev/disk/by-id/") {
				if real, err := filepath.EvalSymlinks(a); err == nil && real == dev {
					out = append(out, c)
					break
				}
			}
		}
	}
	return out
}

// labMount is one line of the kernel's mount table.
type labMount struct {
	point, options, fstype, source, super string
}

func labMounts(t *testing.T) []labMount {
	t.Helper()
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []labMount
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, fl := range fields {
			if fl == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || len(fields) < sep+4 {
			continue
		}
		out = append(out, labMount{point: fields[4], options: fields[5], fstype: fields[sep+1], source: fields[sep+2], super: fields[sep+3]})
	}
	return out
}

func labMountAt(t *testing.T, where string) (labMount, bool) {
	t.Helper()
	var found labMount
	ok := false
	for _, m := range labMounts(t) {
		if m.point == where {
			found, ok = m, true
		}
	}
	return found, ok
}

// readOnly is whether the mount, and for a block filesystem its superblock,
// is read-only.
func (m labMount) readOnly() bool {
	if strings.HasPrefix(m.fstype, "fuse") {
		return hasOpt(m.options, "ro")
	}
	return hasOpt(m.options, "ro") && hasOpt(m.super, "ro")
}

// assertNoSourceMounted fails when the kernel lists any of devs as a mount's
// source.
func assertNoSourceMounted(t *testing.T, what string, devs ...string) {
	t.Helper()
	for _, m := range labMounts(t) {
		for _, d := range devs {
			if m.source == d {
				t.Errorf("%s: %s is mounted at %s", what, d, m.point)
			}
		}
	}
}

// labInventory is this lab's disks as udev would list them once attached: each
// with a by-id link for its partition, as every real disk has. The links are
// made in the lab container's own /dev and removed with the test.
func labInventory(t *testing.T, a *labArray, withLinks bool, slots ...string) *disk.FakeProvider {
	t.Helper()
	p := disk.NewFakeProvider()
	if withLinks {
		if err := os.MkdirAll("/dev/disk/by-id", 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range slots {
		s := a.slot(name)
		d := disk.Disk{Serial: s.id, Size: s.size, Filesystem: s.fs, FSDevice: s.part, FSUUID: s.uuid}
		if withLinks {
			byID := "ata-LABDISK_" + name
			link := "/dev/disk/by-id/" + byID + "-part1"
			_ = os.Remove(link)
			if err := os.Symlink(s.part, link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(link) })
			d.ByIDName, d.FSByIDName = byID, byID+"-part1"
		}
		p.AddDisk(s.whole, d)
	}
	return p
}

// labImport is the daemon's part of the import, built the way hoservad builds
// it, over this lab's disks.
type labImport struct {
	t        *testing.T
	a        *labArray
	rr       *recordingRunner
	svc      *Service
	arrays   *store.ArrayStore
	genRoot  string
	sched    *job.Scheduler
	registry *job.Registry

	mu  sync.Mutex
	seq *job.ArraySequence
}

// failingMounter fails the mount of one mountpoint and delegates the rest.
type failingMounter struct {
	disk.UnitMounter
	failAt string
}

func (f failingMounter) Mount(ctx context.Context, u disk.MountUnit) error {
	if u.Where == f.failAt {
		return errors.New("injected: the mount failed")
	}
	return f.UnitMounter.Mount(ctx, u)
}

func newLabImport(t *testing.T, a *labArray, mounter func(disk.UnitMounter) disk.UnitMounter) *labImport {
	t.Helper()
	sessions, db := newSessionsDB(t)
	_ = sessions
	li := &labImport{t: t, a: a, rr: &recordingRunner{inner: a.runner}, arrays: store.NewArrayStore(db), genRoot: t.TempDir()}
	sc := a.scanner(t, nil)
	li.svc = &Service{Dir: filepath.Join(t.TempDir(), "migrate"), Scanner: sc, Sessions: sessions}
	li.svc.Pending = li.arrays.MigrationPending

	gen := cfggen.NewGenerator(li.genRoot)
	gen.StoppedFlagPath = filepath.Join(li.genRoot, "array-stopped")
	var um disk.UnitMounter = disk.DirectMounter{Runner: li.rr}
	if mounter != nil {
		um = mounter(um)
	}
	registry := job.NewRegistry()
	registry.Register(job.TypeSync, false, func(context.Context, *job.RunContext) error { return nil })
	registry.Register(job.TypeMigrationImport, false, job.RunMigrationImport(job.MigrationImportDeps{
		Plan: func(ctx context.Context, assignments []disk.AdoptionAssignment) (disk.AdoptionPlan, error) {
			p, err := li.svc.PlanImport(ctx, assignments)
			if err != nil {
				return disk.AdoptionPlan{}, err
			}
			return p.Plan, nil
		},
		Runner:     li.rr,
		Store:      li.arrays,
		Generator:  gen,
		Mounter:    um,
		ArrayReady: li.rebuild,
		Array:      li.current,
	}))
	li.registry = registry
	li.sched = job.NewScheduler(job.NewStore(db), job.NewLogStore(t.TempDir()), job.NewHub(), registry)
	li.sched.SetMigrationPending(li.arrays.MigrationPending)
	t.Cleanup(li.cleanup)
	return li
}

func (li *labImport) current() *job.ArraySequence {
	li.mu.Lock()
	defer li.mu.Unlock()
	return li.seq
}

// rebuild is what hoservad's array sequence builder does for a pending array:
// read-only, device-bound disk units and a read-only catch-all.
func (li *labImport) rebuild(ctx context.Context) error {
	settings, disks, err := li.arrays.GetArray(ctx)
	li.mu.Lock()
	defer li.mu.Unlock()
	if errors.Is(err, store.ErrNoArray) {
		li.seq = nil
		return nil
	}
	if err != nil {
		return err
	}
	units, err := job.ArrayMountUnits(settings, disks)
	if err != nil {
		return err
	}
	var mounts []job.ArrayMount
	var data []string
	for _, u := range units {
		mounts = append(mounts, disk.DirectMountController{Unit: u, Runner: li.rr})
		data = append(data, u.Where)
	}
	opts := pool.Options{MinFreeSpace: "50M", Responsiveness: pool.Responsive}
	var catchAll pool.Mount
	if settings.MigrationPending {
		catchAll, err = pool.CatchAllMountReadOnly(data, opts)
	} else {
		catchAll, err = pool.CatchAllMount(data, opts)
	}
	if err != nil {
		return err
	}
	li.seq = &job.ArraySequence{
		Disks:     mounts,
		DiskCheck: job.ArrayDiskUUIDCheck{Mounts: disk.KernelMounts{Runner: li.rr}, Disks: units},
		CatchAll:  pool.MountController{Mnt: catchAll, Mounter: pool.Mounter{Runner: li.rr}},
	}
	return nil
}

// cleanup unmounts whatever a test left, pool first, and removes the mountpoints
// the import made.
func (li *labImport) cleanup() {
	ctx := context.Background()
	_, _ = li.rr.inner.Run(ctx, "fusermount", "-u", "/mnt/user")
	for _, d := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"} {
		_, _ = li.rr.inner.Run(ctx, "umount", d)
	}
	for _, d := range []string{"/mnt/user", "/mnt/disk1", "/mnt/disk2", "/mnt/disk3"} {
		_ = os.Remove(d)
	}
}

// scanZip scans the flash backup zip into the session, as the scan job does.
func (li *labImport) scanZip(data []byte) {
	li.t.Helper()
	if err := scanNow(li.svc, data, ScanOptions{}); err != nil {
		li.t.Fatalf("scan: %v", err)
	}
}

// proposedRoles is the mapping the CLI pre-fills from the scan's disk table.
func (li *labImport) proposedRoles() []disk.AdoptionAssignment {
	li.t.Helper()
	st, err := li.svc.State(context.Background())
	if err != nil || st.Report == nil || st.Report.Review == nil {
		li.t.Fatalf("State = %+v, %v", st, err)
	}
	var out []disk.AdoptionAssignment
	for _, d := range st.Report.Review.Disks {
		switch d.ProposedRole {
		case ProposeParity, ProposeData, ProposeCache:
			out = append(out, disk.AdoptionAssignment{Role: disk.AdoptionRole(d.ProposedRole), Serial: d.Serial})
		}
	}
	return out
}

// runImport queues the import as the API does and waits for the job.
func (li *labImport) runImport(assignments []disk.AdoptionAssignment) *job.Job {
	li.t.Helper()
	ctx := context.Background()
	ip, err := li.svc.PlanImport(ctx, assignments)
	if err != nil {
		li.t.Fatalf("PlanImport: %v", err)
	}
	body, err := json.Marshal(job.MigrationImportParams{Assignments: ip.Assignments, Plan: ip.Plan})
	if err != nil {
		li.t.Fatal(err)
	}
	j, err := li.sched.Submit(ctx, job.TypeMigrationImport, []string{JobResource}, body)
	if err != nil {
		li.t.Fatalf("Submit: %v", err)
	}
	done, err := li.sched.Await(ctx, j.ID)
	if err != nil {
		li.t.Fatalf("Await: %v", err)
	}
	return done
}

// assertPoolMatchesManifest reads every file through the pool at /mnt/user and
// compares it to what the fixture's builder recorded on the data disks: each
// path has the sha256 of one of its copies, and the pool lists no file the
// manifest does not.
func assertPoolMatchesManifest(t *testing.T, manifest map[string]map[string]manifestFile, dataSlots []string, minFiles int, when string) {
	t.Helper()
	want := map[string]map[string]bool{}
	for _, slot := range dataSlots {
		for path, mf := range manifest[slot] {
			if want[path] == nil {
				want[path] = map[string]bool{}
			}
			want[path][mf.sha] = true
		}
	}
	seen := 0
	err := filepath.WalkDir("/mnt/user", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel("/mnt/user", p)
		shas, ok := want[filepath.ToSlash(rel)]
		if !ok {
			t.Errorf("%s: the pool lists %s, which no data disk of the manifest holds", when, rel)
			return nil
		}
		got := sha256File(t, p)
		if !shas[got] {
			t.Errorf("%s: %s reads as sha256 %s through the pool, which none of its copies on the data disks has", when, rel, got)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walking /mnt/user: %v", when, err)
	}
	if seen != len(want) {
		t.Errorf("%s: the pool lists %d files, the manifest %d", when, seen, len(want))
	}
	if seen < minFiles {
		t.Errorf("%s: only %d files were compared, want at least %d", when, seen, minFiles)
	}
}

// assertWritesRefused shows nothing can write through the pool or to a disk
// branch while the migration is pending.
func assertWritesRefused(t *testing.T, disks ...string) {
	t.Helper()
	for _, dir := range append([]string{"/mnt/user"}, disks...) {
		p := filepath.Join(dir, "written-by-the-test")
		if err := os.WriteFile(p, []byte("x"), 0o644); err == nil {
			_ = os.Remove(p)
			t.Errorf("a write to %s succeeded: the adopted array is not read-only", dir)
		}
	}
}

func readFileOrFail(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestLabImport_AdoptsReadOnlyAndNeverWritesASourceDisk is the rollback
// guarantee of doc 05 §5. It scans #74's primary fixture, imports it, reads the
// whole array back through the pool, stops and starts the array, and compares
// the whole-device sha256 of the data disks, the parity disk (which carries the
// XOR-produced XFS signature) and the cache device with what they were before.
func TestLabImport_AdoptsReadOnlyAndNeverWritesASourceDisk(t *testing.T) {
	ctx := context.Background()
	a := attachFixture(t, primary)
	before := a.hashes()
	parity, cache := a.slot("parity"), a.slot("cache")
	if parity.fs == "" {
		t.Fatalf("the fixture's parity disk reports no filesystem; it should carry the XOR artefact")
	}
	a.disks = labInventory(t, a, true, "parity", "disk1", "disk2", "disk3", "cache")
	li := newLabImport(t, a, nil)
	zipData := readFileOrFail(t, a.zip)
	li.scanZip(zipData)
	a.assertUnchanged(before, "after the scan")

	assignments := li.proposedRoles()
	var data, parities, caches int
	for _, as := range assignments {
		switch as.Role {
		case disk.AdoptData:
			data++
		case disk.AdoptParity:
			parities++
		case disk.AdoptCache:
			caches++
		}
	}
	if data != 3 || parities != 1 || caches != 1 {
		t.Fatalf("the scan proposes %d data, %d parity and %d cache disks (%+v), want 3, 1 and 1", data, parities, caches, assignments)
	}

	done := li.runImport(assignments)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("the import job ended %s: %s", done.Status, done.ErrorMessage)
	}
	a.assertUnchanged(before, "after the import")

	// Every data disk is mounted read-only from its own partition, the pool is
	// read-only over them, and neither the parity disk nor the cache device was
	// mounted or opened by a mount.
	for i, slot := range []string{"disk1", "disk2", "disk3"} {
		m, ok := labMountAt(t, fmt.Sprintf("/mnt/disk%d", i+1))
		if !ok || m.source != a.slot(slot).part || !m.readOnly() {
			t.Errorf("/mnt/disk%d = %+v (mounted %v), want %s mounted read-only", i+1, m, ok, a.slot(slot).part)
		}
		if !hasOpt(m.options, "ro") || !strings.Contains(m.fstype, "xfs") {
			t.Errorf("/mnt/disk%d is %s with %s", i+1, m.fstype, m.options)
		}
	}
	if m, ok := labMountAt(t, "/mnt/user"); !ok || !m.readOnly() || m.fstype != "fuse.mergerfs" {
		t.Errorf("/mnt/user = %+v (mounted %v), want a read-only mergerfs pool", m, ok)
	}
	assertNoSourceMounted(t, "after the import", parity.part, parity.whole, cache.part, cache.whole)
	for _, dev := range []string{parity.part, parity.whole, cache.part, cache.whole} {
		if calls := li.rr.mentioning(dev); len(calls) > 0 {
			for _, c := range calls {
				if c[0] == "mount" {
					t.Errorf("%s was passed to %v", dev, c)
				}
			}
		}
	}
	for _, c := range li.rr.calls {
		if c[0] == "mount" {
			opts := ""
			for i, arg := range c {
				if arg == "-o" && i+1 < len(c) {
					opts = c[i+1]
				}
			}
			if !hasOpt(opts, "ro") {
				t.Errorf("a mount was not read-only: %v", c)
			}
		}
		if strings.HasPrefix(c[0], "mkfs") || c[0] == "wipefs" || c[0] == "sgdisk" || c[0] == "parted" {
			t.Errorf("the import ran %v", c)
		}
	}

	// The pending state is what the design says: no snapraid.conf, the read-only
	// units, the parity and cache disks recorded and nothing more.
	_ = filepath.WalkDir(li.genRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "snapraid") {
			t.Errorf("an import generated %s: no snapraid.conf may exist before the point of no return", p)
		}
		return nil
	})
	settings, disks, err := li.arrays.GetArray(ctx)
	if err != nil || !settings.MigrationPending || len(disks) != 3 {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	recorded, err := li.arrays.RecordedDisks(ctx)
	if err != nil || len(recorded) != 2 || recorded[0].Role != store.ArrayRoleParity || recorded[1].Role != store.ArrayRoleCache {
		t.Fatalf("recorded = %+v, %v", recorded, err)
	}
	unit := readFileOrFail(t, filepath.Join(li.genRoot, "systemd", "system", "mnt-disk1.mount"))
	if !bytes.Contains(unit, []byte("Options=ro,norecovery,")) || !bytes.Contains(unit, []byte("What=/dev/disk/by-id/ata-LABDISK_")) {
		t.Errorf("mnt-disk1.mount:\n%s", unit)
	}
	pending, err := li.arrays.MigrationPending(ctx)
	if err != nil || !pending {
		t.Errorf("MigrationPending = %v, %v", pending, err)
	}
	// While it is pending a sync cannot even be queued.
	if _, err := li.sched.Submit(ctx, job.TypeSync, nil, nil); !errors.Is(err, job.ErrMigrationInProgress) {
		t.Errorf("a sync submitted while the import is pending = %v, want the migration_in_progress refusal", err)
	}

	manifest := readManifest(t, a.dir)
	dataSlots := []string{"disk1", "disk2", "disk3"}
	assertPoolMatchesManifest(t, manifest, dataSlots, 20, "after the import")
	assertWritesRefused(t, "/mnt/disk1", "/mnt/disk2", "/mnt/disk3")

	// Stop and start the array through ArraySequence, as `hoserva array stop` and
	// `start` do: the disks come back read-only from the same devices.
	seq := li.current()
	if seq == nil {
		t.Fatal("the import left no array sequence")
	}
	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, where := range []string{"/mnt/user", "/mnt/disk1", "/mnt/disk2", "/mnt/disk3"} {
		if _, ok := labMountAt(t, where); ok {
			t.Errorf("%s is still mounted after Stop", where)
		}
	}
	a.assertUnchanged(before, "after stopping the array")
	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i, slot := range dataSlots {
		if m, ok := labMountAt(t, fmt.Sprintf("/mnt/disk%d", i+1)); !ok || m.source != a.slot(slot).part || !m.readOnly() {
			t.Errorf("after Start, /mnt/disk%d = %+v (mounted %v)", i+1, m, ok)
		}
	}
	if m, ok := labMountAt(t, "/mnt/user"); !ok || !m.readOnly() {
		t.Errorf("after Start, /mnt/user = %+v (mounted %v), want a read-only pool", m, ok)
	}
	assertPoolMatchesManifest(t, manifest, dataSlots, 20, "after the array was stopped and started")
	assertWritesRefused(t, "/mnt/disk1")
	assertNoSourceMounted(t, "after Start", parity.part, parity.whole, cache.part, cache.whole)
	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, where := range []string{"/mnt/user", "/mnt/disk1", "/mnt/disk2", "/mnt/disk3"} {
		if _, ok := labMountAt(t, where); ok {
			t.Errorf("%s is still mounted after the array was stopped again", where)
		}
	}
	a.assertUnchanged(before, "after the array was stopped again")

	// The control arm: the same harness sees a read-write mount. It runs on a
	// copy of a data disk, never on the fixture's own image, and a plain
	// read-write XFS mount of it changes the device and is reported read-write.
	rr := disk.CommandRunner{}
	d1 := a.slot("disk1")
	copyImg := filepath.Join(labDir(t), "control-76.img")
	if out, err := rr.Run(ctx, "cp", "--sparse=always", d1.img, copyImg); err != nil {
		t.Fatalf("copying %s: %v %s", d1.img, err, out)
	}
	t.Cleanup(func() { _ = os.Remove(copyImg) })
	g := a.geometry(d1)
	cwhole := attachLoop(ctx, t, rr, copyImg, 0, 0)
	cpart := attachLoop(ctx, t, rr, copyImg, g[0]*512, g[1]*512)
	cwhere := filepath.Join(labDir(t), "control-76")
	if err := os.MkdirAll(cwhere, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cwhere) })
	controlBefore := sha256File(t, cwhole)
	if controlBefore != before["disk1"] {
		t.Fatalf("the copy's sha256 is %s, the original's %s", controlBefore, before["disk1"])
	}
	if out, err := rr.Run(ctx, "mount", "-t", "xfs", cpart, cwhere); err != nil {
		t.Fatalf("control: mounting read-write: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = rr.Run(context.Background(), "umount", cwhere) })
	if m, ok := labMountAt(t, resolved(t, cwhere)); !ok || m.readOnly() {
		t.Errorf("control: a read-write mount is reported %+v (mounted %v) as read-only", m, ok)
	}
	if _, err := rr.Run(ctx, "umount", cwhere); err != nil {
		t.Fatalf("control: unmounting: %v", err)
	}
	if got := sha256File(t, cwhole); got == controlBefore {
		t.Errorf("control: a read-write XFS mount left the device's sha256 unchanged (%s): this harness would not have seen an import write to a source disk", got)
	}
}

// A failure part-way through mounting the data disks leaves no pool, no record of
// the array and nothing mounted, and no byte of any source disk written: what was
// mounted is unmounted, the record deleted and the generated units removed.
func TestLabImport_AFailedMountLeavesNoPartialPoolAndNoRecord(t *testing.T) {
	ctx := context.Background()
	a := attachFixture(t, primary)
	before := a.hashes()
	a.disks = labInventory(t, a, true, "parity", "disk1", "disk2", "disk3", "cache")
	li := newLabImport(t, a, func(inner disk.UnitMounter) disk.UnitMounter {
		return failingMounter{UnitMounter: inner, failAt: "/mnt/disk3"}
	})
	li.scanZip(readFileOrFail(t, a.zip))
	done := li.runImport(li.proposedRoles())
	if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "injected: the mount failed") {
		t.Fatalf("the import job ended %s: %q, want it failed by the injected mount failure", done.Status, done.ErrorMessage)
	}
	for _, where := range []string{"/mnt/user", "/mnt/disk1", "/mnt/disk2", "/mnt/disk3"} {
		if _, ok := labMountAt(t, where); ok {
			t.Errorf("%s is still mounted after the failed import", where)
		}
	}
	if exists, err := li.arrays.Exists(ctx); err != nil || exists {
		t.Errorf("array record exists = %v, %v after the failed import", exists, err)
	}
	if recorded, err := li.arrays.RecordedDisks(ctx); err != nil || len(recorded) != 0 {
		t.Errorf("recorded disks = %+v, %v after the failed import", recorded, err)
	}
	units, _ := filepath.Glob(filepath.Join(li.genRoot, "systemd", "system", "mnt-*.mount"))
	if len(units) != 0 {
		t.Errorf("the failed import left units %v", units)
	}
	a.assertUnchanged(before, "after the failed import")
	st, err := li.svc.State(ctx)
	if err != nil || st.Phase != PhaseScanned {
		t.Errorf("session phase = %v, %v, want scanned", st.Phase, err)
	}
}

// copyRange copies n bytes from src at srcOff to dst at dstOff.
func copyRange(t *testing.T, src string, srcOff int64, dst string, dstOff, n int64) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(io.NewOffsetWriter(out, dstOff), io.NewSectionReader(in, srcOff, n)); err != nil {
		t.Fatal(err)
	}
	if err := out.Sync(); err != nil {
		t.Fatal(err)
	}
}

// zipWith returns the zip at path with the named entries replaced.
func zipWith(t *testing.T, path string, replace map[string]string) []byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range zr.File {
		w, err := zw.Create(e.Name)
		if err != nil {
			t.Fatal(err)
		}
		if content, ok := replace[e.Name]; ok {
			_, _ = io.WriteString(w, content)
			continue
		}
		rc, err := e.Open()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, rc); err != nil {
			t.Fatal(err)
		}
		_ = rc.Close()
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// oneDataDiskArray is #74's primary fixture cut down to a one-data-disk array
// with single parity, as Unraid lays it out: parity is the bytewise XOR of the
// data disks' partitions, which for one disk is a copy of it, so the parity
// partition carries the data disk's XFS superblock and with it its filesystem
// UUID. The two disks are copies of the fixture's own images; the parity copy
// has the data partition's first 64 MiB (the window the builder XORs) written
// over its own partition.
func oneDataDiskArray(t *testing.T) (*labArray, []byte) {
	t.Helper()
	ctx := context.Background()
	rr := disk.CommandRunner{}
	src := labFixture(t, primary)
	scratch := filepath.Join(labDir(t), "one-data-disk-76")
	_ = os.RemoveAll(scratch)
	if err := os.MkdirAll(filepath.Join(scratch, "img"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	for _, sub := range []string{"expected"} {
		if out, err := rr.Run(ctx, "cp", "-r", filepath.Join(src, sub), scratch); err != nil {
			t.Fatalf("copying %s: %v %s", sub, err, out)
		}
	}
	if out, err := rr.Run(ctx, "cp", "-r", filepath.Join(src, "flash"), scratch); err != nil {
		t.Fatalf("copying the flash: %v %s", err, out)
	}
	for _, slot := range []string{"parity", "disk1"} {
		if out, err := rr.Run(ctx, "cp", "--sparse=always", filepath.Join(src, "img", slot+".img"), filepath.Join(scratch, "img", slot+".img")); err != nil {
			t.Fatalf("copying %s: %v %s", slot, err, out)
		}
	}
	probeA := &labArray{t: t, dir: src}
	gp, gd := probeA.geometry(&labSlot{slot: "parity"}), probeA.geometry(&labSlot{slot: "disk1"})
	const window = 64 << 20
	copyRange(t, filepath.Join(scratch, "img", "disk1.img"), gd[0]*512, filepath.Join(scratch, "img", "parity.img"), gp[0]*512, window)

	// Attach the two disks the way attachFixture does, from the cut-down
	// directory and a flash backup that names only them.
	srcFlash, err := OpenDir(filepath.Join(scratch, "flash"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ReadFlash(srcFlash)
	_ = srcFlash.Close()
	if err != nil {
		t.Fatal(err)
	}
	var ini strings.Builder
	a := &labArray{t: t, dir: scratch, disks: disk.NewFakeProvider(), runner: rr, flash: f}
	for _, s := range f.Slots {
		if s.Name != "parity" && s.Name != "disk1" {
			continue
		}
		fmt.Fprintf(&ini, "[\"%s\"]\nidx=\"%d\"\nname=\"%s\"\nid=\"%s\"\nsize=\"%d\"\nstatus=\"DISK_OK\"\ntype=\"%s\"\nfsType=\"%s\"\n", s.Name, s.Index, s.Name, s.ID, s.SizeKiB, s.Type, s.FsType)
		g := gp
		if s.Name == "disk1" {
			g = gd
		}
		img := filepath.Join(scratch, "img", s.Name+".img")
		st, err := os.Stat(img)
		if err != nil {
			t.Fatal(err)
		}
		ls := &labSlot{slot: s.Name, id: s.ID, img: img, size: st.Size()}
		ls.whole = attachLoop(ctx, t, rr, img, 0, 0)
		ls.part = attachLoop(ctx, t, rr, img, g[0]*512, g[1]*512)
		props := probe(t, rr, ls.part)
		ls.fs, ls.uuid = props["TYPE"], props["UUID"]
		a.slots = append(a.slots, ls)
	}
	if len(a.slots) != 2 {
		t.Fatalf("attached %d disks, want the parity disk and disk1", len(a.slots))
	}
	if a.slot("parity").uuid == "" || a.slot("parity").uuid != a.slot("disk1").uuid {
		t.Fatalf("the parity partition's UUID is %q and the data disk's %q: the one-data-disk layout (parity holds a copy of the filesystem) was not built", a.slot("parity").uuid, a.slot("disk1").uuid)
	}
	zipData := zipWith(t, filepath.Join(src, "expected", "flash-backup.zip"), map[string]string{"config/hoserva/disks.ini": ini.String()})
	return a, zipData
}

// With one data disk, Unraid's single parity is a copy of it, so the parity
// partition carries the data disk's filesystem UUID. The data disk is adopted
// through its own device (a by-id link), the former parity disk is never mounted
// in its place or opened by a mount, and no source disk is written.
func TestLabImport_OneDataDiskNeverMountsItsParityCopy(t *testing.T) {
	ctx := context.Background()
	a, zipData := oneDataDiskArray(t)
	parity, data := a.slot("parity"), a.slot("disk1")
	before := a.hashes()
	a.disks = labInventory(t, a, true, "parity", "disk1")
	li := newLabImport(t, a, nil)
	li.scanZip(zipData)
	a.assertUnchanged(before, "after the scan")

	st, err := li.svc.State(ctx)
	if err != nil || st.Report.Verdict == VerdictNoGo {
		t.Fatalf("the scan of a one-data-disk array is no-go: %v %+v", err, st.Report.Rows)
	}
	done := li.runImport(li.proposedRoles())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("the import job ended %s: %s", done.Status, done.ErrorMessage)
	}

	m, ok := labMountAt(t, "/mnt/disk1")
	if !ok || m.source != data.part || !m.readOnly() {
		t.Errorf("/mnt/disk1 = %+v (mounted %v), want %s mounted read-only", m, ok, data.part)
	}
	assertNoSourceMounted(t, "after the import", parity.part, parity.whole)
	for _, dev := range []string{parity.part, parity.whole} {
		for _, c := range li.rr.mentioning(dev) {
			if c[0] == "mount" {
				t.Errorf("the parity device %s was passed to %v", dev, c)
			}
		}
	}
	for _, c := range li.rr.calls {
		if c[0] == "mount" && strings.Contains(strings.Join(c, " "), "-U") {
			t.Errorf("a mount looked a filesystem up by UUID, which the parity copy shares: %v", c)
		}
	}
	a.assertUnchanged(before, "after the import")
	manifest := readManifest(t, labFixture(t, primary))
	assertPoolMatchesManifest(t, manifest, []string{"disk1"}, 8, "after the import")

	seq := li.current()
	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := seq.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m, ok := labMountAt(t, "/mnt/disk1"); !ok || m.source != data.part {
		t.Errorf("after Start, /mnt/disk1 = %+v (mounted %v), want %s", m, ok, data.part)
	}
	assertNoSourceMounted(t, "after Start", parity.part, parity.whole)
	if err := seq.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	a.assertUnchanged(before, "after stopping the array")
}

// Without a by-id link nothing but the UUID could tell the data disk from the
// parity copy, so the scan refuses the data disk and no import is possible; the
// refusal holds for a mapping built by hand too.
func TestLabImport_OneDataDiskWithoutAnIdentityLinkIsRefused(t *testing.T) {
	ctx := context.Background()
	a, zipData := oneDataDiskArray(t)
	before := a.hashes()
	a.disks = labInventory(t, a, false, "parity", "disk1")
	li := newLabImport(t, a, nil)
	li.scanZip(zipData)
	st, err := li.svc.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var refused bool
	for _, d := range st.Report.Review.Disks {
		if d.Slot == "disk1" && d.Refused && d.RefusalCode == RefuseDuplicateUUID {
			refused = true
		}
	}
	if !refused || st.Report.Verdict != VerdictNoGo {
		t.Fatalf("verdict %s, disk1 refused as duplicate_uuid = %v: %+v", st.Report.Verdict, refused, st.Report.Rows)
	}
	_, err = li.svc.PlanImport(ctx, []disk.AdoptionAssignment{
		{Role: disk.AdoptParity, Serial: a.slot("parity").id},
		{Role: disk.AdoptData, Serial: a.slot("disk1").id},
	})
	if !errors.Is(err, ErrImportNoGo) {
		t.Errorf("PlanImport = %v, want the no-go refusal", err)
	}
	listed, _ := a.disks.List(ctx)
	_, err = disk.ResolveAdoption(listed, []disk.AdoptionAssignment{
		{Role: disk.AdoptParity, Serial: a.slot("parity").id},
		{Role: disk.AdoptData, Serial: a.slot("disk1").id},
	})
	if !errors.Is(err, disk.ErrAdoptUUIDShared) {
		t.Errorf("ResolveAdoption = %v, want the shared-UUID refusal", err)
	}
	assertNoSourceMounted(t, "after the refusal", a.slot("parity").part, a.slot("disk1").part)
	a.assertUnchanged(before, "after the refusals")
}
