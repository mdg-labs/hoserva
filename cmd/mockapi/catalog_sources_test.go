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
	if err != nil || len(list.Sources) != 3 || list.Sources[0].ID != template.SourceCurated || list.Sources[0].Kind != apiv1.CatalogSourceKindCurated || list.Sources[1].ID != unsigned.ID || list.Sources[2].ID != signed.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: unsigned.ID}); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.ListCatalogSources(ctx); len(list.Sources) != 2 || list.Sources[1].ID != signed.ID {
		t.Fatalf("list after removal = %+v", list)
	}
}

func TestMockCatalogEntriesCarryTheCuratedSignedBadge(t *testing.T) {
	h, err := newHandler("healthy")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := h.ListCatalog(ctx)
	if err != nil || len(list.Templates) == 0 {
		t.Fatalf("ListCatalog = %+v, %v", list, err)
	}
	for _, e := range list.Templates {
		if e.Source != template.SourceCurated || e.SourceKind != apiv1.CatalogSourceKindCurated || !e.Signed {
			t.Errorf("%s: %+v", e.ID, e)
		}
	}
	d, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "jellyfin"})
	if err != nil || d.SourceKind != apiv1.CatalogSourceKindCurated || !d.Signed {
		t.Fatalf("GetCatalogTemplate = %+v, %v", d, err)
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
