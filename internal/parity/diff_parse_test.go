package parity

import "testing"

func TestParseDiff_AllAdded(t *testing.T) {
	d, err := ParseDiff(readCorpus(t, "snapraid_diff_all_added.log"))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if d.Added != 3 || d.Equal != 0 || d.Removed != 0 || d.Updated != 0 || d.Moved != 0 || d.Copied != 0 {
		t.Fatalf("ParseDiff: got %+v, want Added=3 and everything else 0", d)
	}
}

// TestParseDiff_Mixed exercises a real add/remove/update/copy diff — one
// of each category snapraid.txt §5.5 documents that this lab's SnapRAID
// build can actually produce (doc 08 §5: `moved` cannot be, since no
// data-disk UUID is ever available here).
func TestParseDiff_Mixed(t *testing.T) {
	d, err := ParseDiff(readCorpus(t, "snapraid_diff_mixed.log"))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	if d.Added != 2 || d.Removed != 2 || d.Updated != 1 || d.Moved != 0 || d.Copied != 1 || d.Equal != 0 {
		t.Fatalf("ParseDiff: got %+v, want Added=2 Removed=2 Updated=1 Copied=1", d)
	}
	wantMounts := map[string]string{
		"d1": "/lab/28-a1/mnt/disk1/",
		"d2": "/lab/28-a1/mnt/disk2/",
		"d3": "/lab/28-a1/mnt/disk3/",
	}
	for id, path := range wantMounts {
		if d.DataMounts[id] != path {
			t.Errorf("DataMounts[%s] = %q, want %q", id, d.DataMounts[id], path)
		}
	}
}

// TestBuildDiffReport_Mixed combines the mixed diff fixture with a
// synthetic before-snapshot (the real lab state immediately before that
// diff ran: one file per disk) and checks the per-disk projection
// against what the real filesystem state actually was afterwards:
// disk1 gained d-new.bin and kept archive/a.bin (a.bin itself removed,
// its copy counted, not the removed original) — net +1; disk2 gained
// b-copy.bin — net +1; disk3 lost its only file — net -1, to zero.
func TestBuildDiffReport_Mixed(t *testing.T) {
	d, err := ParseDiff(readCorpus(t, "snapraid_diff_mixed.log"))
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	before := StatusReport{
		DataMounts:       d.DataMounts,
		PerDiskFileCount: map[string]int{"d1": 1, "d2": 1, "d3": 1},
	}

	report := BuildDiffReport(before, d)

	if report.Added != 2 || report.Removed != 2 || report.Updated != 1 || report.Copied != 1 || report.Moved != 0 {
		t.Fatalf("BuildDiffReport aggregate: got %+v", report)
	}

	want := map[string]DiskDiff{
		"/lab/28-a1/mnt/disk1": {FilesBefore: 1, FilesAfter: 2},
		"/lab/28-a1/mnt/disk2": {FilesBefore: 1, FilesAfter: 2},
		"/lab/28-a1/mnt/disk3": {FilesBefore: 1, FilesAfter: 0},
	}
	for mount, wantDiff := range want {
		got, ok := report.PerDisk[mount]
		if !ok {
			t.Errorf("PerDisk missing mount %s (have %v)", mount, report.PerDisk)
			continue
		}
		if got != wantDiff {
			t.Errorf("PerDisk[%s] = %+v, want %+v", mount, got, wantDiff)
		}
	}
	if report.MovedByHoserva != 0 {
		t.Errorf("MovedByHoserva = %d, want 0 — accounting relocations is the guard's own job, not Engine's", report.MovedByHoserva)
	}
}

func TestParseDiff_EmptyInput(t *testing.T) {
	if _, err := ParseDiff(nil); err == nil {
		t.Fatal("ParseDiff(nil): got nil error")
	}
}
