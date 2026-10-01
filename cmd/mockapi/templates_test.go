package main

import (
	"context"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestMockInstallMovesAConflictingPortAndRecordsTheStack(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	for _, in := range res.Plan.Inputs {
		if in.Name == "WEBUI_PORT" && (in.Value.Or("") != "8097" || in.RequestedValue.Or("") != "8096") {
			t.Errorf("WEBUI_PORT = %+v, want 8097 because the mock's own jellyfin container publishes 8096", in)
		}
	}
	stack, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "jellyfin"})
	if err != nil || stack.Template.ID != "jellyfin" || stack.Template.Revision != "1" {
		t.Errorf("GetStack = %+v, %v", stack, err)
	}
}

func TestMockPreviewReportsThePrivilegesAndKeepsSecretsOut(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "risky-agent"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[apiv1.TemplatePrivilegeKind]bool{}
	for _, p := range plan.Privileges {
		kinds[p.Kind] = true
	}
	if !kinds[apiv1.TemplatePrivilegeKindPrivileged] || !kinds[apiv1.TemplatePrivilegeKindHostNetwork] || !kinds[apiv1.TemplatePrivilegeKindDockerSocket] ||
		!kinds[apiv1.TemplatePrivilegeKindAddedCapabilities] || !kinds[apiv1.TemplatePrivilegeKindConfinementDisabled] {
		t.Errorf("privileges = %+v", plan.Privileges)
	}
	notes, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{}, apiv1.PreviewTemplateInstallParams{ID: "aio-notes"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range notes.Inputs {
		if in.Name == "DB_PASSWORD" && (!in.Generated || in.Value.Set) {
			t.Errorf("DB_PASSWORD = %+v, want generated with no value", in)
		}
	}
	if _, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "aio-notes"}); err == nil {
		t.Error("a preview created a stack")
	}
}
