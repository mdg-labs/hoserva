//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host. It is the data-loss scenario of the point of no return (doc 05 §4 step
// 17), the first step of the migration that formats anything: the former parity
// disk and the cache are erased, and the data disks are the user's data and,
// until the sync completes, unprotected.
//
// Two cases on #74's primary fixture, scanned, imported read-only and verified as
// hoservad does it. (1) Initialising without a passing verify, after a failed one,
// or with a confirmation that names a data disk refuses, and every source disk is
// byte-identical afterwards: the whole-device sha256 of the data disks, the parity
// disk and the cache is the same as before the attempt. (2) The happy path formats
// only the confirmed parity and cache devices through real mkfs, mounts the data
// disks read-write, generates snapraid.conf, completes the initial sync through
// the threshold guard with a real snapraid, and every data file still matches the
// fixture's manifest.
//
// The lab keeps the fixture's cache image at 256 MiB, below the 300 MiB XFS
// needs: the test grows its copy of the cache image before it is attached. The
// boot copy of the content file is Q18's /var/lib/hoserva/snapraid.content, which
// no test writes: the generated snapraid.conf is asserted to name it and is then
// pointed at the lab's own directory before the sync runs, the way
// cmd/hoservad/parity_registrar_lab_test.go does.

package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/parity"
	"github.com/mdg-labs/hoserva/internal/share"
	"github.com/mdg-labs/hoserva/internal/store"
)

// labFormatProvider lists the lab's disks as udev would and formats a loop device
// with the real mkfs, refusing any device that is not one of the lab's parity or
// cache disks: it is the test's own guard that nothing but the confirmed devices
// is formatted, beside the one the production code has.
type labFormatProvider struct {
	disk.Provider
	run     disk.Runner
	allowed map[string]string
}

func (p labFormatProvider) Format(ctx context.Context, dev string, fs disk.FilesystemType) error {
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return fmt.Errorf("the lab will not format %s: %w", dev, err)
	}
	if _, ok := p.allowed[real]; !ok || fs != disk.XFS {
		return fmt.Errorf("the lab will not format %s (%s) as %s: it is not a confirmed parity or cache device", dev, real, fs)
	}
	_, err = p.run.Run(ctx, "mkfs.xfs", "-f", dev)
	return err
}

type labParity struct {
	*labVerify
	engine   *parity.SnapraidEngine
	confPath string
	bootCopy string
	provider labFormatProvider
	syncID   string
}

// growCache makes the cache image big enough for XFS (it holds a btrfs of 256
// MiB in the fixture); the partition inside it is unchanged.
func growCache(dir string) {
	if err := os.Truncate(filepath.Join(dir, "img", "cache.img"), 384<<20); err != nil {
		panic(err)
	}
}

// newLabParity scans, imports (seeding the shares) and wires the point of no
// return the way hoservad does. The verify is not run: each test decides.
func newLabParity(t *testing.T, change func(a *labArray, manifest map[string]map[string]manifestFile)) *labParity {
	t.Helper()
	lv := newLabVerifyWith(t, labVerifyOptions{prepare: growCache, change: change, seed: true})
	li, a := lv.li, lv.a
	ctx := context.Background()

	if err := os.MkdirAll("/dev/disk/by-id", 0o755); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]string{}
	for _, name := range []string{"parity", "cache"} {
		link := "/dev/disk/by-id/ata-LABDISK_" + name
		_ = os.Remove(link)
		if err := os.Symlink(a.slot(name).whole, link); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		allowed[a.slot(name).whole] = name
	}
	li.svc.Record = func(ctx context.Context) ([]store.ArrayDisk, []store.RecordedDisk, error) {
		_, disks, err := li.arrays.GetArray(ctx)
		if err != nil {
			return nil, nil, err
		}
		recorded, err := li.arrays.RecordedDisks(ctx)
		return disks, recorded, err
	}
	li.svc.Finishing = li.arrays.MigrationFinishing
	li.sched.SetMigrationPending(li.arrays.MigrationUnfinished)
	// The import's own tests seed through a filesystem that refuses every write;
	// past the point of no return the share step writes for real, inside the lab.
	li.shares.FS = share.OSFS{}

	lab := labDir(t)
	work := filepath.Join(lab, "parity-init")
	_ = os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	lp := &labParity{
		labVerify: lv, confPath: filepath.Join(li.genRoot, "snapraid.conf"), bootCopy: filepath.Join(work, "boot.content"),
		provider: labFormatProvider{Provider: a.disks, run: li.rr, allowed: allowed},
	}
	lp.engine = &parity.SnapraidEngine{ConfPath: lp.confPath, LogDir: filepath.Join(work, "logs"), Runner: parity.CommandRunner{}}
	li.syncRun = job.RunSync(lp.engine)

	// What hoservad's ArrayReady hook does for the point of no return, with the
	// one change this lab needs: the boot copy of the content file is pointed at
	// the lab's own directory before any sync runs.
	ready := func(ctx context.Context) error {
		conf, err := os.ReadFile(lp.confPath)
		if err != nil {
			return err
		}
		repointed := strings.Replace(string(conf), "content "+parity.BootContentPath+"\n", "content "+lp.bootCopy+"\n", 1)
		if repointed == string(conf) {
			return fmt.Errorf("snapraid.conf does not place the boot copy of the content file at %s:\n%s", parity.BootContentPath, conf)
		}
		if err := os.WriteFile(lp.confPath, []byte(repointed), 0o644); err != nil {
			return err
		}
		return li.rebuild(ctx)
	}
	li.registry.Register(job.TypeMigrationParity, false, job.RunMigrationParity(job.MigrationParityDeps{
		Plan:       li.svc.PlanParityInit,
		Provider:   lp.provider,
		Runner:     li.rr,
		Store:      li.arrays,
		Generator:  li.gen,
		Mounter:    disk.DirectMounter{Runner: li.rr},
		ArrayReady: ready,
		Array:      li.current,
		Shares: func(ctx context.Context, out io.Writer) error {
			res, err := li.shares.CompleteMigration(ctx)
			_, _ = fmt.Fprintf(out, "shares: %+v\n", res)
			return err
		},
		QueueSync: func(ctx context.Context) (string, error) {
			id, err := job.QueueInitialSync(li.sched)(ctx)
			lp.syncID = id
			return id, err
		},
	}))
	_ = ctx
	return lp
}

// attempt submits the point of no return as a client that skipped the API's own
// checks would, with confirmation: the job's gate and its own confirmation check
// are what is under test.
func (lp *labParity) attempt(confirmation string) *job.Job {
	lp.t.Helper()
	ctx := context.Background()
	body, err := json.Marshal(job.MigrationParityParams{Confirmation: confirmation})
	if err != nil {
		lp.t.Fatal(err)
	}
	j, err := lp.li.sched.Submit(ctx, job.TypeMigrationParity, []string{JobResource}, body)
	if err != nil {
		lp.t.Fatalf("Submit: %v", err)
	}
	done, err := lp.li.sched.Await(ctx, j.ID)
	if err != nil {
		lp.t.Fatalf("Await: %v", err)
	}
	return done
}

func (lp *labParity) assertNothingFormatted(when string) {
	lp.t.Helper()
	for _, c := range lp.li.rr.calls {
		if strings.HasPrefix(c[0], "mkfs") || c[0] == "wipefs" || c[0] == "sgdisk" || c[0] == "parted" {
			lp.t.Errorf("%s: ran %v", when, c)
		}
	}
	// The migration is still pending, with nothing recorded and no snapraid.conf.
	ctx := context.Background()
	if pending, err := lp.li.arrays.MigrationPending(ctx); err != nil || !pending {
		lp.t.Errorf("%s: MigrationPending = %v, %v, want the pending state intact", when, pending, err)
	}
	if _, disks, err := lp.li.arrays.GetArray(ctx); err != nil || len(disks) != 3 {
		lp.t.Errorf("%s: array disks = %+v, %v, want only the three adopted data disks", when, disks, err)
	}
	if _, err := os.Stat(lp.confPath); err == nil {
		lp.t.Errorf("%s: snapraid.conf was generated", when)
	}
	for _, where := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/user"} {
		if m, ok := labMountAt(lp.t, where); !ok || !m.readOnly() {
			lp.t.Errorf("%s: %s = %+v (mounted %v), want it still mounted read-only", when, where, m, ok)
		}
	}
	for _, where := range []string{"/mnt/parity1", "/mnt/cache"} {
		if _, ok := labMountAt(lp.t, where); ok {
			lp.t.Errorf("%s: %s is mounted", when, where)
		}
	}
	lp.a.assertUnchanged(lp.before, when)
}

// A client that skips the API's checks and submits the job directly is refused
// by the job itself, before anything is stopped, unmounted or erased: without a
// verify, after a failed one, and with a passing verify but a confirmation that
// names a data disk. Every source disk is byte-identical afterwards.
func TestLabParity_InitialisingWithoutAPassingVerifyOrTheRightConfirmationErasesNothing(t *testing.T) {
	t.Run("no verify has run", func(t *testing.T) {
		lp := newLabParity(t, nil)
		ctx := context.Background()
		if _, err := lp.li.svc.ExpectedParityConfirmation(ctx); err == nil || !strings.Contains(err.Error(), "no passing verify") {
			t.Fatalf("ExpectedParityConfirmation = %v, want ErrVerifyRequired: the API offers no confirmation without a pass", err)
		}
		// The confirmation a verified array would have, which a client could guess.
		guess := "ERASE " + lp.a.slot("cache").whole + ", " + lp.a.slot("parity").whole
		done := lp.attempt(guess)
		if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "no passing verify") {
			t.Fatalf("the job ended %s: %q, want it refused for want of a verify", done.Status, done.ErrorMessage)
		}
		lp.assertNothingFormatted("a job with no verify")
	})

	t.Run("the latest verify failed", func(t *testing.T) {
		var path string
		lp := newLabParity(t, func(a *labArray, manifest map[string]map[string]manifestFile) {
			probe := &labVerify{t: t, manifest: manifest}
			path = probe.pick("disk1", func(mf manifestFile) bool { return mf.size >= 2 && mf.size <= DefaultSmallFileBytes })
			changeDisk(t, a, "disk1", func(root string) {
				if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
					t.Fatal(err)
				}
			})
		})
		done, res := lp.verify()
		if done.Status != job.StatusFailed || res == nil || res.Status != VerifyFailed {
			t.Fatalf("the verify ended %s (%s): %+v, want it failed on the removed file", done.Status, done.ErrorMessage, res)
		}
		guess := "ERASE " + lp.a.slot("cache").whole + ", " + lp.a.slot("parity").whole
		done = lp.attempt(guess)
		if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "latest verify failed") {
			t.Fatalf("the job ended %s: %q, want it refused after a failed verify", done.Status, done.ErrorMessage)
		}
		lp.assertNothingFormatted("a job after a failed verify")
	})

	t.Run("a passing verify and a confirmation that names a data disk", func(t *testing.T) {
		lp := newLabParity(t, nil)
		if done, res := lp.verify(); done.Status != job.StatusSucceeded || res == nil || res.Status != VerifyPassed {
			t.Fatalf("the verify ended %s (%s): %+v", done.Status, done.ErrorMessage, res)
		}
		ctx := context.Background()
		want, err := lp.li.svc.ExpectedParityConfirmation(ctx)
		if err != nil || !strings.HasPrefix(want, "ERASE ") {
			t.Fatalf("ExpectedParityConfirmation = %q, %v", want, err)
		}
		for _, slot := range []string{"disk1", "disk2", "disk3"} {
			if strings.Contains(want, lp.a.slot(slot).whole) {
				t.Fatalf("the confirmation %q names data disk %s", want, slot)
			}
		}
		done := lp.attempt(want + ", " + lp.a.slot("disk1").whole)
		if done.Status != job.StatusFailed || !strings.Contains(done.ErrorMessage, "typed confirmation") {
			t.Fatalf("the job ended %s: %q, want it refused for its confirmation", done.Status, done.ErrorMessage)
		}
		lp.assertNothingFormatted("a confirmation naming a data disk")
		// A job with no confirmation is not even queued.
		if _, err := lp.li.sched.Submit(ctx, job.TypeMigrationParity, []string{JobResource}, []byte(`{"confirmation":""}`)); err == nil {
			t.Error("a job with an empty confirmation was queued")
		}
		lp.assertNothingFormatted("a job with no confirmation")
	})
}

// The happy path: only the confirmed parity disk and cache are formatted, the
// initial sync completes through the guard, and every data file still matches the
// fixture's manifest.
func TestLabParity_FormatsOnlyTheConfirmedDevicesSyncsThroughTheGuardAndKeepsEveryDataFile(t *testing.T) {
	lp := newLabParity(t, nil)
	ctx := context.Background()
	a, li := lp.a, lp.li
	if done, res := lp.verify(); done.Status != job.StatusSucceeded || res == nil || res.Status != VerifyPassed {
		t.Fatalf("the verify ended %s (%s): %+v", done.Status, done.ErrorMessage, res)
	}
	want, err := li.svc.ExpectedParityConfirmation(ctx)
	if err != nil {
		t.Fatalf("ExpectedParityConfirmation: %v", err)
	}
	if wantStr := "ERASE " + a.slot("cache").whole + ", " + a.slot("parity").whole; want != wantStr {
		t.Fatalf("confirmation = %q, want %q: the parity disk and the cache, no data disk", want, wantStr)
	}
	if info, err := li.svc.ParityInit(ctx); err != nil || info == nil || info.Confirmation != want || len(info.Erases) != 2 {
		t.Fatalf("ParityInit = %+v, %v", info, err)
	}

	manifest := lp.manifest
	dataSlots := []string{"disk1", "disk2", "disk3"}
	// A file under a share keeps its mode and owner: only top-level directories
	// are touched.
	var sample, sampleDisk string
	for _, slot := range dataSlots {
		for p := range manifest[slot] {
			if !hiddenTopLevel(p) && strings.Contains(p, "/") && !strings.HasPrefix(p, "lost+found") {
				sample, sampleDisk = p, slot
			}
		}
	}
	diskDir := map[string]string{"disk1": "/mnt/disk1", "disk2": "/mnt/disk2", "disk3": "/mnt/disk3"}
	statOf := func() (os.FileMode, uint32, uint32) {
		info, err := os.Lstat(filepath.Join(diskDir[sampleDisk], filepath.FromSlash(sample)))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		return info.Mode(), st.Uid, st.Gid
	}
	modeBefore, uidBefore, gidBefore := statOf()

	done := lp.attempt(want)
	if done.Status != job.StatusSucceeded {
		t.Fatalf("the point of no return ended %s: %s", done.Status, done.ErrorMessage)
	}

	// Only the confirmed devices were formatted, through the real mkfs: the parity
	// disk and the cache, as whole disks, and no command named a data disk's device
	// or by-id link.
	var mkfs [][]string
	for _, c := range li.rr.calls {
		if strings.HasPrefix(c[0], "mkfs") {
			mkfs = append(mkfs, c)
		}
	}
	if len(mkfs) != 2 || mkfs[0][0] != "mkfs.xfs" || mkfs[1][0] != "mkfs.xfs" {
		t.Fatalf("mkfs commands = %v, want exactly two XFS formats", mkfs)
	}
	wantDevs := map[string]bool{a.slot("parity").whole: true, a.slot("cache").whole: true}
	for _, c := range mkfs {
		real, err := filepath.EvalSymlinks(c[len(c)-1])
		if err != nil || !wantDevs[real] {
			t.Errorf("%v formatted %s, which is not a confirmed device", c, real)
		}
	}
	for _, slot := range dataSlots {
		for _, dev := range []string{a.slot(slot).whole, a.slot(slot).part} {
			for _, c := range li.rr.mentioning(dev) {
				if strings.HasPrefix(c[0], "mkfs") || c[0] == "wipefs" || c[0] == "sgdisk" || c[0] == "parted" || c[0] == "xfs_repair" && !hasArg(c, "-n") {
					t.Errorf("data disk %s (%s) was passed to %v", slot, dev, c)
				}
			}
		}
	}
	// Control arm: the whole-device hash this file's refusals rely on does see a
	// format, so "unchanged" after a refusal means something.
	after := a.hashes()
	for _, slot := range []string{"parity", "cache"} {
		if after[slot] == lp.before[slot] {
			t.Errorf("%s's whole-device sha256 is the same after its format: the harness would not have seen a refusal fail to protect it", slot)
		}
	}
	for name, dev := range map[string]string{"parity": a.slot("parity").whole, "cache": a.slot("cache").whole} {
		if fsType := probe(t, li.rr, dev)["TYPE"]; fsType != "xfs" {
			t.Errorf("%s (%s) is %q, want xfs", name, dev, fsType)
		}
	}

	// The record: three data disks, the parity disk at /mnt/parity1 and the cache at
	// /mnt/cache with the filesystems just made, no longer pending, and finished.
	settings, disks, err := li.arrays.GetArray(ctx)
	if err != nil || settings.MigrationPending || len(disks) != 5 {
		t.Fatalf("array = %+v %+v, %v", settings, disks, err)
	}
	byMount := map[string]store.ArrayDisk{}
	for _, d := range disks {
		byMount[d.Mountpoint] = d
	}
	for mount, role := range map[string]string{"/mnt/parity1": store.ArrayRoleParity, "/mnt/cache": store.ArrayRoleCache} {
		d := byMount[mount]
		wholeProbe := probe(t, li.rr, a.slot(map[string]string{store.ArrayRoleParity: "parity", store.ArrayRoleCache: "cache"}[role]).whole)
		if d.Role != role || d.FSUUID == "" || !strings.EqualFold(d.FSUUID, wholeProbe["UUID"]) {
			t.Errorf("%s row = %+v, want the filesystem on the device (UUID %s)", mount, d, wholeProbe["UUID"])
		}
	}
	if unfinished, err := li.arrays.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished = %v, %v after the job", unfinished, err)
	}

	// Everything is mounted read-write now, the data disks from their own devices.
	for _, where := range []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/parity1", "/mnt/cache", "/mnt/user"} {
		m, ok := labMountAt(t, where)
		if !ok || hasOpt(m.options, "ro") {
			t.Errorf("%s = %+v (mounted %v), want it mounted read-write", where, m, ok)
		}
	}
	for i, slot := range dataSlots {
		if m, _ := labMountAt(t, diskDir[slot]); m.source != a.slot(slot).part {
			t.Errorf("/mnt/disk%d is mounted from %s, want %s", i+1, m.source, a.slot(slot).part)
		}
	}

	// snapraid.conf: the parity file, Q18's three content copies and every data
	// disk (the boot copy was pointed at the lab's directory after this).
	conf := readFileOrFail(t, lp.confPath)
	for _, line := range []string{"parity /mnt/parity1/snapraid.parity", "content " + lp.bootCopy, "content /mnt/cache/snapraid.content", "content /mnt/disk1/snapraid.content", "data d1 /mnt/disk1/", "data d2 /mnt/disk2/", "data d3 /mnt/disk3/"} {
		if !strings.Contains(string(conf), line+"\n") {
			t.Errorf("snapraid.conf lacks %q:\n%s", line, conf)
		}
	}

	// The initial sync is the ordinary sync job, run by the real snapraid through
	// the guard, and it completed.
	if lp.syncID == "" {
		t.Fatal("the job queued no sync")
	}
	sj, err := li.sched.Await(ctx, lp.syncID)
	if err != nil || sj.Type != job.TypeSync || sj.Status != job.StatusSucceeded {
		t.Fatalf("the initial sync = %+v, %v", sj, err)
	}
	if opts, err := job.SyncOptsFromParams(sj.Params); err != nil || opts.Confirm || opts.DryRun {
		t.Errorf("sync params = %+v, %v, want a real sync that confirms no guard block", opts, err)
	}
	if info, err := os.Stat("/mnt/parity1/snapraid.parity"); err != nil || info.Size() == 0 {
		t.Errorf("parity file = %v, %v, want real parity written by the sync", info, err)
	}
	for _, p := range []string{lp.bootCopy, "/mnt/cache/snapraid.content", "/mnt/disk1/snapraid.content"} {
		if info, err := os.Stat(p); err != nil || info.Size() == 0 {
			t.Errorf("content file %s = %v, %v", p, info, err)
		}
	}
	if d, err := lp.engine.Diff(ctx); err != nil || d.Added != 0 || d.Removed != 0 || d.Updated != 0 || d.Moved != 0 {
		t.Errorf("snapraid diff after the sync = %+v, %v, want nothing left to sync", d, err)
	}

	// Every data file is as the fixture recorded it, read from the disks, and the
	// disks hold nothing else but SnapRAID's content file.
	for _, slot := range dataSlots {
		root := diskDir[slot]
		for p, mf := range manifest[slot] {
			if got := sha256File(t, filepath.Join(root, filepath.FromSlash(p))); got != mf.sha {
				t.Errorf("%s/%s has sha256 %s, the manifest says %s", slot, p, got, mf.sha)
			}
		}
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if _, ok := manifest[slot][rel]; !ok && rel != "snapraid.content" {
				t.Errorf("%s holds %s, which the fixture's manifest does not", slot, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if m, u, g := statOf(); m != modeBefore || u != uidBefore || g != gidBefore {
		t.Errorf("%s/%s changed from mode %v owner %d:%d to %v %d:%d: the point of no return touches top-level directories only", sampleDisk, sample, modeBefore, uidBefore, gidBefore, m, u, g)
	}

	// The deferred share settings: each seeded share's top-level directory on the
	// data disks and the cache is setgid, group 100.
	shares, err := li.shares.Shares.List(ctx)
	if err != nil || len(shares) == 0 {
		t.Fatalf("shares = %d, %v", len(shares), err)
	}
	for _, sh := range shares {
		if sh.TargetCacheMode != "" {
			t.Errorf("share %s still has the deferred cache mode %s", sh.Name, sh.TargetCacheMode)
		}
		// The branches a share's cache mode uses: the array disks for array-only, the
		// cache and the array disks for cache-then-move, the cache alone for cache-only.
		var dirs []string
		if sh.CacheMode != "cache-only" {
			dirs = append(dirs, "/mnt/disk1/"+sh.Name, "/mnt/disk2/"+sh.Name, "/mnt/disk3/"+sh.Name)
		}
		if sh.CacheMode != "array-only" {
			dirs = append(dirs, "/mnt/cache/"+sh.Name)
		}
		for _, dir := range dirs {
			info, err := os.Stat(dir)
			if err != nil {
				t.Errorf("%s: %v", dir, err)
				continue
			}
			if info.Mode().Perm() != 0o775 || info.Mode()&os.ModeSetgid == 0 || info.Sys().(*syscall.Stat_t).Gid != share.ShareGID {
				t.Errorf("%s has mode %v group %d, want rwxrwsr-x and group %d (Q26)", dir, info.Mode(), info.Sys().(*syscall.Stat_t).Gid, share.ShareGID)
			}
		}
	}
	appdata, err := li.shares.Shares.Get(ctx, "appdata")
	if err != nil || appdata.CacheMode != "cache-only" {
		t.Errorf("appdata = %+v, %v, want the deferred cache-only applied", appdata, err)
	}
}

func hasArg(c []string, arg string) bool {
	for _, a := range c[1:] {
		if a == arg {
			return true
		}
	}
	return false
}
