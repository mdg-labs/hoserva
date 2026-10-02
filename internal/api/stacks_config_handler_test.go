package api_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

func readStackFile(t *testing.T, root, stack, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, stack, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func installProbe(t *testing.T) (*api.Handler, string) {
	t.Helper()
	h, root := newTemplateHandler(t)
	if _, err := h.InstallTemplate(context.Background(), &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "probe"}); err != nil {
		t.Fatal(err)
	}
	return h, root
}

func configValues(req map[string]string) *apiv1.UpdateStackConfigRequest {
	return &apiv1.UpdateStackConfigRequest{Values: apiv1.NewOptUpdateStackConfigRequestValues(req)}
}

func configInputOf(t *testing.T, c *apiv1.StackConfig, name string) apiv1.StackConfigInput {
	t.Helper()
	for _, in := range c.Inputs {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("no input %s in %+v", name, c.Inputs)
	return apiv1.StackConfigInput{}
}

func TestStackConfig_NotConfiguredIs501(t *testing.T) {
	h, _, _ := newTestHandler(t)
	_, err := h.GetStackConfig(context.Background(), apiv1.GetStackConfigParams{Name: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("GetStackConfig = %d %q, want 501 not_configured", status, code)
	}
	_, err = h.UpdateStackConfig(context.Background(), configValues(nil), apiv1.UpdateStackConfigParams{Name: "probe"})
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("UpdateStackConfig = %d %q, want 501 not_configured", status, code)
	}
}

func TestStackConfig_ValuesRoundTripAndTheSecretIsNeverInTheResponse(t *testing.T) {
	ctx := context.Background()
	h, root := installProbe(t)
	secret := strings.TrimSpace(strings.TrimPrefix(
		envLineStartingWith(t, readStackFile(t, root, "probe", ".env"), "PW="), "PW="))
	if secret == "" {
		t.Fatal("setup: the install wrote no secret")
	}

	cfg, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stack.Name != "probe" || cfg.Stack.ManuallyEdited {
		t.Errorf("stack = %+v", cfg.Stack)
	}
	if port := configInputOf(t, cfg, "PORT"); port.Value.Or("") != "8081" || port.ReadOnly {
		t.Errorf("PORT = %+v", port)
	}
	pw := configInputOf(t, cfg, "PW")
	if !pw.Set.Or(false) || pw.Value.Set {
		t.Errorf("PW = %+v, want set true and no value", pw)
	}
	if gpu := configInputOf(t, cfg, "GPU"); !gpu.ReadOnly {
		t.Errorf("GPU = %+v, want read-only", gpu)
	}

	updated, err := h.UpdateStackConfig(ctx, configValues(map[string]string{"PORT": "8090"}), apiv1.UpdateStackConfigParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if got := configInputOf(t, updated, "PORT").Value.Or(""); got != "8090" {
		t.Errorf("PORT after the update = %q", got)
	}
	env := readStackFile(t, root, "probe", ".env")
	if !strings.Contains(env, "PORT=8090\n") || !strings.Contains(env, "PW="+secret+"\n") {
		t.Errorf(".env = %q, want the new port and the same secret", env)
	}
	for name, v := range map[string]any{"get": cfg, "update": updated} {
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Errorf("the %s response contains the secret", name)
		}
	}
	again, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "probe"})
	if err != nil || configInputOf(t, again, "PORT").Value.Or("") != "8090" {
		t.Errorf("a later read = %+v, %v; want the stored change", again, err)
	}
}

func envLineStartingWith(t *testing.T, env, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(env, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("no %s line in %q", prefix, env)
	return ""
}

func TestStackConfig_AGeneratedSecretIsStoredAndNotReturned(t *testing.T) {
	ctx := context.Background()
	h, root := installProbe(t)
	before := readStackFile(t, root, "probe", ".env")
	cfg, err := h.UpdateStackConfig(ctx, &apiv1.UpdateStackConfigRequest{Generate: []string{"PW"}}, apiv1.UpdateStackConfigParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	after := readStackFile(t, root, "probe", ".env")
	oldPW, newPW := envLineStartingWith(t, before, "PW="), envLineStartingWith(t, after, "PW=")
	if oldPW == newPW {
		t.Error("the secret was not replaced")
	}
	body, _ := json.Marshal(cfg)
	if strings.Contains(string(body), strings.TrimPrefix(newPW, "PW=")) {
		t.Error("the response contains the generated secret")
	}
}

func TestStackConfig_RefusalsLeaveTheEnvByteIdentical(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		req    *apiv1.UpdateStackConfigRequest
		status int
		code   string
	}{
		{"port out of range", configValues(map[string]string{"PORT": "70000"}), 400, "invalid_template_input"},
		{"input the stack lacks", configValues(map[string]string{"NOPE": "1"}), 400, "invalid_template_input"},
		{"a device change", configValues(map[string]string{"GPU": "/dev/dri/renderD128"}), 400, "invalid_template_input"},
		{"generate of a non-secret", &apiv1.UpdateStackConfigRequest{Generate: []string{"PORT"}}, 400, "invalid_template_input"},
		{"a port another container takes", configValues(map[string]string{"PORT": "9000"}), 409, "no_free_port"},
	} {
		h, root := installProbe(t)
		h.TemplateInstall.Ports = tplPorts{used: map[int]bool{8080: true, 8081: true, 9000: true}}
		before := readStackFile(t, root, "probe", ".env")
		_, err := h.UpdateStackConfig(ctx, tc.req, apiv1.UpdateStackConfigParams{Name: "probe"})
		if status, code := statusOf(h, err); status != tc.status || code != tc.code {
			t.Errorf("%s = %d %q, want %d %q", tc.name, status, code, tc.status, tc.code)
		}
		if got := readStackFile(t, root, "probe", ".env"); got != before {
			t.Errorf("%s changed the .env:\n%s", tc.name, got)
		}
	}
}

func TestStackConfig_ErrorsHaveTheirOwnStatusAndCode(t *testing.T) {
	ctx := context.Background()
	h, _ := installProbe(t)
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "plain", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		stack  string
		status int
		code   string
	}{
		{"unknown stack", "missing", 404, "stack_not_found"},
		{"bad name", "../x", 400, "invalid_stack_name"},
		{"a stack made without a template", "plain", 409, "stack_has_no_template"},
	} {
		_, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: tc.stack})
		if status, code := statusOf(h, err); status != tc.status || code != tc.code {
			t.Errorf("get: %s = %d %q, want %d %q", tc.name, status, code, tc.status, tc.code)
		}
		_, err = h.UpdateStackConfig(ctx, configValues(nil), apiv1.UpdateStackConfigParams{Name: tc.stack})
		if status, code := statusOf(h, err); status != tc.status || code != tc.code {
			t.Errorf("update: %s = %d %q, want %d %q", tc.name, status, code, tc.status, tc.code)
		}
	}
}

func TestStackConfig_AManuallyEditedComposeFileIsNotTouchedByAnUpdate(t *testing.T) {
	ctx := context.Background()
	h, root := installProbe(t)
	installed := readStackFile(t, root, "probe", "docker-compose.yml")
	edited := strings.Replace(installed, "image: x:1", "image: x:2", 1)
	if _, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: edited}, apiv1.UpdateStackParams{Name: "probe"}); err != nil {
		t.Fatal(err)
	}

	cfg, err := h.UpdateStackConfig(ctx, configValues(map[string]string{"PORT": "8095"}), apiv1.UpdateStackConfigParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Stack.ManuallyEdited {
		t.Error("the config does not say the stack was edited by hand")
	}
	if got := readStackFile(t, root, "probe", "docker-compose.yml"); got != edited {
		t.Errorf("the Compose file was rewritten:\n%s", got)
	}
	stack, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "probe"})
	if err != nil || stack.Compose.Or("") != edited || !stack.ManuallyEdited {
		t.Errorf("the row's Compose text = %q (edited %v), %v; want the manual edit", stack.Compose.Or(""), stack.ManuallyEdited, err)
	}
}

func TestStackConfig_TheRowAndTheFileAgreeAfterAnUpdate(t *testing.T) {
	ctx := context.Background()
	h, root := installProbe(t)
	if _, err := h.UpdateStackConfig(ctx, configValues(map[string]string{"PORT": "8095"}), apiv1.UpdateStackConfigParams{Name: "probe"}); err != nil {
		t.Fatal(err)
	}
	want, err := h.Stacks.Env(ctx, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if got := readStackFile(t, root, "probe", ".env"); got != want || !strings.Contains(want, "PORT=8095\n") {
		t.Errorf("file %q, row %q", got, want)
	}
}

// The settings of an installed stack hold generated secrets' presence and the
// values a stack runs with, so reading them is an admin operation too.
func TestStackConfig_ReadingAndChangingAreAdminOperations(t *testing.T) {
	for _, op := range []apiv1.OperationName{apiv1.GetStackConfigOperation, apiv1.UpdateStackConfigOperation} {
		if got, ok := api.RoleFor(op); !ok || got != api.RoleAdmin {
			t.Errorf("%s needs %v (known %v), want admin", op, got, ok)
		}
	}
}
