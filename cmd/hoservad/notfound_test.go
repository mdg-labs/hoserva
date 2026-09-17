package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/api"
)

// TestJSONAPINotFoundHandlerWritesTheErrorSchema is the review finding
// this issue closes: an unrecognized API path must answer with the
// spec's own Error shape (doc 01 §5), not plain text.
func TestJSONAPINotFoundHandlerWritesTheErrorSchema(t *testing.T) {
	rec := httptest.NewRecorder()
	jsonAPINotFoundHandler(rec, httptest.NewRequest("GET", "/api/v1/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not valid JSON: %v (%q)", err, rec.Body.String())
	}
	if body.Code == "" || body.Message == "" {
		t.Errorf("body = %+v, want a non-empty code and message (the spec's Error schema)", body)
	}
}

// TestGeneratedServerUsesJSONNotFoundHandler proves apiv1.WithNotFound
// actually reaches the generated router, not only this file's own mux
// registrations: a path under apiPathPrefix that matches no declared
// operation (e.g. /api/v1/nope) must get the spec's Error shape from the
// generated server itself, not net/http's own plain-text 404.
func TestGeneratedServerUsesJSONNotFoundHandler(t *testing.T) {
	apiServer, err := apiv1.NewServer(&api.Handler{}, api.TrustedSecurityHandler{}, apiv1.WithPathPrefix(apiPathPrefix), apiv1.WithNotFound(jsonAPINotFoundHandler))
	if err != nil {
		t.Fatalf("building generated API server: %v", err)
	}

	rec := httptest.NewRecorder()
	apiServer.ServeHTTP(rec, httptest.NewRequest("GET", apiPathPrefix+"/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not net/http's own plain-text 404)", ct)
	}
}

// TestMountAPINotFoundRoutesCoversTheWholeAPITree is the review finding
// this issue closes, at the mux level: every path under /api that isn't
// claimed by a more specific registration — including the bare "/api"
// and "/api/" themselves, not only a path one level deeper — gets the
// JSON 404, never the SPA and never plain text. A more specific pattern
// (standing in for apiPathPrefix's own "/api/v1/" registration) still
// wins for anything actually under it, regardless of which was
// registered first.
func TestMountAPINotFoundRoutesCoversTheWholeAPITree(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(apiPathPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot) // a fixed, recognizable marker.
	})
	mountAPINotFoundRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>spa shell</html>"))
	})

	cases := []struct {
		path       string
		wantStatus int
		wantJSON   bool
	}{
		{"/api", http.StatusNotFound, true},
		{"/api/", http.StatusNotFound, true},
		{"/api/foo", http.StatusNotFound, true},
		{"/api/v2/jobs", http.StatusNotFound, true},
		{apiPathPrefix, http.StatusNotFound, true},
		{apiPathPrefix + "/jobs", http.StatusTeapot, false},
		{"/storage/disks/42", http.StatusOK, false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
			if rec.Code != c.wantStatus {
				t.Errorf("GET %s: status = %d, want %d", c.path, rec.Code, c.wantStatus)
			}
			if c.wantJSON && rec.Header().Get("Content-Type") != "application/json" {
				t.Errorf("GET %s: Content-Type = %q, want application/json", c.path, rec.Header().Get("Content-Type"))
			}
			if !c.wantJSON && rec.Header().Get("Content-Type") == "application/json" {
				t.Errorf("GET %s: unexpectedly got application/json", c.path)
			}
		})
	}
}
