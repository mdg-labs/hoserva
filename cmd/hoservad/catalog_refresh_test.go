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
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/notify"
	"github.com/mdg-labs/hoserva/internal/template"
)

func catalogArchive(t *testing.T, serial int64) []byte {
	t.Helper()
	index := fmt.Sprintf(`{"schema":1,"serial":%d,"templates":[{"id":"fresh-app","revision":1,"title":"Fresh app","categories":[],"docs":"https://example.com"}]}`, serial)
	var tarball bytes.Buffer
	tw := tar.NewWriter(&tarball)
	for _, f := range []struct{ name, body string }{{"index.json", index}, {"fresh-app/compose.yaml", "services: {}\n"}} {
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

// refreshedTemplates is startedTemplatesWith with the catalog check pointed
// at a host serving whatever *archive and *sig hold, and trusting key.
func refreshedTemplates(t *testing.T, notifier catalogPublisher, key ed25519.PublicKey, archive, sig *[]byte) *api.Handler {
	t.Helper()
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog.tar.zst":
			_, _ = w.Write(*archive)
		case "/catalog.tar.zst.sig":
			_, _ = w.Write(*sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	h := startedTemplatesWith(t, t.TempDir(), notifier)
	refresher, ok := h.CatalogRefresh.(*template.Refresher)
	if !ok {
		t.Fatalf("startTemplates left Handler.CatalogRefresh = %T, so POST /catalog/refresh would 501", h.CatalogRefresh)
	}
	if refresher.URL != "" {
		t.Fatalf("the daemon's refresher URL = %q, want the compiled-in default (empty)", refresher.URL)
	}
	refresher.URL = host.URL
	refresher.Store.Key = key
	return h
}

func TestStartTemplates_RefreshCatalogInstallsANewerSignedCatalogAndTheListReportsTheCheck(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var archive, sig []byte
	notifier := &recordingPublisher{}
	h := refreshedTemplates(t, notifier, pub, &archive, &sig)

	before, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.LastCheckedAt.IsSet() || before.LastOutcome.IsSet() {
		t.Fatalf("the list reports a check before any ran: %+v %+v", before.LastCheckedAt, before.LastOutcome)
	}

	archive = catalogArchive(t, before.Serial+1)
	sig = ed25519.Sign(priv, archive)
	res, err := h.RefreshCatalog(ctx)
	if err != nil {
		t.Fatalf("RefreshCatalog: %v", err)
	}
	if res.Outcome != apiv1.CatalogCheckOutcomeUpdated || res.NewTemplates.Or(-1) != 1 || res.UpdatedTemplates.Or(-1) != 0 || res.Reason.IsSet() {
		t.Fatalf("result = %+v, want updated with one new template (fresh-app) and the snapshot's others dropped", res)
	}

	after, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Serial != before.Serial+1 || len(after.Templates) != 1 || after.Templates[0].ID != "fresh-app" {
		t.Fatalf("list after the refresh = serial %d, %+v", after.Serial, after.Templates)
	}
	if at, ok := after.LastCheckedAt.Get(); !ok || !at.Equal(res.CheckedAt) {
		t.Fatalf("lastCheckedAt = %v, %v, want %v", at, ok, res.CheckedAt)
	}
	if o, ok := after.LastOutcome.Get(); !ok || o != apiv1.CatalogCheckOutcomeUpdated {
		t.Fatalf("lastOutcome = %v, %v", o, ok)
	}
	if n := notifier.count(notify.EventCatalogCheckFailed); n != 0 {
		t.Fatalf("a successful check published %d catalog_check_failed events", n)
	}
}

func TestStartTemplates_AFailedVerificationKeepsTheCatalogAndPublishesTheEventButANetworkFailureDoesNot(t *testing.T) {
	ctx := context.Background()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var archive, sig []byte
	notifier := &recordingPublisher{}
	h := refreshedTemplates(t, notifier, pub, &archive, &sig)
	before, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}

	archive = catalogArchive(t, before.Serial+1)
	_, otherPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig = ed25519.Sign(otherPriv, archive)
	res, err := h.RefreshCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != apiv1.CatalogCheckOutcomeFailed || res.Reason.Or("") != apiv1.CatalogRefreshReasonBadSignature || res.Message.Or("") == "" {
		t.Fatalf("result = %+v, want failed with bad_signature and a message", res)
	}
	if n := notifier.count(notify.EventCatalogCheckFailed); n != 1 {
		t.Fatalf("catalog_check_failed published %d times, want once", n)
	}

	archive = catalogArchive(t, before.Serial)
	sig = ed25519.Sign(priv, archive)
	if res, err := h.RefreshCatalog(ctx); err != nil || res.Reason.Or("") != apiv1.CatalogRefreshReasonNotNewer {
		t.Fatalf("a replayed serial: %+v, %v", res, err)
	}
	if n := notifier.count(notify.EventCatalogCheckFailed); n != 2 {
		t.Fatalf("catalog_check_failed published %d times, want twice", n)
	}

	after, err := h.ListCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Serial != before.Serial || len(after.Templates) != len(before.Templates) {
		t.Fatalf("a refused archive changed the catalog: serial %d to %d", before.Serial, after.Serial)
	}
	if o, ok := after.LastOutcome.Get(); !ok || o != apiv1.CatalogCheckOutcomeFailed {
		t.Fatalf("lastOutcome = %v, %v, want failed", o, ok)
	}

	h.CatalogRefresh.(*template.Refresher).URL = "http://127.0.0.1:1"
	res, err = h.RefreshCatalog(ctx)
	if err != nil || res.Reason.Or("") != apiv1.CatalogRefreshReasonFetchFailed {
		t.Fatalf("an unreachable host: %+v, %v", res, err)
	}
	if n := notifier.count(notify.EventCatalogCheckFailed); n != 2 {
		t.Fatalf("a network failure published an event: %d in all", n)
	}
}

func TestCatalogCheckFailedIsAWarningNotifyEventAndAnUnnotifiedRefresherHasNoHook(t *testing.T) {
	if _, ok := notify.DefaultSeverity(notify.EventCatalogCheckFailed); !ok || !notify.ValidEventType(notify.EventCatalogCheckFailed) {
		t.Fatal("catalog_check_failed is not a notify event type with a default severity")
	}
	if sev, _ := notify.DefaultSeverity(notify.EventCatalogCheckFailed); sev != notify.SeverityWarning {
		t.Fatalf("severity = %q, want warning", sev)
	}
	dir := filepath.Join(t.TempDir(), "state")
	if r := newCatalogRefresher(dir, nil); r.Notify != nil {
		t.Fatal("a refresher with no notifier carries a Notify hook")
	}
}
