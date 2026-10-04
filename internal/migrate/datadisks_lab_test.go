//go:build lab

// This file runs only inside the loop-device lab (doc 06 §3, Q45), never on the
// host: it is built with `go test -tags lab -c` from the host (compiling touches
// no device) and the binary is run inside the lab container. It scans Unraid
// fixtures built by scripts/devenv/unraid-fixture.sh (doc 06 §5) through the real
// read-only mounter and the real filesystem checks, with the fixtures' disk
// images attached as the lab's own loop devices, and shows that no source disk is
// written: the sha256 of every whole device is the same after a full scan as
// before it.
//
// The lab's device cgroup lets a container open only loop devices, never the
// partition nodes the kernel makes of one, so each filesystem sits on a second
// loop device over the same image, at the partition's offset.

package migrate

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// labFixture returns the directory of a fixture variant under the lab, building
// it first when it is not there: the builder is the lab's own, run the way
// `make lab-unraid-fixture` runs it.
func labFixture(t *testing.T, variant string) string {
	t.Helper()
	lab := labDir(t)
	dir := filepath.Join(lab, "unraid", variant)
	if _, err := os.Stat(filepath.Join(dir, "expected", "manifest.sha256")); err == nil {
		return dir
	}
	cmd := exec.Command("bash", "/src/scripts/devenv/unraid-fixture.sh", "--tier", "l2", variant)
	cmd.Env = append(os.Environ(), "HOSERVA_LAB_ID="+os.Getenv("HOSERVA_LAB_ID"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the %s fixture: %v\n%s", variant, err, out)
	}
	return dir
}

// labSlot is one disk of a fixture attached to the lab: the whole image, and the
// partition the filesystem is in.
type labSlot struct {
	slot, id, img string
	whole, part   string
	size          int64
	fs, uuid      string
}

// labArray is a fixture attached as loop devices, with the inventory a scan of it
// sees. zip is the Flash Backup it is scanned from.
type labArray struct {
	t      *testing.T
	dir    string
	zip    string
	slots  []*labSlot
	disks  *disk.FakeProvider
	runner disk.Runner
	flash  *Flash
}

// attachFixture attaches every disk of the variant, and probes each partition's
// filesystem the way udev would report it.
func attachFixture(t *testing.T, variant string) *labArray {
	t.Helper()
	return attachFixtureAt(t, variant, labFixture(t, variant))
}

// attachFixtureAt attaches the disks of the variant's fixture built in dir,
// which is the lab's own build or a copy of it a test may change.
func attachFixtureAt(t *testing.T, variant, dir string) *labArray {
	t.Helper()
	ctx := context.Background()
	r := disk.CommandRunner{}
	src, err := OpenDir(filepath.Join(dir, "flash"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := ReadFlash(src)
	_ = src.Close()
	if err != nil {
		t.Fatal(err)
	}
	geometry := map[string][2]int64{}
	kinds := map[string]string{}
	var order []string
	layout, err := os.Open(filepath.Join(dir, "expected", "layout.txt"))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(layout)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[0] != "disk" {
			continue
		}
		var start, sectors int64
		for _, kv := range fields[2:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "start":
				start, _ = strconv.ParseInt(v, 10, 64)
			case "sectors":
				sectors, _ = strconv.ParseInt(v, 10, 64)
			case "kind":
				kinds[fields[1]] = v
			}
		}
		geometry[fields[1]] = [2]int64{start, sectors}
		order = append(order, fields[1])
	}
	_ = layout.Close()

	zipPath := filepath.Join(dir, "expected", "flash-backup.zip")
	slots := f.Slots
	if !f.HaveDisksINI {
		// A variant with no capture has no disks.ini, so the scan has no roles
		// to go by until the user assigns them (doc 05 §3). The role table is
		// what the capture would have said, written from the fixture's own
		// layout and added to a copy of its zip.
		var ini strings.Builder
		slots = nil
		for _, name := range order {
			sl := Slot{Name: name, ID: "FIXTURE_" + name + "-lab", Status: "DISK_OK"}
			switch kinds[name] {
			case "parity":
				sl.Type = "Parity"
				if name != "parity" {
					sl.Index = parity2Slot
				}
			case "data":
				sl.Type = "Data"
				n, _ := strconv.Atoi(strings.TrimPrefix(name, "disk"))
				sl.Index = n
				sl.FsType = f.DiskCfg.FsTypes[n]
			default:
				sl.Type = "Cache"
				if len(f.Pools) == 1 {
					sl.ID = f.Pools[0].ID
				}
			}
			fmt.Fprintf(&ini, "[\"%s\"]\nidx=\"%d\"\nname=\"%s\"\nid=\"%s\"\nsize=\"%d\"\nstatus=\"DISK_OK\"\ntype=\"%s\"\nfsType=\"%s\"\n", sl.Name, sl.Index, sl.Name, sl.ID, 1<<18, sl.Type, sl.FsType)
			slots = append(slots, sl)
		}
		zipPath = withDisksINI(t, dir, ini.String())
	}

	a := &labArray{t: t, dir: dir, zip: zipPath, disks: disk.NewFakeProvider(), runner: r, flash: f}
	for _, sl := range slots {
		g, ok := geometry[sl.Name]
		if !ok || !sl.Assigned() {
			continue
		}
		img := filepath.Join(dir, "img", sl.Name+".img")
		st, err := os.Stat(img)
		if err != nil {
			t.Fatal(err)
		}
		s := &labSlot{slot: sl.Name, id: sl.ID, img: img, size: st.Size()}
		s.whole = attachLoop(ctx, t, r, img, 0, 0)
		s.part = attachLoop(ctx, t, r, img, g[0]*512, g[1]*512)
		props := probe(t, r, s.part)
		s.fs, s.uuid = props["TYPE"], props["UUID"]
		a.slots = append(a.slots, s)
		a.disks.AddDisk(s.whole, disk.Disk{Serial: s.id, Size: s.size, Filesystem: s.fs, FSDevice: s.part, FSUUID: s.uuid})
	}
	if len(a.slots) == 0 {
		t.Fatalf("%s has no disk to attach", variant)
	}
	return a
}

// withDisksINI copies the variant's Flash Backup with a capture's disks.ini
// added, under the lab's own directory, and returns the copy's path.
func withDisksINI(t *testing.T, dir, ini string) string {
	t.Helper()
	zr, err := zip.OpenReader(filepath.Join(dir, "expected", "flash-backup.zip"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	out := filepath.Join(dir, "expected", "flash-backup-with-roles.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, e := range zr.File {
		w, err := zw.Create(e.Name)
		if err != nil {
			t.Fatal(err)
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
	w, err := zw.Create("config/hoserva/disks.ini")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, ini); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// probe reads a device's filesystem signature the way blkid reports it.
func probe(t *testing.T, r disk.Runner, dev string) map[string]string {
	t.Helper()
	out, _ := r.Run(context.Background(), "blkid", "-p", "-o", "export", dev)
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			props[k] = v
		}
	}
	return props
}

func (a *labArray) slot(name string) *labSlot {
	for _, s := range a.slots {
		if s.slot == name {
			return s
		}
	}
	a.t.Fatalf("no slot %s", name)
	return nil
}

// hashes returns the sha256 of every whole device, by slot.
func (a *labArray) hashes() map[string]string {
	out := map[string]string{}
	for _, s := range a.slots {
		out[s.slot] = sha256File(a.t, s.whole)
	}
	return out
}

func (a *labArray) assertUnchanged(before map[string]string, when string) {
	a.t.Helper()
	for slot, h := range a.hashes() {
		if before[slot] != h {
			a.t.Errorf("%s: %s's whole-device sha256 is %s, was %s: the scan wrote to a source disk", when, slot, h, before[slot])
		}
	}
}

// scanner is a Scanner as hoservad builds it, over the lab's disks, with the
// real runner and the real read-only mounter behind a probe.
func (a *labArray) scanner(t *testing.T, probe *readOnlyProbe) *Scanner {
	t.Helper()
	dir := filepath.Join(labDir(t), "scan-"+filepath.Base(a.dir))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	mounter := disk.ReadOnlyMounter(disk.KernelReadOnlyMounter{Runner: a.runner})
	if probe != nil {
		probe.ReadOnlyMounter = mounter
		mounter = probe
	}
	return &Scanner{
		Disks: a.disks, Runner: a.runner, Mounter: mounter, Dir: dir,
		Dirs: &DiskReader{Mounter: mounter, Runner: a.runner, Dir: dir}, UIDOwner: noUID, Now: fixedNow,
	}
}

func (a *labArray) scan(t *testing.T, s *Scanner, opts ScanOptions) (*Report, error) {
	t.Helper()
	zsrc, f, err := OpenZipFile(a.zip)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	return s.Scan(context.Background(), zsrc, opts)
}

// readOnlyProbe wraps the real mounter and records, for every mount, what the
// kernel's table says about it right after it is made.
type readOnlyProbe struct {
	disk.ReadOnlyMounter
	t       *testing.T
	devices []string
	options [][2]string
	onMount func(n int)
}

func (p *readOnlyProbe) MountReadOnly(ctx context.Context, fsType, device, uuid, where string) error {
	if err := p.ReadOnlyMounter.MountReadOnly(ctx, fsType, device, uuid, where); err != nil {
		return err
	}
	p.devices = append(p.devices, device)
	m, s := mountOptionsAt(p.t, resolved(p.t, where))
	p.options = append(p.options, [2]string{m, s})
	if p.onMount != nil {
		p.onMount(len(p.devices))
	}
	return nil
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// assertNoMountUnder fails when the kernel still lists a mount under dir.
func assertNoMountUnder(t *testing.T, dir string) {
	t.Helper()
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		real = dir
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && (strings.HasPrefix(fields[4], dir) || strings.HasPrefix(fields[4], real)) {
			t.Errorf("a mount is still at %s", fields[4])
		}
	}
}

type manifestFile struct {
	size int64
	sha  string
}

// readManifest returns the fixture's files by disk and path, as the builder
// recorded them from the mounted disks.
func readManifest(t *testing.T, dir string) map[string]map[string]manifestFile {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "expected", "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]map[string]manifestFile{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 6 {
			t.Fatalf("manifest line %q", line)
		}
		size, _ := strconv.ParseInt(cols[1], 10, 64)
		if out[cols[4]] == nil {
			out[cols[4]] = map[string]manifestFile{}
		}
		out[cols[4]][cols[5]] = manifestFile{size: size, sha: cols[0]}
	}
	return out
}

func hiddenTopLevel(path string) bool { return strings.HasPrefix(path, ".") }

// TestLabDataDisks_ScanNeverWritesASourceDisk is the data-loss scenario of #296:
// the Unraid data disks are the user's data and the rollback, and a scan that
// writes to one of them, even the superblock a plain read-only XFS mount
// rewrites, has changed what the user would go back to. It scans #74's primary
// fixture end to end and compares the whole-device sha256 of every disk before
// and after, with a control arm that shows the same harness sees a plain
// read-only mount write.
func TestLabDataDisks_ScanNeverWritesASourceDisk(t *testing.T) {
	a := attachFixture(t, primary)
	before := a.hashes()

	p := &readOnlyProbe{t: t}
	s := a.scanner(t, p)
	r, err := a.scan(t, s, ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	a.assertUnchanged(before, "after the scan")
	assertNoMountUnder(t, s.Dir)

	// Only the data disks and the cache pool were mounted, each read-only on the
	// mount and on the superblock; never the parity disk, whose device reports a
	// filesystem.
	parity := a.slot("parity")
	if parity.fs == "" {
		t.Fatalf("the fixture's parity disk reports no filesystem; it should carry the XOR artefact")
	}
	want := map[string]bool{a.slot("disk1").part: true, a.slot("disk2").part: true, a.slot("disk3").part: true, a.slot("cache").part: true}
	for i, dev := range p.devices {
		if dev == parity.part || dev == parity.whole {
			t.Errorf("the parity disk was mounted: %s", dev)
		}
		if !want[dev] {
			t.Errorf("mounted %s, which is not a data disk or the cache", dev)
		}
		if o := p.options[i]; !hasOpt(o[0], "ro") || !hasOpt(o[1], "ro") {
			t.Errorf("%s was mounted %q (superblock %q), want read-only on both", dev, o[0], o[1])
		}
	}
	for _, c := range []string{"disk1", "disk2", "disk3"} {
		if row := findRow(t, r, CheckIntegrity, c); row.Status != StatusPass {
			t.Errorf("%s integrity row = %+v", c, row)
		}
	}

	// The baseline holds every file the builder recorded on the mounted disks,
	// with the same size and sha256, and leaves hidden top-level directories out.
	manifest := readManifest(t, a.dir)
	br := openBaselineOf(t, s, r)
	seen := map[string]map[string]BaselineEntry{}
	if err := br.Each(func(e BaselineEntry) error {
		if seen[e.Disk] == nil {
			seen[e.Disk] = map[string]BaselineEntry{}
		}
		seen[e.Disk][e.name()] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	hashed := 0
	for _, slot := range []string{"disk1", "disk2", "disk3"} {
		for path, mf := range manifest[slot] {
			e, ok := seen[slot][path]
			if hiddenTopLevel(path) {
				if ok {
					t.Errorf("%s: %s is in a hidden top-level directory and is in the baseline", slot, path)
				}
				continue
			}
			switch {
			case !ok || e.Kind != KindFile:
				t.Errorf("%s: %s is in the manifest and not in the baseline as a file: %+v", slot, path, e)
			case e.Size != mf.size:
				t.Errorf("%s: %s has size %d in the baseline, %d in the manifest", slot, path, e.Size, mf.size)
			case e.SHA256 == "":
				t.Errorf("%s: %s was not hashed, although this fixture has fewer than 200 larger files", slot, path)
			case e.SHA256 != mf.sha:
				t.Errorf("%s: %s has sha256 %s in the baseline, %s in the manifest", slot, path, e.SHA256, mf.sha)
			default:
				hashed++
			}
		}
		for path, e := range seen[slot] {
			if e.Kind == KindFile {
				if _, ok := manifest[slot][path]; !ok {
					t.Errorf("%s: %s is in the baseline and not in the manifest", slot, path)
				}
			}
		}
	}
	if hashed < 20 {
		t.Errorf("only %d files were compared", hashed)
	}
	var kinds []string
	for _, slot := range []string{"disk1", "disk2", "disk3"} {
		for path, e := range seen[slot] {
			if e.Kind != KindFile {
				kinds = append(kinds, slot+":"+path+":"+e.Kind+":"+e.Special+e.Target)
			}
		}
	}
	sort.Strings(kinds)
	t.Logf("symlinks and special files recorded: %v", kinds)

	// The control arm. The same harness has to see what a plain read-only XFS
	// mount does to a device, or the assertions above prove nothing (doc 08 §2:
	// it rewrites the superblock and the log even on a cleanly unmounted disk).
	// It runs on a copy, never on the fixture's own image.
	ctx := context.Background()
	rr := disk.CommandRunner{}
	d1 := a.slot("disk1")
	copyImg := filepath.Join(labDir(t), "control-296.img")
	if out, err := rr.Run(ctx, "cp", "--sparse=always", d1.img, copyImg); err != nil {
		t.Fatalf("copying %s: %v %s", d1.img, err, out)
	}
	t.Cleanup(func() { _ = os.Remove(copyImg) })
	g := a.geometry(d1)
	cwhole := attachLoop(ctx, t, rr, copyImg, 0, 0)
	cpart := attachLoop(ctx, t, rr, copyImg, g[0]*512, g[1]*512)
	cwhere := filepath.Join(labDir(t), "control-296")
	if err := os.MkdirAll(cwhere, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(cwhere) })
	controlBefore := sha256File(t, cwhole)
	if controlBefore != before["disk1"] {
		t.Fatalf("the copy's sha256 is %s, the original's %s", controlBefore, before["disk1"])
	}
	if out, err := rr.Run(ctx, "mount", "-t", "xfs", "-o", "ro", cpart, cwhere); err != nil {
		t.Fatalf("control: mounting read-only: %v %s", err, out)
	}
	t.Cleanup(func() { _, _ = rr.Run(context.Background(), "umount", cwhere) })
	if _, err := rr.Run(ctx, "umount", cwhere); err != nil {
		t.Fatalf("control: unmounting: %v", err)
	}
	if got := sha256File(t, cwhole); got == controlBefore {
		t.Errorf("control: a plain read-only XFS mount left the device's sha256 unchanged (%s): this harness would not have seen a scan write to a source disk", got)
	}
}

func (a *labArray) geometry(s *labSlot) [2]int64 {
	a.t.Helper()
	layout, err := os.ReadFile(filepath.Join(a.dir, "expected", "layout.txt"))
	if err != nil {
		a.t.Fatal(err)
	}
	for _, line := range strings.Split(string(layout), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[0] == "disk" && fields[1] == s.slot {
			var g [2]int64
			for _, kv := range fields[2:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "start":
					g[0], _ = strconv.ParseInt(v, 10, 64)
				case "sectors":
					g[1], _ = strconv.ParseInt(v, 10, 64)
				}
			}
			return g
		}
	}
	a.t.Fatalf("no geometry for %s", s.slot)
	return [2]int64{}
}

func openBaselineOf(t *testing.T, s *Scanner, r *Report) *BaselineReader {
	t.Helper()
	if r.Baseline == nil {
		t.Fatalf("the scan recorded no baseline: %+v", r.Rows)
	}
	br, err := OpenBaseline(filepath.Join(s.Dir, r.Baseline.File))
	if err != nil {
		t.Fatal(err)
	}
	return br
}

// expectedScan is the fixture builder's own expected result, measured on its
// disks: a verdict per slot.
type expectedScan struct {
	verdict map[string]string // slot -> parity, adopt, refuse, recreated
	reason  map[string]string
	adopted []string
}

func readExpectedScan(t *testing.T, dir string) expectedScan {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "expected", "scan.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	e := expectedScan{verdict: map[string]string{}, reason: map[string]string{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		cols := strings.Split(sc.Text(), "\t")
		switch {
		case len(cols) >= 4 && cols[0] == "disk":
			e.verdict[cols[1]], e.reason[cols[1]] = cols[2], cols[3]
		case len(cols) == 2 && cols[0] == "adopted" && cols[1] != "-":
			e.adopted = strings.Split(cols[1], ",")
		}
	}
	return e
}

// scanAgainstExpectation scans the variant and checks each disk against the
// builder's expected result and that no disk was written. The expected result
// says adopt, refuse with a reason, parity or recreated for every slot.
func scanAgainstExpectation(t *testing.T, variant string, hashesAreRecorded bool) (*labArray, *Scanner, *Report, *readOnlyProbe) {
	t.Helper()
	a := attachFixture(t, variant)
	want := readExpectedScan(t, a.dir)
	before := a.hashes()
	if hashesAreRecorded {
		// The builder recorded the whole-device hashes when it built the
		// disks; they are what a scan must leave.
		raw, err := os.ReadFile(filepath.Join(a.dir, "expected", "source-disks.sha256"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			cols := strings.Split(line, "\t")
			if len(cols) == 3 && !strings.HasPrefix(line, "#") {
				if before[cols[2]] != cols[0] {
					t.Fatalf("%s's sha256 is %s before the scan, the builder recorded %s", cols[2], before[cols[2]], cols[0])
				}
			}
		}
	}
	p := &readOnlyProbe{t: t}
	s := a.scanner(t, p)
	r, err := a.scan(t, s, ScanOptions{})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	a.assertUnchanged(before, "after the scan")
	assertNoMountUnder(t, s.Dir)

	for slot, verdict := range want.verdict {
		switch verdict {
		case "parity":
			for _, dev := range p.devices {
				if dev == a.slot(slot).part {
					t.Errorf("the parity disk %s was mounted", slot)
				}
			}
		case "adopt":
			if row := findRow(t, r, CheckDataDisks, slot); row.Status != StatusPass {
				t.Errorf("%s: data disk row = %+v, want adopted", slot, row)
			}
			if row := findRow(t, r, CheckIntegrity, slot); row.Status != StatusPass {
				t.Errorf("%s: integrity row = %+v, want clean", slot, row)
			}
			mounted := false
			for _, dev := range p.devices {
				mounted = mounted || dev == a.slot(slot).part
			}
			if !mounted {
				t.Errorf("%s was adopted and never read", slot)
			}
		case "refuse":
			check := CheckDataDisks
			if want.reason[slot] == "integrity" {
				check = CheckIntegrity
			}
			row := findRow(t, r, check, slot)
			if row.Status != StatusRefuse || !strings.Contains(row.Detail, slot+" is not adopted") {
				t.Errorf("%s: %s row = %+v, want it refused by name", slot, check, row)
			}
			for _, dev := range p.devices {
				if dev == a.slot(slot).part {
					t.Errorf("the refused disk %s was mounted", slot)
				}
			}
		}
	}
	if r.Baseline != nil {
		var got []string
		for _, d := range r.Baseline.Disks {
			got = append(got, d.Slot)
		}
		sort.Strings(got)
		wantAdopted := append([]string(nil), want.adopted...)
		sort.Strings(wantAdopted)
		if strings.Join(got, ",") != strings.Join(wantAdopted, ",") {
			t.Errorf("baseline covers %v, the fixture expects %v adopted", got, wantAdopted)
		}
	}
	return a, s, r, p
}

// A disk whose metadata fails xfs_repair -n is refused by name, never mounted, and
// the other data disks are adopted: the fixture's expected result.
func TestLabDataDisks_CorruptXFSIsRefusedAndTheRestProceed(t *testing.T) {
	_, _, r, _ := scanAgainstExpectation(t, "unraid-corrupt-xfs", true)
	if r.Verdict != VerdictNoGo {
		t.Errorf("verdict = %s, want no_go", r.Verdict)
	}
	row := findRow(t, r, CheckIntegrity, "disk2")
	if !strings.Contains(row.Detail, "read-only xfs check failed") {
		t.Errorf("disk2 row = %+v", row)
	}
}

// An ext4 and a single-device btrfs disk beside two XFS ones are each checked by
// their own read-only tool, mounted read-only without replaying a journal, and
// adopted; no device is written.
func TestLabDataDisks_BtrfsAndExt4DisksAreAdopted(t *testing.T) {
	a, _, r, p := scanAgainstExpectation(t, "unraid-btrfs-and-ext4-disks", false)
	if r.Verdict == VerdictNoGo {
		for _, row := range r.Rows {
			if row.Status == StatusRefuse {
				t.Errorf("unexpected refusal: %+v", row)
			}
		}
	}
	types := map[string]string{}
	for i, dev := range p.devices {
		types[dev] = p.options[i][1]
	}
	for _, slot := range []string{"disk3", "disk4"} {
		if _, ok := types[a.slot(slot).part]; !ok {
			t.Errorf("%s was not mounted", slot)
		}
	}
	if a.slot("disk3").fs != "btrfs" || a.slot("disk4").fs != "ext4" {
		t.Errorf("the fixture's disks are %s and %s", a.slot("disk3").fs, a.slot("disk4").fs)
	}
}

// A scan cancelled while a source disk is mounted unmounts it, writes nothing and
// leaves no baseline.
func TestLabDataDisks_ACancelledScanLeavesNothingMounted(t *testing.T) {
	a := attachFixture(t, primary)
	before := a.hashes()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &readOnlyProbe{t: t, onMount: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	s := a.scanner(t, p)
	zsrc, f, err := OpenZipFile(a.zip)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	_, err = s.Scan(ctx, zsrc, ScanOptions{})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("Scan = %v, want it cancelled", err)
	}
	assertNoMountUnder(t, s.Dir)
	a.assertUnchanged(before, "after the cancelled scan")
	baselines, _ := filepath.Glob(filepath.Join(s.Dir, baselinePrefix+"*"))
	scratch, _ := filepath.Glob(filepath.Join(s.Dir, tmpPrefix+"*"))
	if len(baselines)+len(scratch) != 0 {
		t.Errorf("a cancelled scan left %v %v", baselines, scratch)
	}
	if len(p.devices) != 2 {
		t.Errorf("mounted %d disks, want the scan to stop at the second", len(p.devices))
	}
}
