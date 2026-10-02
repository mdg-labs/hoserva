package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mdg-labs/hoserva/internal/auth"
	"github.com/mdg-labs/hoserva/internal/store"
)

type memStackStore struct {
	rows      map[string]store.Stack
	insertErr error
	deleteErr error
	updateErr error
	envErr    error
	calls     int
}

func newMemStackStore() *memStackStore { return &memStackStore{rows: map[string]store.Stack{}} }

func (m *memStackStore) Insert(_ context.Context, st store.Stack) error {
	m.calls++
	if m.insertErr != nil {
		return m.insertErr
	}
	if _, ok := m.rows[st.Name]; ok {
		return store.ErrStackExists
	}
	m.rows[st.Name] = st
	return nil
}

func (m *memStackStore) Get(_ context.Context, name string) (store.Stack, error) {
	m.calls++
	st, ok := m.rows[name]
	if !ok {
		return store.Stack{}, store.ErrStackNotFound
	}
	return st, nil
}

func (m *memStackStore) List(_ context.Context) ([]store.Stack, error) {
	m.calls++
	var out []store.Stack
	for _, st := range m.rows {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memStackStore) UpdateCompose(ctx context.Context, name, compose string, manuallyEdited bool) error {
	m.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.updateErr != nil {
		return m.updateErr
	}
	st, ok := m.rows[name]
	if !ok {
		return store.ErrStackNotFound
	}
	st.Compose, st.ManuallyEdited = compose, manuallyEdited
	m.rows[name] = st
	return nil
}

func (m *memStackStore) UpdateEnv(ctx context.Context, name string, sealedEnv []byte) error {
	m.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.envErr != nil {
		return m.envErr
	}
	st, ok := m.rows[name]
	if !ok {
		return store.ErrStackNotFound
	}
	st.SealedEnv = sealedEnv
	m.rows[name] = st
	return nil
}

func (m *memStackStore) Delete(ctx context.Context, name string) error {
	m.calls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.deleteErr != nil {
		return m.deleteErr
	}
	if _, ok := m.rows[name]; !ok {
		return store.ErrStackNotFound
	}
	delete(m.rows, name)
	return nil
}

type xorCipher struct{}

func (xorCipher) Encrypt(p []byte) ([]byte, error) { return xorAll(p), nil }
func (xorCipher) Decrypt(c []byte) ([]byte, error) { return xorAll(c), nil }

// unopenableCipher cannot open what was sealed, as under another machine key.
type unopenableCipher struct{ xorCipher }

func (unopenableCipher) Decrypt([]byte) ([]byte, error) { return nil, errors.New("wrong key") }

func xorAll(in []byte) []byte {
	out := make([]byte, len(in))
	for i, b := range in {
		out[i] = b ^ 0x5a
	}
	return out
}

type stackRig struct {
	svc    *StackService
	store  *memStackStore
	runner *FakeRunner
	fake   *FakeProvider
	root   string
	parent string
	// appdata is the appdata root the service may delete under.
	appdata string
	sim     *composeSim
}

// composeSim is the Engine side of `compose down` for the fake: it deletes
// from the FakeProvider what compose would. A file-based down removes the
// project's containers that were started from the stack's own directory; one
// with no --file, which works from the project label alone, removes every
// container of the project. Containers named in keep survive either (an
// orphan that is not in the compose file).
type composeSim struct {
	Runner
	fake *FakeProvider
	root string
	keep map[string]bool
}

func (c *composeSim) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	out, err := c.Runner.Run(ctx, env, name, args...)
	if err != nil {
		return out, err
	}
	var project string
	var down, hasFile bool
	for i, a := range args {
		switch a {
		case "down":
			down = true
		case "--file":
			hasFile = true
		case "--project-name":
			project = args[i+1]
		}
	}
	if !down {
		return out, nil
	}
	all, err := c.fake.List(ctx)
	if err != nil {
		return out, nil
	}
	for _, ct := range all {
		if ct.Labels[composeProjectLabel] != project || c.keep[ct.ID] {
			continue
		}
		if hasFile && ct.Labels[composeWorkingDirLabel] != filepath.Join(c.root, project) {
			continue
		}
		c.fake.RemoveContainer(ct.ID)
	}
	return out, nil
}

func newStackRig(t *testing.T) *stackRig {
	t.Helper()
	parent := t.TempDir()
	root := filepath.Join(parent, "stacks")
	appdata := filepath.Join(parent, "cache", "appdata")
	st := newMemStackStore()
	runner := NewFakeRunner()
	fake := NewFakeProvider()
	sim := &composeSim{Runner: runner, fake: fake, root: root, keep: map[string]bool{}}
	return &stackRig{
		sim: sim,
		svc: &StackService{
			Store:               st,
			Cipher:              xorCipher{},
			Runner:              sim,
			Root:                root,
			Now:                 func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) },
			RequireArrayRunning: func() error { return nil },
			Provider:            fake,
			AppdataRoots:        func(context.Context) ([]string, error) { return []string{appdata}, nil },
		},
		store:   st,
		runner:  runner,
		fake:    fake,
		root:    root,
		parent:  parent,
		appdata: appdata,
	}
}

// mkAppdata makes <appdata root>/<rel> with a file in it and returns the
// directory's resolved path, which is what a deletion reports.
func (r *stackRig) mkAppdata(t *testing.T, rel string) string {
	t.Helper()
	dir := filepath.Join(r.appdata, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// addContainer registers a container of the Compose project project that
// binds the given host directories.
func (r *stackRig) addContainer(project, name string, sources ...string) {
	r.addContainerFrom(project, filepath.Join(r.root, project), name, sources...)
}

// addContainerFrom is addContainer for a container Compose started from
// workingDir: the stack's own directory for a container of the stack, any
// other for a project of the same name that something else runs.
func (r *stackRig) addContainerFrom(project, workingDir, name string, sources ...string) {
	c := Container{ID: "id-" + name, Name: name, State: "running"}
	if project != "" {
		c.Labels = map[string]string{
			composeProjectLabel:     project,
			composeWorkingDirLabel:  workingDir,
			composeConfigFilesLabel: filepath.Join(workingDir, "compose.yml"),
		}
	}
	for _, s := range sources {
		c.Mounts = append(c.Mounts, Mount{Source: s, Destination: "/mnt/" + filepath.Base(s), ReadWrite: true})
	}
	r.fake.AddContainer(c)
}

func (r *stackRig) downArgv(name string, sub ...string) []string {
	return r.svc.composeArgs(name, append([]string{"down"}, sub...)...)
}

func (r *stackRig) composeCalls(verb string) []RunCall {
	var out []RunCall
	for _, c := range r.runner.Calls() {
		for _, a := range c.Args {
			if a == verb {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (r *stackRig) create(t *testing.T, name string) {
	t.Helper()
	if _, err := r.svc.Create(context.Background(), NewStack{
		Name:             name,
		Compose:          "services:\n  web:\n    image: nginx:1.27\n",
		Env:              "TOKEN=s3cret\n",
		TemplateSource:   "hoserva-catalog",
		TemplateID:       "nginx",
		TemplateRevision: "3",
	}); err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
}

func (r *stackRig) composeArgv(name string, sub ...string) []string {
	return r.svc.composeArgs(name, sub...)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func TestStackCreate_GeneratesFilesFromRowAndValidatesWithComposeConfig(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")

	dir := filepath.Join(r.root, "nginx")
	if got := string(readFile(t, filepath.Join(dir, "docker-compose.yml"))); got != "services:\n  web:\n    image: nginx:1.27\n" {
		t.Fatalf("docker-compose.yml = %q", got)
	}
	if got := string(readFile(t, filepath.Join(dir, ".env"))); got != "TOKEN=s3cret\n" {
		t.Fatalf(".env = %q", got)
	}
	wantMeta := "{\n  \"name\": \"nginx\",\n  \"template\": {\n    \"source\": \"hoserva-catalog\",\n    \"id\": \"nginx\",\n    \"revision\": \"3\"\n  },\n  \"installedAt\": \"2026-09-30T12:00:00Z\"\n}\n"
	if got := string(readFile(t, filepath.Join(dir, "meta.json"))); got != wantMeta {
		t.Fatalf("meta.json = %q, want %q", got, wantMeta)
	}
	if info, _ := os.Stat(filepath.Join(dir, ".env")); info.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode = %v, want 0600", info.Mode().Perm())
	}

	row := r.store.rows["nginx"]
	if bytes.Contains(row.SealedEnv, []byte("s3cret")) {
		t.Fatalf("the stored .env is not sealed: %q", row.SealedEnv)
	}

	calls := r.runner.Calls()
	if len(calls) != 1 || calls[0].Name != "docker" {
		t.Fatalf("Calls() = %v, want one docker call", calls)
	}
	want := r.composeArgv("nginx", "config", "--quiet")
	if strings.Join(calls[0].Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", calls[0].Args, want)
	}
}

// TestStackRegenerate_SameRowIsByteIdentical proves the row, not a file, is
// authoritative: a file changed or deleted on disk is rewritten to exactly
// the bytes a first generation produced.
func TestStackRegenerate_SameRowIsByteIdentical(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	row := r.store.rows["nginx"]

	names := []string{"docker-compose.yml", ".env", "meta.json"}
	first := map[string][]byte{}
	for _, n := range names {
		first[n] = readFile(t, filepath.Join(dir, n))
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := r.svc.writeFiles(row, dir, false, false); err != nil {
			t.Fatalf("writeFiles #%d: %v", i+1, err)
		}
		for _, n := range names {
			if got := readFile(t, filepath.Join(dir, n)); !bytes.Equal(got, first[n]) {
				t.Fatalf("regeneration #%d changed %s: %q != %q", i+1, n, got, first[n])
			}
		}
	}
}

func TestStackCreate_RefusesExistingDirectoryAndTouchesNothingInIt(t *testing.T) {
	r := newStackRig(t)
	dir := filepath.Join(r.root, "nginx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "docker-compose.yml")
	if err := os.WriteFile(keep, []byte("hand written"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services: {}\n"})
	if !errors.Is(err, ErrStackDirExists) {
		t.Fatalf("Create() error = %v, want ErrStackDirExists", err)
	}
	if got := string(readFile(t, keep)); got != "hand written" {
		t.Fatalf("an existing stack file was changed: %q", got)
	}
	if len(r.store.rows) != 0 || len(r.runner.Calls()) != 0 {
		t.Fatalf("a refused create wrote a row (%d) or ran docker (%v)", len(r.store.rows), r.runner.Calls())
	}
}

func TestStackCreate_RefusesExistingStack(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	_, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services: {}\n"})
	if !errors.Is(err, ErrStackExists) {
		t.Fatalf("Create() error = %v, want ErrStackExists", err)
	}
}

func TestStackCreate_RejectedComposeLeavesNoRowAndNoDirectory(t *testing.T) {
	r := newStackRig(t)
	r.runner.Script("docker", r.composeArgv("bad", "config", "--quiet"), nil,
		&exec.ExitError{})

	_, err := r.svc.Create(context.Background(), NewStack{Name: "bad", Compose: "services: [\n"})
	if !errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Create() error = %v, want ErrInvalidStack", err)
	}
	if len(r.store.rows) != 0 {
		t.Fatalf("a rejected compose file left a row: %v", r.store.rows)
	}
	if exists(t, filepath.Join(r.root, "bad")) {
		t.Fatal("a rejected compose file left its directory behind")
	}
}

func TestStackCreate_PluginMissingIsReported(t *testing.T) {
	r := newStackRig(t)
	r.runner.Script("docker", r.composeArgv("x", "config", "--quiet"), nil,
		errors.New("docker: unknown command: docker compose"))
	_, err := r.svc.Create(context.Background(), NewStack{Name: "x", Compose: "services: {}\n"})
	if !errors.Is(err, ErrComposeUnavailable) {
		t.Fatalf("Create() error = %v, want ErrComposeUnavailable", err)
	}
	if exists(t, filepath.Join(r.root, "x")) || len(r.store.rows) != 0 {
		t.Fatal("a failed create left a directory or a row")
	}
}

func TestStackCreate_InsertFailureWritesNoFiles(t *testing.T) {
	r := newStackRig(t)
	r.store.insertErr = errors.New("disk I/O error")
	if _, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services: {}\n"}); err == nil {
		t.Fatal("Create() succeeded although the row could not be written")
	}
	if exists(t, filepath.Join(r.root, "nginx")) {
		t.Fatal("the directory of a stack with no row was left behind")
	}
}

// TestStackCreate_AdoptsAnExistingDirectoryWithoutACompose is the reinstall:
// a stack removed without its appdata leaves its ./data behind, and creating
// the stack again must neither fail nor touch that data.
func TestStackCreate_AdoptsAnExistingDirectoryWithoutACompose(t *testing.T) {
	r := newStackRig(t)
	dir := filepath.Join(r.root, "nginx")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data", "library.db")
	if err := os.WriteFile(data, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("LEFTOVER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r.create(t, "nginx")
	if got := string(readFile(t, data)); got != "precious" {
		t.Fatalf("an adopted directory's own file was changed: %q", got)
	}
	if got := string(readFile(t, filepath.Join(dir, ".env"))); got != "TOKEN=s3cret\n" {
		t.Fatalf(".env = %q, want the new stack's", got)
	}
	if !exists(t, filepath.Join(dir, "docker-compose.yml")) || !exists(t, filepath.Join(dir, "meta.json")) {
		t.Fatal("the stack's files were not generated into the adopted directory")
	}
}

// A create that fails in an adopted directory deletes only what it wrote.
func TestStackCreate_FailureInAnAdoptedDirectoryKeepsItsOwnFilesAndLeavesNoRow(t *testing.T) {
	r := newStackRig(t)
	dir := filepath.Join(r.root, "nginx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data.bin")
	if err := os.WriteFile(data, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.runner.Script("docker", r.composeArgv("nginx", "config", "--quiet"), nil, &exec.ExitError{})

	if _, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services: [\n"}); !errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Create() error = %v, want ErrInvalidStack", err)
	}
	if got := string(readFile(t, data)); got != "precious" {
		t.Fatalf("a failed create changed the directory's own file: %q", got)
	}
	for _, n := range []string{"docker-compose.yml", ".env", "meta.json"} {
		if exists(t, filepath.Join(dir, n)) {
			t.Fatalf("a failed create left %s behind", n)
		}
	}
	if len(r.store.rows) != 0 {
		t.Fatalf("a failed create left a row: %v", r.store.rows)
	}
}

// TestStackCreate_ACreateThatDiedHalfWayCanBeRemovedAndRetried covers the
// daemon dying between the row and the last file: the stack is listed, so
// Remove takes it away (regenerating whatever is missing), and the name is
// free again.
func TestStackCreate_ACreateThatDiedHalfWayCanBeRemovedAndRetried(t *testing.T) {
	r := newStackRig(t)
	ctx := context.Background()
	sealed, _ := r.svc.Cipher.Encrypt([]byte("TOKEN=s3cret\n"))
	r.store.rows["nginx"] = store.Stack{Name: "nginx", Compose: "services: {}\n", SealedEnv: sealed}
	dir := filepath.Join(r.root, "nginx")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.svc.Create(ctx, NewStack{Name: "nginx", Compose: "services: {}\n"}); !errors.Is(err, ErrStackExists) {
		t.Fatalf("Create() over the half-made stack = %v, want ErrStackExists", err)
	}
	if _, err := r.svc.Remove(ctx, "nginx", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	r.create(t, "nginx")
}

// A create whose request context is cancelled after the row was stored
// still removes the row it is undoing.
func TestStackCreate_FailureCompensationSurvivesACancelledRequest(t *testing.T) {
	r := newStackRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	r.runner.Script("docker", r.composeArgv("nginx", "config", "--quiet"), nil, &exec.ExitError{})
	r.svc.Runner = cancelOnRun{Runner: r.runner, cancel: cancel}

	if _, err := r.svc.Create(ctx, NewStack{Name: "nginx", Compose: "services: [\n"}); !errors.Is(err, ErrInvalidStack) {
		t.Fatalf("Create() error = %v, want ErrInvalidStack", err)
	}
	if len(r.store.rows) != 0 {
		t.Fatalf("a cancelled create left a row: %v", r.store.rows)
	}
}

type cancelOnRun struct {
	Runner
	cancel context.CancelFunc
}

func (c cancelOnRun) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	out, err := c.Runner.Run(ctx, env, name, args...)
	c.cancel()
	return out, err
}

var unsafeStackNames = []string{
	"", "..", ".", "../x", "a/b", "/etc", "/", "-x", "A", "a b", "x\x00y",
	"a..b/../../c", strings.Repeat("a", 64), "x\n", ".hidden",
}

// TestStack_UnsafeNameIsRefusedBeforeAnyFilesystemOrStoreAccess is the
// data-loss scenario for Remove: a name that resolves outside
// <root>/<name> must never reach a path operation.
func TestStack_UnsafeNameIsRefusedBeforeAnyFilesystemOrStoreAccess(t *testing.T) {
	r := newStackRig(t)
	victim := filepath.Join(r.parent, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "data"), []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.root, 0o755); err != nil {
		t.Fatal(err)
	}

	names := append([]string{"../victim", victim}, unsafeStackNames...)
	for _, name := range names {
		ctx := context.Background()
		if _, err := r.svc.Remove(ctx, name, true); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("Remove(%q) error = %v, want ErrInvalidStackName", name, err)
		}
		if _, err := r.svc.Create(ctx, NewStack{Name: name, Compose: "services: {}\n"}); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidStackName", name, err)
		}
		if err := r.svc.Up(ctx, name); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("Up(%q) error = %v, want ErrInvalidStackName", name, err)
		}
		if _, err := r.svc.Get(ctx, name); !errors.Is(err, ErrInvalidStackName) {
			t.Errorf("Get(%q) error = %v, want ErrInvalidStackName", name, err)
		}
	}
	if r.store.calls != 0 || len(r.runner.Calls()) != 0 {
		t.Fatalf("an invalid name reached the store (%d calls) or docker (%v)", r.store.calls, r.runner.Calls())
	}
	if got := string(readFile(t, filepath.Join(victim, "data"))); got != "precious" {
		t.Fatalf("a path outside the stacks directory was changed: %q", got)
	}
}

func TestStackRemove_PlainRemoveDeletesOnlyTheGeneratedFilesAndKeepsEveryOtherFile(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	data := filepath.Join(dir, "data", "library.db")
	if err := os.MkdirAll(filepath.Dir(data), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	appdata := r.mkAppdata(t, "nginx")
	r.addContainer("nginx", "nginx-web-1", appdata)
	// A plain remove needs no appdata root.
	r.svc.AppdataRoots = nil

	res, err := r.svc.Remove(context.Background(), "nginx", false)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(res.DeletedPaths) != 0 {
		t.Fatalf("DeletedPaths = %v, want none", res.DeletedPaths)
	}
	for _, n := range []string{"docker-compose.yml", ".env", "meta.json"} {
		if exists(t, filepath.Join(dir, n)) {
			t.Fatalf("%s was kept after the stack was removed", n)
		}
	}
	if got := string(readFile(t, data)); got != "precious" {
		t.Fatalf("a file of the stack directory that is not generated was changed: %q", got)
	}
	if !exists(t, filepath.Join(appdata, "data")) {
		t.Fatal("appdata was deleted although appdata deletion was not chosen")
	}
	if _, ok := r.store.rows["nginx"]; ok {
		t.Fatal("the row was kept")
	}
	calls := r.runner.Calls()
	if last := calls[len(calls)-1]; strings.Join(last.Args, "\x00") != strings.Join(r.downArgv("nginx"), "\x00") {
		t.Fatalf("last argv = %q, want %q", last.Args, r.downArgv("nginx"))
	}
	for _, c := range calls {
		for _, a := range c.Args {
			if a == "-v" || a == "--volumes" {
				t.Fatalf("down was run with %q, which deletes named volumes: %v", a, c.Args)
			}
		}
	}
}

// TestStackRemove_ThenCreateAgainReusesTheName is the remove-and-reinstall
// flow: a plain remove must leave the name usable, with or without files of
// the user's own in the directory.
func TestStackRemove_ThenCreateAgainReusesTheName(t *testing.T) {
	r := newStackRig(t)
	ctx := context.Background()
	r.create(t, "plex")
	if _, err := r.svc.Remove(ctx, "plex", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if exists(t, filepath.Join(r.root, "plex")) {
		t.Fatal("the directory of a removed stack that holds nothing else was kept")
	}
	r.create(t, "plex")

	data := filepath.Join(r.root, "plex", "data", "library.db")
	if err := os.MkdirAll(filepath.Dir(data), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Remove(ctx, "plex", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	r.create(t, "plex")
	if got := string(readFile(t, data)); got != "precious" {
		t.Fatalf("reinstalling changed the stack directory's own file: %q", got)
	}
}

func TestStackRemove_DeleteAppdataDeletesTheStacksAppdataAndItsDirectory(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "plex")
	r.create(t, "plex2")
	dir := filepath.Join(r.root, "plex")
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := r.mkAppdata(t, "plex/config")
	cache := r.mkAppdata(t, "plex/cache")
	otherApp := r.mkAppdata(t, "other")
	neighbour := r.mkAppdata(t, "plex2")
	media := filepath.Join(r.parent, "pool", "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "movie.mkv"), []byte("movie"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.addContainer("plex", "plex-web-1", config, media)
	r.addContainer("plex", "plex-worker-1", cache, config)
	r.addContainer("plex2", "plex2-web-1", neighbour)
	r.addContainer("", "unmanaged", otherApp)
	outside := filepath.Join(r.parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.svc.Remove(context.Background(), "plex", true)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	want := map[string]bool{config: true, cache: true, dir: true}
	if len(res.DeletedPaths) != len(want) {
		t.Fatalf("DeletedPaths = %v, want %v", res.DeletedPaths, want)
	}
	for _, p := range res.DeletedPaths {
		if !want[p] {
			t.Fatalf("DeletedPaths = %v, want %v", res.DeletedPaths, want)
		}
		if exists(t, p) {
			t.Fatalf("%s was reported deleted but exists", p)
		}
	}
	for _, p := range []string{filepath.Join(media, "movie.mkv"), filepath.Join(otherApp, "data"), filepath.Join(neighbour, "data"), outside, r.appdata,
		filepath.Join(r.root, "plex2", "docker-compose.yml")} {
		if !exists(t, p) {
			t.Fatalf("%s was deleted, it is not the stack's appdata", p)
		}
	}
	if _, ok := r.store.rows["plex"]; ok {
		t.Fatal("the row was kept")
	}
	if _, ok := r.store.rows["plex2"]; !ok {
		t.Fatal("another stack's row was deleted")
	}
	if down := r.composeCalls("down"); len(down) != 1 || strings.Join(down[0].Args, "\x00") != strings.Join(r.downArgv("plex", "--volumes"), "\x00") {
		t.Fatalf("down calls = %v, want one %q", down, r.downArgv("plex", "--volumes"))
	}
}

// A directory another container uses is that container's data: the remove is
// refused before anything is taken down or deleted.
func TestStackRemove_AppdataSharedWithAnotherContainerIsRefusedBeforeDown(t *testing.T) {
	for name, otherProject := range map[string]string{"unmanaged container": "", "another stack": "other"} {
		t.Run(name, func(t *testing.T) {
			r := newStackRig(t)
			r.create(t, "plex")
			config := r.mkAppdata(t, "plex/config")
			r.addContainer("plex", "plex-web-1", config)
			r.addContainer(otherProject, "reader", filepath.Join(config, "sub"))
			before := len(r.runner.Calls())

			_, err := r.svc.Remove(context.Background(), "plex", true)
			if !errors.Is(err, ErrAppdataShared) {
				t.Fatalf("Remove() error = %v, want ErrAppdataShared", err)
			}
			if len(r.runner.Calls()) != before {
				t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
			}
			if !exists(t, filepath.Join(config, "data")) || !exists(t, filepath.Join(r.root, "plex", "docker-compose.yml")) {
				t.Fatal("appdata or the stack's files were deleted by a refused remove")
			}
			if _, ok := r.store.rows["plex"]; !ok {
				t.Fatal("the row was deleted by a refused remove")
			}
		})
	}
}

// A project of the same name that something else runs (a hand-run
// ~/immich/compose.yml is project "immich") is not the stack's, and
// `compose down` would remove its containers: the remove is refused with its
// appdata and its containers untouched. Sharing a directory with it is refused
// as appdata_shared, like any other container.
func TestStackRemove_AProjectOfTheSameNameStartedElsewhereIsLeftAlone(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "immich")
	own := r.mkAppdata(t, "immich/library")
	foreignData := r.mkAppdata(t, "immich-by-hand")
	r.addContainer("immich", "immich-server-1", own)
	r.addContainerFrom("immich", filepath.Join(r.parent, "home", "immich"), "hand-run", foreignData)
	before := len(r.runner.Calls())

	if _, err := r.svc.Remove(context.Background(), "immich", true); !errors.Is(err, ErrStackProjectShared) {
		t.Fatalf("Remove() error = %v, want ErrStackProjectShared", err)
	}
	if len(r.runner.Calls()) != before {
		t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
	}
	if !exists(t, filepath.Join(own, "data")) || !exists(t, filepath.Join(foreignData, "data")) {
		t.Fatal("appdata was deleted by a refused remove")
	}
	if _, err := r.fake.Inspect(context.Background(), "hand-run"); err != nil {
		t.Fatalf("the hand-run container was taken down: %v", err)
	}

	r = newStackRig(t)
	r.create(t, "immich")
	shared := r.mkAppdata(t, "shared")
	r.addContainer("immich", "immich-server-1", shared)
	r.addContainerFrom("immich", filepath.Join(r.parent, "home", "immich"), "hand-run", shared)
	before = len(r.runner.Calls())
	if _, err := r.svc.Remove(context.Background(), "immich", true); !errors.Is(err, ErrAppdataShared) {
		t.Fatalf("Remove() error = %v, want ErrAppdataShared", err)
	}
	if len(r.runner.Calls()) != before || !exists(t, filepath.Join(shared, "data")) {
		t.Fatal("a refused remove ran docker or deleted appdata")
	}
}

// A container that carries the stack's labels but that `compose down` leaves
// running (an orphan of a hand-edited compose file) still uses its appdata:
// nothing is deleted, and the error says so.
func TestStackRemove_AppdataOfAContainerThatDownLeavesRunningIsKept(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "plex")
	config := r.mkAppdata(t, "plex/config")
	orphan := r.mkAppdata(t, "plex/orphan")
	r.addContainer("plex", "plex-web-1", config)
	r.addContainer("plex", "plex-orphan-1", orphan)
	r.sim.keep["id-plex-orphan-1"] = true

	_, err := r.svc.Remove(context.Background(), "plex", true)
	if err == nil || !strings.Contains(err.Error(), "plex-orphan-1") {
		t.Fatalf("Remove() error = %v, want one that names the container still running", err)
	}
	for _, p := range []string{config, orphan, filepath.Join(r.root, "plex", "docker-compose.yml")} {
		if !exists(t, p) {
			t.Fatalf("%s was deleted although a container of the stack is still running", p)
		}
	}
	if _, ok := r.store.rows["plex"]; !ok {
		t.Fatal("the row was deleted although a container of the stack is still running")
	}
}

// Another container that binds something inside the stack's directory loses it
// with the directory, so the remove is refused before docker runs.
func TestStackRemove_AContainerMountingInsideTheStackDirectoryRefusesTheDelete(t *testing.T) {
	for name, project := range map[string]string{"unmanaged": "", "another stack": "other"} {
		t.Run(name, func(t *testing.T) {
			r := newStackRig(t)
			r.create(t, "plex")
			shared := filepath.Join(r.root, "plex", "shared")
			if err := os.MkdirAll(shared, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(shared, "data"), []byte("precious"), 0o644); err != nil {
				t.Fatal(err)
			}
			config := r.mkAppdata(t, "plex/config")
			r.addContainer("plex", "plex-web-1", config)
			r.addContainer(project, "reader", shared)
			before := len(r.runner.Calls())

			_, err := r.svc.Remove(context.Background(), "plex", true)
			if !errors.Is(err, ErrAppdataShared) {
				t.Fatalf("Remove() error = %v, want ErrAppdataShared", err)
			}
			if len(r.runner.Calls()) != before {
				t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
			}
			if got := string(readFile(t, filepath.Join(shared, "data"))); got != "precious" {
				t.Fatalf("a directory another container uses was changed: %q", got)
			}
			if !exists(t, filepath.Join(config, "data")) {
				t.Fatal("appdata was deleted by a refused remove")
			}
			if _, ok := r.store.rows["plex"]; !ok {
				t.Fatal("the row was deleted by a refused remove")
			}
		})
	}
}

// With no .env, down works from the project name alone and removes every
// container and network of that name (and with appdata its named volumes), so a
// remove, plain or with appdata, is
// refused before docker runs while a container of that project is not the
// stack's own: a project of the same name that something else runs (a
// hand-run ~/immich/compose.yml is project "immich"). It also refuses when the
// containers cannot be listed or there is no Provider to list them. With
// appdata deletion the planning step, which lists the containers first, refuses
// those cases with its own errors.
func TestStackRemove_WithoutAnEnvIsRefusedWhileAnotherProjectOfThatNameIsRunning(t *testing.T) {
	cases := map[string]struct {
		setup func(r *stackRig)
		// plain and withAppdata are the error for deleteAppdata false and true.
		plain, withAppdata error
	}{
		"foreign container": {func(r *stackRig) {
			r.addContainer("web", "web-1", r.mkAppdata(t, "web/config"))
			r.addContainerFrom("web", filepath.Join(r.parent, "home", "web"), "hand-run-db")
		}, ErrStackProjectShared, ErrStackProjectShared},
		"only foreign containers": {func(r *stackRig) {
			r.addContainerFrom("web", filepath.Join(r.parent, "home", "web"), "hand-run-db")
		}, ErrStackProjectShared, ErrStackProjectShared},
		"listing fails": {func(r *stackRig) { r.fake.SetUnavailable(nil) }, ErrUnavailable, ErrUnavailable},
		"no Provider":   {func(r *stackRig) { r.svc.Provider = nil }, ErrUnavailable, ErrAppdataUnavailable},
	}
	for label, tc := range cases {
		for _, deleteAppdata := range []bool{false, true} {
			want := tc.plain
			if deleteAppdata {
				want = tc.withAppdata
			}
			t.Run(fmt.Sprintf("%s/deleteAppdata=%v", label, deleteAppdata), func(t *testing.T) {
				r := newStackRig(t)
				r.create(t, "web")
				tc.setup(r)
				r.svc.Cipher = unopenableCipher{}
				if err := os.Remove(filepath.Join(r.root, "web", ".env")); err != nil {
					t.Fatal(err)
				}
				before := len(r.runner.Calls())

				if _, err := r.svc.Remove(context.Background(), "web", deleteAppdata); !errors.Is(err, want) {
					t.Fatalf("Remove() error = %v, want %v", err, want)
				}
				if len(r.runner.Calls()) != before {
					t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
				}
				if _, ok := r.store.rows["web"]; !ok {
					t.Fatal("the row was deleted by a refused remove")
				}
			})
		}
	}
}

// With the .env present, `compose down --file …` still removes every container
// of the project name, and with --volumes its `<project>_<volume>` volumes,
// whatever directory they were started from. So a remove, plain or with
// appdata, is refused before docker runs while a container of that project is
// not the stack's own, and when the containers cannot be listed.
func TestStackRemove_WithAnEnvIsRefusedWhileAnotherProjectOfThatNameIsRunning(t *testing.T) {
	cases := map[string]struct {
		setup func(r *stackRig)
		want  error
	}{
		"foreign container": {func(r *stackRig) {
			r.addContainer("web", "web-1", r.mkAppdata(t, "web/config"))
			r.addContainerFrom("web", filepath.Join(r.parent, "home", "web"), "hand-run-db")
		}, ErrStackProjectShared},
		"only foreign containers": {func(r *stackRig) {
			r.addContainerFrom("web", filepath.Join(r.parent, "home", "web"), "hand-run-db")
		}, ErrStackProjectShared},
		"no Provider": {func(r *stackRig) { r.svc.Provider = nil }, ErrUnavailable},
	}
	for label, tc := range cases {
		for _, deleteAppdata := range []bool{false, true} {
			want := tc.want
			if deleteAppdata && label == "no Provider" {
				want = ErrAppdataUnavailable
			}
			t.Run(fmt.Sprintf("%s/deleteAppdata=%v", label, deleteAppdata), func(t *testing.T) {
				r := newStackRig(t)
				r.create(t, "web")
				tc.setup(r)
				if !exists(t, filepath.Join(r.root, "web", ".env")) {
					t.Fatal("the stack has no .env")
				}
				before := len(r.runner.Calls())

				if _, err := r.svc.Remove(context.Background(), "web", deleteAppdata); !errors.Is(err, want) {
					t.Fatalf("Remove() error = %v, want %v", err, want)
				}
				if len(r.runner.Calls()) != before {
					t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
				}
				if _, ok := r.store.rows["web"]; !ok {
					t.Fatal("the row was deleted by a refused remove")
				}
			})
		}
	}
}

// A project whose containers are all the stack's own is taken down by its name
// with no .env, however many of them there are, and an unrelated project is
// not in the way.
func TestStackRemove_WithoutAnEnvAllowsAProjectThatIsAllTheStacks(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "web")
	r.addContainer("web", "web-1")
	r.addContainer("web", "web-db")
	r.addContainerFrom("other", filepath.Join(r.parent, "home", "other"), "other-1")
	r.svc.Cipher = unopenableCipher{}
	if err := os.Remove(filepath.Join(r.root, "web", ".env")); err != nil {
		t.Fatal(err)
	}

	if _, err := r.svc.Remove(context.Background(), "web", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(r.composeCalls("down")) != 1 {
		t.Fatalf("down calls = %v, want one", r.composeCalls("down"))
	}
}

// Every way the array can be unusable, or appdata unlocatable, refuses a
// remove with appdata deletion before docker runs; a plain remove needs none
// of them.
func TestStackRemove_DeleteAppdataIsRefusedBeforeDockerRuns(t *testing.T) {
	cases := map[string]struct {
		set  func(r *stackRig)
		want error
	}{
		"array stopped": {func(r *stackRig) { r.svc.RequireArrayRunning = func() error { return ErrArrayStopped } }, ErrArrayStopped},
		"array unknown": {func(r *stackRig) { r.svc.RequireArrayRunning = nil }, ErrArrayStateUnknown},
		"hold refused": {func(r *stackRig) {
			r.svc.Admit = (&arrayHold{refuse: ErrArrayStopped}).admit
		}, ErrArrayStopped},
		"no appdata location": {func(r *stackRig) { r.svc.AppdataRoots = func(context.Context) ([]string, error) { return nil, nil } }, ErrAppdataUnavailable},
		"appdata not wired":   {func(r *stackRig) { r.svc.AppdataRoots = nil }, ErrAppdataUnavailable},
		"no engine":           {func(r *stackRig) { r.svc.Provider = nil }, ErrAppdataUnavailable},
		"engine unreachable":  {func(r *stackRig) { r.fake.SetUnavailable(nil) }, ErrUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newStackRig(t)
			r.create(t, "plex")
			config := r.mkAppdata(t, "plex/config")
			r.addContainer("plex", "plex-web-1", config)
			tc.set(r)
			before := len(r.runner.Calls())

			if _, err := r.svc.Remove(context.Background(), "plex", true); !errors.Is(err, tc.want) {
				t.Fatalf("Remove() error = %v, want %v", err, tc.want)
			}
			if len(r.runner.Calls()) != before {
				t.Fatalf("docker ran although the remove was refused: %v", r.runner.Calls()[before:])
			}
			if !exists(t, filepath.Join(config, "data")) || !exists(t, filepath.Join(r.root, "plex", "docker-compose.yml")) {
				t.Fatal("appdata or the stack's files were deleted by a refused remove")
			}
			if _, ok := r.store.rows["plex"]; !ok {
				t.Fatal("the row was deleted by a refused remove")
			}
		})
	}

	r := newStackRig(t)
	r.create(t, "plex")
	r.svc.RequireArrayRunning = func() error { return ErrArrayStopped }
	r.svc.Admit = (&arrayHold{refuse: ErrArrayStopped}).admit
	if _, err := r.svc.Remove(context.Background(), "plex", false); err != nil {
		t.Fatalf("a remove that keeps appdata was refused on a stopped array: %v", err)
	}
}

// The hold array stop drains is kept from the array check until the last
// deletion, so the cache cannot be unmounted in between.
func TestStackRemove_DeleteAppdataHoldsTheArrayActionUntilTheAppdataIsDeleted(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "plex")
	config := r.mkAppdata(t, "plex/config")
	r.addContainer("plex", "plex-web-1", config)
	hold := &arrayHold{}
	r.svc.Admit = hold.admit
	var appdataAtRelease, dirAtRelease bool
	hold.onFinish = func() {
		appdataAtRelease = exists(t, config)
		dirAtRelease = exists(t, filepath.Join(r.root, "plex"))
	}

	if _, err := r.svc.Remove(context.Background(), "plex", true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if appdataAtRelease || dirAtRelease {
		t.Fatal("the hold was released before the appdata and the stack directory were deleted")
	}
	if inflight, admitted := hold.counts(); inflight != 0 || admitted != 1 {
		t.Fatalf("hold in flight = %d, admitted = %d, want 0 and 1", inflight, admitted)
	}
}

func TestStackRemove_FailedDownDeletesNothing(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	appdata := r.mkAppdata(t, "nginx")
	r.addContainer("nginx", "nginx-web-1", appdata)
	r.runner.Script("docker", r.downArgv("nginx", "--volumes"), nil, errors.New("cannot connect to the Docker daemon"))

	if _, err := r.svc.Remove(context.Background(), "nginx", true); err == nil {
		t.Fatal("Remove() succeeded although docker compose down failed")
	}
	if !exists(t, filepath.Join(r.root, "nginx", "docker-compose.yml")) || !exists(t, filepath.Join(appdata, "data")) {
		t.Fatal("the stack directory or its appdata was deleted although its containers were not taken down")
	}
	if _, ok := r.store.rows["nginx"]; !ok {
		t.Fatal("the row was deleted although its containers were not taken down")
	}
}

func TestStackRemove_FailedRowDeleteIsReportedAndRetryable(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	r.store.deleteErr = errors.New("database is locked")

	res, err := r.svc.Remove(context.Background(), "nginx", true)
	if err == nil {
		t.Fatal("Remove() succeeded although the row could not be deleted")
	}
	if len(res.DeletedPaths) != 1 {
		t.Fatalf("DeletedPaths = %v, want the directory that was deleted", res.DeletedPaths)
	}

	r.store.deleteErr = nil
	if _, err := r.svc.Remove(context.Background(), "nginx", false); err != nil {
		t.Fatalf("retrying Remove: %v", err)
	}
	if _, ok := r.store.rows["nginx"]; ok {
		t.Fatal("the row survived the retry")
	}
}

func TestStackRemove_SymlinkedDirectoryIsNeitherFollowedNorDeleted(t *testing.T) {
	for _, deleteAppdata := range []bool{false, true} {
		r := newStackRig(t)
		r.create(t, "nginx")
		real := filepath.Join(r.parent, "elsewhere")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, "data"), []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(real, ".env"), []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(r.root, "nginx")
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, dir); err != nil {
			t.Fatal(err)
		}

		_, err := r.svc.Remove(context.Background(), "nginx", deleteAppdata)
		if !errors.Is(err, ErrStackDirUnsafe) {
			t.Fatalf("Remove(deleteAppdata=%v) error = %v, want ErrStackDirUnsafe", deleteAppdata, err)
		}
		for _, n := range []string{"data", ".env"} {
			if got := string(readFile(t, filepath.Join(real, n))); got != "precious" {
				t.Fatalf("a symlink target's %s was changed: %q", n, got)
			}
		}
		if _, ok := r.store.rows["nginx"]; !ok {
			t.Fatal("the row was deleted although the directory was refused")
		}
	}
}

// hookRunner calls before on every docker run, before the Runner sees it.
type hookRunner struct {
	Runner
	before func(args []string)
}

func (h hookRunner) Run(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
	h.before(args)
	return h.Runner.Run(ctx, env, name, args...)
}

func TestStackRemove_MissingDirectoryIsRegeneratedForDownThenRemoved(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	if err := os.RemoveAll(filepath.Join(r.root, "nginx")); err != nil {
		t.Fatal(err)
	}
	before := len(r.runner.Calls())
	var composeAtDown bool
	r.svc.Runner = hookRunner{Runner: r.runner, before: func([]string) {
		composeAtDown = exists(t, filepath.Join(r.root, "nginx", "docker-compose.yml"))
	}}

	if _, err := r.svc.Remove(context.Background(), "nginx", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	calls := r.runner.Calls()[before:]
	if len(calls) != 1 || calls[0].Args[len(calls[0].Args)-1] != "down" {
		t.Fatalf("calls = %v, want one compose down", calls)
	}
	if !composeAtDown {
		t.Fatal("down ran without a compose file to read")
	}
	if exists(t, filepath.Join(r.root, "nginx")) {
		t.Fatal("the regenerated files were kept after the stack was removed")
	}
}

// TestStackRemove_NeedsNoEnvToTakeTheStackDown is the state a bare-metal
// restore without the backup passphrase, or a changed machine key, leaves: a
// row whose .env cannot be opened and no .env file. The stack must still be
// removable, with or without appdata.
func TestStackRemove_NeedsNoEnvToTakeTheStackDown(t *testing.T) {
	ctx := context.Background()
	newKey := func() SecretCipher {
		key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(t.TempDir(), "secret.key"), &auth.FakeMachineKeyStore{})
		if err != nil {
			t.Fatalf("machine key: %v", err)
		}
		return key
	}
	states := map[string]func(r *stackRig){
		"cleared column": func(r *stackRig) {
			row := r.store.rows["web"]
			row.SealedEnv = []byte{}
			r.store.rows["web"] = row
		},
		"another machine key": func(r *stackRig) { r.svc.Cipher = newKey() },
	}
	for state, apply := range states {
		for _, deleteAppdata := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deleteAppdata=%v", state, deleteAppdata), func(t *testing.T) {
				r := newStackRig(t)
				r.svc.Cipher = newKey()
				r.create(t, "web")
				config := r.mkAppdata(t, "web/config")
				r.addContainer("web", "web-1", config)
				apply(r)
				if err := os.Remove(filepath.Join(r.root, "web", ".env")); err != nil {
					t.Fatal(err)
				}

				if _, err := r.svc.Remove(ctx, "web", deleteAppdata); err != nil {
					t.Fatalf("Remove(deleteAppdata=%v): %v", deleteAppdata, err)
				}
				var sub []string
				if deleteAppdata {
					sub = []string{"--volumes"}
				}
				// With no .env there is nothing to interpolate the compose file
				// with, and a template's compose file does not parse with its
				// variables empty, so down must not load the file at all: it
				// works from the project label.
				want := append([]string{"compose", "--project-name", "web", "down"}, sub...)
				calls := r.composeCalls("down")
				if len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(want, "\x00") {
					t.Fatalf("down calls = %v, want one %q", calls, want)
				}
				for _, a := range calls[0].Args {
					if a == "--file" || a == "--env-file" || a == "--project-directory" {
						t.Fatalf("down loads the compose file (%s) although there is no .env to interpolate it with: %v", a, calls[0].Args)
					}
				}
				if _, ok := r.store.rows["web"]; ok {
					t.Fatal("the row was kept")
				}
				if exists(t, filepath.Join(r.root, "web", ".env")) {
					t.Fatal("an .env was written from a row that cannot open it")
				}
				if deleteAppdata == exists(t, config) {
					t.Fatalf("appdata exists = %v with deleteAppdata = %v", exists(t, config), deleteAppdata)
				}
			})
		}
	}
}

// With an .env on disk, down reads it, however the row looks.
func TestStackRemove_UsesTheEnvFileThatIsOnDisk(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "web")
	row := r.store.rows["web"]
	row.SealedEnv = []byte{}
	r.store.rows["web"] = row

	if _, err := r.svc.Remove(context.Background(), "web", false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if calls := r.composeCalls("down"); len(calls) != 1 || strings.Join(calls[0].Args, "\x00") != strings.Join(r.downArgv("web"), "\x00") {
		t.Fatalf("down calls = %v, want one %q", calls, r.downArgv("web"))
	}
}

func TestStackUp_RefusedWhileArrayStoppedOrUnknown(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	before := len(r.runner.Calls())

	r.svc.RequireArrayRunning = func() error { return ErrArrayStopped }
	if err := r.svc.Up(context.Background(), "nginx"); !errors.Is(err, ErrArrayStopped) {
		t.Fatalf("Up() error = %v, want ErrArrayStopped", err)
	}
	r.svc.RequireArrayRunning = nil
	if err := r.svc.Up(context.Background(), "nginx"); !errors.Is(err, ErrArrayStateUnknown) {
		t.Fatalf("Up() error = %v, want ErrArrayStateUnknown", err)
	}
	if len(r.runner.Calls()) != before {
		t.Fatal("docker ran although the array was not known to be running")
	}
}

func TestStackUp_RunsComposeUpAndKeepsHandEditedFiles(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}
	released := false
	r.svc.Admit = func() (func(), error) { return func() { released = true }, nil }

	if err := r.svc.Up(context.Background(), "nginx"); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !released {
		t.Fatal("the array-action hold was not released")
	}
	if got := string(readFile(t, filepath.Join(dir, "docker-compose.yml"))); got != "services: {}\n# edited\n" {
		t.Fatalf("Up overwrote a file that exists: %q", got)
	}
	if !exists(t, filepath.Join(dir, "meta.json")) {
		t.Fatal("Up did not regenerate a missing file from the row")
	}
	calls := r.runner.Calls()
	last := calls[len(calls)-1]
	if want := r.composeArgv("nginx", "up", "--detach"); strings.Join(last.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", last.Args, want)
	}
}

func TestStack_RelativeRootIsRefused(t *testing.T) {
	r := newStackRig(t)
	r.svc.Root = "stacks"
	if _, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services: {}\n"}); err == nil {
		t.Fatal("Create() accepted a relative stacks directory")
	}
	if exists(t, "stacks") {
		_ = os.RemoveAll("stacks")
		t.Fatal("a relative stacks directory was created in the working directory")
	}
}

func TestStackGetAndList(t *testing.T) {
	r := newStackRig(t)
	r.create(t, "b")
	r.create(t, "a")
	list, err := r.svc.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" || list[0].TemplateID != "nginx" {
		t.Fatalf("List() = %+v", list)
	}
	if _, err := r.svc.Get(context.Background(), "missing"); !errors.Is(err, ErrStackNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrStackNotFound", err)
	}
}

// TestStack_ClearedSealedEnvNeverWritesAnEmptyEnv is the bare-metal restore
// case: the restore clears stacks.env (internal/backup sealedColumns) and
// brings each stack's .env back from secrets.age. With the real machine
// key, an empty sealed column must neither overwrite the restored .env nor
// be turned into an empty one when that file is missing.
func TestStack_ClearedSealedEnvNeverWritesAnEmptyEnv(t *testing.T) {
	ctx := context.Background()
	key, err := auth.LoadOrGenerateMachineKey(ctx, filepath.Join(t.TempDir(), "secret.key"), &auth.FakeMachineKeyStore{})
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	r := newStackRig(t)
	r.svc.Cipher = key
	r.create(t, "nginx")
	dir := filepath.Join(r.root, "nginx")

	row := r.store.rows["nginx"]
	row.SealedEnv = []byte{}
	r.store.rows["nginx"] = row
	restored := filepath.Join(dir, ".env")
	if err := os.WriteFile(restored, []byte("TOKEN=restored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "meta.json")); err != nil {
		t.Fatal(err)
	}

	if err := r.svc.Up(ctx, "nginx"); err != nil {
		t.Fatalf("Up with a restored .env: %v", err)
	}
	if got := string(readFile(t, restored)); got != "TOKEN=restored\n" {
		t.Fatalf("a restored .env was overwritten: %q", got)
	}
	if !exists(t, filepath.Join(dir, "meta.json")) {
		t.Fatal("a missing meta.json was not regenerated")
	}

	if err := os.Remove(restored); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Up(ctx, "nginx"); err == nil {
		t.Fatal("Up succeeded with no .env and no sealed env to regenerate it from")
	}
	if exists(t, restored) {
		t.Fatal("an empty .env was written from a cleared sealed env")
	}
}

// clearComposeEnv unsets every variable composeEnv passes on, restoring them
// when the test ends, so a test sees only what it sets.
func clearComposeEnv(t *testing.T) {
	t.Helper()
	for _, name := range composeEnvNames {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func envNames(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		out[name] = value
	}
	return out
}

// Compose lets a variable of its own environment override --env-file, so the
// daemon's environment must not reach it whole: a stack's .env, whose
// non-reserved names the daemon also has, is interpolated from the .env, and
// Docker still gets the daemon's PATH and the other variables it needs.
func TestStack_ComposeRunsWithTheDaemonsDockerVariablesAndNothingThatOverridesTheStacksEnv(t *testing.T) {
	clearComposeEnv(t)
	t.Setenv("DOCKER_HOST", "unix:///daemon.sock")
	t.Setenv("PATH", "/daemon/bin")
	t.Setenv("HOME", "/daemon/home")
	t.Setenv("INVOCATION_ID", "daemon-invocation")
	t.Setenv("COMPOSE_PROJECT_NAME", "daemon-project")
	r := newStackRig(t)
	if _, err := r.svc.Create(context.Background(), NewStack{
		Name:    "nginx",
		Compose: "services:\n  web:\n    image: nginx:1.27\n",
		Env:     "INVOCATION_ID=stack-invocation\nTOKEN=s3cret\n",
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Up(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Remove(context.Background(), "nginx", false); err != nil {
		t.Fatal(err)
	}

	calls := r.runner.Calls()
	if len(calls) < 3 {
		t.Fatalf("calls = %v, want a config, an up and a down", calls)
	}
	for _, c := range calls {
		if c.Env == nil {
			t.Fatalf("%v ran with the daemon's whole environment", c.Args)
		}
		env := envNames(c.Env)
		if env["PATH"] != "/daemon/bin" || env["HOME"] != "/daemon/home" || env["DOCKER_HOST"] != "unix:///daemon.sock" {
			t.Errorf("%v: environment %v, want the daemon's PATH, HOME and DOCKER_HOST, which Docker needs", c.Args, c.Env)
		}
		for _, name := range []string{"INVOCATION_ID", "COMPOSE_PROJECT_NAME"} {
			if _, ok := env[name]; ok {
				t.Errorf("%v: %s is in the environment, where Compose would take it over the stack's .env: %v", c.Args, name, c.Env)
			}
		}
		if len(env) != 3 {
			t.Errorf("%v: environment %v, want only PATH, HOME and DOCKER_HOST", c.Args, c.Env)
		}
	}
}

func TestStackCreate_RefusesAnEnvThatDefinesAReservedNameAndWritesNothing(t *testing.T) {
	for _, env := range []string{
		"PATH=/stack/bin\n",
		"TOKEN=x\nexport HOME=/stack\n",
		"DOCKER_HOST=tcp://stack:2375\n",
		"DOCKER_CONFIG\n",
		"  XDG_RUNTIME_DIR : /run\n",
	} {
		r := newStackRig(t)
		_, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services:\n  web:\n    image: nginx\n", Env: env})
		if !errors.Is(err, ErrReservedEnvName) {
			t.Fatalf("Create with .env %q: err = %v, want ErrReservedEnvName", env, err)
		}
		if len(r.store.rows) != 0 || r.store.calls != 0 {
			t.Errorf(".env %q: a row was stored (%d rows, %d store calls)", env, len(r.store.rows), r.store.calls)
		}
		if _, statErr := os.Lstat(filepath.Join(r.root, "nginx")); statErr == nil || len(r.runner.Calls()) != 0 {
			t.Errorf(".env %q: files were written or compose ran (%v, %d calls)", env, statErr, len(r.runner.Calls()))
		}
	}
	r := newStackRig(t)
	_, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services:\n  web:\n    image: nginx\n", Env: "PATH=/x\nHOME=/y\n"})
	if err == nil || !strings.Contains(err.Error(), "PATH, HOME") {
		t.Errorf("err = %v, want it to name every reserved variable", err)
	}
	for _, name := range []string{"MYPATH", "PATHX", "DOCKER_HOSTNAME", "HOMEDIR"} {
		r := newStackRig(t)
		if _, err := r.svc.Create(context.Background(), NewStack{Name: "nginx", Compose: "services:\n  web:\n    image: nginx\n", Env: name + "=x\n# PATH=/commented\n"}); err != nil {
			t.Errorf("a .env with %s was refused: %v", name, err)
		}
	}
}

// A stack stored before the rule keeps working: the daemon's value of a name
// its .env defines is left out of the run, as it was, and a warning says so.
func TestStack_ALegacyEnvThatDefinesAReservedNameKeepsTheDropAndLogsAWarning(t *testing.T) {
	clearComposeEnv(t)
	t.Setenv("PATH", "/daemon/bin")
	t.Setenv("HOME", "/daemon/home")
	r := newStackRig(t)
	r.create(t, "nginx")
	row := r.store.rows["nginx"]
	row.SealedEnv = xorAll([]byte("export PATH=/stack/bin\nTOKEN=s3cret\n"))
	r.store.rows["nginx"] = row
	if err := os.WriteFile(filepath.Join(r.root, "nginx", ".env"), []byte("export PATH=/stack/bin\nTOKEN=s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	before := len(r.runner.Calls())
	if err := r.svc.Up(context.Background(), "nginx"); err != nil {
		t.Fatal(err)
	}
	calls := r.runner.Calls()[before:]
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want one up", calls)
	}
	env := envNames(calls[0].Env)
	if _, ok := env["PATH"]; ok || env["HOME"] != "/daemon/home" {
		t.Errorf("environment %v, want the stack's PATH to win (the daemon's left out) and HOME to be the daemon's", calls[0].Env)
	}
	if got := logged.String(); !strings.Contains(got, "defines PATH") || !strings.Contains(got, filepath.Join(r.root, "nginx", ".env")) {
		t.Errorf("log = %q, want a warning naming the .env and PATH", got)
	}
}

func TestStack_ComposeEnvKeepsTheDockerVariablesTheEnvFileDoesNotDefine(t *testing.T) {
	clearComposeEnv(t)
	t.Setenv("DOCKER_HOST", "unix:///daemon.sock")
	t.Setenv("PATH", "/daemon/bin")
	t.Setenv("INVOCATION_ID", "daemon-invocation")
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("# PATH=/commented\nMYPATH=/mine\nTOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{envFile, "", filepath.Join(t.TempDir(), "missing.env")} {
		env, err := composeEnv(file)
		if err != nil {
			t.Fatal(err)
		}
		got := envNames(env)
		if got["PATH"] != "/daemon/bin" || got["DOCKER_HOST"] != "unix:///daemon.sock" {
			t.Errorf("composeEnv(%q) = %v, want the daemon's PATH and DOCKER_HOST", file, env)
		}
		if _, ok := got["INVOCATION_ID"]; ok {
			t.Errorf("composeEnv(%q) = %v, passed a variable Docker does not need", file, env)
		}
		if env == nil {
			t.Errorf("composeEnv(%q) is nil, which runs the command with the daemon's whole environment", file)
		}
	}
}

func TestStack_ComposeEnvIsNotNilWhenNothingIsPassed(t *testing.T) {
	clearComposeEnv(t)
	env, err := composeEnv("")
	if err != nil || env == nil || len(env) != 0 {
		t.Fatalf("composeEnv = %#v, %v, want an empty non-nil environment", env, err)
	}
}

func TestStack_EnvFileDefines(t *testing.T) {
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"PATH=/x", true},
		{"  PATH = /x", true},
		{"export PATH=/x", true},
		{"PATH: /x", true},
		{"PATH", true},
		{"PATH\r\nOTHER=1", true},
		{"OTHER=1\nPATH='/x'", true},
		{"PATHX=/x", false},
		{"MYPATH=/x", false},
		{"# PATH=/x", false},
		{"TOKEN=PATH=/x", false},
		{"", false},
	} {
		if got := envFileDefines([]byte(tc.line), "PATH"); got != tc.want {
			t.Errorf("envFileDefines(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
