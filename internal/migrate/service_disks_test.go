package migrate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// newDiskService is a Service whose scans read the fixture's data disks through
// the env's fake mounter and runner.
func newDiskService(t *testing.T, variant string) (*Service, *dataEnv) {
	t.Helper()
	e := newDataEnv(t, variant)
	svc := &Service{Dir: e.dir, Scanner: e.scanner, Sessions: newSessions(t), Mounter: e.mounter}
	return svc, e
}

func startAndRun(t *testing.T, s *Service, e *dataEnv, opts ScanOptions, ctx context.Context, out io.Writer) error {
	t.Helper()
	var upload string
	if err := s.StartScan(ctx0, bytes.NewReader(zipOf(t, e.files, false)), opts, func(_ context.Context, got string) (string, error) {
		upload = got
		return "job-1", nil
	}); err != nil {
		t.Fatal(err)
	}
	return s.RunScan(ctx, out, upload)
}

// The options a scan was started with reach the job through the session row, and
// the baseline is a private file the report names.
func TestService_TheBaselineIsAPrivateFileTheSessionNamesAndTheOptionsReachTheJob(t *testing.T) {
	s, e := newDiskService(t, primary)
	var out bytes.Buffer
	var pcts []int
	ctx := WithProgress(ctx0, func(p int) { pcts = append(pcts, p) })
	if err := startAndRun(t, s, e, ScanOptions{FullChecksums: true}, ctx, &out); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx0)
	if err != nil || st.Phase != PhaseScanned || st.Report.Baseline == nil {
		t.Fatalf("State = %+v, %v", st, err)
	}
	if !st.Report.Baseline.Rule.Full {
		t.Errorf("the rule is %+v: the option the scan was started with did not reach the job", st.Report.Baseline.Rule)
	}
	info, err := os.Stat(filepath.Join(s.Dir, st.Report.Baseline.File))
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("baseline file = %v, %v; want a private file", info, err)
	}
	br, err := s.OpenBaseline(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	_ = br.Each(func(BaselineEntry) error { n++; return nil })
	if n != 3 {
		t.Errorf("%d entries, want the one file of each of the three data disks", n)
	}
	if len(pcts) == 0 || pcts[len(pcts)-1] != 100 {
		t.Errorf("the job was told %v, want progress ending at 100", pcts)
	}
	if !strings.Contains(out.String(), "disk1: ") || !strings.Contains(out.String(), "migration scan finished") {
		t.Errorf("the job log:\n%s", out.String())
	}
	where := filepath.Join(s.Dir, mountsDir, dataMount)
	for _, m := range e.mounter.Mounts {
		if m.Where != where {
			t.Errorf("mounted at %s, want the private mountpoint %s", m.Where, where)
		}
	}
	e.assertNothingLeft()
}

// A second scan's baseline replaces the first once it is committed, and a failed
// one leaves the previous report with its own baseline.
func TestService_ARescanReplacesTheBaselineAndAFailedOneKeepsIt(t *testing.T) {
	s, e := newDiskService(t, primary)
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	first, _ := s.State(ctx0)
	firstFile := first.Report.Baseline.File

	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	second, _ := s.State(ctx0)
	if second.Report.Baseline.File == firstFile {
		t.Fatal("a rescan reused the baseline's name")
	}
	if got := e.baselineFiles(); len(got) != 1 || got[0] != second.Report.Baseline.File {
		t.Errorf("baseline files = %v, want only the new one", got)
	}

	e.mounter.UnmountErr = errors.New("busy")
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err == nil {
		t.Fatal("a scan that could not release a disk succeeded")
	}
	e.mounter.UnmountErr = nil
	st, _ := s.State(ctx0)
	if st.Phase != PhaseScanFailed || st.Report == nil || st.Report.Baseline == nil || st.Report.Baseline.File != second.Report.Baseline.File {
		t.Fatalf("State = %+v, want the failure with the previous report and baseline", st)
	}
	if got := e.baselineFiles(); len(got) != 1 || got[0] != second.Report.Baseline.File {
		t.Errorf("baseline files = %v, want the previous one only", got)
	}
	if _, err := s.OpenBaseline(ctx0); err != nil {
		t.Errorf("the previous baseline is unreadable: %v", err)
	}
}

// A scan cancelled while a disk is mounted releases it, records the failure and
// keeps the previous report.
func TestService_ACancelledScanReleasesItsDisksAndKeepsThePreviousReport(t *testing.T) {
	s, e := newDiskService(t, primary)
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	before, _ := s.State(ctx0)
	e.mounter.Mounts = nil

	ctx, cancel := context.WithCancel(ctx0)
	inner := e.mounter.OnMount
	e.mounter.OnMount = func(where string) error {
		if err := inner(where); err != nil {
			return err
		}
		cancel()
		return nil
	}
	err := startAndRun(t, s, e, ScanOptions{}, ctx, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunScan = %v, want context.Canceled", err)
	}
	e.assertNothingLeft()
	st, _ := s.State(ctx0)
	if st.Phase != PhaseScanFailed || st.Report == nil || st.Report.Baseline.File != before.Report.Baseline.File {
		t.Fatalf("State = %+v, want the failure with the previous report", st)
	}
	if got := e.baselineFiles(); len(got) != 1 {
		t.Errorf("baseline files = %v", got)
	}
}

func TestService_ForgetDeletesTheBaselineToo(t *testing.T) {
	s, e := newDiskService(t, primary)
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx0); err != nil {
		t.Fatal(err)
	}
	if got := e.baselineFiles(); len(got) != 0 {
		t.Errorf("Forget left %v", got)
	}
	if _, err := s.OpenBaseline(ctx0); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("OpenBaseline after Forget = %v, want ErrNoBaseline", err)
	}
}

// At start a source disk a killed process left mounted is released, the scratch
// space of its scan is removed, and a baseline file no row names is pruned.
func TestService_RecoverReleasesALeftoverDiskMountAndScratchSpace(t *testing.T) {
	s, e := newDiskService(t, primary)
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	st, _ := s.State(ctx0)
	kept := st.Report.Baseline.File

	where := filepath.Join(s.Dir, mountsDir, dataMount)
	e.mounter.SetMounted(where)
	scratch := filepath.Join(s.Dir, tmpPrefix+"deadbeef")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "disk1.walk"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(s.Dir, baselinePrefix+"orphan"+baselineSuffix)
	if err := os.WriteFile(stray, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Recover(ctx0); err != nil {
		t.Fatal(err)
	}
	if p := e.mounter.MountedPaths(); len(p) != 0 {
		t.Errorf("still mounted after Recover: %v", p)
	}
	for _, gone := range []string{scratch, stray} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s survived Recover", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, kept)); err != nil {
		t.Errorf("the baseline the row names was removed: %v", err)
	}
}

// A mount Recover cannot release is not hidden: the next read of a disk refuses
// to mount over it.
func TestService_ADiskMountThatCannotBeReleasedStopsTheNextScan(t *testing.T) {
	s, e := newDiskService(t, primary)
	where := filepath.Join(s.Dir, mountsDir, dataMount)
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	e.mounter.SetMounted(where)
	e.mounter.UnmountErr = errors.New("target is busy")
	_, err := e.scanE()
	if !errors.Is(err, ErrDiskRelease) {
		t.Fatalf("Scan = %v, want ErrDiskRelease", err)
	}
	if len(e.mounter.Mounts) != 0 {
		t.Errorf("mounted over a mount that could not be released: %v", e.mounter.Mounts)
	}
}

// What a restored row names is not trusted: a baseline that is not here, or a
// name with a path in it, is no baseline.
func TestService_OpenBaselineDoesNotTrustTheRowsName(t *testing.T) {
	s, e := newDiskService(t, primary)
	if _, err := s.OpenBaseline(ctx0); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("before a scan: %v, want ErrNoBaseline", err)
	}
	if err := startAndRun(t, s, e, ScanOptions{}, ctx0, io.Discard); err != nil {
		t.Fatal(err)
	}
	st, _ := s.State(ctx0)
	if err := os.Remove(filepath.Join(s.Dir, st.Report.Baseline.File)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenBaseline(ctx0); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("baseline file removed: %v, want ErrNoBaseline", err)
	}
	sess, _ := s.load(ctx0)
	sess.Report.Baseline.File = "../" + sess.Report.Baseline.File
	if err := s.save(ctx0, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenBaseline(ctx0); !errors.Is(err, ErrNoBaseline) {
		t.Errorf("a name with a path: %v, want ErrNoBaseline", err)
	}
}

// The DiskReader mounts a disk read-only to list its top-level directories, and
// refuses what is not an ordinary single filesystem.
func TestDiskReader_ListsTopLevelDirectoriesAndRefusesWhatItCannotShowIsAnOrdinaryFilesystem(t *testing.T) {
	e := newDataEnv(t, primary)
	reader := &DiskReader{Mounter: e.mounter, Runner: e.runner, Dir: e.dir}
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"media/a": "a", "docs/b": "b", "loose.txt": "c"}, dirs: []string{"empty", ".Trash-0"}}
	list, _ := e.disks.List(ctx0)
	byDev := map[string]int{}
	for i, d := range list {
		byDev[d.Device] = i
	}
	dirs, err := reader.TopLevelDirs(ctx0, list[byDev[e.dev["disk1"]]])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dirs, ",") != ".Trash-0,docs,empty,media" {
		t.Errorf("dirs = %v", dirs)
	}
	if p := e.mounter.MountedPaths(); len(p) != 0 {
		t.Errorf("still mounted: %v", p)
	}
	if len(e.mounter.Mounts) != 1 || e.mounter.Mounts[0].FSType != "xfs" || e.mounter.Mounts[0].Where != filepath.Join(e.dir, mountsDir, dirsMount) {
		t.Errorf("mounts = %+v", e.mounter.Mounts)
	}

	e.mounter.Mounts = nil
	for name, change := range map[string]func(*disk.Disk){
		"an Unraid boot device": func(d *disk.Disk) { d.UnraidBoot = true },
		"the Unraid stick":      func(d *disk.Disk) { d.Filesystem, d.Label = "vfat", "UNRAID" },
		"a ZFS member":          func(d *disk.Disk) { d.Filesystem = "zfs_member" },
		"no filesystem uuid":    func(d *disk.Disk) { d.FSUUID = "" },
	} {
		d := list[byDev[e.dev["disk1"]]]
		change(&d)
		if _, err := reader.TopLevelDirs(ctx0, d); err == nil {
			t.Errorf("%s: listed", name)
		}
	}
	if len(e.mounter.Mounts) != 0 {
		t.Errorf("mounted %+v for a device that was refused", e.mounter.Mounts)
	}
}

// The appdata row says how large the share is, on the array from the baseline.
func TestScan_TheAppdataRowGainsItsSize(t *testing.T) {
	e := newDataEnv(t, primary)
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"appdata/x/db": strings.Repeat("a", 2000), "appdata/y": "bb", "media/m": "m"}}
	e.trees[e.dev["disk2"]+"1"] = tree{files: map[string]string{"appdata/z": strings.Repeat("c", 1000)}}
	r := e.scan()
	row, ok := rowFor(r, CheckCache, "appdata")
	if !ok || !strings.Contains(row.Detail, "it holds 2.9 KiB on the array (disk1: 2.0 KiB, disk2: 1000 B)") {
		t.Errorf("appdata row = %+v", row)
	}
}

// A pool device's appdata is measured through the reader's read-only mount.
func TestScan_TheAppdataRowSizesTheCachePool(t *testing.T) {
	e := newDataEnv(t, primary)
	e.trees[e.dev["disk1"]+"1"] = tree{files: map[string]string{"media/m": "m"}}
	e.trees[e.dev["cache"]+"1"] = tree{files: map[string]string{"appdata/db": strings.Repeat("a", 5000), "system/x": "s"}}
	e.scanner.Dirs = &DiskReader{Mounter: e.mounter, Runner: e.runner, Dir: e.dir}
	r := e.scan()
	row, _ := rowFor(r, CheckCache, "appdata")
	if row.Status != StatusWarn || !strings.Contains(row.Detail, "it holds 4.9 KiB on pool cache") {
		t.Errorf("appdata row = %+v", row)
	}
	e.assertNothingLeft()
}
