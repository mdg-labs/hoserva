package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/store"
)

type stubCatalogSettings struct {
	settings store.CatalogSettings
	updates  []store.CatalogSettingsUpdate
	err      error
}

func (s *stubCatalogSettings) CatalogSettings(context.Context) (store.CatalogSettings, error) {
	return s.settings, s.err
}

func (s *stubCatalogSettings) UpdateCatalogSettings(_ context.Context, u store.CatalogSettingsUpdate) (store.CatalogSettings, error) {
	if s.err != nil {
		return store.CatalogSettings{}, s.err
	}
	s.updates = append(s.updates, u)
	if u.RefreshInterval != nil {
		s.settings.RefreshInterval = *u.RefreshInterval
	}
	if u.CheckOnOpen != nil {
		s.settings.CheckOnOpen = *u.CheckOnOpen
	}
	return s.settings, nil
}

func catalogPut(t *testing.T, srv *httptest.Server, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestCatalogSettings_NotConfiguredIs501(t *testing.T) {
	srv := catalogServer(t, newCatalogHandler(t))
	if resp, _ := catalogGet(t, srv, "/settings/catalog"); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("GET = %d, want 501", resp.StatusCode)
	}
	if resp, _ := catalogPut(t, srv, "/settings/catalog", `{"checkOnOpen":false}`); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("PUT = %d, want 501", resp.StatusCode)
	}
}

func TestCatalogSettings_PassOnlyTheFieldsTheRequestCarries(t *testing.T) {
	h := newCatalogHandler(t)
	stub := &stubCatalogSettings{settings: store.DefaultCatalogSettings}
	h.CatalogSettings = stub
	srv := catalogServer(t, h)

	resp, body := catalogGet(t, srv, "/settings/catalog")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"refreshInterval":"24h"`) || !strings.Contains(string(body), `"checkOnOpen":true`) {
		t.Fatalf("GET = %d %s", resp.StatusCode, body)
	}
	if resp, body := catalogPut(t, srv, "/settings/catalog", `{"refreshInterval":"1h"}`); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"checkOnOpen":true`) {
		t.Fatalf("PUT interval = %d %s", resp.StatusCode, body)
	}
	if resp, _ := catalogPut(t, srv, "/settings/catalog", `{"checkOnOpen":false}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT check-on-open = %d", resp.StatusCode)
	}
	if len(stub.updates) != 2 {
		t.Fatalf("updates = %+v", stub.updates)
	}
	if u := stub.updates[0]; u.RefreshInterval == nil || *u.RefreshInterval != "1h" || u.CheckOnOpen != nil {
		t.Fatalf("first update = %+v, want only the interval", u)
	}
	if u := stub.updates[1]; u.RefreshInterval != nil || u.CheckOnOpen == nil || *u.CheckOnOpen {
		t.Fatalf("second update = %+v, want only check-on-open false", u)
	}
}

func TestCatalogSettings_AnUnknownIntervalIsRefusedWith400AndStoresNothing(t *testing.T) {
	h := newCatalogHandler(t)
	stub := &stubCatalogSettings{settings: store.DefaultCatalogSettings}
	h.CatalogSettings = stub
	srv := catalogServer(t, h)
	if resp, body := catalogPut(t, srv, "/settings/catalog", `{"refreshInterval":"2h","checkOnOpen":false}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT = %d %s, want 400", resp.StatusCode, body)
	}
	if len(stub.updates) != 0 {
		t.Fatalf("a refused request stored %+v", stub.updates)
	}
}

func TestCatalogSettings_AStoreFailureIsNotBlamedOnTheRequest(t *testing.T) {
	h := newCatalogHandler(t)
	h.CatalogSettings = &stubCatalogSettings{err: errors.New("database is locked")}
	ctx := context.Background()
	_, err := h.UpdateCatalogSettings(ctx, &apiv1.CatalogSettingsUpdate{CheckOnOpen: apiv1.NewOptBool(true)})
	if st := h.NewError(ctx, err); st.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a store failure = %d, want 500", st.StatusCode)
	}
	_, err = h.GetCatalogSettings(ctx)
	if st := h.NewError(ctx, err); st.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a store read failure = %d, want 500", st.StatusCode)
	}
}

type stubOpener struct{ opened int }

func (s *stubOpener) CheckOnOpen(context.Context) { s.opened++ }

func TestListCatalog_StartsTheCheckOnOpenTriggerOncePerListing(t *testing.T) {
	h := newCatalogHandler(t)
	opener := &stubOpener{}
	h.CatalogOpen = opener
	for i := 1; i <= 3; i++ {
		if _, err := h.ListCatalog(context.Background()); err != nil {
			t.Fatal(err)
		}
		if opener.opened != i {
			t.Fatalf("after %d listings the trigger ran %d times", i, opener.opened)
		}
	}
}
