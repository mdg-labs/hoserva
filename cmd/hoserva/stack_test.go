package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testStack(edited bool, compose string) *apiv1.Stack {
	s := &apiv1.Stack{Name: "web", InstalledAt: time.Now().UTC(), ManuallyEdited: edited}
	if compose != "" {
		s.Compose = apiv1.NewOptString(compose)
	}
	return s
}

func TestStackListShowAndStartCallTheirOperations(t *testing.T) {
	jobID := uuid.New()
	var requests []string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/stacks":
			writeJSON(t, w, http.StatusOK, &apiv1.ListStacksOK{Stacks: []apiv1.Stack{*testStack(true, "")}})
		case "/api/v1/stacks/web":
			writeJSON(t, w, http.StatusOK, testStack(true, "services: {}\n"))
		case "/api/v1/stacks/web/start":
			writeJSON(t, w, http.StatusOK, &apiv1.Job{ID: jobID, Type: apiv1.JobTypeStackStart, Class: apiv1.JobClassService, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()})
		default:
			http.NotFound(w, r)
		}
	})

	printed, err := runAppCLI(t, sock, "stack", "list")
	if err != nil || !strings.Contains(printed, `"manuallyEdited": true`) {
		t.Fatalf("stack list = %q, %v; want the manuallyEdited flag", printed, err)
	}
	printed, err = runAppCLI(t, sock, "stack", "show", "web")
	if err != nil || printed != "services: {}\n" {
		t.Fatalf("stack show = %q, %v; want the compose text alone", printed, err)
	}
	printed, err = runAppCLI(t, sock, "--json", "stack", "show", "web")
	if err != nil || !strings.Contains(printed, `"compose": "services: {}\n"`) {
		t.Fatalf("stack show --json = %q, %v", printed, err)
	}
	printed, err = runAppCLI(t, sock, "stack", "start", "web")
	if err != nil || !strings.Contains(printed, jobID.String()) {
		t.Fatalf("stack start = %q, %v; want the job id", printed, err)
	}
	want := []string{"GET /api/v1/stacks", "GET /api/v1/stacks/web", "GET /api/v1/stacks/web", "POST /api/v1/stacks/web/start"}
	if strings.Join(requests, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestStackEditSendsTheFileAndHonoursDryRun(t *testing.T) {
	file := filepath.Join(t.TempDir(), "docker-compose.yml")
	const text = "services:\n  web:\n    image: nginx:1.28\n"
	if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		args      []string
		wantQuery string
		wantOut   string
	}{
		{"saving", []string{"stack", "edit", "web", "--file", file}, "", "marked it manually edited"},
		{"dry run", []string{"stack", "edit", "web", "--file", file, "--dry-run"}, "dryRun=true", "Nothing was stored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRequest, gotQuery, gotBody string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotRequest = r.Method + " " + r.URL.Path
				gotQuery = r.URL.RawQuery
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				writeJSON(t, w, http.StatusOK, &apiv1.UpdateStackResult{Applied: r.URL.Query().Get("dryRun") != "true", Stack: *testStack(true, text)})
			})
			printed, err := runAppCLI(t, sock, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if gotRequest != "PUT /api/v1/stacks/web" || gotQuery != tc.wantQuery {
				t.Fatalf("request = %q ?%s, want PUT /api/v1/stacks/web ?%s", gotRequest, gotQuery, tc.wantQuery)
			}
			var body struct {
				Compose string `json:"compose"`
			}
			if err := json.Unmarshal([]byte(gotBody), &body); err != nil || body.Compose != text {
				t.Fatalf("body = %q, %v; want the file's text", gotBody, err)
			}
			if !strings.Contains(printed, tc.wantOut) {
				t.Fatalf("output %q does not say %q", printed, tc.wantOut)
			}
		})
	}
}

func TestStackEditRefusedFileIsAnErrorAndNeedsAFile(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_stack","message":"container: invalid compose stack: services must be a mapping"}`))
	})
	file := filepath.Join(t.TempDir(), "bad.yml")
	if err := os.WriteFile(file, []byte("services: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runAppCLI(t, sock, "stack", "edit", "web", "--file", file); err == nil || !strings.Contains(err.Error(), "invalid compose stack") {
		t.Fatalf("stack edit of a refused file: error = %v, want the compiler's message", err)
	}
	if _, err := runAppCLI(t, sock, "stack", "edit", "web"); err == nil {
		t.Fatal("stack edit without --file succeeded")
	}
	if _, err := runAppCLI(t, sock, "stack", "edit", "web", "--file", filepath.Join(t.TempDir(), "missing.yml")); err == nil {
		t.Fatal("stack edit of a missing file succeeded")
	}
}
