package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/pool"
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

	if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}

	created := dirs.Created()
	if len(created) != 1 || created[0] != DockerDataRootCache {
		t.Fatalf("Created() = %v, want exactly [/mnt/cache/.docker]", created)
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
  "data-root": "/mnt/cache/.docker",
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

	if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot: %v", err)
	}
	if !restart.Stopped() || !restart.Started() {
		t.Fatal("an already-running Docker must be stopped then started so it picks up the new data-root")
	}
}

func TestApplyDockerDataRoot_NeverStartsDockerAcrossRepeatedInactiveCalls(t *testing.T) {
	// A fresh install: Docker hasn't been started yet (hoserva-storage.target's
	// own drop-in is what starts it the first time, Q69) and ApplyHostConfig
	// is called more than once — e.g. a replayed or duplicate request — before
	// that happens. Every call sees daemon.json already at dataRoot (from the
	// previous call) and Docker inactive: identical, from Active() and
	// daemon.json content alone, to a prior attempt's Stop having outlived a
	// failed Start. Only the restart marker tells them apart, and no call
	// ever issued Stop here, so none of them may start Docker.
	root := t.TempDir()
	g := NewGenerator(root)
	dirs := &FakeDirMaker{}

	for i := 0; i < 2; i++ {
		restart := &FakeServiceRestarter{}
		restart.SetActive(false)
		if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now()); err != nil {
			t.Fatalf("ApplyDockerDataRoot() call %d: %v", i, err)
		}
		if restart.Stopped() || restart.Started() {
			t.Fatalf("call %d: Stop/Start called on a Docker that was never running — must not start ahead of hoserva-storage.target", i)
		}
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
	err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, firstAttempt, 1, time.Now())
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
	if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, retry, 1, time.Now()); err != nil {
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

	err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now())
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

	err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now())
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

func TestDockerDataRootCacheIsNeverAShareBranch(t *testing.T) {
	names := []string{"docker", "Docker", "docker-data", "appdata", "media", ".docker", "..", "."}
	for _, name := range names {
		if pool.ValidateShareName(name) != nil {
			continue
		}
		if filepath.Join(filepath.Dir(DockerDataRootCache), name) == DockerDataRootCache {
			t.Fatalf("share name %q is accepted and its cache branch is Docker's data-root %s", name, DockerDataRootCache)
		}
	}
	if err := pool.ValidateShareName(filepath.Base(DockerDataRootCache)); err == nil {
		t.Fatalf("share name %q is accepted", filepath.Base(DockerDataRootCache))
	}
}

func TestApplyDockerDataRoot_RefusesADirectoryThatHoldsFiles(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "cache", "docker")
	if err := os.MkdirAll(filepath.Join(dataRoot, "overlay2"), 0o2775); err != nil {
		t.Fatal(err)
	}
	g := NewGenerator(root)
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}
	restart.SetActive(true)

	err := g.ApplyDockerDataRoot(context.Background(), dataRoot, dirs, restart, 1, time.Now())
	if !errors.Is(err, ErrDockerDataRootInUse) {
		t.Fatalf("ApplyDockerDataRoot() error = %v, want ErrDockerDataRootInUse", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "docker", "daemon.json")); !os.IsNotExist(statErr) {
		t.Fatal("daemon.json was written for a directory that already holds files")
	}
	if len(dirs.Created()) != 0 || restart.Stopped() {
		t.Fatalf("Created() = %v, Stopped() = %v, want nothing touched", dirs.Created(), restart.Stopped())
	}
	if err := g.CanApplyDockerDataRoot(context.Background(), dataRoot); !errors.Is(err, ErrDockerDataRootInUse) {
		t.Fatalf("CanApplyDockerDataRoot() error = %v, want ErrDockerDataRootInUse", err)
	}
}

func TestApplyDockerDataRoot_AcceptsAnEmptyExistingDirectory(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "cache", ".docker")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	g := NewGenerator(root)
	if err := g.ApplyDockerDataRoot(context.Background(), dataRoot, &FakeDirMaker{}, nil, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot() = %v, want an empty directory accepted", err)
	}
}

func TestApplyDockerDataRoot_ReapplyAfterCompletedMoveStaysANoop(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "cache", ".docker")
	g := NewGenerator(root)
	if err := g.ApplyDockerDataRoot(context.Background(), dataRoot, nil, nil, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataRoot, "containers"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyDockerDataRoot(context.Background(), dataRoot, nil, nil, 1, time.Now()); err != nil {
		t.Fatalf("re-applying a completed move = %v, want a no-op", err)
	}
}

func TestApplyDockerDataRoot_UnreadableDirectoryRefuses(t *testing.T) {
	root := t.TempDir()
	notADir := filepath.Join(root, "cache-docker")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := NewGenerator(root)
	dirs := &FakeDirMaker{}
	if err := g.ApplyDockerDataRoot(context.Background(), notADir, dirs, nil, 1, time.Now()); err == nil {
		t.Fatal("ApplyDockerDataRoot() = nil, want a refusal when the path cannot be read as a directory")
	}
	if _, statErr := os.Stat(filepath.Join(root, "docker", "daemon.json")); !os.IsNotExist(statErr) {
		t.Fatal("daemon.json was written")
	}
}

func writeLegacyDaemonJSON(t *testing.T, g *Generator) (path string, body []byte) {
	t.Helper()
	path = filepath.Join(g.Root, "docker", "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body = []byte(`{"data-root": "` + DockerDataRootCacheLegacy + `", "storage-driver": "overlay2"}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, body
}

func TestApplyDockerDataRoot_KeepsALegacyCacheDataRoot(t *testing.T) {
	g := NewGenerator(t.TempDir())
	path, body := writeLegacyDaemonJSON(t, g)
	dirs := &FakeDirMaker{}
	restart := &FakeServiceRestarter{}
	restart.SetActive(true)

	if err := g.CanApplyDockerDataRoot(context.Background(), DockerDataRootCache); err != nil {
		t.Fatalf("CanApplyDockerDataRoot() = %v, want the legacy install accepted", err)
	}
	if err := g.ApplyDockerDataRoot(context.Background(), DockerDataRootCache, dirs, restart, 1, time.Now()); err != nil {
		t.Fatalf("ApplyDockerDataRoot() = %v, want a no-op", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(body) {
		t.Fatalf("daemon.json = %q (%v), want it untouched", got, err)
	}
	if len(dirs.Created()) != 0 || restart.Stopped() || restart.Started() {
		t.Fatalf("Created() = %v, Stopped() = %v, Started() = %v, want nothing touched", dirs.Created(), restart.Stopped(), restart.Started())
	}
	root, err := g.EffectiveDockerDataRoot(DockerDataRootCache)
	if err != nil || root != DockerDataRootCacheLegacy {
		t.Fatalf("EffectiveDockerDataRoot() = %q, %v, want %s", root, err, DockerDataRootCacheLegacy)
	}
}

func TestEffectiveDockerDataRoot_OtherwiseUnchanged(t *testing.T) {
	g := NewGenerator(t.TempDir())
	for _, in := range []string{DockerDataRootDefault, DockerDataRootCache} {
		got, err := g.EffectiveDockerDataRoot(in)
		if err != nil || got != in {
			t.Fatalf("EffectiveDockerDataRoot(%q) = %q, %v, want it unchanged", in, got, err)
		}
	}
}
