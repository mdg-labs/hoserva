package migrate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// verifyEnv is an Unraid array as the verify phase sees it after the import: the
// scan recorded the baseline from the fake mounts of the fixture's data disks,
// and each adopted disk is a directory holding the same files, which a test then
// changes. The pool is the first-branch union of the adopted disks, built when a
// verify asks for it.
type verifyEnv struct {
	t     *testing.T
	s     *Service
	e     *dataEnv
	slots []string
	roots map[string]string

	mu       sync.Mutex
	pending  bool
	readonly map[string]error
	checked  []string
	poolFrom func() string
}

func newVerifyEnv(t *testing.T, mutate func(trees map[string]tree)) *verifyEnv {
	t.Helper()
	s, e := newDiskService(t, primary)
	trees := map[string]tree{
		"disk1": {
			files: map[string]string{
				"media/a.txt":      "alpha alpha alpha",
				"media/b.txt":      "bravo",
				"docs/note.txt":    "a note",
				"shared/dup.txt":   "one",
				"shared/only1.txt": "only on disk one",
			},
			big:   map[string]int64{"media/big.bin": 3 << 20},
			links: map[string]string{"media/link": "a.txt"},
			fifos: []string{"media/pipe"},
		},
		"disk2": {
			files: map[string]string{
				"media/c.txt":    "charlie",
				"shared/dup.txt": "second copy",
				"movies/m.txt":   "movie",
				"top.txt":        "a file in the root",
			},
		},
		"disk3": {
			files: map[string]string{"movies/n.txt": "another movie"},
		},
	}
	if mutate != nil {
		mutate(trees)
	}
	for slot, tr := range trees {
		e.trees[e.dev[slot]+"1"] = tr
	}
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatalf("scan: %v", err)
	}
	st, err := s.State(ctx0)
	if err != nil || st.Report == nil || st.Report.Baseline == nil || st.Report.Review == nil {
		t.Fatalf("State = %+v, %v", st, err)
	}
	v := &verifyEnv{t: t, s: s, e: e, slots: []string{"disk1", "disk2", "disk3"}, roots: map[string]string{}, pending: true, readonly: map[string]error{}}
	serials := map[string]string{}
	for _, d := range st.Report.Review.Disks {
		serials[d.Slot] = d.Serial
	}
	for _, slot := range v.slots {
		v.roots[slot] = t.TempDir()
		trees[slot].populate(t, v.roots[slot])
	}
	pool := ""
	v.poolFrom = func() string {
		if pool == "" {
			pool = v.buildPool()
		}
		return pool
	}
	s.Pending = func(context.Context) (bool, error) {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.pending, nil
	}
	s.Adopted = func(context.Context) (Adoption, error) {
		ad := Adoption{Pool: v.poolFrom()}
		for _, slot := range v.slots {
			ad.Disks = append(ad.Disks, AdoptedDisk{Serial: serials[slot], Mountpoint: v.roots[slot]})
		}
		return ad, nil
	}
	s.ConfirmReadOnly = func(_ context.Context, where string) error {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.checked = append(v.checked, where)
		return v.readonly[where]
	}
	v.poolFrom()
	return v
}

// buildPool is what mergerfs shows over the adopted disks: every path once, from
// the first disk that has it.
func (v *verifyEnv) buildPool() string {
	v.t.Helper()
	pool := v.t.TempDir()
	for _, slot := range v.slots {
		root := v.roots[slot]
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			if rel == "." {
				return nil
			}
			dst := filepath.Join(pool, rel)
			switch {
			case d.IsDir():
				return os.MkdirAll(dst, 0o755)
			case d.Type()&fs.ModeSymlink != 0:
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				if _, err := os.Lstat(dst); err == nil {
					return nil
				}
				return os.Symlink(target, dst)
			case d.Type()&fs.ModeNamedPipe != 0:
				if _, err := os.Lstat(dst); err == nil {
					return nil
				}
				return syscall.Mkfifo(dst, 0o644)
			case d.Type().IsRegular():
				if _, err := os.Lstat(dst); err == nil {
					return nil
				}
				in, err := os.Open(p)
				if err != nil {
					return err
				}
				defer func() { _ = in.Close() }()
				out, err := os.Create(dst)
				if err != nil {
					return err
				}
				if _, err := io.Copy(out, in); err != nil {
					_ = out.Close()
					return err
				}
				if err := out.Close(); err != nil {
					return err
				}
				info, err := in.Stat()
				if err != nil {
					return err
				}
				return os.Truncate(dst, info.Size())
			}
			return nil
		})
		if err != nil {
			v.t.Fatal(err)
		}
	}
	return pool
}

// at is a path on an adopted disk, pool a path in the pool, which is built from
// the disks as they were when the scan recorded them: a change the pool shows is
// made to both.
func (v *verifyEnv) at(slot, rel string) string {
	return filepath.Join(v.roots[slot], filepath.FromSlash(rel))
}

func (v *verifyEnv) pool(rel string) string {
	return filepath.Join(v.poolFrom(), filepath.FromSlash(rel))
}

func (v *verifyEnv) run() (*VerifyResult, error) {
	v.t.Helper()
	err := v.s.RunVerify(ctx0, io.Discard)
	st, serr := v.s.State(ctx0)
	if serr != nil {
		v.t.Fatal(serr)
	}
	return st.Verify, err
}

func scopeOf(t *testing.T, scopes []VerifyScope, name string) *VerifyScope {
	t.Helper()
	for i := range scopes {
		if scopes[i].Name == name {
			return &scopes[i]
		}
	}
	t.Fatalf("no scope %q in %+v", name, scopes)
	return nil
}

func wantPaths(t *testing.T, what string, l VerifyList, paths ...string) {
	t.Helper()
	if l.Total != int64(len(paths)) || strings.Join(l.Paths, ",") != strings.Join(paths, ",") {
		t.Errorf("%s = %+v, want exactly %v", what, l, paths)
	}
}

func assertOnlyFailed(t *testing.T, res *VerifyResult, disks, shares []string) {
	t.Helper()
	want := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, d := range res.Disks {
		if d.Passed() == want(disks, d.Name) {
			t.Errorf("disk %s: Passed = %v, want %v: %+v", d.Name, d.Passed(), !want(disks, d.Name), d)
		}
	}
	for _, sh := range res.Shares {
		if sh.Passed() == want(shares, sh.Name) {
			t.Errorf("share %q: Passed = %v, want %v: %+v", sh.Name, sh.Passed(), !want(shares, sh.Name), sh)
		}
	}
}

func assertRootsUntouched(t *testing.T, before, after map[string]string) {
	t.Helper()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s changed during verify: %q -> %q", k, v, after[k])
		}
	}
	if len(before) != len(after) {
		t.Errorf("the disks hold %d entries after verify, %d before", len(after), len(before))
	}
}

func snapshotRoots(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			out[p] = info.Mode().String() + info.ModTime().String() + strconv.FormatInt(info.Size(), 10)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// A pool that holds exactly what the scan recorded passes: every disk and every
// share matches, the path two disks hold is listed once as a duplicate and is not
// missing or extra, and the disks are not written.
func TestVerify_ACleanPoolPasses(t *testing.T) {
	v := newVerifyEnv(t, nil)
	before := snapshotRoots(t, v.roots["disk1"], v.roots["disk2"], v.roots["disk3"])
	res, err := v.run()
	if err != nil {
		t.Fatalf("RunVerify = %v, want a pass", err)
	}
	if res.Status != VerifyPassed {
		t.Fatalf("result = %+v", res)
	}
	st, _ := v.s.State(ctx0)
	if st.Phase != PhaseVerified {
		t.Errorf("phase = %s, want verified", st.Phase)
	}
	assertOnlyFailed(t, res, nil, nil)
	d1 := scopeOf(t, res.Disks, "disk1")
	if d1.Expected.Files != 6 || d1.Found.Files != 6 || d1.Expected.Symlinks != 1 || d1.Expected.Special != 1 || d1.Expected.Bytes != d1.Found.Bytes || d1.Hashed != 6 {
		t.Errorf("disk1 = %+v, want 6 files (every one hashed), a symlink and a fifo found as recorded", d1)
	}
	// The union, not the sum: shared/dup.txt is on two disks and counted once.
	shared := scopeOf(t, res.Shares, "shared")
	if shared.Expected.Files != 2 || shared.Found.Files != 2 {
		t.Errorf("share shared = %+v, want the union of two paths (dup.txt once, only1.txt)", shared)
	}
	if shared.Expected.Bytes != int64(len("one")+len("only on disk one")) {
		t.Errorf("share shared expects %d bytes, want dup.txt from the first disk only", shared.Expected.Bytes)
	}
	if got := scopeOf(t, res.Shares, "movies"); got.Expected.Files != 2 {
		t.Errorf("share movies = %+v, want a file from each of two disks", got)
	}
	if got := scopeOf(t, res.Shares, ""); got.Expected.Files != 1 || got.Found.Files != 1 {
		t.Errorf("the root = %+v, want top.txt", got)
	}
	if res.Duplicates != 1 || len(res.DuplicateSample) != 1 || res.DuplicateSample[0].Path != "shared/dup.txt" || strings.Join(res.DuplicateSample[0].Disks, ",") != "disk1,disk2" {
		t.Errorf("duplicates = %d %+v, want shared/dup.txt on disk1 and disk2", res.Duplicates, res.DuplicateSample)
	}
	assertRootsUntouched(t, before, snapshotRoots(t, v.roots["disk1"], v.roots["disk2"], v.roots["disk3"]))
	for _, slot := range v.slots {
		found := false
		for _, c := range v.checked {
			found = found || c == v.roots[slot]
		}
		if !found {
			t.Errorf("%s was walked without its mount being confirmed read-only: %v", slot, v.checked)
		}
	}
	e := v.e
	entries, _ := os.ReadDir(e.dir)
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), tmpPrefix) {
			t.Errorf("a verify scratch directory is left: %s", ent.Name())
		}
	}
}

// A file cut short fails the verify and is named, on its disk and in its share.
func TestVerify_ATruncatedFileFailsAndIsNamed(t *testing.T) {
	v := newVerifyEnv(t, nil)
	for _, p := range []string{v.at("disk1", "media/a.txt"), v.pool("media/a.txt")} {
		if err := os.Truncate(p, 5); err != nil {
			t.Fatal(err)
		}
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	st, _ := v.s.State(ctx0)
	if st.Phase != PhaseVerifyFailed || res.Status != VerifyFailed {
		t.Errorf("phase = %s, status = %s, want a failed verify", st.Phase, res.Status)
	}
	assertOnlyFailed(t, res, []string{"disk1"}, []string{"media"})
	wantPaths(t, "disk1 size changed", scopeOf(t, res.Disks, "disk1").SizeChanged, "media/a.txt")
	wantPaths(t, "media size changed", scopeOf(t, res.Shares, "media").SizeChanged, "media/a.txt")
	if d := scopeOf(t, res.Disks, "disk1"); d.Found.Bytes != d.Expected.Bytes-int64(len("alpha alpha alpha")-5) {
		t.Errorf("disk1 found %d bytes, expected %d", d.Found.Bytes, d.Expected.Bytes)
	}
}

// A sample file that changed without changing its size has the same count and
// the same bytes everywhere: only its checksum shows it, and the file is named.
func TestVerify_ASampleFileChangedAtTheSameSizeFailsAndIsNamed(t *testing.T) {
	v := newVerifyEnv(t, nil)
	for _, p := range []string{v.at("disk1", "docs/note.txt"), v.pool("docs/note.txt")} {
		if err := os.WriteFile(p, []byte("a n0te"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	d1 := scopeOf(t, res.Disks, "disk1")
	if d1.Expected != d1.Found || d1.SizeChanged.Total != 0 {
		t.Fatalf("disk1 counts and sizes differ (%+v): the test no longer shows a change only a checksum sees", d1)
	}
	wantPaths(t, "disk1 checksum changed", d1.ChecksumChanged, "docs/note.txt")
	wantPaths(t, "docs checksum changed", scopeOf(t, res.Shares, "docs").ChecksumChanged, "docs/note.txt")
	assertOnlyFailed(t, res, []string{"disk1"}, []string{"docs"})
}

// A file that is not there fails the verify and is named, on its disk and in its
// share.
func TestVerify_AMissingFileFailsAndIsNamed(t *testing.T) {
	v := newVerifyEnv(t, nil)
	for _, p := range []string{v.at("disk2", "movies/m.txt"), v.pool("movies/m.txt")} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	wantPaths(t, "disk2 missing", scopeOf(t, res.Disks, "disk2").Missing, "movies/m.txt")
	wantPaths(t, "movies missing", scopeOf(t, res.Shares, "movies").Missing, "movies/m.txt")
	assertOnlyFailed(t, res, []string{"disk2"}, []string{"movies"})
}

// A path only one adopted disk holds that the scan never saw is extra, and it
// fails.
func TestVerify_AnExtraFileFails(t *testing.T) {
	v := newVerifyEnv(t, nil)
	for _, p := range []string{v.at("disk3", "movies/new.txt"), v.pool("movies/new.txt")} {
		if err := os.WriteFile(p, []byte("new"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	wantPaths(t, "disk3 extra", scopeOf(t, res.Disks, "disk3").Extra, "movies/new.txt")
	wantPaths(t, "movies extra", scopeOf(t, res.Shares, "movies").Extra, "movies/new.txt")
}

// A symlink that is gone, one that points elsewhere and a special file that is
// now something else each fail like a missing file.
func TestVerify_ASymlinkOrSpecialFileThatDiffersFails(t *testing.T) {
	for name, tc := range map[string]struct {
		change  func(v *verifyEnv, p func(slot, rel string) string)
		missing []string
		changed []string
	}{
		"a symlink that is gone": {
			change: func(v *verifyEnv, p func(string, string) string) {
				for _, path := range []string{p("disk1", "media/link"), v.pool("media/link")} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
			},
			missing: []string{"media/link"},
		},
		"a symlink with another target": {
			change: func(v *verifyEnv, p func(string, string) string) {
				for _, path := range []string{p("disk1", "media/link"), v.pool("media/link")} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("b.txt", path); err != nil {
						t.Fatal(err)
					}
				}
			},
			changed: []string{"media/link"},
		},
		"a fifo replaced by a file": {
			change: func(v *verifyEnv, p func(string, string) string) {
				for _, path := range []string{p("disk1", "media/pipe"), v.pool("media/pipe")} {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			},
			changed: []string{"media/pipe"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := newVerifyEnv(t, nil)
			tc.change(v, v.at)
			res, err := v.run()
			if !errors.Is(err, ErrVerifyMismatch) {
				t.Fatalf("RunVerify = %v, want the mismatch", err)
			}
			d1, media := scopeOf(t, res.Disks, "disk1"), scopeOf(t, res.Shares, "media")
			wantPaths(t, "disk1 missing", d1.Missing, tc.missing...)
			wantPaths(t, "disk1 changed", d1.Changed, tc.changed...)
			wantPaths(t, "media missing", media.Missing, tc.missing...)
			wantPaths(t, "media changed", media.Changed, tc.changed...)
		})
	}
}

// The pool shows the first branch's copy of a path two disks hold. A pool that
// shows the second disk's instead differs from the baseline's union, and that is
// reported; it is not what a sum of the disks would expect either.
func TestVerify_TheUnionTakesTheFirstBranchsCopy(t *testing.T) {
	v := newVerifyEnv(t, nil)
	if err := os.WriteFile(v.pool("shared/dup.txt"), []byte("second copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	wantPaths(t, "shared size changed", scopeOf(t, res.Shares, "shared").SizeChanged, "shared/dup.txt")
	for _, d := range res.Disks {
		if !d.Passed() {
			t.Errorf("disk %s failed, but each disk still holds what was recorded: %+v", d.Name, d)
		}
	}
}

// A verify that cannot read a file does not pass: the error is the result, the
// job fails, and the session is verify_failed.
func TestVerify_AnUnreadableSampleFileIsNeverAPass(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a file's permissions do not stop root from reading it")
	}
	v := newVerifyEnv(t, nil)
	p := v.at("disk1", "media/b.txt")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	res, err := v.run()
	if err == nil || errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want an error that is not a mismatch", err)
	}
	if res.Status != VerifyFailed || !strings.Contains(res.Error, "media/b.txt") {
		t.Errorf("result = %+v, want a failure naming the file", res)
	}
	if st, _ := v.s.State(ctx0); st.Phase != PhaseVerifyFailed {
		t.Errorf("phase = %s", st.Phase)
	}
}

// A disk or the pool that is not shown read-only by the mount table is not read,
// and an earlier passing result is gone by then.
func TestVerify_AMountNotConfirmedReadOnlyIsNotReadAndClearsAnEarlierPass(t *testing.T) {
	for _, where := range []string{"disk2", "pool"} {
		t.Run(where, func(t *testing.T) {
			v := newVerifyEnv(t, nil)
			if _, err := v.run(); err != nil {
				t.Fatal(err)
			}
			if where == "pool" {
				v.readonly[v.poolFrom()] = errors.New("mounted read-write")
			} else {
				v.readonly[v.roots[where]] = errors.New("mounted read-write")
			}
			res, err := v.run()
			if err == nil || errors.Is(err, ErrVerifyMismatch) {
				t.Fatalf("RunVerify = %v, want the read-only refusal", err)
			}
			if res.Status != VerifyFailed || !strings.Contains(res.Error, "read-only") || len(res.Disks) != 0 {
				t.Errorf("result = %+v, want a failure that compared nothing", res)
			}
			if st, _ := v.s.State(ctx0); st.Phase != PhaseVerifyFailed {
				t.Errorf("phase = %s, want verify_failed: the earlier pass must not stand", st.Phase)
			}
		})
	}
}

// A verify cancelled while it runs is never a pass, and what it leaves is a
// failed result a re-run replaces.
func TestVerify_ACancelledVerifyIsNeverAPass(t *testing.T) {
	v := newVerifyEnv(t, nil)
	ctx, cancel := context.WithCancel(ctx0)
	v.s.ConfirmReadOnly = func(context.Context, string) error { cancel(); return nil }
	err := v.s.RunVerify(ctx, io.Discard)
	if err == nil {
		t.Fatal("a cancelled verify succeeded")
	}
	st, _ := v.s.State(ctx0)
	if st.Phase != PhaseVerifyFailed || st.Verify.Status != VerifyFailed || !strings.Contains(st.Verify.Error, "cancelled") {
		t.Errorf("phase = %s, result = %+v, want a failed verify that says it was cancelled", st.Phase, st.Verify)
	}
	v.s.ConfirmReadOnly = func(context.Context, string) error { return nil }
	if _, err := v.run(); err != nil {
		t.Fatalf("the re-run = %v, want a pass", err)
	}
	if st, _ := v.s.State(ctx0); st.Phase != PhaseVerified {
		t.Errorf("phase after the re-run = %s", st.Phase)
	}
}

// A baseline disk no adopted disk matches, and an adopted disk the baseline never
// had, are failures, not skips.
func TestVerify_ADiskWithoutItsPartnerFails(t *testing.T) {
	v := newVerifyEnv(t, nil)
	adopted := v.s.Adopted
	v.s.Adopted = func(ctx context.Context) (Adoption, error) {
		ad, err := adopted(ctx)
		ad.Disks = append(ad.Disks[:1], ad.Disks[2:]...)
		ad.Disks = append(ad.Disks, AdoptedDisk{Serial: "not-in-the-scan", Mountpoint: v.t.TempDir()})
		return ad, err
	}
	res, err := v.run()
	if !errors.Is(err, ErrVerifyMismatch) {
		t.Fatalf("RunVerify = %v, want the mismatch", err)
	}
	if d := scopeOf(t, res.Disks, "disk2"); d.Problem == "" || d.Passed() {
		t.Errorf("disk2 = %+v, want a problem: no adopted disk has its serial", d)
	}
	var orphan bool
	for _, d := range res.Disks {
		orphan = orphan || (d.Problem != "" && d.Name != "disk2")
	}
	if !orphan {
		t.Errorf("no scope says the extra adopted disk has no baseline: %+v", res.Disks)
	}
}

// Verify runs only while an import is pending, and a refusal leaves the session
// as it was.
func TestVerify_NeedsAPendingImport(t *testing.T) {
	v := newVerifyEnv(t, nil)
	v.pending = false
	if err := v.s.CheckVerify(ctx0); !errors.Is(err, ErrVerifyNotPending) {
		t.Errorf("CheckVerify = %v, want the not-pending refusal", err)
	}
	if err := v.s.RunVerify(ctx0, io.Discard); !errors.Is(err, ErrVerifyNotPending) {
		t.Errorf("RunVerify = %v, want the not-pending refusal", err)
	}
	if st, _ := v.s.State(ctx0); st.Verify != nil || st.Phase != PhaseScanned {
		t.Errorf("State = %+v, want the session unchanged", st)
	}
	v.s.Adopted = nil
	v.pending = true
	if err := v.s.RunVerify(ctx0, io.Discard); !errors.Is(err, ErrVerifyNotConfigured) {
		t.Errorf("RunVerify without adopted disks = %v, want the not-configured refusal", err)
	}
}

// A verify the previous process was running is a failed one after a restart, and
// a new scan, whose baseline is a different one, clears the result.
func TestVerify_ARestartFailsARunningVerifyAndAScanClearsTheResult(t *testing.T) {
	v := newVerifyEnv(t, nil)
	if err := v.s.setVerify(ctx0, &VerifyResult{Status: VerifyRunning}); err != nil {
		t.Fatal(err)
	}
	if st, _ := v.s.State(ctx0); st.Phase != PhaseVerifying {
		t.Fatalf("phase = %s, want verifying", st.Phase)
	}
	if err := v.s.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	st, _ := v.s.State(ctx0)
	if st.Phase != PhaseVerifyFailed || !strings.Contains(st.Verify.Error, "restart") {
		t.Errorf("after Recover: phase = %s, result = %+v", st.Phase, st.Verify)
	}
	v.pending = false
	if err := startAndRun(t, v.s, v.e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	v.pending = true
	if st, _ := v.s.State(ctx0); st.Verify != nil || st.Phase != PhaseImported {
		t.Errorf("after a new scan: phase = %s, result = %+v, want none", st.Phase, st.Verify)
	}
}

// The job's log says what it compared and the percentage reaches 100.
func TestVerify_ReportsProgress(t *testing.T) {
	v := newVerifyEnv(t, nil)
	var out bytes.Buffer
	var pcts []int
	ctx := WithProgress(ctx0, func(p int) { pcts = append(pcts, p) })
	if err := v.s.RunVerify(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Errorf("progress = %v, want it to end at 100", pcts)
	}
	if !strings.Contains(out.String(), "disk1: ") || !strings.Contains(out.String(), "verify passed") {
		t.Errorf("log:\n%s", out.String())
	}
}
