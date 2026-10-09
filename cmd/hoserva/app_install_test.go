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
			{Kind: apiv1.TemplatePrivilegeKindAddedCapabilities, Service: "agent", Detail: apiv1.NewOptString("SYS_ADMIN"), Description: "Is given extra Linux capabilities."},
			{Kind: apiv1.TemplatePrivilegeKindConfinementDisabled, Service: "agent", Detail: apiv1.NewOptString("apparmor:unconfined"), Description: "Switches off part of the container's confinement."},
		},
		Compose: "services: {}\n",
		Digest:  "abc123digest",
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
		"- added_capabilities SYS_ADMIN (service agent): Is given extra Linux capabilities.",
		"- confinement_disabled apparmor:unconfined (service agent): Switches off part of the container's confinement.",
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
	if !strings.Contains(printed, "Plan digest: abc123digest") || !strings.Contains(printed, "--plan-digest") {
		t.Errorf("output lacks the digest and how to pass it:\n%s", printed)
	}
}

func TestAppInstallSendsThePlanDigestAndLeavesItOutByDefault(t *testing.T) {
	var gotBody apiv1.TemplateInstallRequest
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = apiv1.TemplateInstallRequest{}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body %q: %v", raw, err)
		}
		writeJSON(t, w, http.StatusOK, &apiv1.TemplateInstallResult{Stack: apiv1.Stack{Name: "agent", InstalledAt: time.Now().UTC()}, Plan: testInstallPlan()})
	})
	if _, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--plan-digest", "abc123digest"); err != nil {
		t.Fatal(err)
	}
	if got := gotBody.PlanDigest.Or(""); got != "abc123digest" {
		t.Errorf("planDigest = %q, want abc123digest", got)
	}
	if _, err := runAppCLI(t, sock, "app", "install", "risky-agent"); err != nil {
		t.Fatal(err)
	}
	if gotBody.PlanDigest.Set {
		t.Errorf("planDigest = %q sent without --plan-digest", gotBody.PlanDigest.Value)
	}
}

func TestAppInstallReportsAChangedTemplateInPlainWords(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"template_changed","message":"template: the template changed since it was previewed: x is not the revision that was previewed"}`))
	})
	_, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--plan-digest", "old")
	if err == nil {
		t.Fatal("a changed template was installed")
	}
	for _, want := range []string{"changed since it was previewed", "nothing was installed", "--dry-run", "--plan-digest"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "code 409") {
		t.Errorf("error %q is the raw API error", err)
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
	if err := json.Unmarshal([]byte(printed), &out); err != nil || len(out.Plan.Privileges) != 4 {
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

func TestAppInstallSendsTheContainerSettingsAndPrintsTheWarnings(t *testing.T) {
	var gotBody apiv1.TemplateInstallRequest
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("request body %q: %v", raw, err)
		}
		plan := testInstallPlan()
		plan.Warnings = []apiv1.ConversionWarning{
			{Class: apiv1.ConversionWarningClassMissingNetwork, Message: "The network iot does not exist.", Detail: apiv1.NewOptString("iot"), Command: apiv1.NewOptString("docker network create iot")},
			{Class: apiv1.ConversionWarningClassUntranslatedFlag, Message: "The ExtraParams flag --nope has no Compose equivalent the converter knows."},
		}
		writeJSON(t, w, http.StatusOK, &plan)
	})
	printed, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--dry-run", "--network", "iot", "--restart", "unless-stopped", "--cpus", "1.5", "--memory-mib", "512", "--extra-params", "--cap-add NET_ADMIN --nope")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody.NetworkMode.Or("") != "iot" || gotBody.Restart.Or("") != apiv1.TemplateInstallRequestRestartUnlessStopped || gotBody.Cpus.Or(0) != 1.5 || gotBody.MemoryMiB.Or(0) != 512 || gotBody.ExtraParams.Or("") != "--cap-add NET_ADMIN --nope" {
		t.Errorf("request body = %+v", gotBody)
	}
	for _, want := range []string{"Warnings:", "- missing_network: The network iot does not exist.", "    docker network create iot", "- untranslated_flag: The ExtraParams flag --nope"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

func TestAppInstallLeavesTheContainerSettingsOutWhenNoFlagIsGiven(t *testing.T) {
	var raw []byte
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		plan := testInstallPlan()
		writeJSON(t, w, http.StatusOK, &plan)
	})
	if _, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"networkMode", "restart", "cpus", "memoryMiB", "extraParams"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("request body %s carries %s although no flag was given", raw, key)
		}
	}
}

func TestAppInstallRefusesAFlagGivenNothingBeforeCallingTheDaemon(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, args := range [][]string{
		{"app", "install", "x", "--network", ""},
		{"app", "install", "x", "--restart", ""},
		{"app", "install", "x", "--restart", "sometimes"},
		{"app", "install", "x", "--extra-params", ""},
		{"app", "install", "x", "--cpus", "0"},
		{"app", "install", "x", "--memory-mib", "0"},
	} {
		if _, err := runAppCLI(t, sock, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
	if called {
		t.Error("a flag with no value reached the daemon")
	}
}

func TestAppNetworksListsTheDaemonsNetworksAndNeverAnEmptyListForUnreachableDocker(t *testing.T) {
	var gotRequest string
	available := true
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		out := &apiv1.ListDockerNetworksOK{Available: available, Networks: []apiv1.DockerNetwork{{Name: "bridge", Driver: "bridge"}, {Name: "lan", Driver: "macvlan"}}}
		if !available {
			out = &apiv1.ListDockerNetworksOK{Networks: []apiv1.DockerNetwork{}, Message: apiv1.NewOptString("Docker is not reachable")}
		}
		writeJSON(t, w, http.StatusOK, out)
	})
	printed, err := runAppCLI(t, sock, "app", "networks")
	if err != nil || gotRequest != "GET /api/v1/apps/networks" || !strings.Contains(printed, "lan\tmacvlan") {
		t.Fatalf("app networks = %q, %v (request %q)", printed, err, gotRequest)
	}
	available = false
	if _, err := runAppCLI(t, sock, "app", "networks"); err == nil || !strings.Contains(err.Error(), "Docker is not reachable") {
		t.Errorf("err = %v, want the unreachable Docker reported", err)
	}
}

func TestInstallSummaryPrintsControlCharactersEscaped(t *testing.T) {
	plan := testInstallPlan()
	plan.Template.ID = "agent\x1b[2K"
	plan.Inputs = []apiv1.TemplateInput{
		{Name: "NOTE", Kind: apiv1.TemplateInputKindString, Value: apiv1.NewOptString("a\x1b[1A\x1b[2K\nPrivileges: none beyond an ordinary container.\r\xff\x9b")},
		{Name: "BAD", Kind: apiv1.TemplateInputKindString, Error: apiv1.NewOptString("is \u009b[2Kbad")},
	}
	plan.Warnings = []apiv1.ConversionWarning{{
		Class: apiv1.ConversionWarningClassNote, Message: "read\x1b[2K this", Command: apiv1.NewOptString("docker network create x\x1b[1A"),
	}}
	plan.Privileges = []apiv1.TemplatePrivilege{
		{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "agent", Description: "Runs with full access to the server."},
		{Kind: apiv1.TemplatePrivilegeKindDockerSocket, Service: "agent\x1b[2K", Detail: apiv1.NewOptString("/var/run/docker.sock\x1b[1A\x1b[2K"), Description: "Can control Docker itself.\u009b"},
	}
	printed := installSummary("Would install", &plan, nil)

	assertNoTerminalControl(t, printed)
	for _, want := range []string{
		`from hoserva/agent\x1b[2K (revision 2).`,
		`  NOTE: a\x1b[1A\x1b[2K\nPrivileges: none beyond an ordinary container.\r\xff\x9b` + "\n",
		`  BAD: is \u009b[2Kbad` + "\n",
		`: read\x1b[2K this` + "\n",
		`      docker network create x\x1b[1A` + "\n",
		"  - privileged (service agent): Runs with full access to the server.\n",
		`  - docker_socket /var/run/docker.sock\x1b[1A\x1b[2K (service agent\x1b[2K): Can control Docker itself.\u009b` + "\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "\nPrivileges: none") {
		t.Errorf("a value forged a line of the summary:\n%s", printed)
	}
}

func TestAppInstallDryRunPrintsAnInputValueWithControlCharactersEscaped(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		plan := testInstallPlan()
		plan.Inputs = []apiv1.TemplateInput{{Name: "NOTE", Kind: apiv1.TemplateInputKindString, Value: apiv1.NewOptString("x\x1b[2K")}}
		writeJSON(t, w, http.StatusOK, &plan)
	})
	printed, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--dry-run")
	if err != nil {
		t.Fatalf("app install --dry-run: %v", err)
	}
	assertNoTerminalControl(t, printed)
	if !strings.Contains(printed, `  NOTE: x\x1b[2K`) {
		t.Errorf("output lacks the escaped value:\n%s", printed)
	}
}

func TestAppInstallRefusesAnEmptyPlanDigest(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		writeJSON(t, w, http.StatusOK, &apiv1.TemplateInstallResult{Stack: apiv1.Stack{Name: "agent", InstalledAt: time.Now().UTC()}, Plan: testInstallPlan()})
	})
	_, err := runAppCLI(t, sock, "app", "install", "risky-agent", "--plan-digest", "")
	if err == nil || !strings.Contains(err.Error(), "--plan-digest needs a value") {
		t.Fatalf("err = %v, want --plan-digest needs a value", err)
	}
	if called {
		t.Error("an empty --plan-digest reached the server")
	}
}
