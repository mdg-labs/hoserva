package job

import (
	"context"
	"testing"

	"github.com/mdg-labs/hoserva/internal/parity"
)

func TestEngineDiffGuard_BlockedDiffDoesNotNeedSync(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{
		Removed: 600,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 400},
		},
	})
	g := EngineDiffGuard{Engine: eng, Guard: parity.Guard{}}
	blocked, err := g.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !blocked {
		t.Fatal("Evaluate blocked = false, want true for 600 removals")
	}
}

func TestEngineDiffGuard_SmallDiffIsNotBlocked(t *testing.T) {
	eng := parity.NewFakeEngine()
	eng.SetDiff(parity.DiffReport{
		Removed: 1,
		PerDisk: map[string]parity.DiskDiff{
			"/mnt/disk1": {FilesBefore: 1000, FilesAfter: 999},
		},
	})
	g := EngineDiffGuard{Engine: eng, Guard: parity.Guard{}}
	blocked, err := g.Evaluate(context.Background())
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if blocked {
		t.Fatal("Evaluate blocked = true, want false for one removal")
	}
}
