package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

const catalogSVG = `<svg xmlns="http://www.w3.org/2000/svg"/>`

// listingStackStore is the stack store fake that, unlike stackMemStore, lists
// the rows it holds.
type listingStackStore struct{ *stackMemStore }

func (s listingStackStore) List(context.Context) ([]store.Stack, error) {
	out := make([]store.Stack, 0, len(s.rows))
	for _, r := range s.rows {
		out = append(out, r)
	}
	return out, nil
}

func newCatalogHandler(t *testing.T) *api.Handler {
	t.Helper()
	h, _ := newStacksHandler(t)
	h.Stacks.Store = listingStackStore{h.Stacks.Store.(*stackMemStore)}
	h.Catalog = template.MapCatalog{
		Source:      "hoserva",
		Kind:        store.CatalogSourceCurated,
		Signed:      true,
		Serial:      9,
		GeneratedAt: time.Date(2026, 10, 1, 11, 14, 10, 0, time.UTC),
		Templates: map[string]string{
			"probe":  tplTemplate("probe", ""),
			"risky":  tplTemplate("risky", "    privileged: true\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"),
			"broken": "services: {}\n",
		},
		Icons: map[string][]byte{"probe": []byte(catalogSVG)},
	}
	return h
}

func catalogServer(t *testing.T, h *api.Handler) *httptest.Server {
	t.Helper()
	server, err := apiv1.NewServer(h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix("/api/v1"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return srv
}

func catalogGet(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestCatalog_NotConfiguredIs501(t *testing.T) {
	h, _ := newStacksHandler(t)
	ctx := context.Background()
	_, err := h.ListCatalog(ctx)
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("ListCatalog = %d %q, want 501 not_configured", status, code)
	}
	_, err = h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("GetCatalogTemplate = %d %q, want 501 not_configured", status, code)
	}
	_, err = h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("GetCatalogTemplateIcon = %d %q, want 501 not_configured", status, code)
	}

	h = newCatalogHandler(t)
	h.Stacks = nil
	_, err = h.ListCatalog(ctx)
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("ListCatalog with no stack service = %d %q, want 501 (the installed flag cannot be read)", status, code)
	}
}

func TestCatalog_ListNamesTheSourceAndMarksInstalledTemplates(t *testing.T) {
	h := newCatalogHandler(t)
	ctx := context.Background()
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "agent", Compose: "services: {}\n", TemplateSource: "hoserva", TemplateID: "risky", TemplateRevision: "4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "by-hand", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.Serial != 9 || list.GeneratedAt.Or(time.Time{}).IsZero() {
		t.Errorf("serial %d, generatedAt %v", list.Serial, list.GeneratedAt)
	}
	installed := map[string]bool{}
	var ids []string
	for _, e := range list.Templates {
		ids = append(ids, e.ID)
		installed[e.ID] = e.Installed
		if e.Source != "hoserva" {
			t.Errorf("%s: source = %q, want hoserva", e.ID, e.Source)
		}
	}
	if strings.Join(ids, ",") != "broken,probe,risky" {
		t.Fatalf("templates = %v", ids)
	}
	if installed["probe"] || installed["broken"] || !installed["risky"] {
		t.Errorf("installed = %v, want only risky", installed)
	}
	probe := list.Templates[1]
	if probe.Revision != 4 || probe.Title != "Probe" || len(probe.Categories) != 1 || probe.Categories[0] != "system" || probe.Docs != "https://example.com" {
		t.Errorf("probe = %+v", probe)
	}
}

func TestCatalog_ListOfAMissingOrUnreadableCatalogIs503NotAnEmptyList(t *testing.T) {
	root := t.TempDir()
	for name, catalog := range map[string]template.DirCatalog{
		"no catalog directory": {Root: filepath.Join(root, "absent"), Source: template.SourceCurated},
		"unreadable index": func() template.DirCatalog {
			dir := filepath.Join(root, "bad")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{"), 0o644); err != nil {
				t.Fatal(err)
			}
			return template.DirCatalog{Root: dir, Source: template.SourceCurated}
		}(),
	} {
		h := newCatalogHandler(t)
		h.Catalog = catalog
		list, err := h.ListCatalog(context.Background())
		if status, code := statusOf(h, err); status != 503 || code != "catalog_unavailable" || list != nil {
			t.Errorf("%s: ListCatalog = %+v, %d %q, want 503 catalog_unavailable", name, list, status, code)
		}
	}
}

func TestCatalog_ListCarriesTheIndexDescriptionAndRefusesAnInvalidOne(t *testing.T) {
	root := t.TempDir()
	write := func(name, index string) template.DirCatalog {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte(index), 0o644); err != nil {
			t.Fatal(err)
		}
		return template.DirCatalog{Root: dir, Source: template.SourceCurated}
	}
	h := newCatalogHandler(t)
	h.Catalog = write("good", `{"schema":1,"serial":3,"templates":[
    {"id":"described","revision":1,"title":"Described","categories":[],"docs":"https://example.com","description":"Line one.\n\nLine two."},
    {"id":"bare","revision":1,"title":"Bare","categories":[],"docs":"https://example.com"}]}`)
	list, err := h.ListCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Templates) != 2 || list.Templates[0].Description.Or("") != "Line one.\n\nLine two." || list.Templates[1].Description.IsSet() {
		t.Errorf("templates = %+v, want the described entry's description and none on the bare one", list.Templates)
	}

	h.Catalog = write("bad", `{"schema":1,"serial":3,"templates":[{"id":"x","description":"a\u0007b"}]}`)
	list, err = h.ListCatalog(context.Background())
	if status, code := statusOf(h, err); status != 503 || code != "catalog_unavailable" || list != nil {
		t.Errorf("ListCatalog = %+v, %d %q, want 503 catalog_unavailable", list, status, code)
	}
}

func TestCatalog_DetailReturnsTheRawComposeAndThePrivilegeSummary(t *testing.T) {
	h := newCatalogHandler(t)
	ctx := context.Background()
	probe, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if probe.ID != "probe" || probe.Revision != 4 || probe.Source != "hoserva" || probe.Compose != tplTemplate("probe", "") || len(probe.Privileges) != 0 {
		t.Errorf("probe = %+v", probe)
	}
	risky, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "risky"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[apiv1.TemplatePrivilegeKind]string{}
	for _, p := range risky.Privileges {
		got[p.Kind] = p.Detail.Or("")
	}
	if _, ok := got[apiv1.TemplatePrivilegeKindPrivileged]; !ok || got[apiv1.TemplatePrivilegeKindDockerSocket] != "/var/run/docker.sock" {
		t.Errorf("risky privileges = %v", got)
	}
}

func TestCatalog_DetailAndIconErrors(t *testing.T) {
	h := newCatalogHandler(t)
	ctx := context.Background()
	_, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "broken"})
	if status, code := statusOf(h, err); status != 422 || code != "template_invalid" {
		t.Errorf("an invalid entry = %d %q, want 422 template_invalid", status, code)
	}
	_, err = h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "nope"})
	if status, code := statusOf(h, err); status != 404 || code != "template_not_found" {
		t.Errorf("an unknown id = %d %q, want 404 template_not_found", status, code)
	}
	_, err = h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "nope"})
	if status, code := statusOf(h, err); status != 404 || code != "template_not_found" {
		t.Errorf("icon of an unknown id = %d %q, want 404 template_not_found", status, code)
	}
	_, err = h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "risky"})
	if status, code := statusOf(h, err); status != 404 || code != "template_icon_not_found" {
		t.Errorf("a template with no icon = %d %q, want 404 template_icon_not_found", status, code)
	}
}

func TestCatalog_IconIsServedWithAnAllowListedTypeAndLockedDownHeaders(t *testing.T) {
	srv := catalogServer(t, newCatalogHandler(t))
	resp, body := catalogGet(t, srv, "/catalog/probe/icon")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" || string(body) != catalogSVG {
		t.Fatalf("status %d, type %q, body %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "sandbox"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy = %q lacks %q", csp, want)
		}
	}

	resp, body = catalogGet(t, srv, "/catalog/risky/icon")
	var e apiv1.Error
	if err := json.Unmarshal(body, &e); err != nil || resp.StatusCode != 404 || e.Code != "template_icon_not_found" {
		t.Errorf("no icon: status %d, body %s", resp.StatusCode, body)
	}
}

func TestCatalog_IconThatIsASymlinkIsNeverServed(t *testing.T) {
	root := t.TempDir()
	compose := tplTemplate("probe", "")
	if err := os.MkdirAll(filepath.Join(root, "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe", "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.svg")
	if err := os.WriteFile(secret, []byte("<svg>secret</svg>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "probe", "icon.svg")); err != nil {
		t.Fatal(err)
	}
	h := newCatalogHandler(t)
	h.Catalog = template.DirCatalog{Root: root, Source: template.SourceCurated}
	resp, body := catalogGet(t, catalogServer(t, h), "/catalog/probe/icon")
	if resp.StatusCode != 404 || strings.Contains(string(body), "secret") {
		t.Fatalf("status %d, body %s, want 404 and no file content", resp.StatusCode, body)
	}
}

func TestCatalog_EmbeddedSnapshotIsListedShownAndItsIconsServed(t *testing.T) {
	archive, sig, err := template.EmbeddedSnapshot()
	if err != nil {
		t.Fatalf("%v — run `make catalog-snapshot` first", err)
	}
	store := template.CatalogStore{Dir: filepath.Join(t.TempDir(), "catalog")}
	if _, err := store.Seed(archive, sig); err != nil {
		t.Fatal(err)
	}
	h := newCatalogHandler(t)
	h.Catalog = template.DirCatalog{Root: store.Dir, Source: template.SourceCurated}
	srv := catalogServer(t, h)

	resp, body := catalogGet(t, srv, "/catalog")
	var list apiv1.CatalogList
	if err := json.Unmarshal(body, &list); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status %d, body %s, %v", resp.StatusCode, body, err)
	}
	if len(list.Templates) == 0 {
		t.Fatal("the embedded catalog lists no template")
	}
	for _, e := range list.Templates {
		if e.Source != template.SourceCurated || e.Installed {
			t.Errorf("%s: %+v", e.ID, e)
		}
		resp, body := catalogGet(t, srv, "/catalog/"+e.ID)
		var d apiv1.CatalogTemplate
		if err := json.Unmarshal(body, &d); err != nil || resp.StatusCode != 200 || d.ID != e.ID || d.Compose == "" {
			t.Errorf("%s: detail status %d, %v", e.ID, resp.StatusCode, err)
		}
		resp, body = catalogGet(t, srv, "/catalog/"+e.ID+"/icon")
		if resp.StatusCode != 200 || len(body) == 0 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
			t.Errorf("%s: icon status %d type %q", e.ID, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

type failingListStore struct{ *stackMemStore }

func (failingListStore) List(context.Context) ([]store.Stack, error) {
	return nil, errors.New("disk I/O error")
}

func TestCatalog_ListWhoseStacksCannotBeReadIsAnErrorNotAllNotInstalled(t *testing.T) {
	h := newCatalogHandler(t)
	h.Stacks.Store = failingListStore{h.Stacks.Store.(listingStackStore).stackMemStore}
	list, err := h.ListCatalog(context.Background())
	if status, code := statusOf(h, err); status != 500 || list != nil {
		t.Fatalf("ListCatalog = %+v, %d %q, want a 500", list, status, code)
	}
}

var catalogPNG = []byte("\x89PNG\r\n\x1a\nnot really")

const catalogMetadata = `  maintainer: Example Team
  description: |-
    Line one.

    Line two.
  screenshots: [shots/a.png, b.webp]
  links:
    project: https://example.com/project
    donate: https://example.com/donate
  docs: https://example.com
`

func newMetadataHandler(t *testing.T) *api.Handler {
	t.Helper()
	h := newCatalogHandler(t)
	h.Catalog = template.MapCatalog{
		Source: "hoserva",
		Kind:   store.CatalogSourceCurated,
		Signed: true,
		Serial: 9,
		Templates: map[string]string{
			"rich":  strings.Replace(tplTemplate("rich", ""), "  docs: https://example.com\n", catalogMetadata, 1),
			"plain": tplTemplate("plain", ""),
			"bad":   strings.Replace(tplTemplate("bad", ""), "  docs: https://example.com\n", "  docs: https://example.com\n  links: { donate: \"http://example.com/\" }\n", 1),
		},
		Screenshots: map[string][][]byte{"rich": {catalogPNG, []byte("RIFF....WEBP")}},
	}
	return h
}

func TestCatalog_ListAndDetailCarryTheMetadataOnlyWhenTheTemplateSetsIt(t *testing.T) {
	h := newMetadataHandler(t)
	ctx := context.Background()
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	maintainers := map[string]apiv1.OptString{}
	descriptions := map[string]apiv1.OptString{}
	for _, e := range list.Templates {
		maintainers[e.ID] = e.Maintainer
		descriptions[e.ID] = e.Description
	}
	if descriptions["rich"].Or("") != "Line one.\n\nLine two." || descriptions["plain"].IsSet() {
		t.Errorf("descriptions = %+v", descriptions)
	}
	if maintainers["rich"].Or("") != "Example Team" || maintainers["plain"].IsSet() {
		t.Errorf("maintainers = %+v", maintainers)
	}

	rich, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "rich"})
	if err != nil {
		t.Fatal(err)
	}
	links, hasLinks := rich.Links.Get()
	if rich.Maintainer.Or("") != "Example Team" || rich.Description.Or("") != "Line one.\n\nLine two." || rich.ScreenshotCount != 2 ||
		!hasLinks || links.Project.Or("") != "https://example.com/project" || links.Donate.Or("") != "https://example.com/donate" || links.Support.IsSet() {
		t.Errorf("rich = %+v", rich)
	}
	plain, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Maintainer.IsSet() || plain.Description.IsSet() || plain.Links.IsSet() || plain.ScreenshotCount != 0 {
		t.Errorf("plain = %+v", plain)
	}
	_, err = h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "bad"})
	if status, code := statusOf(h, err); status != 422 || code != "template_invalid" {
		t.Errorf("an http link = %d %q, want 422 template_invalid", status, code)
	}
}

func TestCatalog_ScreenshotIsServedByPositionWithLockedDownHeaders(t *testing.T) {
	srv := catalogServer(t, newMetadataHandler(t))
	resp, body := catalogGet(t, srv, "/catalog/rich/screenshots/0")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || string(body) != string(catalogPNG) {
		t.Fatalf("status %d, type %q, body %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "sandbox"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy = %q lacks %q", csp, want)
		}
	}
	if resp, _ = catalogGet(t, srv, "/catalog/rich/screenshots/1"); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/webp" {
		t.Errorf("second screenshot: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	for path, wantCode := range map[string]string{
		"/catalog/rich/screenshots/2":  "template_screenshot_not_found",
		"/catalog/plain/screenshots/0": "template_screenshot_not_found",
		"/catalog/nope/screenshots/0":  "template_not_found",
	} {
		resp, body := catalogGet(t, srv, path)
		var e apiv1.Error
		if err := json.Unmarshal(body, &e); err != nil || resp.StatusCode != 404 || e.Code != wantCode {
			t.Errorf("%s: status %d, body %s, want 404 %q", path, resp.StatusCode, body, wantCode)
		}
	}
	for _, path := range []string{"/catalog/rich/screenshots/-1", "/catalog/rich/screenshots/a.png", "/catalog/rich/screenshots/8"} {
		if resp, body := catalogGet(t, srv, path); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, body %s, want 400 for a position the API never has", path, resp.StatusCode, body)
		}
	}
}

func TestCatalog_ScreenshotNotConfiguredIs501(t *testing.T) {
	h, _ := newStacksHandler(t)
	_, err := h.GetCatalogTemplateScreenshot(context.Background(), apiv1.GetCatalogTemplateScreenshotParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Errorf("GetCatalogTemplateScreenshot = %d %q, want 501 not_configured", status, code)
	}
}

func TestCatalog_ScreenshotThatIsASymlinkIsNeverServed(t *testing.T) {
	root := t.TempDir()
	compose := strings.Replace(tplTemplate("probe", ""), "  docs: https://example.com\n", "  docs: https://example.com\n  screenshots: [shots/a.png]\n", 1)
	if err := os.MkdirAll(filepath.Join(root, "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "probe", "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "a.png"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "probe", "shots")); err != nil {
		t.Fatal(err)
	}
	h := newCatalogHandler(t)
	h.Catalog = template.DirCatalog{Root: root, Source: template.SourceCurated}
	resp, body := catalogGet(t, catalogServer(t, h), "/catalog/probe/screenshots/0")
	if resp.StatusCode != 404 || strings.Contains(string(body), "secret") {
		t.Fatalf("status %d, body %s, want 404 and no file content", resp.StatusCode, body)
	}
}
