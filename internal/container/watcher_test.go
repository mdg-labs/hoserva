package container

import (
	"context"
	"sync"
	"testing"
	"time"
)

func startWatcher(t *testing.T, fake *FakeProvider, onUnhealthy func(StateChange)) *Hub {
	t.Helper()
	hub := NewHub()
	w := &Watcher{Provider: fake, Hub: hub, OnUnhealthy: onUnhealthy, Retry: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// Run subscribes asynchronously; wait until the fake sees the watcher
	// so an immediately following Kill cannot be emitted before it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.watchers)
		fake.mu.Unlock()
		if n > 0 {
			return hub
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher never subscribed to the Engine's events")
		}
		time.Sleep(time.Millisecond)
	}
}

// A container killed from outside Hoserva reaches an event subscriber as
// an event; nothing in this test lists or inspects the container to find
// out, so it cannot have been polled.
func TestWatcher_KilledContainerArrivesAsAnEvent(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running"})
	hub := startWatcher(t, fake, nil)
	ch, unsub := hub.Subscribe()
	defer unsub()

	if err := fake.Kill("c1"); err != nil {
		t.Fatal(err)
	}

	select {
	case sc := <-ch:
		if sc.ID != "c1" || sc.Name != "jellyfin" || sc.State != "exited" {
			t.Fatalf("event = %+v, want jellyfin exited", sc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event for a killed container")
	}
}

func TestWatcher_UnhealthyTransitionNotifiesOncePerTransition(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running", Health: HealthHealthy})
	var mu sync.Mutex
	var got []StateChange
	notified := make(chan struct{}, 8)
	hub := startWatcher(t, fake, func(sc StateChange) {
		mu.Lock()
		got = append(got, sc)
		mu.Unlock()
		notified <- struct{}{}
	})
	ch, unsub := hub.Subscribe()
	defer unsub()

	for _, h := range []string{HealthUnhealthy, HealthUnhealthy} {
		if err := fake.SetHealth("c1", h); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		select {
		case sc := <-ch:
			if sc.Health != HealthUnhealthy {
				t.Fatalf("event = %+v, want unhealthy", sc)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no health event")
		}
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("no unhealthy notification")
	}
	// Recovering and failing again is a new transition.
	if err := fake.SetHealth("c1", HealthHealthy); err != nil {
		t.Fatal(err)
	}
	if err := fake.SetHealth("c1", HealthUnhealthy); err != nil {
		t.Fatal(err)
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("no notification for the second unhealthy transition")
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("notified %d times, want 2 (one per transition into unhealthy)", len(got))
	}
}

func TestWatcher_ReconnectsAfterTheEngineWasUnavailable(t *testing.T) {
	fake := NewFakeProvider()
	fake.AddContainer(Container{ID: "c1", Name: "jellyfin", State: "running"})
	fake.SetUnavailable(nil)
	hub := NewHub()
	w := &Watcher{Provider: fake, Hub: hub, Retry: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	ch, unsub := hub.Subscribe()
	defer unsub()

	time.Sleep(30 * time.Millisecond)
	fake.mu.Lock()
	fake.listErr = nil
	fake.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		n := len(fake.watchers)
		fake.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher never reconnected")
		}
		time.Sleep(time.Millisecond)
	}
	if err := fake.Kill("c1"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("no event after reconnecting")
	}
}
