package main

import (
	"context"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// init keeps a second, permanent SIGHUP registration for this test
// binary's whole lifetime: SIGHUP's default disposition is to terminate
// the process, and TestInstallReloadHandler_StopsWithContext deliberately
// calls signal.Stop on installReloadHandler's own channel mid-test. Go
// only reverts a signal to its default disposition once every Notify
// registration for it has been stopped — this placeholder means that
// never happens, so a SIGHUP raised anywhere near that test's own timing
// window can never kill the test binary instead of merely being dropped
// on the floor.
func init() {
	signal.Notify(make(chan os.Signal, 1), syscall.SIGHUP)
}

// TestInstallReloadHandler_RunsOneRebuildOnInstall proves installer's own
// unconditional rebuild (#372 finding 2): without it, a disk that arrives
// before this process's own READY=1 leaves nothing queued in sighup at
// all (systemd merges that reload into the still-running start job and
// never runs ExecReload), so only a call that runs regardless of whether
// a signal was ever delivered can catch it.
func TestInstallReloadHandler_RunsOneRebuildOnInstall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sighup := make(chan os.Signal, 1)
	var calls int32
	installReloadHandler(ctx, sighup, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("rebuild call count = %d, want 1 immediately on install, with no signal ever sent", got)
	}
}

// TestInstallReloadHandler_SignalQueuedBeforeInstallStillRebuilds is
// finding 2's other half: a SIGHUP delivered after newReloadSignal
// registered sighup, but before installReloadHandler could run — the
// window main.go's own startup work between disk listing and building
// rebuild leaves open — must be drained here rather than left for some
// later, unrelated SIGHUP, and must never take Go's default (process
// exit) action in the meantime.
func TestInstallReloadHandler_SignalQueuedBeforeInstallStillRebuilds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sighup := newReloadSignal()
	defer signal.Stop(sighup)

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP to self: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the signal land in sighup

	var calls int32
	installReloadHandler(ctx, sighup, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})

	// installReloadHandler's own unconditional rebuild above runs
	// synchronously, before its handler goroutine is even started, so
	// asserting immediately after it returns would pass whether or not
	// the queued signal was ever drained — the goroutine has not had a
	// chance to consume it yet either way. Waiting first gives that
	// goroutine time to receive the still-queued signal and call rebuild
	// a second time if the drain is missing, so the assertion below
	// actually exercises it.
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("rebuild call count = %d, want exactly 1 — the queued signal must be drained, not double-counted against the unconditional install-time rebuild", got)
	}
}

// TestInstallReloadHandler_SIGHUPTriggersRebuild proves finding 4's own
// entry point actually runs: packaging/debian/hoserva-storage.rules's
// RUN+= sends hoserva.service SIGHUP, ExecReload turns that into this
// process's own SIGHUP, and installReloadHandler must call rebuild for
// it — the same mechanism a disk reappearing while hoservad is already
// running relies on to reach disk.StorageGate without a reboot or an
// unrelated share edit.
func TestInstallReloadHandler_SIGHUPTriggersRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	var calls int32
	done := make(chan struct{}, 1)
	installReloadHandler(ctx, sighup, func(context.Context) error {
		if atomic.AddInt32(&calls, 1) == 2 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
		return nil
	})

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP to self: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("rebuild was not called a second time within 5s of the SIGHUP")
	}
	// One call for install itself, one for the SIGHUP sent above.
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("rebuild call count = %d, want 2", got)
	}
}

// TestInstallReloadHandler_StopsWithContext proves the handler goroutine
// does not outlive cmd/hoservad's own lifetime: once ctx is cancelled, a
// later SIGHUP must not call rebuild again.
func TestInstallReloadHandler_StopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)

	var calls int32
	installReloadHandler(ctx, sighup, func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	cancel()
	// Give the handler goroutine time to observe ctx.Done() and call
	// signal.Stop before this test's own SIGHUP below.
	time.Sleep(100 * time.Millisecond)

	before := atomic.LoadInt32(&calls) // the unconditional install-time call

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("sending SIGHUP to self: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if got := atomic.LoadInt32(&calls); got != before {
		t.Fatalf("rebuild call count = %d, want unchanged at %d once the handler has stopped", got, before)
	}
}
