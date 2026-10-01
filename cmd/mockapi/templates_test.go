package main

import (
	"context"
	"io"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/template"
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

func TestMockInstallsInARowAreGivenDifferentPortsAndARemovedStackFreesItsPort(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	port := func(name string) string {
		t.Helper()
		res, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString(name)}, apiv1.InstallTemplateParams{ID: "jellyfin"})
		if err != nil {
			t.Fatalf("InstallTemplate %s: %v", name, err)
		}
		for _, in := range res.Plan.Inputs {
			if in.Name == "WEBUI_PORT" {
				return in.Value.Or("")
			}
		}
		t.Fatal("no WEBUI_PORT input")
		return ""
	}
	if a, b := port("jellyfin"), port("jellyfin-2"); a != "8097" || b != "8098" {
		t.Fatalf("ports = %s, %s, want 8097 then 8098: 8096 is the running container's and nothing was started", a, b)
	}
	if _, err := h.RemoveStack(ctx, apiv1.RemoveStackParams{Name: "jellyfin"}); err != nil {
		t.Fatal(err)
	}
	if got := port("jellyfin"); got != "8097" {
		t.Errorf("port after removing the stack = %s, want 8097 again", got)
	}
}

func TestComposePortsSubstitutesTheEnvAndSkipsWhatTheEngineChooses(t *testing.T) {
	compose := "services:\n  web:\n    ports:\n      - ${PORT}:80\n      - \"127.0.0.1:${OTHER:-7000}:70\"\n      - \"81\"\n      - target: 82\n        published: ${LONG}\n  db:\n    image: x\n"
	got := composePorts(compose, "PORT=8096\nLONG=\"9000\"\n")
	if len(got) != 3 || !got[8096] || !got[7000] || !got[9000] {
		t.Errorf("composePorts = %v, want 8096, 7000 and 9000", got)
	}
	if got := composePorts("services: [", ""); len(got) != 0 {
		t.Errorf("composePorts of an unreadable file = %v, want none", got)
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

func TestMockCatalogMarksAnInstalledTemplateAndNamesTheCuratedSource(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	installed := func() map[string]bool {
		t.Helper()
		list, err := h.ListCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, e := range list.Templates {
			out[e.ID] = e.Installed
			if e.Source != template.SourceCurated {
				t.Errorf("%s: source = %q", e.ID, e.Source)
			}
		}
		return out
	}
	before := installed()
	if len(before) != 3 || before["aio-notes"] || before["risky-agent"] {
		t.Fatalf("before = %v", before)
	}
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{Name: apiv1.NewOptString("notes")}, apiv1.InstallTemplateParams{ID: "aio-notes"}); err != nil {
		t.Fatal(err)
	}
	if after := installed(); !after["aio-notes"] || after["risky-agent"] {
		t.Errorf("after installing aio-notes = %v", after)
	}
}

func TestMockCatalogDetailReportsThePrivilegesTheProductionSummaryComputes(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.GetCatalogTemplate(context.Background(), apiv1.GetCatalogTemplateParams{ID: "risky-agent"})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[apiv1.TemplatePrivilegeKind]bool{}
	for _, p := range d.Privileges {
		kinds[p.Kind] = true
	}
	if !kinds[apiv1.TemplatePrivilegeKindPrivileged] || !kinds[apiv1.TemplatePrivilegeKindDockerSocket] || d.Compose != mockTemplates["risky-agent"] {
		t.Errorf("detail = %+v", d)
	}
}

func TestMockCatalogIconHasTheProductionHeaders(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.GetCatalogTemplateIcon(context.Background(), apiv1.GetCatalogTemplateIconParams{ID: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	svg, ok := res.(*apiv1.GetCatalogTemplateIconOKImageSvgXMLHeaders)
	if !ok {
		t.Fatalf("response is %T, want the SVG response", res)
	}
	prod, err := (&api.Handler{Catalog: mockCatalog()}).GetCatalogTemplateIcon(context.Background(), apiv1.GetCatalogTemplateIconParams{ID: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	want := prod.(*apiv1.GetCatalogTemplateIconOKImageSvgXMLHeaders)
	if svg.ContentSecurityPolicy != want.ContentSecurityPolicy || svg.XContentTypeOptions != want.XContentTypeOptions {
		t.Errorf("headers = %q %q, production %q %q", svg.ContentSecurityPolicy, svg.XContentTypeOptions, want.ContentSecurityPolicy, want.XContentTypeOptions)
	}
	got, _ := io.ReadAll(svg.Response)
	if string(got) != string(mockIcons["jellyfin"]) {
		t.Errorf("body = %q", got)
	}
}
