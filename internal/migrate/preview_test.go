package migrate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/store"
)

const (
	tmplDir     = "config/plugins/dockerMan/templates-user/"
	composeFile = "config/plugins/compose.manager/projects/stack/compose.yaml"
	// notesSecret is the value of a masked variable in the fixture's notes
	// template, and notesImage and notesPath other settings of it.
	notesSecret = "fixture-not-a-secret"
	notesImage  = "fixture/notes:1.0"
	notesPath   = "/mnt/user/appdata/notes"
)

func entryByFile(t *testing.T, r *Report, file string) TemplateEntry {
	t.Helper()
	for _, e := range r.Import.Templates {
		if templateKey(e) == file {
			return e
		}
	}
	t.Fatalf("no template %s in %+v", file, r.Import.Templates)
	return TemplateEntry{}
}

func warningOf(p *Preview, class string) (PreviewWarning, bool) {
	for _, w := range p.Warnings {
		if w.Class == class {
			return w, true
		}
	}
	return PreviewWarning{}, false
}

// scanned runs a scan through the Service, so the session holds the report and
// the zip, and returns the Service.
func scanned(t *testing.T, mutate func(map[string][]byte)) *Service {
	t.Helper()
	s, _, files := newService(t, inventoryVariant)
	if mutate != nil {
		mutate(files)
	}
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	return s
}

// The fixture's installed templates are three that convert cleanly (photos,
// mediaserver and syncer) and two with warnings (gateway on the custom br0
// network, notes with a field the converter does not carry over); the two
// template-only ones are converted for the preview and left out of the counts.
func TestPreview_CountsCoverInstalledTemplatesOnly(t *testing.T) {
	r := scanInventory(t, nil)
	c := r.Import.TemplateCounts
	if c == nil || c.Clean != 3 || c.WithWarnings != 2 || c.Failed != 0 || c.TemplateOnly != 2 || c.AllTemplates || c.ComposeProjects != 1 {
		t.Fatalf("counts = %+v, want 3 clean, 2 with warnings, 2 template only, 1 project", c)
	}
	requireRow(t, r, CheckTemplates, StatusInfo, "Of the 5 installed templates, 3 convert cleanly and 2 with warnings (Q36)", "2 template-only templates are converted for the preview and are not counted")
	requireRow(t, r, CheckTemplates, StatusInfo, "gateway", "1 warning", "missing_network")
	requireRow(t, r, CheckTemplates, StatusInfo, "notes", "1 warning", "untranslated_field")
	refuteRow(t, r, CheckTemplates, StatusInfo, "unused-tool", "to review")
	requireRow(t, r, CheckContainers, StatusInfo, "1 Compose Manager project previewed with its own compose.yaml and not converted")

	photos := entryByFile(t, r, "my-Photos.xml")
	if photos.Class != ClassAutostart || photos.Outcome.Status != PreviewClean || photos.Outcome.ActionWarnings() != 0 {
		t.Errorf("photos = %+v, want a clean autostart conversion", photos.Outcome)
	}
	hasLayer := false
	for _, w := range photos.Outcome.Warnings {
		hasLayer = hasLayer || w == "writable_layer"
	}
	if !hasLayer {
		t.Errorf("photos warnings = %v: a clean conversion lacks doc 04 section 5's writable-layer warning, which does not count against clean (Q36)", photos.Outcome.Warnings)
	}
	if gw := entryByFile(t, r, "my-gateway.xml"); gw.Outcome.Status != PreviewWarnings || gw.Outcome.ActionWarnings() != 1 {
		t.Errorf("gateway = %+v", gw.Outcome)
	}
}

func TestPreview_TemplateOnlyIsPreviewedAndNotCounted(t *testing.T) {
	s := scanned(t, nil)
	st, err := s.State(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	e := entryByFile(t, st.Report, "my-unused-tool.xml")
	if e.Class != ClassTemplateOnly || e.Counted() || e.Outcome.Status != PreviewWarnings {
		t.Fatalf("unused-tool = %+v, want a template-only entry that is not counted", e)
	}
	if c := st.Report.Import.TemplateCounts; c.Clean+c.WithWarnings+c.Failed != 5 {
		t.Errorf("counts = %+v: the two template-only templates are in them", c)
	}
	v, err := s.Template(ctx0, "my-unused-tool.xml")
	if err != nil || v.Preview.Compose == "" || !strings.Contains(v.Preview.Source, "<Name>unused-tool</Name>") {
		t.Errorf("Template(my-unused-tool.xml) = %+v, %v, want a built preview of the uncounted template", v, err)
	}
}

// The preview is built on request, with the capture's networks as the
// converter's input, so the command that creates a custom network is exact.
func TestPreview_CustomNetworkGetsTheExactCommandWhenTheCaptureHoldsIt(t *testing.T) {
	v, err := scanned(t, nil).Template(ctx0, "my-gateway.xml")
	if err != nil {
		t.Fatal(err)
	}
	w, ok := warningOf(v.Preview, "missing_network")
	if !ok {
		t.Fatal("gateway has no missing_network warning")
	}
	const want = "docker network create -d ipvlan --subnet 192.168.50.0/24 --gateway 192.168.50.1 -o parent=ens20 -o ipvlan_mode=l2 br0"
	if w.Command != want {
		t.Errorf("command = %q, want %q", w.Command, want)
	}
	if strings.Contains(w.Command, "<") || strings.Contains(w.Message, "<PLACEHOLDER>") {
		t.Errorf("an exact command carries a placeholder: %+v", w)
	}
	if v.Preview.Compose == "" || !strings.Contains(v.Preview.Source, "<Name>gateway</Name>") {
		t.Errorf("preview = %+v, want its source and Compose", v.Preview)
	}

	v, err = scanned(t, func(f map[string][]byte) { delete(f, "config/hoserva/networks.json") }).Template(ctx0, "my-gateway.xml")
	if err != nil {
		t.Fatal(err)
	}
	w, _ = warningOf(v.Preview, "missing_network")
	if !strings.Contains(w.Command, "<SUBNET>") || !strings.Contains(w.Command, "<INTERFACE>") {
		t.Errorf("without the capture's networks the command is %q, want placeholders", w.Command)
	}
}

func TestPreview_WithoutACaptureTheCountsCoverEveryTemplateAndSaySo(t *testing.T) {
	r := scanInventory(t, func(f map[string][]byte) { delete(f, "config/hoserva/containers.json") })
	c := r.Import.TemplateCounts
	if c.TemplateOnly != 0 || !c.AllTemplates || c.Clean+c.WithWarnings+c.Failed != 7 {
		t.Fatalf("counts = %+v, want all 7 templates counted", c)
	}
	for _, e := range r.Import.Templates {
		if e.Class != ClassUnknown || !e.Counted() {
			t.Errorf("%s is %s and counted=%v without a capture", e.Name, e.Class, e.Counted())
		}
	}
	requireRow(t, r, CheckTemplates, StatusInfo, "Of all 7 templates", "these counts cover every template")
}

// A template the converter cannot read is reported by name and counted as
// failed, never as clean and never left out.
func TestPreview_AFailedConversionIsReportedNotCountedClean(t *testing.T) {
	mutate := func(f map[string][]byte) {
		big := `<?xml version="1.0"?><Container version="2"><Name>dbtool</Name><Repository>fixture/dbtool:1</Repository><Overview>` +
			strings.Repeat("x", 60*1024) + `</Overview></Container>`
		f[tmplDir+"my-dbtool.xml"] = []byte(big)
	}
	s := scanned(t, mutate)
	st, _ := s.State(ctx0)
	r := st.Report
	c := r.Import.TemplateCounts
	if c.Failed != 1 || c.Clean != 3 || c.WithWarnings != 2 {
		t.Fatalf("counts = %+v, want the failed template apart from 3 clean and 2 with warnings", c)
	}
	e := entryByFile(t, r, "my-dbtool.xml")
	if e.Class != ClassRunning || e.Outcome.Status != PreviewFailed || e.Outcome.Failure != FailureTooLarge {
		t.Errorf("dbtool = %+v, want a failed running template", e.Outcome)
	}
	requireRow(t, r, CheckTemplates, StatusWarn, "dbtool", "could not read the template", "larger than")
	requireRow(t, r, CheckTemplates, StatusInfo, "and 1 could not be converted")
	if r.Verdict != VerdictGoWithWarnings {
		t.Errorf("verdict = %s, want go with warnings", r.Verdict)
	}
	v, err := s.Template(ctx0, "my-dbtool.xml")
	if err != nil || v.Preview.Failure != FailureTooLarge || v.Preview.Error == "" || v.Preview.Compose != "" {
		t.Errorf("Template(my-dbtool.xml) = %+v, %v, want the failure and no Compose", v, err)
	}
}

func TestPreview_ComposeProjectsAreReviewedAsTheyAreAndNotConverted(t *testing.T) {
	s := scanned(t, nil)
	st, _ := s.State(ctx0)
	if o := st.Report.Import.ComposeProjects[0].Outcome; o == nil || o.Status != PreviewOnly || len(o.Warnings) != 0 {
		t.Fatalf("project outcome = %+v, want it previewed as it is", o)
	}
	if c := st.Report.Import.TemplateCounts; c.Clean+c.WithWarnings+c.Failed != 5 || c.ComposeProjects != 1 {
		t.Errorf("counts = %+v: a project is counted apart from the templates", c)
	}
	v, err := s.Template(ctx0, "stack")
	if err != nil || v.Kind != KindComposeProject || v.Preview.Compose != v.Preview.Source || !strings.Contains(v.Preview.Source, "container_name: stack-web") {
		t.Fatalf("Template(stack) = %+v, %v, want the compose.yaml as it is", v, err)
	}

	v, err = scanned(t, func(f map[string][]byte) {
		f[composeFile] = []byte("services:\n  web:\n    image: x\n    privileged: true\n    volumes:\n      - /boot:/boot\n")
	}).Template(ctx0, "stack")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, pr := range v.Preview.Privileges {
		kinds[pr.Kind] = true
	}
	if !kinds["privileged"] || !kinds["host_path"] {
		t.Errorf("privileges = %+v, want the same review a converted stack gets", v.Preview.Privileges)
	}

	r := scanInventory(t, func(f map[string][]byte) { f[composeFile] = []byte("services: [unclosed\n") })
	if o := r.Import.ComposeProjects[0].Outcome; o.Status != PreviewFailed || o.Failure != FailureInvalidYAML || r.Import.TemplateCounts.ComposeProjects != 0 {
		t.Errorf("an unreadable compose.yaml = %+v, counts %+v", o, r.Import.TemplateCounts)
	}
	requireRow(t, r, CheckContainers, StatusWarn, "stack", "compose.yaml is not valid YAML")

	r = scanInventory(t, func(f map[string][]byte) {
		f[composeFile] = []byte("services:\n  web:\n    image: x\n---\nservices:\n  evil:\n    privileged: true\n")
	})
	requireRow(t, r, CheckContainers, StatusWarn, "stack", "more than one YAML document")

	for name, body := range map[string]string{
		"empty":            "",
		"comment-only":     "# nothing yet\n",
		"without services": "networks:\n  front: {}\n",
		"include-only":     "include:\n  - other.yaml\n",
		"empty services":   "services: {}\n",
	} {
		r = scanInventory(t, func(f map[string][]byte) { f[composeFile] = []byte(body) })
		if o := r.Import.ComposeProjects[0].Outcome; o.Status != PreviewFailed || o.Failure != FailureNoServices || r.Import.TemplateCounts.ComposeProjects != 0 {
			t.Errorf("a %s compose.yaml = %+v, counts %+v, want it failed and not counted", name, o, r.Import.TemplateCounts)
		}
		requireRow(t, r, CheckContainers, StatusWarn, "stack", "declares no services")
	}
}

// A project is held to the keys a template may use, so its privileges are
// computed from everything its compose.yaml does: a file that uses a key the
// template allow list refuses is not previewed.
func TestPreview_AProjectWithAKeyTheAllowListRefusesIsFailed(t *testing.T) {
	cases := map[string]string{
		"secrets from a file":         "services:\n  web:\n    image: x\n    secrets: [token]\nsecrets:\n  token:\n    file: /etc/hoserva/key\n",
		"configs from a file":         "services:\n  web:\n    image: x\n    configs: [conf]\nconfigs:\n  conf:\n    file: /var/lib/hoserva/hoserva.db\n",
		"include":                     "include:\n  - other.yaml\nservices:\n  web:\n    image: x\n",
		"build":                       "services:\n  web:\n    build: /root\n",
		"env_file of another file":    "services:\n  web:\n    image: x\n    env_file: /etc/hoserva/key\n",
		"volumes_from a container":    "services:\n  web:\n    image: x\n    volumes_from: [\"container:other\"]\n",
		"volumes_from a bare name":    "services:\n  web:\n    image: x\n    volumes_from: [other]\n",
		"extends another file":        "services:\n  web:\n    extends:\n      file: other.yaml\n      service: web\n",
		"ipc host":                    "services:\n  web:\n    image: x\n    ipc: host\n",
		"network_mode container":      "services:\n  web:\n    image: x\n    network_mode: \"container:other\"\n",
		"service on the host network": "services:\n  web:\n    image: x\n    networks: [host]\n",
		"a host network definition":   "services:\n  web:\n    image: x\nnetworks:\n  host: {}\n",
		"gpus":                        "services:\n  web:\n    image: x\n    gpus: all\n",
		"post_start":                  "services:\n  web:\n    image: x\n    post_start:\n      - command: id\n",
		"uts host":                    "services:\n  web:\n    image: x\n    uts: host\n",
		"userns_mode host":            "services:\n  web:\n    image: x\n    userns_mode: host\n",
		"a volume driver":             "services:\n  web:\n    image: x\nvolumes:\n  data:\n    driver: other\n",
		"an unknown top-level key":    "services:\n  web:\n    image: x\nmodels: {}\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := previewCompose([]byte(body))
			o := p.Outcome(true)
			if o.Status != PreviewFailed || o.Failure != FailureNotAccepted || o.FailureText() == "" {
				t.Errorf("outcome = %+v, want a failed preview", o)
			}
			if p.Error == "" || p.Compose != "" || p.Source != body || len(p.Privileges) != 0 {
				t.Errorf("preview = %+v, want the reason, the source and no Compose or privileges", p)
			}
			r := scanInventory(t, func(f map[string][]byte) { f[composeFile] = []byte(body) })
			if o := r.Import.ComposeProjects[0].Outcome; o.Status != PreviewFailed || o.Failure != FailureNotAccepted || r.Import.TemplateCounts.ComposeProjects != 0 {
				t.Errorf("the scan's outcome = %+v, counts %+v, want it failed and not counted", o, r.Import.TemplateCounts)
			}
			requireRow(t, r, CheckContainers, StatusWarn, "stack", "compose.yaml")
		})
	}
}

// What the allow list accepts is still previewed with its privileges, and a
// service may inherit the volumes of another service of the same file.
func TestPreview_AProjectWithinTheAllowListIsStillPreviewed(t *testing.T) {
	p := previewCompose([]byte("services:\n  db:\n    image: x\n    volumes:\n      - /mnt/user/appdata/db:/data\n  web:\n    image: x\n    privileged: true\n    volumes_from: [db]\n    env_file: .env\n"))
	o := p.Outcome(true)
	if o.Status != PreviewOnly || p.Compose == "" || p.Failure != "" {
		t.Fatalf("outcome = %+v, preview %+v, want it previewed", o, p)
	}
	kinds := map[string]bool{}
	for _, pr := range p.Privileges {
		kinds[pr.Kind] = true
	}
	if !kinds["privileged"] {
		t.Errorf("privileges = %+v, want privileged", p.Privileges)
	}
}

// Host networking, the host PID namespace and the host cgroup namespace are on
// the allow list, so they are previewed and reported, not refused.
func TestPreview_HostNamespaceModesAreReportedNotRefused(t *testing.T) {
	for _, key := range []string{"network_mode", "pid", "cgroup"} {
		p := previewCompose([]byte("services:\n  web:\n    image: x\n    " + key + ": host\n"))
		if o := p.Outcome(true); o.Status != PreviewOnly || p.Failure != "" || len(p.Privileges) != 1 {
			t.Errorf("%s: outcome = %+v, preview %+v, want it previewed with one privilege", key, o, p)
		}
	}
}

// The obsolete top-level version key is ignored, and only that key: the rest of
// the file is held to the allow list as before.
func TestPreview_ATopLevelVersionKeyIsIgnored(t *testing.T) {
	ok := "version: '3'\nservices:\n  web:\n    image: x\n    privileged: true\n"
	p := previewCompose([]byte(ok))
	if o := p.Outcome(true); o.Status != PreviewOnly || p.Failure != "" || p.Error != "" {
		t.Fatalf("outcome = %+v, preview %+v, want it previewed", o, p)
	}
	if p.Compose != ok || len(p.Privileges) != 1 || p.Privileges[0].Kind != "privileged" {
		t.Errorf("preview = %+v, want the file as written and its one privilege", p)
	}
	for name, body := range map[string]string{
		"secrets":    "version: '3'\nservices:\n  web:\n    image: x\n    secrets: [t]\nsecrets:\n  t:\n    file: /f\n",
		"build":      "version: \"3.8\"\nservices:\n  web:\n    build: /root\n",
		"include":    "version: '3'\ninclude: [o.yaml]\nservices:\n  web:\n    image: x\n",
		"a service":  "version: '3'\nservices:\n  web:\n    image: x\n    ipc: host\n",
		"top-level":  "version: '3'\nservices:\n  web:\n    image: x\nmodels: {}\n",
		"no version": "services:\n  web:\n    image: x\nmodels: {}\n",
	} {
		if o := previewCompose([]byte(body)).Outcome(true); o.Status != PreviewFailed || o.Failure != FailureNotAccepted {
			t.Errorf("%s: outcome = %+v, want a failed preview", name, o)
		}
	}
}

// The failure text covers a value or a character, not only a key.
func TestPreview_TheFailureTextForARefusedValueDoesNotNameAKey(t *testing.T) {
	o := previewCompose([]byte("services:\n  web:\n    image: x\n    ipc: host\n")).Outcome(true)
	if o.Failure != FailureNotAccepted {
		t.Fatalf("outcome = %+v, want it not accepted", o)
	}
	if got, want := o.FailureText(), "compose.yaml uses what a stack of Hoserva does not accept"; got != want {
		t.Errorf("FailureText = %q, want %q", got, want)
	}
}

// The version key is exempt from the allow list, not from the text rule: a
// control character in its value refuses the project.
func TestPreview_AControlCharacterInTheVersionValueIsRefused(t *testing.T) {
	for name, version := range map[string]string{"escape": `"3\e[2K"`, "C1": `"3\u009b"`} {
		body := "version: " + version + "\nservices:\n  web:\n    image: x\n"
		p := previewCompose([]byte(body))
		if o := p.Outcome(true); o.Status != PreviewFailed || o.Failure != FailureNotAccepted || p.Compose != "" {
			t.Errorf("%s: outcome = %+v, preview %+v, want a failed preview with no Compose", name, o, p)
		}
	}
}

// The report names and counts. Neither its rows nor the session row holds a
// template's content: the fixture's masked variable, its image and its host
// path are in the zip and in the on-request preview, and in nothing the session
// keeps.
func TestPreview_TheSessionKeepsNoTemplateContent(t *testing.T) {
	s := scanned(t, nil)
	row, found, err := s.Sessions.Get(ctx0)
	if err != nil || !found {
		t.Fatalf("session row = %v, %v", found, err)
	}
	stored := string(row.Report) + row.SourceFile + row.ScanFile + row.ScanError
	for _, content := range []string{notesSecret, notesImage, notesPath + "/tailscale", "<Container", "<Repository>", "docker network create", "A template describes how a container is launched", "services:"} {
		if strings.Contains(stored, content) {
			t.Errorf("the session row holds %q", content)
		}
	}
	st, _ := s.State(ctx0)
	if md := st.Report.Markdown(); strings.Contains(md, notesSecret) || strings.Contains(md, notesImage) {
		t.Error("the downloadable report quotes a template")
	}
	v, err := s.Template(ctx0, "my-notes.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{notesSecret, notesImage} {
		if !strings.Contains(v.Preview.Source, want) {
			t.Errorf("the on-request preview source lacks %q", want)
		}
	}
	if strings.Contains(v.Preview.Compose, notesSecret) || !strings.Contains(v.Preview.Env, notesSecret) {
		t.Error("the on-request preview must keep the masked value out of its Compose and carry it in the .env the stack is created with")
	}
}

func TestService_ListNeedsNoZipButAPreviewDoes(t *testing.T) {
	s, _, files := newService(t, inventoryVariant)
	if _, err := s.Templates(ctx0); !errors.Is(err, ErrNoReport) {
		t.Fatalf("Templates before a scan = %v, want ErrNoReport", err)
	}
	if _, err := s.Template(ctx0, "my-notes.xml"); !errors.Is(err, ErrNoReport) {
		t.Fatalf("Template before a scan = %v, want ErrNoReport", err)
	}
	if err := scanNow(s, zipOf(t, files, false), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	again := &Service{Dir: s.Dir, Scanner: s.Scanner, Sessions: s.Sessions}
	list, err := again.Templates(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if list.Counts.Clean != 3 || list.Counts.WithWarnings != 2 || list.Counts.TemplateOnly != 2 || len(list.Templates) != 7 || len(list.ComposeProjects) != 1 {
		t.Errorf("list = %+v", list.Counts)
	}
	if v, err := again.Template(ctx0, "my-gateway.xml"); err != nil || v.Kind != KindTemplate || v.Entry.Name != "gateway" || v.Preview == nil {
		t.Fatalf("Template(my-gateway.xml) = %+v, %v", v, err)
	}
	if _, err := again.Template(ctx0, "photos"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("Template(photos) = %v, want ErrTemplateNotFound: a template is looked up by its file", err)
	}

	st, _ := again.State(ctx0)
	if err := os.Remove(filepath.Join(s.Dir, st.Source.File)); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Template(ctx0, "my-gateway.xml"); !errors.Is(err, ErrSourceUnavailable) {
		t.Errorf("Template without the zip = %v, want ErrSourceUnavailable", err)
	}
	if _, err := again.Template(ctx0, "stack"); !errors.Is(err, ErrSourceUnavailable) {
		t.Errorf("Template of a project without the zip = %v, want ErrSourceUnavailable", err)
	}
	if list, err := again.Templates(ctx0); err != nil || list.Counts.Clean != 3 {
		t.Errorf("Templates without the zip = %+v, %v, want the kept facts", list, err)
	}
}

// A session scanned before templates were converted holds no outcome, and says
// so instead of answering with an empty list.
func TestService_AReportWithoutOutcomesSaysSo(t *testing.T) {
	s := scanned(t, nil)
	row, found, err := s.Sessions.Get(ctx0)
	if err != nil || !found {
		t.Fatalf("session row = %v, %v", found, err)
	}
	var rep map[string]any
	if err := json.Unmarshal(row.Report, &rep); err != nil {
		t.Fatal(err)
	}
	delete(rep["import"].(map[string]any), "templateCounts")
	row.Report, _ = json.Marshal(rep)
	if err := s.Sessions.Put(ctx0, store.MigrationSession(row)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Templates(ctx0); !errors.Is(err, ErrNoPreview) {
		t.Errorf("Templates = %v, want ErrNoPreview", err)
	}
	if _, err := s.Template(ctx0, "my-notes.xml"); !errors.Is(err, ErrNoPreview) {
		t.Errorf("Template = %v, want ErrNoPreview", err)
	}
}
