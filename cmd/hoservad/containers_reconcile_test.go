package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	b := &lockedBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

func reconcileCalls(f *container.FakeProvider) int {
	n := 0
	for _, c := range f.Calls() {
		if c.Op == "reconcile" {
			n++
		}
	}
	return n
}

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// The interrupted-recreate reconciliation starts a container, so it waits
// for the array to be up, and it says what it found in the daemon log.
func TestReconcileContainersAtStart_WaitsForTheArrayThenReportsWhatItFound(t *testing.T) {
	logs := captureLog(t)
	fake := container.NewFakeProvider()
	fake.SetReconciliation([]container.Reconciliation{
		{Name: "jellyfin", Outcome: container.ReconcileRestored, Detail: "renamed jellyfin_hoserva-old back to jellyfin"},
		{Name: "sonarr", Outcome: container.ReconcileAmbiguous, Detail: "sonarr, sonarr_hoserva-old all exist; nothing was removed"},
	})
	apps := newContainers(fake, t.TempDir(), nil, nil)

	var halted, ready atomic.Bool
	halted.Store(true)
	done := reconcileContainersAtStart(context.Background(), apps, halted.Load, ready.Load, 5*time.Millisecond)

	if closedWithin(done, 100*time.Millisecond) || reconcileCalls(fake) != 0 {
		t.Fatalf("reconciled while the array was stopped: %v", fake.Calls())
	}
	halted.Store(false)
	if closedWithin(done, 100*time.Millisecond) || reconcileCalls(fake) != 0 {
		t.Fatalf("reconciled while storage was not ready: %v", fake.Calls())
	}
	ready.Store(true)
	if !closedWithin(done, 5*time.Second) {
		t.Fatal("the reconciliation did not finish once the array was up")
	}
	if n := reconcileCalls(fake); n != 1 {
		t.Fatalf("reconcile ran %d times, want once", n)
	}
	out := logs.String()
	for _, want := range []string{"jellyfin", "restored", "renamed jellyfin_hoserva-old back to jellyfin", "sonarr", "ambiguous", "nothing was removed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the daemon log does not report %q:\n%s", want, out)
		}
	}
}

func TestReconcileContainersAtStart_FailsClosedWithoutTheArrayState(t *testing.T) {
	fake := container.NewFakeProvider()
	apps := newContainers(fake, t.TempDir(), nil, nil)
	done := reconcileContainersAtStart(context.Background(), apps, nil, nil, 5*time.Millisecond)
	if closedWithin(done, 100*time.Millisecond) || reconcileCalls(fake) != 0 {
		t.Fatalf("reconciled with no array state to read: %v", fake.Calls())
	}
}

// Docker not answering yet is what a boot looks like: the pass is tried
// again, and not finished until one completes.
func TestReconcileContainersAtStart_RetriesWhileDockerIsUnavailable(t *testing.T) {
	logs := captureLog(t)
	fake := container.NewFakeProvider()
	fake.FailOn("reconcile", "", container.ErrUnavailable)
	apps := newContainers(fake, t.TempDir(), nil, nil)
	done := reconcileContainersAtStart(context.Background(), apps, func() bool { return false }, func() bool { return true }, 5*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for reconcileCalls(fake) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("the reconciliation was not retried")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if closedWithin(done, 20*time.Millisecond) {
		t.Fatal("the reconciliation finished although Docker never answered")
	}
	fake.FailOn("reconcile", "", nil)
	if !closedWithin(done, 5*time.Second) {
		t.Fatal("the reconciliation did not finish once Docker answered")
	}
	if strings.Contains(logs.String(), "reconciling") {
		t.Fatalf("Docker being unavailable was logged as a failure:\n%s", logs.String())
	}
}

// A pass that fails for another reason is logged and finishes: the other
// containers' recreates are not held up by one that cannot be reconciled,
// and the leftover is still there for the next start.
func TestReconcileContainersAtStart_LogsAFailureAndFinishes(t *testing.T) {
	logs := captureLog(t)
	fake := container.NewFakeProvider()
	fake.FailOn("reconcile", "", errors.New("container \"jellyfin\": starting the original container again: port is already allocated"))
	apps := newContainers(fake, t.TempDir(), nil, nil)
	done := reconcileContainersAtStart(context.Background(), apps, func() bool { return false }, func() bool { return true }, 5*time.Millisecond)
	if !closedWithin(done, 5*time.Second) {
		t.Fatal("the reconciliation did not finish after a failure")
	}
	if n := reconcileCalls(fake); n != 1 {
		t.Fatalf("reconcile ran %d times, want once", n)
	}
	if !strings.Contains(logs.String(), "port is already allocated") {
		t.Fatalf("the failure was not logged:\n%s", logs.String())
	}
}

// The daemon wiring runs no recreate job before the reconciliation has
// finished: while Docker is not answering, the job waits, and once the
// pass completes the Engine sees reconcile before recreate.
func TestContainersWiring_RecreateJobWaitsForTheStartupReconciliation(t *testing.T) {
	captureLog(t)
	w := newContainersWiringHarnessWith(t, func(f *container.FakeProvider) {
		f.FailOn("reconcile", "", container.ErrUnavailable)
	})

	j, err := w.scheduler.Submit(context.Background(), job.TypeContainerRecreate, []string{"container:jellyfin"}, []byte(`{"id":"jellyfin"}`))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	w.startReconcile()
	time.Sleep(200 * time.Millisecond)
	for _, c := range w.fake.Calls() {
		if c.Op == "recreate" {
			t.Fatalf("the recreate job reached the Engine before the reconciliation finished: %v", w.fake.Calls())
		}
	}
	_, body := w.do(t, http.MethodGet, "/jobs/"+j.ID)
	if !bytes.Contains(body, []byte(`"running"`)) {
		t.Fatalf("the recreate job should be waiting as a running job: %s", body)
	}

	w.fake.FailOn("reconcile", "", nil)
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, body = w.do(t, http.MethodGet, "/jobs/"+j.ID)
		if bytes.Contains(body, []byte(`"succeeded"`)) {
			break
		}
		if bytes.Contains(body, []byte(`"failed"`)) || time.Now().After(deadline) {
			t.Fatalf("job did not succeed once the reconciliation finished: %s", body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var ops []string
	for _, c := range w.fake.Calls() {
		if c.Op == "reconcile" || c.Op == "recreate" {
			ops = append(ops, c.Op)
		}
	}
	last := len(ops) - 1
	if last < 1 || ops[last] != "recreate" || ops[last-1] != "reconcile" {
		t.Fatalf("Engine calls = %v, want a completed reconcile before the recreate", ops)
	}
}

// A wiring that never starts the reconciliation gives up on a recreate
// with an error when its context ends, rather than recreating on a guess.
func TestAwaitReconciled_EndsWithTheContextWhenTheReconciliationNeverRuns(t *testing.T) {
	apps := newContainers(container.NewFakeProvider(), t.TempDir(), nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := apps.awaitReconciled(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("awaitReconciled = %v, want the context's deadline", err)
	}
}
