package parity

import (
	"errors"
	"strings"
	"testing"
)

func TestLayout_Validate_NoParity(t *testing.T) {
	l := Layout{DataMounts: []string{"/mnt/disk1"}}
	if err := l.Validate(); !errors.Is(err, ErrNoParityDisks) {
		t.Fatalf("Validate: got %v, want ErrNoParityDisks", err)
	}
}

func TestLayout_Validate_TooManyParity(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1", "/mnt/parity2", "/mnt/parity3"}, DataMounts: []string{"/mnt/disk1"}}
	if err := l.Validate(); !errors.Is(err, ErrTooManyParityDisks) {
		t.Fatalf("Validate: got %v, want ErrTooManyParityDisks", err)
	}
}

func TestLayout_Validate_NoData(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}}
	if err := l.Validate(); !errors.Is(err, ErrNoDataDisks) {
		t.Fatalf("Validate: got %v, want ErrNoDataDisks", err)
	}
}

func TestLayout_Validate_RejectsDuplicateDataMount(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1", "/mnt/disk1"}}
	if err := l.Validate(); !errors.Is(err, ErrDuplicateMount) {
		t.Fatalf("Validate: got %v, want ErrDuplicateMount", err)
	}
}

func TestLayout_Validate_RejectsDuplicateParityMount(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1", "/mnt/parity1"}, DataMounts: []string{"/mnt/disk1"}}
	if err := l.Validate(); !errors.Is(err, ErrDuplicateMount) {
		t.Fatalf("Validate: got %v, want ErrDuplicateMount", err)
	}
}

// A mount path spelled differently (a trailing slash, a redundant "./")
// still names the same physical device, so it must collide too — this
// exercises Validate's filepath.Clean normalization, not a byte-for-byte
// comparison a caller could dodge by accident.
func TestLayout_Validate_RejectsEquivalentSpellingsOfTheSameMount(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1/", "/mnt/disk1"}}
	if err := l.Validate(); !errors.Is(err, ErrDuplicateMount) {
		t.Fatalf("Validate: got %v, want ErrDuplicateMount", err)
	}
}

func TestLayout_Validate_RejectsRoleOverlapWithCacheMount(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1"}, CacheMount: "/mnt/disk1"}
	if err := l.Validate(); !errors.Is(err, ErrDuplicateMount) {
		t.Fatalf("Validate: got %v, want ErrDuplicateMount", err)
	}
}

func TestLayout_Validate_RejectsEmptyMount(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{""}}
	if err := l.Validate(); !errors.Is(err, ErrEmptyMount) {
		t.Fatalf("Validate: got %v, want ErrEmptyMount", err)
	}
}

// TestLayout_ContentPaths_RefusesTooFewDistinctDevices is Q18's own
// safety property: a single data disk and no cache means only the boot
// device and that one disk can ever hold a content file copy — two
// distinct devices, never the three Q18 requires — so the generator
// must refuse this layout rather than emit a config that risks losing
// every content file.
func TestLayout_ContentPaths_RefusesTooFewDistinctDevices(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1"}}
	if _, err := l.ContentPaths(); !errors.Is(err, ErrContentPlacement) {
		t.Fatalf("ContentPaths: got %v, want ErrContentPlacement", err)
	}
}

func TestLayout_ContentPaths_SucceedsAtExactlyThreeDevices(t *testing.T) {
	l := Layout{ParityMounts: []string{"/mnt/parity1"}, DataMounts: []string{"/mnt/disk1", "/mnt/disk2"}}
	got, err := l.ContentPaths()
	if err != nil {
		t.Fatalf("ContentPaths: %v", err)
	}
	want := []string{BootContentPath, "/mnt/disk1/snapraid.content", "/mnt/disk2/snapraid.content"}
	if !equalStrings(got, want) {
		t.Fatalf("ContentPaths: got %v, want %v", got, want)
	}
}

func TestLayout_ContentPaths_PrefersCacheBeforeData(t *testing.T) {
	l := Layout{
		ParityMounts: []string{"/mnt/parity1"},
		DataMounts:   []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"},
		CacheMount:   "/mnt/cache",
	}
	got, err := l.ContentPaths()
	if err != nil {
		t.Fatalf("ContentPaths: %v", err)
	}
	// min = 1 (parity) + 2 = 3: boot, cache, disk1 — disk2 and disk3 are
	// not needed and must not appear.
	want := []string{BootContentPath, "/mnt/cache/snapraid.content", "/mnt/disk1/snapraid.content"}
	if !equalStrings(got, want) {
		t.Fatalf("ContentPaths: got %v, want %v", got, want)
	}
}

func TestLayout_ContentPaths_TwoParityNeedsFourCopies(t *testing.T) {
	l := Layout{
		ParityMounts: []string{"/mnt/parity1", "/mnt/parity2"},
		DataMounts:   []string{"/mnt/disk1", "/mnt/disk2"},
	}
	// min = 2 + 2 = 4, but only boot + 2 data disks (3) are available.
	if _, err := l.ContentPaths(); !errors.Is(err, ErrContentPlacement) {
		t.Fatalf("ContentPaths: got %v, want ErrContentPlacement", err)
	}

	l.DataMounts = append(l.DataMounts, "/mnt/disk3")
	got, err := l.ContentPaths()
	if err != nil {
		t.Fatalf("ContentPaths: %v", err)
	}
	want := []string{BootContentPath, "/mnt/disk1/snapraid.content", "/mnt/disk2/snapraid.content", "/mnt/disk3/snapraid.content"}
	if !equalStrings(got, want) {
		t.Fatalf("ContentPaths: got %v, want %v", got, want)
	}
}

func TestLayout_Render_TwoParityDirectiveNames(t *testing.T) {
	l := Layout{
		ParityMounts: []string{"/mnt/parity1", "/mnt/parity2"},
		DataMounts:   []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3"},
	}
	got, err := l.Render()
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{"parity /mnt/parity1/snapraid.parity\n", "2-parity /mnt/parity2/snapraid.parity\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Render() = %q, want it to contain %q", got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
