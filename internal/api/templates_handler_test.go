package api_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/template"
)

type tplPorts struct {
	used map[int]bool
	err  error
}

func (p tplPorts) UsedPorts(context.Context) (map[int]bool, error) { return p.used, p.err }

type tplGPU struct{ gidErr error }

func (tplGPU) RenderDevices(context.Context) ([]string, error) {
	return []string{"/dev/dri/renderD128"}, nil
}
func (g tplGPU) RenderGID(context.Context) (string, error) { return "44", g.gidErr }

const tplCatalogHead = `x-hoserva:
  schema: 1
  id: %s
  revision: 4
  title: Probe
  categories: [system]
  icon: icon.svg
  docs: https://example.com
  inputs:
    PORT: { kind: port, default: 8080 }
    PW:   { kind: secret }
    GPU:  { kind: device, role: gpu }
`

func tplTemplate(id string, serviceExtra string) string {
	return "services:\n  app:\n    image: x:1\n    env_file: .env\n    ports:\n      - ${PORT}:80\n" + serviceExtra + "\n" + fmt.Sprintf(tplCatalogHead, id)
}

func newTemplateHandler(t *testing.T) (*api.Handler, string) {
	t.Helper()
	h, root := newStacksHandler(t)
	h.TemplateInstall = &template.Installer{
		Catalog: template.MapCatalog{Source: "hoserva", Templates: map[string]string{
			"probe":  tplTemplate("probe", ""),
			"risky":  tplTemplate("risky", "    privileged: true\n    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n"),
			"broken": "services: {}\n",
		}},
		Stacks: h.Stacks,
		Ports:  tplPorts{used: map[int]bool{8080: true}},
		GPU:    tplGPU{},
	}
	return h, root
}

func TestTemplates_NotConfiguredIs501(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("InstallTemplate = %d %q, want 501 not_configured", status, code)
	}
	_, err = h.PreviewTemplateInstall(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("PreviewTemplateInstall = %d %q, want 501 not_configured", status, code)
	}
}

func TestTemplates_InstallMovesTheConflictingPortAndKeepsSecretsOutOfTheResponse(t *testing.T) {
	h, root := newTemplateHandler(t)
	res, err := h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"})
	if err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	if res.Stack.Name != "probe" || res.Stack.Template.Source != "hoserva" || res.Stack.Template.ID != "probe" || res.Stack.Template.Revision != "4" {
		t.Errorf("stack = %+v", res.Stack)
	}
	byName := map[string]apiv1.TemplateInput{}
	for _, in := range res.Plan.Inputs {
		byName[in.Name] = in
	}
	if p := byName["PORT"]; p.Value.Or("") != "8081" || p.RequestedValue.Or("") != "8080" {
		t.Errorf("PORT = %+v, want 8081 requested 8080", p)
	}
	if pw := byName["PW"]; !pw.Generated || pw.Value.Set {
		t.Errorf("PW = %+v, want generated and no value in the response", pw)
	}
	env, err := os.ReadFile(filepath.Join(root, "probe", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env), "PORT=8081\n") || !strings.Contains(string(env), "PW=") {
		t.Errorf(".env = %q", env)
	}
	if len(res.Plan.Privileges) != 0 {
		t.Errorf("privileges = %+v", res.Plan.Privileges)
	}
}

func TestTemplates_ARequestedPrivilegeIsInTheResultAndThePreview(t *testing.T) {
	h, _ := newTemplateHandler(t)
	plan, err := h.PreviewTemplateInstall(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "risky"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[apiv1.TemplatePrivilegeKind]string{}
	for _, p := range plan.Privileges {
		if p.Description == "" || p.Service != "app" {
			t.Errorf("privilege %+v lacks its service or description", p)
		}
		got[p.Kind] = p.Detail.Or("")
	}
	if _, ok := got[apiv1.TemplatePrivilegeKindPrivileged]; !ok || got[apiv1.TemplatePrivilegeKindDockerSocket] != "/var/run/docker.sock" {
		t.Errorf("preview privileges = %v", got)
	}
	res, err := h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "risky"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Privileges) != 2 {
		t.Errorf("install result privileges = %+v, want privileged and the Docker socket", res.Plan.Privileges)
	}
}

func TestTemplates_ErrorsHaveTheirOwnStatusAndCode(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		setup  func(h *api.Handler)
		id     string
		req    apiv1.TemplateInstallRequest
		status int
		code   string
	}{
		{name: "unknown template", id: "nope", status: 404, code: "template_not_found"},
		{name: "template that fails the rules", id: "broken", status: 422, code: "template_invalid"},
		{name: "input the template lacks", id: "probe", req: apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"X": "1"})}, status: 400, code: "invalid_template_input"},
		{name: "bad stack name", id: "probe", req: apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("../x")}, status: 400, code: "invalid_stack_name"},
		{name: "gpu the host does not offer", id: "probe", req: apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"GPU": "/dev/dri/renderD9"})}, status: 400, code: "invalid_template_input"},
		{
			name: "no render group", id: "probe", status: 409, code: "gpu_unavailable",
			setup: func(h *api.Handler) { h.TemplateInstall.GPU = tplGPU{gidErr: template.ErrGPUUnavailable} },
			req:   apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"GPU": "/dev/dri/renderD128"})},
		},
		{
			name: "no free port", id: "probe", status: 409, code: "no_free_port",
			setup: func(h *api.Handler) {
				used := map[int]bool{}
				for p := 8080; p <= 65535; p++ {
					used[p] = true
				}
				h.TemplateInstall.Ports = tplPorts{used: used}
			},
		},
		{
			name: "docker not reachable", id: "probe", status: 503, code: "docker_unavailable",
			setup: func(h *api.Handler) {
				h.TemplateInstall.Ports = tplPorts{err: fmt.Errorf("listing: %w", container.ErrUnavailable)}
			},
		},
	} {
		h, _ := newTemplateHandler(t)
		if tc.setup != nil {
			tc.setup(h)
		}
		for op, call := range map[string]func() error{
			"preview": func() error {
				_, err := h.PreviewTemplateInstall(ctx, &tc.req, apiv1.PreviewTemplateInstallParams{ID: tc.id})
				return err
			},
			"install": func() error {
				_, err := h.InstallTemplate(ctx, &tc.req, apiv1.InstallTemplateParams{ID: tc.id})
				return err
			},
		} {
			if status, code := statusOf(h, call()); status != tc.status || code != tc.code {
				t.Errorf("%s %s = %d %q, want %d %q", tc.name, op, status, code, tc.status, tc.code)
			}
		}
	}
}

func TestTemplates_AStackOfThatNameIsRefused(t *testing.T) {
	h, _ := newTemplateHandler(t)
	ctx := context.Background()
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"}); err != nil {
		t.Fatal(err)
	}
	_, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 409 || code != "stack_exists" {
		t.Errorf("second install = %d %q, want 409 stack_exists", status, code)
	}
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("probe-two")}, apiv1.InstallTemplateParams{ID: "probe"}); err != nil {
		t.Errorf("a second install under another name: %v", err)
	}
}

func withNetworks(h *api.Handler) *container.FakeProvider {
	engine := container.NewFakeProvider()
	engine.SetNetworks(container.Network{Name: "bridge", Driver: "bridge"}, container.Network{Name: "lan", Driver: "macvlan"})
	h.Container = engine
	h.TemplateInstall.Networks = engine
	return engine
}

func TestTemplates_TheContainerSettingsReachTheComposeFileAndThePlanEchoesTheWarnings(t *testing.T) {
	h, root := newTemplateHandler(t)
	withNetworks(h)
	req := &apiv1.TemplateInstallRequest{
		NetworkMode: apiv1.NewOptString("lan"),
		Restart:     apiv1.NewOptTemplateInstallRequestRestart(apiv1.TemplateInstallRequestRestartAlways),
		Cpus:        apiv1.NewOptFloat64(1.5),
		MemoryMiB:   apiv1.NewOptInt(512),
		ExtraParams: apiv1.NewOptString("--init --no-such-flag --cap-add NET_ADMIN"),
	}
	res, err := h.InstallTemplate(context.Background(), req, apiv1.InstallTemplateParams{ID: "probe"})
	if err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	compose, err := os.ReadFile(filepath.Join(root, "probe", "docker-compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"restart: always", "mem_limit: 512m", "cpus: 1.5", "init: true", "- NET_ADMIN", "lan: {}", "external: true", "#   --no-such-flag"} {
		if !strings.Contains(string(compose), want) {
			t.Errorf("docker-compose.yml lacks %q:\n%s", want, compose)
		}
	}
	if res.Plan.Compose != string(compose) {
		t.Error("the plan's Compose differs from the file that was written")
	}
	if len(res.Plan.Warnings) != 1 || res.Plan.Warnings[0].Class != apiv1.ConversionWarningClassUntranslatedFlag || res.Plan.Warnings[0].Detail.Or("") != "--no-such-flag" {
		t.Errorf("warnings = %+v, want the untranslated flag", res.Plan.Warnings)
	}
	if !res.Plan.AdvancedAvailable {
		t.Error("advancedAvailable is false for a single-service template")
	}
	var caps bool
	for _, p := range res.Plan.Privileges {
		caps = caps || (p.Kind == apiv1.TemplatePrivilegeKindAddedCapabilities && p.Detail.Or("") == "NET_ADMIN")
	}
	if !caps {
		t.Errorf("privileges = %+v, want the added capability", res.Plan.Privileges)
	}
}

func TestTemplates_AMissingNetworkIsAWarningWithItsCommandInThePreviewAndRefusesTheInstall(t *testing.T) {
	h, root := newTemplateHandler(t)
	withNetworks(h)
	req := &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("iot")}
	plan, err := h.PreviewTemplateInstall(context.Background(), req, apiv1.PreviewTemplateInstallParams{ID: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Class != apiv1.ConversionWarningClassMissingNetwork || plan.Warnings[0].Command.Or("") != "docker network create iot" {
		t.Errorf("warnings = %+v, want missing_network with the exact command", plan.Warnings)
	}
	_, err = h.InstallTemplate(context.Background(), req, apiv1.InstallTemplateParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 409 || code != "network_missing" {
		t.Errorf("install = %d %q, want 409 network_missing", status, code)
	}
	if _, statErr := os.Stat(filepath.Join(root, "probe")); !os.IsNotExist(statErr) {
		t.Errorf("a refused install left the stack directory behind: %v", statErr)
	}
}

func TestTemplates_ARefusedInputOrSettingNamesItInTheErrorDetails(t *testing.T) {
	h, _ := newTemplateHandler(t)
	withNetworks(h)
	for _, tc := range []struct {
		name  string
		req   apiv1.TemplateInstallRequest
		input string
	}{
		{"a value", apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"PORT": "70000"})}, "PORT"},
		{"an input the template lacks", apiv1.TemplateInstallRequest{Values: apiv1.NewOptTemplateInstallRequestValues(apiv1.TemplateInstallRequestValues{"X": "1"})}, "X"},
		{"a network name", apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("my net")}, "networkMode"},
		{"a cpu limit", apiv1.TemplateInstallRequest{Cpus: apiv1.NewOptFloat64(5000)}, "cpus"},
		{"a cpu limit of zero, which is not a way to say no limit", apiv1.TemplateInstallRequest{Cpus: apiv1.NewOptFloat64(0)}, "cpus"},
		{"a memory limit", apiv1.TemplateInstallRequest{MemoryMiB: apiv1.NewOptInt(1)}, "memoryMiB"},
		{"extra parameters", apiv1.TemplateInstallRequest{ExtraParams: apiv1.NewOptString("--restart always"), Restart: apiv1.NewOptTemplateInstallRequestRestart(apiv1.TemplateInstallRequestRestartNo)}, "extraParams"},
	} {
		for op, call := range map[string]func() error{
			"preview": func() error {
				_, err := h.PreviewTemplateInstall(context.Background(), &tc.req, apiv1.PreviewTemplateInstallParams{ID: "probe"})
				return err
			},
			"install": func() error {
				_, err := h.InstallTemplate(context.Background(), &tc.req, apiv1.InstallTemplateParams{ID: "probe"})
				return err
			},
		} {
			resp := h.NewError(context.Background(), call())
			got, ok := resp.Response.Details.Get()
			if resp.StatusCode != 400 || resp.Response.Code != "invalid_template_input" || !ok || string(got["input"]) != `"`+tc.input+`"` {
				t.Errorf("%s %s = %d %q details %v, want 400 invalid_template_input naming %s", tc.name, op, resp.StatusCode, resp.Response.Code, got, tc.input)
			}
		}
	}
}

func TestTemplates_AFailedNetworkListIsAnErrorNotAMissingOrExistingNetwork(t *testing.T) {
	h, _ := newTemplateHandler(t)
	engine := withNetworks(h)
	engine.SetUnavailable(nil)
	_, err := h.PreviewTemplateInstall(context.Background(), &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("lan")}, apiv1.PreviewTemplateInstallParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 503 || code != "docker_unavailable" {
		t.Errorf("preview = %d %q, want 503 docker_unavailable", status, code)
	}
}

func TestListDockerNetworks(t *testing.T) {
	h, _ := newTemplateHandler(t)
	engine := withNetworks(h)
	got, err := h.ListDockerNetworks(context.Background())
	if err != nil || !got.Available || len(got.Networks) != 2 || got.Networks[1].Name != "lan" || got.Networks[1].Driver != "macvlan" {
		t.Fatalf("ListDockerNetworks = %+v, %v, want bridge and lan", got, err)
	}

	engine.SetUnavailable(nil)
	got, err = h.ListDockerNetworks(context.Background())
	if err != nil || got.Available || len(got.Networks) != 0 || got.Message.Or("") == "" {
		t.Errorf("with Docker unreachable: %+v, %v, want available false with a message and no error", got, err)
	}

	engine.SetUnavailable(nil)
	engine.SetNetworksError(fmt.Errorf("the Engine answered 500"))
	if got, err = h.ListDockerNetworks(context.Background()); err == nil {
		t.Errorf("a failed list returned %+v and no error, want the failure", got)
	}

	h.Container = nil
	if got, err = h.ListDockerNetworks(context.Background()); err != nil || got.Available {
		t.Errorf("with no Docker service: %+v, %v, want available false", got, err)
	}
}

func TestTemplates_APreviewListsARequiredInputThatHasNoValueAndTheInstallRefusesIt(t *testing.T) {
	h, _ := newTemplateHandler(t)
	h.TemplateInstall.Catalog = template.MapCatalog{Source: "hoserva", Templates: map[string]string{"probe": "services:\n  app:\n    image: x:1\n    environment:\n      NAME: ${NAME}\nx-hoserva:\n  schema: 1\n  id: probe\n  revision: 1\n  title: Probe\n  categories: [system]\n  icon: icon.svg\n  docs: https://example.com\n  inputs:\n    NAME: { kind: string, label: Name }\n"}}
	plan, err := h.PreviewTemplateInstall(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "probe"})
	if err != nil {
		t.Fatalf("PreviewTemplateInstall: %v", err)
	}
	if len(plan.Inputs) != 1 || !plan.Inputs[0].Required || plan.Inputs[0].Error.Or("") != "NAME needs a value" {
		t.Errorf("inputs = %+v, want NAME required with its error", plan.Inputs)
	}
	_, err = h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_template_input" {
		t.Errorf("install = %d %q, want 400 invalid_template_input", status, code)
	}
}

func TestTemplates_APortTheExtraParametersPublishIsRefusedLikeAPortInput(t *testing.T) {
	h, root := newTemplateHandler(t)
	h.TemplateInstall.Ports = tplPorts{used: map[int]bool{9000: true}}
	ctx := context.Background()
	for _, flags := range []string{"-p 9000:80", "-p 8081:80 -p 8081:81"} {
		req := &apiv1.TemplateInstallRequest{ExtraParams: apiv1.NewOptString(flags)}
		for op, call := range map[string]func() error{
			"preview": func() error {
				_, err := h.PreviewTemplateInstall(ctx, req, apiv1.PreviewTemplateInstallParams{ID: "probe"})
				return err
			},
			"install": func() error {
				_, err := h.InstallTemplate(ctx, req, apiv1.InstallTemplateParams{ID: "probe"})
				return err
			},
		} {
			resp := h.NewError(ctx, call())
			got, ok := resp.Response.Details.Get()
			if resp.StatusCode != 409 || resp.Response.Code != "no_free_port" || !ok || string(got["input"]) != `"extraParams"` {
				t.Errorf("%s with %q = %d %q details %v, want 409 no_free_port naming extraParams", op, flags, resp.StatusCode, resp.Response.Code, got)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "probe")); !os.IsNotExist(err) {
		t.Errorf("a refused install left the stack directory behind: %v", err)
	}
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{ExtraParams: apiv1.NewOptString("-p 9100:80")}, apiv1.InstallTemplateParams{ID: "probe"}); err != nil {
		t.Errorf("a free extra port was refused: %v", err)
	}
}
