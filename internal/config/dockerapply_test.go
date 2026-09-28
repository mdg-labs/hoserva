package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestApplyDockerDataRoot_NoopAtDefault(t *testing.T) {
	g := NewGenerator(t.TempDir())
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}

	if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootDefault, dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}
	if len(dirs.Created()) != 0 {
		t.Fatalf("Created() = %v, want nothing created when dataRoot is the default", dirs.Created())
	}
	if restart.Stopped() || restart.Started() {
		t.Fatal("Stop/Start called when dataRoot is the default — nothing should have moved")
	}
	if _, err := os.Stat(filepath.Join(g.Root, "docker", "daemon.json")); !os.IsNotExist(err) {
		t.Fatalf("daemon.json exists at the default data-root, want none written")
	}
}

func TestApplyDockerDataRoot_WritesConfigAndDirectory(t *testing.T) {
	g := NewGenerator(t.TempDir())
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}
	restart.SetActive(false)

	if err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}

	created := dirs.Created()
	if len(created) != 1 || created[0] != "/mnt/cache/docker" {
		t.Fatalf("Created() = %v, want exactly [/mnt/cache/docker]", created)
	}

	raw, err := os.ReadFile(filepath.Join(g.Root, "docker", "daemon.json"))
	if err != nil {
		t.Fatalf("reading daemon.json: %v", err)
	}
	// RawBody: Docker's daemon.json is parsed as plain JSON, so it must
	// carry none of Write's usual `#`-comment header — this is the
	// property that would fail if ApplyDockerDataRoot ever called Write
	// without File.RawBody set.
	if strings.HasPrefix(string(raw), "#") {
		t.Fatalf("daemon.json = %s, want no #-comment header (must be valid JSON)", raw)
	}
	want := `{
  "data-root": "/mnt/cache/docker",
  "storage-driver": "overlay2"
}`
	if string(raw) != want {
		t.Fatalf("daemon.json = %s, want %s", raw, want)
	}

	// restart was not active, so ApplyDockerDataRoot must not have started
	// Docker itself — Q69's own drop-in is what starts it the first time.
	if restart.Stopped() || restart.Started() {
		t.Fatal("Stop/Start called on an inactive service — a data-root move must never start Docker ahead of hoserva-storage.target")
	}
}

func TestApplyDockerDataRoot_RestartsAnAlreadyRunningDocker(t *testing.T) {
	g := NewGenerator(t.TempDir())
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}
	restart.SetActive(true)

	if err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}
	if !restart.Stopped() || !restart.Started() {
		t.Fatal("an already-running Docker must be stopped then started so it picks up the new data-root")
	}
}

func TestApplyDockerDataRoot_RecoversRestartAfterPriorStopWithoutStart(t *testing.T) {
	root := t.TempDir()
	g := NewGenerator(root)
	dirs := &FakeDirMaker{}

	// First call: Docker is running, Stop succeeds, Start fails — leaving
	// daemon.json already written at dataRoot and Docker stopped.
	firstAttempt := &FakeServiceRestarter{}
	firstAttempt.SetActive(true)
	firstAttempt.SetStartErr(errors.New("simulated start failure"))
	err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, firstAttempt, 1, time.Now())
	if err == nil {
		t.Fatal("ApplyDockerDataRoot() = nil, want the simulated start failure surfaced")
	}
	if !firstAttempt.Stopped() {
		t.Fatal("Stop was not called on the first attempt")
	}

	// Retry with a fresh restarter: daemon.json is unchanged (already at
	// dataRoot) and Docker is inactive because the first attempt's Stop
	// was never followed by a successful Start. This must recover by
	// starting Docker again, not silently treat "inactive" as "nothing to
	// do."
	retry := &FakeServiceRestarter{}
	retry.SetActive(false)
	if err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, retry, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot() retry: %v", err)
	}
	if retry.Stopped() {
		t.Fatal("Stop was called on the retry — Docker was already inactive, nothing to stop")
	}
	if !retry.Started() {
		t.Fatal("Start was not called on the retry — a prior Stop without a successful Start must be recovered, not silently skipped")
	}
}

func TestApplyDockerDataRoot_ExistingUnmanagedDaemonJSONRefuses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docker", "daemon.json"), []byte(`{"log-driver":"journald"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	g := NewGenerator(root)
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}

	err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, restart, 1, time.Now())
	if !errors.Is(err, ErrExistingHostFile) {
		t.Fatalf("ApplyDockerDataRoot() error = %v, want ErrExistingHostFile — an existing, unmanaged daemon.json must never be overwritten", err)
	}
	raw, readErr := os.ReadFile(filepath.Join(root, "docker", "daemon.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != `{"log-driver":"journald"}` {
		t.Fatalf("daemon.json was modified: %s", raw)
	}
}

func TestApplyDockerDataRoot_MkdirFailurePropagates(t *testing.T) {
	g := NewGenerator(t.TempDir())
	dirs := &FakeDirMaker{}
	dirs.SetErr(errors.New("simulated mkdir failure"))
	restart := &FakeServiceRestarter{}

	err := g.ApplyDockerDataRoot(context.Background(), "/mnt/cache/docker", dirs, restart, 1, time.Now())
	if err == nil {
		t.Fatal("ApplyDockerDataRoot() = nil, want the mkdir failure surfaced")
	}
	if _, statErr := os.Stat(filepath.Join(g.Root, "docker", "daemon.json")); !os.IsNotExist(statErr) {
		t.Fatal("daemon.json was written despite the directory creation failing")
	}
}

func TestApplyDockerDataRoot_NilDirsAndRestartAreSafe(t *testing.T) {
	// A nil DirMaker/ServiceRestarter must never panic — main.go leaves
	// DockerRestart nil whenever it has nothing to wire, and this proves
	// ApplyDockerDataRoot degrades to "write the config, skip the restart"
	// rather than crashing the request that reaches it. DirMaker falls
	// back to the real OSDirMaker, so this still creates a directory —
	// under a throwaway root inside t.TempDir(), never a real host path.
	root := t.TempDir()
	dataRoot := filepath.Join(root, "cache-docker")
	g := NewGenerator(root)

	if err := g.ApplyDockerDataRoot(context.Background(), dataRoot, nil, nil, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}
	if info, err := os.Stat(dataRoot); err != nil || !info.IsDir() {
		t.Fatalf("data-root directory was not created: %v", err)
	}
}
