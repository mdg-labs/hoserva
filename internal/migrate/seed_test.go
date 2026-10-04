package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/share"
)

func planShare(t *testing.T, plan SeedPlan, name string) share.SeedShare {
	t.Helper()
	for _, sh := range plan.Shares {
		if sh.Name == name {
			return sh
		}
	}
	t.Fatalf("share %s is not in the plan %v", name, plan.Shares)
	return share.SeedShare{}
}

func hasNote(sh share.SeedShare, parts ...string) bool {
notes:
	for _, n := range sh.Notes {
		for _, p := range parts {
			if !strings.Contains(n, p) {
				continue notes
			}
		}
		return true
	}
	return false
}

func TestSeedPlan_EveryShareTheScanKeptIsPlannedWithItsSettings(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/shares/media.cfg"] = []byte("shareAllocator=\"highwater\"\nshareSplitLevel=\"2\"\nshareFloor=\"50000000\"\nshareUseCache=\"no\"\nshareExport=\"e\"\nshareSecurity=\"public\"\n")
		f["config/shares/documents.cfg"] = []byte("shareAllocator=\"mostfree\"\nshareUseCache=\"yes\"\nshareExport=\"e\"\nshareSecurity=\"private\"\nshareReadList=\"bob,ghost\"\nshareWriteList=\"alice\"\nshareSplitLevel=\"\"\nshareFloor=\"0\"\n")
		f["config/shares/backup.cfg"] = []byte("shareAllocator=\"fillup\"\nshareUseCache=\"no\"\nshareExport=\"-\"\nshareSecurity=\"secure\"\nshareReadList=\"alice\"\nshareWriteList=\"\"\n")
		f["config/shares/appdata.cfg"] = []byte("shareAllocator=\"mostfree\"\nshareUseCache=\"prefer\"\nshareExport=\"e\"\nshareSecurity=\"public\"\n")
		f["config/passwd"] = []byte("root:x:0:0:Console:/root:/bin/bash\nalice:HASH-IN-PASSWD:1000:100:Alice:/dev/null:/bin/false\nbob:x:1001:100::/dev/null:/bin/false\n")
	})
	plan := r.Import.SeedPlan()
	if len(plan.Shares) != len(r.Import.Shares) || len(plan.SkippedShares) != 0 {
		t.Fatalf("plan has %d shares and %d skipped, the scan kept %d", len(plan.Shares), len(plan.SkippedShares), len(r.Import.Shares))
	}
	if fmt.Sprint(plan.Users) != "[alice bob]" {
		t.Errorf("users = %v", plan.Users)
	}

	media := planShare(t, plan, "media")
	if media.CreatePolicy != pool.BalanceAcrossDisks || media.TargetCacheMode != "" || !media.SMB.Enabled || !media.SMB.Guest || media.MinFreeSpace != "50000000K" {
		t.Errorf("media = %+v", media)
	}
	if !hasNote(media, "mapped to Balance across disks") || !hasNote(media, "split level 2", "no split-level setting", "create policy") {
		t.Errorf("media notes = %q: want the High-water mapping and the split-level notes", media.Notes)
	}

	docs := planShare(t, plan, "documents")
	if docs.CreatePolicy != pool.BalanceAcrossDisks || docs.TargetCacheMode != pool.CacheThenMove || docs.SMB.Guest || docs.MinFreeSpace != "" {
		t.Errorf("documents = %+v", docs)
	}
	if fmt.Sprint(docs.Access) != "[{alice read-write} {bob read-only}]" {
		t.Errorf("documents access = %v, want the write list read-write and the read list read-only for imported users only", docs.Access)
	}
	if !hasNote(docs, "not among the imported", "ghost") {
		t.Errorf("documents notes = %q: want the unknown list entry named", docs.Notes)
	}
	if hasNote(docs, "split level") || hasNote(docs, "mapped to Balance across disks") {
		t.Errorf("documents notes = %q: an empty split level and Most-free need no note", docs.Notes)
	}
	if !hasNote(docs, "cache mode cache-then-move", "once the cache disk exists") {
		t.Errorf("documents notes = %q: want the deferred cache mode said", docs.Notes)
	}

	backup := planShare(t, plan, "backup")
	if backup.CreatePolicy != pool.FillDisksInOrder || backup.SMB.Enabled || backup.SMB.Guest || fmt.Sprint(backup.Access) != "[{alice read-only}]" || !hasNote(backup, "secure") {
		t.Errorf("backup = %+v", backup)
	}

	appdata := planShare(t, plan, "appdata")
	if appdata.TargetCacheMode != pool.CacheOnly || !hasNote(appdata, "prefer", "fall through") {
		t.Errorf("appdata = %+v", appdata)
	}
	for _, sh := range plan.Shares {
		for _, n := range sh.Notes {
			if strings.Contains(n, "HASH-IN-PASSWD") {
				t.Errorf("password material is in a note: %q", n)
			}
		}
	}
}

// Comment on #299: a name pool.ValidateShareName refuses is reported with its
// reason, never renamed, and the other shares are still planned.
func TestSeedPlan_AShareNameHoservaRefusesIsSkippedNeverRenamed(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) {
		f["config/shares/Family Photos.cfg"] = []byte("shareAllocator=\"highwater\"\nshareUseCache=\"no\"\nshareExport=\"e\"\n")
	})
	if _, ok := shareByName(r, "Family Photos"); !ok {
		t.Fatal("the scan did not keep the share named with a space")
	}
	row := findRow(t, r, CheckShares, "Family Photos")
	if row.Status != StatusFlag || !strings.Contains(row.Detail, "not carried over") || !strings.Contains(row.Detail, "not renamed") {
		t.Errorf("scan row = %+v, want the share flagged as not carried over and never renamed", row)
	}
	var previewWarnings int
	for _, p := range r.Review.Shares {
		if p.Name == "Family Photos" {
			previewWarnings = p.WarningCount
		}
	}
	if previewWarnings == 0 {
		t.Errorf("the review's share preview has no warning for it")
	}

	plan := r.Import.SeedPlan()
	if len(plan.SkippedShares) != 1 || plan.SkippedShares[0].Name != "Family Photos" || !strings.Contains(plan.SkippedShares[0].Reason, "not renamed") {
		t.Fatalf("skipped = %+v", plan.SkippedShares)
	}
	for _, sh := range plan.Shares {
		if sh.Name == "Family Photos" || strings.Contains(sh.Name, " ") || strings.EqualFold(sh.Name, "FamilyPhotos") || strings.EqualFold(sh.Name, "Family_Photos") {
			t.Errorf("the plan seeds %q", sh.Name)
		}
	}
	if len(plan.Shares) != len(r.Import.Shares)-1 {
		t.Errorf("plan has %d shares, want every other share (%d)", len(plan.Shares), len(r.Import.Shares)-1)
	}
}

func TestSeedPlan_FloorsAndOddSettings(t *testing.T) {
	imp := Import{
		Shares: []Share{
			{Name: "a", Allocator: "fillup", UseCache: "no", Export: "e", Security: "public", Floor: "not-a-number"},
			{Name: "b", Allocator: "fillup", UseCache: "no", Export: "e", Security: "public", Floor: "99999999999999999999"},
			{Name: "c", Allocator: "roundrobin", UseCache: "sometimes", Export: "e", Security: "weird", CachePool: "fast", Include: []string{"disk1"}, Exclude: []string{"disk2"}},
			{Name: "d", Allocator: "fillup", UseCache: "only", Export: "e", Security: "public", CachePool: "fast"},
			{Name: "e", Allocator: "fillup", UseCache: "no", Export: "e", Security: "public", Floor: "0"},
		},
		Users: []string{"Alice", "alice", "bad name", "bob"},
	}
	plan := imp.SeedPlan()
	if a := planShare(t, plan, "a"); a.MinFreeSpace != "" || !hasNote(a, "floor") {
		t.Errorf("a = %+v, want an unreadable floor ignored and said", a)
	}
	if b := planShare(t, plan, "b"); b.MinFreeSpace != "" || !hasNote(b, "floor") {
		t.Errorf("b = %+v, want a floor too large for mergerfs ignored and said", b)
	}
	c := planShare(t, plan, "c")
	if c.CreatePolicy != pool.DefaultCreatePolicy || c.TargetCacheMode != "" || c.SMB.Guest {
		t.Errorf("c = %+v", c)
	}
	for _, want := range []string{"allocation method", "cache setting", "security setting", "disk1", "disk2"} {
		if !hasNote(c, want) {
			t.Errorf("c notes = %q, missing %q", c.Notes, want)
		}
	}
	if d := planShare(t, plan, "d"); !hasNote(d, "pool \"fast\"") {
		t.Errorf("d notes = %q", d.Notes)
	}
	if e := planShare(t, plan, "e"); e.MinFreeSpace != "" || len(e.Notes) != 0 {
		t.Errorf("e = %+v, want a zero floor and Fill-up to need nothing", e)
	}
	if fmt.Sprint(plan.Users) != "[alice bob]" || len(plan.SkippedUsers) != 1 || plan.SkippedUsers[0].Name != "bad name" {
		t.Errorf("users = %v skipped = %+v", plan.Users, plan.SkippedUsers)
	}
}

func TestService_SeedPlanNeedsAReport(t *testing.T) {
	svc := &Service{Dir: t.TempDir(), Sessions: newSessions(t)}
	if _, err := svc.SeedPlan(context.Background()); !errors.Is(err, ErrImportNoReport) {
		t.Fatalf("SeedPlan with no scan = %v, want ErrImportNoReport", err)
	}
}

// Unraid's SMB export select (SecuritySMB.page in unraid/webgui) stores -, e,
// eh, et and eth. Hidden never becomes browseable, Time Machine is said not to
// be carried over, and a value Hoserva cannot read is exported hidden with a note.
func TestSeedPlan_SMBExportValuesMapToEnabledAndBrowseable(t *testing.T) {
	cases := []struct {
		export              string
		enabled, browseable bool
		note                string
	}{
		{"-", false, true, ""},
		{"", false, true, ""},
		{"e", true, true, ""},
		{"eh", true, false, ""},
		{"et", true, true, "Time Machine"},
		{"eth", true, false, "Time Machine"},
		{"ex", true, false, `export setting "ex" is not one Hoserva maps`},
	}
	for _, c := range cases {
		imp := Import{Shares: []Share{{Name: "s", Allocator: "fillup", UseCache: "no", Security: "public", Export: c.export}}}
		sh := planShare(t, imp.SeedPlan(), "s")
		if sh.SMB.Enabled != c.enabled || sh.SMB.Browseable != c.browseable || sh.SMB.TimeMachine {
			t.Errorf("export %q = %+v, want enabled %v browseable %v and no Time Machine", c.export, sh.SMB, c.enabled, c.browseable)
		}
		if c.note == "" && len(sh.Notes) != 0 {
			t.Errorf("export %q has notes %q", c.export, sh.Notes)
		}
		if c.note != "" && !hasNote(sh, c.note) {
			t.Errorf("export %q notes = %q, want one with %q", c.export, sh.Notes, c.note)
		}
	}
}
