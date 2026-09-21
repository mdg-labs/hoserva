package pool

import (
	"context"
	"errors"
	"testing"
)

// fakeSpaceStatter is a scriptable SpaceStatter: each path is registered
// with its own SpaceStat (or an error), so a test can assert on
// ComputePoolSpace's behaviour without ever touching a real filesystem.
type fakeSpaceStatter struct {
	stats map[string]SpaceStat
	errs  map[string]error
}

func (f fakeSpaceStatter) StatSpace(_ context.Context, path string) (SpaceStat, error) {
	if err, ok := f.errs[path]; ok {
		return SpaceStat{}, err
	}
	return f.stats[path], nil
}

func TestParseMinFreeSpace(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{"50G", 50 * (1 << 30)},
		{"50g", 50 * (1 << 30)},
		{"4T", 4 * (1 << 40)},
		{"512M", 512 * (1 << 20)},
		{"10K", 10 * (1 << 10)},
	}
	for _, c := range cases {
		got, err := ParseMinFreeSpace(c.in)
		if err != nil {
			t.Fatalf("ParseMinFreeSpace(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseMinFreeSpace(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseMinFreeSpace_Invalid(t *testing.T) {
	for _, in := range []string{"", "G", "-5G", "5X", "abc"} {
		if _, err := ParseMinFreeSpace(in); err == nil {
			t.Fatalf("ParseMinFreeSpace(%q): want an error, got nil", in)
		}
	}
}

// TestParseMinFreeSpace_RejectsInt64Overflow proves a value whose
// numeric part times its unit would wrap the int64 result is rejected
// rather than silently turned into a negative threshold: "8388608T" is
// exactly 8388608*(1<<40), one past math.MaxInt64, and a negative
// minFreeBytes would make ComputePoolSpace treat every disk as having
// room, however little is actually free.
func TestParseMinFreeSpace_RejectsInt64Overflow(t *testing.T) {
	for _, in := range []string{"8388608T", "9223372036854775807T"} {
		got, err := ParseMinFreeSpace(in)
		if err == nil {
			t.Fatalf("ParseMinFreeSpace(%q) = %d, want an overflow error", in, got)
		}
	}
}

func TestComputePoolSpace_PoolFreeAndLargestDisk(t *testing.T) {
	ctx := context.Background()
	statter := fakeSpaceStatter{stats: map[string]SpaceStat{
		"/mnt/disk1": {TotalBytes: 100 * (1 << 30), FreeBytes: 60 * (1 << 30)},
		"/mnt/disk2": {TotalBytes: 100 * (1 << 30), FreeBytes: 10 * (1 << 30)},
		"/mnt/disk3": {TotalBytes: 200 * (1 << 30), FreeBytes: 90 * (1 << 30)},
	}}

	got, err := ComputePoolSpace(ctx, statter, []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"}, "50G")
	if err != nil {
		t.Fatalf("ComputePoolSpace: %v", err)
	}

	wantPoolFree := int64((60 + 10 + 90) * (1 << 30))
	if got.PoolFreeBytes != wantPoolFree {
		t.Fatalf("PoolFreeBytes = %d, want %d", got.PoolFreeBytes, wantPoolFree)
	}
	if got.LargestDiskPath != "/mnt/disk3" {
		t.Fatalf("LargestDiskPath = %q, want /mnt/disk3", got.LargestDiskPath)
	}
	if got.LargestDiskFreeBytes != 90*(1<<30) {
		t.Fatalf("LargestDiskFreeBytes = %d, want %d", got.LargestDiskFreeBytes, 90*(1<<30))
	}

	if len(got.Disks) != 3 {
		t.Fatalf("len(Disks) = %d, want 3", len(got.Disks))
	}
	for _, d := range got.Disks {
		want := d.Path == "/mnt/disk2"
		if d.NearMinFreeSpace != want {
			t.Fatalf("disk %s NearMinFreeSpace = %v, want %v (minfreespace 50G, free %d)", d.Path, d.NearMinFreeSpace, want, d.FreeBytes)
		}
	}
}

func TestComputePoolSpace_NoDisks(t *testing.T) {
	if _, err := ComputePoolSpace(context.Background(), fakeSpaceStatter{}, nil, "50G"); !errors.Is(err, ErrNoDataDisks) {
		t.Fatalf("ComputePoolSpace with no disks: err = %v, want ErrNoDataDisks", err)
	}
}

func TestComputePoolSpace_PropagatesStatError(t *testing.T) {
	statErr := errors.New("statfs failed")
	statter := fakeSpaceStatter{errs: map[string]error{"/mnt/disk1": statErr}}
	if _, err := ComputePoolSpace(context.Background(), statter, []string{"/mnt/disk1"}, "50G"); !errors.Is(err, statErr) {
		t.Fatalf("ComputePoolSpace: err = %v, want it to wrap %v", err, statErr)
	}
}

func TestComputePoolSpace_InvalidMinFreeSpace(t *testing.T) {
	statter := fakeSpaceStatter{stats: map[string]SpaceStat{"/mnt/disk1": {}}}
	if _, err := ComputePoolSpace(context.Background(), statter, []string{"/mnt/disk1"}, "not-a-size"); err == nil {
		t.Fatal("ComputePoolSpace with an invalid minfreespace: want an error, got nil")
	}
}

func TestDetectRebalanceSuggestion_FiresForKeepFoldersTogetherWhenOneDiskIsConstrained(t *testing.T) {
	space := PoolSpace{
		Disks: []DiskSpace{
			{Path: "/mnt/disk1", FreeBytes: 1 * (1 << 30), NearMinFreeSpace: true},
			{Path: "/mnt/disk2", FreeBytes: 90 * (1 << 30), NearMinFreeSpace: false},
		},
		LargestDiskPath:      "/mnt/disk2",
		LargestDiskFreeBytes: 90 * (1 << 30),
	}

	got, ok := DetectRebalanceSuggestion(KeepFoldersTogether, space)
	if !ok {
		t.Fatal("DetectRebalanceSuggestion: want a suggestion, got none")
	}
	if got.ConstrainedDiskPath != "/mnt/disk1" {
		t.Fatalf("ConstrainedDiskPath = %q, want /mnt/disk1", got.ConstrainedDiskPath)
	}
	if got.Reason == "" {
		t.Fatal("Reason is empty, want an explanation")
	}
}

func TestDetectRebalanceSuggestion_NoneWhenNoDiskIsConstrained(t *testing.T) {
	space := PoolSpace{
		Disks: []DiskSpace{
			{Path: "/mnt/disk1", FreeBytes: 60 * (1 << 30), NearMinFreeSpace: false},
			{Path: "/mnt/disk2", FreeBytes: 90 * (1 << 30), NearMinFreeSpace: false},
		},
		LargestDiskPath:      "/mnt/disk2",
		LargestDiskFreeBytes: 90 * (1 << 30),
	}
	if _, ok := DetectRebalanceSuggestion(KeepFoldersTogether, space); ok {
		t.Fatal("DetectRebalanceSuggestion: want no suggestion when nothing is constrained")
	}
}

func TestDetectRebalanceSuggestion_NoneForBalanceAcrossDisks(t *testing.T) {
	space := PoolSpace{
		Disks: []DiskSpace{
			{Path: "/mnt/disk1", FreeBytes: 1 * (1 << 30), NearMinFreeSpace: true},
			{Path: "/mnt/disk2", FreeBytes: 90 * (1 << 30), NearMinFreeSpace: false},
		},
		LargestDiskPath:      "/mnt/disk2",
		LargestDiskFreeBytes: 90 * (1 << 30),
	}
	// mfs is not path-preserving (doc 09 §1), so the sharp edge this
	// detects — a path-preserving policy failing toward a specific
	// already-full disk — does not apply to it.
	if _, ok := DetectRebalanceSuggestion(BalanceAcrossDisks, space); ok {
		t.Fatal("DetectRebalanceSuggestion: want no suggestion for a non-path-preserving policy")
	}
}

// TestDetectRebalanceSuggestion_NoneWhenEveryDiskIsConstrained proves
// picking LargestDiskPath by free-byte count alone isn't enough: when
// the largest disk is itself at or below minfreespace, every disk is —
// a pool-wide shortage with nowhere to rebalance toward, not the
// path-preserving-policy sharp edge this detects.
func TestDetectRebalanceSuggestion_NoneWhenEveryDiskIsConstrained(t *testing.T) {
	space := PoolSpace{
		Disks: []DiskSpace{
			{Path: "/mnt/disk1", FreeBytes: 1 * (1 << 30), NearMinFreeSpace: true},
			{Path: "/mnt/disk2", FreeBytes: 2 * (1 << 30), NearMinFreeSpace: true},
		},
		LargestDiskPath:      "/mnt/disk2",
		LargestDiskFreeBytes: 2 * (1 << 30),
	}
	if _, ok := DetectRebalanceSuggestion(KeepFoldersTogether, space); ok {
		t.Fatal("DetectRebalanceSuggestion: want no suggestion when the largest disk is itself constrained")
	}
}

func TestDetectRebalanceSuggestion_NoneWhenTheOnlyDiskIsConstrained(t *testing.T) {
	// A pool-wide shortage, not a path-preserving-policy sharp edge —
	// rebalancing would have nowhere to send the files.
	space := PoolSpace{
		Disks: []DiskSpace{
			{Path: "/mnt/disk1", FreeBytes: 1 * (1 << 30), NearMinFreeSpace: true},
		},
		LargestDiskPath:      "/mnt/disk1",
		LargestDiskFreeBytes: 1 * (1 << 30),
	}
	if _, ok := DetectRebalanceSuggestion(KeepFoldersTogether, space); ok {
		t.Fatal("DetectRebalanceSuggestion: want no suggestion when the constrained disk is the only (and thus largest) one")
	}
}
