package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

// TestStartTemplates_ServesTheEmbeddedCatalogOverTheCatalogOperations goes
// through the handler startTemplates builds, as main.go does: a fresh state
// directory lists and shows the curated catalog it was seeded with, names the
// curated source on every entry, serves an icon, and marks a template
// installed once a stack of it exists.
func TestStartTemplates_ServesTheEmbeddedCatalogOverTheCatalogOperations(t *testing.T) {
	ctx := context.Background()
	h := startedTemplates(t, t.TempDir())
	if h.Catalog == nil {
		t.Fatal("startTemplates left Handler.Catalog nil, so every /catalog operation would 501")
	}

	list, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatalf("ListCatalog: %v", err)
	}
	if list.Serial <= 0 || len(list.Templates) == 0 {
		t.Fatalf("list = serial %d, %d templates", list.Serial, len(list.Templates))
	}
	var jellyfin *apiv1.CatalogEntry
	for i, e := range list.Templates {
		if e.Source != template.SourceCurated || e.Installed {
			t.Errorf("%s: source %q installed %v", e.ID, e.Source, e.Installed)
		}
		if e.ID == "jellyfin" {
			jellyfin = &list.Templates[i]
		}
	}
	if jellyfin == nil {
		t.Fatal("the embedded catalog lists no jellyfin")
	}

	detail, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "jellyfin"})
	if err != nil || detail.Compose == "" || detail.Revision != jellyfin.Revision {
		t.Fatalf("GetCatalogTemplate = %+v, %v", detail, err)
	}
	if icon, err := h.GetCatalogTemplateIcon(ctx, apiv1.GetCatalogTemplateIconParams{ID: "jellyfin"}); err != nil || icon == nil {
		t.Fatalf("GetCatalogTemplateIcon = %v, %v", icon, err)
	}

	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "jellyfin"}); err != nil {
		t.Fatalf("InstallTemplate: %v", err)
	}
	list, err = h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list.Templates {
		if want := e.ID == "jellyfin"; e.Installed != want {
			t.Errorf("%s: installed = %v, want %v after installing jellyfin", e.ID, e.Installed, want)
		}
	}
}

func TestStartTemplates_ACatalogRemovedFromTheStateDirectoryMakesTheListUnavailable(t *testing.T) {
	stateDir := t.TempDir()
	h := startedTemplates(t, stateDir)
	if err := os.RemoveAll(filepath.Join(stateDir, catalogDirName)); err != nil {
		t.Fatal(err)
	}
	_, err := h.ListCatalog(context.Background())
	if st := h.NewError(context.Background(), err); st.StatusCode != 503 || st.Response.Code != "catalog_unavailable" {
		t.Fatalf("ListCatalog with no catalog = %d %q, want 503 catalog_unavailable", st.StatusCode, st.Response.Code)
	}
}
