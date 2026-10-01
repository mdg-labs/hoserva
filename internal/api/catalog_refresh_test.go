package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
	"github.com/mdg-labs/hoserva/internal/template"
)

type stubRefresher struct {
	res     template.CheckResult
	err     error
	checked bool
}

func (s *stubRefresher) Refresh(context.Context) (template.CheckResult, error) {
	s.checked = true
	return s.res, s.err
}

func (s *stubRefresher) Last() (template.CheckResult, bool) { return s.res, s.checked }

func catalogPost(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.UnixSocketCredentialHeader, api.UnixSocketCredentialValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		body.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, []byte(body.String())
}

func TestCatalogRefresh_NotConfiguredIs501(t *testing.T) {
	h := newCatalogHandler(t)
	_, err := h.RefreshCatalog(context.Background())
	if status, code := statusOf(h, err); status != 501 || code != "not_configured" {
		t.Fatalf("RefreshCatalog = %d %q, want 501 not_configured", status, code)
	}
	list, err := h.ListCatalog(context.Background())
	if err != nil || list.LastCheckedAt.IsSet() || list.LastOutcome.IsSet() {
		t.Fatalf("a list with no refresher: %+v, %v", list, err)
	}
}

func TestCatalogRefresh_ReportsEachOutcomeWithOnlyTheFieldsItCarries(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	cases := []struct {
		name    string
		res     template.CheckResult
		present []string
		absent  []string
	}{
		{"updated", template.CheckResult{CheckedAt: at, Outcome: template.OutcomeUpdated, New: 3, Updated: 1},
			[]string{"checkedAt", "outcome", "newTemplates", "updatedTemplates"}, []string{"reason", "message"}},
		{"unchanged", template.CheckResult{CheckedAt: at, Outcome: template.OutcomeUnchanged},
			[]string{"checkedAt", "outcome"}, []string{"newTemplates", "updatedTemplates", "reason", "message"}},
		{"failed", template.CheckResult{CheckedAt: at, Outcome: template.OutcomeFailed, Reason: template.ReasonBadSignature, Message: "does not verify"},
			[]string{"checkedAt", "outcome", "reason", "message"}, []string{"newTemplates", "updatedTemplates"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCatalogHandler(t)
			h.CatalogRefresh = &stubRefresher{res: c.res}
			srv := catalogServer(t, h)

			resp, body := catalogPost(t, srv, "/catalog/refresh")
			if resp.StatusCode != 200 {
				t.Fatalf("status %d, body %s", resp.StatusCode, body)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			for _, k := range c.present {
				if _, ok := doc[k]; !ok {
					t.Errorf("response lacks %q: %s", k, body)
				}
			}
			for _, k := range c.absent {
				if _, ok := doc[k]; ok {
					t.Errorf("response carries %q: %s", k, body)
				}
			}
			var got apiv1.CatalogRefresh
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if string(got.Outcome) != string(c.res.Outcome) || !got.CheckedAt.Equal(at) ||
				got.NewTemplates.Or(0) != c.res.New || got.UpdatedTemplates.Or(0) != c.res.Updated ||
				string(got.Reason.Or("")) != string(c.res.Reason) || got.Message.Or("") != c.res.Message {
				t.Errorf("response = %+v, want %+v", got, c.res)
			}

			_, body = catalogGet(t, srv, "/catalog")
			var list apiv1.CatalogList
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatal(err)
			}
			if o, ok := list.LastOutcome.Get(); !ok || string(o) != string(c.res.Outcome) {
				t.Errorf("list lastOutcome = %v, %v", o, ok)
			}
			if got, ok := list.LastCheckedAt.Get(); !ok || !got.Equal(at) {
				t.Errorf("list lastCheckedAt = %v, %v", got, ok)
			}
		})
	}
}

func TestCatalogRefresh_TheListHasNoCheckFieldsBeforeAnyCheck(t *testing.T) {
	h := newCatalogHandler(t)
	h.CatalogRefresh = &stubRefresher{}
	_, body := catalogGet(t, catalogServer(t, h), "/catalog")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lastCheckedAt", "lastOutcome"} {
		if _, ok := doc[k]; ok {
			t.Errorf("the list carries %q before any check: %s", k, body)
		}
	}
}

func TestCatalogRefresh_AWaitThatEndsWithTheCallersContextIsAnError(t *testing.T) {
	h := newCatalogHandler(t)
	h.CatalogRefresh = &stubRefresher{err: context.Canceled}
	_, err := h.RefreshCatalog(context.Background())
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("RefreshCatalog = %v, want the context error", err)
	}
}
