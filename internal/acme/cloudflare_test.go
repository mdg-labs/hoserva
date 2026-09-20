package acme

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCloudflareSolverPresentsAndCleansTXT(t *testing.T) {
	var created, deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			name := r.URL.Query().Get("name")
			result := []any{}
			if name == "example.com" {
				result = []any{map[string]string{"id": "zone-1", "name": "example.com"}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-1/dns_records":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"type":"TXT"`) {
				t.Errorf("create body = %s", body)
			}
			created = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]string{"id": "rec-1"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/zones/zone-1/dns_records/rec-1":
			deleted = true
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	s := &CloudflareSolver{APIToken: "test-token", APIBase: srv.URL, HTTPClient: srv.Client()}
	fqdn := "_acme-challenge.nas.example.com"
	if err := s.Present(context.Background(), fqdn, "txt-value"); err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected Cloudflare TXT create")
	}
	if err := s.CleanUp(context.Background(), fqdn, "txt-value"); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("expected Cloudflare TXT delete")
	}
}
