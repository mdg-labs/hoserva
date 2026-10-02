package migrate

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
)

// The fixtures under testdata/unraid-fixtures are definitions, not built
// sources: scripts/devenv/unraid-fixture.sh renders disk.cfg, disks.ini,
// changes.txt, the junk a flash carries and the two zips from them at build
// time, in a lab. These helpers render the same files from the same definitions
// without a lab, so the scan runs against the committed flash trees.

const fixturesDir = "../../testdata/unraid-fixtures"

var fixtureVariants = []string{
	"unraid-6.12-xfs-single-parity",
	"unraid-7x-xfs-single-parity",
	"unraid-dual-parity",
	"unraid-named-pools",
	"unraid-no-cache",
}

type fixtureDisk struct {
	slot, kind, fs string
	size           int64
	pool           string
}

func (d fixtureDisk) serial() string { return d.slot + "-hoserva-test" }
func (d fixtureDisk) id() string     { return "FIXTURE_" + d.serial() }

type fixtureSpec struct {
	version string
	disks   []fixtureDisk
}

func parseSize(t *testing.T, s string) int64 {
	t.Helper()
	mult := map[byte]int64{'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}[s[len(s)-1]]
	n, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
	if err != nil || mult == 0 {
		t.Fatalf("bad size %q", s)
	}
	return n * mult
}

func readSpec(t *testing.T, variant string) fixtureSpec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, variant, "spec"))
	if err != nil {
		t.Fatal(err)
	}
	var spec fixtureSpec
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0 || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "unraid_version="):
			spec.version = strings.TrimPrefix(line, "unraid_version=")
		case fields[0] == "disk":
			d := fixtureDisk{slot: fields[1]}
			for _, kv := range fields[2:] {
				k, v, _ := strings.Cut(kv, "=")
				switch k {
				case "kind":
					d.kind = v
				case "fs":
					d.fs = v
				case "size":
					d.size = parseSize(t, v)
				case "pool":
					d.pool = v
				}
			}
			spec.disks = append(spec.disks, d)
		}
	}
	return spec
}

func slotIndex(slot string, spec fixtureSpec) int {
	switch {
	case slot == "parity":
		return 0
	case slot == "parity2":
		return 29
	case strings.HasPrefix(slot, "disk"):
		n, _ := strconv.Atoi(strings.TrimPrefix(slot, "disk"))
		return n
	}
	return 0
}

func walkTree(t *testing.T, root string, into map[string][]byte) {
	t.Helper()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		into[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// flashTree renders a variant's flash the way build_flash does.
func flashTree(t *testing.T, variant string) (map[string][]byte, fixtureSpec) {
	t.Helper()
	spec := readSpec(t, variant)
	files := map[string][]byte{}
	walkTree(t, filepath.Join(fixturesDir, "common", "flash"), files)
	if _, err := os.Stat(filepath.Join(fixturesDir, variant, "flash")); err == nil {
		walkTree(t, filepath.Join(fixturesDir, variant, "flash"), files)
	}

	files["changes.txt"] = []byte("# Version " + spec.version + " 2026-01-01\nAuthored for tests.\n")
	var cfg, ini strings.Builder
	cfg.WriteString("startArray=\"yes\"\ndefaultFsType=\"xfs\"\n")
	letters := "bcdefghij"
	for i, d := range spec.disks {
		n := slotIndex(d.slot, spec)
		typ := map[string]string{"parity": "Parity", "data": "Data", "pool": "Cache"}[d.kind]
		switch d.kind {
		case "parity":
			fmt.Fprintf(&cfg, "diskIdSlot.%d=\"-\"\n", n)
		case "data":
			fmt.Fprintf(&cfg, "diskIdSlot.%d=\"-\"\ndiskFsType.%d=\"%s\"\n", n, n, d.fs)
		}
		fsType := d.fs
		if fsType == "none" {
			fsType = ""
		}
		fmt.Fprintf(&ini, "[\"%s\"]\nidx=\"%d\"\nname=\"%s\"\ndevice=\"sd%c\"\nid=\"%s\"\nsize=\"%d\"\nstatus=\"DISK_OK\"\ntype=\"%s\"\nfsType=\"%s\"\n",
			d.slot, n, d.slot, letters[i], d.id(), d.size/1024, typ, fsType)
	}
	files["config/disk.cfg"] = []byte(cfg.String())
	files["config/hoserva/disks.ini"] = []byte(ini.String())
	files["config/super.dat"] = []byte{0, 1, 2, 3, 0xff}
	for _, k := range []string{"bzimage", "bzroot", "bzfirmware", "bzmodules"} {
		files[k] = []byte("placeholder, not a kernel image\n")
	}
	files["EFI/boot/bootx64.efi"] = []byte("placeholder\n")
	files["previous/bzimage"] = []byte("previous release\n")
	files["previous/changes.txt"] = []byte("# Version 6.11.0 2022-01-01\n")
	files["previous/config/disk.cfg"] = []byte("this must never be read\n")
	files[".git/HEAD"] = []byte("ref: refs/heads/master\n")
	files[".git/config/disk.cfg"] = []byte("this must never be read\n")
	files[".gitattributes"] = []byte("config/wireguard/** filter=noprivatekeys\n")
	files[".Spotlight-V100/Store"] = []byte("junk")
	files["System Volume Information/x"] = []byte("junk")
	for _, rel := range []string{"config/plugins/dockerMan/templates-user/my-notes.xml", "config/share.cfg", "config/shares/media.cfg", "changes.txt"} {
		files[filepath.ToSlash(filepath.Join(filepath.Dir(rel), "._"+filepath.Base(rel)))] = []byte("\x00\x05\x16\x07 AppleDouble")
	}
	for name, data := range files {
		for _, d := range spec.disks {
			data = bytes.ReplaceAll(data, []byte("@ID:"+d.slot+"@"), []byte(d.id()))
		}
		files[name] = data
	}
	return files, spec
}

// zipOf packs files. handZipped keeps previous/ the way a user zipping /boot
// would; Unraid's flash_backup leaves prev and previous out.
func zipOf(t *testing.T, files map[string][]byte, handZipped bool) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		if !handZipped && (strings.HasPrefix(n, "previous/") || strings.HasPrefix(n, "prev/")) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func openZipBytes(t *testing.T, data []byte) *ZipSource {
	t.Helper()
	src, err := OpenZip(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("OpenZip: %v", err)
	}
	return src
}

// fixtureDisks is this machine as the scan sees it: every array and pool disk
// of the variant, present, healthy and awake. A parity disk reports XFS, as a
// real one does.
func fixtureDisks(spec fixtureSpec) *disk.FakeProvider {
	p := disk.NewFakeProvider()
	for i, d := range spec.disks {
		fsType := d.fs
		if d.kind == "parity" {
			fsType = "xfs"
		}
		p.AddDisk(fmt.Sprintf("/dev/sd%c", "bcdefghij"[i]), disk.Disk{Serial: d.serial(), Size: d.size, Filesystem: fsType})
	}
	return p
}

func noUID(int) (string, error) { return "", nil }

// scanTime is the clock the scans in these tests run at: eight days after the
// fixtures' last parity check, so its age never depends on the day tests run.
var scanTime = time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)

func scanner(p disk.Provider) *Scanner {
	return &Scanner{Disks: p, UIDOwner: noUID, Now: func() time.Time { return scanTime }}
}

func rowsFor(r *Report, check string) []Row {
	var out []Row
	for _, row := range r.Rows {
		if row.Check == check {
			out = append(out, row)
		}
	}
	return out
}

func findRow(t *testing.T, r *Report, check, subject string) Row {
	t.Helper()
	for _, row := range r.Rows {
		if row.Check == check && row.Subject == subject {
			return row
		}
	}
	t.Fatalf("no %s row for %q in %+v", check, subject, r.Rows)
	return Row{}
}

func bytesReaderAt(b []byte) (*bytes.Reader, int64) { return bytes.NewReader(b), int64(len(b)) }
