package main

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/pool"
)

// TestRebalanceSharesFromStore_KeepsADiskInRemoval pins that the share
// list the evacuation job's post-check reads still has a branch on a disk
// in removal: without it cache.EvacuationPostCheck would have nothing on
// that disk to check and would pass. Rebalance and evacuation planning
// drop leaving disks in the handler instead (#366).
func TestRebalanceSharesFromStore_KeepsADiskInRemoval(t *testing.T) {
	for _, state := range allRemovalStates {
		t.Run(state, func(t *testing.T) {
			shares, arrays := newMoverTestStores(t)
			putMoverTestArray(t, arrays, "20G")
			insertMoverTestShare(t, shares, "movies", string(pool.ArrayOnly))
			markTestRemoval(t, arrays, "/mnt/disk2", state)

			got, err := rebalanceSharesFromStore(shares, arrays)(context.Background())
			if err != nil {
				t.Fatalf("resolver: %v", err)
			}
			if len(got) != 1 || got[0].Name != "movies" || len(got[0].Branches) != 2 || got[0].Branches[0] != "/mnt/disk1/movies" || got[0].Branches[1] != "/mnt/disk2/movies" {
				t.Fatalf("resolved shares = %+v, want movies on /mnt/disk1 and /mnt/disk2", got)
			}
		})
	}
}
