package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newArrayFixture(t *testing.T) (*ArrayService, *FakeProvider) {
	t.Helper()
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "a", Name: "jellyfin", State: "running"})
	fake.AddContainer(Container{ID: "b", Name: "sonarr", State: "running"})
	fake.AddContainer(Container{ID: "c", Name: "portainer", State: "exited"})
	return &ArrayService{Provider: fake, StatePath: filepath.Join(t.TempDir(), "array-containers.json")}, fake
}

func stateOf(t *testing.T, f *FakeProvider, name string) string {
	t.Helper()
	c, err := f.Inspect(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return c.State
}

func TestArrayService_StopStopsRunningContainersAndStartRestartsOnlyThose(t *testing.T) {
	svc, fake := newArrayFixture(t)
	ctx := context.Background()

	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, name := range []string{"jellyfin", "sonarr", "portainer"} {
		if s := stateOf(t, fake, name); s != "exited" {
			t.Fatalf("%s is %s after Stop, want exited", name, s)
		}
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after Start, want running", s)
	}
	if s := stateOf(t, fake, "sonarr"); s != "running" {
		t.Fatalf("sonarr is %s after Start, want running", s)
	}
	if s := stateOf(t, fake, "portainer"); s != "exited" {
		t.Fatalf("portainer was stopped before the array stop and is %s after Start, want exited", s)
	}
	if _, err := os.Stat(svc.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the list of stopped containers survived a complete Start: %v", err)
	}
}

// A container that will not stop must fail the service, so ArraySequence
// aborts the array stop before any unmount.
func TestArrayService_FailedStopIsAnErrorNamingTheContainer(t *testing.T) {
	svc, fake := newArrayFixture(t)
	fake.FailOn("stop", "jellyfin", errors.New("device or resource busy"))

	err := svc.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop succeeded although a container would not stop")
	}
	if !strings.Contains(err.Error(), "jellyfin") {
		t.Fatalf("error %q does not name the container", err)
	}
	if s := stateOf(t, fake, "sonarr"); s != "exited" {
		t.Fatalf("the other container was left %s; Stop should still stop what it can", s)
	}
}

// After a stop that failed part-way, the retry must still remember the
// container the first attempt already stopped.
func TestArrayService_RetryAfterFailedStopRemembersWhatWasAlreadyStopped(t *testing.T) {
	svc, fake := newArrayFixture(t)
	ctx := context.Background()
	fake.FailOn("stop", "jellyfin", errors.New("busy"))
	if err := svc.Stop(ctx); err == nil {
		t.Fatal("first Stop succeeded")
	}
	fake.FailOn("stop", "jellyfin", nil)
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("retry Stop: %v", err)
	}

	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := stateOf(t, fake, "sonarr"); s != "running" {
		t.Fatalf("sonarr, stopped by the first attempt, is %s after Start, want running", s)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after Start, want running", s)
	}
}

func TestArrayService_TheListSurvivesADaemonRestart(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := &ArrayService{Provider: fake, StatePath: svc.StatePath}
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s, want running", s)
	}
}

// If a container will not start, the ones already started must be stopped
// again: the array is rolled back and unmounted next, and a container must
// not run against an unmounted pool.
func TestArrayService_FailedStartStopsWhatItStartedAndKeepsTheList(t *testing.T) {
	svc, fake := newArrayFixture(t)
	ctx := context.Background()
	if err := svc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	fake.FailOn("start", "sonarr", errors.New("port already allocated"))

	if err := svc.Start(ctx); err == nil {
		t.Fatal("Start succeeded although a container would not start")
	}
	if s := stateOf(t, fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s, want it stopped again after the failed Start", s)
	}

	fake.FailOn("start", "sonarr", nil)
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	for _, name := range []string{"jellyfin", "sonarr"} {
		if s := stateOf(t, fake, name); s != "running" {
			t.Fatalf("%s is %s after the retry, want running", name, s)
		}
	}
}

func TestArrayService_FailedRestopIsReported(t *testing.T) {
	svc, fake := newArrayFixture(t)
	ctx := context.Background()
	if err := svc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	fake.FailOn("start", "sonarr", errors.New("port already allocated"))
	fake.FailOn("stop", "jellyfin", errors.New("busy"))

	err := svc.Start(ctx)
	if err == nil || !strings.Contains(err.Error(), "again after a failed start") {
		t.Fatalf("Start error = %v, want it to say a container could not be stopped again", err)
	}
}

func TestArrayService_ContainerRemovedWhileStoppedIsDropped(t *testing.T) {
	svc, fake := newArrayFixture(t)
	ctx := context.Background()
	if err := svc.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fake.Remove(ctx, "sonarr", RemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s, want running", s)
	}
}

// Docker not installed is the normal state of a host without Apps: the
// array stop must not be blocked by it, and Start has nothing to do.
func TestArrayService_NoDockerIsNotAnError(t *testing.T) {
	fake := NewFakeProvider()
	fake.SetUnavailable(nil)
	svc := &ArrayService{Provider: fake, StatePath: filepath.Join(t.TempDir(), "s.json")}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// Docker that is installed but stops answering after containers were
// recorded must not let Start report success: the containers are owed.
func TestArrayService_StartFailsWhenDockerIsUnreachableWithContainersOwed(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetUnavailable(nil)
	if err := svc.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded although Docker was unreachable and containers were owed")
	}
}

func TestArrayService_ListFailureThatIsNotUnavailabilityFailsStop(t *testing.T) {
	fake := NewFakeProvider()
	fake.SetUnavailable(errors.New("permission denied on the docker socket"))
	svc := &ArrayService{Provider: fake, StatePath: filepath.Join(t.TempDir(), "s.json")}
	if err := svc.Stop(context.Background()); err == nil {
		t.Fatal("Stop succeeded although the container list could not be read")
	}
}

// An update reboot or UPS shutdown stops the containers through the Engine
// without a persisted array stop, and dockerd does not bring a container
// stopped that way back on its own: the daemon starts them at the next
// boot, once storage is ready.
func TestArrayService_RestoreAfterShutdownStartsTheRecordedContainers(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	booted := &ArrayService{
		Provider:     fake,
		StatePath:    svc.StatePath,
		Halted:       func() bool { return false },
		StorageReady: func() bool { return true },
	}
	retry, err := booted.restoreOnce(context.Background())
	if err != nil || retry {
		t.Fatalf("restoreOnce = (%v, %v), want (false, nil)", retry, err)
	}
	for _, name := range []string{"jellyfin", "sonarr"} {
		if s := stateOf(t, fake, name); s != "running" {
			t.Fatalf("%s is %s after the boot restore, want running", name, s)
		}
	}
	if s := stateOf(t, fake, "portainer"); s != "exited" {
		t.Fatalf("portainer was stopped before the shutdown and is %s, want exited", s)
	}
	if _, err := os.Stat(svc.StatePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the list survived a complete restore: %v", err)
	}
}

// A persisted `array stop` is the user's: nothing starts until `array
// start`, which still owes the whole list.
func TestArrayService_RestoreAfterShutdownNeverStartsWhileTheArrayIsStopped(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	booted := &ArrayService{
		Provider:     fake,
		StatePath:    svc.StatePath,
		Halted:       func() bool { return true },
		StorageReady: func() bool { return true },
	}
	retry, err := booted.restoreOnce(context.Background())
	if err != nil || retry {
		t.Fatalf("restoreOnce = (%v, %v), want (false, nil)", retry, err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s while the array is stopped, want exited", s)
	}
	if err := booted.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after array start, want running", s)
	}
}

func TestArrayService_RestoreAfterShutdownWaitsForStorageAndDocker(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := false
	booted := &ArrayService{
		Provider:     fake,
		StatePath:    svc.StatePath,
		Halted:       func() bool { return false },
		StorageReady: func() bool { return ready },
	}

	retry, err := booted.restoreOnce(context.Background())
	if err != nil || !retry {
		t.Fatalf("restoreOnce before storage is ready = (%v, %v), want (true, nil)", retry, err)
	}
	ready = true
	fake.SetUnavailable(nil)
	retry, err = booted.restoreOnce(context.Background())
	if err != nil || !retry {
		t.Fatalf("restoreOnce before Docker answers = (%v, %v), want (true, nil)", retry, err)
	}
	if _, err := os.Stat(svc.StatePath); err != nil {
		t.Fatalf("the list was lost while waiting: %v", err)
	}
}

func TestArrayService_RestoreAfterShutdownKeepsWhatWillNotStart(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.FailOn("start", "sonarr", errors.New("port already allocated"))
	booted := &ArrayService{Provider: fake, StatePath: svc.StatePath}

	retry, err := booted.restoreOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sonarr") || retry {
		t.Fatalf("restoreOnce = (%v, %v), want (false, an error naming sonarr)", retry, err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s, want running: one container failing must not stop the others", s)
	}
	st, err := booted.load()
	if err != nil || len(st.Containers) != 1 || st.Containers[0] != "sonarr" {
		t.Fatalf("list = %v (%v), want only sonarr", st.Containers, err)
	}
}

func TestArrayService_RestoreAfterShutdownRetriesUntilStorageIsReady(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var polls atomic.Int32
	booted := &ArrayService{
		Provider:  fake,
		StatePath: svc.StatePath,
		StorageReady: func() bool {
			return polls.Add(1) > 2
		},
	}
	if err := booted.RestoreAfterShutdown(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("RestoreAfterShutdown: %v", err)
	}
	if s := stateOf(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s, want running", s)
	}
}

func TestArrayService_RestoreAfterShutdownStopsWhenTheContextEnds(t *testing.T) {
	svc, fake := newArrayFixture(t)
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	booted := &ArrayService{Provider: fake, StatePath: svc.StatePath, StorageReady: func() bool { return false }}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := booted.RestoreAfterShutdown(ctx, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RestoreAfterShutdown = %v, want the context's error", err)
	}
}
