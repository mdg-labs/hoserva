package job

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mdg-labs/hoserva/internal/cache"
)

// fakeOpen is the OpenChecker every unit test running a mover, relocation,
// rebalance or evacuation RunFunc injects: with it those tests never walk
// the host's /proc, so their outcome does not depend on how many processes
// the machine is running (#423). Nothing is open unless a test scripts it.
func fakeOpen() *cache.FakeOpenChecker {
	return cache.NewFakeOpenChecker()
}

// TestRunMover_InjectedOpenCheckerBlocksTheMove proves MoverDeps.Open is the
// checker cache.Run actually consults: a file the injected checker reports
// open stays on the cache and is never copied to the array, so fakeOpen
// genuinely replaces the /proc scan rather than being ignored.
func TestRunMover_InjectedOpenCheckerBlocksTheMove(t *testing.T) {
	s := newTestScheduler(t)
	share := newTestMoverShare(t)
	src := filepath.Join(share.CachePath, "movie.mkv")

	open := fakeOpen()
	open.SetOpen(src, true)
	s.registry.Register(TypeMover, true, RunMover(MoverDeps{
		Shares: func(context.Context) ([]cache.Share, error) { return []cache.Share{share}, nil },
		Open:   open,
	}))

	j, err := s.Submit(context.Background(), TypeMover, nil, nil)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := s.Await(context.Background(), j.ID); err != nil {
		t.Fatalf("Await: %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Errorf("source %s: %v — a file the injected checker reports open must stay in place", src, err)
	}
	if _, err := os.Stat(filepath.Join(share.ArrayPath, "movie.mkv")); err == nil {
		t.Error("a file the injected checker reports open was moved to the array")
	}
}
