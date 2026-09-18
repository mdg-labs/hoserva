package disk

import (
	"errors"
	"testing"
)

func TestTopologyPlan_Validate_OK(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data: []AssignedDisk{
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sdc", Filesystem: EXT4},
		},
		Cache: &AssignedDisk{Device: "/dev/sdd", Filesystem: XFS},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB, "/dev/sdc": 8 * TB, "/dev/sdd": TB}

	if err := plan.Validate(sizes); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTopologyPlan_Validate_NoParity(t *testing.T) {
	plan := TopologyPlan{Data: []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}}}
	if err := plan.Validate(nil); !errors.Is(err, ErrNoParityDisks) {
		t.Fatalf("Validate: got %v, want ErrNoParityDisks", err)
	}
}

func TestTopologyPlan_Validate_TooManyParity(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{
			{Device: "/dev/sda", Filesystem: XFS},
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sdc", Filesystem: XFS},
		},
		Data: []AssignedDisk{{Device: "/dev/sdd", Filesystem: XFS}},
	}
	if err := plan.Validate(nil); !errors.Is(err, ErrTooManyParityDisks) {
		t.Fatalf("Validate: got %v, want ErrTooManyParityDisks", err)
	}
}

func TestTopologyPlan_Validate_NoData(t *testing.T) {
	plan := TopologyPlan{Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}}}
	if err := plan.Validate(nil); !errors.Is(err, ErrNoDataDisks) {
		t.Fatalf("Validate: got %v, want ErrNoDataDisks", err)
	}
}

func TestTopologyPlan_Validate_ParityTooSmall(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 4 * TB, "/dev/sdb": 8 * TB}
	if err := plan.Validate(sizes); !errors.Is(err, ErrParityTooSmall) {
		t.Fatalf("Validate: got %v, want ErrParityTooSmall", err)
	}
}

func TestTopologyPlan_Validate_ParityEqualToLargestDataDiskOK(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 8 * TB}
	if err := plan.Validate(sizes); err != nil {
		t.Fatalf("Validate: got %v, want nil (parity == largest data disk is allowed)", err)
	}
}

func TestTopologyPlan_Validate_ParityNotXFS(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: EXT4}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB}
	if err := plan.Validate(sizes); !errors.Is(err, ErrParityNotXFS) {
		t.Fatalf("Validate: got %v, want ErrParityNotXFS", err)
	}
}

func TestTopologyPlan_Validate_ParityCannotAdopt(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS, Adopt: true}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB}
	if err := plan.Validate(sizes); !errors.Is(err, ErrParityCannotAdopt) {
		t.Fatalf("Validate: got %v, want ErrParityCannotAdopt", err)
	}
}

func TestTopologyPlan_Validate_UnsupportedDataFilesystem(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: FilesystemType("zfs")}},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB}
	if err := plan.Validate(sizes); !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Fatalf("Validate: got %v, want ErrUnsupportedFilesystem", err)
	}
}

func TestTopologyPlan_Validate_UnsupportedCacheFilesystem(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
		Cache:  &AssignedDisk{Device: "/dev/sdc", Filesystem: FilesystemType("zfs")},
	}
	sizes := map[string]int64{"/dev/sda": 8 * TB, "/dev/sdb": 4 * TB, "/dev/sdc": TB}
	if err := plan.Validate(sizes); !errors.Is(err, ErrUnsupportedFilesystem) {
		t.Fatalf("Validate: got %v, want ErrUnsupportedFilesystem", err)
	}
}

func TestTopologyPlan_Confirmation_ListsErasedDevicesSorted(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sdz", Filesystem: XFS}},
		Data: []AssignedDisk{
			{Device: "/dev/sdb", Filesystem: XFS},
			{Device: "/dev/sda", Filesystem: XFS, Adopt: true}, // kept, must not appear
		},
	}
	want := "ERASE /dev/sdb, /dev/sdz"
	if got := plan.Confirmation(); got != want {
		t.Fatalf("Confirmation: got %q, want %q", got, want)
	}
}

func TestTopologyPlan_Confirmation_AdoptOnly(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS, Adopt: true}},
	}
	// Parity is never adopted (ErrParityCannotAdopt), so it still counts.
	want := "ERASE /dev/sda"
	if got := plan.Confirmation(); got != want {
		t.Fatalf("Confirmation: got %q, want %q", got, want)
	}
}

func TestTopologyPlan_CheckConfirmation(t *testing.T) {
	plan := TopologyPlan{
		Parity: []AssignedDisk{{Device: "/dev/sda", Filesystem: XFS}},
		Data:   []AssignedDisk{{Device: "/dev/sdb", Filesystem: XFS}},
	}

	if err := plan.CheckConfirmation("ERASE /dev/sda, /dev/sdb"); err != nil {
		t.Fatalf("CheckConfirmation(exact match): %v", err)
	}
	if err := plan.CheckConfirmation("yes"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("CheckConfirmation(generic text): got %v, want ErrConfirmationMismatch", err)
	}
	if err := plan.CheckConfirmation("ERASE /dev/sda"); !errors.Is(err, ErrConfirmationMismatch) {
		t.Fatalf("CheckConfirmation(subset of the real plan): got %v, want ErrConfirmationMismatch", err)
	}
}
