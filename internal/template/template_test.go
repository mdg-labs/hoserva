package template

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
)

var update = flag.Bool("update", false, "rewrite schema/v1.json from the Go definition")

const fixtureDir = "testdata/catalog"

func readFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// catalogWith writes a one-template catalog whose compose.yaml is the
// fixture with old replaced by new, and returns its directory.
func catalogWith(t *testing.T, old, replacement string) string {
	t.Helper()
	src := readFixture(t)
	if !strings.Contains(src, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return catalogFrom(t, "jellyfin", strings.Replace(src, old, replacement, 1))
}

func catalogFrom(t *testing.T, id, compose string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, ComposeFile), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	icon, err := os.ReadFile(filepath.Join(fixtureDir, "jellyfin", "icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id, "icon.svg"), icon, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func lintStrings(t *testing.T, dir string) []string {
	t.Helper()
	found, err := Lint(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(found))
	for i, f := range found {
		out[i] = f.String()
	}
	return out
}

func TestSchemaFileIsCurrent(t *testing.T) {
	want, err := JSONSchema(1)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join("schema", "v1.json")
	if *update {
		if err := os.WriteFile(file, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the schema generated from the Go definition; regenerate it with `go test ./internal/template -run TestSchemaFileIsCurrent -update` and explain the diff in the commit", file)
	}
	if !json.Valid(got) {
		t.Fatalf("%s is not valid JSON", file)
	}
}

func TestVersion1TemplateValidatesWithCurrentValidator(t *testing.T) {
	got := lintStrings(t, fixtureDir)
	if len(got) != 0 {
		t.Fatalf("the version-1 fixture must lint clean, got:\n%s", strings.Join(got, "\n"))
	}
	tpl, issues := Parse([]byte(readFixture(t)))
	if tpl == nil {
		t.Fatalf("Parse: %v", issues)
	}
	b := tpl.Block
	if b.Schema != 1 || b.ID != "jellyfin" || b.Revision != 1 || b.Title != "Jellyfin" {
		t.Errorf("block fields: %+v", b)
	}
	if in := b.Inputs["APPDATA"]; in.Kind != KindPath || in.Role != RoleAppdata || in.Default != "/mnt/cache/appdata" {
		t.Errorf("APPDATA input: %+v", in)
	}
	if in := b.Inputs["TRANSCODE_GPU"]; in.Kind != KindDevice || in.Role != RoleGPU {
		t.Errorf("TRANSCODE_GPU input: %+v", in)
	}
	if got := SupportedVersions(); len(got) == 0 || got[0] != 1 {
		t.Errorf("SupportedVersions = %v, want to include 1", got)
	}
}

func TestLintRefusesControlCharactersInTemplateText(t *testing.T) {
	cases := []struct{ name, old, replacement, want string }{
		{"title", "  title: Jellyfin\n", "  title: \"Jelly\\x1bfin\"\n", "x-hoserva.title: holds a control character"},
		{"title with a line break", "  title: Jellyfin\n", "  title: \"Jelly\\nfin\"\n", "x-hoserva.title: holds a control character"},
		{"docs address", "  docs: https://docs.linuxserver.io/images/docker-jellyfin/\n", "  docs: \"https://example.com/\\x1b[2K\"\n", "x-hoserva.docs: holds a control character"},
		{"input label", "label: Media library", "label: \"Media\\x1b[2K\"", "x-hoserva.inputs.MEDIA.label: holds a control character"},
		{"input description", "label: Media library", "label: Media library, description: \"a\\x9bb\"", "x-hoserva.inputs.MEDIA.description: holds a control character"},
		{"input default", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    NOTE: { kind: string, default: \"a\\x1b[2Kb\" }", "x-hoserva.inputs.NOTE.default: holds a control character"},
		{"input default with a C1 character", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    NOTE: { kind: string, default: \"a\\u009bb\" }", "x-hoserva.inputs.NOTE.default: holds a control character"},
		{"service name", "services:\n  jellyfin:", "services:\n  \"jelly\\x1bfin\":", "services: holds a key with a control character"},
		{"environment value", "TZ: ${TZ}", "TZ: ${TZ}\n      EXTRA: \"a\\x1b[2Kb\"", "services.jellyfin.environment.EXTRA: holds a control character"},
		{"environment name", "TZ: ${TZ}", "TZ: ${TZ}\n      \"EX\\x1bTRA\": a", "services.jellyfin.environment: holds a key with a control character"},
		{"volume", "- ${APPDATA}/jellyfin:/config", "- \"${APPDATA}/jellyfin:/con\\x1b[2Kfig\"", "services.jellyfin.volumes.0: holds a control character"},
		{"extension key", "    restart: unless-stopped\n", "    restart: unless-stopped\n    \"x-a\\x1bb\": 1\n", "services.jellyfin: holds a key with a control character"},
		{"top-level extension key", "\nx-hoserva:", "\n\"x-a\\x1bb\": 1\nx-hoserva:", "holds a top-level key with a control character"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintStrings(t, catalogWith(t, tc.old, tc.replacement))
			if joined := strings.Join(got, "\n"); !strings.Contains(joined, tc.want) {
				t.Fatalf("findings do not mention %q:\n%s", tc.want, joined)
			}
		})
	}
}

func TestLintAcceptsLineBreaksAndTabsInComposeStrings(t *testing.T) {
	dir := catalogWith(t, "TZ: ${TZ}", "TZ: ${TZ}\n      EXTRA: \"a\\tb\\nc\"")
	if got := lintStrings(t, dir); len(got) != 0 {
		t.Fatalf("lint refused a line break and a tab in a Compose string:\n%s", strings.Join(got, "\n"))
	}
}

func TestCheckComposeRefusesControlCharactersInAnyComposeDocument(t *testing.T) {
	compose := map[string]any{"services": map[string]any{"web": map[string]any{
		"image":       "example/web:1",
		"environment": []any{"A=b\x1b[2K"},
	}}}
	issues := CheckCompose(compose)
	if len(issues) != 1 || strings.Join(issues[0].Path, ".") != "services.web.environment.0" || !strings.Contains(issues[0].Message, "control character") {
		t.Fatalf("CheckCompose = %v, want one control-character issue on services.web.environment.0", issues)
	}
	compose["services"].(map[string]any)["web"].(map[string]any)["environment"] = []any{"A=b\tc\nd"}
	if issues := CheckCompose(compose); len(issues) != 0 {
		t.Fatalf("CheckCompose refused a tab and a line break: %v", issues)
	}
}

func TestLintReportsMalformedTemplates(t *testing.T) {
	cases := []struct {
		name, old, replacement string
		want                   string
	}{
		{"missing title", "  title: Jellyfin\n", "", "missing property 'title'"},
		{"wrong kind", "WEBUI_PORT: { kind: port,", "WEBUI_PORT: { kind: number,", "x-hoserva.inputs.WEBUI_PORT.kind"},
		{"unknown field", "  revision: 1\n", "  revision: 1\n  colour: blue\n", "additional properties 'colour' not allowed"},
		{"revision zero", "  revision: 1\n", "  revision: 0\n", "x-hoserva.revision"},
		{"unsupported schema version", "  schema: 1\n", "  schema: 2\n", "schema version 2 is not supported; this Hoserva reads 1"},
		{"missing schema version", "  schema: 1\n", "", "x-hoserva.schema: is required"},
		{"path without role", "{ kind: path, role: appdata, default", "{ kind: path, default", "missing property 'role'"},
		{"role on a string", "TZ:         { kind: timezone }", "TZ:         { kind: timezone, role: appdata }", "x-hoserva.inputs.TZ"},
		{"port out of range", "{ kind: port, default: 8096 }", "{ kind: port, default: 70000 }", "x-hoserva.inputs.WEBUI_PORT.default"},
		{"secret with a default", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    TOKEN: { kind: secret, default: abc }", "x-hoserva.inputs.TOKEN"},
		{"optional port", "{ kind: port, default: 8096 }", "{ kind: port, default: 8096, optional: true }", "x-hoserva.inputs.WEBUI_PORT"},
		{"optional path", "{ kind: path, role: media, default: /mnt/user/media,", "{ kind: path, role: media, optional: true, default: /mnt/user/media,", "x-hoserva.inputs.MEDIA"},
		{"optional timezone", "TZ:         { kind: timezone }", "TZ:         { kind: timezone, optional: true }", "x-hoserva.inputs.TZ"},
		{"optional secret", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    TOKEN: { kind: secret, optional: true }", "x-hoserva.inputs.TOKEN"},
		{"optional device", "{ kind: device, role: gpu,", "{ kind: device, role: gpu, optional: true,", "x-hoserva.inputs.TRANSCODE_GPU"},
		{"optional string with a default", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    NOTE: { kind: string, optional: true, default: x }", "x-hoserva.inputs.NOTE"},
		{"optional that is not a boolean", "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    NOTE: { kind: string, optional: \"yes\" }", "x-hoserva.inputs.NOTE.optional"},
		{"device with a path role", "{ kind: device, role: gpu,", "{ kind: device, role: appdata,", "x-hoserva.inputs.TRANSCODE_GPU"},
		{"lowercase input name", "TZ:         { kind: timezone }", "tz:         { kind: timezone }", "'tz' does not match pattern"},
		{"appdata off cache", "default: /mnt/cache/appdata }", "default: /srv/appdata }", "appdata belongs on cache"},
		{"appdata escaping cache", "default: /mnt/cache/appdata }", "default: /mnt/cache/../disk1 }", "appdata belongs on cache"},
		{"media off the pool", "default: /mnt/user/media,", "default: /mnt/disk1/media,", "media belongs on the pool"},
		{"relative bind", "- ${APPDATA}/jellyfin:/config", "- ./config:/config", "is relative"},
		{"bind from a port input", "- ${MEDIA}:/data/media", "- ${WEBUI_PORT}:/data/media", "not a path input"},
		{"PUID other than 99", "PUID: \"99\"", "PUID: \"1000\"", "services.jellyfin.environment.PUID: must be \"99\""},
		{"PGID other than 100", "PGID: \"100\"", "PGID: \"10\"", "services.jellyfin.environment.PGID: must be \"100\""},
		{"undeclared variable", "TZ: ${TZ}", "TZ: ${TZ}\n      EXTRA: ${NOT_DECLARED}", "${NOT_DECLARED}, which is not declared"},
		{"unused input", "      - ${MEDIA}:/data/media\n", "", "x-hoserva.inputs.MEDIA: is declared but nothing"},
		{"undeclared variable in a nested default", "TZ: ${TZ}", "TZ: ${TZ:-${NOT_DECLARED}}", "${NOT_DECLARED}, which is not declared"},
		{"undeclared variable in a default after an escape", "TZ: ${TZ}", "TZ: ${TZ:-$$HOME/${NOT_DECLARED}}", "${NOT_DECLARED}, which is not declared"},
		{"env_file other than the stack's .env does not use the inputs", "      - ${MEDIA}:/data/media\n", "    env_file: [other.env]\n", "x-hoserva.inputs.MEDIA: is declared but nothing"},
		{"no services", "services:\n  jellyfin:", "services: {}\nunused:\n  jellyfin:", "a template needs at least one service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintStrings(t, catalogWith(t, tc.old, tc.replacement))
			if len(got) == 0 {
				t.Fatal("lint passed a malformed template")
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Fatalf("findings do not mention %q:\n%s", tc.want, joined)
			}
			if !strings.HasPrefix(got[0], "jellyfin/compose.yaml: ") {
				t.Errorf("finding does not name its file: %q", got[0])
			}
		})
	}
}

func TestLintRefusesAnInputNamedLikeAReservedDockerVariable(t *testing.T) {
	for _, name := range container.ReservedEnvNames() {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(readFixture(t), "TZ: ${TZ}", "TZ: ${TZ}\n      EXTRA: ${"+name+"}", 1)
			src = strings.Replace(src, "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    "+name+": { kind: string, default: x }", 1)
			got := strings.Join(lintStrings(t, catalogFrom(t, "jellyfin", src)), "\n")
			want := "x-hoserva.inputs." + name + ": " + name + " is a variable Docker takes from the daemon's environment"
			if !strings.Contains(got, want) {
				t.Fatalf("findings do not mention %q:\n%s", want, got)
			}
		})
	}
	src := strings.Replace(readFixture(t), "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    MYPATH: { kind: string, optional: true }", 1)
	if got := strings.Join(lintStrings(t, catalogFrom(t, "jellyfin", src)), "\n"); strings.Contains(got, "Docker takes from") {
		t.Errorf("an input that only contains a reserved name was refused:\n%s", got)
	}
}

func TestLintAcceptsAnOptionalStringInput(t *testing.T) {
	src := readFixture(t)
	src = strings.Replace(src, "TZ: ${TZ}", "TZ: ${TZ}\n      NOTE: ${NOTE}", 1)
	src = strings.Replace(src, "TZ:         { kind: timezone }", "TZ:         { kind: timezone }\n    NOTE: { kind: string, optional: true }", 1)
	if got := lintStrings(t, catalogFrom(t, "jellyfin", src)); len(got) != 0 {
		t.Fatalf("an optional string input must lint clean, got:\n%s", strings.Join(got, "\n"))
	}
	tpl, issues := Parse([]byte(src))
	if tpl == nil {
		t.Fatal(issues)
	}
	if !tpl.Block.Inputs["NOTE"].Optional || tpl.Block.Inputs["TZ"].Optional {
		t.Errorf("Optional not read: %+v", tpl.Block.Inputs)
	}
}

func TestLintFindingsCarryLineNumbers(t *testing.T) {
	got := lintStrings(t, catalogWith(t, "WEBUI_PORT: { kind: port,", "WEBUI_PORT: { kind: number,"))
	if len(got) == 0 || !strings.Contains(got[0], "line 28: x-hoserva.inputs.WEBUI_PORT.kind") {
		t.Fatalf("got %q", got)
	}
}

func TestLintNotYAMLAndMissingBlock(t *testing.T) {
	if got := lintStrings(t, catalogFrom(t, "app", "services: [")); len(got) != 1 || !strings.Contains(got[0], "not valid YAML") {
		t.Errorf("malformed YAML: %q", got)
	}
	if got := lintStrings(t, catalogFrom(t, "app", "services:\n  app:\n    image: x:1\n")); len(got) != 1 || !strings.Contains(got[0], "no x-hoserva block") {
		t.Errorf("no block: %q", got)
	}
}

func TestLintDirectoryConventions(t *testing.T) {
	src := readFixture(t)

	got := lintStrings(t, catalogFrom(t, "plex", src))
	if !strings.Contains(strings.Join(got, "\n"), `is "jellyfin" but the directory is named "plex"`) {
		t.Errorf("id/directory mismatch not reported: %q", got)
	}

	dir := catalogFrom(t, "jellyfin", src)
	if err := os.Remove(filepath.Join(dir, "jellyfin", "icon.svg")); err != nil {
		t.Fatal(err)
	}
	if got := lintStrings(t, dir); len(got) != 1 || !strings.Contains(got[0], `names "icon.svg", which is not a file`) {
		t.Errorf("missing icon: %q", got)
	}

	dir = catalogFrom(t, "jellyfin", src)
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lintStrings(t, dir); len(got) != 1 || got[0] != "empty/compose.yaml: compose.yaml is missing" {
		t.Errorf("missing compose.yaml: %q", got)
	}

	if got := lintStrings(t, t.TempDir()); len(got) != 1 || !strings.Contains(got[0], "no template directories found") {
		t.Errorf("empty catalog: %q", got)
	}

	if _, err := Lint(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("an unreadable catalog directory must be an error, not a clean lint")
	}
}

func TestEveryFindingIsSortedAndStable(t *testing.T) {
	dir := catalogWith(t, "PUID: \"99\"", "PUID: \"1\"")
	first := lintStrings(t, dir)
	second := lintStrings(t, dir)
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Errorf("unstable output:\n%q\n%q", first, second)
	}
}

func TestNestedDefaultCountsAsAUseOfTheInnerInput(t *testing.T) {
	dir := catalogWith(t, "      - ${MEDIA}:/data/media\n", "      - ${APPDATA:-${MEDIA}}/media:/data/media\n")
	if got := lintStrings(t, dir); len(got) != 0 {
		t.Fatalf("an input used only inside a nested default is used, got:\n%s", strings.Join(got, "\n"))
	}
}

func TestReferencesParsesInterpolationForms(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"${A:-${B}}", "A B"},
		{"${A:-${B:-${C}}}/x", "A B C"},
		{"${A:-x}/${B}", "A B"},
		{"$A/$B", "A B"},
		{"$$A ${B}", "B"},
		{"${A:-$$B}", "A"},
		{"${A:-$$}/${B}", "A B"},
		{"${A:-${B}", "A B"},
		{"${} $ ${1}", ""},
	} {
		if got := strings.Join(references(tc.in), " "); got != tc.want {
			t.Errorf("references(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestInputsReachingAServiceOnlyThroughTheStackEnvFileAreUsed(t *testing.T) {
	for _, tc := range []struct {
		name, envFile string
		clean         bool
	}{
		{"env_file .env as a string", "    env_file: .env\n", true},
		{"env_file ./.env as a string", "    env_file: ./.env\n", true},
		{"env_file .env in a list", "    env_file:\n      - .env\n", true},
		{"env_file .env in the long form", "    env_file:\n      - path: .env\n        required: false\n", true},
		{"env_file of another file does not count", "    env_file: other.env\n", false},
		{"env_file of a parent directory's .env does not count", "    env_file: ../.env\n", false},
		{"env_file .env.local does not count", "    env_file: .env.local\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(readFixture(t), "    restart: unless-stopped\n", tc.envFile, 1)
			src = strings.Replace(src, "      - ${MEDIA}:/data/media\n", "", 1)
			got := lintStrings(t, catalogFrom(t, "jellyfin", src))
			if tc.clean && len(got) != 0 {
				t.Fatalf("the stack's .env carries every input, got:\n%s", strings.Join(got, "\n"))
			}
			if !tc.clean && !strings.Contains(strings.Join(got, "\n"), "x-hoserva.inputs.MEDIA: is declared but nothing") {
				t.Fatalf("MEDIA is used by nothing, got:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}

func TestEnvFileStillHoldsReferencesToDeclaredInputs(t *testing.T) {
	dir := catalogWith(t, "    restart: unless-stopped\n", "    env_file: .env\n    labels:\n      x: ${NOT_DECLARED}\n")
	if got := strings.Join(lintStrings(t, dir), "\n"); !strings.Contains(got, "${NOT_DECLARED}, which is not declared") {
		t.Fatalf("got %q", got)
	}
}

func TestLintReportsSymlinkedAndUnreadableEntries(t *testing.T) {
	dir := catalogFrom(t, "jellyfin", readFixture(t))
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink("jellyfin", filepath.Join(dir, "alias")))
	must(os.Symlink("absent", filepath.Join(dir, "dangling")))
	must(os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644))
	must(os.Symlink("README.md", filepath.Join(dir, "NOTES.md")))
	must(os.Symlink("jellyfin", filepath.Join(dir, ".hidden")))

	got := lintStrings(t, dir)
	want := []string{
		"alias: is a symbolic link, which is not followed",
		"dangling: is a symbolic link, which is not followed",
	}
	if len(got) != len(want) {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i], w) {
			t.Errorf("finding %d = %q, want prefix %q", i, got[i], w)
		}
	}
}

func TestLintOfOnlyASymlinkedTemplateFails(t *testing.T) {
	real := catalogFrom(t, "jellyfin", readFixture(t))
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(real, "jellyfin"), filepath.Join(dir, "jellyfin")); err != nil {
		t.Fatal(err)
	}
	got := lintStrings(t, dir)
	if len(got) != 1 || !strings.HasPrefix(got[0], "jellyfin: is a symbolic link") {
		t.Fatalf("got %q", got)
	}
}

// withMetadata is the fixture with extra x-hoserva lines before its webui
// line, written as a one-template catalog.
func withMetadata(t *testing.T, extra string) string {
	t.Helper()
	return catalogWith(t, "  webui:", extra+"  webui:")
}

func writeShot(t *testing.T, dir, rel string, size int) {
	t.Helper()
	p := filepath.Join(dir, "jellyfin", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLintAcceptsTheCatalogMetadata(t *testing.T) {
	dir := withMetadata(t, `  maintainer: LinuxServer.io
  description: |-
    First paragraph.

    Second paragraph.
  screenshots: [screenshots/library.png, player.webp, a/b/c.JPG.jpeg]
  links:
    project: https://jellyfin.org/
    support: https://jellyfin.org/docs/general/getting-help/?q=a
    donate: https://opencollective.com/jellyfin
`)
	for _, rel := range []string{"screenshots/library.png", "player.webp", "a/b/c.JPG.jpeg"} {
		writeShot(t, dir, rel, 10)
	}
	if got := lintStrings(t, dir); len(got) != 0 {
		t.Fatalf("lint refused valid metadata:\n%s", strings.Join(got, "\n"))
	}
	tpl, _ := Parse([]byte(readFile(t, filepath.Join(dir, "jellyfin", ComposeFile))))
	if tpl == nil || tpl.Block.Maintainer != "LinuxServer.io" || tpl.Block.Links.Donate != "https://opencollective.com/jellyfin" ||
		len(tpl.Block.Screenshots) != 3 || !strings.Contains(tpl.Block.Description, "\n\nSecond") {
		t.Fatalf("block = %+v", tpl)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLintRefusesInvalidCatalogMetadata(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	cases := []struct{ name, extra, want string }{
		{"empty maintainer", "  maintainer: \"\"\n", "x-hoserva.maintainer"},
		{"blank maintainer", "  maintainer: \"   \"\n", "x-hoserva.maintainer: is blank"},
		{"long maintainer", "  maintainer: " + long(101) + "\n", "x-hoserva.maintainer"},
		{"maintainer with a control character", "  maintainer: \"a\\x07b\"\n", "x-hoserva.maintainer: holds a control character"},
		{"maintainer with a line break", "  maintainer: \"a\\nb\"\n", "x-hoserva.maintainer: holds a control character"},
		{"long description", "  description: " + long(2001) + "\n", "x-hoserva.description"},
		{"blank description", "  description: \"  \\n \"\n", "x-hoserva.description: is blank"},
		{"description with a control character", "  description: \"a\\x1bb\"\n", "x-hoserva.description: holds a control character"},
		{"http link", "  links: { project: http://example.com/ }\n", "x-hoserva.links.project"},
		{"javascript link", "  links: { support: \"javascript:alert(1)\" }\n", "x-hoserva.links.support"},
		{"data link", "  links: { donate: \"data:text/html,x\" }\n", "x-hoserva.links.donate"},
		{"scheme-relative link", "  links: { project: //example.com/ }\n", "x-hoserva.links.project"},
		{"link without a host", "  links: { project: \"https://\" }\n", "x-hoserva.links.project: has no host"},
		{"link with only a port", "  links: { project: \"https://:443/x\" }\n", "x-hoserva.links.project: has no host"},
		{"link with credentials", "  links: { project: \"https://user:pw@example.com/\" }\n", "x-hoserva.links.project: must not carry a user name or password"},
		{"link with a space", "  links: { project: \"https://example.com/a b\" }\n", "x-hoserva.links.project: holds whitespace"},
		{"link with a control character", "  links: { project: \"https://example.com/a\\x07\" }\n", "x-hoserva.links.project"},
		{"long link", "  links: { project: \"https://example.com/" + long(2040) + "\" }\n", "x-hoserva.links.project"},
		{"unknown link", "  links: { homepage: https://example.com/ }\n", "additional properties 'homepage' not allowed"},
		{"no screenshots listed", "  screenshots: []\n", "x-hoserva.screenshots"},
		{"parent directory", "  screenshots: [../x.png]\n", "x-hoserva.screenshots.0"},
		{"parent directory in the middle", "  screenshots: [a/../b.png]\n", "x-hoserva.screenshots.0"},
		{"absolute path", "  screenshots: [/etc/x.png]\n", "x-hoserva.screenshots.0"},
		{"hidden file", "  screenshots: [.x.png]\n", "x-hoserva.screenshots.0"},
		{"hidden directory", "  screenshots: [.git/x.png]\n", "x-hoserva.screenshots.0"},
		{"empty segment", "  screenshots: [a//b.png]\n", "x-hoserva.screenshots.0"},
		{"backslash", "  screenshots: ['a\\b.png']\n", "x-hoserva.screenshots.0"},
		{"svg", "  screenshots: [x.svg]\n", "x-hoserva.screenshots.0"},
		{"gif", "  screenshots: [x.gif]\n", "x-hoserva.screenshots.0"},
		{"no extension", "  screenshots: [x]\n", "x-hoserva.screenshots.0"},
		{"duplicates", "  screenshots: [x.png, x.png]\n", "x-hoserva.screenshots"},
		{"nine", "  screenshots: [1.png, 2.png, 3.png, 4.png, 5.png, 6.png, 7.png, 8.png, 9.png]\n", "x-hoserva.screenshots"},
		{"long path", "  screenshots: [" + long(201) + ".png]\n", "x-hoserva.screenshots.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintStrings(t, withMetadata(t, tc.extra))
			if len(got) == 0 {
				t.Fatal("lint passed invalid metadata")
			}
			if joined := strings.Join(got, "\n"); !strings.Contains(joined, tc.want) {
				t.Fatalf("findings do not mention %q:\n%s", tc.want, joined)
			}
		})
	}
}

func TestLintRefusesAScreenshotThatIsNotAPlainFileInTheTemplateDirectory(t *testing.T) {
	outside := t.TempDir()
	writeShotAt := func(t *testing.T, p string, size int) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]func(t *testing.T, tpl string){
		"missing": func(t *testing.T, tpl string) {},
		"a directory": func(t *testing.T, tpl string) {
			if err := os.MkdirAll(filepath.Join(tpl, "shots", "x.png"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"a symlink to a file": func(t *testing.T, tpl string) {
			writeShotAt(t, filepath.Join(outside, "x.png"), 10)
			if err := os.MkdirAll(filepath.Join(tpl, "shots"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "x.png"), filepath.Join(tpl, "shots", "x.png")); err != nil {
				t.Fatal(err)
			}
		},
		"inside a symlinked directory": func(t *testing.T, tpl string) {
			writeShotAt(t, filepath.Join(outside, "x.png"), 10)
			if err := os.Symlink(outside, filepath.Join(tpl, "shots")); err != nil {
				t.Fatal(err)
			}
		},
		"over the size cap": func(t *testing.T, tpl string) {
			writeShotAt(t, filepath.Join(tpl, "shots", "x.png"), maxScreenshotBytes+1)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := withMetadata(t, "  screenshots: [shots/x.png]\n")
			setup(t, filepath.Join(dir, "jellyfin"))
			got := lintStrings(t, dir)
			if len(got) != 1 || !strings.Contains(got[0], "x-hoserva.screenshots.0: names \"shots/x.png\"") {
				t.Fatalf("findings = %q, want one about the screenshot", got)
			}
		})
	}
}
