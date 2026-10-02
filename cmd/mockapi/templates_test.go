package main

import (
	"context"
	"io"
	"testing"
	"time"

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

func TestMockCatalogMarksAnInstalledTemplateAndNamesItsSource(t *testing.T) {
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
			if want := map[bool]string{true: mockExtrasSourceID, false: template.SourceCurated}[e.ID == "quickpaste"]; e.Source != want {
				t.Errorf("%s: source = %q, want %q", e.ID, e.Source, want)
			}
		}
		return out
	}
	before := installed()
	if len(before) != 4 || before["aio-notes"] || before["risky-agent"] {
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

func TestMockRefreshCatalogScriptsEveryOutcomeAndTheListReportsTheLastCheck(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list.LastCheckedAt.IsSet() || list.LastOutcome.IsSet() {
		t.Fatalf("the list reports a check before any ran: %+v %+v", list.LastCheckedAt, list.LastOutcome)
	}

	for i, want := range []apiv1.CatalogCheckOutcome{
		apiv1.CatalogCheckOutcomeUpdated, apiv1.CatalogCheckOutcomeUnchanged, apiv1.CatalogCheckOutcomeFailed,
		apiv1.CatalogCheckOutcomeUpdated,
	} {
		res, err := h.RefreshCatalog(ctx)
		if err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		if res.Outcome != want || res.CheckedAt.IsZero() {
			t.Fatalf("check %d = %+v, want %s", i, res, want)
		}
		list, err := h.ListCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if o, ok := list.LastOutcome.Get(); !ok || o != want {
			t.Fatalf("check %d: lastOutcome = %v, %v", i, o, ok)
		}
		if at, ok := list.LastCheckedAt.Get(); !ok || !at.Equal(res.CheckedAt) {
			t.Fatalf("check %d: lastCheckedAt = %v, %v, want %v", i, at, ok, res.CheckedAt)
		}
	}
}

// TestMockRefreshCatalogAnswersTheBodyTheProductionHandlerBuilds runs each
// outcome through the production handler and the mock and compares what each
// puts in the response, so the mock cannot drift on which fields an outcome
// carries.
func TestMockRefreshCatalogAnswersTheBodyTheProductionHandlerBuilds(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	outcomes := []template.CheckResult{
		{CheckedAt: at, Outcome: template.OutcomeUpdated, New: 2, Updated: 1},
		{CheckedAt: at, Outcome: template.OutcomeUnchanged},
		{CheckedAt: at, Outcome: template.OutcomeFailed, Reason: template.ReasonFetchFailed, Message: "x"},
	}
	for _, want := range outcomes {
		prod := &api.Handler{CatalogRefresh: fixedRefresher{want}}
		p, err := prod.RefreshCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		m, err := h.RefreshCatalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if p.Outcome != m.Outcome || p.NewTemplates.IsSet() != m.NewTemplates.IsSet() || p.UpdatedTemplates.IsSet() != m.UpdatedTemplates.IsSet() ||
			p.Reason.IsSet() != m.Reason.IsSet() || p.Message.IsSet() != m.Message.IsSet() {
			t.Errorf("%s: production sets %+v, the mock %+v", want.Outcome, p, m)
		}
	}
}

type fixedRefresher struct{ res template.CheckResult }

func (f fixedRefresher) Refresh(context.Context) (template.CheckResult, error) { return f.res, nil }
func (f fixedRefresher) Last() (template.CheckResult, bool)                    { return f.res, true }

func TestMockCatalogSettingsStartDailyAndOnAndChangeOneFieldAtATime(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := h.GetCatalogSettings(ctx); err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval24h || !got.CheckOnOpen {
		t.Fatalf("GetCatalogSettings = %+v, %v, want 24h and on", got, err)
	}
	if got, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{CheckOnOpen: apiv1.NewOptBool(false)}); err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval24h || got.CheckOnOpen {
		t.Fatalf("check-on-open only = %+v, %v", got, err)
	}
	if got, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval(apiv1.CatalogRefreshInterval12h)}); err != nil || got.RefreshInterval != apiv1.CatalogRefreshInterval12h || got.CheckOnOpen {
		t.Fatalf("interval only = %+v, %v", got, err)
	}
	_, err = h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{RefreshInterval: apiv1.NewOptCatalogRefreshInterval("2h"), CheckOnOpen: apiv1.NewOptBool(true)})
	if err == nil || mockErrorCode(t, err) != "invalid_catalog_interval" {
		t.Fatalf("an unknown interval = %v, want invalid_catalog_interval", err)
	}
	if got, _ := h.GetCatalogSettings(ctx); got.RefreshInterval != apiv1.CatalogRefreshInterval12h || got.CheckOnOpen {
		t.Fatalf("a refused update changed the settings: %+v", got)
	}
}

func TestMockCatalogHasAnEntryWithEveryPieceOfMetadataAndServesItsScreenshots(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "jellyfin"})
	if err != nil {
		t.Fatal(err)
	}
	links, ok := d.Links.Get()
	if d.Maintainer.Or("") == "" || d.Description.Or("") == "" || d.ScreenshotCount != 2 || !ok ||
		links.Project.Or("") == "" || links.Support.Or("") == "" || links.Donate.Or("") == "" {
		t.Fatalf("jellyfin = %+v, want every piece of metadata", d)
	}
	plain, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "risky-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Maintainer.IsSet() || plain.Description.IsSet() || plain.Links.IsSet() || plain.ScreenshotCount != 0 {
		t.Errorf("risky-agent = %+v, want none", plain)
	}
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	maintainers := map[string]string{}
	descriptions := map[string]string{}
	for _, e := range list.Templates {
		maintainers[e.ID] = e.Maintainer.Or("")
		descriptions[e.ID] = e.Description.Or("")
	}
	if descriptions["jellyfin"] != d.Description.Or("") || descriptions["risky-agent"] != "" {
		t.Errorf("descriptions = %v, want the detail's description on jellyfin and none on risky-agent", descriptions)
	}
	if maintainers["jellyfin"] != d.Maintainer.Or("") || maintainers["quickpaste"] == "" || maintainers["risky-agent"] != "" {
		t.Errorf("maintainers = %v", maintainers)
	}

	res, err := h.GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "jellyfin", Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	png, ok := res.(*apiv1.GetCatalogTemplateScreenshotOKImagePNGHeaders)
	if !ok {
		t.Fatalf("response is %T, want the PNG response", res)
	}
	prod, err := (&api.Handler{Catalog: mockCatalog()}).GetCatalogTemplateScreenshot(ctx, apiv1.GetCatalogTemplateScreenshotParams{ID: "jellyfin", Index: 0})
	if err != nil {
		t.Fatal(err)
	}
	want := prod.(*apiv1.GetCatalogTemplateScreenshotOKImagePNGHeaders)
	if png.ContentSecurityPolicy != want.ContentSecurityPolicy || png.XContentTypeOptions != want.XContentTypeOptions {
		t.Errorf("headers = %q %q, production %q %q", png.ContentSecurityPolicy, png.XContentTypeOptions, want.ContentSecurityPolicy, want.XContentTypeOptions)
	}
	got, _ := io.ReadAll(png.Response)
	if string(got) != string(mockScreenshots["jellyfin"][0]) {
		t.Errorf("body is %d bytes, want the first mock screenshot", len(got))
	}
}

func TestMockNetworkModeMirrorsProduction(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	nets, err := h.ListDockerNetworks(ctx)
	if err != nil || !nets.Available || len(nets.Networks) != len(mockNetworkList) {
		t.Fatalf("ListDockerNetworks = %+v, %v", nets, err)
	}
	preview := func(mode string) *apiv1.TemplateInstallPlan {
		t.Helper()
		plan, err := h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString(mode)}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
		if err != nil {
			t.Fatalf("PreviewTemplateInstall(%s): %v", mode, err)
		}
		return plan
	}
	if plan := preview("lan"); len(plan.Warnings) != 0 {
		t.Errorf("a network the mock lists has warnings %+v", plan.Warnings)
	}
	plan := preview("iot")
	if len(plan.Warnings) != 1 || plan.Warnings[0].Class != apiv1.ConversionWarningClassMissingNetwork || plan.Warnings[0].Command.Or("") != "docker network create iot" {
		t.Errorf("warnings = %+v, want the missing network with its exact command", plan.Warnings)
	}
	_, err = h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{NetworkMode: apiv1.NewOptString("iot")}, apiv1.InstallTemplateParams{ID: "jellyfin"})
	if code := mockErrorCode(t, err); code != "network_missing" {
		t.Errorf("install on a missing network: %v, want network_missing", err)
	}
	if _, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "jellyfin"}); err == nil {
		t.Error("a refused install recorded a stack")
	}

	_, err = h.PreviewTemplateInstall(ctx, &apiv1.TemplateInstallRequest{MemoryMiB: apiv1.NewOptInt(2)}, apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
	resp := h.NewError(ctx, err)
	if got, ok := resp.Response.Details.Get(); resp.Response.Code != "invalid_template_input" || !ok || string(got["input"]) != `"memoryMiB"` {
		t.Errorf("a memory limit out of range: %d %q %v, want details.input memoryMiB", resp.StatusCode, resp.Response.Code, got)
	}
}

func TestMockRefusesAnExtraParameterPortTheHostHolds(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := func(flags string) *apiv1.TemplateInstallRequest {
		return &apiv1.TemplateInstallRequest{ExtraParams: apiv1.NewOptString(flags)}
	}
	_, err = h.PreviewTemplateInstall(ctx, req("-p 9000:80"), apiv1.PreviewTemplateInstallParams{ID: "jellyfin"})
	resp := h.NewError(ctx, err)
	if got, ok := resp.Response.Details.Get(); resp.StatusCode != 409 || resp.Response.Code != "no_free_port" || !ok || string(got["input"]) != `"extraParams"` {
		t.Errorf("the scripted busy port in extra parameters: %d %q %v, want 409 no_free_port naming extraParams", resp.StatusCode, resp.Response.Code, got)
	}
	if _, err := h.PreviewTemplateInstall(ctx, req("-p 9100:80"), apiv1.PreviewTemplateInstallParams{ID: "jellyfin"}); err != nil {
		t.Errorf("a free port was refused: %v", err)
	}
}
