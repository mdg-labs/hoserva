package main

import (
	"context"
	"strconv"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockSeedsOneStackThatIsAlreadyManuallyEdited(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := h.GetStack(ctx, apiv1.GetStackParams{Name: mockStack})
	if err != nil || !s.ManuallyEdited || s.Compose.Or("") == "" {
		t.Fatalf("GetStack(%s) = %+v, %v; want a manually edited stack with its Compose text", mockStack, s, err)
	}
	list, err := h.ListStacks(ctx)
	if err != nil || len(list.Stacks) != 1 || !list.Stacks[0].ManuallyEdited || list.Stacks[0].Compose.IsSet() {
		t.Fatalf("ListStacks = %+v, %v; want the flag and no Compose text", list, err)
	}
}

func TestMockUpdateStackValidatesStoresAndSetsTheFlag(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "nginx", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	if s, _ := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"}); s.ManuallyEdited {
		t.Fatal("a created stack is manually edited")
	}
	const edited = "services:\n  web:\n    image: nginx:1.28\n    ports:\n      - \"8081:80\"\n"

	_, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: "services: [\n"}, apiv1.UpdateStackParams{Name: "nginx"})
	if e := h.NewError(ctx, err); e.StatusCode != 400 || e.Response.Code != "invalid_stack" {
		t.Fatalf("an invalid file = %d %q, want 400 invalid_stack", e.StatusCode, e.Response.Code)
	}
	if s, _ := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"}); s.ManuallyEdited || s.Compose.Or("") != "services: {}\n" {
		t.Fatalf("a refused edit changed the stack: %+v", s)
	}

	res, err := h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: edited}, apiv1.UpdateStackParams{Name: "nginx", DryRun: apiv1.NewOptBool(true)})
	if err != nil || res.Applied {
		t.Fatalf("dry run = %+v, %v; want applied false", res, err)
	}
	if s, _ := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"}); s.ManuallyEdited || strings.Contains(s.Compose.Or(""), "1.28") {
		t.Fatalf("a dry run changed the stack: %+v", s)
	}

	res, err = h.UpdateStack(ctx, &apiv1.UpdateStackRequest{Compose: edited}, apiv1.UpdateStackParams{Name: "nginx"})
	if err != nil || !res.Applied || !res.Stack.ManuallyEdited {
		t.Fatalf("UpdateStack = %+v, %v", res, err)
	}
	if s, _ := h.GetStack(ctx, apiv1.GetStackParams{Name: "nginx"}); !s.ManuallyEdited || s.Compose.Or("") != edited {
		t.Fatalf("GetStack after an edit = %+v", s)
	}
	h.stacksMu.Lock()
	published := h.stackPorts["nginx"][8081]
	h.stacksMu.Unlock()
	if !published {
		t.Fatal("the stack's ports were not worked out again from the edited text")
	}
}

func TestMockStartStackQueuesAStackStartJob(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	j, err := h.StartStack(context.Background(), apiv1.StartStackParams{Name: mockStack})
	if err != nil {
		t.Fatalf("StartStack: %v", err)
	}
	if j.Type != apiv1.JobTypeStackStart || j.Class != apiv1.JobClassService || j.Status != apiv1.JobStatusQueued {
		t.Fatalf("job = %+v, want a queued stack_start in the service class", j)
	}
}

func configInputByName(t *testing.T, c *apiv1.StackConfig, name string) apiv1.StackConfigInput {
	t.Helper()
	for _, in := range c.Inputs {
		if in.Name == name {
			return in
		}
	}
	t.Fatalf("no input %s in %+v", name, c.Inputs)
	return apiv1.StackConfigInput{}
}

func TestMockStackConfigSeededStackHasAPortAndASecretAndANeverReturnedValue(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg, err := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: mockStack})
	if err != nil {
		t.Fatalf("GetStackConfig(%s): %v", mockStack, err)
	}
	if !cfg.Stack.ManuallyEdited {
		t.Error("the seeded stack is not shown as edited by hand")
	}
	if port := configInputByName(t, cfg, "WEBUI_PORT"); port.Value.Or("") != "8096" || port.Kind != apiv1.StackConfigInputKindPort {
		t.Errorf("WEBUI_PORT = %+v", port)
	}
	pw := configInputByName(t, cfg, "ADMIN_PASSWORD")
	if !pw.Set.Or(false) || pw.Value.Set {
		t.Errorf("ADMIN_PASSWORD = %+v, want set and no value", pw)
	}
	body, err := cfg.MarshalJSON()
	if err != nil || strings.Contains(string(body), "mock-admin-password") {
		t.Errorf("the response gives the secret away: %s, %v", body, err)
	}
}

func TestMockUpdateStackConfigValidatesLikeProductionAndKeepsTheComposeText(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	before, err := h.GetStack(ctx, apiv1.GetStackParams{Name: mockStack})
	if err != nil {
		t.Fatal(err)
	}
	update := func(values map[string]string, generate ...string) (*apiv1.StackConfig, error) {
		req := &apiv1.UpdateStackConfigRequest{Generate: generate}
		if values != nil {
			req.Values = apiv1.NewOptUpdateStackConfigRequestValues(values)
		}
		return h.UpdateStackConfig(ctx, req, apiv1.UpdateStackConfigParams{Name: mockStack})
	}

	_, err = update(map[string]string{"WEBUI_PORT": strconv.Itoa(mockBusyPort)})
	if e := h.NewError(ctx, err); e.StatusCode != 409 || e.Response.Code != "no_free_port" {
		t.Fatalf("the scripted busy port = %d %q, want 409 no_free_port", e.StatusCode, e.Response.Code)
	}
	if cfg, _ := h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: mockStack}); configInputByName(t, cfg, "WEBUI_PORT").Value.Or("") != "8096" {
		t.Error("a refused port changed the stack")
	}
	_, err = update(map[string]string{"WEBUI_PORT": "0"})
	if e := h.NewError(ctx, err); e.StatusCode != 400 || e.Response.Code != "invalid_template_input" {
		t.Fatalf("a port out of range = %d %q, want 400 invalid_template_input", e.StatusCode, e.Response.Code)
	}

	cfg, err := update(map[string]string{"WEBUI_PORT": "8200"}, "ADMIN_PASSWORD")
	if err != nil {
		t.Fatalf("a valid change: %v", err)
	}
	if configInputByName(t, cfg, "WEBUI_PORT").Value.Or("") != "8200" || !configInputByName(t, cfg, "ADMIN_PASSWORD").Set.Or(false) {
		t.Errorf("config after the change = %+v", cfg.Inputs)
	}
	after, err := h.GetStack(ctx, apiv1.GetStackParams{Name: mockStack})
	if err != nil || after.Compose.Or("") != before.Compose.Or("") || !after.ManuallyEdited {
		t.Errorf("the Compose text or the edited flag changed: %+v, %v", after, err)
	}
	h.stacksMu.Lock()
	published := h.stackPorts[mockStack][8200] && !h.stackPorts[mockStack][8096]
	h.stacksMu.Unlock()
	if !published {
		t.Error("the stack's ports were not worked out again from the new .env")
	}
}

func TestMockStackConfigOfAStackWithoutATemplateIs409(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.CreateStack(ctx, &apiv1.CreateStackRequest{Name: "plain", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	_, err = h.GetStackConfig(ctx, apiv1.GetStackConfigParams{Name: "plain"})
	if e := h.NewError(ctx, err); e.StatusCode != 409 || e.Response.Code != "stack_has_no_template" {
		t.Fatalf("GetStackConfig = %d %q, want 409 stack_has_no_template", e.StatusCode, e.Response.Code)
	}
}
