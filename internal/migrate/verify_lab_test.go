//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host. It is the data-loss scenario of the verify phase (doc 05 §4 step 16):
// the last checkpoint before parity is touched has to find a file that was
// truncated, a sample file that changed at the same size and a file that is
// gone, name each, and pass a pool that holds exactly what the scan recorded. It
// scans #74's primary fixture, changes the disks the way a damaged array would
// differ (through a read-write mount of a copy of the fixture's images, never the
// fixture's own), imports them read-only, and runs the migration_verify job
// through the scheduler, the pool at /mnt/user and the kernel's mount table as
// hoservad does. No source disk is written by the import or the verify: the
// whole-device sha256 of every data disk is the same before the import and after
// the verify.

package migrate

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/pool"
)

// copiedFixture is a copy of the lab's build of a fixture variant under a
// directory of its own, so a test may change its images.
func copiedFixture(t *testing.T, variant string) string {
	t.Helper()
	src := labFixture(t, variant)
	dst := filepath.Join(labDir(t), "verify-copy", variant)
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(dst)) })
	if out, err := (disk.CommandRunner{}).Run(context.Background(), "cp", "-r", "--sparse=always", src, dst); err != nil {
		t.Fatalf("copying the fixture: %v %s", err, out)
	}
	return dst
}

// changeDisk mounts a slot's filesystem read-write at a scratch mountpoint, lets
// fn change it and unmounts it cleanly, so the XFS log is clean again for the
// read-only mount the import makes.
func changeDisk(t *testing.T, a *labArray, slot string, fn func(root string)) {
	t.Helper()
	ctx := context.Background()
	rr := disk.CommandRunner{}
	where := filepath.Join(labDir(t), "verify-rw-"+slot)
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(where) })
	if out, err := rr.Run(ctx, "mount", "-t", "xfs", a.slot(slot).part, where); err != nil {
		t.Fatalf("mounting %s read-write on the copy: %v %s", slot, err, out)
	}
	mounted := true
	unmount := func() {
		if !mounted {
			return
		}
		mounted = false
		if out, err := rr.Run(ctx, "umount", where); err != nil {
			t.Fatalf("unmounting %s: %v %s", where, err, out)
		}
	}
	defer unmount()
	fn(where)
	unmount()
}

// labVerify is a scanned and imported primary fixture with the verify job wired
// the way hoservad wires it.
type labVerify struct {
	t        *testing.T
	a        *labArray
	li       *labImport
	manifest map[string]map[string]manifestFile
	before   map[string]string
}

var dataSlots3 = []string{"disk1", "disk2", "disk3"}

// newLabVerify scans the copy of the primary fixture, applies change to its disks
// (nil for none), imports it read-only and wires the verify job.
func newLabVerify(t *testing.T, change func(a *labArray, manifest map[string]map[string]manifestFile)) *labVerify {
	t.Helper()
	dir := copiedFixture(t, primary)
	a := attachFixtureAt(t, primary, dir)
	a.disks = labInventory(t, a, true, "parity", "disk1", "disk2", "disk3", "cache")
	li := newLabImport(t, a, nil)
	// The session's directory is the scanner's, as hoservad's is: the baseline
	// the scan writes is the file the verify reads.
	li.svc.Dir = li.svc.Scanner.Dir
	li.scanZip(readFileOrFail(t, a.zip))
	manifest := readManifest(t, dir)
	if change != nil {
		change(a, manifest)
	}
	lv := &labVerify{t: t, a: a, li: li, manifest: manifest, before: a.hashes()}

	done := li.runImport(li.proposedRoles())
	if done.Status != job.StatusSucceeded {
		t.Fatalf("the import job ended %s: %s", done.Status, done.ErrorMessage)
	}
	li.svc.Adopted = func(ctx context.Context) (Adoption, error) {
		_, disks, err := li.arrays.GetArray(ctx)
		if err != nil {
			return Adoption{}, err
		}
		sort.SliceStable(disks, func(i, j int) bool { return disks[i].RoleIndex < disks[j].RoleIndex })
		ad := Adoption{Pool: pool.CatchAllPath}
		for _, d := range disks {
			ad.Disks = append(ad.Disks, AdoptedDisk{Serial: d.Serial, WWN: d.WWN, Mountpoint: d.Mountpoint})
		}
		return ad, nil
	}
	li.svc.ConfirmReadOnly = func(ctx context.Context, where string) error {
		return disk.ConfirmMountedReadOnly(ctx, li.rr, where)
	}
	li.registry.Register(job.TypeMigrationVerify, true, job.RunMigrationVerify(li.svc.RunVerify))
	return lv
}

// verify queues the migration_verify job as the API does, while the import is
// pending, and returns the finished job and the session's result.
func (lv *labVerify) verify() (*job.Job, *VerifyResult) {
	lv.t.Helper()
	ctx := context.Background()
	if err := lv.li.svc.CheckVerify(ctx); err != nil {
		lv.t.Fatalf("CheckVerify: %v", err)
	}
	j, err := lv.li.sched.Submit(ctx, job.TypeMigrationVerify, []string{JobResource}, nil)
	if err != nil {
		lv.t.Fatalf("Submit: %v", err)
	}
	done, err := lv.li.sched.Await(ctx, j.ID)
	if err != nil {
		lv.t.Fatalf("Await: %v", err)
	}
	st, err := lv.li.svc.State(ctx)
	if err != nil {
		lv.t.Fatal(err)
	}
	lv.a.assertUnchanged(lv.before, "after the import and the verify")
	return done, st.Verify
}

// pick returns the first path (in path order) of slot's manifest that no other
// data disk holds, is not hidden and satisfies ok.
func (lv *labVerify) pick(slot string, ok func(manifestFile) bool) string {
	lv.t.Helper()
	var paths []string
	for p, mf := range lv.manifest[slot] {
		if hiddenTopLevel(p) || !ok(mf) {
			continue
		}
		dup := false
		for _, other := range dataSlots3 {
			if _, has := lv.manifest[other][p]; has && other != slot {
				dup = true
			}
		}
		if !dup {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		lv.t.Fatalf("%s's manifest has no file for the test to change", slot)
	}
	sort.Strings(paths)
	return paths[0]
}

func shareOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// A pool that holds exactly what the scan recorded passes, and the figures it
// passes with are the fixture's own: each disk's files and bytes from its
// manifest, each share's the union of the disks' (a path two disks hold once),
// and the paths on more than one disk listed, not counted missing or extra.
func TestLabVerify_ACleanPoolPasses(t *testing.T) {
	lv := newLabVerify(t, nil)
	done, res := lv.verify()
	if done.Status != job.StatusSucceeded || res == nil || res.Status != VerifyPassed {
		t.Fatalf("the verify job ended %s (%s): %+v", done.Status, done.ErrorMessage, res)
	}
	for _, s := range append(append([]VerifyScope{}, res.Disks...), res.Shares...) {
		if !s.Passed() {
			t.Errorf("%q did not pass: %+v", s.Name, s)
		}
	}

	type fig struct{ files, bytes int64 }
	perDisk := map[string]fig{}
	union := map[string]fig{}
	seen := map[string]bool{}
	var dups int64
	for _, slot := range dataSlots3 {
		for p, mf := range lv.manifest[slot] {
			if hiddenTopLevel(p) {
				continue
			}
			f := perDisk[slot]
			perDisk[slot] = fig{f.files + 1, f.bytes + mf.size}
			if seen[p] {
				dups++
				continue
			}
			seen[p] = true
			u := union[shareOf(p)]
			union[shareOf(p)] = fig{u.files + 1, u.bytes + mf.size}
		}
	}
	for _, slot := range dataSlots3 {
		d := scopeOf(t, res.Disks, slot)
		if d.Expected.Files != perDisk[slot].files || d.Expected.Bytes != perDisk[slot].bytes || d.Found != d.Expected {
			t.Errorf("%s expected %+v and found %+v, want the manifest's %+v", slot, d.Expected, d.Found, perDisk[slot])
		}
		if d.Hashed < 1 {
			t.Errorf("%s: no sample file was hashed again", slot)
		}
	}
	for share, want := range union {
		s := scopeOf(t, res.Shares, share)
		if s.Expected.Files != want.files || s.Expected.Bytes != want.bytes || s.Found != s.Expected {
			t.Errorf("share %q expected %+v and found %+v, want the union %+v", share, s.Expected, s.Found, want)
		}
	}
	if len(res.Shares) != len(union) {
		t.Errorf("%d shares in the result, %d in the manifest", len(res.Shares), len(union))
	}
	if res.Duplicates != dups {
		t.Errorf("%d duplicate paths, want the manifest's %d", res.Duplicates, dups)
	}
	if st, _ := lv.li.svc.State(context.Background()); st.Phase != PhaseVerified {
		t.Errorf("phase = %s, want verified", st.Phase)
	}
	// A verify can be run again.
	if again, res := lv.verify(); again.Status != job.StatusSucceeded || res.Status != VerifyPassed {
		t.Errorf("the second verify ended %s: %+v", again.Status, res)
	}
}

// Each way a damaged array differs from the scan fails the verify and names the
// file, on its disk and in its share.
func TestLabVerify_ADamagedPoolFailsAndNamesTheFile(t *testing.T) {
	for name, tc := range map[string]struct {
		small bool
		// change makes the damage to the file on the disk; found reads the
		// scope's list that must name it.
		change func(path string)
		list   func(s *VerifyScope) VerifyList
		same   bool
	}{
		"a file truncated to a shorter length": {
			change: func(p string) {
				info, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Truncate(p, info.Size()/2); err != nil {
					t.Fatal(err)
				}
			},
			list: func(s *VerifyScope) VerifyList { return s.SizeChanged },
		},
		"a sample file changed at the same size": {
			small: true,
			change: func(p string) {
				data, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				data[0] ^= 0xff
				if err := os.WriteFile(p, data, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			list: func(s *VerifyScope) VerifyList { return s.ChecksumChanged },
			same: true,
		},
		"a file that is gone": {
			change: func(p string) {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
			},
			list: func(s *VerifyScope) VerifyList { return s.Missing },
		},
	} {
		t.Run(name, func(t *testing.T) {
			var path string
			lv := newLabVerify(t, func(a *labArray, manifest map[string]map[string]manifestFile) {
				probe := &labVerify{t: t, manifest: manifest}
				path = probe.pick("disk1", func(mf manifestFile) bool { return mf.size >= 2 && mf.size <= DefaultSmallFileBytes })
				changeDisk(t, a, "disk1", func(root string) { tc.change(filepath.Join(root, filepath.FromSlash(path))) })
			})
			done, res := lv.verify()
			if done.Status != job.StatusFailed || res == nil || res.Status != VerifyFailed {
				t.Fatalf("the verify job ended %s (%s): %+v", done.Status, done.ErrorMessage, res)
			}
			if st, _ := lv.li.svc.State(context.Background()); st.Phase != PhaseVerifyFailed {
				t.Errorf("phase = %s, want verify_failed", st.Phase)
			}
			d1 := scopeOf(t, res.Disks, "disk1")
			wantPaths(t, "disk1", tc.list(d1), path)
			if tc.same && (d1.SizeChanged.Total != 0 || d1.Expected.Bytes != d1.Found.Bytes) {
				t.Errorf("disk1's sizes differ (%+v): the test no longer shows a change only the checksum sees", d1)
			}
			wantPaths(t, "its share", tc.list(scopeOf(t, res.Shares, shareOf(path))), path)
			for _, d := range res.Disks {
				if d.Name != "disk1" && !d.Passed() {
					t.Errorf("%s failed: %+v", d.Name, d)
				}
			}
		})
	}
}
