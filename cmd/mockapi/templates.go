package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/container"
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

// mockGPU stands for a host with one render device and a render group.
type mockGPU struct{}

func (mockGPU) RenderDevices(context.Context) ([]string, error) {
	return []string{"/dev/dri/renderD128"}, nil
}

func (mockGPU) RenderGID(context.Context) (string, error) { return "44", nil }

// mockPorts reports the host ports this mock's running apps publish.
type mockPorts struct{ h *handler }

func (m mockPorts) UsedPorts(context.Context) (map[int]bool, error) {
	m.h.appsMu.Lock()
	defer m.h.appsMu.Unlock()
	used := map[int]bool{}
	for _, a := range m.h.apps {
		switch a.State {
		case apiv1.AppStateRunning, apiv1.AppStateRestarting, apiv1.AppStatePaused:
		default:
			continue
		}
		for _, p := range a.Ports {
			if hp, ok := p.HostPort.Get(); ok && hp != 0 {
				used[hp] = true
			}
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
	m.h.stacks[n.Name] = apiv1.Stack{
		Name:        n.Name,
		Template:    apiv1.StackTemplate{Source: n.TemplateSource, ID: n.TemplateID, Revision: n.TemplateRevision},
		InstalledAt: now,
	}
	return container.Stack{Name: n.Name, TemplateSource: n.TemplateSource, TemplateID: n.TemplateID, TemplateRevision: n.TemplateRevision, InstalledAt: now}, nil
}

func (h *handler) templateInstaller() *template.Installer {
	return &template.Installer{
		Catalog: template.MapCatalog{Source: template.SourceCurated, Templates: mockTemplates},
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
	case errors.Is(err, template.ErrTemplateNotFound):
		return &mockError{code: "template_not_found", statusCode: 404, message: err.Error()}
	case errors.Is(err, template.ErrInvalidTemplate):
		return &mockError{code: "template_invalid", statusCode: 422, message: err.Error()}
	case errors.Is(err, template.ErrInvalidInput):
		return &mockError{code: "invalid_template_input", statusCode: 400, message: err.Error()}
	case errors.Is(err, template.ErrGPUUnavailable):
		return &mockError{code: "gpu_unavailable", statusCode: 409, message: err.Error()}
	case errors.Is(err, template.ErrNoFreePort):
		return &mockError{code: "no_free_port", statusCode: 409, message: err.Error()}
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
