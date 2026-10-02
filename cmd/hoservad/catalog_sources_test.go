package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

const userSourceCompose = `services:
  app:
    image: registry.example.com/user/app:1.0.0
    privileged: true
    ports:
      - ${WEBUI_PORT}:80
x-hoserva:
  schema: 1
  id: user-app
  revision: %d
  title: User app
  categories: [system]
  icon: icon.svg
  docs: https://example.com/docs
  inputs:
    WEBUI_PORT: { kind: port, default: 18080 }
`

// userSourceArchive is an unsigned user catalog: one privileged template at
// the given revision.
func userSourceArchive(t *testing.T, serial int64, revision int) []byte {
	t.Helper()
	index := fmt.Sprintf(`{"schema":1,"serial":%d,"templates":[{"id":"user-app","revision":%d,"title":"User app","categories":["system"],"docs":"https://example.com/docs"}]}`, serial, revision)
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	files := []struct{ name, body string }{{"index.json", index}, {"user-app/compose.yaml", fmt.Sprintf(userSourceCompose, revision)}}
	for _, f := range files {
		if strings.HasSuffix(f.name, "compose.yaml") {
			if err := tw.WriteHeader(&tar.Header{Name: "user-app/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(tarball.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// userSourceHost serves whatever *archive holds as an unsigned catalog and
// counts the requests it sees.
func userSourceHost(t *testing.T, archive *[]byte) (url string, client *http.Client, requests *[]string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.URL.Path)
		if r.URL.Path != "/catalog.tar.zst" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(*archive)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client(), &seen
}

func TestStartTemplates_CatalogSourcesAreReachableAndBadgeUserAddedEntries(t *testing.T) {
	ctx := context.Background()
	h := startedTemplatesWithSettings(t, t.TempDir(), nil)
	sources, ok := h.CatalogSources.(*template.Sources)
	if !ok || h.Catalog != template.Catalog(sources) {
		t.Fatalf("startTemplates left Handler.CatalogSources = %T and Handler.Catalog = %T, so /catalog-sources would 501 or the catalog would not include the user's sources", h.CatalogSources, h.Catalog)
	}
	if h.TemplateInstall.Catalog != template.Catalog(sources) {
		t.Fatalf("the installer resolves templates from %T, so a user-added source's template could not be installed", h.TemplateInstall.Catalog)
	}

	list, err := h.ListCatalogSources(ctx)
	if err != nil || len(list.Sources) != 1 {
		t.Fatalf("ListCatalogSources = %+v, %v, want the curated catalog's row on a fresh database", list, err)
	}
	if c := list.Sources[0]; c.ID != template.SourceCurated || c.Kind != apiv1.CatalogSourceKindCurated || !c.Signed || c.Serial.Or(0) <= 0 {
		t.Fatalf("curated source = %+v", c)
	}

	archive := userSourceArchive(t, 5, 1)
	url, client, requests := userSourceHost(t, &archive)
	sources.Client = client
	added, err := h.AddCatalogSource(ctx, &apiv1.AddCatalogSourceRequest{URL: url})
	if err != nil {
		t.Fatalf("AddCatalogSource: %v", err)
	}
	if added.Kind != apiv1.CatalogSourceKindUserAdded || added.Signed || added.Serial.Or(0) != 5 {
		t.Fatalf("added = %+v, want a user-added, unsigned source at serial 5", added)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %v: an unsigned source must not be asked for a signature", *requests)
	}

	catalog, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entry *apiv1.CatalogEntry
	for i, e := range catalog.Templates {
		if e.ID == "user-app" {
			entry = &catalog.Templates[i]
		} else if e.SourceKind != apiv1.CatalogSourceKindCurated || !e.Signed || e.Source != template.SourceCurated {
			t.Errorf("curated entry %s lost its badge: %+v", e.ID, e)
		}
	}
	if entry == nil || entry.Source != added.ID || entry.SourceKind != apiv1.CatalogSourceKindUserAdded || entry.Signed {
		t.Fatalf("user-app entry = %+v, want source %s, user-added, unsigned", entry, added.ID)
	}
	detail, err := h.GetCatalogTemplate(ctx, apiv1.GetCatalogTemplateParams{ID: "user-app"})
	if err != nil || detail.Signed || detail.SourceKind != apiv1.CatalogSourceKindUserAdded || len(detail.Privileges) == 0 {
		t.Fatalf("detail = %+v, %v, want the unsigned badge and the privilege summary", detail, err)
	}
	if _, err := h.InstallTemplate(ctx, &apiv1.TemplateInstallRequest{}, apiv1.InstallTemplateParams{ID: "user-app"}); err != nil {
		t.Fatalf("InstallTemplate from the user-added source: %v", err)
	}
	stack, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "user-app"})
	if err != nil || stack.Template.Source != added.ID || stack.Template.Revision != "1" {
		t.Fatalf("stack = %+v, %v", stack, err)
	}

	u, err := h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "user-app"})
	if err != nil || u.Status != apiv1.StackTemplateUpdateStatusUpToDate {
		t.Fatalf("update = %+v, %v, want up_to_date", u, err)
	}
	archive = userSourceArchive(t, 6, 2)
	res, err := h.RefreshCatalogSource(ctx, apiv1.RefreshCatalogSourceParams{ID: added.ID})
	if err != nil || res.Outcome != apiv1.CatalogCheckOutcomeUpdated || res.UpdatedTemplates.Or(0) != 1 {
		t.Fatalf("RefreshCatalogSource = %+v, %v", res, err)
	}
	u, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "user-app"})
	if err != nil || u.Status != apiv1.StackTemplateUpdateStatusUpdateAvailable || u.AvailableRevision.Or(0) != 2 || u.Signed.Or(true) || !strings.Contains(u.Diff.Or(""), "-  revision: 1\n+  revision: 2\n") {
		t.Fatalf("update = %+v, %v", u, err)
	}
	after, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "user-app"})
	if err != nil || after.Compose != stack.Compose || after.Template != stack.Template {
		t.Fatalf("an available update changed the installed stack: %+v -> %+v", stack, after)
	}

	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: template.SourceCurated}); err == nil {
		t.Fatal("the curated catalog was removable")
	}
	if err := h.RemoveCatalogSource(ctx, apiv1.RemoveCatalogSourceParams{ID: added.ID}); err != nil {
		t.Fatalf("RemoveCatalogSource: %v", err)
	}
	u, err = h.GetStackTemplateUpdate(ctx, apiv1.GetStackTemplateUpdateParams{Name: "user-app"})
	if err != nil || u.Status != apiv1.StackTemplateUpdateStatusSourceRemoved {
		t.Fatalf("update after removal = %+v, %v", u, err)
	}
	kept, err := h.GetStack(ctx, apiv1.GetStackParams{Name: "user-app"})
	if err != nil || kept.Compose != stack.Compose {
		t.Fatalf("removing the source changed the installed stack: %+v, %v", kept, err)
	}
	catalog, err = h.ListCatalog(ctx)
	if err != nil || len(catalog.Templates) == 0 {
		t.Fatalf("catalog after the removal = %+v, %v", catalog, err)
	}
	for _, e := range catalog.Templates {
		if e.ID == "user-app" {
			t.Fatal("the removed source still supplies its template")
		}
	}
}

func TestStartTemplates_AFinishedCuratedCheckIsRecordedOnTheCuratedSourceOnlyWhenItSucceeded(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var archive, sig []byte
	h := refreshedTemplates(t, nil, pub, &archive, &sig)
	before, err := h.ListCatalogSources(ctx)
	if err != nil || before.Sources[0].LastRefreshedAt.IsSet() {
		t.Fatalf("before any check: %+v, %v", before, err)
	}

	archive = catalogArchive(t, before.Sources[0].Serial.Or(0)+1)
	sig = ed25519.Sign(otherPriv, archive)
	if res, err := h.RefreshCatalog(ctx); err != nil || res.Outcome != apiv1.CatalogCheckOutcomeFailed {
		t.Fatalf("RefreshCatalog with a signature by another key = %+v, %v, want a failed check", res, err)
	}
	if after, err := h.ListCatalogSources(ctx); err != nil || after.Sources[0].LastRefreshedAt.IsSet() {
		t.Fatalf("a failed curated check was recorded as a refresh: %+v, %v", after, err)
	}

	sig = ed25519.Sign(priv, archive)
	res, err := h.RefreshCatalog(ctx)
	if err != nil || res.Outcome != apiv1.CatalogCheckOutcomeUpdated {
		t.Fatalf("RefreshCatalog = %+v, %v", res, err)
	}
	after, err := h.ListCatalogSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := after.Sources[0].LastRefreshedAt.Get(); !ok || at.Before(res.CheckedAt.Add(-time.Second)) || after.Sources[0].Serial.Or(0) != before.Sources[0].Serial.Or(0)+1 {
		t.Fatalf("curated source after a successful check = %+v, want lastRefreshedAt near %v and the new serial", after.Sources[0], res.CheckedAt)
	}
}
