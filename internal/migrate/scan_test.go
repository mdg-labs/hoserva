package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

func scanVariant(t *testing.T, variant string, handZipped bool) *Report {
	t.Helper()
	files, spec := flashTree(t, variant)
	src := openZipBytes(t, zipOf(t, files, handZipped))
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
	if err != nil {
		t.Fatalf("Scan(%s): %v", variant, err)
	}
	return r
}

// Every doc 05 §2 variant fixture produces the report its definition implies,
// and the two zip shapes the fixtures build give the same one.
func TestScan_EveryVariantFixtureProducesItsReport(t *testing.T) {
	want := map[string]struct {
		version  string
		parity   int
		mapped   int
		pools    []string
		parityFS string
	}{
		"unraid-6.12-xfs-single-parity": {"6.12.15", 1, 5, []string{"pool cache"}, "xfs"},
		"unraid-7x-xfs-single-parity":   {"7.3.2", 1, 5, []string{"pool cache"}, "xfs"},
		"unraid-dual-parity":            {"6.12.15", 2, 7, []string{"pool cache"}, "xfs"},
		"unraid-named-pools":            {"7.3.2", 1, 6, []string{"pool cache", "pool fast"}, "xfs"},
		"unraid-no-cache":               {"7.3.2", 1, 4, nil, "xfs"},
	}
	for _, variant := range fixtureVariants {
		w := want[variant]
		t.Run(variant, func(t *testing.T) {
			r := scanVariant(t, variant, false)
			hand := scanVariant(t, variant, true)
			r.GeneratedAt, hand.GeneratedAt = time.Time{}, time.Time{}
			if fmt.Sprint(r) != fmt.Sprint(hand) {
				t.Errorf("flash-backup and hand-zipped shapes give different reports:\n%v\n%v", r, hand)
			}

			if r.UnraidVersion != w.version {
				t.Errorf("version = %q, want %q", r.UnraidVersion, w.version)
			}
			if v := rowsFor(r, CheckVersion); len(v) != 1 || v[0].Status != StatusPass {
				t.Errorf("version rows = %+v, want one pass", v)
			}
			pc := rowsFor(r, CheckParity)
			if len(pc) != 1 || pc[0].Status != StatusInfo || !strings.Contains(pc[0].Detail, fmt.Sprintf("%d parity disk(s)", w.parity)) {
				t.Errorf("parity rows = %+v, want %d parity disk(s)", pc, w.parity)
			}
			mapped := 0
			for _, row := range rowsFor(r, CheckMapping) {
				if row.Status == StatusPass {
					mapped++
				}
			}
			if mapped != w.mapped {
				t.Errorf("%d mapped disks, want %d: %+v", mapped, w.mapped, rowsFor(r, CheckMapping))
			}
			for _, pool := range w.pools {
				if row := findRow(t, r, CheckMapping, pool); row.Status != StatusPass {
					t.Errorf("%s row = %+v", pool, row)
				}
			}
			if r.Verdict != VerdictGoWithWarnings && r.Verdict != VerdictGo {
				t.Errorf("verdict = %s on a healthy fixture: %+v", r.Verdict, r.Rows)
			}
			// The only thing the healthy fixture's scan flags is what the fixture
			// genuinely lacks: no boot mode in a capture... which the committed
			// captures do carry, so nothing is flagged at all.
			for _, row := range r.Rows {
				if row.Status == StatusRefuse || row.Status == StatusFlag {
					t.Errorf("unexpected %s row on a healthy fixture: %+v", row.Status, row)
				}
			}
		})
	}
}

// A parity disk reports a valid-looking XFS signature on a real array; the
// scan takes its role from disks.ini and reports it as parity.
func TestScan_ParityRoleComesFromTheSlotNotTheFilesystem(t *testing.T) {
	files, spec := flashTree(t, "unraid-6.12-xfs-single-parity")
	disks := fixtureDisks(spec)
	list, _ := disks.List(context.Background())
	var parityDev string
	for _, d := range list {
		if d.Serial == "parity-hoserva-test" {
			parityDev = d.Device
			if d.Filesystem != "xfs" {
				t.Fatalf("fixture parity disk filesystem = %q, want xfs", d.Filesystem)
			}
		}
	}
	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, r, CheckMapping, "parity")
	if row.Status != StatusPass || !strings.Contains(row.Detail, "Unraid parity slot 0") || !strings.Contains(row.Detail, parityDev) {
		t.Errorf("parity mapping row = %+v, want it reported as parity slot 0 on %s", row, parityDev)
	}
	for _, row := range rowsFor(r, CheckMapping) {
		if row.Subject == "parity" && strings.Contains(row.Detail, "disk 0") {
			t.Errorf("parity reported as a data disk: %+v", row)
		}
	}
	if len(rowsFor(r, CheckParitySize)) != 1 || rowsFor(r, CheckParitySize)[0].Status != StatusPass {
		t.Errorf("parity size rows = %+v", rowsFor(r, CheckParitySize))
	}
}

func TestScan_VersionAndLayoutAllowlist(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string][]byte)
		problem string
	}{
		{"6.12.x", func(f map[string][]byte) { f["changes.txt"] = []byte("# Version 6.12.4 2023-01-01\n") }, ""},
		{"7.0 prerelease", func(f map[string][]byte) { f["changes.txt"] = []byte("# Version 7.0.0-rc.1 2024-01-01\n") }, ""},
		{"6.11", func(f map[string][]byte) { f["changes.txt"] = []byte("# Version 6.11.5 2022-01-01\n") }, "neither 6.12.x nor 7.x"},
		{"8.0", func(f map[string][]byte) { f["changes.txt"] = []byte("# Version 8.0.0 2030-01-01\n") }, "neither 6.12.x nor 7.x"},
		{"no changes.txt", func(f map[string][]byte) { delete(f, "changes.txt") }, "does not state an Unraid version"},
		{"changes.txt without a version", func(f map[string][]byte) { f["changes.txt"] = []byte("hello\n") }, "does not state an Unraid version"},
		{"no kernel", func(f map[string][]byte) { delete(f, "bzimage") }, "no bzimage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
			tc.mutate(files)
			src := openZipBytes(t, zipOf(t, files, false))

			_, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
			if tc.problem == "" {
				if err != nil {
					t.Fatalf("a supported layout was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupportedLayout) || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("Scan without the override = %v, want ErrUnsupportedLayout mentioning %q", err, tc.problem)
			}

			r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{UnverifiedLayout: true})
			if err != nil {
				t.Fatalf("Scan with the override: %v", err)
			}
			if !r.UnverifiedLayout {
				t.Error("the override is not recorded in the report")
			}
			if row := rowsFor(r, CheckVersion)[0]; row.Status != StatusWarn || !strings.Contains(row.Detail, tc.problem) {
				t.Errorf("version row = %+v, want a warn naming %q", row, tc.problem)
			}
			if md := r.Markdown(); !strings.HasPrefix(md, "# Hoserva migration scan report\n\n> **Unverified layout.**") {
				t.Errorf("the override is not printed at the top of the document:\n%.300s", md)
			}
		})
	}
}

func TestScan_OverrideOnAVerifiedLayoutIsNotRecorded(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{UnverifiedLayout: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.UnverifiedLayout || strings.Contains(r.Markdown(), "Unverified layout") {
		t.Error("an override that overrode nothing is recorded as used")
	}
}

func TestScan_RefusesWithoutAUsableDiskCfgWhateverTheOverride(t *testing.T) {
	for name, mutate := range map[string]func(map[string][]byte){
		"missing": func(f map[string][]byte) { delete(f, "config/disk.cfg") },
		"binary":  func(f map[string][]byte) { f["config/disk.cfg"] = []byte{0x00, 0x01, 0xff, 0xfe} },
		"not cfg": func(f map[string][]byte) { f["config/disk.cfg"] = []byte("this is not a settings file\n") },
		"empty":   func(f map[string][]byte) { f["config/disk.cfg"] = []byte("# nothing\n") },
		"only junk": func(f map[string][]byte) {
			delete(f, "config/disk.cfg")
			f["previous/config/disk.cfg"] = []byte("a=\"b\"\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
			mutate(files)
			src := openZipBytes(t, zipOf(t, files, true))
			for _, override := range []bool{false, true} {
				_, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{UnverifiedLayout: override})
				if !errors.Is(err, ErrNoDiskCfg) {
					t.Errorf("override=%v: Scan = %v, want ErrNoDiskCfg", override, err)
				}
			}
		})
	}
}

// Nothing the junk carries is read as configuration: the previous release's
// changes.txt and disk.cfg, a .git directory, desktop litter and AppleDouble
// twins would each change the report if they were.
func TestScan_JunkIsNeverReadAsConfiguration(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	files["config/plugins/dockerMan/templates-user/._my-notes.xml"] = []byte("<Container><Name>evil</Name></Container>")
	files["config/._disk.cfg"] = []byte("startArray=\"no\"\n")
	files["config/pools/._cache.cfg"] = []byte("diskId=\"FIXTURE_evil\"\n")
	files["config/hoserva/._disks.ini"] = []byte("[\"disk9\"]\nid=\"FIXTURE_evil\"\ntype=\"Data\"\nstatus=\"DISK_OK\"\n")
	files[".Trash-1000/config/disk.cfg"] = []byte("x=\"y\"\n")
	files[".fseventsd/config/disk.cfg"] = []byte("x=\"y\"\n")
	files["prev/changes.txt"] = []byte("# Version 5.0.0\n")
	data := zipOf(t, files, true)
	src := openZipBytes(t, data)

	for _, n := range src.List("") {
		for _, part := range strings.Split(n, "/") {
			if part == ".git" || strings.HasPrefix(part, "._") || part == "previous" || part == "prev" ||
				part == ".Spotlight-V100" || part == ".fseventsd" || part == "System Volume Information" || strings.HasPrefix(part, ".Trash") {
				t.Errorf("junk entry %q is listed as a source file", n)
			}
		}
	}
	if _, err := src.Read("previous/changes.txt"); err == nil {
		t.Error("previous/changes.txt is readable")
	}

	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.UnraidVersion != "7.3.2" {
		t.Errorf("version = %q: a junk changes.txt was read", r.UnraidVersion)
	}
	for _, row := range r.Rows {
		if strings.Contains(row.Detail, "evil") || strings.Contains(row.Subject, "evil") || row.Subject == "disk9" {
			t.Errorf("row built from a junk file: %+v", row)
		}
	}
	plain := scanVariant(t, "unraid-7x-xfs-single-parity", false)
	r.GeneratedAt, plain.GeneratedAt = time.Time{}, time.Time{}
	if fmt.Sprint(r) != fmt.Sprint(plain) {
		t.Error("a zip carrying extra junk gives a different report")
	}
}

func TestOpenZip_RefusesUnsafeAndOversizedSources(t *testing.T) {
	for name, entry := range map[string]string{
		"dotdot":        "../evil",
		"nested dotdot": "config/../../evil",
		"absolute":      "/etc/passwd",
		"backslash":     `..\evil`,
		"junk dotdot":   "previous/../../evil",
	} {
		t.Run(name, func(t *testing.T) {
			files, _ := flashTree(t, "unraid-7x-xfs-single-parity")
			files[entry] = []byte("x")
			_, err := OpenZip(bytesReaderAt(zipOf(t, files, true)))
			if !errors.Is(err, ErrInvalidZip) {
				t.Errorf("OpenZip = %v, want ErrInvalidZip", err)
			}
		})
	}
	if _, err := OpenZip(strings.NewReader("x"), maxZipBytes+1); !errors.Is(err, ErrInvalidZip) {
		t.Errorf("an oversized zip: %v, want ErrInvalidZip", err)
	}
	if _, err := OpenZip(strings.NewReader("this is not a zip at all"), 24); !errors.Is(err, ErrInvalidZip) {
		t.Errorf("not a zip: %v, want ErrInvalidZip", err)
	}
	files, _ := flashTree(t, "unraid-7x-xfs-single-parity")
	files["a..b/file"] = []byte("x")
	if _, err := OpenZip(bytesReaderAt(zipOf(t, files, true))); err != nil {
		t.Errorf("a name merely containing two dots was refused: %v", err)
	}
}

func TestScan_MissingDisksINIIsAWarningAndTheTableComesFromStepSeven(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	delete(files, "config/hoserva/disks.ini")
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, row := range rowsFor(r, CheckMapping) {
		if row.Status == StatusWarn && strings.Contains(row.Detail, "step 7") {
			warned = true
		}
		if row.Status == StatusRefuse {
			t.Errorf("a missing disks.ini refused: %+v", row)
		}
	}
	if !warned {
		t.Errorf("no warning that the serial table comes from step 7: %+v", rowsFor(r, CheckMapping))
	}
	if pc := rowsFor(r, CheckParity); len(pc) != 1 || !strings.Contains(pc[0].Detail, "disk.cfg") || !strings.Contains(pc[0].Detail, "1 parity disk(s)") {
		t.Errorf("parity rows without disks.ini = %+v, want the count from disk.cfg", pc)
	}
}

func TestScan_UnmatchedSlotIsFlaggedByName(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	disks := disk.NewFakeProvider()
	for i, d := range spec.disks {
		if d.slot == "disk2" {
			continue
		}
		disks.AddDisk(fmt.Sprintf("/dev/sd%c", 'b'+i), disk.Disk{Serial: d.serial(), Size: d.size})
	}
	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, r, CheckMapping, "disk2")
	if row.Status != StatusFlag || !strings.Contains(row.Detail, "disk2-hoserva-test") {
		t.Errorf("disk2 row = %+v, want a flag naming its serial", row)
	}
	if r.Verdict != VerdictGoWithWarnings {
		t.Errorf("verdict = %s, want go_with_warnings", r.Verdict)
	}
}

func TestScan_WeakIdentity(t *testing.T) {
	for _, tc := range []struct {
		slot   string
		status Status
		want   Verdict
	}{
		{"parity", StatusRefuse, VerdictNoGo},
		{"disk1", StatusFlag, VerdictGoWithWarnings},
	} {
		t.Run(tc.slot, func(t *testing.T) {
			files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
			disks := disk.NewFakeProvider()
			for i, d := range spec.disks {
				disks.AddDisk(fmt.Sprintf("/dev/sd%c", 'b'+i), disk.Disk{Serial: d.serial(), Size: d.size, WeakIdentity: d.slot == tc.slot})
			}
			r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if row := findRow(t, r, CheckIdentity, tc.slot); row.Status != tc.status {
				t.Errorf("identity row = %+v, want %s", row, tc.status)
			}
			if r.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s", r.Verdict, tc.want)
			}
		})
	}
}

func TestScan_AmbiguousIdentityTakesNoDisk(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	disks := fixtureDisks(spec)
	disks.AddDisk("/dev/sdz", disk.Disk{Serial: "disk1-hoserva-test", Size: 1 << 30})
	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, r, CheckMapping, "disk1")
	if row.Status != StatusFlag || !strings.Contains(row.Detail, "match this identity") {
		t.Errorf("disk1 row = %+v, want an ambiguity flag", row)
	}
	if rows := rowsFor(r, CheckSMART); len(rows) != 4 {
		t.Errorf("%d SMART rows, want 4 (the ambiguous disk is not polled)", len(rows))
	}
}

func TestScan_MatchesByWWN(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	files["config/hoserva/disks.ini"] = []byte(strings.ReplaceAll(string(files["config/hoserva/disks.ini"]), "FIXTURE_disk1-hoserva-test", "WDC_WD40_5000c500a1b2c3d4"))
	disks := fixtureDisks(spec)
	disks.AddDisk("/dev/sdq", disk.Disk{WWN: "0x5000c500a1b2c3d4", Size: 320 << 20})
	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if row := findRow(t, r, CheckMapping, "disk1"); row.Status != StatusPass || !strings.Contains(row.Detail, "/dev/sdq") {
		t.Errorf("disk1 row = %+v, want a WWN match on /dev/sdq", row)
	}
}

func TestScan_ParitySize(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	disks := disk.NewFakeProvider()
	for i, d := range spec.disks {
		size := d.size
		if d.slot == "parity" {
			size = 1 << 30
		}
		disks.AddDisk(fmt.Sprintf("/dev/sd%c", 'b'+i), disk.Disk{Serial: d.serial(), Size: size})
	}
	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	row := rowsFor(r, CheckParitySize)[0]
	if row.Status != StatusRefuse || !strings.Contains(row.Detail, "Q20") || r.Verdict != VerdictNoGo {
		t.Errorf("parity size row = %+v, verdict %s; want a refusal", row, r.Verdict)
	}
}

func TestScan_ParityCountAboveTwoIsRefused(t *testing.T) {
	files, spec := flashTree(t, "unraid-dual-parity")
	files["config/hoserva/disks.ini"] = append(files["config/hoserva/disks.ini"],
		[]byte("[\"parity3\"]\nidx=\"30\"\nid=\"FIXTURE_p3\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n")...)
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if row := rowsFor(r, CheckParity)[0]; row.Status != StatusRefuse || !strings.Contains(row.Detail, "Q19") {
		t.Errorf("parity row = %+v, want a refusal", row)
	}
}

func TestScan_SMART(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	disks := fixtureDisks(spec)
	list, _ := disks.List(context.Background())
	dev := map[string]string{}
	for _, d := range list {
		dev[d.Serial] = d.Device
	}
	disks.SetSMART(dev["disk1-hoserva-test"], disk.SMARTReport{ReallocatedSectors: 8})
	disks.SetSMART(dev["disk2-hoserva-test"], disk.SMARTReport{PendingSectors: 1})
	disks.SetSpinState(dev["disk3-hoserva-test"], disk.Standby)

	r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{"disk1", "disk2"} {
		if row := findRow(t, r, CheckSMART, slot); row.Status != StatusFlag || !strings.Contains(row.Detail, "Recommend aborting") {
			t.Errorf("%s row = %+v", slot, row)
		}
	}
	if row := findRow(t, r, CheckSMART, "disk3"); row.Status != StatusWarn || !strings.Contains(row.Detail, "not checked") {
		t.Errorf("disk3 (standby) row = %+v, want it reported as not checked", row)
	}
	if row := findRow(t, r, CheckSMART, "parity"); row.Status != StatusPass {
		t.Errorf("parity row = %+v", row)
	}
	for _, c := range disks.SMARTCalls() {
		if c.Mode != disk.SMARTPollRespectStandby || c.Woke {
			t.Errorf("SMART call %+v woke a disk or forced a poll", c)
		}
	}
	if st, _ := disks.SpinState(dev["disk3-hoserva-test"]); st != disk.Standby {
		t.Error("the standby disk was woken")
	}
}

type failingSMART struct{ *disk.FakeProvider }

func (f failingSMART) SMART(context.Context, string, disk.SMARTPollMode) (disk.SMARTReport, error) {
	return disk.SMARTReport{}, errors.New("device unreachable")
}

func TestScan_SMARTErrorIsNotAPass(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	r, err := scanner(failingSMART{fixtureDisks(spec)}).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rowsFor(r, CheckSMART) {
		if row.Status != StatusWarn || !strings.Contains(row.Detail, "not checked") {
			t.Errorf("a SMART failure read as %+v", row)
		}
	}
}

func TestScan_UID99(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	src := openZipBytes(t, zipOf(t, files, false))
	for name, tc := range map[string]struct {
		owner  func(int) (string, error)
		status Status
	}{
		"free":  {noUID, StatusPass},
		"taken": {func(uid int) (string, error) { return "someone", nil }, StatusFlag},
		"error": {func(int) (string, error) { return "", errors.New("nss down") }, StatusWarn},
	} {
		t.Run(name, func(t *testing.T) {
			r, err := (&Scanner{Disks: fixtureDisks(spec), UIDOwner: tc.owner}).Scan(context.Background(), src, ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if row := rowsFor(r, CheckUID99)[0]; row.Status != tc.status {
				t.Errorf("uid row = %+v, want %s", row, tc.status)
			}
		})
	}
}

func TestSystemUIDOwner_AFreeUIDIsNotAnError(t *testing.T) {
	if _, err := systemUIDOwner(1<<31 - 2); err != nil {
		t.Errorf("an unknown uid returned %v", err)
	}
	if owner, err := systemUIDOwner(0); err != nil || owner == "" {
		t.Errorf("uid 0 = %q, %v; want its account", owner, err)
	}
}

func TestScan_BootMode(t *testing.T) {
	setBoot := func(files map[string][]byte, boot string) {
		files["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":` + boot + `}`)
	}
	t.Run("internal", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
		setBoot(files, `{"mode":"internal","filesystem":"zfs","devices":[{"name":"nvme0n1","serial":"BOOTSER1","model":"M","size":"465G"},{"name":"nvme1n1","serial":"BOOTSER2","model":"M","size":"465G"}],"mirrored":true,"shared_with_data_pool":false}`)
		disks := fixtureDisks(spec)
		disks.AddDisk("/dev/nvme0n1", disk.Disk{Serial: "BOOTSER1", Size: 500 << 30})
		r, err := scanner(disks).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		rows := rowsFor(r, CheckBootDevice)
		if len(rows) != 3 || !strings.Contains(rows[0].Detail, "only configuration source") || !strings.Contains(rows[0].Detail, "mirrored") {
			t.Fatalf("boot rows = %+v", rows)
		}
		if !strings.Contains(findRow(t, r, CheckBootDevice, "nvme0n1").Detail, "/dev/nvme0n1") ||
			!strings.Contains(findRow(t, r, CheckBootDevice, "nvme1n1").Detail, "never offered a data or parity role") {
			t.Errorf("boot device rows = %+v", rows)
		}
		for _, row := range r.Rows {
			if row.Check == CheckMapping && strings.Contains(row.Detail, "nvme0n1") {
				t.Errorf("the boot device was offered a role: %+v", row)
			}
		}
	})
	t.Run("an array slot on the boot device is refused", func(t *testing.T) {
		files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
		setBoot(files, `{"mode":"internal","filesystem":"zfs","devices":[{"name":"sdc","serial":"disk1-hoserva-test","model":"M","size":"1G"}]}`)
		r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if row := findRow(t, r, CheckMapping, "disk1"); row.Status != StatusRefuse {
			t.Errorf("disk1 row = %+v, want a refusal", row)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		for name, mutate := range map[string]func(map[string][]byte){
			"no boot mode": func(f map[string][]byte) { setBoot(f, `{}`) },
			"no capture":   func(f map[string][]byte) { delete(f, "config/hoserva/capture.json") },
			"bad capture":  func(f map[string][]byte) { f["config/hoserva/capture.json"] = []byte("{") },
		} {
			files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
			mutate(files)
			r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if row := rowsFor(r, CheckBootDevice)[0]; row.Status != StatusWarn || !strings.Contains(row.Detail, "unknown") {
				t.Errorf("%s: boot row = %+v, want it reported as unknown", name, row)
			}
		}
	})
	t.Run("usb", func(t *testing.T) {
		r := scanVariant(t, "unraid-7x-xfs-single-parity", false)
		if row := rowsFor(r, CheckBootDevice)[0]; row.Status != StatusInfo || !strings.Contains(row.Detail, "USB stick") {
			t.Errorf("boot row = %+v", row)
		}
	})
}

func TestScan_CaptureVersionMismatchIsFlagged(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	files["config/hoserva/capture.json"] = []byte(`{"unraid_version":"6.12.15","boot":{"mode":"usb"}}`)
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, row := range rowsFor(r, CheckVersion) {
		flagged = flagged || row.Status == StatusFlag
	}
	if !flagged {
		t.Errorf("version rows = %+v, want a flag for the mismatch", rowsFor(r, CheckVersion))
	}
}

func TestScan_SyncEstimate(t *testing.T) {
	r := scanVariant(t, "unraid-7x-xfs-single-parity", false)
	row := rowsFor(r, CheckSyncEstimate)[0]
	if row.Status != StatusInfo || !strings.Contains(row.Detail, "hours") {
		t.Errorf("estimate row = %+v", row)
	}
}

type listFails struct{ *disk.FakeProvider }

func (listFails) List(context.Context) ([]disk.Disk, error) { return nil, errors.New("udev gone") }

func TestScan_FailureToListDisksIsAnErrorNotAReport(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	r, err := scanner(listFails{fixtureDisks(spec)}).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err == nil || r != nil {
		t.Fatalf("Scan = %v, %v; want an error and no report", r, err)
	}
}

func TestReport_MarkdownEscapesWhatTheSourceCarries(t *testing.T) {
	r := &Report{Verdict: VerdictGo, Rows: []Row{{Check: CheckMapping, Status: StatusPass, Subject: "a|b", Detail: "x\n| y <script> `z`"}}}
	md := r.Markdown()
	if strings.Contains(md, "<script>") || strings.Contains(md, "\n| y") || !strings.Contains(md, `a\|b`) {
		t.Errorf("unescaped source text in:\n%s", md)
	}
}

func TestReport_Verdicts(t *testing.T) {
	for _, tc := range []struct {
		rows []Status
		want Verdict
	}{
		{[]Status{StatusPass, StatusInfo}, VerdictGo},
		{[]Status{StatusPass, StatusWarn}, VerdictGoWithWarnings},
		{[]Status{StatusFlag, StatusRefuse, StatusWarn}, VerdictNoGo},
	} {
		r := &Report{}
		for _, s := range tc.rows {
			r.Rows = append(r.Rows, Row{Check: "x", Status: s})
		}
		r.conclude()
		if r.Verdict != tc.want {
			t.Errorf("%v -> %s, want %s", tc.rows, r.Verdict, tc.want)
		}
	}
}

type recordingSource struct {
	FlashSource
	read []string
}

func (r *recordingSource) Read(name string) ([]byte, error) {
	r.read = append(r.read, name)
	return r.FlashSource.Read(name)
}

// The array assignment is in a binary file Unraid owns; the scan takes the slot
// to disk identity from the capture instead and never reads it.
func TestScan_NeverReadsSuperDat(t *testing.T) {
	files, spec := flashTree(t, "unraid-7x-xfs-single-parity")
	if _, ok := files["config/super.dat"]; !ok {
		t.Fatal("the fixture flash has no super.dat, so this test proves nothing")
	}
	src := &recordingSource{FlashSource: openZipBytes(t, zipOf(t, files, false))}
	if _, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range src.read {
		if strings.HasPrefix(name, "config/super.") {
			t.Errorf("the scan read %s", name)
		}
	}
	if len(src.read) == 0 {
		t.Error("the scan read nothing")
	}
}
