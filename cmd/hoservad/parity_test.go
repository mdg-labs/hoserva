package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/parity"
)

const parityCorpusDir = "../../testdata/parsers"

func readParityCorpus(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(parityCorpusDir, name))
	if err != nil {
		t.Fatalf("reading corpus fixture %s: %v", name, err)
	}
	return data
}

func writeSnapraidConf(t *testing.T, configRoot string) string {
	t.Helper()
	confPath := filepath.Join(configRoot, snapraidConfRelPath)
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		t.Fatalf("creating config root: %v", err)
	}
	if err := os.WriteFile(confPath, []byte("content /\n"), 0o600); err != nil {
		t.Fatalf("writing snapraid.conf: %v", err)
	}
	return confPath
}

func newParityTestHandler() *api.Handler {
	return &api.Handler{}
}

func attachDaemonParity(t *testing.T, configRoot, stateDir string, runner parity.Runner, h *api.Handler) {
	t.Helper()
	engine := newSnapraidEngine(configRoot, stateDir, runner)
	if engine == nil {
		t.Fatal("newSnapraidEngine returned nil with snapraid.conf present — GET /parity would 501")
	}
	h.Parity = engine
	h.ParityGuard = engine.Guard
}

// parityScriptedRunner is a fake parity.Runner for daemon-construction
// tests: it plays back canned snapraid log bodies without a real binary.
type parityScriptedRunner struct {
	t      *testing.T
	script []parityScriptedResult
	i      int
}

type parityScriptedResult struct {
	logBody string
	err     error
}

func (r *parityScriptedRunner) Start(ctx context.Context, name string, args ...string) (parity.Process, error) {
	r.t.Helper()
	if r.i >= len(r.script) {
		r.t.Fatalf("unexpected extra snapraid invocation: %v", args)
	}
	res := r.script[r.i]
	r.i++

	for i, a := range args {
		if a == "-l" && i+1 < len(args) && res.logBody != "" {
			if err := os.WriteFile(args[i+1], []byte(res.logBody), 0o644); err != nil {
				r.t.Fatalf("writing scripted log: %v", err)
			}
		}
	}

	ch := make(chan string)
	close(ch)
	return &parityScriptedProcess{lines: ch, err: res.err}, nil
}

type parityScriptedProcess struct {
	lines chan string
	err   error
}

func (p *parityScriptedProcess) Lines() <-chan string { return p.lines }
func (p *parityScriptedProcess) Wait() error          { return p.err }

// TestNewSnapraidEngine_NilWhenNoConf is the empty-path half of #191:
// with no generated snapraid.conf, Parity stays nil so GET /parity and
// POST /parity/diff 501 rather than invoking snapraid.
func TestNewSnapraidEngine_NilWhenNoConf(t *testing.T) {
	configRoot := t.TempDir()
	stateDir := t.TempDir()
	h := newParityTestHandler()

	if engine := newSnapraidEngine(configRoot, stateDir, nil); engine != nil {
		t.Fatal("newSnapraidEngine returned non-nil without snapraid.conf")
	}
	if h.Parity != nil {
		t.Fatal("Handler.Parity is set with no snapraid.conf — parity operations must 501, not invoke snapraid")
	}

	ctx := context.Background()
	_, err := h.GetParity(ctx)
	status := handlerAPIError(t, h, err)
	if status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("GetParity = %+v, want 501 not_configured", status)
	}
	_, err = h.RunParityDiff(ctx)
	status = handlerAPIError(t, h, err)
	if status.StatusCode != 501 || status.Response.Code != "not_configured" {
		t.Fatalf("RunParityDiff = %+v, want 501 not_configured", status)
	}
}

// TestNewSnapraidEngine_WiresGetParityWhenConfExists is the live-config
// half of #191: a generated snapraid.conf must leave Handler.Parity and
// ParityGuard set to one SnapraidEngine so GET /parity runs Status
// instead of returning 501.
func TestNewSnapraidEngine_WiresGetParityWhenConfExists(t *testing.T) {
	configRoot := t.TempDir()
	stateDir := t.TempDir()
	confPath := writeSnapraidConf(t, configRoot)

	runner := &parityScriptedRunner{t: t, script: []parityScriptedResult{
		{logBody: string(readParityCorpus(t, "snapraid_status_clean.log"))},
	}}
	h := newParityTestHandler()
	attachDaemonParity(t, configRoot, stateDir, runner, h)

	engine, ok := h.Parity.(*parity.SnapraidEngine)
	if !ok {
		t.Fatalf("Parity is %T, want *parity.SnapraidEngine", h.Parity)
	}
	if engine.ConfPath != confPath {
		t.Fatalf("ConfPath = %q, want %q", engine.ConfPath, confPath)
	}
	wantLogDir := filepath.Join(stateDir, "snapraid")
	if engine.LogDir != wantLogDir {
		t.Fatalf("LogDir = %q, want %q", engine.LogDir, wantLogDir)
	}

	got, err := h.GetParity(context.Background())
	if err != nil {
		t.Fatalf("GetParity: %v", err)
	}
	if got.Freshness != apiv1.ParityFreshnessGreen {
		t.Fatalf("freshness = %q, want green", got.Freshness)
	}
	if got.Guard.Set {
		t.Fatal("guard present before any run-diff, want omitted")
	}
}

// TestNewSnapraidEngine_WiresRunDiffWhenConfExists confirms POST
// /parity/diff uses the same engine and reports guard state from the
// engine's own Guard — not a second threshold implementation in hoservad.
func TestNewSnapraidEngine_WiresRunDiffWhenConfExists(t *testing.T) {
	configRoot := t.TempDir()
	stateDir := t.TempDir()
	writeSnapraidConf(t, configRoot)

	runner := &parityScriptedRunner{t: t, script: []parityScriptedResult{
		{logBody: string(readParityCorpus(t, "snapraid_status_new_array.log"))},
		{logBody: string(readParityCorpus(t, "snapraid_diff_mixed.log")), err: &parityExitError{code: 2}},
	}}
	h := newParityTestHandler()
	attachDaemonParity(t, configRoot, stateDir, runner, h)

	engine := h.Parity.(*parity.SnapraidEngine)
	if h.ParityGuard != engine.Guard {
		t.Fatal("ParityGuard is not the same Guard the engine uses — thresholds would be duplicated")
	}

	got, err := h.RunParityDiff(context.Background())
	if err != nil {
		t.Fatalf("RunParityDiff: %v", err)
	}
	if len(got.Groups) != 6 {
		t.Fatalf("groups = %d, want 6 doc 02 categories", len(got.Groups))
	}
	if got.Guard.WouldBlock && len(got.Guard.Triggers) == 0 {
		t.Fatalf("guard = %+v, want triggers when wouldBlock is true", got.Guard)
	}
}

type parityExitError struct{ code int }

func (e *parityExitError) Error() string { return "exit status" }
func (e *parityExitError) ExitCode() int { return e.code }

var _ interface{ ExitCode() int } = (*parityExitError)(nil)

// TestNewSnapraidEngine_ConstructionNilWithoutConf confirms
// newSnapraidEngine itself returns nil when the config file is absent.
func TestNewSnapraidEngine_ConstructionNilWithoutConf(t *testing.T) {
	if got := newSnapraidEngine(t.TempDir(), t.TempDir(), nil); got != nil {
		t.Fatalf("newSnapraidEngine = %v, want nil without snapraid.conf", got)
	}
}

// TestNewSnapraidEngine_ConstructionReturnsEngineWithConf confirms
// newSnapraidEngine returns a configured engine when snapraid.conf exists.
func TestNewSnapraidEngine_ConstructionReturnsEngineWithConf(t *testing.T) {
	configRoot := t.TempDir()
	stateDir := t.TempDir()
	confPath := writeSnapraidConf(t, configRoot)

	got := newSnapraidEngine(configRoot, stateDir, nil)
	if got == nil {
		t.Fatal("newSnapraidEngine returned nil with snapraid.conf present")
	}
	if got.ConfPath != confPath {
		t.Fatalf("ConfPath = %q, want %q", got.ConfPath, confPath)
	}
	if got.LogDir != filepath.Join(stateDir, "snapraid") {
		t.Fatalf("LogDir = %q, want %q", got.LogDir, filepath.Join(stateDir, "snapraid"))
	}
	if got.Runner != nil {
		t.Fatal("Runner should be nil in production wiring — CommandRunner is the default")
	}
}

// Ensure parityScriptedRunner satisfies parity.Runner at compile time.
var _ parity.Runner = (*parityScriptedRunner)(nil)

// Ensure parityScriptedProcess satisfies parity.Process at compile time.
var _ parity.Process = (*parityScriptedProcess)(nil)
