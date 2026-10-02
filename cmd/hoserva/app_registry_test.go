package main

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func withStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	saved := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = saved; _ = r.Close() })
}

func TestAppRegistryCredentialSetReadsThePasswordFromStdinNeverAnArgument(t *testing.T) {
	var gotPath, gotMethod string
	var body apiv1.PutRegistryCredentialRequest
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := body.UnmarshalJSON(raw); err != nil {
			t.Errorf("body %q: %v", raw, err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	withStdin(t, "s3cret-token\n")
	printed, err := runAppCLI(t, sock, "app", "registry-credential-set", "ghcr.io", "--username", "me")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/v1/registry-credentials/ghcr.io" || body.Username != "me" || body.Password != "s3cret-token" {
		t.Fatalf("request = %s %s %+v, want the password from stdin without its newline", gotMethod, gotPath, body)
	}
	if strings.Contains(printed, "s3cret-token") || !strings.Contains(printed, "Saved the credential for ghcr.io.") {
		t.Errorf("output = %q", printed)
	}
}

func TestAppRegistryCredentialSetRefusesWithoutAUsernameOrPassword(t *testing.T) {
	calls := 0
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(http.StatusNoContent) })
	withStdin(t, "pw\n")
	if _, err := runAppCLI(t, sock, "app", "registry-credential-set", "ghcr.io"); err == nil {
		t.Error("a missing --username was accepted")
	}
	withStdin(t, "")
	if _, err := runAppCLI(t, sock, "app", "registry-credential-set", "ghcr.io", "--username", "me"); err == nil {
		t.Error("an empty password was accepted")
	}
	if calls != 0 {
		t.Errorf("a refused invocation still called the API %d times", calls)
	}
}

func TestAppRegistryCredentialSetReportsARefusal(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, &apiv1.Error{Code: "invalid_registry_credential", Message: "a username cannot contain a colon"})
	})
	withStdin(t, "pw\n")
	printed, err := runAppCLI(t, sock, "app", "registry-credential-set", "ghcr.io", "--username", "a:b")
	if err == nil || printed != "" {
		t.Fatalf("printed %q, err %v, want an error and no output", printed, err)
	}
}

func TestAppRegistryCredentialsListAndDeleteCallTheirOperation(t *testing.T) {
	var requests []string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(t, w, http.StatusOK, &apiv1.RegistryCredentialList{Registries: []string{"ghcr.io"}})
	})
	printed, err := runAppCLI(t, sock, "app", "registry-credentials")
	if err != nil || !strings.Contains(printed, "ghcr.io") {
		t.Fatalf("list: %q, %v", printed, err)
	}
	printed, err = runAppCLI(t, sock, "app", "registry-credential-delete", "registry.example.com:5000")
	if err != nil || !strings.Contains(printed, "Deleted the credential for registry.example.com:5000.") {
		t.Fatalf("delete: %q, %v", printed, err)
	}
	want := "GET /api/v1/registry-credentials|DELETE /api/v1/registry-credentials/registry.example.com:5000"
	if strings.Join(requests, "|") != want {
		t.Fatalf("requests = %v, want %s", requests, want)
	}
}
