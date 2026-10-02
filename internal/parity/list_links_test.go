package parity

import (
	"context"
	"testing"
)

const listLogWithLinks = "data:d1:/m/d1/\n" +
	"data:d2:/m/d2/\n" +
	"file:d1:s/a.bin:5:1790006330:712259445:132\n" +
	"link_symlink:d2:s/rel-link:a.bin\n" +
	"link_hardlink:d2:s/a-hardlink.bin:s/a.bin\n" +
	"link_symlink:d2:s/odd:name:a.bin\n" +
	"summary:file_count:1\n" +
	"summary:link_count:3\n" +
	"summary:exit:ok\n"

func TestParseList_LinksAreRecordedWithTheirDiskAndPath(t *testing.T) {
	r, err := ParseList([]byte(listLogWithLinks))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	want := []ListLink{{Disk: "d2", RelPath: "s/rel-link"}, {Disk: "d2", RelPath: "s/a-hardlink.bin"}}
	if len(r.Links) != len(want) {
		t.Fatalf("Links = %+v, want %+v (the link whose path or target holds a ':' is ambiguous and not recorded)", r.Links, want)
	}
	for i, l := range want {
		if r.Links[i] != l {
			t.Errorf("Links[%d] = %+v, want %+v", i, r.Links[i], l)
		}
	}
	if len(r.Files) != 1 {
		t.Errorf("Files = %+v, want only the regular file", r.Files)
	}
}

func TestParseList_SharesCorpusLinks(t *testing.T) {
	r, err := ParseList(readCorpus(t, "snapraid_list_shares.log"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	want := []ListLink{{Disk: "disk1", RelPath: "movies/link.mkv"}, {Disk: "disk1", RelPath: "photos/p1-hardlink.jpg"}}
	if len(r.Links) != len(want) || r.Links[0] != want[0] || r.Links[1] != want[1] {
		t.Fatalf("Links = %+v, want %+v", r.Links, want)
	}
}

// A symlink moved between two data disks is a removal on the source and an
// addition on the target; the trailing sync's removal is accounted only when
// the target's tracked state, which `snapraid list` reports as a link line,
// is recognised.
func TestConfirmManifestTargets_ConfirmsASymlinkTrackedOnTheTarget(t *testing.T) {
	list, err := ParseList([]byte(listLogWithLinks))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	lister := NewFakeEngine()
	lister.SetList(list)

	diff := DiffReport{
		Removed: 3,
		RemovedFiles: []DiffFile{
			{Disk: "/m/d1", RelPath: "s/rel-link"},
			{Disk: "/m/d1", RelPath: "s/a-hardlink.bin"},
			{Disk: "/m/d1", RelPath: "s/gone-link"},
		},
		PerDisk: map[string]DiskDiff{
			"/m/d1": {FilesBefore: 10, FilesAfter: 7},
			"/m/d2": {FilesBefore: 10, FilesAfter: 10},
		},
	}
	manifest := []ManifestEntry{
		{RelPath: "s/rel-link", SourceDisk: "/m/d1", TargetDisk: "/m/d2"},
		{RelPath: "s/a-hardlink.bin", SourceDisk: "/m/d1", TargetDisk: "/m/d2"},
		{RelPath: "s/gone-link", SourceDisk: "/m/d1", TargetDisk: "/m/d2"},
	}

	confirmed, err := ConfirmManifestTargets(context.Background(), lister, diff, manifest)
	if err != nil {
		t.Fatalf("ConfirmManifestTargets: %v", err)
	}
	if !confirmed[0].TargetConfirmed || !confirmed[1].TargetConfirmed {
		t.Errorf("tracked links not confirmed: %+v", confirmed)
	}
	if confirmed[2].TargetConfirmed {
		t.Errorf("a link `snapraid list` does not show on the target must stay unconfirmed: %+v", confirmed[2])
	}

	guard := Guard{Config: GuardConfig{RemovedFilesMax: 1, RemovedUpdatedPercent: 100}}
	res := guard.Evaluate(diff, confirmed, nil)
	if res.RemovedCount != 1 {
		t.Fatalf("RemovedCount = %d, want 1 (only the unconfirmed link)", res.RemovedCount)
	}
}
