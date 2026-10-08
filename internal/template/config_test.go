package template

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
)

const installedSecret = "abababababababababababababababababababababababab"

// installed installs a fixture template and returns the installer that can
// read it back, with the fake that holds its row.
func installed(t *testing.T, id string, values map[string]string) (*Installer, *fakeStacks) {
	t.Helper()
	in, stacks := newInstaller(t)
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: id, Values: values}); err != nil {
		t.Fatal(err)
	}
	return in, stacks
}

func configInput(t *testing.T, c *StackConfig, name string) ConfigInput {
	t.Helper()
	for _, in := range c.Inputs {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("config has no input %s: %+v", name, c.Inputs)
	return ConfigInput{}
}

func TestConfigReturnsEachInputWithItsValueAndNeverASecret(t *testing.T) {
	in, stacks := installed(t, "aio-notes", map[string]string{"SITE_NAME": "My notes", "WEBUI_PORT": "3100"})
	cfg, err := in.Config(context.Background(), "aio-notes")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stack.Name != "aio-notes" || cfg.Stack.TemplateID != "aio-notes" || cfg.Stack.TemplateRevision != "3" {
		t.Errorf("stack = %+v", cfg.Stack)
	}
	for name, want := range map[string]string{"APPDATA": "/mnt/cache/appdata", "WEBUI_PORT": "3100", "TZ": "Europe/Vienna", "SITE_NAME": "My notes"} {
		if got := configInput(t, cfg, name).Value; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	pw := configInput(t, cfg, "DB_PASSWORD")
	if pw.Kind != KindSecret || !pw.Set || pw.Value != "" || pw.Label != "Database password" {
		t.Errorf("DB_PASSWORD = %+v, want a secret that is set and carries no value", pw)
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg), installedSecret) {
		t.Error("the secret is in the config")
	}
	if len(stacks.created) != 1 || stacks.envWrites != 0 {
		t.Errorf("reading the config changed something: %d stacks, %d env writes", len(stacks.created), stacks.envWrites)
	}
}

func TestConfigOffersTheSharesToANonAppdataPathAndLocksTheDeviceInput(t *testing.T) {
	in, _ := installed(t, "jellyfin", nil)
	cfg, err := in.Config(context.Background(), "jellyfin")
	if err != nil {
		t.Fatal(err)
	}
	media := configInput(t, cfg, "MEDIA")
	if strings.Join(media.Suggestions, ",") != "/mnt/user/downloads,/mnt/user/media" {
		t.Errorf("MEDIA suggestions = %v", media.Suggestions)
	}
	if got := configInput(t, cfg, "APPDATA"); len(got.Suggestions) != 0 {
		t.Errorf("an appdata input is offered shares: %v", got.Suggestions)
	}
	gpu := configInput(t, cfg, "TRANSCODE_GPU")
	if !gpu.ReadOnly || gpu.Value != "" {
		t.Errorf("TRANSCODE_GPU = %+v, want read-only", gpu)
	}
	if configInput(t, cfg, "WEBUI_PORT").ReadOnly {
		t.Error("a port input is read-only")
	}
}

func TestUpdateConfigChangesOnlyTheChangedLinesOfTheEnv(t *testing.T) {
	in, stacks := installed(t, "aio-notes", nil)
	stacks.created[0].Env = "# kept\nEXTRA=1\n" + stacks.created[0].Env + "ANOTHER='a b'\n"
	before := stacks.created[0].Env
	composeBefore := stacks.created[0].Compose

	cfg, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{
		"SITE_NAME": "Team notes", "WEBUI_PORT": "3100", "APPDATA": "/mnt/cache/notes data",
	}})
	if err != nil {
		t.Fatal(err)
	}
	after := stacks.created[0].Env
	for _, want := range []string{`SITE_NAME="Team notes"` + "\n", "WEBUI_PORT=3100\n", `APPDATA="/mnt/cache/notes data"` + "\n"} {
		if !strings.Contains(after, want) {
			t.Errorf(".env lacks the line %q:\n%s", want, after)
		}
	}
	for _, kept := range []string{"# kept\n", "EXTRA=1\n", "ANOTHER='a b'\n", "DB_PASSWORD=" + installedSecret + "\n", "TZ=Europe/Vienna\n"} {
		if !strings.Contains(after, kept) {
			t.Errorf(".env lost %q:\n%s", kept, after)
		}
	}
	if len(strings.Split(after, "\n")) != len(strings.Split(before, "\n")) {
		t.Errorf("an input line was duplicated or dropped:\nbefore %q\nafter  %q", before, after)
	}
	if stacks.created[0].Compose != composeBefore {
		t.Error("the Compose file was regenerated")
	}
	if got := configInput(t, cfg, "WEBUI_PORT").Value; got != "3100" || configInput(t, cfg, "SITE_NAME").Value != "Team notes" {
		t.Errorf("the returned config does not show the new values: %+v", cfg.Inputs)
	}
	if strings.Contains(fmt.Sprintf("%+v", cfg), installedSecret) {
		t.Error("the secret is in the returned config")
	}
}

func TestUpdateConfigWithNothingChangedLeavesTheEnvByteIdentical(t *testing.T) {
	in, stacks := installed(t, "aio-notes", nil)
	stacks.created[0].Env = strings.Replace(stacks.created[0].Env, "SITE_NAME=Notes", "export SITE_NAME = \"Notes\"", 1)
	before := stacks.created[0].Env
	if _, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "Notes", "WEBUI_PORT": "3000"}}); err != nil {
		t.Fatal(err)
	}
	if stacks.created[0].Env != before {
		t.Errorf(".env changed although no value did:\nbefore %q\nafter  %q", before, stacks.created[0].Env)
	}
}

func TestASecretKeepsItsSealedValueUnlessReplacedOrGenerated(t *testing.T) {
	ctx := context.Background()
	in, stacks := installed(t, "aio-notes", nil)

	for name, values := range map[string]map[string]string{"left out": nil, "given empty": {"DB_PASSWORD": ""}} {
		if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: values}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := envLines(stacks.created[0].Env)["DB_PASSWORD"]; got != installedSecret {
			t.Fatalf("a secret %s changed to %q", name, got)
		}
	}

	if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"DB_PASSWORD": "my new password"}}); err != nil {
		t.Fatal(err)
	}
	if got := envLines(stacks.created[0].Env)["DB_PASSWORD"]; got != `"my new password"` {
		t.Errorf("a replaced secret = %q", got)
	}

	in.Random = repeatReader(0xcd)
	cfg, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Generate: []string{"DB_PASSWORD"}})
	if err != nil {
		t.Fatal(err)
	}
	generated := strings.Repeat("cd", 24)
	if got := envLines(stacks.created[0].Env)["DB_PASSWORD"]; got != generated {
		t.Errorf("a generated secret = %q, want %q", got, generated)
	}
	if pw := configInput(t, cfg, "DB_PASSWORD"); !pw.Set || pw.Value != "" || strings.Contains(fmt.Sprintf("%+v", cfg), generated) {
		t.Errorf("the response gives the generated secret away: %+v", pw)
	}
}

type repeatReader byte

func (r repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

func TestARefusedUpdateLeavesTheEnvByteIdenticalAndWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  ConfigUpdate
		want error
	}{
		{"port out of range", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": "70000"}}, ErrInvalidInput},
		{"port that is not a number", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": "web"}}, ErrInvalidInput},
		{"relative path", ConfigUpdate{Values: map[string]string{"APPDATA": "appdata"}}, ErrInvalidInput},
		{"bad time zone", ConfigUpdate{Values: map[string]string{"TZ": "../etc/passwd"}}, ErrInvalidInput},
		{"line break", ConfigUpdate{Values: map[string]string{"SITE_NAME": "a\nPATH=/x"}}, ErrInvalidInput},
		{"single quote", ConfigUpdate{Values: map[string]string{"DB_PASSWORD": "it's"}}, ErrInvalidInput},
		{"an input the stack lacks", ConfigUpdate{Values: map[string]string{"NOPE": "1"}}, ErrInvalidInput},
		{"generate of a name the stack lacks", ConfigUpdate{Generate: []string{"NOPE"}}, ErrInvalidInput},
		{"generate of a non-secret", ConfigUpdate{Generate: []string{"SITE_NAME"}}, ErrInvalidInput},
		{"generate with a value", ConfigUpdate{Values: map[string]string{"DB_PASSWORD": "x"}, Generate: []string{"DB_PASSWORD"}}, ErrInvalidInput},
		{"a valid change beside an invalid one", ConfigUpdate{Values: map[string]string{"SITE_NAME": "ok", "WEBUI_PORT": "0"}}, ErrInvalidInput},
	} {
		in, stacks := installed(t, "aio-notes", nil)
		before := stacks.created[0].Env
		_, err := in.UpdateConfig(context.Background(), "aio-notes", tc.req)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
		if stacks.created[0].Env != before || stacks.envWrites != 0 {
			t.Errorf("%s: a refused update reached the stack (%d writes):\n%s", tc.name, stacks.envWrites, stacks.created[0].Env)
		}
	}
}

func TestAnEmptyEntryTakesTheDefaultAndAnOptionalInputIsCleared(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"probe": strings.Replace(optionalProbe, "%s", ", optional: true", 1)}}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "probe", Values: map[string]string{"CLAIM": "claim-abc"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := in.UpdateConfig(context.Background(), "probe", ConfigUpdate{Values: map[string]string{"CLAIM": ""}}); err != nil {
		t.Fatal(err)
	}
	if v, ok := envLines(stacks.created[0].Env)["CLAIM"]; !ok || v != "" {
		t.Errorf(".env = %q, want CLAIM cleared to an empty value", stacks.created[0].Env)
	}

	in2, stacks2 := installed(t, "aio-notes", nil)
	if _, err := in2.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "Other"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := in2.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": ""}}); err != nil {
		t.Fatal(err)
	}
	if got := envLines(stacks2.created[0].Env)["SITE_NAME"]; got != "Notes" {
		t.Errorf("SITE_NAME = %q, want the default Notes", got)
	}
}

func TestAManuallyEditedStacksComposeFileIsNeverTouchedByAnUpdate(t *testing.T) {
	in, stacks := installed(t, "aio-notes", nil)
	edited := strings.Replace(stacks.created[0].Compose, "restart: unless-stopped", "restart: always", 1) + "# edited by hand\n"
	stacks.created[0].Compose = edited
	stacks.edited = map[string]bool{"aio-notes": true}

	cfg, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "Changed"}})
	if err != nil {
		t.Fatal(err)
	}
	if stacks.created[0].Compose != edited {
		t.Error("an update rewrote the manually edited Compose file")
	}
	if !cfg.Stack.ManuallyEdited {
		t.Error("the config does not say the stack was edited by hand")
	}
	if got := envLines(stacks.created[0].Env)["SITE_NAME"]; got != "Changed" {
		t.Errorf("SITE_NAME = %q", got)
	}
}

func TestAPortAnotherStackOrContainerTakesIsRefusedAndTheStacksOwnPortIsNot(t *testing.T) {
	ctx := context.Background()
	in, stacks := installed(t, "aio-notes", nil)
	in.Ports = fakePorts{used: map[int]bool{3000: true, 4000: true}}
	stacks.existing = map[int]bool{5000: true}
	before := stacks.created[0].Env

	for _, port := range []string{"4000", "5000"} {
		_, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": port}})
		if !errors.Is(err, ErrPortTaken) || !strings.Contains(err.Error(), "WEBUI_PORT asks for "+port) {
			t.Errorf("port %s: error = %v, want ErrPortTaken naming the input", port, err)
		}
		if stacks.created[0].Env != before || stacks.envWrites != 0 {
			t.Fatalf("port %s: the refusal reached the stack", port)
		}
	}

	if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": "3000", "SITE_NAME": "Same port"}}); err != nil {
		t.Errorf("keeping the stack's own port: %v", err)
	}
	if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": "3001"}}); err != nil {
		t.Errorf("a free port: %v", err)
	}
	if got := envLines(stacks.created[0].Env)["WEBUI_PORT"]; got != "3001" {
		t.Errorf("WEBUI_PORT = %q", got)
	}
}

func TestTheStacksPortsAreNotReadWhenNoPortChanges(t *testing.T) {
	in, stacks := installed(t, "aio-notes", nil)
	in.Ports = fakePorts{err: errors.New("cannot read the sockets")}
	stacks.portsErr = errors.New("cannot read the stacks")
	if _, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "Offline"}}); err != nil {
		t.Errorf("a change that touches no port needs the port sources: %v", err)
	}
	before := stacks.created[0].Env
	_, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"WEBUI_PORT": "3500"}})
	if err == nil || stacks.created[0].Env != before {
		t.Errorf("a port change with unreadable port sources: error = %v; it must be refused, never assumed free", err)
	}
}

func TestTwoPortInputsOfAStackCanMoveTogetherButNotOntoEachOther(t *testing.T) {
	in, stacks := newInstaller(t)
	in.Catalog = MapCatalog{Templates: map[string]string{"twoports": twoPortsTemplate}}
	if _, _, err := in.Install(context.Background(), PlanRequest{ID: "twoports"}); err != nil {
		t.Fatal(err)
	}
	in.Ports = fakePorts{used: map[int]bool{8001: true, 8002: true}}
	stacks.existing = map[int]bool{8001: true, 8002: true}
	ctx := context.Background()

	if _, err := in.UpdateConfig(ctx, "twoports", ConfigUpdate{Values: map[string]string{"HTTP_PORT": "8002", "ADMIN_PORT": "8001"}}); err != nil {
		t.Errorf("swapping the stack's own two ports: %v", err)
	}
	before := stacks.created[0].Env
	_, err := in.UpdateConfig(ctx, "twoports", ConfigUpdate{Values: map[string]string{"HTTP_PORT": "9100", "ADMIN_PORT": "9100"}})
	if !errors.Is(err, ErrPortTaken) || stacks.created[0].Env != before {
		t.Errorf("two inputs on one port: error = %v", err)
	}
}

const twoPortsTemplate = `services:
  app:
    image: x:1
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
  docs: https://example.com
  inputs:
    HTTP_PORT:  { kind: port, default: 8001 }
    ADMIN_PORT: { kind: port, default: 8002 }
`

func TestADeviceInputCannotChangeButItsCurrentValueMayBeSentBack(t *testing.T) {
	in, stacks := installed(t, "jellyfin", map[string]string{"TRANSCODE_GPU": "/dev/dri/renderD128"})
	before, composeBefore := stacks.created[0].Env, stacks.created[0].Compose
	if got := envLines(before)["TRANSCODE_GPU"]; got != "/dev/dri/renderD128" {
		t.Fatalf("setup: .env = %q", before)
	}

	for _, v := range []string{"", "/dev/dri/renderD129"} {
		_, err := in.UpdateConfig(context.Background(), "jellyfin", ConfigUpdate{Values: map[string]string{"TRANSCODE_GPU": v}})
		if !errors.Is(err, ErrInvalidInput) || !strings.Contains(err.Error(), "TRANSCODE_GPU") {
			t.Errorf("device set to %q: error = %v, want ErrInvalidInput naming the input", v, err)
		}
		if stacks.created[0].Env != before || stacks.envWrites != 0 {
			t.Fatalf("device set to %q reached the stack", v)
		}
	}
	cfg, err := in.UpdateConfig(context.Background(), "jellyfin", ConfigUpdate{Values: map[string]string{"TRANSCODE_GPU": "/dev/dri/renderD128", "WEBUI_PORT": "8200"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := configInput(t, cfg, "TRANSCODE_GPU").Value; got != "/dev/dri/renderD128" {
		t.Errorf("device = %q after an update that left it alone", got)
	}
	if stacks.created[0].Compose != composeBefore {
		t.Error("the Compose file's device mapping changed")
	}
}

func TestAStackWithoutAnXHosevaBlockHasNoConfig(t *testing.T) {
	in, stacks := newInstaller(t)
	stacks.created = append(stacks.created, container.NewStack{Name: "plain", Compose: "services:\n  web:\n    image: nginx:1\n", Env: "A=1\n"})
	if _, err := in.Config(context.Background(), "plain"); !errors.Is(err, ErrStackHasNoTemplate) {
		t.Errorf("Config = %v, want ErrStackHasNoTemplate", err)
	}
	_, err := in.UpdateConfig(context.Background(), "plain", ConfigUpdate{Values: map[string]string{"A": "2"}})
	if !errors.Is(err, ErrStackHasNoTemplate) || stacks.envWrites != 0 || stacks.created[0].Env != "A=1\n" {
		t.Errorf("UpdateConfig = %v (%d writes), want ErrStackHasNoTemplate and nothing written", err, stacks.envWrites)
	}
}

func TestAStoredBlockThatIsNotValidIsATemplateError(t *testing.T) {
	in, stacks := newInstaller(t)
	stacks.created = append(stacks.created,
		container.NewStack{Name: "bad", Compose: "services: {}\nx-hoserva:\n  schema: 1\n", Env: ""},
		container.NewStack{Name: "yaml", Compose: "services: [\n", Env: ""})
	for _, name := range []string{"bad", "yaml"} {
		if _, err := in.Config(context.Background(), name); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("Config(%s) = %v, want ErrInvalidTemplate", name, err)
		}
	}
}

func TestUnknownStackAndFailuresOfTheStackAreReturned(t *testing.T) {
	ctx := context.Background()
	in, stacks := installed(t, "aio-notes", nil)
	if _, err := in.Config(ctx, "nope"); !errors.Is(err, container.ErrStackNotFound) {
		t.Errorf("Config(unknown) = %v", err)
	}
	if _, err := in.UpdateConfig(ctx, "nope", ConfigUpdate{}); !errors.Is(err, container.ErrStackNotFound) {
		t.Errorf("UpdateConfig(unknown) = %v", err)
	}

	before := stacks.created[0].Env
	stacks.envErr = errors.New("disk full")
	_, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "x"}})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("a failed write = %v, want the failure, never a success", err)
	}
	if stacks.created[0].Env != before {
		t.Error("a failed write changed the env")
	}
}

func TestAnEnvThatDefinesAReservedNameIsRefusedByTheStackAndNothingChanges(t *testing.T) {
	in, stacks := installed(t, "aio-notes", nil)
	stacks.created[0].Env += "PATH=/tmp\n"
	before := stacks.created[0].Env
	_, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": "x"}})
	if !errors.Is(err, container.ErrReservedEnvName) || stacks.created[0].Env != before {
		t.Errorf("error = %v, env changed = %v", err, stacks.created[0].Env != before)
	}
}

func TestParseEnvLineReadsTheFormsComposeAccepts(t *testing.T) {
	for line, want := range map[string][3]string{
		"A=1":                   {"A", "1", "true"},
		"  export A = 2":        {"A", "2", "true"},
		"A='x y'":               {"A", "x y", "true"},
		`A='c:\dir\'`:           {"A", `c:\dir\`, "true"},
		`A='a $b \n'`:           {"A", `a $b \n`, "true"},
		`A="a \$b"`:             {"A", "a $b", "true"},
		`A="a\nb\rc"`:           {"A", "a\nb\rc", "true"},
		`A="c:\\dir\\"`:         {"A", `c:\dir\`, "true"},
		`A="a\qb"`:              {"A", `a\qb`, "true"},
		`A="x \"y\" \\ z"`:      {"A", `x "y" \ z`, "true"},
		"A=plain # comment":     {"A", "plain", "true"},
		"A=":                    {"A", "", "true"},
		"A=b=c":                 {"A", "b=c", "true"},
		"# A=1":                 {"", "", "false"},
		"":                      {"", "", "false"},
		"not a definition":      {"", "", "false"},
		"exportA=1":             {"exportA", "1", "true"},
		"A='unterminated":       {"A", "unterminated", "true"},
		"1BAD=1":                {"", "", "false"},
		"A=value-with#hash":     {"A", "value-with#hash", "true"},
		"A='it''s'":             {"A", "it", "true"},
		"A=\ttabbed\t":          {"A", "tabbed", "true"},
		"export\tA=1":           {"A", "1", "true"},
		"A=\"single ' inside\"": {"A", "single ' inside", "true"},
	} {
		k, v, ok := parseEnvLine(line)
		if k != want[0] || v != want[1] || fmt.Sprint(ok) != want[2] {
			t.Errorf("parseEnvLine(%q) = %q, %q, %v; want %q, %q, %s", line, k, v, ok, want[0], want[1], want[2])
		}
	}
}

func TestMergeEnvRewritesChangedInputsAndKeepsEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		values    map[string]string
		want      string
	}{
		{"nothing changed", "A=1\nB=2\n", map[string]string{"A": "1", "B": "2"}, "A=1\nB=2\n"},
		{"nothing changed, odd formatting kept", "export A = \"1\"\n", map[string]string{"A": "1"}, "export A = \"1\"\n"},
		{"one changed", "A=1\nB=2\n", map[string]string{"A": "1", "B": "3"}, "A=1\nB=3\n"},
		{"missing appended in name order", "X=9\n", map[string]string{"C": "c", "A": "a"}, "X=9\nA=a\nC=c\n"},
		{"duplicates collapse to one line", "A=1\nB=2\nA=3\n", map[string]string{"A": "4", "B": "2"}, "A=4\nB=2\n"},
		{"duplicates that did not change stay", "A=1\nA=3\n", map[string]string{"A": "3"}, "A=1\nA=3\n"},
		{"value that needs quoting", "A=1\n", map[string]string{"A": "a b"}, "A=\"a b\"\n"},
		{"foreign lines and comments stay", "# c\nX=1\nA=1\n\n", map[string]string{"A": "2"}, "# c\nX=1\nA=2\n\n"},
		{"no trailing newline", "A=1", map[string]string{"A": "2"}, "A=2\n"},
		{"empty file", "", map[string]string{"A": "1"}, "A=1\n"},
		{"a value emptied", "A=1\n", map[string]string{"A": ""}, "A=\n"},
	} {
		got := mergeEnv(tc.env, sortedKeys(tc.values), tc.values, parseEnv(tc.env))
		if got != tc.want {
			t.Errorf("%s: mergeEnv(%q) = %q, want %q", tc.name, tc.env, got, tc.want)
		}
	}
}

func TestUpdateConfigWritesEveryInputAsAnEnvValueComposeReadsBackExactly(t *testing.T) {
	for name, value := range envValues {
		t.Run(name, func(t *testing.T) {
			in, stacks := installed(t, "aio-notes", nil)
			if _, err := in.UpdateConfig(context.Background(), "aio-notes", ConfigUpdate{Values: map[string]string{"DB_PASSWORD": value, "SITE_NAME": value}}); err != nil {
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

func TestConfigReadsBackWhatInstallAndUpdateWrote(t *testing.T) {
	for name, value := range envValues {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			in, stacks := installed(t, "aio-notes", map[string]string{"SITE_NAME": value})
			cfg, err := in.Config(ctx, "aio-notes")
			if err != nil {
				t.Fatal(err)
			}
			if got := configInput(t, cfg, "SITE_NAME").Value; got != value {
				t.Errorf("Config reads the installed SITE_NAME as %q, want %q", got, value)
			}
			if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": value, "WEBUI_PORT": "3100"}}); err != nil {
				t.Fatal(err)
			}
			got, err := composeDotenv(stacks.created[0].Env)
			if err != nil || got["SITE_NAME"] != value || got["WEBUI_PORT"] != "3100" {
				t.Errorf("an update that sends the value back changed it: %q, %v\n%s", got["SITE_NAME"], err, stacks.created[0].Env)
			}
		})
	}
}

func TestAnExistingSingleQuotedEnvIsReadBackExactlyAndRewrittenInTheNewForm(t *testing.T) {
	ctx := context.Background()
	in, stacks := installed(t, "aio-notes", nil)
	stacks.created[0].Env = strings.Replace(stacks.created[0].Env, "SITE_NAME=Notes", `SITE_NAME='my $site \n #1'`, 1)
	cfg, err := in.Config(ctx, "aio-notes")
	if err != nil {
		t.Fatal(err)
	}
	if got := configInput(t, cfg, "SITE_NAME").Value; got != `my $site \n #1` {
		t.Fatalf("Config reads the single-quoted SITE_NAME as %q", got)
	}
	before := stacks.created[0].Env
	if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": `my $site \n #1`}}); err != nil || stacks.created[0].Env != before {
		t.Errorf("sending the unchanged value back rewrote the .env (%v):\n%s", err, stacks.created[0].Env)
	}
	if _, err := in.UpdateConfig(ctx, "aio-notes", ConfigUpdate{Values: map[string]string{"SITE_NAME": `my $site \`}}); err != nil {
		t.Fatal(err)
	}
	got, err := composeDotenv(stacks.created[0].Env)
	if err != nil || got["SITE_NAME"] != `my $site \` {
		t.Errorf("Compose reads SITE_NAME as %q, %v\n%s", got["SITE_NAME"], err, stacks.created[0].Env)
	}
}
