package api_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mdg-labs/hoserva/internal/api"
)

type noAdmins struct{}

func (noAdmins) CountAdmins(context.Context) (int64, error) { return 0, nil }

// The two import operations pass the setup gate only where the caller names
// them, which is the Unix socket's listener and no other; every other
// operation stays gated on both.
func TestSetupGate_ExemptsTheConfigImportOperationsOnlyWhereTheyAreNamed(t *testing.T) {
	reached := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})
	tcp := api.SetupGate(inner, noAdmins{}, "/api/v1")
	socket := api.SetupGate(inner, noAdmins{}, "/api/v1", api.ConfigImportPaths...)

	for _, path := range []string{"/api/v1/config/import", "/api/v1/config/import/preview"} {
		for name, tc := range map[string]struct {
			gate http.Handler
			want int
		}{"tcp": {tcp, http.StatusConflict}, "socket": {socket, http.StatusOK}} {
			rec := httptest.NewRecorder()
			tc.gate.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
			if rec.Code != tc.want {
				t.Errorf("%s listener, POST %s = %d, want %d", name, path, rec.Code, tc.want)
			}
			if tc.want == http.StatusConflict && !bytes.Contains(rec.Body.Bytes(), []byte("setup_required")) {
				t.Errorf("%s listener, POST %s = %s, want setup_required", name, path, rec.Body.String())
			}
		}
	}

	before := reached
	for _, path := range []string{"/api/v1/config/export", "/api/v1/jobs", "/api/v1/config/importx", "/api/v1/config"} {
		rec := httptest.NewRecorder()
		socket.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != http.StatusConflict {
			t.Errorf("socket listener, POST %s = %d, want every other operation still gated", path, rec.Code)
		}
	}
	if reached != before {
		t.Error("a gated operation reached the handler")
	}
}
