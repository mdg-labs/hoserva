package parity

import "testing"

// TestParseList_Shares is captured from a real snapraid 12.4-1 binary in
// the loop-device lab (testdata/parsers/snapraid_list_shares.log, #223):
// two shares ("movies", "photos") split across disk1/disk2, one share
// ("backups") on disk3 alone, a file at a data disk's own root
// (root-note.txt, no share of its own) and one hardlink/one symlink —
// neither of which carries a size of its own.
func TestParseList_Shares(t *testing.T) {
	r, err := ParseList(readCorpus(t, "snapraid_list_shares.log"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	wantMounts := map[string]string{
		"disk1": "/lab/223-a2/mnt/disk1/",
		"disk2": "/lab/223-a2/mnt/disk2/",
		"disk3": "/lab/223-a2/mnt/disk3/",
	}
	for id, path := range wantMounts {
		if r.DataMounts[id] != path {
			t.Errorf("DataMounts[%s] = %q, want %q", id, r.DataMounts[id], path)
		}
	}

	want := []ListFile{
		{Disk: "disk1", RelPath: "movies/a.mkv", Size: 102400},
		{Disk: "disk1", RelPath: "photos/p1.jpg", Size: 10240},
		{Disk: "disk1", RelPath: "root-note.txt", Size: 23},
		{Disk: "disk2", RelPath: "movies/b.mkv", Size: 204800},
		{Disk: "disk2", RelPath: "movies/c.mkv", Size: 30720},
		{Disk: "disk3", RelPath: "backups/dump.tar", Size: 51200},
	}
	if len(r.Files) != len(want) {
		t.Fatalf("len(Files) = %d, want %d: %+v", len(r.Files), len(want), r.Files)
	}
	for i, f := range want {
		if r.Files[i] != f {
			t.Errorf("Files[%d] = %+v, want %+v", i, r.Files[i], f)
		}
	}
}

func TestParseList_EmptyInput(t *testing.T) {
	if _, err := ParseList(nil); err == nil {
		t.Fatal("ParseList(nil): got nil error")
	}
}

func TestParseList_Malformed(t *testing.T) {
	if _, err := ParseList([]byte("not a snapraid log\nat all\n")); err == nil {
		t.Fatal("ParseList(malformed): got nil error, want ErrListParse")
	}
}

func TestAggregateShareUsage(t *testing.T) {
	r, err := ParseList(readCorpus(t, "snapraid_list_shares.log"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	got := AggregateShareUsage(r)
	want := []ShareUsage{
		{Share: "backups", Disk: "/lab/223-a2/mnt/disk3", Bytes: 51200},
		{Share: "movies", Disk: "/lab/223-a2/mnt/disk1", Bytes: 102400},
		{Share: "movies", Disk: "/lab/223-a2/mnt/disk2", Bytes: 235520},
		{Share: "photos", Disk: "/lab/223-a2/mnt/disk1", Bytes: 10240},
	}
	if len(got) != len(want) {
		t.Fatalf("AggregateShareUsage = %+v, want %+v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("AggregateShareUsage[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestAggregateShareUsage_RootFileNotAShare confirms root-note.txt (no
// top-level directory of its own) never produces a phantom share — the
// fixture above already carries it; this pins the specific behaviour.
func TestAggregateShareUsage_RootFileNotAShare(t *testing.T) {
	r, err := ParseList(readCorpus(t, "snapraid_list_shares.log"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	for _, u := range AggregateShareUsage(r) {
		if u.Share == "root-note.txt" {
			t.Fatalf("root-note.txt was aggregated as a share: %+v", u)
		}
	}
}

func TestAggregateShareUsage_UnknownDiskSkipped(t *testing.T) {
	list := ListReport{
		DataMounts: map[string]string{"disk1": "/mnt/disk1/"},
		Files: []ListFile{
			{Disk: "disk1", RelPath: "movies/a.mkv", Size: 100},
			{Disk: "disk9", RelPath: "movies/b.mkv", Size: 200},
		},
	}
	got := AggregateShareUsage(list)
	want := []ShareUsage{{Share: "movies", Disk: "/mnt/disk1", Bytes: 100}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("AggregateShareUsage = %+v, want %+v", got, want)
	}
}
