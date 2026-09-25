package job

import (
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

// TestPoolStateFromStore_SetsRemovingDiskFromRemovalState proves
// writeArrayFromStore's own PoolState derivation carries a data disk's
// removal_state ('evacuating') into config.PoolState.RemovingDisk (#359,
// doc 09 §4 step 2) — the field every WriteCatchAllMount call this
// package makes (applyArrayFromStore, regenerateArrayFromStore) reads to
// choose pool.CatchAllMountRemoving over the plain builder. The disk
// itself must stay listed among DataDisks — removal only marks it
// no-create, doc 09 §4 step 7 (not this issue) is what actually drops it.
func TestPoolStateFromStore_SetsRemovingDiskFromRemovalState(t *testing.T) {
	settings := store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "10G"}
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk1", RemovalState: store.RemovalStateEvacuating},
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk2"},
		{Role: store.ArrayRoleCache, Mountpoint: "/mnt/cache"},
	}

	state := poolStateFromStore(settings, disks)

	if state.RemovingDisk != "/mnt/disk1" {
		t.Fatalf("RemovingDisk = %q, want /mnt/disk1", state.RemovingDisk)
	}
	if len(state.DataDisks) != 2 {
		t.Fatalf("DataDisks = %+v, want both data disks still listed", state.DataDisks)
	}
}

// TestPoolStateFromStore_EvacuatedAlsoSetsRemovingDisk proves the
// 'evacuated' state (doc 09 §4 step 2's own "still no-create" window
// between the copy finishing and steps 7-9 running) is treated exactly
// like 'evacuating' for mount generation.
func TestPoolStateFromStore_EvacuatedAlsoSetsRemovingDisk(t *testing.T) {
	settings := store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "10G"}
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk1", RemovalState: store.RemovalStateEvacuated},
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk2"},
	}

	state := poolStateFromStore(settings, disks)

	if state.RemovingDisk != "/mnt/disk1" {
		t.Fatalf("RemovingDisk = %q, want /mnt/disk1 (evacuated still means no-create)", state.RemovingDisk)
	}
}

// TestPoolStateFromStore_NoRemovalState_LeavesRemovingDiskEmpty proves the
// common case is unaffected: with no disk in removal, RemovingDisk stays
// empty, so every WriteCatchAllMount call keeps using the plain builder
// and produces byte-identical units to before #359.
func TestPoolStateFromStore_NoRemovalState_LeavesRemovingDiskEmpty(t *testing.T) {
	settings := store.ArraySettings{CreatePolicy: "mfs", MinFreeSpace: "10G"}
	disks := []store.ArrayDisk{
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk1"},
		{Role: store.ArrayRoleData, Mountpoint: "/mnt/disk2"},
	}

	state := poolStateFromStore(settings, disks)

	if state.RemovingDisk != "" {
		t.Fatalf("RemovingDisk = %q, want empty", state.RemovingDisk)
	}
}

// TestPoolStateAndLayoutFromStore_DiskLeavingTheArray is #358's generator
// filter: an unpooled disk is in no pool branch list but still in the
// SnapRAID layout (it must be synced empty while listed); an unlisted
// one is in neither. An evacuating or evacuated disk stays a branch.
func TestPoolStateAndLayoutFromStore_DiskLeavingTheArray(t *testing.T) {
	for _, tc := range []struct {
		state            string
		inPool, inLayout bool
	}{
		{state: "", inPool: true, inLayout: true},
		{state: store.RemovalStateEvacuating, inPool: true, inLayout: true},
		{state: store.RemovalStateEvacuated, inPool: true, inLayout: true},
		{state: store.RemovalStateUnpooled, inPool: false, inLayout: true},
		{state: store.RemovalStateUnlisted, inPool: false, inLayout: false},
	} {
		disks := []store.ArrayDisk{
			{Role: store.ArrayRoleParity, RoleIndex: 1, Mountpoint: "/mnt/parity1"},
			{Role: store.ArrayRoleData, RoleIndex: 1, Mountpoint: "/mnt/disk1"},
			{Role: store.ArrayRoleData, RoleIndex: 2, Mountpoint: "/mnt/disk2", RemovalState: tc.state},
			{Role: store.ArrayRoleData, RoleIndex: 3, Mountpoint: "/mnt/disk3"},
		}
		state := poolStateFromStore(store.ArraySettings{}, disks)
		var inPool bool
		for _, d := range state.DataDisks {
			inPool = inPool || d == "/mnt/disk2"
		}
		if inPool != tc.inPool || (!tc.inPool && state.RemovingDisk == "/mnt/disk2") {
			t.Fatalf("%q: /mnt/disk2 in the pool = %v (removing %q), want %v", tc.state, inPool, state.RemovingDisk, tc.inPool)
		}
		var inLayout bool
		for _, m := range layoutFromStore(disks).DataMounts {
			if m.Mountpoint == "/mnt/disk2" {
				inLayout = true
			}
			if m.Mountpoint == "/mnt/disk3" && m.RoleIndex != 3 {
				t.Fatalf("%q: /mnt/disk3 renamed to role_index %d", tc.state, m.RoleIndex)
			}
		}
		if inLayout != tc.inLayout {
			t.Fatalf("%q: /mnt/disk2 in the SnapRAID layout = %v, want %v", tc.state, inLayout, tc.inLayout)
		}
	}
}
