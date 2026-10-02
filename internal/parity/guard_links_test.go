package parity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// linksStatusLog is a status log in the shape a real snapraid 12.4-1
// writes (confirmed in the loop-device lab): d1 tracks 5 regular files and
// the loading header reports 1 hardlink and 2 symlinks, which
// `disk_file_count` does not include.
const linksStatusLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
msg:verbose:        6 files
msg:verbose:        1 hardlinks
msg:verbose:        2 symlinks
msg:verbose:        0 empty dirs
summary:disk_file_count:d1:5
summary:disk_file_count:d2:1
`

const linksListLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
file:d1:s/empty0:0:1790951495:631796139:138
file:d1:s/fa.bin:100000:1790951495:623363268:132
file:d1:s/fb.bin:100000:1790951495:631179676:133
file:d1:s/fc.bin:100000:1790951495:631433082:134
file:d1:s/fd.bin:100000:1790951495:631796139:135
link_symlink:d1:s/dangling:/nonexistent/abs
link_hardlink:d1:s/hard-fb:s/fb.bin
link_symlink:d1:s/rel-link:fa.bin
file:d2:s/other.bin:100000:1790951495:631796139:132
summary:file_count:6
summary:file_size:500000
summary:link_count:3
summary:exit:ok
`

// linksEmptiedDiffLog is d1 with every entry gone, as an unmounted disk
// shows: SnapRAID reports the 5 files and the 3 links as 8 removals.
const linksEmptiedDiffLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
scan:remove:d1:s/empty0
scan:remove:d1:s/fa.bin
scan:remove:d1:s/fb.bin
scan:remove:d1:s/fc.bin
scan:remove:d1:s/fd.bin
scan:remove:d1:s/hard-fb
scan:remove:d1:s/rel-link
scan:remove:d1:s/dangling
summary:equal:1
summary:added:0
summary:removed:8
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

// linksPartialDiffLog removes the 3 links and one regular file from d1:
// 4 regular files stay.
const linksPartialDiffLog = `data:d1:/lab/guard/mnt/disk1/
data:d2:/lab/guard/mnt/disk2/
scan:remove:d1:s/empty0
scan:remove:d1:s/hard-fb
scan:remove:d1:s/rel-link
scan:remove:d1:s/dangling
summary:equal:5
summary:added:0
summary:removed:4
summary:updated:0
summary:moved:0
summary:copied:0
summary:restored:0
summary:exit:diff
`

func TestParseStatus_ReportsTheLinksTheLoadingHeaderCounts(t *testing.T) {
	r, err := ParseStatus([]byte(linksStatusLog))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	if r.TrackedLinks != 3 {
		t.Fatalf("TrackedLinks = %d, want 3 (1 hardlink + 2 symlinks)", r.TrackedLinks)
	}

	clean, err := ParseStatus(readCorpus(t, "snapraid_status_clean.log"))
	if err != nil {
		t.Fatalf("ParseStatus(clean): %v", err)
	}
	if clean.TrackedLinks != 0 {
		t.Fatalf("TrackedLinks on a real log with 0 hardlinks and 0 symlinks = %d, want 0", clean.TrackedLinks)
	}
}

func TestParseList_CountsLinksPerDisk(t *testing.T) {
	r, err := ParseList([]byte(linksListLog))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if r.LinkCount["d1"] != 3 || r.LinkCount["d2"] != 0 || len(r.LinkCount) != 1 {
		t.Fatalf("LinkCount = %v, want only d1:3", r.LinkCount)
	}

	shares, err := ParseList(readCorpus(t, "snapraid_list_shares.log"))
	if err != nil {
		t.Fatalf("ParseList(shares): %v", err)
	}
	if shares.LinkCount["disk1"] != 2 {
		t.Fatalf("LinkCount on the real shares capture = %v, want disk1:2", shares.LinkCount)
	}
}

func TestParseList_RefusesLinkLinesThatDisagreeWithTheSummary(t *testing.T) {
	log := strings.Replace(linksListLog, "link_symlink:d1:s/rel-link:fa.bin\n", "", 1)
	if _, err := ParseList([]byte(log)); !errors.Is(err, ErrListParse) {
		t.Fatalf("ParseList with 2 link lines against summary:link_count:3: err = %v, want ErrListParse", err)
	}
}

func buildLinksDiff(t *testing.T, diffLog string) DiffReport {
	t.Helper()
	before, err := ParseStatus([]byte(linksStatusLog))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	list, err := ParseList([]byte(linksListLog))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	before.PerDiskLinkCount = list.LinkCount
	d, err := ParseDiff([]byte(diffLog))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	return BuildDiffReport(before, d)
}

func TestBuildDiffReport_CountsLinksOnBothSidesOfTheProjection(t *testing.T) {
	report := buildLinksDiff(t, linksEmptiedDiffLog)
	got := report.PerDisk["/lab/guard/mnt/disk1"]
	if got.FilesBefore != 8 || got.FilesAfter != 0 {
		t.Fatalf("PerDisk[d1] = %+v, want {FilesBefore:8 FilesAfter:0}: 5 files and 3 links, all removed", got)
	}
	if d2 := report.PerDisk["/lab/guard/mnt/disk2"]; d2.FilesBefore != 1 || d2.FilesAfter != 1 {
		t.Fatalf("PerDisk[d2] = %+v, want {1 1}", d2)
	}

	partial := buildLinksDiff(t, linksPartialDiffLog).PerDisk["/lab/guard/mnt/disk1"]
	if partial.FilesAfter != 4 {
		t.Fatalf("PerDisk[d1] = %+v after removing 3 links and 1 file, want FilesAfter 4 (the 4 regular files left)", partial)
	}
}

func TestGuard_BlocksADiskEmptiedOfFilesAndLinks(t *testing.T) {
	result := Guard{}.Evaluate(buildLinksDiff(t, linksEmptiedDiffLog), nil, nil)
	if !result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Triggers = %v, want zero-files", result.Triggers)
	}
	if len(result.ZeroFilesDisks) != 1 || result.ZeroFilesDisks[0].Disk != "/lab/guard/mnt/disk1" {
		t.Fatalf("ZeroFilesDisks = %+v, want only /lab/guard/mnt/disk1", result.ZeroFilesDisks)
	}
}

func TestGuard_DoesNotBlockADiskThatKeepsRegularFiles(t *testing.T) {
	result := Guard{}.Evaluate(buildLinksDiff(t, linksPartialDiffLog), nil, nil)
	if result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Triggers = %v, want no zero-files for a disk that keeps 4 regular files", result.Triggers)
	}
}

func TestGuard_ZeroFilesAlsoCatchesANegativeProjection(t *testing.T) {
	diff := DiffReport{PerDisk: map[string]DiskDiff{
		"/m/d1": {FilesBefore: 5, FilesAfter: -3},
		"/m/d2": {FilesBefore: 5, FilesAfter: 5},
	}}
	result := Guard{}.Evaluate(diff, nil, nil)
	if !result.hasTrigger(TriggerZeroFiles) || len(result.ZeroFilesDisks) != 1 || result.ZeroFilesDisks[0].Disk != "/m/d1" {
		t.Fatalf("Result = %+v, want zero-files for /m/d1 alone", result)
	}
	if !anyDiskEmptied(diff) {
		t.Fatal("anyDiskEmptied = false for a disk projected below zero files")
	}
}

// isolateZeroFiles lifts the count and percent triggers out of the way so a
// decision in the engine tests below is the zero-files rule's alone. The
// percent rule divides by regular files, which removed links can outnumber.
var isolateZeroFiles = Guard{Config: GuardConfig{RemovedFilesMax: 100000, RemovedUpdatedPercent: 100000}}

func syncOps(calls [][]string) []string {
	ops := make([]string, len(calls))
	for i, c := range calls {
		ops[i] = c[len(c)-1]
	}
	return ops
}

func TestSnapraidEngine_Sync_BlocksADiskEmptiedOfFilesAndSymlinks(t *testing.T) {
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: linksStatusLog},
		{logBody: linksListLog},
		{logBody: linksEmptiedDiffLog, err: &fakeExitError{code: 2}},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r, Guard: isolateZeroFiles}

	ch, err := e.Sync(context.Background(), SyncOpts{})
	var blocked *GuardBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Sync: err = %v, want a *GuardBlockedError", err)
	}
	if ch != nil {
		t.Fatal("Sync returned a progress channel for a blocked sync")
	}
	if len(blocked.Result.Triggers) != 1 || !blocked.Result.hasTrigger(TriggerZeroFiles) {
		t.Fatalf("Triggers = %v, want zero-files alone", blocked.Result.Triggers)
	}
	if got := strings.Join(syncOps(r.calls), ","); got != "status,list,diff" {
		t.Fatalf("snapraid calls = %s, want status,list,diff and no sync", got)
	}
}

func TestSnapraidEngine_Sync_ForceEmptyOnlyForTheEvacuatingDisk(t *testing.T) {
	script := func() *scriptedRunner {
		return &scriptedRunner{t: t, script: []scriptedResult{
			{logBody: linksStatusLog},
			{logBody: linksListLog},
			{logBody: linksEmptiedDiffLog, err: &fakeExitError{code: 2}},
			{logBody: string(readCorpus(t, "snapraid_sync_ok.log"))},
		}}
	}

	r := script()
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r, Guard: isolateZeroFiles}
	ch, err := e.Sync(context.Background(), SyncOpts{RemovingDisks: map[string]bool{"/lab/guard/mnt/disk1": true}})
	if err != nil {
		t.Fatalf("Sync of an evacuated disk: %v", err)
	}
	if final := drain(t, ch); final.Err != nil {
		t.Fatalf("Sync final Err = %v", final.Err)
	}
	last := r.calls[len(r.calls)-1]
	if last[len(last)-1] != "sync" || !containsArg(last, "-E") {
		t.Fatalf("sync call = %v, want -E for the evacuated disk emptied of files and symlinks", last)
	}

	r = script()
	e = &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r, Guard: isolateZeroFiles}
	if _, err := e.Sync(context.Background(), SyncOpts{RemovingDisks: map[string]bool{"/lab/guard/mnt/disk2": true}}); !errors.Is(err, ErrGuardBlocked) {
		t.Fatalf("Sync with another disk exempted: err = %v, want ErrGuardBlocked", err)
	}
	for _, c := range r.calls {
		if containsArg(c, "-E") {
			t.Fatalf("a blocked sync passed --force-empty: %v", r.calls)
		}
	}
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func TestSnapraidEngine_Diff_SkipsListWhenNoLinksAreTracked(t *testing.T) {
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: string(readCorpus(t, "snapraid_status_clean.log"))},
		{logBody: noChangeDiffLog},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r}
	if _, err := e.Diff(context.Background()); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := strings.Join(syncOps(r.calls), ","); got != "status,diff" {
		t.Fatalf("snapraid calls = %s, want status,diff when the status header reports no links", got)
	}
}

func TestSnapraidEngine_Sync_FailsClosedWhenTheLinkCountsCannotBeRead(t *testing.T) {
	r := &scriptedRunner{t: t, script: []scriptedResult{
		{logBody: linksStatusLog},
		{logBody: "", err: errors.New("exit status 1")},
	}}
	e := &SnapraidEngine{ConfPath: "snapraid.conf", LogDir: t.TempDir(), Runner: r}

	ch, err := e.Sync(context.Background(), SyncOpts{Confirm: true})
	if err == nil || ch != nil {
		t.Fatalf("Sync = (%v, %v), want an error and no channel when `snapraid list` fails", ch, err)
	}
	if got := strings.Join(syncOps(r.calls), ","); got != "status,list" {
		t.Fatalf("snapraid calls = %s, want status,list and no diff or sync", got)
	}
}

// An array of 100 regular files and 900 symlinks that loses 11 regular
// files is at 11% of its regular files, over the default 10%. The percent
// rule divides by the regular-file total, as it always has, so counting
// links per disk for the zero-files projection must not dilute it to 1.1%.
func TestGuard_RemovedPercentStillDividesByRegularFiles(t *testing.T) {
	var diffLog strings.Builder
	diffLog.WriteString("data:d1:/lab/guard/mnt/disk1/\n")
	for i := 0; i < 11; i++ {
		fmt.Fprintf(&diffLog, "scan:remove:d1:s/f%d.bin\n", i)
	}
	diffLog.WriteString("summary:equal:989\nsummary:added:0\nsummary:removed:11\nsummary:updated:0\nsummary:moved:0\nsummary:copied:0\nsummary:restored:0\nsummary:exit:diff\n")

	before, err := ParseStatus([]byte("data:d1:/lab/guard/mnt/disk1/\nsummary:disk_file_count:d1:100\n"))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	before.PerDiskLinkCount = map[string]int{"d1": 900}
	d, err := ParseDiff([]byte(diffLog.String()))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	diff := BuildDiffReport(before, d)
	if got := diff.PerDisk["/lab/guard/mnt/disk1"]; got.FilesBefore != 1000 || got.FilesAfter != 989 || got.LinksBefore != 900 {
		t.Fatalf("PerDisk = %+v, want {FilesBefore:1000 FilesAfter:989 LinksBefore:900}", got)
	}
	if got := diff.RegularFilesBefore(); got != 100 {
		t.Fatalf("RegularFilesBefore = %d, want 100", got)
	}

	result := Guard{}.Evaluate(diff, nil, nil)
	if !result.hasTrigger(TriggerRemovedUpdatedPercent) {
		t.Fatalf("Triggers = %v, want removed-updated-percent: 11 of 100 regular files is over the default 10%%", result.Triggers)
	}
	if result.RemovedUpdatedPercent != 11 {
		t.Fatalf("RemovedUpdatedPercent = %v, want 11", result.RemovedUpdatedPercent)
	}
}
