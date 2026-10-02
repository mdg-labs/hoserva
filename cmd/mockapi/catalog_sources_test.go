package main

import (
	"context"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

func TestMockCatalogSourcesBadgeWhatTheyAreAndTheCuratedOneStays(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	unsigned, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com/a/"})
	if err != nil {
		t.Fatal(err)
	}
	signed, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: "https://catalog.example.com/b", PublicKey: apiv1.NewOptString(mockSourcePublicKey())})
	if err != nil {
		t.Fatal(err)
	}
	if unsigned.Kind != apiv1.CatalogSourceKindUserAdded || unsigned.Signed || signed.Kind != apiv1.CatalogSourceKindUserAdded || !signed.Signed {
		t.Fatalf("unsigned = %+v, signed = %+v", unsigned, signed)
	}
	list, err := h.ListCatalogSources(ctx)
	if err != nil || len(list.Sources) != 4 || list.Sources[0].ID != template.SourceCurated || list.Sources[0].Kind != apiv1.CatalogSourceKindCurated || list.Sources[1].ID != mockExtrasSourceID || list.Sources[2].ID != unsigned.ID || list.Sources[3].ID != signed.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: unsigned.ID}); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.ListCatalogSources(ctx); len(list.Sources) != 3 || list.Sources[2].ID != signed.ID {
		t.Fatalf("list after removal = %+v", list)
	}
}

func TestMockCatalogEntriesCarryTheirSourcesBadge(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := h.ListCatalog(ctx)
	if err != nil || len(list.Templates) == 0 {
		t.Fatalf("ListCatalog = %+v, %v", list, err)
	}
	var unsigned []string
	for _, e := range list.Templates {
		if e.ID == "quickpaste" {
			if e.Source != mockExtrasSourceID || e.SourceKind != apiv1.CatalogSourceKindUserAdded || e.Signed {
				t.Errorf("%s: %+v, want an unsigned user-added entry", e.ID, e)
			}
			unsigned = append(unsigned, e.ID)
			continue
		}
		if e.Source != template.SourceCurated || e.SourceKind != apiv1.CatalogSourceKindCurated || !e.Signed {
			t.Errorf("%s: %+v", e.ID, e)
		}
	}
	if len(unsigned) != 1 {
		t.Fatalf("the catalog lists %d unsigned entries, want the one scripted one", len(unsigned))
	}
	d, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "jellyfin"})
	if err != nil || d.SourceKind != apiv1.CatalogSourceKindCurated || !d.Signed {
		t.Fatalf("GetCatalogTemplate = %+v, %v", d, err)
	}
	q, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "quickpaste"})
	if err != nil || q.SourceKind != apiv1.CatalogSourceKindUserAdded || q.Signed || q.Source != mockExtrasSourceID {
		t.Fatalf("GetCatalogTemplate(quickpaste) = %+v, %v", q, err)
	}
	if len(q.Privileges) == 0 {
		t.Errorf("quickpaste asks for host networking, but its privilege summary is empty")
	}
	if _, err := h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "quickpaste"}); err != nil {
		t.Errorf("GetCatalogTemplateIcon(quickpaste): %v", err)
	}
}

func TestMockCatalogDropsTheScriptedEntryWithItsSource(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: mockExtrasSourceID}); err != nil {
		t.Fatal(err)
	}
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Templates {
		if e.ID == "quickpaste" {
			t.Errorf("quickpaste is still listed after its source was removed")
		}
	}
	if _, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "quickpaste"}); err == nil {
		t.Errorf("GetCatalogTemplate(quickpaste) succeeded after its source was removed")
	}
}

func TestMockInstallsAUserAddedTemplateAndRecordsItsSource(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	res, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "quickpaste"})
	if err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	if res.Stack.Template.Source != mockExtrasSourceID {
		t.Errorf("stack source = %q, want %q", res.Stack.Template.Source, mockExtrasSourceID)
	}
	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Templates {
		if e.ID == "quickpaste" && !e.Installed {
			t.Errorf("quickpaste is not reported installed after its install")
		}
	}
}

func TestMockTemplateUpdateMarksAManuallyEditedStack(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := h.ListStacks(ctx)
	if err != nil || len(list.Stacks) == 0 {
		t.Fatalf("ListStacks = %+v, %v", list, err)
	}
	var edited string
	for _, s := range list.Stacks {
		if s.ManuallyEdited {
			edited = s.Name
		}
	}
	if edited == "" {
		t.Fatal("the mock has no manually edited stack to check")
	}
	u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: edited})
	if err != nil || !u.ManuallyEdited {
		t.Fatalf("GetStackTemplateUpdate = %+v, %v, want manuallyEdited", u, err)
	}
}
