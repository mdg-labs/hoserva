package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

// mockTemplates is this mock's catalog: three templates covering the input
// kinds the install form shows, and every privilege the summary reports.
// They are read by the production resolver, so the mock cannot accept an
// install the real one refuses.
var mockTemplates = map[string]string{
	"jellyfin": `services:
  jellyfin:
    image: lscr.io/linuxserver/jellyfin:10.10.7
    environment:
      PUID: "99"
      PGID: "100"
      TZ: ${TZ}
    volumes:
      - ${APPDATA}/jellyfin:/config
      - ${MEDIA}:/data/media
    ports:
      - ${WEBUI_PORT}:8096
x-hoserva:
  schema: 1
  id: jellyfin
  revision: 1
  title: Jellyfin
  categories: [media]
  icon: icon.svg
  docs: https://docs.linuxserver.io/images/docker-jellyfin/
  maintainer: LinuxServer.io
  description: |-
    Jellyfin streams your own movies, shows and music to the devices in your home.

    It runs on your server, keeps its own library and needs no account.
  screenshots:
    - screenshots/library.png
    - screenshots/player.png
  links:
    project: https://jellyfin.org/
    support: https://jellyfin.org/docs/general/getting-help/
    donate: https://opencollective.com/jellyfin
  webui: http://{host}:${WEBUI_PORT}
  inputs:
    APPDATA:       { kind: path, role: appdata, default: /mnt/cache/appdata }
    MEDIA:         { kind: path, role: media, default: /mnt/user/media, label: Media library }
    WEBUI_PORT:    { kind: port, default: 8096 }
    TZ:            { kind: timezone }
    TRANSCODE_GPU: { kind: device, role: gpu, label: Hardware transcoding }
`,
	"aio-notes": `services:
  app:
    image: registry.example.com/notes/app:1.0.0
    environment:
      DATABASE_URL: postgres://notes:${DB_PASSWORD}@db/notes
    volumes:
      - ${APPDATA}/notes/data:/data
    ports:
      - ${WEBUI_PORT}:3000
  db:
    image: postgres:16.4
    environment:
      POSTGRES_PASSWORD: ${DB_PASSWORD}
    volumes:
      - ${APPDATA}/notes/db:/var/lib/postgresql/data
x-hoserva:
  schema: 1
  id: aio-notes
  revision: 3
  title: Notes (all in one)
  categories: [productivity]
  icon: icon.svg
  docs: https://example.com/notes/docs
  maintainer: Example Notes Project
  inputs:
    APPDATA:     { kind: path, role: appdata, default: /mnt/cache/appdata }
    WEBUI_PORT:  { kind: port, default: 3000 }
    DB_PASSWORD: { kind: secret, label: Database password }
`,
	"risky-agent": `services:
  agent:
    image: registry.example.com/agent/agent:1.0.0
    privileged: true
    network_mode: host
    cap_add: [SYS_ADMIN]
    security_opt: [apparmor:unconfined]
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - ${APPDATA}/agent:/config
      - ${HOST_DATA}:/host-data:ro
x-hoserva:
  schema: 1
  id: risky-agent
  revision: 1
  title: Risky agent
  categories: [system]
  icon: icon.svg
  docs: https://example.com/agent/docs
  inputs:
    APPDATA:   { kind: path, role: appdata, default: /mnt/cache/appdata }
    HOST_DATA: { kind: path, role: share, default: /mnt/user/data }
`,
}

// mockIcons is each mock template's icon file.
var mockIcons = map[string][]byte{
	"jellyfin":    []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10" fill="#6a5acd"/></svg>`),
	"aio-notes":   []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect x="4" y="3" width="16" height="18" fill="#2e8b57"/></svg>`),
	"risky-agent": []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M12 2 22 22H2z" fill="#cd5c5c"/></svg>`),
}

// mockScreenshots are jellyfin's two screenshots: small real PNG files, so a
// page built on the mock loads actual images.
var mockScreenshots = map[string][][]byte{
	"jellyfin": {mockPNG(color.RGBA{R: 0x6a, G: 0x5a, B: 0xcd, A: 0xff}), mockPNG(color.RGBA{R: 0x2e, G: 0x8b, B: 0x57, A: 0xff})},
}

func mockPNG(c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 320, 180))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// mockExtrasTemplates are the templates of the one user-added source the
// mock starts with. That source has no public key, so its entries are
// badged user-added and unsigned.
var mockExtrasTemplates = map[string]string{
	"quickpaste": `services:
  paste:
    image: registry.example.org/quickpaste/quickpaste:2.1.0
    network_mode: host
    volumes:
      - ${APPDATA}/quickpaste:/data
x-hoserva:
  schema: 1
  id: quickpaste
  revision: 2
  title: Quick Paste
  categories: [tools]
  icon: icon.svg
  docs: https://example.org/quickpaste/docs
  maintainer: Community contributor
  inputs:
    APPDATA: { kind: path, role: appdata, default: /mnt/cache/appdata }
`,
}

var mockExtrasIcons = map[string][]byte{
	"quickpaste": []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect x="5" y="4" width="14" height="17" rx="2" fill="#b8860b"/></svg>`),
}

// mockExtrasCatalog is what the mock's seeded user-added source supplies.
func mockExtrasCatalog() template.MapCatalog {
	return template.MapCatalog{
		Source:      mockExtrasSourceID,
		Kind:        store.CatalogSourceUserAdded,
		Signed:      false,
		Serial:      1,
		GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Templates:   mockExtrasTemplates,
		Icons:       mockExtrasIcons,
	}
}

// mockMerged is the catalog as the daemon serves it: the curated catalog's
// templates first, then the user-added sources' in the order they were
// added, and a template id an earlier one lists is never supplied by a later
// one. A source added through this mock supplies no templates of its own;
// only the seeded one does.
type mockMerged struct{ catalogs []template.Catalog }

func (h *handler) mockCatalogs() mockMerged {
	merged := mockMerged{catalogs: []template.Catalog{mockCatalog()}}
	h.sourcesMu.Lock()
	defer h.sourcesMu.Unlock()
	for _, s := range h.catalogSources {
		if s.ID == mockExtrasSourceID {
			merged.catalogs = append(merged.catalogs, mockExtrasCatalog())
		}
	}
	return merged
}

func (m mockMerged) Name() string { return template.SourceCurated }

func (m mockMerged) Index(ctx context.Context) (template.Index, error) {
	var out template.Index
	seen := map[string]bool{}
	for i, c := range m.catalogs {
		idx, err := c.Index(ctx)
		if err != nil {
			return template.Index{}, err
		}
		if i == 0 {
			out.Serial, out.GeneratedAt = idx.Serial, idx.GeneratedAt
		}
		for _, t := range idx.Templates {
			if !seen[t.ID] {
				seen[t.ID] = true
				out.Templates = append(out.Templates, t)
			}
		}
	}
	return out, nil
}

func (m mockMerged) Entry(ctx context.Context, id string) (template.Entry, error) {
	for _, c := range m.catalogs {
		e, err := c.Entry(ctx, id)
		if !errors.Is(err, template.ErrTemplateNotFound) {
			return e, err
		}
	}
	return template.Entry{}, fmt.Errorf("%w: %q", template.ErrTemplateNotFound, id)
}

func (m mockMerged) Icon(ctx context.Context, id string) (template.Icon, error) {
	for _, c := range m.catalogs {
		ic, err := c.Icon(ctx, id)
		if !errors.Is(err, template.ErrTemplateNotFound) {
			return ic, err
		}
	}
	return template.Icon{}, fmt.Errorf("%w: %q", template.ErrTemplateNotFound, id)
}

func (m mockMerged) Screenshot(ctx context.Context, id string, index int) (template.Screenshot, error) {
	for _, c := range m.catalogs {
		shot, err := c.Screenshot(ctx, id, index)
		if !errors.Is(err, template.ErrTemplateNotFound) {
			return shot, err
		}
	}
	return template.Screenshot{}, fmt.Errorf("%w: %q", template.ErrTemplateNotFound, id)
}

// mockCatalog is this mock's catalog source: the curated source's name over
// the mock templates and their icons.
func mockCatalog() template.MapCatalog {
	return template.MapCatalog{
		Source:      template.SourceCurated,
		Kind:        store.CatalogSourceCurated,
		Signed:      true,
		Serial:      1,
		GeneratedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Templates:   mockTemplates,
		Icons:       mockIcons,
		Screenshots: mockScreenshots,
	}
}

// mockGPU stands for a host with one render device and a render group.
type mockGPU struct{}

func (mockGPU) RenderDevices(context.Context) ([]string, error) {
	return []string{"/dev/dri/renderD128"}, nil
}

func (mockGPU) RenderGID(context.Context) (string, error) { return "44", nil }

// mockBusyPort is a host port something outside Hoserva listens on in this
// mock, so a form can meet a port conflict on a port no app lists.
const mockBusyPort = 9000

// mockPorts reports the host ports this mock's apps publish or are
// configured to publish, running or stopped, and the one the host listens on.
type mockPorts struct{ h *handler }

func (m mockPorts) UsedPorts(context.Context) (map[int]bool, error) {
	m.h.appsMu.Lock()
	defer m.h.appsMu.Unlock()
	used := map[int]bool{mockBusyPort: true}
	for _, a := range m.h.apps {
		for _, p := range a.Ports {
			if hp, ok := p.HostPort.Get(); ok && hp != 0 {
				used[hp] = true
			}
		}
	}
	return used, nil
}

// composePorts is the host ports a Compose file publishes with env
// substituted: the first number of a `ports` entry given as a string
// ("${PORT}:80", "127.0.0.1:8080:80"), or a long-syntax `published`. An entry
// with no host port is the Engine's to choose and is not a port.
func composePorts(compose, env string) map[int]bool {
	var doc struct {
		Services map[string]struct {
			Ports []yaml.Node `yaml:"ports"`
		} `yaml:"services"`
	}
	used := map[int]bool{}
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return used
	}
	values := map[string]string{}
	for _, l := range strings.Split(env, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			values[k] = strings.Trim(v, `"'`)
		}
	}
	expand := func(s string) string {
		return os.Expand(s, func(k string) string {
			name, def, hasDef := strings.Cut(k, ":-")
			if v := values[name]; v != "" || !hasDef {
				return v
			}
			return def
		})
	}
	for _, svc := range doc.Services {
		for i := range svc.Ports {
			var host string
			switch svc.Ports[i].Kind {
			case yaml.ScalarNode:
				parts := strings.Split(expand(svc.Ports[i].Value), ":")
				if len(parts) < 2 {
					continue
				}
				host = parts[len(parts)-2]
			case yaml.MappingNode:
				var long struct {
					Published string `yaml:"published"`
				}
				if svc.Ports[i].Decode(&long) == nil {
					host = expand(long.Published)
				}
			}
			if n, err := strconv.Atoi(host); err == nil && n > 0 {
				used[n] = true
			}
		}
	}
	return used
}

func (m mockStackCreator) PublishedPorts(context.Context) (map[int]bool, error) {
	m.h.stacksMu.Lock()
	defer m.h.stacksMu.Unlock()
	used := map[int]bool{}
	for _, ports := range m.h.stackPorts {
		for p := range ports {
			used[p] = true
		}
	}
	return used, nil
}

// mockStackCreator stores a stack in memory, refusing what CreateStack does.
type mockStackCreator struct{ h *handler }

func (m mockStackCreator) Create(_ context.Context, n container.NewStack) (container.Stack, error) {
	if strings.TrimSpace(n.Compose) == "" {
		return container.Stack{}, fmt.Errorf("%w: the compose file is empty", container.ErrInvalidStack)
	}
	if names := container.ReservedEnvDefined(n.Env); len(names) > 0 {
		return container.Stack{}, fmt.Errorf("%w: %s", container.ErrReservedEnvName, strings.Join(names, ", "))
	}
	m.h.stacksMu.Lock()
	defer m.h.stacksMu.Unlock()
	if _, ok := m.h.stacks[n.Name]; ok {
		return container.Stack{}, fmt.Errorf("%w: %s", container.ErrStackExists, n.Name)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if m.h.stacks == nil {
		m.h.stacks = map[string]apiv1.Stack{}
	}
	m.h.setStackPorts(n.Name, composePorts(n.Compose, n.Env))
	m.h.setStackEnv(n.Name, n.Env)
	m.h.stacks[n.Name] = apiv1.Stack{
		Name:        n.Name,
		Template:    apiv1.StackTemplate{Source: n.TemplateSource, ID: n.TemplateID, Revision: n.TemplateRevision},
		InstalledAt: now,
		Compose:     apiv1.NewOptString(n.Compose),
	}
	return container.Stack{Name: n.Name, TemplateSource: n.TemplateSource, TemplateID: n.TemplateID, TemplateRevision: n.TemplateRevision, InstalledAt: now}, nil
}

func (m mockStackCreator) Get(_ context.Context, name string) (container.Stack, error) {
	if !container.ValidStackName(name) {
		return container.Stack{}, fmt.Errorf("%w: %q", container.ErrInvalidStackName, name)
	}
	m.h.stacksMu.Lock()
	defer m.h.stacksMu.Unlock()
	s, ok := m.h.stacks[name]
	if !ok {
		return container.Stack{}, fmt.Errorf("%w: %s", container.ErrStackNotFound, name)
	}
	return container.Stack{
		Name:             s.Name,
		TemplateSource:   s.Template.Source,
		TemplateID:       s.Template.ID,
		TemplateRevision: s.Template.Revision,
		InstalledAt:      s.InstalledAt,
		Compose:          s.Compose.Or(""),
		ManuallyEdited:   s.ManuallyEdited,
	}, nil
}

func (m mockStackCreator) Env(ctx context.Context, name string) (string, error) {
	if _, err := m.Get(ctx, name); err != nil {
		return "", err
	}
	m.h.stacksMu.Lock()
	defer m.h.stacksMu.Unlock()
	return m.h.stackEnvs[name], nil
}

// UpdateEnv replaces the stack's .env and works its ports out again from the
// Compose text, as production does with the row and `docker compose config`.
func (m mockStackCreator) UpdateEnv(ctx context.Context, name, env string) (container.Stack, error) {
	if names := container.ReservedEnvDefined(env); len(names) > 0 {
		return container.Stack{}, fmt.Errorf("%w: %s", container.ErrReservedEnvName, strings.Join(names, ", "))
	}
	st, err := m.Get(ctx, name)
	if err != nil {
		return container.Stack{}, err
	}
	m.h.stacksMu.Lock()
	defer m.h.stacksMu.Unlock()
	m.h.setStackEnv(name, env)
	m.h.setStackPorts(name, composePorts(st.Compose, env))
	return st, nil
}

func (h *handler) templateInstaller() *template.Installer {
	return &template.Installer{
		Catalog: h.mockCatalogs(),
		Stacks:  mockStackCreator{h},
		Ports:   mockPorts{h},
		Shares: func(context.Context) ([]string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			names := make([]string, 0, len(h.shares))
			for n := range h.shares {
				names = append(names, n)
			}
			return names, nil
		},
		GPU:      mockGPU{},
		Timezone: func() string { return "UTC" },
	}
}

// mapMockTemplateError gives an install error the status and code the
// production handler gives it.
func mapMockTemplateError(name string, err error) error {
	switch {
	case errors.Is(err, template.ErrCatalogUnavailable):
		return &mockError{code: "catalog_unavailable", statusCode: 503, message: err.Error()}
	case errors.Is(err, template.ErrIconNotFound):
		return &mockError{code: "template_icon_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrScreenshotNotFound):
		return &mockError{code: "template_screenshot_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrTemplateNotFound):
		return &mockError{code: "template_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrInvalidTemplate):
		return &mockError{code: "template_invalid", statusCode: 422, message: err.Error()}
	case errors.Is(err, template.ErrInvalidInput):
		return &mockError{code: "invalid_template_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, template.ErrGPUUnavailable):
		return &mockError{code: "gpu_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrNoFreePort), errors.Is(err, template.ErrPortTaken):
		return &mockError{code: "no_free_port", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrStackHasNoTemplate):
		return &mockError{code: "stack_has_no_template", statusCode: 409, message: fmt.Sprintf("stack %q was not installed from a template and has no inputs to change; edit its Compose file instead", name)}
	case errors.Is(err, container.ErrStackNotFound):
		return errStackNotFound(name)
	case errors.Is(err, container.ErrInvalidStackName):
		return &mockError{code: "invalid_stack_name", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrInvalidStack):
		return &mockError{code: "invalid_stack", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrReservedEnvName):
		return &mockError{code: "invalid_stack_env", statusCode: 400, message: err.Error()}
	case errors.Is(err, container.ErrStackExists):
		return &mockError{code: "stack_exists", statusCode: 409, message: fmt.Sprintf("a stack named %q already exists", name)}
	}
	return err
}

func (h *handler) ListCatalog(ctx context.Context) (*apiv1.CatalogList, error) {
	index, err := h.mockCatalogs().Index(ctx)
	if err != nil {
		return nil, mapMockTemplateError("", err)
	}
	h.stacksMu.Lock()
	installed := map[string]bool{}
	for _, s := range h.stacks {
		if s.Template.ID != "" {
			installed[s.Template.ID] = true
		}
	}
	h.stacksMu.Unlock()
	out := &apiv1.CatalogList{Serial: index.Serial, Templates: make([]apiv1.CatalogEntry, len(index.Templates))}
	if !index.GeneratedAt.IsZero() {
		out.GeneratedAt = apiv1.NewOptDateTime(index.GeneratedAt)
	}
	for i, t := range index.Templates {
		kind, signed := mockBadge(t.Kind, t.Signed)
		out.Templates[i] = apiv1.CatalogEntry{
			ID: t.ID, Revision: t.Revision, Title: t.Title, Categories: t.Categories, Docs: t.Docs, Maintainer: mockOptString(t.Maintainer), Description: mockOptString(t.Description),
			Source: t.Source, SourceKind: kind, Signed: signed, Installed: installed[t.ID],
		}
	}
	h.catalogMu.Lock()
	if last := h.catalogLast; last != nil {
		out.LastCheckedAt = apiv1.NewOptDateTime(last.CheckedAt)
		out.LastOutcome = apiv1.NewOptCatalogCheckOutcome(last.Outcome)
	}
	h.catalogMu.Unlock()
	return out, nil
}

// RefreshCatalog answers a scripted sequence of checks, so a page built on
// the mock meets every outcome: the first check updates the catalog (two
// new templates and one updated), the second finds it unchanged, the third
// fails to reach the host, and the sequence then repeats. The mock's catalog
// itself never changes.
func (h *handler) RefreshCatalog(context.Context) (*apiv1.CatalogRefresh, error) {
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	out := &apiv1.CatalogRefresh{CheckedAt: time.Now().UTC().Truncate(time.Second)}
	switch h.catalogChecks % 3 {
	case 0:
		out.Outcome = apiv1.CatalogCheckOutcomeUpdated
		out.NewTemplates = apiv1.NewOptInt(2)
		out.UpdatedTemplates = apiv1.NewOptInt(1)
	case 1:
		out.Outcome = apiv1.CatalogCheckOutcomeUnchanged
	default:
		out.Outcome = apiv1.CatalogCheckOutcomeFailed
		out.Reason = apiv1.NewOptCatalogRefreshReason(apiv1.CatalogRefreshReasonFetchFailed)
		out.Message = apiv1.NewOptString("fetching https://catalog.hoserva.dev/catalog.tar.zst: the mock's scripted network failure")
	}
	h.catalogChecks++
	last := *out
	h.catalogLast = &last
	return out, nil
}

func (h *handler) GetCatalogSettings(context.Context) (*apiv1.CatalogSettings, error) {
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	return mockCatalogSettings(h.catalogSettings), nil
}

func mockCatalogSettings(s store.CatalogSettings) *apiv1.CatalogSettings {
	return &apiv1.CatalogSettings{RefreshInterval: apiv1.CatalogRefreshInterval(s.RefreshInterval), CheckOnOpen: s.CheckOnOpen}
}

func (h *handler) UpdateCatalogSettings(_ context.Context, req *apiv1.CatalogSettingsUpdate) (*apiv1.CatalogSettings, error) {
	h.catalogMu.Lock()
	defer h.catalogMu.Unlock()
	next := h.catalogSettings
	if v, ok := req.RefreshInterval.Get(); ok {
		switch string(v) {
		case store.CatalogIntervalOff, store.CatalogInterval1h, store.CatalogInterval6h, store.CatalogInterval12h, store.CatalogInterval24h:
			next.RefreshInterval = string(v)
		default:
			return nil, &mockError{code: "invalid_catalog_interval", statusCode: 400, message: fmt.Sprintf("%s: %q", store.ErrCatalogInterval, string(v))}
		}
	}
	if v, ok := req.CheckOnOpen.Get(); ok {
		next.CheckOnOpen = v
	}
	h.catalogSettings = next
	return mockCatalogSettings(next), nil
}

func (h *handler) GetCatalogTemplate(ctx context.Context, params apiv1.GetCatalogTemplateParams) (*apiv1.CatalogTemplate, error) {
	d, err := template.Show(ctx, h.mockCatalogs(), params.ID)
	if err != nil {
		return nil, mapMockTemplateError(params.ID, err)
	}
	kind, signed := mockBadge(d.Kind, d.Signed)
	out := &apiv1.CatalogTemplate{
		ID: d.ID, Revision: d.Revision, Title: d.Title, Categories: d.Categories, Docs: d.Docs,
		Maintainer: mockOptString(d.Maintainer), Description: mockOptString(d.Description), ScreenshotCount: d.Screenshots,
		Source: d.Source, SourceKind: kind, Signed: signed, Compose: d.Compose, Privileges: make([]apiv1.TemplatePrivilege, len(d.Privileges)),
	}
	if d.Links != (template.Links{}) {
		out.Links = apiv1.NewOptCatalogTemplateLinks(apiv1.CatalogTemplateLinks{
			Project: mockOptString(d.Links.Project), Support: mockOptString(d.Links.Support), Donate: mockOptString(d.Links.Donate),
		})
	}
	for i, pr := range d.Privileges {
		tp := apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description}
		if pr.Detail != "" {
			tp.Detail = apiv1.NewOptString(pr.Detail)
		}
		out.Privileges[i] = tp
	}
	return out, nil
}

const (
	mockIconCSP     = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	mockIconNoSniff = "nosniff"
)

func (h *handler) GetCatalogTemplateIcon(ctx context.Context, params apiv1.GetCatalogTemplateIconParams) (apiv1.GetCatalogTemplateIconRes, error) {
	icon, err := h.mockCatalogs().Icon(ctx, params.ID)
	if err != nil {
		return nil, mapMockTemplateError(params.ID, err)
	}
	body := bytes.NewReader(icon.Data)
	switch icon.ContentType {
	case "image/svg+xml":
		return &apiv1.GetCatalogTemplateIconOKImageSvgXMLHeaders{ContentSecurityPolicy: mockIconCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateIconOKImageSvgXML{Data: body}}, nil
	case "image/png":
		return &apiv1.GetCatalogTemplateIconOKImagePNGHeaders{ContentSecurityPolicy: mockIconCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateIconOKImagePNG{Data: body}}, nil
	case "image/webp":
		return &apiv1.GetCatalogTemplateIconOKImageWEBPHeaders{ContentSecurityPolicy: mockIconCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateIconOKImageWEBP{Data: body}}, nil
	case "image/jpeg":
		return &apiv1.GetCatalogTemplateIconOKImageJpegHeaders{ContentSecurityPolicy: mockIconCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateIconOKImageJpeg{Data: body}}, nil
	}
	return nil, fmt.Errorf("mock catalog icon of template %q has the content type %q, which the API does not serve", params.ID, icon.ContentType)
}

func mockOptString(s string) apiv1.OptString {
	if s == "" {
		return apiv1.OptString{}
	}
	return apiv1.NewOptString(s)
}

const mockScreenshotCSP = "default-src 'none'; sandbox"

func (h *handler) GetCatalogTemplateScreenshot(ctx context.Context, params apiv1.GetCatalogTemplateScreenshotParams) (apiv1.GetCatalogTemplateScreenshotRes, error) {
	shot, err := h.mockCatalogs().Screenshot(ctx, params.ID, params.Index)
	if err != nil {
		return nil, mapMockTemplateError(params.ID, err)
	}
	body := bytes.NewReader(shot.Data)
	switch shot.ContentType {
	case "image/png":
		return &apiv1.GetCatalogTemplateScreenshotOKImagePNGHeaders{ContentSecurityPolicy: mockScreenshotCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateScreenshotOKImagePNG{Data: body}}, nil
	case "image/webp":
		return &apiv1.GetCatalogTemplateScreenshotOKImageWEBPHeaders{ContentSecurityPolicy: mockScreenshotCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateScreenshotOKImageWEBP{Data: body}}, nil
	case "image/jpeg":
		return &apiv1.GetCatalogTemplateScreenshotOKImageJpegHeaders{ContentSecurityPolicy: mockScreenshotCSP, XContentTypeOptions: mockIconNoSniff, Response: apiv1.GetCatalogTemplateScreenshotOKImageJpeg{Data: body}}, nil
	}
	return nil, fmt.Errorf("mock catalog screenshot %d of template %q has the content type %q, which the API does not serve", params.Index, params.ID, shot.ContentType)
}

func (h *handler) PreviewTemplateInstall(ctx context.Context, req *apiv1.TemplateInstallRequest, params apiv1.PreviewTemplateInstallParams) (*apiv1.TemplateInstallPlan, error) {
	plan, err := h.templateInstaller().Preview(ctx, mockPlanRequest(params.ID, req))
	if err != nil {
		return nil, mapMockTemplateError(req.Name.Or(params.ID), err)
	}
	out := mockPlanToAPI(plan)
	return &out, nil
}

func (h *handler) InstallTemplate(ctx context.Context, req *apiv1.TemplateInstallRequest, params apiv1.InstallTemplateParams) (*apiv1.TemplateInstallResult, error) {
	plan, st, err := h.templateInstaller().Install(ctx, mockPlanRequest(params.ID, req))
	if err != nil {
		return nil, mapMockTemplateError(req.Name.Or(params.ID), err)
	}
	return &apiv1.TemplateInstallResult{
		Stack: apiv1.Stack{
			Name:        st.Name,
			Template:    apiv1.StackTemplate{Source: st.TemplateSource, ID: st.TemplateID, Revision: st.TemplateRevision},
			InstalledAt: st.InstalledAt,
		},
		Plan: mockPlanToAPI(plan),
	}, nil
}

func mockPlanRequest(id string, req *apiv1.TemplateInstallRequest) template.PlanRequest {
	return template.PlanRequest{ID: id, Name: req.Name.Or(""), Values: req.Values.Or(nil)}
}

func mockPlanToAPI(p *template.Plan) apiv1.TemplateInstallPlan {
	out := apiv1.TemplateInstallPlan{
		Template:   apiv1.StackTemplate{Source: p.Source, ID: p.ID, Revision: strconv.Itoa(p.Revision)},
		Title:      p.Title,
		Name:       p.Name,
		Inputs:     make([]apiv1.TemplateInput, len(p.Inputs)),
		Privileges: make([]apiv1.TemplatePrivilege, len(p.Privileges)),
		Compose:    p.Compose,
	}
	for i, in := range p.Inputs {
		ti := apiv1.TemplateInput{Name: in.Name, Kind: apiv1.TemplateInputKind(in.Kind), Generated: in.Generated, Suggestions: in.Suggestions}
		if in.Role != "" {
			ti.Role = apiv1.NewOptTemplateInputRole(apiv1.TemplateInputRole(in.Role))
		}
		if in.Label != "" {
			ti.Label = apiv1.NewOptString(in.Label)
		}
		if in.Description != "" {
			ti.Description = apiv1.NewOptString(in.Description)
		}
		if in.Kind != template.KindSecret {
			ti.Value = apiv1.NewOptString(in.Value)
		}
		if in.Requested != "" {
			ti.RequestedValue = apiv1.NewOptString(in.Requested)
		}
		out.Inputs[i] = ti
	}
	for i, pr := range p.Privileges {
		tp := apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description}
		if pr.Detail != "" {
			tp.Detail = apiv1.NewOptString(pr.Detail)
		}
		out.Privileges[i] = tp
	}
	return out
}

// ConvertUnraidTemplate runs the production converter over the posted
// template, so the mock refuses and warns exactly as the daemon does.
func (h *handler) ConvertUnraidTemplate(_ context.Context, req *apiv1.UnraidConvertRequest) (*apiv1.UnraidConversion, error) {
	conv, err := template.ConvertUnraid([]byte(req.XML), template.ConvertOptions{})
	if err != nil {
		if errors.Is(err, template.ErrInvalidUnraidTemplate) {
			return nil, &mockError{code: "invalid_unraid_template", statusCode: 400, message: err.Error()}
		}
		return nil, err
	}
	opt := func(s string) apiv1.OptString {
		if s == "" {
			return apiv1.OptString{}
		}
		return apiv1.NewOptString(s)
	}
	out := &apiv1.UnraidConversion{
		Source:     conv.Source,
		Compose:    conv.Compose,
		Clean:      conv.Clean(),
		Warnings:   make([]apiv1.ConversionWarning, len(conv.Warnings)),
		Privileges: make([]apiv1.TemplatePrivilege, len(conv.Privileges)),
		Metadata: apiv1.UnraidTemplateMetadata{
			Title:      conv.Metadata.Title,
			Overview:   opt(conv.Metadata.Overview),
			Category:   opt(conv.Metadata.Category),
			Support:    opt(conv.Metadata.Support),
			Project:    opt(conv.Metadata.Project),
			Webui:      opt(conv.Metadata.WebUI),
			Icon:       opt(conv.Metadata.Icon),
			Requires:   opt(conv.Metadata.Requires),
			DonateLink: opt(conv.Metadata.DonateLink),
			Variables:  make([]apiv1.UnraidVariable, len(conv.Metadata.Variables)),
		},
	}
	for i, w := range conv.Warnings {
		out.Warnings[i] = apiv1.ConversionWarning{Class: apiv1.ConversionWarningClass(w.Class), Message: w.Message, Detail: opt(w.Detail), Command: opt(w.Command)}
	}
	for i, pr := range conv.Privileges {
		out.Privileges[i] = apiv1.TemplatePrivilege{Kind: apiv1.TemplatePrivilegeKind(pr.Kind), Service: pr.Service, Description: pr.Description, Detail: opt(pr.Detail)}
	}
	for i, v := range conv.Metadata.Variables {
		out.Metadata.Variables[i] = apiv1.UnraidVariable{Name: v.Name, Value: v.Value, Description: opt(v.Description), Secret: v.Secret}
	}
	return out, nil
}
