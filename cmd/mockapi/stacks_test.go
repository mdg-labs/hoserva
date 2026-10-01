package main

import (
	"context"
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
