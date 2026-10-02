package api_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/store"
	"github.com/mdg-labs/hoserva/internal/template"
)

type fakeSources struct {
	list       []template.SourceStatus
	added      []template.AddRequest
	removed    []string
	refreshed  []string
	addErr     error
	removeErr  error
	refreshErr error
	result     template.CheckResult
	catalog    template.Catalog
	fromErr    error
}

func (f *fakeSources) List(context.Context) ([]template.SourceStatus, error) { return f.list, nil }

func (f *fakeSources) Add(_ context.Context, req template.AddRequest) (template.SourceStatus, error) {
	f.added = append(f.added, req)
	if f.addErr != nil {
		return template.SourceStatus{}, f.addErr
	}
	return template.SourceStatus{ID: "src-0123456789", URL: req.URL, Kind: store.CatalogSourceUserAdded, Signed: req.PublicKey != ""}, nil
}

func (f *fakeSources) Refresh(_ context.Context, id string) (template.CheckResult, error) {
	f.refreshed = append(f.refreshed, id)
	return f.result, f.refreshErr
}

func (f *fakeSources) Remove(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}

func (f *fakeSources) From(context.Context, string) (template.Catalog, error) {
	return f.catalog, f.fromErr
}

func TestCatalogSources_NotConfiguredIs501(t *testing.T) {
	h := newCatalogHandler(t)
	ctx := context.Background()
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "agent", Compose: "services: {}\n"}); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"ListCatalogSources": func() error { _, err := h.ListCatalogSources(ctx); return err },
		"AddCatalogSource": func() error {
			_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://x.example"})
			return err
		},
		"RefreshCatalogSource": func() error {
			_, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: "x"})
			return err
		},
		"RemoveCatalogSource": func() error { return h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: "x"}) },
		"GetStackTemplateUpdate": func() error {
			_, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "agent"})
			return err
		},
	}
	for name, call := range calls {
		if status, code := statusOf(h, call()); status != 501 || code != "not_configured" {
			t.Errorf("%s = %d %q, want 501 not_configured", name, status, code)
		}
	}
}

func TestCatalogSources_ErrorsMapToTheirStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: x", template.ErrInvalidSource), 400, "invalid_catalog_source"},
		{fmt.Errorf("%w: x", template.ErrSourceExists), 409, "catalog_source_exists"},
		{fmt.Errorf("%w: x", template.ErrSourceNotFound), 404, "catalog_source_not_found"},
		{template.ErrSourceCurated, 409, "catalog_source_curated"},
		{&template.SourceCheckError{Result: template.CheckResult{Reason: template.ReasonFetchFailed, Message: "down"}}, 502, "catalog_source_unreachable"},
		{&template.SourceCheckError{Result: template.CheckResult{Reason: template.ReasonBadSignature, Message: "bad"}}, 422, "catalog_source_rejected"},
		{&template.SourceCheckError{Result: template.CheckResult{Reason: template.ReasonInstallFailed, Message: "disk"}}, 500, ""},
	}
	for _, c := range cases {
		h := newCatalogHandler(t)
		h.CatalogSources = &fakeSources{addErr: c.err, removeErr: c.err, refreshErr: c.err}
		for name, err := range map[string]error{
			"add": func() error {
				_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://x.example"})
				return err
			}(),
			"remove": h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: "x"}),
			"refresh": func() error {
				_, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: "x"})
				return err
			}(),
		} {
			status, code := statusOf(h, err)
			if status != c.status || (c.code != "" && code != c.code) {
				t.Errorf("%s with %v = %d %q, want %d %q", name, c.err, status, code, c.status, c.code)
			}
		}
	}
}

func TestCatalogSources_ListAddRefreshRemovePassTheRequestThrough(t *testing.T) {
	ctx := context.Background()
	h := newCatalogHandler(t)
	at := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	fake := &fakeSources{
		list: []template.SourceStatus{
			{ID: "hoserva", URL: "https://catalog.hoserva.dev", Kind: store.CatalogSourceCurated, Signed: true, Serial: 7, LastRefreshedAt: at},
			{ID: "src-0123456789", URL: "https://example.com", Kind: store.CatalogSourceUserAdded},
		},
		result: template.CheckResult{CheckedAt: at, Outcome: template.OutcomeFailed, Reason: template.ReasonBadSignature, Message: "no"},
	}
	h.CatalogSources = fake

	list, err := h.ListCatalogSources(ctx)
	if err != nil || len(list.Sources) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	c, u := list.Sources[0], list.Sources[1]
	if c.Kind != apiv1.CatalogSourceKindCurated || !c.Signed || c.Serial.Or(0) != 7 || !c.LastRefreshedAt.IsSet() {
		t.Errorf("curated = %+v", c)
	}
	if u.Kind != apiv1.CatalogSourceKindUserAdded || u.Signed || u.Serial.IsSet() || u.LastRefreshedAt.IsSet() {
		t.Errorf("user-added = %+v, want unsigned with no serial and no refresh time", u)
	}

	added, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://example.com", PublicKey: apiv1.NewOptString("KEY")})
	if err != nil || !added.Signed || added.Kind != apiv1.CatalogSourceKindUserAdded || len(fake.added) != 1 || fake.added[0].PublicKey != "KEY" {
		t.Fatalf("added = %+v, %v, requests %+v", added, err, fake.added)
	}
	unsigned, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://example.org"})
	if err != nil || unsigned.Signed || fake.added[1].PublicKey != "" {
		t.Fatalf("unsigned = %+v, %v, requests %+v", unsigned, err, fake.added)
	}

	for _, blank := range []string{"", " \n"} {
		_, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://example.net", PublicKey: apiv1.NewOptString(blank)})
		if status, code := statusOf(h, err); status != 400 || code != "invalid_catalog_source" {
			t.Errorf("a blank key %q = %d %q, want 400 invalid_catalog_source", blank, status, code)
		}
	}
	if len(fake.added) != 2 {
		t.Fatalf("a blank key reached the service: %+v", fake.added)
	}

	res, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: "src-0123456789"})
	if err != nil || res.Outcome != apiv1.CatalogCheckOutcomeFailed || res.Reason.Or("") != apiv1.CatalogRefreshReasonBadSignature || res.Message.Or("") != "no" || len(fake.refreshed) != 1 {
		t.Fatalf("refresh = %+v, %v", res, err)
	}
	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: "src-0123456789"}); err != nil || len(fake.removed) != 1 || fake.removed[0] != "src-0123456789" {
		t.Fatalf("remove = %v, %v", err, fake.removed)
	}
}

func TestCatalog_EntriesCarryTheirSourcesBadgeAsData(t *testing.T) {
	ctx := context.Background()
	h := newCatalogHandler(t)
	for name, c := range map[string]struct {
		kind       store.CatalogSourceKind
		signed     bool
		wantKind   apiv1.CatalogSourceKind
		wantSigned bool
	}{
		"curated":             {store.CatalogSourceCurated, true, apiv1.CatalogSourceKindCurated, true},
		"user-added signed":   {store.CatalogSourceUserAdded, true, apiv1.CatalogSourceKindUserAdded, true},
		"user-added unsigned": {store.CatalogSourceUserAdded, false, apiv1.CatalogSourceKindUserAdded, false},
		"a catalog saying nothing about its trust": {"", true, apiv1.CatalogSourceKindUserAdded, false},
	} {
		h.Catalog = template.MapCatalog{Source: "src-0123456789", Kind: c.kind, Signed: c.signed, Templates: map[string]string{"probe": tplTemplate("probe", "")}}
		list, err := h.ListCatalog(ctx)
		if err != nil || len(list.Templates) != 1 {
			t.Fatalf("%s: %+v, %v", name, list, err)
		}
		if e := list.Templates[0]; e.SourceKind != c.wantKind || e.Signed != c.wantSigned || e.Source != "src-0123456789" {
			t.Errorf("%s: entry = %+v", name, e)
		}
		d, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "probe"})
		if err != nil || d.SourceKind != c.wantKind || d.Signed != c.wantSigned {
			t.Errorf("%s: detail = %+v, %v", name, d, err)
		}
	}
}

func templateUpdateHandler(t *testing.T) (*api.Handler, *fakeSources) {
	t.Helper()
	h := newCatalogHandler(t)
	fake := &fakeSources{catalog: template.MapCatalog{
		Source: "src-0123456789", Kind: store.CatalogSourceUserAdded,
		Templates: map[string]string{"probe": tplTemplate("probe", "")},
	}}
	h.CatalogSources = fake
	return h, fake
}

func TestStackTemplateUpdate_ANewerRevisionIsOfferedAndTheStackIsNotChanged(t *testing.T) {
	ctx := context.Background()
	h, _ := templateUpdateHandler(t)
	installed := strings.Replace(tplTemplate("probe", ""), "revision: 4", "revision: 2", 1)
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "probe", Compose: installed, Env: "PORT=8080\n", TemplateSource: "src-0123456789", TemplateID: "probe", TemplateRevision: "2"}); err != nil {
		t.Fatal(err)
	}
	before, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}

	u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != apiv1.StackTemplateUpdateStatusUpdateAvailable || u.InstalledRevision.Or(0) != 2 || u.AvailableRevision.Or(0) != 4 || u.ManuallyEdited {
		t.Fatalf("update = %+v", u)
	}
	if u.SourceKind.Or("") != apiv1.CatalogSourceKindUserAdded || u.Signed.Or(true) {
		t.Fatalf("badge = %v signed %v, want user-added and unsigned", u.SourceKind, u.Signed)
	}
	if !strings.Contains(u.Diff.Or(""), "-  revision: 2\n+  revision: 4\n") {
		t.Fatalf("diff:\n%s", u.Diff.Or(""))
	}

	after, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Compose != before.Compose || after.ManuallyEdited != before.ManuallyEdited || after.Template != before.Template || !after.InstalledAt.Equal(before.InstalledAt) {
		t.Fatalf("computing the update changed the stack:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestStackTemplateUpdate_AManuallyEditedStackIsMarked(t *testing.T) {
	ctx := context.Background()
	h, _ := templateUpdateHandler(t)
	if _, err := h.Stacks.Create(ctx, container.NewStack{Name: "probe", Compose: "services: {}\n", TemplateSource: "src-0123456789", TemplateID: "probe", TemplateRevision: "1"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Stacks.Store.UpdateCompose(ctx, "probe", "services: {}\n# edited\n", true); err != nil {
		t.Fatal(err)
	}
	u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "probe"})
	if err != nil || u.Status != apiv1.StackTemplateUpdateStatusUpdateAvailable || !u.ManuallyEdited {
		t.Fatalf("update = %+v, %v, want available and manuallyEdited", u, err)
	}
	if !strings.Contains(u.Diff.Or(""), "-# edited") {
		t.Fatalf("the diff is not against the edited file:\n%s", u.Diff.Or(""))
	}
}

func TestStackTemplateUpdate_StatusesWithoutADiffAndErrors(t *testing.T) {
	ctx := context.Background()
	h, fake := templateUpdateHandler(t)
	for name, n := range map[string]container.NewStack{
		"current": {Name: "current", Compose: "services: {}\n", TemplateSource: "src-0123456789", TemplateID: "probe", TemplateRevision: "4"},
		"byhand":  {Name: "byhand", Compose: "services: {}\n"},
		"gone":    {Name: "gone", Compose: "services: {}\n", TemplateSource: "src-0123456789", TemplateID: "vanished", TemplateRevision: "1"},
	} {
		if _, err := h.Stacks.Create(ctx, n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, want := range map[string]apiv1.StackTemplateUpdateStatus{
		"current": apiv1.StackTemplateUpdateStatusUpToDate,
		"byhand":  apiv1.StackTemplateUpdateStatusNotFromTemplate,
		"gone":    apiv1.StackTemplateUpdateStatusTemplateRemoved,
	} {
		u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: name})
		if err != nil || u.Status != want || u.Diff.IsSet() {
			t.Errorf("%s: %+v, %v, want %s with no diff", name, u, err, want)
		}
	}

	fake.fromErr = fmt.Errorf("%w: gone", template.ErrSourceNotFound)
	u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "current"})
	if err != nil || u.Status != apiv1.StackTemplateUpdateStatusSourceRemoved {
		t.Fatalf("a removed source = %+v, %v", u, err)
	}

	fake.fromErr = template.ErrCatalogUnavailable
	_, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "current"})
	if status, code := statusOf(h, err); status != 503 || code != "catalog_unavailable" {
		t.Errorf("an unreadable catalog = %d %q, want 503 catalog_unavailable and never up_to_date", status, code)
	}
	fake.fromErr = nil

	_, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "missing"})
	if status, code := statusOf(h, err); status != 404 || code != "stack_not_found" {
		t.Errorf("an unknown stack = %d %q", status, code)
	}
	_, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "../etc"})
	if status, code := statusOf(h, err); status != 400 || code != "invalid_stack_name" {
		t.Errorf("an invalid name = %d %q", status, code)
	}
}
