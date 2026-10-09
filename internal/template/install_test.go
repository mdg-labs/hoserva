package template

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/mdg-labs/hoserva/internal/container"
)

type fakePorts struct {
	used map[int]bool
	err  error
}

func (f fakePorts) UsedPorts(context.Context) (map[int]bool, error) { return f.used, f.err }

type fakeGPU struct {
	devices []string
	gid     string
	gidErr  error
}

func (f fakeGPU) RenderDevices(context.Context) ([]string, error) { return f.devices, nil }
func (f fakeGPU) RenderGID(context.Context) (string, error)       { return f.gid, f.gidErr }

// fakeStacks keeps what is created, and publishes the host ports of those
// stacks the way Compose resolves them: each `ports` entry with the stack's
// .env substituted. Nothing is ever started.
type fakeStacks struct {
	created []container.NewStack
	// edited names the stacks Get reports as manually edited.
	edited   map[string]bool
	err      error
	portsErr error
	// existing are ports of stacks that were not created through this fake.
	existing map[int]bool
	// envErr fails UpdateEnv, and envWrites counts the calls that reached it.
	envErr    error
	envWrites int
}

func (f *fakeStacks) find(name string) (int, bool) {
	for i, n := range f.created {
		if n.Name == name {
			return i, true
		}
	}
	return 0, false
}

func (f *fakeStacks) Get(_ context.Context, name string) (container.Stack, error) {
	i, ok := f.find(name)
	if !ok {
		return container.Stack{}, container.ErrStackNotFound
	}
	n := f.created[i]
	return container.Stack{Name: n.Name, Compose: n.Compose, TemplateSource: n.TemplateSource, TemplateID: n.TemplateID, TemplateRevision: n.TemplateRevision, ManuallyEdited: f.edited[name]}, nil
}

func (f *fakeStacks) Env(_ context.Context, name string) (string, error) {
	i, ok := f.find(name)
	if !ok {
		return "", container.ErrStackNotFound
	}
	return f.created[i].Env, nil
}

func (f *fakeStacks) UpdateEnv(ctx context.Context, name, env string) (container.Stack, error) {
	f.envWrites++
	if f.envErr != nil {
		return container.Stack{}, f.envErr
	}
	if names := container.ReservedEnvDefined(env); len(names) > 0 {
		return container.Stack{}, container.ErrReservedEnvName
	}
	i, ok := f.find(name)
	if !ok {
		return container.Stack{}, container.ErrStackNotFound
	}
	f.created[i].Env = env
	return f.Get(ctx, name)
}

func (f *fakeStacks) Create(_ context.Context, n container.NewStack) (container.Stack, error) {
	if f.err != nil {
		return container.Stack{}, f.err
	}
	f.created = append(f.created, n)
	return container.Stack{Name: n.Name, TemplateSource: n.TemplateSource, TemplateID: n.TemplateID, TemplateRevision: n.TemplateRevision}, nil
}

func (f *fakeStacks) PublishedPorts(context.Context) (map[int]bool, error) {
	if f.portsErr != nil {
		return nil, f.portsErr
	}
	used := map[int]bool{}
	for p := range f.existing {
		used[p] = true
	}
	for _, n := range f.created {
		var doc struct {
			Services map[string]struct {
				Ports []string `yaml:"ports"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal([]byte(n.Compose), &doc); err != nil {
			return nil, err
		}
		env := envLines(n.Env)
		for _, svc := range doc.Services {
			for _, spec := range svc.Ports {
				parts := strings.Split(os.Expand(spec, func(k string) string { return env[k] }), ":")
				if len(parts) < 2 {
					continue
				}
				host := parts[len(parts)-2]
				if p, err := strconv.Atoi(host); err == nil {
					used[p] = true
				}
			}
		}
	}
	return used, nil
}

// zeroReader makes generated secrets repeatable.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0xab
	}
	return len(p), nil
}

func newInstaller(t *testing.T) (*Installer, *fakeStacks) {
	t.Helper()
	stacks := &fakeStacks{}
	return &Installer{
		Catalog:  DirCatalog{Root: fixtureDir, Source: SourceCurated},
		Stacks:   stacks,
		Ports:    fakePorts{used: map[int]bool{}},
		Shares:   func(context.Context) ([]string, error) { return []string{"downloads", "media"}, nil },
		GPU:      fakeGPU{devices: []string{"/dev/dri/renderD128"}, gid: "44"},
		Timezone: func() string { return "Europe/Vienna" },
		Random:   zeroReader{},
	}, stacks
}

func input(t *testing.T, p *Plan, name string) ResolvedInput {
	t.Helper()
	for _, in := range p.Inputs {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("plan has no input %s: %+v", name, p.Inputs)
	return ResolvedInput{}
}

func envLines(env string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(env), "\n") {
		k, v, _ := strings.Cut(l, "=")
		out[k] = v
	}
	return out
}

func TestInstallRecordsSourceIDAndRevisionAndWritesTheTemplateAsItIs(t *testing.T) {
	in, stacks := newInstaller(t)
	plan, st, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stacks.created) != 1 {
		t.Fatalf("created %d stacks, want 1", len(stacks.created))
	}
	got := stacks.created[0]
	if got.Name != "jellyfin" || got.TemplateSource != SourceCurated || got.TemplateID != "jellyfin" || got.TemplateRevision != "1" {
		t.Errorf("stack = %+v", got)
	}
	if st.TemplateID != "jellyfin" || plan.Revision != 1 {
		t.Errorf("returned stack %+v, plan revision %d", st, plan.Revision)
	}
	want, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if got.Compose != string(want) {
		t.Errorf("the Compose file must be the template unchanged, with its x-hoserva block:\n%s", got.Compose)
	}
	env := envLines(got.Env)
	for k, v := range map[string]string{
		"APPDATA":       "/mnt/cache/appdata",
		"MEDIA":         "/mnt/user/media",
		"WEBUI_PORT":    "8096",
		"TZ":            "Europe/Vienna",
		"TRANSCODE_GPU": "",
	} {
		if env[k] != v {
			t.Errorf(".env %s = %q, want %q", k, env[k], v)
		}
	}
}

func TestPortConflictGetsTheNextFreePortInsteadOfFailingTheInstall(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Ports = fakePorts{used: map[int]bool{8096: true, 8097: true}}
	plan, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("a port conflict must not fail the install: %v", err)
	}
	port := input(t, plan, "WEBUI_PORT")
	if port.Value != "8098" || port.Requested != "8096" {
		t.Errorf("WEBUI_PORT = %q (requested %q), want 8098 (requested 8096)", port.Value, port.Requested)
	}
	if env := envLines(stacks.created[0].Env); env["WEBUI_PORT"] != "8098" {
		t.Errorf(".env WEBUI_PORT = %q, want 8098", env["WEBUI_PORT"])
	}
	if free := input(t, plan, "APPDATA"); free.Requested != "" {
		t.Errorf("a non-port input reports a requested value %q", free.Requested)
	}
}

func TestAnExplicitPortThatIsFreeIsKept(t *testing.T) {
	in, _ := newInstaller(t)
	in.Ports = fakePorts{used: map[int]bool{8096: true}}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{"WEBUI_PORT": "9000"}})
	if err != nil {
		t.Fatal(err)
	}
	if port := input(t, plan, "WEBUI_PORT"); port.Value != "9000" || port.Requested != "" {
		t.Errorf("WEBUI_PORT = %+v", port)
	}
}

func TestTwoPortInputsAreNeverGivenTheSamePort(t *testing.T) {
	in, _ := newInstaller(t)
	in.Catalog = MapCatalog{Source: "test", Templates: map[string]string{"twoports": `
services:
  web:
    image: registry.example.com/x:1
    ports:
      - ${HTTP_PORT}:80
      - ${ADMIN_PORT}:81
x-hoserva:
  schema: 1
  id: twoports
  revision: 1
  title: Two ports
  categories: [system]
  icon: icon.svg
  docs: https://example.com/docs
  inputs:
    HTTP_PORT:  { kind: port, default: 8000 }
    ADMIN_PORT: { kind: port, default: 8000 }
`}}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "twoports"})
	if err != nil {
		t.Fatal(err)
	}
	a, h := input(t, plan, "ADMIN_PORT"), input(t, plan, "HTTP_PORT")
	if a.Value != "8000" || h.Value != "8001" || h.Requested != "8000" {
		t.Errorf("ADMIN_PORT=%+v HTTP_PORT=%+v: the second input must move off the first's port", a, h)
	}
}

func TestNoFreePortAboveTheRequestedOneIsAnError(t *testing.T) {
	in, stacks := newInstaller(t)
	used := map[int]bool{}
	for p := 65000; p <= 65535; p++ {
		used[p] = true
	}
	in.Ports = fakePorts{used: used}
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{"WEBUI_PORT": "65000"}})
	if !errors.Is(err, ErrNoFreePort) || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d", err, len(stacks.created))
	}
}

func TestAnUnreadablePortSourceFailsTheInstallInsteadOfAssumingFreePorts(t *testing.T) {
	in, stacks := newInstaller(t)
	boom := errors.New("proc unreadable")
	in.Ports = fakePorts{err: boom}
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"})
	if !errors.Is(err, boom) || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d", err, len(stacks.created))
	}
}

func TestTwoInstallsInARowAreGivenDifferentPortsAlthoughNothingIsStarted(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Ports = fakePorts{used: map[int]bool{}}
	var ports []string
	for _, name := range []string{"jellyfin", "jellyfin-2", "jellyfin-3"} {
		plan, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		ports = append(ports, input(t, plan, "WEBUI_PORT").Value)
	}
	if strings.Join(ports, ",") != "8096,8097,8098" {
		t.Errorf("ports = %v, want 8096, 8097, 8098", ports)
	}
	if env := envLines(stacks.created[1].Env); env["WEBUI_PORT"] != "8097" {
		t.Errorf("the second stack's .env WEBUI_PORT = %q, want 8097", env["WEBUI_PORT"])
	}
}

func TestPreviewSkipsThePortsOfStacksThatWereNeverStarted(t *testing.T) {
	in, stacks := newInstaller(t)
	stacks.existing = map[int]bool{8096: true}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	if port := input(t, plan, "WEBUI_PORT"); port.Value != "8097" || port.Requested != "8096" {
		t.Errorf("WEBUI_PORT = %+v, want 8097 requested 8096", port)
	}
}

func TestAnUnreadableStackPortSourceFailsThePreviewAndTheInstall(t *testing.T) {
	in, stacks := newInstaller(t)
	boom := errors.New("compose config failed")
	stacks.portsErr = boom
	if _, err := in.Preview(context.Background(), PlanRequest{ID: "jellyfin"}); !errors.Is(err, boom) {
		t.Fatalf("Preview err = %v, want the stack port error", err)
	}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"}); !errors.Is(err, boom) || len(stacks.created) != 0 {
		t.Fatalf("Install err = %v, created = %d", err, len(stacks.created))
	}
}

func TestSecretsAreGeneratedAndOnlyEverWrittenToTheEnv(t *testing.T) {
	in, stacks := newInstaller(t)
	plan, _, err := in.Install(context.Background(), PlanRequest{ID: "aio-notes"})
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("ab", 24)
	if env := envLines(stacks.created[0].Env); env["DB_PASSWORD"] != secret {
		t.Fatalf(".env DB_PASSWORD = %q, want the generated %q", env["DB_PASSWORD"], secret)
	}
	pw := input(t, plan, "DB_PASSWORD")
	if !pw.Generated || pw.Value != "" {
		t.Errorf("the plan reports the secret: %+v", pw)
	}
	if strings.Contains(plan.Compose, secret) || strings.Contains(stacks.created[0].Compose, secret) {
		t.Error("a secret is in the Compose file")
	}
	for _, p := range plan.Privileges {
		if strings.Contains(p.Detail, secret) {
			t.Error("a secret is in the privilege summary")
		}
	}
}

func TestSecretsComeFromTheRandomSourceAndDifferBetweenInstalls(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Random = nil
	for _, name := range []string{"one", "two"} {
		if _, _, err := in.Install(context.Background(), PlanRequest{ID: "aio-notes", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	a, b := envLines(stacks.created[0].Env)["DB_PASSWORD"], envLines(stacks.created[1].Env)["DB_PASSWORD"]
	if len(a) != 48 || a == b {
		t.Errorf("secrets %q and %q must be 48 characters and differ", a, b)
	}
}

func TestPreviewGeneratesNoSecret(t *testing.T) {
	in, stacks := newInstaller(t)
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "aio-notes"})
	if err != nil {
		t.Fatal(err)
	}
	if pw := input(t, plan, "DB_PASSWORD"); !pw.Generated || pw.Value != "" {
		t.Errorf("DB_PASSWORD = %+v", pw)
	}
	if strings.Contains(plan.env, strings.Repeat("ab", 24)) {
		t.Error("a preview generated a secret")
	}
	if len(stacks.created) != 0 {
		t.Error("a preview created a stack")
	}
}

func TestASuppliedSecretIsUsed(t *testing.T) {
	in, stacks := newInstaller(t)
	plan, _, err := in.Install(context.Background(), PlanRequest{ID: "aio-notes", Values: map[string]string{"DB_PASSWORD": "my secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if env := envLines(stacks.created[0].Env); env["DB_PASSWORD"] != `"my secret"` {
		t.Errorf(".env DB_PASSWORD = %q, want it double-quoted", env["DB_PASSWORD"])
	}
	if pw := input(t, plan, "DB_PASSWORD"); pw.Generated || pw.Value != "" {
		t.Errorf("DB_PASSWORD = %+v", pw)
	}
}

func TestMultiServiceTemplateIsInstalledWithEveryService(t *testing.T) {
	in, stacks := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "aio-notes"}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(stacks.created[0].Compose), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Services) != 3 {
		t.Errorf("services = %v, want app, db and cache", doc.Services)
	}
}

func composeServices(t *testing.T, compose string) map[string]map[string]any {
	t.Helper()
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		t.Fatalf("the Compose file is not YAML: %v\n%s", err, compose)
	}
	return doc.Services
}

func TestGPUDeviceIsMappedAndTheStackJoinsTheRenderGroup(t *testing.T) {
	in, stacks := newInstaller(t)
	plan, _, err := in.Install(context.Background(), PlanRequest{ID: "render-box", Values: map[string]string{"GPU": "/dev/dri/renderD128"}})
	if err != nil {
		t.Fatal(err)
	}
	svc := composeServices(t, stacks.created[0].Compose)["render-box"]
	if got := svc["devices"]; !equalList(got, "/dev/dri/renderD128:/dev/dri/renderD128") {
		t.Errorf("devices = %v", got)
	}
	if got := svc["group_add"]; !equalList(got, "44") {
		t.Errorf("group_add = %v, want the host's render group id as a string", got)
	}
	if !strings.Contains(stacks.created[0].Compose, "x-hoserva:") || !strings.Contains(stacks.created[0].Compose, "# Test fixture") {
		t.Error("the GPU mapping dropped the x-hoserva block or the template's comments")
	}
	if gpu := input(t, plan, "GPU"); gpu.Value != "/dev/dri/renderD128" || len(gpu.Suggestions) != 1 {
		t.Errorf("GPU = %+v", gpu)
	}
	if env := envLines(stacks.created[0].Env); env["GPU"] != "/dev/dri/renderD128" {
		t.Errorf(".env GPU = %q", env["GPU"])
	}
}

func equalList(v any, want ...string) bool {
	l, ok := v.([]any)
	if !ok || len(l) != len(want) {
		return false
	}
	for i, e := range l {
		if e != want[i] {
			return false
		}
	}
	return true
}

func TestGPUMappingKeepsEntriesAServiceAlreadyHas(t *testing.T) {
	out, err := addGPU([]byte("services:\n  a:\n    image: x\n    devices:\n      - /dev/fuse:/dev/fuse\n    group_add:\n      - \"44\"\n  b:\n    image: y\n"), "/dev/dri/renderD128", "44")
	if err != nil {
		t.Fatal(err)
	}
	svcs := composeServices(t, string(out))
	if !equalList(svcs["a"]["devices"], "/dev/fuse:/dev/fuse", "/dev/dri/renderD128:/dev/dri/renderD128") {
		t.Errorf("a.devices = %v", svcs["a"]["devices"])
	}
	if !equalList(svcs["a"]["group_add"], "44") {
		t.Errorf("a.group_add = %v: the group must not be listed twice", svcs["a"]["group_add"])
	}
	if !equalList(svcs["b"]["devices"], "/dev/dri/renderD128:/dev/dri/renderD128") {
		t.Errorf("b.devices = %v", svcs["b"]["devices"])
	}
}

func TestNoGPUChosenLeavesTheComposeFileUntouched(t *testing.T) {
	in, stacks := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "render-box"}); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(filepath.Join(fixtureDir, "render-box", ComposeFile))
	if !bytes.Equal([]byte(stacks.created[0].Compose), want) {
		t.Error("the Compose file changed without a GPU")
	}
}

func TestAGPUThatTheHostDoesNotOfferIsRefused(t *testing.T) {
	in, stacks := newInstaller(t)
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "render-box", Values: map[string]string{"GPU": "/dev/dri/renderD129"}})
	if !errors.Is(err, ErrInvalidInput) || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d", err, len(stacks.created))
	}
	in.GPU = nil
	_, _, err = in.Install(context.Background(), PlanRequest{ID: "render-box", Values: map[string]string{"GPU": "/dev/dri/renderD128"}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("with no GPU host, err = %v", err)
	}
}

func TestGPUWithoutARenderGroupFailsTheInstall(t *testing.T) {
	in, stacks := newInstaller(t)
	in.GPU = fakeGPU{devices: []string{"/dev/dri/renderD128"}, gidErr: ErrGPUUnavailable}
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "render-box", Values: map[string]string{"GPU": "/dev/dri/renderD128"}})
	if !errors.Is(err, ErrGPUUnavailable) || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d", err, len(stacks.created))
	}
}

func TestPathWithoutADefaultTakesTheShareOfItsRole(t *testing.T) {
	in, _ := newInstaller(t)
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "render-box"})
	if err != nil {
		t.Fatal(err)
	}
	d := input(t, plan, "DOWNLOADS")
	if d.Value != "/mnt/user/downloads" {
		t.Errorf("DOWNLOADS = %q, want the downloads share", d.Value)
	}
	if strings.Join(d.Suggestions, ",") != "/mnt/user/downloads,/mnt/user/media" {
		t.Errorf("suggestions = %v, want the existing shares", d.Suggestions)
	}
	if a := input(t, plan, "APPDATA"); a.Value != "/mnt/cache/appdata" || len(a.Suggestions) != 0 {
		t.Errorf("APPDATA = %+v", a)
	}
}

func TestPathWithoutADefaultAndNoMatchingShareNeedsAValue(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Shares = func(context.Context) ([]string, error) { return []string{"media"}, nil }
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "render-box"})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "DOWNLOADS") || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d", err, len(stacks.created))
	}
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "render-box", Values: map[string]string{"DOWNLOADS": "/mnt/user/media/dl/"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := input(t, plan, "DOWNLOADS"); d.Value != "/mnt/user/media/dl" {
		t.Errorf("DOWNLOADS = %q, want the cleaned path", d.Value)
	}
}

func TestInvalidInputsAreRefusedBeforeAnythingIsCreated(t *testing.T) {
	for name, req := range map[string]PlanRequest{
		"unknown input":      {ID: "jellyfin", Values: map[string]string{"NOPE": "1"}},
		"port out of range":  {ID: "jellyfin", Values: map[string]string{"WEBUI_PORT": "70000"}},
		"port not a number":  {ID: "jellyfin", Values: map[string]string{"WEBUI_PORT": "web"}},
		"relative path":      {ID: "jellyfin", Values: map[string]string{"MEDIA": "media"}},
		"quote in path":      {ID: "jellyfin", Values: map[string]string{"MEDIA": "/mnt/user/it's"}},
		"line break in path": {ID: "jellyfin", Values: map[string]string{"MEDIA": "/mnt/user/a\nB=1"}},
		"bad time zone":      {ID: "jellyfin", Values: map[string]string{"TZ": "../etc/passwd"}},
	} {
		in, stacks := newInstaller(t)
		_, _, err := in.Install(context.Background(), req)
		if !errors.Is(err, ErrInvalidInput) || len(stacks.created) != 0 {
			t.Errorf("%s: err = %v, created = %d", name, err, len(stacks.created))
		}
	}
	in, _ := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Name: "../x"}); !errors.Is(err, container.ErrInvalidStackName) {
		t.Errorf("bad stack name: err = %v", err)
	}
}

func TestPathWithSpacesIsQuotedInTheEnv(t *testing.T) {
	in, stacks := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{"MEDIA": "/mnt/user/my $media #1"}}); err != nil {
		t.Fatal(err)
	}
	if env := envLines(stacks.created[0].Env); env["MEDIA"] != `"/mnt/user/my \$media #1"` {
		t.Errorf(".env MEDIA = %q", env["MEDIA"])
	}
}

func TestTemplateProblemsStopTheInstall(t *testing.T) {
	in, stacks := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "nope"}); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	in.Catalog = MapCatalog{Templates: map[string]string{
		"broken": "services:\n  a:\n    image: x\n    volumes:\n      - ${GONE}:/data\nx-hoserva:\n  schema: 1\n  id: broken\n  revision: 1\n  title: B\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n",
		"other":  strings.Replace(mustRead(t, "jellyfin"), "id: jellyfin", "id: not-other", 1),
	}}
	for _, id := range []string{"broken", "other"} {
		if _, _, err := in.Install(context.Background(), PlanRequest{ID: id}); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("%s: err = %v, want ErrInvalidTemplate", id, err)
		}
	}
	if len(stacks.created) != 0 {
		t.Error("a template with problems was installed")
	}
}

func TestASecondYAMLDocumentIsRefusedBecauseComposeMergesIt(t *testing.T) {
	const first = "services:\n  probe:\n    image: x\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"
	const hidden = "services:\n  probe:\n    privileged: true\n    network_mode: host\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n      - /etc:/hostetc\n"
	for name, compose := range map[string]string{
		"second document":        first + "---\n" + hidden,
		"explicit end, then one": first + "...\n---\n" + hidden,
		"trailing separator":     first + "---\n",
		"empty map document":     first + "---\n{}\n",
		"comment-only document":  first + "---\n# nothing\n",
	} {
		if tpl, issues := Parse([]byte(compose)); tpl != nil || len(issues) != 1 || !strings.Contains(issues[0].Message, "single YAML document") {
			t.Errorf("%s: Parse = %v, %v, want the single-document refusal", name, tpl, issues)
		}
		in, stacks := newInstaller(t)
		in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
		if _, err := in.Preview(context.Background(), PlanRequest{ID: "probe"}); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("%s: Preview err = %v, want ErrInvalidTemplate", name, err)
		}
		if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe"}); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("%s: Install err = %v, want ErrInvalidTemplate", name, err)
		}
		if len(stacks.created) != 0 {
			t.Errorf("%s: a stack was created", name)
		}
	}
}

func TestDocumentMarkersAroundASingleDocumentAreAccepted(t *testing.T) {
	const doc = "services:\n  probe:\n    image: x\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"
	for name, compose := range map[string]string{
		"leading separator": "---\n" + doc,
		"trailing end":      doc + "...\n",
		"both":              "---\n" + doc + "...\n",
	} {
		if _, issues := Parse([]byte(compose)); len(issues) != 0 {
			t.Errorf("%s: Parse issues = %v", name, issues)
		}
	}
}

func mustRead(t *testing.T, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, id, ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestACreateFailureIsReturnedAsIs(t *testing.T) {
	in, stacks := newInstaller(t)
	stacks.err = container.ErrStackExists
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "jellyfin"}); !errors.Is(err, container.ErrStackExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimezoneDefaultsToTheHostsAndThenUTC(t *testing.T) {
	in, _ := newInstaller(t)
	plan, _ := in.Preview(context.Background(), PlanRequest{ID: "jellyfin"})
	if tz := input(t, plan, "TZ"); tz.Value != "Europe/Vienna" {
		t.Errorf("TZ = %q", tz.Value)
	}
	in.Timezone = nil
	plan, _ = in.Preview(context.Background(), PlanRequest{ID: "jellyfin", Values: map[string]string{}})
	if tz := input(t, plan, "TZ"); tz.Value != "UTC" {
		t.Errorf("TZ = %q", tz.Value)
	}
}

func privilegeKinds(p []Privilege) []string {
	out := make([]string, len(p))
	for i, e := range p {
		out[i] = e.Kind + ":" + e.Detail
	}
	return out
}

func TestPrivilegedTemplateSurfacesEveryRequestInTheSummary(t *testing.T) {
	in, _ := newInstaller(t)
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "risky-agent"})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(privilegeKinds(plan.Privileges), "|")
	want := "privileged:|host_network:|host_pid:host|host_cgroup:|device_cgroup_rules:c 189:* rmw|added_capabilities:SYS_ADMIN|confinement_disabled:apparmor:unconfined|group_add:disk|docker_socket:/var/run/docker.sock"
	if got != want {
		t.Errorf("summary:\n got %s\nwant %s", got, want)
	}
	for _, p := range plan.Privileges {
		if p.Service != "agent" || p.Description == "" {
			t.Errorf("privilege %+v needs its service and a plain-language description", p)
		}
	}
}

func TestPrivilegeSummaryFollowsTheChosenPaths(t *testing.T) {
	in, _ := newInstaller(t)
	plan, err := in.Preview(context.Background(), PlanRequest{ID: "risky-agent", Values: map[string]string{"HOST_DATA": "/etc"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(privilegeKinds(plan.Privileges), "|"); !strings.HasSuffix(got, "|host_path:/etc") {
		t.Errorf("summary = %s, want it to end with the path outside the pool", got)
	}
}

func TestCleanTemplateHasNoPrivileges(t *testing.T) {
	in, _ := newInstaller(t)
	for _, id := range []string{"jellyfin", "aio-notes", "render-box"} {
		plan, err := in.Preview(context.Background(), PlanRequest{ID: id, Values: map[string]string{}})
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if len(plan.Privileges) != 0 {
			t.Errorf("%s: privileges %v, want none (appdata on cache and media on the pool are the layout)", id, privilegeKinds(plan.Privileges))
		}
	}
}

func TestPrivilegeSummaryIsReadFromTheComposeContent(t *testing.T) {
	const head = "x-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"
	tests := []struct {
		name, service, inputs string
		want                  string
	}{
		{"privileged from an input", "privileged: ${PRIV}", "  inputs:\n    PRIV: { kind: string, default: \"true\" }\n", "privileged:"},
		{"quoted host network", "network_mode: \"host\"", "", "host_network:"},
		{"bridge network", "network_mode: bridge", "", ""},
		{"pid of another container", "pid: container:other", "", "host_pid:container:other"},
		{"cgroup private", "cgroup: private", "", ""},
		{"privileged false", "privileged: false", "", ""},
		{"privileged y", "privileged: y", "", "privileged:"},
		{"privileged capital Y", "privileged: Y", "", "privileged:"},
		{"privileged yes", "privileged: yes", "", "privileged:"},
		{"privileged quoted on", "privileged: \"on\"", "", "privileged:"},
		{"privileged mixed case", "privileged: \"tRuE\"", "", "privileged:"},
		{"privileged from an input with y", "privileged: ${P}", "  inputs:\n    P: { kind: string, default: \"y\" }\n", "privileged:"},
		{"privileged from an input's alternative", "privileged: ${P:+Y}", "  inputs:\n    P: { kind: string, default: \"a\" }\n", "privileged:"},
		{"privileged n", "privileged: n", "", ""},
		{"privileged quoted off", "privileged: \"OFF\"", "", ""},
		{"privileged from an input with no", "privileged: ${P}", "  inputs:\n    P: { kind: string, default: \"no\" }\n", ""},
		{"privileged an unknown word", "privileged: maybe", "", "refused:privileged: must be true or false"},
		{"privileged as one", "privileged: 1", "", "refused:privileged: must be true or false"},
		{"privileged as a quoted one", "privileged: \"1\"", "", "refused:privileged: must be true or false"},
		{"privileged empty", "privileged: \"\"", "", "refused:privileged: must be true or false"},
		{"privileged null", "privileged:", "", "refused:privileged: must be true or false"},
		{"network of the older external mapping", "networks: [hostnet]\nnetworks:\n  hostnet: { external: { name: host } }", "", "refused:external: must be true or false"},
		{"network external as a word", "networks: [hostnet]\nnetworks:\n  hostnet: { external: \"yes\" }", "", "refused:external: must be true or false"},
		{"external volume of the older mapping", "volumes:\n      - data:/d\nvolumes:\n  data: { external: { name: other } }", "", "refused:\"external\" is not accepted"},
		{"socket in a long entry", "volumes:\n      - { type: bind, source: /run/docker.sock, target: /s }", "", "docker_socket:/run/docker.sock"},
		{"directory holding the socket", "volumes:\n      - /var/run:/hostrun", "", "docker_socket:/var/run|host_path:/var/run"},
		{"the whole host", "volumes:\n      - /:/host:ro", "", "docker_socket:/|host_path:/"},
		{"outside the pool", "volumes:\n      - /dev/dri:/dev/dri", "", "host_path:/dev/dri"},
		{"next to the pool", "volumes:\n      - /mnt/user0/x:/x", "", "host_path:/mnt/user0/x"},
		{"dot dot out of the pool", "volumes:\n      - /mnt/user/../etc:/x", "", "host_path:/mnt/user/../etc"},
		{"inside the pool", "volumes:\n      - /mnt/user/media:/m\n      - /mnt/cache/appdata/x:/c", "", ""},
		{"the whole cache, which holds Docker's data-root", "volumes:\n      - /mnt/cache:/c", "", "host_path:/mnt/cache"},
		{"Docker's data-root on the cache", "volumes:\n      - /mnt/cache/docker:/d", "", "host_path:/mnt/cache/docker"},
		{"inside Docker's data-root", "volumes:\n      - /mnt/cache/docker/overlay2:/o", "", "host_path:/mnt/cache/docker/overlay2"},
		{"Docker's current data-root on the cache", "volumes:\n      - /mnt/cache/.docker:/d", "", "host_path:/mnt/cache/.docker"},
		{"inside Docker's current data-root", "volumes:\n      - /mnt/cache/.docker/overlay2:/o", "", "host_path:/mnt/cache/.docker/overlay2"},
		{"the pool's view of Docker's current data-root", "volumes:\n      - /mnt/user/.docker/volumes:/o", "", "host_path:/mnt/user/.docker/volumes"},
		{"the pool's view of the legacy data-root", "volumes:\n      - /mnt/user/docker:/d", "", "host_path:/mnt/user/docker"},
		{"next to Docker's current data-root", "volumes:\n      - /mnt/cache/.dockerish:/d", "", ""},
		{"next to Docker's data-root", "volumes:\n      - /mnt/cache/dockerish:/d", "", ""},
		{"named volume bound to Docker's data-root", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: /mnt/cache/docker/volumes }", "", "host_path:/mnt/cache/docker/volumes"},
		{"named volume", "volumes:\n      - data:/data", "", ""},
		{"long entry in the pool", "volumes:\n      - { type: bind, source: /mnt/user/a, target: /a }", "", ""},
		{"named volume bound to a host folder", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: /etc }", "", "host_path:/etc"},
		{"named volume bound to the socket directory", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: /var/run }", "", "docker_socket:/var/run|host_path:/var/run"},
		{"named volume in a long entry", "volumes:\n      - { type: volume, source: data, target: /d }\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: /etc }", "", "host_path:/etc"},
		{"named volume bound inside the pool", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: none, o: bind, device: /mnt/user/media }", "", ""},
		{"named volume on a network share", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: nfs, o: \"addr=10.0.0.2\", device: \":/export\" }", "", ""},
		{"named volume without options", "volumes:\n      - data:/d\nvolumes:\n  data: {}", "", ""},
		{"SYS_ADMIN added", "cap_add:\n      - SYS_ADMIN", "", "added_capabilities:SYS_ADMIN"},
		{"a benign capability added", "cap_add: [NET_BIND_SERVICE]", "", "added_capabilities:NET_BIND_SERVICE"},
		{"every added capability is listed", "cap_add: [NET_ADMIN, CHOWN]", "", "added_capabilities:NET_ADMIN, CHOWN"},
		{"all capabilities added", "cap_add: [ALL]", "", "added_capabilities:ALL"},
		{"a lone capability instead of a list", "cap_add: SYS_ADMIN", "", "added_capabilities:SYS_ADMIN"},
		{"capability chosen by an input", "cap_add: [\"${CAP}\"]", "  inputs:\n    CAP: { kind: string, default: SYS_PTRACE }\n", "added_capabilities:SYS_PTRACE"},
		{"capability from an empty optional input", "cap_add: [\"${CAP}\"]", "  inputs:\n    CAP: { kind: string, optional: true }\n", ""},
		{"no capability added", "cap_add: []", "", ""},
		{"capabilities dropped", "cap_drop: [ALL]", "", ""},
		{"apparmor unconfined", "security_opt: [apparmor:unconfined]", "", "confinement_disabled:apparmor:unconfined"},
		{"apparmor unconfined with an equals sign", "security_opt: [apparmor=unconfined]", "", "confinement_disabled:apparmor=unconfined"},
		{"apparmor unconfined in capitals", "security_opt: [\"APPARMOR:UNCONFINED\"]", "", "confinement_disabled:APPARMOR:UNCONFINED"},
		{"seccomp unconfined", "security_opt:\n      - seccomp:unconfined", "", "confinement_disabled:seccomp:unconfined"},
		{"selinux labelling disabled", "security_opt: [label:disable]", "", "confinement_disabled:label:disable"},
		{"no-new-privileges off", "security_opt: [no-new-privileges:false]", "", "confinement_disabled:no-new-privileges:false"},
		{"unmasked system paths", "security_opt: [systempaths=unconfined]", "", "confinement_disabled:systempaths=unconfined"},
		{"a seccomp profile the summary cannot read", "security_opt: [seccomp:/etc/allow-all.json]", "", "confinement_disabled:seccomp:/etc/allow-all.json"},
		{"an selinux label", "security_opt: [label:type:spc_t]", "", "confinement_disabled:label:type:spc_t"},
		{"a security option the summary does not know", "security_opt: [future-option:on]", "", "confinement_disabled:future-option:on"},
		{"no-new-privileges on", "security_opt: [no-new-privileges:true]", "", ""},
		{"no-new-privileges bare", "security_opt: [no-new-privileges]", "", ""},
		{"a named apparmor profile", "security_opt: [apparmor:docker-default]", "", ""},
		{"only the weakening options are listed", "security_opt: [no-new-privileges:true, seccomp:unconfined, apparmor:unconfined]", "", "confinement_disabled:seccomp:unconfined, apparmor:unconfined"},
		{"confinement disabled by an input", "security_opt: [\"${OPT}\"]", "  inputs:\n    OPT: { kind: string, default: \"seccomp:unconfined\" }\n", "confinement_disabled:seccomp:unconfined"},
		{"confinement option from an empty optional input", "security_opt: [\"${OPT}\"]", "  inputs:\n    OPT: { kind: string, optional: true }\n", ""},
		{"sysctls", "sysctls:\n      net.ipv4.ip_forward: \"1\"", "", ""},
		{"extra groups", "group_add: [video, \"44\"]", "", "group_add:video, 44"},
		{"an extra group as a number", "group_add:\n      - 44", "", "group_add:44"},
		{"every kind in one service", "cap_add: [SYS_ADMIN]\n    security_opt: [apparmor:unconfined]\n    group_add: [disk]", "", "added_capabilities:SYS_ADMIN|confinement_disabled:apparmor:unconfined|group_add:disk"},
		{"every GPU reserved", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: nvidia, count: all, capabilities: [gpu] }", "", "gpu_reservation:driver nvidia, count all, capabilities gpu"},
		{"a count of GPUs reserved", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: nvidia, count: 2, capabilities: [gpu, utility] }", "", "gpu_reservation:driver nvidia, count 2, capabilities gpu, utility"},
		{"GPUs reserved by device id", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: nvidia, device_ids: [\"0\", GPU-abc], capabilities: [gpu] }", "", "gpu_reservation:driver nvidia, device ids 0, GPU-abc, capabilities gpu"},
		{"a reservation without a driver", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { capabilities: [gpu] }", "", "gpu_reservation:capabilities gpu"},
		{"every reservation is listed", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: nvidia, count: 1, capabilities: [gpu] }\n            - { driver: amd, count: all, capabilities: [gpu] }", "", "gpu_reservation:driver nvidia, count 1, capabilities gpu; driver amd, count all, capabilities gpu"},
		{"a reservation chosen by an input", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: \"${DRV}\", count: all, capabilities: [gpu] }", "  inputs:\n    DRV: { kind: string, default: nvidia }\n", "gpu_reservation:driver nvidia, count all, capabilities gpu"},
		{"a reservation from empty optional inputs", "deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: \"${DRV}\", capabilities: [\"${CAP}\"] }", "  inputs:\n    DRV: { kind: string, optional: true }\n    CAP: { kind: string, optional: true }\n", ""},
		{"a reservation that is not a mapping", "deploy:\n      resources:\n        reservations:\n          devices:\n            - nvidia", "", "gpu_reservation:nvidia"},
		{"reservations of CPU and memory only", "deploy:\n      resources:\n        reservations: { cpus: \"0.5\", memory: 256M }", "", ""},
		{"no device reservations", "deploy:\n      resources:\n        reservations:\n          devices: []", "", ""},
		{"limits only", "deploy:\n      resources:\n        limits: { memory: 1g, cpus: \"2\" }", "", ""},
		{"nvidia runtime", "runtime: nvidia", "", "container_runtime:nvidia"},
		{"runtime chosen by an input", "runtime: ${RT}", "  inputs:\n    RT: { kind: string, default: kata-runtime }\n", "container_runtime:kata-runtime"},
		{"Docker's default runtime", "runtime: runc", "", ""},
		{"runtime from an empty optional input", "runtime: ${RT}", "  inputs:\n    RT: { kind: string, optional: true }\n", ""},
		{"a runtime that is not a string", "runtime: 5", "", "container_runtime:5"},
		{"runtime and reservation together", "runtime: nvidia\n    deploy:\n      resources:\n        reservations:\n          devices:\n            - { driver: nvidia, count: all, capabilities: [gpu] }", "", "container_runtime:nvidia|gpu_reservation:driver nvidia, count all, capabilities gpu"},
		{"raw disk as a device", "devices:\n      - /dev/sda:/dev/sda", "", "host_path:/dev/sda"},
		{"device in a long entry", "devices:\n      - { source: /dev/sdb, target: /dev/sdb }", "", "host_path:/dev/sdb"},
		{"device chosen by an input", "devices:\n      - ${DEV}:/dev/x:rwm", "  inputs:\n    DEV: { kind: string, default: /dev/nvme0n1 }\n", "host_path:/dev/nvme0n1"},
		{"secret spliced into a path", "volumes:\n      - /mnt/user/${KEY:+../../etc}:/x", "  inputs:\n    KEY: { kind: secret }\n", "host_path:/mnt/user/../../etc"},
		{"volumes_from a container", "volumes_from:\n      - container:other", "", "refused:container:other"},
		{"volumes_from an unknown service", "volumes_from:\n      - ghost:ro", "", "refused:ghost:ro"},
		{"volumes_from a service of the template", "volumes_from:\n      - sidecar:ro\n  sidecar:\n    image: y", "", ""},
		{"include", "volumes: []\ninclude:\n  - other.yaml", "", "refused:include pulls in"},
		{"extends a file", "extends:\n      file: other.yaml\n      service: base", "", "refused:extends a service of another file"},
		{"extends in the same file", "extends: sidecar\n  sidecar:\n    image: y", "", ""},
		{"nfs volume with a bind option", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: nfs, o: bind, device: /etc }", "", "host_path:/etc"},
		{"cifs volume with an rbind option", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: cifs, o: \"addr=x,rbind\", device: /var/run }", "", "docker_socket:/var/run|host_path:/var/run"},
		{"nfs volume whose bind option is an input", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: nfs, o: \"${OPTS}\", device: /etc }", "  inputs:\n    OPTS: { kind: string, default: bind }\n", "host_path:/etc"},
		{"nfs volume with a local path as its device", "volumes:\n      - data:/d\nvolumes:\n  data:\n    driver_opts: { type: nfs, o: \"addr=x\", device: /etc }", "", "host_path:/etc"},
		{"top-level secret from a host file", "secrets: [shadow]\nsecrets:\n  shadow: { file: /etc/shadow }", "", "refused:secrets and configs mount a file"},
		{"top-level config from the socket", "configs: [sock]\nconfigs:\n  sock: { file: /var/run/docker.sock }", "", "refused:secrets and configs mount a file"},
		{"env file of another stack", "env_file: /var/lib/hoserva/stacks/other/.env", "", "refused:only the stack's own .env"},
		{"env file list with another file", "env_file:\n      - .env\n      - { path: ../other/.env }", "", "refused:only the stack's own .env"},
		{"env file of the stack", "env_file:\n      - ./.env", "", ""},
		{"build from a host context", "build: { context: /etc, dockerfile_inline: \"FROM alpine\\nCOPY shadow /shadow\" }\n    pull_policy: build", "", "refused:build reads a host directory"},
		{"pull policy build", "pull_policy: build", "", "refused:must not be build"},
		{"unknown service key", "userns_mode: host", "", "refused:\"userns_mode\" is not accepted"},
		{"use of the Docker API socket", "use_api_socket: true", "", "refused:\"use_api_socket\" is not accepted"},
		{"unknown top-level key", "tty: true\nfoo: 1", "", "refused:\"foo\" is not accepted"},
	}
	for _, tc := range tests {
		compose := "services:\n  probe:\n    image: x\n    " + tc.service + "\n" + head + tc.inputs
		in, _ := newInstaller(t)
		in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
		plan, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
		if refused, ok := strings.CutPrefix(tc.want, "refused:"); ok {
			if !errors.Is(err, ErrInvalidTemplate) || !strings.Contains(err.Error(), refused) {
				t.Errorf("%s: err = %v, want the template refused with %q", tc.name, err, refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got := strings.Join(privilegeKinds(plan.Privileges), "|")
		if got != tc.want {
			t.Errorf("%s: summary %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAPrivilegedValueThatIsNotABooleanIsRefusedNotTreatedAsFalse(t *testing.T) {
	const compose = "services:\n  probe:\n    image: x\n    privileged: ${P}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    P: { kind: string, default: \"false\" }\n"
	for _, v := range []string{"maybe", "1", "0", "t", " true", "true "} {
		in, stacks := newInstaller(t)
		in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
		for name, call := range map[string]func() error{
			"preview": func() error {
				_, err := in.Preview(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"P": v}})
				return err
			},
			"install": func() error {
				_, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"P": v}})
				return err
			},
		} {
			if err := call(); !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "privileged") {
				t.Errorf("%s with P=%q: err = %v, want the value refused", name, v, err)
			}
		}
		if len(stacks.created) != 0 {
			t.Errorf("P=%q: a stack was created", v)
		}
	}
}

func TestPreviewSummaryMatchesTheInstalledSummary(t *testing.T) {
	const compose = "services:\n  probe:\n    image: x\n    volumes:\n      - /mnt/user/${KEY:+../../etc}:/x\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    KEY: { kind: secret }\n"
	in, _ := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
	preview, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	installed, _, err := in.Install(context.Background(), PlanRequest{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	want := "host_path:/mnt/user/../../etc"
	for name, p := range map[string]*Plan{"preview": preview, "install": installed} {
		if got := strings.Join(privilegeKinds(p.Privileges), "|"); got != want {
			t.Errorf("%s summary = %q, want %q", name, got, want)
		}
	}
}

func TestInterpolateFollowsCompose(t *testing.T) {
	vals := map[string]string{"A": "a", "EMPTY": ""}
	for in, want := range map[string]string{
		"$A/x":              "a/x",
		"${A}":              "a",
		"${EMPTY:-d}":       "d",
		"${EMPTY-d}":        "",
		"${MISSING-d}":      "d",
		"${A:-d}":           "a",
		"${A:+alt}":         "alt",
		"${EMPTY:+alt}":     "",
		"${MISSING:-${A}x}": "ax",
		"$$A":               "$A",
		"cost $5":           "cost $5",
		"${":                "${",
	} {
		if got := interpolate(in, vals); got != want {
			t.Errorf("interpolate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummaryFlagsASourceRelativeToTheStackDirectory(t *testing.T) {
	tpl, issues := Parse([]byte("services:\n  a:\n    image: x\n    volumes:\n      - ./data:/data\nx-hoserva:\n  schema: 1\n  id: a\n  revision: 1\n  title: A\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"))
	if tpl == nil {
		t.Fatal(issues)
	}
	if got := strings.Join(privilegeKinds(tpl.Privileges(nil)), "|"); got != "host_path:./data" {
		t.Errorf("summary = %q", got)
	}
}

const optionalProbe = "services:\n  probe:\n    image: x\n    environment:\n      CLAIM: ${CLAIM}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    CLAIM: { kind: string%s }\n"

func TestAnOptionalStringInputMayStayEmptyAndARequiredOneMayNot(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		values     map[string]string
		wantErr    bool
	}{
		{"optional left out", ", optional: true", nil, false},
		{"optional given empty", ", optional: true", map[string]string{"CLAIM": ""}, false},
		{"required left out", "", nil, true},
		{"required given empty", "", map[string]string{"CLAIM": ""}, true},
		{"optional false left out", ", optional: false", nil, true},
	} {
		compose := strings.Replace(optionalProbe, "%s", tc.flag, 1)
		for call, run := range map[string]func(*Installer) (*Plan, error){
			"preview": func(in *Installer) (*Plan, error) {
				return in.Preview(context.Background(), PlanRequest{ID: "probe", Values: tc.values})
			},
			"install": func(in *Installer) (*Plan, error) {
				p, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: tc.values})
				return p, err
			},
		} {
			in, stacks := newInstaller(t)
			in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
			plan, err := run(in)
			if tc.wantErr && call == "preview" {
				if err != nil {
					t.Errorf("%s preview: %v, want the input listed with its error", tc.name, err)
					continue
				}
				if got := input(t, plan, "CLAIM"); !got.Required || !strings.Contains(got.Error, "CLAIM needs a value") || got.Value != "" {
					t.Errorf("%s preview: CLAIM = %+v, want it required, empty and with an error", tc.name, got)
				}
				continue
			}
			if tc.wantErr {
				var ie *InputError
				if !errors.Is(err, ErrInvalidInput) || !errors.As(err, &ie) || ie.Input != "CLAIM" || !strings.Contains(err.Error(), "CLAIM needs a value") || len(stacks.created) != 0 {
					t.Errorf("%s %s: err = %v, created = %d", tc.name, call, err, len(stacks.created))
				}
				continue
			}
			if err != nil {
				t.Errorf("%s %s: %v", tc.name, call, err)
				continue
			}
			if got := input(t, plan, "CLAIM").Value; got != "" {
				t.Errorf("%s %s: CLAIM = %q, want empty", tc.name, call, got)
			}
			if call == "install" {
				if v, ok := envLines(stacks.created[0].Env)["CLAIM"]; !ok || v != "" {
					t.Errorf("%s: .env = %q, want CLAIM written as an empty value", tc.name, stacks.created[0].Env)
				}
			}
		}
	}
}

func TestAnOptionalInputGivenAValueKeepsIt(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": strings.Replace(optionalProbe, "%s", ", optional: true", 1)}}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"CLAIM": "claim-abc"}}); err != nil {
		t.Fatal(err)
	}
	if v := envLines(stacks.created[0].Env)["CLAIM"]; v != "claim-abc" {
		t.Errorf("CLAIM = %q", v)
	}
}

func TestAnEmptyOptionalInputInAPrivilegeKeyIsResolvedLikeAnyEmptyValue(t *testing.T) {
	const compose = "services:\n  probe:\n    image: x\n    privileged: ${P}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    P: { kind: string, optional: true }\n"
	in, _ := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": compose}}
	_, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
	if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("err = %v, want the empty value refused as not a boolean", err)
	}
}

func TestInstallRefusesATemplateWhoseInputIsNamedLikeAReservedDockerVariable(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(src), "TZ: ${TZ}", "TZ: ${TZ}\n      EXTRA: ${PATH}", 1)
	text = strings.Replace(text, "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    PATH: { kind: string, default: /x }", 1)
	if err := os.MkdirAll(filepath.Join(dir, "jellyfin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jellyfin", ComposeFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	in, stacks := newInstaller(t)
	in.Catalog = DirCatalog{Root: dir, Source: SourceCurated}
	_, _, err = in.Install(context.Background(), PlanRequest{ID: "jellyfin"})
	if !errors.Is(err, ErrInvalidTemplate) || !strings.Contains(err.Error(), "PATH") || len(stacks.created) != 0 {
		t.Fatalf("err = %v, created = %d, want ErrInvalidTemplate naming PATH and no stack", err, len(stacks.created))
	}
}

// envValues are inputs a .env value has to carry that the single-quote form
// could not write (a backslash before the closing quote) or that Compose
// treats specially. A single quote and a line break are refused by
// checkEnvValue and so are not here.
var envValues = map[string]string{
	"trailing backslash":    `pass\`,
	"three trailing":        `pass\\\`,
	"only a backslash":      `\`,
	"backslash inside":      `a\b\\c`,
	"backslash then n":      `a\nb`,
	"backslash then dollar": `a\$b`,
	"dollar":                "pa$word",
	"dollar brace":          "${X}$$",
	"spaces":                "  two words  ",
	"hash":                  "a #b #",
	"double quote":          `say "hi"\`,
	"equals and colon":      "a=b:c",
	"unicode":               "pässwörd-日本\\",
}

func TestInstallWritesEveryInputAsAnEnvValueComposeReadsBackExactly(t *testing.T) {
	for name, value := range envValues {
		t.Run(name, func(t *testing.T) {
			in, stacks := newInstaller(t)
			_, _, err := in.Install(context.Background(), PlanRequest{ID: "aio-notes", Values: map[string]string{"DB_PASSWORD": value, "SITE_NAME": value}})
			if err != nil {
				t.Fatal(err)
			}
			env := stacks.created[0].Env
			got, err := composeDotenv(env)
			if err != nil {
				t.Fatalf("Compose cannot read the .env: %v\n%s", err, env)
			}
			for _, k := range []string{"DB_PASSWORD", "SITE_NAME"} {
				if got[k] != value {
					t.Errorf("Compose reads %s as %q, want %q\n%s", k, got[k], value, env)
				}
			}
			if got["WEBUI_PORT"] != "3000" {
				t.Errorf("the line after the values reads %q, want 3000", got["WEBUI_PORT"])
			}
		})
	}
}

const digestProbeHead = "x-hoserva:\n  schema: 1\n  id: probe\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n"

func digestProbe(revision int, service string) string {
	return "services:\n  probe:\n    image: x\n" + service + digestProbeHead + "  revision: " + strconv.Itoa(revision) + "\n"
}

func TestInstallRefusesATemplateThatChangedSinceThePreviewAndCreatesNothing(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Source: "curated", Templates: map[string]string{"probe": digestProbe(1, "")}}
	preview, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	in.Catalog = MapCatalog{Source: "curated", Templates: map[string]string{"probe": digestProbe(2, "    privileged: true\n")}}
	_, _, err = in.Install(context.Background(), PlanRequest{ID: "probe", PlanDigest: &preview.Digest})
	if !errors.Is(err, ErrTemplateChanged) {
		t.Fatalf("err = %v, want ErrTemplateChanged", err)
	}
	if len(stacks.created) != 0 {
		t.Errorf("a template that changed since the preview created %d stacks", len(stacks.created))
	}

	changed, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", PlanDigest: &changed.Digest}); err != nil {
		t.Fatalf("install with the matching digest: %v", err)
	}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Name: "probe2"}); err != nil {
		t.Fatalf("install with no digest: %v", err)
	}
	if len(stacks.created) != 2 {
		t.Errorf("created %d stacks, want 2", len(stacks.created))
	}
}

func TestPlanDigestChangesWithWhatWouldBeInstalled(t *testing.T) {
	digest := func(source string, revision int, service string) string {
		t.Helper()
		in, _ := newInstaller(t)
		in.Catalog = MapCatalog{Source: source, Templates: map[string]string{"probe": digestProbe(revision, service)}}
		p, err := in.Preview(context.Background(), PlanRequest{ID: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		if p.Digest == "" {
			t.Fatal("plan has no digest")
		}
		return p.Digest
	}
	base := digest("curated", 1, "")
	if again := digest("curated", 1, ""); again != base {
		t.Errorf("the same template gave %s and %s", base, again)
	}
	for name, got := range map[string]string{
		"source":     digest("mine", 1, ""),
		"revision":   digest("curated", 2, ""),
		"compose":    digest("curated", 1, "    command: [run]\n"),
		"privileges": digest("curated", 1, "    privileged: true\n"),
	} {
		if got == base {
			t.Errorf("a changed %s left the digest as it was", name)
		}
	}
}

func TestPlanDigestChangesWhenOnlyThePrivilegesChange(t *testing.T) {
	in, _ := newInstaller(t)
	a, err := in.Preview(context.Background(), PlanRequest{ID: "risky-agent", Values: map[string]string{"HOST_DATA": "/mnt/user/media"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := in.Preview(context.Background(), PlanRequest{ID: "risky-agent", Values: map[string]string{"HOST_DATA": "/etc"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == b.Digest {
		t.Error("a path outside the pool adds a privilege and must change the digest")
	}
}

func TestPreviewAndInstallOfTheSameRequestHaveTheSameDigest(t *testing.T) {
	for _, req := range []PlanRequest{
		{ID: "jellyfin"},
		{ID: "risky-agent"},
		{ID: "aio-notes", Values: map[string]string{}},
	} {
		in, _ := newInstaller(t)
		preview, err := in.Preview(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", req.ID, err)
		}
		installed, _, err := in.Install(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", req.ID, err)
		}
		if preview.Digest != installed.Digest {
			t.Errorf("%s: preview digest %s, install digest %s", req.ID, preview.Digest, installed.Digest)
		}
		req.PlanDigest = &preview.Digest
		in2, _ := newInstaller(t)
		if _, _, err := in2.Install(context.Background(), req); err != nil {
			t.Errorf("%s: install bound to the preview's digest: %v", req.ID, err)
		}
	}
}

func TestInstallRefusesAnEmptyPlanDigestInsteadOfReadingItAsAbsent(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Source: "curated", Templates: map[string]string{"probe": digestProbe(1, "    privileged: true\n")}}
	empty := ""
	_, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", PlanDigest: &empty})
	if !errors.Is(err, ErrTemplateChanged) {
		t.Fatalf("err = %v, want ErrTemplateChanged", err)
	}
	if len(stacks.created) != 0 {
		t.Errorf("an install bound to an empty digest created %d stacks", len(stacks.created))
	}
}
