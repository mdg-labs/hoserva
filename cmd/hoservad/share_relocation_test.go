package main

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/pool"
	"github.com/mdg-labs/hoserva/internal/store"
)

// TestShareRelocationShareFromStore_LeavesOutADiskInRemoval proves #366's
// share-relocation half: a data disk past "evacuating" is not one of the
// relocation's Branches, and an "evacuating" one still is, since a
// relocation to cache reads the share's files from Branches.
func TestShareRelocationShareFromStore_LeavesOutADiskInRemoval(t *testing.T) {
	for _, c := range []struct {
		state string
		want  []string
	}{
		{store.RemovalStateEvacuating, []string{"/mnt/disk1/movies", "/mnt/disk2/movies"}},
		{store.RemovalStateEvacuated, []string{"/mnt/disk1/movies"}},
		{store.RemovalStateUnpooled, []string{"/mnt/disk1/movies"}},
		{store.RemovalStateUnlisted, []string{"/mnt/disk1/movies"}},
	} {
		t.Run(c.state, func(t *testing.T) {
			shares, arrays := newMoverTestStores(t)
			putMoverTestArray(t, arrays, "20G")
			insertMoverTestShare(t, shares, "movies", string(pool.CacheOnly))
			markTestRemoval(t, arrays, "/mnt/disk2", c.state)

			got, err := shareRelocationShareFromStore(shares, arrays)(context.Background(), "movies")
			if err != nil {
				t.Fatalf("resolver: %v", err)
			}
			if len(got.Branches) != len(c.want) {
				t.Fatalf("Branches = %v, want %v", got.Branches, c.want)
			}
			for i := range c.want {
				if got.Branches[i] != c.want[i] {
					t.Fatalf("Branches = %v, want %v", got.Branches, c.want)
				}
			}
		})
	}
}
