package migrate

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/pool"
)

const inventoryVariant = "unraid-7x-xfs-single-parity"

// fakeDirs is a DiskDirs that answers by serial; a serial listed in fail is a
// disk whose directories cannot be read.
type fakeDirs struct {
	bySerial map[string][]string
	fail     map[string]bool
}

func (f fakeDirs) TopLevelDirs(_ context.Context, d disk.Disk) ([]string, error) {
	if f.fail[d.Serial] {
		return nil, errors.New("the filesystem would not mount read-only")
	}
	return f.bySerial[d.Serial], nil
}

// fixtureDirs is what the fixture's seed files leave on each disk of the 7.x
// variant: oldstuff has a config and no directory anywhere, and .Trash-99 is a
// hidden directory, not a share.
func fixtureDirs() fakeDirs {
	return fakeDirs{bySerial: map[string][]string{
		"disk1-hoserva-test": {"media", "documents", "appdata"},
		"disk2-hoserva-test": {"media", "documents", "isos", ".Trash-99"},
		"disk3-hoserva-test": {"media", "backup"},
		"cache-hoserva-test": {"appdata", "documents", "domains", "system"},
	}}
}

type scanCase struct {
	mutate func(files map[string][]byte)
	dirs   DiskDirs
	now    time.Time
	times  map[string]time.Time
}

func (c scanCase) run(t *testing.T) (*Report, map[string][]byte) {
	t.Helper()
	files, spec := flashTree(t, inventoryVariant)
	if c.mutate != nil {
		c.mutate(files)
	}
	s := scanner(fixtureDisks(spec))
	s.Dirs = c.dirs
	if !c.now.IsZero() {
		s.Now = func() time.Time { return c.now }
	}
	data := zipOf(t, files, false)
	if c.times != nil {
		data = zipWithTimes(t, files, c.times)
	}
	r, err := s.Scan(context.Background(), openZipBytes(t, data), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r, files
}

func scanInventory(t *testing.T, mutate func(map[string][]byte)) *Report {
	t.Helper()
	r, _ := scanCase{mutate: mutate}.run(t)
	return r
}

// zipWithTimes packs files with a modification time each: the one in times for
// that name, or a day before the committed captures otherwise.
func zipWithTimes(t *testing.T, files map[string][]byte, times map[string]time.Time) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for n := range files {
		if !strings.HasPrefix(n, "previous/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		at, ok := times[n]
		if !ok {
			at = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: at})
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

func detail(t *testing.T, r *Report, check, subject string) string {
	t.Helper()
	return findRow(t, r, check, subject).Detail
}

// hasRow reports whether a row of check has the status and a detail containing
// every part.
func hasRow(r *Report, check string, st Status, parts ...string) bool {
rows:
	for _, row := range rowsFor(r, check) {
		if row.Status != st {
			continue
		}
		for _, p := range parts {
			if !strings.Contains(row.Detail+" "+row.Subject, p) {
				continue rows
			}
		}
		return true
	}
	return false
}

func requireRow(t *testing.T, r *Report, check string, st Status, parts ...string) {
	t.Helper()
	if !hasRow(r, check, st, parts...) {
		t.Errorf("no %s row with status %s containing %q in %+v", check, st, parts, rowsFor(r, check))
	}
}

func refuteRow(t *testing.T, r *Report, check string, st Status, parts ...string) {
	t.Helper()
	if hasRow(r, check, st, parts...) {
		t.Errorf("unexpected %s row with status %s containing %q in %+v", check, st, parts, rowsFor(r, check))
	}
}

func shareByName(r *Report, name string) (Share, bool) {
	for _, sh := range r.Import.Shares {
		if sh.Name == name {
			return sh, true
		}
	}
	return Share{}, false
}

func templateByName(r *Report, name string) (TemplateEntry, bool) {
	for _, e := range r.Import.Templates {
		if e.Name == name {
			return e, true
		}
	}
	return TemplateEntry{}, false
}

func TestShares_AllocationAndCacheMapToHoservaSettings(t *testing.T) {
	r := scanInventory(t, nil)

	want := map[string]struct {
		policy pool.CreatePolicy
		mode   pool.CacheMode
	}{
		"media":     {pool.BalanceAcrossDisks, pool.ArrayOnly},
		"documents": {pool.BalanceAcrossDisks, pool.CacheThenMove},
		"backup":    {pool.FillDisksInOrder, pool.ArrayOnly},
		"appdata":   {pool.BalanceAcrossDisks, pool.CacheOnly},
		"domains":   {pool.BalanceAcrossDisks, pool.CacheOnly},
	}
	for name, w := range want {
		sh, ok := shareByName(r, name)
		if !ok {
			t.Errorf("share %s is not in the import", name)
			continue
		}
		if sh.CreatePolicy != w.policy || sh.CacheMode != w.mode {
			t.Errorf("%s = policy %q cache %q, want %q and %q", name, sh.CreatePolicy, sh.CacheMode, w.policy, w.mode)
		}
	}
	if len(r.Import.Shares) != 8 {
		t.Errorf("import has %d shares, want all 8 configs while the disks are not read", len(r.Import.Shares))
	}

	requireRow(t, r, CheckShares, StatusInfo, "8 shares configured")
	if got := findRow(t, r, CheckShares, "media"); got.Status != StatusFlag || !strings.Contains(got.Detail, "High-water: no exact equivalent, mapped to Balance across disks (mfs)") {
		t.Errorf("High-water row = %+v, want it flagged with the Q11 wording", got)
	}
	if got := findRow(t, r, CheckShares, "backup"); got.Status != StatusInfo || !strings.Contains(got.Detail, "Fill-up maps to Fill disks in order (ff)") {
		t.Errorf("Fill-up row = %+v", got)
	}
	if got := findRow(t, r, CheckShares, "documents"); got.Status != StatusInfo || !strings.Contains(got.Detail, "Most-free maps to Balance across disks (mfs)") || !strings.Contains(got.Detail, "cache setting yes maps to cache-then-move") {
		t.Errorf("Most-free row = %+v", got)
	}
	if got := detail(t, r, CheckShares, "domains"); !strings.Contains(got, "cache setting only maps to cache-only") {
		t.Errorf("domains row = %q", got)
	}
	if got := detail(t, r, CheckShares, "appdata"); !strings.Contains(got, "prefer maps to cache-only") || !strings.Contains(got, "fall through to the array") {
		t.Errorf("appdata row = %q, want prefer mapped to cache-only with the difference said", got)
	}
}

func TestShares_ConfigDefaultsComeFromShareCfgAndUnknownValuesAreWarnings(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/share.cfg"] = append(f["config/share.cfg"], []byte("shareUserExclude=\"disk3\"\nshareUserInclude=\"disk1,disk2\"\n")...)
		f["config/shares/media.cfg"] = []byte("shareAllocator=\"highwater\"\nshareSplitLevel=\"2\"\nshareFloor=\"50000000\"\nshareInclude=\"disk2\"\nshareExclude=\"\"\nshareUseCache=\"no\"\nshareExport=\"e\"\n")
		f["config/shares/documents.cfg"] = []byte("shareAllocator=\"roundrobin\"\nshareUseCache=\"sometimes\"\nshareExport=\"-\"\n")
	})
	media, _ := shareByName(r, "media")
	if media.SplitLevel != "2" || media.Floor != "50000000" || strings.Join(media.Include, ",") != "disk2" || strings.Join(media.Exclude, ",") != "disk3" {
		t.Errorf("media = %+v, want split 2, floor, its own include disk2 and the excluded disk3 from share.cfg", media)
	}
	backup, _ := shareByName(r, "backup")
	if strings.Join(backup.Include, ",") != "disk1,disk2" {
		t.Errorf("backup include = %v, want share.cfg's default", backup.Include)
	}
	docs, _ := shareByName(r, "documents")
	if docs.CreatePolicy != "" || docs.CacheMode != "" {
		t.Errorf("documents = %+v, want no mapping for values Hoserva does not know", docs)
	}
	if got := findRow(t, r, CheckShares, "documents"); got.Status != StatusWarn || !strings.Contains(got.Detail, "roundrobin") || !strings.Contains(got.Detail, "sometimes") || !strings.Contains(got.Detail, "not exported over SMB") {
		t.Errorf("documents row = %+v", got)
	}
}

func TestShares_AConfigThatDoesNotParseIsAWarningAndIsNotImported(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/shares/media.cfg"] = []byte("this is not a setting\n")
	})
	if _, ok := shareByName(r, "media"); ok {
		t.Error("an unreadable share config was imported")
	}
	if got := findRow(t, r, CheckShares, "media"); got.Status != StatusWarn || !strings.Contains(got.Detail, "not carried over") {
		t.Errorf("media row = %+v", got)
	}
}

func TestShares_NoConfigAtAllIsAWarningNotAnEmptyList(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		for name := range f {
			if strings.HasPrefix(name, "config/shares/") {
				delete(f, name)
			}
		}
	})
	requireRow(t, r, CheckShares, StatusWarn, "No share configuration was found")
}

func TestShares_AnOrphanConfigIsReportedAndNotImported(t *testing.T) {
	r, _ := scanCase{dirs: fixtureDirs()}.run(t)
	if _, ok := shareByName(r, "oldstuff"); ok {
		t.Error("the orphan share config was passed to the import")
	}
	if len(r.Import.Shares) != 7 {
		t.Errorf("import has %d shares, want the 7 with a directory", len(r.Import.Shares))
	}
	if got := findRow(t, r, CheckShares, "oldstuff"); got.Status != StatusInfo || !strings.Contains(got.Detail, "orphan") {
		t.Errorf("oldstuff row = %+v", got)
	}
	requireRow(t, r, CheckShares, StatusInfo, "7 shares configured; 1 config with no share behind it")
	if hasRow(r, CheckShares, StatusInfo, "does not read the disks' directories") {
		t.Error("the scan says it does not read the directories although it did")
	}
}

func TestShares_ACaptureShareListSavesAConfigFromBeingAnOrphan(t *testing.T) {
	r, _ := scanCase{dirs: fixtureDirs(), mutate: func(f map[string][]byte) {
		f["config/hoserva/shares.ini"] = []byte("[\"media\"]\nname=\"media\"\nhasCfg=\"yes\"\n[\"oldstuff\"]\nname=\"oldstuff\"\nhasCfg=\"yes\"\n")
	}}.run(t)
	if _, ok := shareByName(r, "oldstuff"); !ok {
		t.Error("a share the capture lists was dropped as an orphan")
	}
}

// Dropping a real share is worse than keeping an orphan, so a disk that cannot
// be matched or read means no config is called an orphan.
func TestShares_NothingIsAnOrphanWhileADiskIsUnmatchedOrUnreadable(t *testing.T) {
	missing := fixtureDirs()
	for _, tc := range []struct {
		name string
		dirs fakeDirs
		pre  func(f map[string][]byte)
		want string
	}{
		{"unreadable", fakeDirs{bySerial: missing.bySerial, fail: map[string]bool{"disk2-hoserva-test": true}}, nil, "disk2: the filesystem would not mount"},
		{"unmatched", missing, func(f map[string][]byte) {
			f["config/hoserva/disks.ini"] = bytes.ReplaceAll(f["config/hoserva/disks.ini"], []byte("FIXTURE_disk3-hoserva-test"), []byte("FIXTURE_not-attached"))
		}, "disk3: no disk on this machine has this serial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := scanCase{dirs: tc.dirs, mutate: tc.pre}.run(t)
			if _, ok := shareByName(r, "oldstuff"); !ok {
				t.Error("a config was called an orphan although a disk's directories are not known")
			}
			requireRow(t, r, CheckShares, StatusWarn, tc.want, "no config is called an orphan")
			if got := findRow(t, r, CheckShares, "oldstuff"); strings.Contains(got.Detail, "orphan") {
				t.Errorf("oldstuff row = %+v", got)
			}
		})
	}
}

func TestShares_AShareWithNoDirectoryOnAMatchedDataDiskIsFlagged(t *testing.T) {
	r, _ := scanCase{dirs: fixtureDirs()}.run(t)
	if got := findRow(t, r, CheckShares, "domains"); got.Status != StatusFlag || !strings.Contains(got.Detail, "no directory on any matched data disk") || !strings.Contains(got.Detail, "only on pool cache") {
		t.Errorf("domains row = %+v, want it flagged and placed on the pool", got)
	}
	if got := findRow(t, r, CheckShares, "isos"); !strings.Contains(got.Detail, "directory on disk2") {
		t.Errorf("isos row = %+v, want the disk that holds it", got)
	}
	if got := findRow(t, r, CheckShares, "backup"); got.Status != StatusInfo || !strings.Contains(got.Detail, "directory on disk3") {
		t.Errorf("backup row = %+v", got)
	}
}

func TestTemplates_AreClassedByTheCaptureOnTheirNameNotTheirFileName(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		const dir = "config/plugins/dockerMan/templates-user/"
		// A file named for a running container whose <Name> is another's, and a
		// template whose <Name> differs from the container's only in case.
		f[dir+"gateway-copy.xml"] = []byte("<?xml version=\"1.0\"?>\n<Container version=\"2\"><Name>Gateway</Name></Container>\n")
		f[dir+"photos.xml"] = []byte("<?xml version=\"1.0\"?>\n<Container version=\"2\"><Name>pictures</Name></Container>\n")
	})

	want := map[string]struct {
		class TemplateClass
		pos   int
		wait  int
	}{
		"notes":       {ClassAutostart, 1, 0},
		"photos":      {ClassAutostart, 2, 30},
		"mediaserver": {ClassAutostart, 3, 0},
		"gateway":     {ClassRunning, 0, 0},
		"syncer":      {ClassStopped, 0, 0},
		"old-thing":   {ClassTemplateOnly, 0, 0},
		"unused-tool": {ClassTemplateOnly, 0, 0},
		"Gateway":     {ClassTemplateOnly, 0, 0},
		"pictures":    {ClassTemplateOnly, 0, 0},
	}
	for name, w := range want {
		e, ok := templateByName(r, name)
		if !ok {
			t.Errorf("template %q is not in the import", name)
			continue
		}
		if e.Class != w.class || e.AutostartPosition != w.pos || e.AutostartWaitSeconds != w.wait {
			t.Errorf("%s = %+v, want %s at %d waiting %d", name, e, w.class, w.pos, w.wait)
		}
	}
	if e, _ := templateByName(r, "photos"); !strings.HasSuffix(e.File, "my-Photos.xml") {
		t.Errorf("photos came from %s, want my-Photos.xml (a file name that is not its <Name>)", e.File)
	}
	requireRow(t, r, CheckTemplates, StatusInfo, "9 templates parsed: 3 autostart, 1 running, 1 stopped, 4 template only", "5 are installed")
}

func TestTemplates_ADockerManContainerWithNoTemplateAndAHandMadeOneAreFlaggedByName(t *testing.T) {
	r := scanInventory(t, nil)
	if got := findRow(t, r, CheckContainers, "dbtool"); got.Status != StatusFlag || !strings.Contains(got.Detail, "no template") {
		t.Errorf("dbtool row = %+v", got)
	}
	if got := findRow(t, r, CheckContainers, "handmade"); got.Status != StatusFlag || !strings.Contains(got.Detail, "Created by hand") {
		t.Errorf("handmade row = %+v", got)
	}
	requireRow(t, r, CheckContainers, StatusInfo, "8 containers in the capture: 6 from the Docker page (dockerMan), 1 from Compose Manager, 1 created by hand")
	for _, name := range []string{"notes", "gateway", "syncer", "stack-web"} {
		for _, row := range rowsFor(r, CheckContainers) {
			if row.Status == StatusFlag && row.Subject == name {
				t.Errorf("%s is flagged: %+v", name, row)
			}
		}
	}
}

func TestTemplates_ComposeManagerProjectsKeepTheirComposeFile(t *testing.T) {
	r := scanInventory(t, nil)
	if len(r.Import.ComposeProjects) != 1 {
		t.Fatalf("compose projects = %+v", r.Import.ComposeProjects)
	}
	p := r.Import.ComposeProjects[0]
	if p.Name != "stack" || p.File != "config/plugins/compose.manager/projects/stack/compose.yaml" || strings.Join(p.Containers, ",") != "stack-web" {
		t.Errorf("project = %+v", p)
	}
	requireRow(t, r, CheckContainers, StatusInfo, "stack", "offered as its own compose.yaml")

	r = scanInventory(t, func(f map[string][]byte) {
		delete(f, "config/plugins/compose.manager/projects/stack/compose.yaml")
	})
	requireRow(t, r, CheckContainers, StatusWarn, "stack", "compose.yaml is not in the source")
}

func TestTemplates_AFileThatDoesNotParseIsNamed(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		const dir = "config/plugins/dockerMan/templates-user/"
		f[dir+"broken.xml"] = []byte("<Container><Name>x</Container")
		f[dir+"nameless.xml"] = []byte("<Container version=\"2\"><Repository>x</Repository></Container>")
		f[dir+"other.xml"] = []byte("<Something><Name>x</Name></Something>")
	})
	requireRow(t, r, CheckTemplates, StatusWarn, "broken.xml", "not valid XML")
	requireRow(t, r, CheckTemplates, StatusWarn, "nameless.xml", "no <Name>")
	requireRow(t, r, CheckTemplates, StatusWarn, "other.xml", "not Container")
	requireRow(t, r, CheckTemplates, StatusInfo, "7 templates parsed")
}

func TestTemplates_TwoFilesNamingOneContainerAreWarned(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/plugins/dockerMan/templates-user/my-notes-old.xml"] = []byte("<Container version=\"2\"><Name>notes</Name></Container>")
	})
	requireRow(t, r, CheckTemplates, StatusWarn, "notes (my-notes-old.xml, my-notes.xml)")
}

func dropCapture(f map[string][]byte) {
	for name := range f {
		if strings.HasPrefix(name, "config/hoserva/") && name != "config/hoserva/disks.ini" {
			delete(f, name)
		}
	}
}

func TestTemplates_AMissingCaptureIsAWarningAndEveryTemplateIsUnknown(t *testing.T) {
	r := scanInventory(t, dropCapture)
	if len(r.Import.Templates) != 7 {
		t.Fatalf("templates = %+v", r.Import.Templates)
	}
	for _, e := range r.Import.Templates {
		if e.Class != ClassUnknown || e.AutostartPosition != 0 {
			t.Errorf("%s = %+v, want unknown", e.Name, e)
		}
	}
	requireRow(t, r, CheckContainers, StatusWarn, "capture was not found", "Phase A step 0", "unknown")
	requireRow(t, r, CheckTemplates, StatusInfo, "7 templates parsed", "every one is classed unknown", "pre-selects none")
	if r.Verdict == VerdictNoGo {
		t.Errorf("a missing capture refused the migration: %s", r.Verdict)
	}
	for _, row := range r.Rows {
		if row.Check == CheckContainers && row.Status == StatusFlag {
			t.Errorf("a container is flagged without a container list: %+v", row)
		}
	}
}

func TestTemplates_ACaptureFileThatDoesNotParseIsNamedAndFallsBackLikeAMissingOne(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/hoserva/containers.json"] = []byte("{not json")
		f["config/hoserva/networks.json"] = []byte("[{\"Driver\":\"bridge\"}]")
	})
	for _, e := range r.Import.Templates {
		if e.Class != ClassUnknown {
			t.Errorf("%s = %s, want unknown", e.Name, e.Class)
		}
	}
	requireRow(t, r, CheckContainers, StatusWarn, "containers.json", "does not parse", "every template is classed unknown")
	requireRow(t, r, CheckContainers, StatusWarn, "networks.json", "does not parse")
	if len(r.Import.Networks) != 0 {
		t.Errorf("networks = %+v", r.Import.Networks)
	}
}

func TestTemplates_DockerNotRunningAtCaptureSaysSo(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		delete(f, "config/hoserva/containers.json")
		f["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":{"mode":"usb"},"docker":{"state":"stopped","directory_location":"cache","writable_layers":null}}`)
	})
	requireRow(t, r, CheckContainers, StatusWarn, "Docker was stopped", "unknown")
	requireRow(t, r, CheckCache, StatusWarn, "Docker was not running at capture", "writable-layer size")
}

// committedCaptureTime is the captured_at of the committed capture of the
// variant these tests scan.
func committedCaptureTime(t *testing.T) time.Time {
	t.Helper()
	files, _ := flashTree(t, inventoryVariant)
	var capture struct {
		CapturedAt string `json:"captured_at"`
	}
	if err := json.Unmarshal(files["config/hoserva/capture.json"], &capture); err != nil {
		t.Fatalf("committed capture.json: %v", err)
	}
	at, err := time.Parse(time.RFC3339, capture.CapturedAt)
	if err != nil {
		t.Fatalf("committed capture.json captured_at %q: %v", capture.CapturedAt, err)
	}
	return at
}

func TestTemplates_ACaptureOlderThanTheNewestTemplateIsStale(t *testing.T) {
	const tmpl = "config/plugins/dockerMan/templates-user/my-gateway.xml"
	captured := committedCaptureTime(t)
	for _, tc := range []struct {
		name  string
		saved time.Time
		stale bool
	}{
		{"template saved after the capture", captured.Add(time.Hour), true},
		{"template saved before the capture", captured.Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := scanCase{times: map[string]time.Time{tmpl: tc.saved}}.run(t)
			if got := hasRow(r, CheckCapture, StatusWarn, "may be stale", captured.Format(time.RFC3339), tc.saved.Format(time.RFC3339)); got != tc.stale {
				t.Errorf("stale warning = %v, want %v: %+v", got, tc.stale, rowsFor(r, CheckCapture))
			}
			if r.Verdict == VerdictNoGo {
				t.Error("a stale capture refused the migration")
			}
		})
	}
}

func TestTemplates_AStickSourceReportsAStaleCaptureToo(t *testing.T) {
	r := newStickRig(t, inventoryVariant)
	dir := t.TempDir()
	if err := writeTree(dir, r.files); err != nil {
		t.Fatal(err)
	}
	src, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	scan := func() *Report {
		rep, err := scanner(fixtureDisks(r.spec)).Scan(context.Background(), src, ScanOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if rep := scan(); hasRow(rep, CheckCapture, StatusWarn, "may be stale") {
		t.Fatalf("a flash saved before its capture reads as stale: %+v", rowsFor(rep, CheckCapture))
	}
	later := committedCaptureTime(t).Add(time.Hour)
	if err := os.Chtimes(dir+"/config/plugins/dockerMan/templates-user/my-notes.xml", later, later); err != nil {
		t.Fatal(err)
	}
	if rep := scan(); !hasRow(rep, CheckCapture, StatusWarn, "may be stale") {
		t.Errorf("a template saved after the capture is not reported: %+v", rowsFor(rep, CheckCapture))
	}
	if err := src.Err(); err != nil {
		t.Errorf("source error: %v", err)
	}
}

func TestTemplates_NetworkDefinitionsAreKeptForTheConverter(t *testing.T) {
	r := scanInventory(t, nil)
	var br0 *Network
	for i := range r.Import.Networks {
		if r.Import.Networks[i].Name == "br0" {
			br0 = &r.Import.Networks[i]
		}
	}
	if br0 == nil {
		t.Fatalf("networks = %+v", r.Import.Networks)
	}
	if br0.Driver != "ipvlan" || len(br0.Subnets) != 1 || br0.Subnets[0].Subnet != "192.168.50.0/24" || br0.Subnets[0].Gateway != "192.168.50.1" || br0.Options["parent"] != "ens20" {
		t.Errorf("br0 = %+v", br0)
	}
	if len(r.Import.Networks) != 5 {
		t.Errorf("networks = %d, want all 5 captured", len(r.Import.Networks))
	}
	requireRow(t, r, CheckContainers, StatusInfo, "5 Docker networks captured", "br0", "stack_default")
}

func TestUserScripts_AreReportedByNameScheduleAndStateAndNeverRead(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/user.scripts/scripts/unscheduled/script"] = []byte("#!/bin/bash\necho SECRET-SCRIPT-BODY\n")
	files["config/plugins/user.scripts/scripts/unscheduled/name"] = []byte("Weekly cleanup\n")
	files["config/plugins/user.scripts/customSchedule.cron"] = append(files["config/plugins/user.scripts/customSchedule.cron"],
		[]byte("# a comment\n0 4 * * 0 /usr/local/emhttp/plugins/user.scripts/startCustom.php /boot/config/plugins/user.scripts/scripts/gone/script > /dev/null 2>&1\n")...)
	src := &recordingSource{FlashSource: openZipBytes(t, zipOf(t, files, false))}
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireRow(t, r, CheckUserScripts, StatusInfo, "2 User Scripts entries found", "never executed or translated (Q83)")
	if got := findRow(t, r, CheckUserScripts, "nightly-report"); !strings.Contains(got.Detail, "30 2 * * *") || !strings.Contains(got.Detail, "Scheduled") {
		t.Errorf("nightly-report row = %+v", got)
	}
	if got := findRow(t, r, CheckUserScripts, "Weekly cleanup"); !strings.Contains(got.Detail, "No schedule in customSchedule.cron") {
		t.Errorf("unscheduled row = %+v", got)
	}
	requireRow(t, r, CheckUserScripts, StatusWarn, "gone", "no entry of that name")
	for _, name := range src.read {
		if strings.HasSuffix(name, "/script") {
			t.Errorf("the scan read the script %s", name)
		}
	}
	md := r.Markdown()
	if strings.Contains(md, "SECRET-SCRIPT-BODY") {
		t.Error("a script's content is in the report")
	}
}

func TestUserScripts_ImportKeepsEachScriptsNameAndScheduleForTheChecklist(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/user.scripts/scripts/unscheduled/script"] = []byte("#!/bin/bash\necho SECRET-SCRIPT-BODY\n")
	files["config/plugins/user.scripts/scripts/unscheduled/name"] = []byte("Weekly cleanup\n")
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := []UserScript{{Name: "nightly-report", Schedule: "30 2 * * *"}, {Name: "Weekly cleanup"}}
	if !reflect.DeepEqual(r.Import.UserScripts, want) {
		t.Errorf("Import.UserScripts = %+v, want %+v", r.Import.UserScripts, want)
	}
	if data, _ := json.Marshal(r.Import.UserScripts); strings.Contains(string(data), "SECRET-SCRIPT-BODY") {
		t.Error("a script's content is in the import record")
	}
}

func TestUserScripts_ScheduleOfAFolderWithSpacesAndAShorthandIsReadAndUnreadableLinesWarn(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/user.scripts/scripts/Clear Docker Logs/script"] = []byte("#!/bin/bash\n")
	files["config/plugins/user.scripts/scripts/Daily Tidy/script"] = []byte("#!/bin/bash\n")
	files["config/plugins/user.scripts/scripts/Daily Tidy/name"] = []byte("Daily Tidy\n")
	files["config/plugins/user.scripts/customSchedule.cron"] = append(files["config/plugins/user.scripts/customSchedule.cron"],
		[]byte("0 3 * * * /usr/local/emhttp/plugins/user.scripts/startCustom.php /boot/config/plugins/user.scripts/scripts/Clear Docker Logs/script > /dev/null 2>&1\n"+
			"@daily /usr/local/emhttp/plugins/user.scripts/startCustom.php /boot/config/plugins/user.scripts/scripts/Daily Tidy/script > /dev/null 2>&1\n"+
			"not a cron line at all\n")...)
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := findRow(t, r, CheckUserScripts, "Clear Docker Logs"); !strings.Contains(got.Detail, "0 3 * * *") || !strings.Contains(got.Detail, "Scheduled") {
		t.Errorf("space-named row = %+v", got)
	}
	if got := findRow(t, r, CheckUserScripts, "Daily Tidy"); !strings.Contains(got.Detail, "(@daily)") || !strings.Contains(got.Detail, "Scheduled") {
		t.Errorf("@daily row = %+v", got)
	}
	requireRow(t, r, CheckUserScripts, StatusWarn, "customSchedule.cron", "Line 4 of customSchedule.cron is not")
	if strings.Contains(r.Markdown(), "not a cron line at all") {
		t.Error("an unreadable cron line's text is in the report")
	}
	for _, row := range rowsFor(r, CheckUserScripts) {
		if row.Status == StatusWarn && (row.Subject == "Clear Docker Logs" || row.Subject == "Daily Tidy") {
			t.Errorf("a scheduled script was reported as having no matching entry: %+v", row)
		}
	}
}

func TestUserScripts_OnlyThePluginsOwnCommandIsReadAndEverythingElseIsUnreadableByLineNumber(t *testing.T) {
	const start = "/usr/local/emhttp/plugins/user.scripts/startCustom.php"
	const script = "/boot/config/plugins/user.scripts/scripts/x/script"
	lines := []string{
		"0 3 * * user:pass " + start + " " + script,
		"0 3 * * " + start + " " + script,
		"0 3 * * * user:pass " + start + " " + script,
		"0 3 * * * curl -u admin:hunter2-SECRET https://hc.example/ping",
		"0 3 * * * /usr/bin/other.php " + script,
		"@daily user:pass " + start + " " + script,
		"@nightly " + start + " " + script,
		"0 3 * * * " + start + " " + script + " ; curl -u admin:hunter2-SECRET https://hc.example/ping",
	}
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/user.scripts/scripts/x/script"] = []byte("#!/bin/bash\n")
	files["config/plugins/user.scripts/customSchedule.cron"] = []byte(strings.Join(lines, "\n") + "\n")
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range lines {
		requireRow(t, r, CheckUserScripts, StatusWarn, "customSchedule.cron", fmt.Sprintf("Line %d of customSchedule.cron is not", i+1))
	}
	if got := findRow(t, r, CheckUserScripts, "x"); !strings.Contains(got.Detail, "No schedule in customSchedule.cron") {
		t.Errorf("a line that is not the plugin's was reported as a schedule: %+v", got)
	}
	md := r.Markdown()
	for _, leak := range []string{"user:pass", "hunter2", "curl", "other.php", "@nightly"} {
		if strings.Contains(md, leak) {
			t.Errorf("%q from an unreadable cron line is in the report", leak)
		}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "user:pass") || strings.Contains(string(raw), "hunter2") {
		t.Error("an unreadable cron line is in the report JSON")
	}
}

func TestUserScripts_AShellMetacharacterInTheScriptPathMakesTheLineUnreadableByLineNumber(t *testing.T) {
	const start = "/usr/local/emhttp/plugins/user.scripts/startCustom.php"
	const dir = "/boot/config/plugins/user.scripts/scripts/"
	lines := []string{
		"0 3 * * * " + start + " " + dir + "l; TOKEN=SECRET-P1 ./script",
		"0 3 * * * " + start + " " + dir + "l& SECRET-P1/script",
		"0 3 * * * " + start + " " + dir + "l| SECRET-P1/script",
		"0 3 * * * " + start + " " + dir + "l$SECRET-P1/script",
		"0 3 * * * " + start + " " + dir + "l`SECRET-P1`/script",
		"0 3 * * * " + start + " " + dir + "l<SECRET-P1/script",
		"0 3 * * * " + start + " " + dir + "l>SECRET-P1/script",
		"0 3 * * * " + start + " " + dir + "Clear Docker Logs/script > /dev/null 2>&1",
	}
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/user.scripts/scripts/Clear Docker Logs/script"] = []byte("#!/bin/bash\n")
	files["config/plugins/user.scripts/customSchedule.cron"] = []byte(strings.Join(lines, "\n") + "\n")
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), openZipBytes(t, zipOf(t, files, false)), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(lines)-1; i++ {
		requireRow(t, r, CheckUserScripts, StatusWarn, "customSchedule.cron", fmt.Sprintf("Line %d of customSchedule.cron is not", i+1))
	}
	if got := findRow(t, r, CheckUserScripts, "Clear Docker Logs"); !strings.Contains(got.Detail, "0 3 * * *") || !strings.Contains(got.Detail, "Scheduled") {
		t.Errorf("a folder with spaces stopped being readable: %+v", got)
	}
	for _, row := range rowsFor(r, CheckUserScripts) {
		if strings.Contains(row.Subject, "SECRET-P1") || strings.Contains(row.Detail, "SECRET-P1") {
			t.Errorf("text from a metacharacter line is a row: %+v", row)
		}
	}
	if strings.Contains(r.Markdown(), "SECRET-P1") {
		t.Error("text from a metacharacter line is in the report Markdown")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SECRET-P1") {
		t.Error("text from a metacharacter line is in the report JSON")
	}
}

func TestUserScripts_EntriesThatCannotBeFoundAreNeverReportedAsNone(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		for name := range f {
			if strings.HasPrefix(name, "config/plugins/user.scripts/scripts/") {
				delete(f, name)
			}
		}
	})
	requireRow(t, r, CheckUserScripts, StatusWarn, "plugin is installed", "no entry was found", "not a finding that there are no scripts")
	for _, row := range rowsFor(r, CheckUserScripts) {
		if strings.Contains(strings.ToLower(row.Detail), "no scripts.") && row.Status != StatusWarn {
			t.Errorf("row reads as none: %+v", row)
		}
	}

	r = scanInventory(t, func(f map[string][]byte) {
		for name := range f {
			if strings.HasPrefix(name, "config/plugins/user.scripts/") {
				delete(f, name)
			}
		}
	})
	requireRow(t, r, CheckUserScripts, StatusInfo, "No User Scripts plugin files were found", "not where this scan looks")
}

func TestParityHistory_ReadsTheCaptureAndFallsBackToTheLog(t *testing.T) {
	varINI := func(synced, exit, errs, resync string) func(map[string][]byte) {
		return func(f map[string][]byte) {
			f["config/hoserva/var.ini"] = []byte("sbSynced=\"" + synced + "\"\nsbSyncExit=\"" + exit + "\"\nsbSyncErrs=\"" + errs + "\"\nmdResync=\"" + resync + "\"\n")
		}
	}
	recent := "1790300000" // 2026-09-25, eight days before scanTime
	cases := []struct {
		name   string
		mutate func(map[string][]byte)
		now    time.Time
		status Status
		parts  []string
	}{
		{"clean and recent", nil, scanTime, StatusPass, []string{"2026-09-25", "capture's var.ini", "0 errors"}},
		{"not clean", varINI(recent, "0", "3", "0"), scanTime, StatusWarn, []string{"not clean", "3 errors"}},
		{"cancelled", varINI(recent, "-4", "0", "0"), scanTime, StatusWarn, []string{"not clean", "exit code -4"}},
		{"not recent", nil, scanTime.Add(60 * 24 * time.Hour), StatusWarn, []string{"days old"}},
		{"running at capture", varINI(recent, "0", "0", "123456"), scanTime, StatusWarn, []string{"still running when the capture was taken"}},
		{"log when var.ini has no check", func(f map[string][]byte) { varINI("0", "0", "0", "0")(f) }, scanTime, StatusPass, []string{"2026-09-07", "config/parity-checks.log"}},
		{"log when there is no var.ini", func(f map[string][]byte) { delete(f, "config/hoserva/var.ini") }, scanTime, StatusPass, []string{"2026-09-07", "config/parity-checks.log"}},
		{"var.ini that does not parse", func(f map[string][]byte) { f["config/hoserva/var.ini"] = []byte("garbage line\n") }, scanTime, StatusPass, []string{"2026-09-07", "config/parity-checks.log"}},
		{"log with a bad exit code", func(f map[string][]byte) {
			delete(f, "config/hoserva/var.ini")
			f["config/parity-checks.log"] = []byte("2026 Sep 07 03:00:03|31877|125.5 MB/s|-4|0\n")
		}, scanTime, StatusWarn, []string{"not clean", "exit code -4"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := scanCase{mutate: tc.mutate, now: tc.now}.run(t)
			requireRow(t, r, CheckParityHistory, tc.status, tc.parts...)
		})
	}

	r := scanInventory(t, func(f map[string][]byte) { f["config/hoserva/var.ini"] = []byte("garbage line\n") })
	requireRow(t, r, CheckParityHistory, StatusWarn, "var.ini", "does not parse")
}

func TestParityHistory_NoHistoryWarnsAndPointsAtTheChecklist(t *testing.T) {
	for name, mutate := range map[string]func(map[string][]byte){
		"no capture and no log": func(f map[string][]byte) { dropCapture(f); delete(f, "config/parity-checks.log") },
		"an empty log":          func(f map[string][]byte) { dropCapture(f); f["config/parity-checks.log"] = nil },
		"an unreadable log":     func(f map[string][]byte) { dropCapture(f); f["config/parity-checks.log"] = []byte("nonsense\n") },
	} {
		t.Run(name, func(t *testing.T) {
			r := scanInventory(t, mutate)
			requireRow(t, r, CheckParityHistory, StatusWarn, "No parity-check history", "cannot be confirmed", "pre-cutover checklist")
			refuteRow(t, r, CheckParityHistory, StatusPass)
		})
	}
}

func TestCache_ReportsWhatIsStillOnTheCache(t *testing.T) {
	r := scanInventory(t, nil)
	if got := findRow(t, r, CheckCache, "appdata"); got.Status != StatusWarn || !strings.Contains(got.Detail, "/mnt/user/appdata/") || !strings.Contains(got.Detail, "cache setting prefer") || !strings.Contains(got.Detail, "Phase A step 5") {
		t.Errorf("appdata row = %+v", got)
	}
	for _, name := range []string{"domains", "system"} {
		if got := findRow(t, r, CheckCache, name); got.Status != StatusWarn || !strings.Contains(got.Detail, "Phase A step 5") {
			t.Errorf("%s row = %+v", name, got)
		}
	}
	for _, row := range rowsFor(r, CheckCache) {
		if row.Subject == "media" || row.Subject == "backup" || row.Subject == "isos" {
			t.Errorf("a share set to no cache is listed as on the cache: %+v", row)
		}
	}
	if got := findRow(t, r, CheckCache, "libvirt.img"); got.Status != StatusInfo || !strings.Contains(got.Detail, "VM Manager is disabled") {
		t.Errorf("libvirt row = %+v", got)
	}
}

func TestCache_TheDisksHoldingAShareAreNamedAndALeftoverOnTheCacheWarns(t *testing.T) {
	r, _ := scanCase{dirs: fixtureDirs(), mutate: func(f map[string][]byte) {
		f["config/shares/appdata.cfg"] = bytes.ReplaceAll(f["config/shares/appdata.cfg"], []byte(`shareUseCache="prefer"`), []byte(`shareUseCache="no"`))
		f["config/shares/documents.cfg"] = bytes.ReplaceAll(f["config/shares/documents.cfg"], []byte(`shareUseCache="yes"`), []byte(`shareUseCache="no"`))
	}}.run(t)
	if got := findRow(t, r, CheckCache, "appdata"); got.Status != StatusWarn || !strings.Contains(got.Detail, "its directory is on disk1, pool cache") {
		t.Errorf("appdata row = %+v, want the cache holding a directory to warn whatever the setting says", got)
	}
	if got := findRow(t, r, CheckCache, "documents"); got.Status != StatusWarn || !strings.Contains(got.Detail, "directory on pool cache") {
		t.Errorf("documents row = %+v, want its leftover on the cache named", got)
	}
}

func TestCache_LibvirtAndDockerStorageComeFromTheCaptureAndTheConfig(t *testing.T) {
	capture := func(libvirt, dockerDir, layers string) func(map[string][]byte) {
		return func(f map[string][]byte) {
			f["config/hoserva/capture.json"] = []byte(`{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":{"mode":"usb"},"docker":{"state":"running","directory_location":"` + dockerDir + `","writable_layers":` + layers + `},"libvirt_img_location":"` + libvirt + `"}`)
		}
	}
	r := scanInventory(t, capture("cache", "cache", `[{"container":"big","bytes":3221225472},{"container":"small","bytes":1048576},{"container":"empty","bytes":0},{"container":"unmeasured","bytes":null}]`))
	if got := findRow(t, r, CheckCache, "libvirt.img"); got.Status != StatusWarn || !strings.Contains(got.Detail, "on the cache") || !strings.Contains(got.Detail, "Phase A step 5 must move it") {
		t.Errorf("libvirt row = %+v", got)
	}
	got := findRow(t, r, CheckCache, "Docker storage")
	if got.Status != StatusWarn || !strings.Contains(got.Detail, "It is not moved") || !strings.Contains(got.Detail, "big 3.0 GiB, small 1.0 MiB") || !strings.Contains(got.Detail, "1 container could not be measured") {
		t.Errorf("docker row = %+v", got)
	}
	if strings.Contains(got.Detail, "Phase A step 5 must move it") {
		t.Errorf("docker row tells the user to move Docker's storage: %q", got.Detail)
	}

	r = scanInventory(t, capture("array", "array", "[]"))
	if got := findRow(t, r, CheckCache, "libvirt.img"); got.Status != StatusInfo || !strings.Contains(got.Detail, "on the array") {
		t.Errorf("libvirt row = %+v", got)
	}
	if got := findRow(t, r, CheckCache, "Docker storage"); got.Status != StatusInfo || !strings.Contains(got.Detail, "not on the cache") {
		t.Errorf("docker row = %+v", got)
	}

	r = scanInventory(t, dropCapture)
	requireRow(t, r, CheckCache, StatusWarn, "Docker storage", "without the capture it is not known")
	requireRow(t, r, CheckCache, StatusWarn, "libvirt.img", "without the capture it is not known")
}

func TestCache_TheAppdataShareIsTheOneDockersAppdataPathNames(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/docker.cfg"] = bytes.ReplaceAll(f["config/docker.cfg"], []byte(`DOCKER_APP_CONFIG_PATH="/mnt/user/appdata/"`), []byte(`DOCKER_APP_CONFIG_PATH="/mnt/user/system/appdata/"`))
	})
	if got := findRow(t, r, CheckCache, "system"); got.Status != StatusWarn || !strings.Contains(got.Detail, "/mnt/user/system/appdata/") {
		t.Errorf("system row = %+v, want it reported as the share holding Docker's appdata", got)
	}
	for _, row := range rowsFor(r, CheckCache) {
		if row.Subject == "appdata" && strings.Contains(row.Detail, "Docker keeps container data") {
			t.Errorf("the default appdata share is reported as Docker's although docker.cfg names another: %+v", row)
		}
	}
}

func TestCache_ADockerImageFileIsNamedAsOne(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/docker.cfg"] = bytes.ReplaceAll(f["config/docker.cfg"], []byte(`DOCKER_IMAGE_TYPE="folder"`), []byte(`DOCKER_IMAGE_TYPE="btrfs vdisk"`))
	})
	requireRow(t, r, CheckCache, StatusInfo, "Docker storage", "Docker's image file")
}

func TestCache_AConfigThatDoesNotParseIsAWarning(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) { f["config/docker.cfg"] = []byte("not a setting\n") })
	requireRow(t, r, CheckCache, StatusWarn, "docker.cfg", "does not parse")
}

func TestUsers_AreReadByNameOnlyAndNoPasswordMaterialIsTouched(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	for _, secret := range []string{"config/shadow", "config/smbpasswd"} {
		if _, ok := files[secret]; !ok {
			t.Fatalf("the fixture flash has no %s, so this test proves nothing", secret)
		}
	}
	files["config/passwd"] = []byte("root:x:0:0:Console:/root:/bin/bash\nnobody:x:99:100:Nobody:/:/bin/false\nalice:HASH-IN-PASSWD:1000:100:Alice Fixture:/dev/null:/bin/false\nbob:x:1001:100::/dev/null:/bin/false\n")
	src := &recordingSource{FlashSource: openZipBytes(t, zipOf(t, files, false))}
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.Import.Users, ",") != "alice,bob" {
		t.Errorf("users = %v, want alice and bob", r.Import.Users)
	}
	requireRow(t, r, CheckUsers, StatusInfo, "2 user accounts: alice, bob", "passwords cannot be carried over")
	for _, name := range src.read {
		if name == "config/shadow" || name == "config/smbpasswd" {
			t.Errorf("the scan read %s", name)
		}
	}
	blob, _ := json.Marshal(r)
	for _, secret := range []string{"HASH-IN-PASSWD", "Alice Fixture", "/dev/null"} {
		if strings.Contains(string(blob)+r.Markdown(), secret) {
			t.Errorf("%q from config/passwd is in the report or the import", secret)
		}
	}
}

func TestUsers_AMissingOrDamagedPasswdIsAWarning(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) { delete(f, "config/passwd") })
	requireRow(t, r, CheckUsers, StatusWarn, "config/passwd was not found")
	r = scanInventory(t, func(f map[string][]byte) { f["config/passwd"] = []byte("root:x:0:0\nbroken\nalice:x:1000:100:::\n") })
	requireRow(t, r, CheckUsers, StatusWarn, "1 line in config/passwd could not be read")
	if strings.Join(r.Import.Users, ",") != "alice" {
		t.Errorf("users = %v", r.Import.Users)
	}
}

func TestPlugins_AreListedAndTheOnesWithACounterpartSaySo(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		for _, p := range []string{"user.scripts", "compose.manager", "ca.backup2", "unassigned.devices", "dynamix.system.stats"} {
			f["config/plugins/"+p+".plg"] = []byte("<PLUGIN/>")
		}
	})
	requireRow(t, r, CheckPlugins, StatusInfo, "user.scripts", "Q83")
	requireRow(t, r, CheckPlugins, StatusInfo, "compose.manager", "compose.yaml")
	requireRow(t, r, CheckPlugins, StatusInfo, "ca.backup2", "doc 10")
	requireRow(t, r, CheckPlugins, StatusInfo, "2 other plugins with no known Hoserva counterpart: dynamix.system.stats, unassigned.devices")
}

func TestCustomConfig_SambaAndGoLinesAreReportedAsNotImportedWithoutQuotingThem(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/smb-extra.conf"] = []byte("# comment\n; comment\n\n[custom]\n   path = /mnt/user/SECRET-SHARE\n")
		f["config/go"] = []byte("#!/bin/bash\n# Start the Management Utility\n/usr/local/sbin/emhttp &\nmodprobe SECRET-MODULE\n")
	})
	requireRow(t, r, CheckCustomConfig, StatusWarn, "smb-extra.conf", "2 lines of custom Samba configuration", "not imported")
	requireRow(t, r, CheckCustomConfig, StatusWarn, "go", "1 custom line in config/go")
	if md := r.Markdown(); strings.Contains(md, "SECRET") {
		t.Error("a line of custom configuration is in the report")
	}

	r = scanInventory(t, func(f map[string][]byte) {
		f["config/smb-extra.conf"] = []byte("# only a comment\n")
		f["config/go"] = []byte("#!/bin/bash\n/usr/local/sbin/emhttp &\n")
	})
	if rows := rowsFor(r, CheckCustomConfig); len(rows) != 0 {
		t.Errorf("rows for a comment-only smb-extra.conf and a default go = %+v", rows)
	}
}

func TestSettings_AreKeptForTheChecklistWithoutAnySecret(t *testing.T) {
	files, spec := flashTree(t, inventoryVariant)
	files["config/plugins/dynamix/dynamix.cfg"] = []byte("[display]\ntheme=\"white\"\n[parity]\nmode=\"3\"\nhour=\"30 2\"\ndotm=\"1\"\nday=\"*\"\nwrite=\"NOCORRECT\"\n")
	files["config/disk.cfg"] = append(files["config/disk.cfg"], []byte("spindownDelay=\"30\"\n")...)
	files["config/plugins/dynamix/notifications/agents/Slack.sh"] = []byte("WEBHOOK=https://hooks.example.invalid/SECRET-WEBHOOK\n")
	files["config/plugins/dynamix/notifications/agents/Pushover.xml"] = []byte("<Agent/>")
	src := &recordingSource{FlashSource: openZipBytes(t, zipOf(t, files, false))}
	r, err := scanner(fixtureDisks(spec)).Scan(context.Background(), src, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := r.Import.Schedules
	if s.MoverCron != "40 3 * * *" || s.SpindownDelay != "30" || strings.Join(s.NotifyAgents, ",") != "Pushover,Slack" {
		t.Errorf("schedules = %+v", s)
	}
	if p := s.ParityCheck; !p.Found || p.Mode != "3" || p.Hour != "30 2" || p.DayOfMonth != "1" || p.Correcting {
		t.Errorf("parity check = %+v, want a non-correcting monthly schedule", p)
	}
	requireRow(t, r, CheckSettings, StatusInfo, "mover schedule", "40 3 * * *")
	requireRow(t, r, CheckSettings, StatusInfo, "parity-check schedule", "non-correcting")
	requireRow(t, r, CheckSettings, StatusInfo, "spin-down delay", "30")
	requireRow(t, r, CheckSettings, StatusInfo, "notification agents", "Pushover, Slack", "no secret is carried over")
	for _, name := range src.read {
		if strings.Contains(name, "notifications/agents") {
			t.Errorf("the scan read %s", name)
		}
	}
	blob, _ := json.Marshal(r)
	if strings.Contains(string(blob), "SECRET-WEBHOOK") {
		t.Error("an agent's content is in the import")
	}

	r = scanInventory(t, nil)
	if p := r.Import.Schedules; p.ParityCheck.Found || p.SpindownDelay != "" || len(p.NotifyAgents) != 0 {
		t.Errorf("settings the flash lacks are not empty: %+v", p)
	}
	requireRow(t, r, CheckSettings, StatusInfo, "parity-check schedule", "Not available")
}

func TestImport_IsPersistedWithTheReportAndStaysOutOfTheMarkdown(t *testing.T) {
	s, _, files := newService(t, inventoryVariant)
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx0)
	if err != nil || st.Report == nil {
		t.Fatalf("State = %+v, %v", st, err)
	}
	imp := st.Report.Import
	if len(imp.Shares) != 8 || len(imp.Users) != 2 || len(imp.Templates) != 7 || len(imp.Networks) != 5 || len(imp.ComposeProjects) != 1 || imp.Schedules.MoverCron == "" {
		t.Errorf("the session lost the import: %d shares, %d users, %d templates, %d networks, %d projects, %+v", len(imp.Shares), len(imp.Users), len(imp.Templates), len(imp.Networks), len(imp.ComposeProjects), imp.Schedules)
	}
	md, err := s.ReportMarkdown(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Share configuration", "## Docker templates", "## User Scripts (plugin)", "## Last Unraid parity check", "## What would be lost with the cache"} {
		if !strings.Contains(md, want) {
			t.Errorf("the downloadable report lacks %q", want)
		}
	}
}

func TestZipSource_ModTimeIsTheEntrysAndUnknownWhenThereIsNone(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 8, 0, time.UTC)
	src := openZipBytes(t, zipWithTimes(t, map[string][]byte{"a": []byte("x")}, map[string]time.Time{"a": at}))
	if got, ok := src.ModTime("a"); !ok || !got.Equal(at) {
		t.Errorf("ModTime(a) = %v, %v; want %v", got, ok, at)
	}
	if _, ok := src.ModTime("missing"); ok {
		t.Error("ModTime of a file that is not there is known")
	}
}
