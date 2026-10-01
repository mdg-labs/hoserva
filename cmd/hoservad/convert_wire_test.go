package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

// TestConvertUnraidTemplate_IsServedByTheDaemonsHandler builds the handler
// the way main.go does and serves it through the generated server under the
// same body limit, so an operation main.go never wired would 501 or 404
// here.
func TestConvertUnraidTemplate_IsServedByTheDaemonsHandler(t *testing.T) {
	h := startedTemplates(t, t.TempDir())
	server, err := apiv1.NewServer(h, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix(apiPathPrefix))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.MaxBytesHandler(server, maxRequestBodyBytes))
	defer ts.Close()

	post := func(xml string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"xml": xml})
		req, err := http.NewRequest(http.MethodPost, ts.URL+apiPathPrefix+"/apps/convert", strings.NewReader(string(body)))
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
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	status, out := post(`<Container><Name>web</Name><Repository>example.com/web:1</Repository><ExtraParams>--exotic=1 --cap-add=NET_ADMIN</ExtraParams></Container>`)
	if status != http.StatusOK {
		t.Fatalf("POST /apps/convert = %d %v, want 200", status, out)
	}
	if compose, _ := out["compose"].(string); !strings.Contains(compose, "image: example.com/web:1") || !strings.Contains(compose, "--exotic=1") {
		t.Errorf("compose = %q", compose)
	}
	if out["clean"] != false {
		t.Errorf("clean = %v, want false for an untranslated flag", out["clean"])
	}
	if privs, _ := out["privileges"].([]any); len(privs) != 1 {
		t.Errorf("privileges = %v, want the added capability", out["privileges"])
	}

	status, out = post(`<Compose/>`)
	if status != http.StatusBadRequest || out["code"] != "invalid_unraid_template" {
		t.Errorf("a non-template = %d %v, want 400 invalid_unraid_template", status, out)
	}
}
