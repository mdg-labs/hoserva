package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testInstallPlan() apiv1.TemplateInstallPlan {
	return apiv1.TemplateInstallPlan{
		Template: apiv1.StackTemplate{Source: "hoserva", ID: "risky-agent", Revision: "2"},
		Title:    "Risky agent",
		Name:     "agent",
		Inputs: []apiv1.TemplateInput{
			{Name: "APPDATA", Kind: apiv1.TemplateInputKindPath, Value: apiv1.NewOptString("/mnt/cache/appdata")},
			{Name: "DB_PASSWORD", Kind: apiv1.TemplateInputKindSecret, Generated: true},
			{Name: "WEBUI_PORT", Kind: apiv1.TemplateInputKindPort, Value: apiv1.NewOptString("8097"), RequestedValue: apiv1.NewOptString("8096")},
		},
		Privileges: []apiv1.TemplatePrivilege{
			{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "agent", Description: "Runs with full access to the server."},
			{Kind: apiv1.TemplatePrivilegeKindDockerSocket, Service: "agent", Detail: apiv1.NewOptString("/var/run/docker.sock"), Description: "Can control Docker itself."},
		},
		Compose: "services: {}\n",
	}
}

func TestAppInstallSendsTheNameAndValuesAndPrintsEveryPrivilege(t *testing.T) {
	var gotRequest string
	var gotBody apiv1.TemplateInstallRequest
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body %q: %v", raw, err)
		}
		writeJSON(t, w, http.StatusOK, &apiv1.TemplateInstallResult{
			Stack: apiv1.Stack{Name: "agent", Template: apiv1.StackTemplate{Source: "hoserva", ID: "risky-agent", Revision: "2"}, InstalledAt: time.Now().UTC()},
			Plan:  testInstallPlan(),
		})
	})
	printed, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--name", "agent", "--set", "WEBUI_PORT=8096", "--set", "APPDATA=/mnt/cache/a=b")
	if err != nil {
		t.Fatalf("app install: %v", err)
	}
	if gotRequest != "POST /api/v1/templates/risky-agent/install" {
		t.Errorf("request = %q", gotRequest)
	}
	if gotBody.Name.Or("") != "agent" || gotBody.Values.Or(nil)["WEBUI_PORT"] != "8096" || gotBody.Values.Or(nil)["APPDATA"] != "/mnt/cache/a=b" {
		t.Errorf("request body = %+v", gotBody)
	}
	for _, want := range []string{
		`Installed stack "agent" from hoserva/risky-agent (revision 2).`,
		"DB_PASSWORD: generated",
		"WEBUI_PORT: 8097 (port 8096 is already in use",
		"- privileged (service agent): Runs with full access to the server.",
		"- docker_socket /var/run/docker.sock (service agent): Can control Docker itself.",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

func TestAppInstallDryRunPreviewsInsteadOfInstalling(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		plan := testInstallPlan()
		writeJSON(t, w, http.StatusOK, &plan)
	})
	printed, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if gotRequest != "POST /api/v1/templates/risky-agent/preview" {
		t.Errorf("request = %q: --dry-run must not call install", gotRequest)
	}
	if !strings.Contains(printed, "Would install") || !strings.Contains(printed, "Nothing was created.") {
		t.Errorf("output = %q", printed)
	}
}

func TestAppInstallJSONEmitsTheResultWithItsPrivileges(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &apiv1.TemplateInstallResult{Stack: apiv1.Stack{Name: "agent", InstalledAt: time.Now().UTC()}, Plan: testInstallPlan()})
	})
	printed, err := runAppCLI(t, sock, "--json", "app", "install", "risky-agent")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Plan struct {
			Privileges []struct{ Kind string } `json:"privileges"`
		} `json:"plan"`
	}
	if err := json.Unmarshal([]byte(printed), &out); err != nil || len(out.Plan.Privileges) != 2 {
		t.Errorf("output %q: %v", printed, err)
	}
}

func TestAppInstallRefusesAMalformedSetBeforeCallingTheDaemon(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, args := range [][]string{
		{"app", "install", "x", "--set", "NOEQUALS"},
		{"app", "install", "x", "--set", "=v"},
		{"app", "install", "x", "--set", "A=1", "--set", "A=2"},
	} {
		if _, err := runAppCLI(t, sock, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
	if called {
		t.Error("a malformed --set reached the daemon")
	}
}

func TestAppInstallSurfacesTheDaemonsRefusal(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"template_not_found","message":"no template with that id: \"nope\""}`))
	})
	_, err := runAppCLI(t, sock, "app", "install", "nope")
	if err == nil || !strings.Contains(err.Error(), "no template with that id") {
		t.Errorf("err = %v, want the daemon's message", err)
	}
}
