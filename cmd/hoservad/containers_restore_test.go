package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/job"
)

// restoreBoot is the second boot of the two-process tests below: the
// container service built the way main.go builds it, over the same state
// directory the first boot's service wrote, wired to the new process's own
// scheduler.
func restoreBoot(t *testing.T, w *maintenanceRestartWiring, fake *container.FakeProvider, stateDir string) *appServices {
	t.Helper()
	apps := newContainers(fake, stateDir, nil, nil)
	done := restoreContainersAfterShutdown(context.Background(), apps, w.scheduler.InMaintenance, func() bool { return true })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot restore did not finish")
	}
	return apps
}

// An update reboot or UPS shutdown stops the containers through the Engine
// with no persisted array stop. The next boot must start them again: the
// Engine records them as stopped on purpose and dockerd does not.
func TestContainerRestore_RebootStartsTheContainersTheShutdownStopped(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "containers-reboot.db")
	stateDir := t.TempDir()
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "running"})
	fake.AddContainer(container.Container{ID: "b", Name: "portainer", State: "exited"})

	w1 := newMaintenanceRestartWiring(t, dbPath)
	var mountCalls int32
	seq1 := fakeArraySequence(w1, &mountCalls)
	seq1.Services = []job.ArrayService{newContainers(fake, stateDir, nil, nil).arrayService()}
	if err := seq1.StopForShutdown(ctx); err != nil {
		t.Fatalf("StopForShutdown: %v", err)
	}
	if s := containerState(t, fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s after the shutdown stop, want exited", s)
	}
	if err := w1.db.Close(); err != nil {
		t.Fatal(err)
	}

	w2 := newMaintenanceRestartWiring(t, dbPath)
	t.Cleanup(func() { _ = w2.db.Close() })
	restoreBoot(t, w2, fake, stateDir)

	if s := containerState(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after the next boot, want running", s)
	}
	if s := containerState(t, fake, "portainer"); s != "exited" {
		t.Fatalf("portainer was stopped before the shutdown and is %s after the next boot, want exited", s)
	}
}

// A persisted `array stop` survives a later shutdown: the boot after it
// must leave the containers stopped — `array start` starts them.
func TestContainerRestore_PersistedArrayStopKeepsTheContainersStopped(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "containers-userstop.db")
	stateDir := t.TempDir()
	fake := container.NewFakeProvider()
	fake.AddContainer(container.Container{ID: "a", Name: "jellyfin", State: "running"})

	w1 := newMaintenanceRestartWiring(t, dbPath)
	var mountCalls int32
	seq1 := fakeArraySequence(w1, &mountCalls)
	seq1.Services = []job.ArrayService{newContainers(fake, stateDir, nil, nil).arrayService()}
	w1.handler.SetArray(seq1)
	if _, err := w1.handler.StopArray(ctx, &apiv1.StopArrayRequest{Confirm: true}); err != nil {
		t.Fatalf("StopArray: %v", err)
	}
	if err := seq1.StopForShutdown(ctx); err != nil {
		t.Fatalf("StopForShutdown: %v", err)
	}
	if err := w1.db.Close(); err != nil {
		t.Fatal(err)
	}

	w2 := newMaintenanceRestartWiring(t, dbPath)
	t.Cleanup(func() { _ = w2.db.Close() })
	apps := restoreBoot(t, w2, fake, stateDir)

	if s := containerState(t, fake, "jellyfin"); s != "exited" {
		t.Fatalf("jellyfin is %s at a boot with the array stopped by the user, want exited", s)
	}
	if err := apps.Array.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s := containerState(t, fake, "jellyfin"); s != "running" {
		t.Fatalf("jellyfin is %s after array start, want running", s)
	}
}
