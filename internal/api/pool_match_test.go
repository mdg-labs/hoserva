package api

import (
	"testing"

	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestMatchArrayDisk_WeakIdentityDifferentSizeDoesNotMatch is #327's own
// data-loss scenario: Q21 matches a weak-identity disk on filesystem UUID
// and size. With a size stored on the array row, a cloned filesystem on a
// different-capacity weak-identity disk must not match — otherwise a
// pulled member is quietly replaced by the clone in GetPool's
// reconciliation. Against UUID-only matching this returns true for the
// wrong disk.
func TestMatchArrayDisk_WeakIdentityDifferentSizeDoesNotMatch(t *testing.T) {
	member := store.ArrayDisk{
		Role:         store.ArrayRoleData,
		RoleIndex:    1,
		Device:       "/dev/sdc",
		Filesystem:   "xfs",
		FSUUID:       "uuid-weak",
		WeakIdentity: true,
		Mountpoint:   "/mnt/disk1",
		Size:         4 * disk.TB,
		SizeSet:      true,
	}
	clone := disk.Disk{Size: 8 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"}
	if idx, ok := matchArrayDisk(clone, []store.ArrayDisk{member}); ok {
		t.Fatalf("matchArrayDisk matched different-size clone at index %d; Q21 requires UUID + equal size", idx)
	}
	same := disk.Disk{Size: 4 * disk.TB, WeakIdentity: true, FSUUID: "uuid-weak"}
	if idx, ok := matchArrayDisk(same, []store.ArrayDisk{member}); !ok || idx != 0 {
		t.Fatalf("matchArrayDisk(same size) = %d, %v; want 0, true", idx, ok)
	}
	// Pre-#327 rows keep NULL size: UUID-only match, same as today.
	legacy := member
	legacy.SizeSet = false
	legacy.Size = 0
	if _, ok := matchArrayDisk(clone, []store.ArrayDisk{legacy}); !ok {
		t.Fatal("matchArrayDisk with SizeSet=false must keep UUID-only matching")
	}
}
