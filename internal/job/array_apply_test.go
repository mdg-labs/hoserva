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
