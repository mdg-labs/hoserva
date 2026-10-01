package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testCatalogList() *apiv1.CatalogList {
	return &apiv1.CatalogList{
		Serial:      42,
		GeneratedAt: apiv1.NewOptDateTime(time.Date(2026, 10, 1, 11, 14, 0, 0, time.UTC)),
		Templates: []apiv1.CatalogEntry{
			{ID: "jellyfin", Revision: 4, Title: "Jellyfin", Categories: []string{"media"}, Docs: "https://example.com/jf", Source: "hoserva", Installed: true},
			{ID: "gitea", Revision: 1, Title: "Gitea", Categories: []string{"development", "git"}, Docs: "https://example.com/gitea", Source: "hoserva"},
		},
	}
}

func TestCatalogListPrintsEveryEntryWithItsSourceAndInstalledState(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, testCatalogList())
	})
	printed, err := runAppCLI(t, sock, "catalog", "list")
	if err != nil {
		t.Fatalf("catalog list: %v", err)
	}
	if gotRequest != "GET /api/v1/catalog" {
		t.Errorf("request = %q", gotRequest)
	}
	for _, want := range []string{"Catalog serial 42, built 2026-10-01 11:14 UTC, 2 templates.", "jellyfin", "Jellyfin", "development,git", "hoserva", "yes", "no"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	for _, line := range strings.Split(printed, "\n") {
		if strings.HasPrefix(line, "jellyfin") && !strings.HasSuffix(strings.TrimSpace(line), "yes") {
			t.Errorf("jellyfin is installed but its row says %q", line)
		}
		if strings.HasPrefix(line, "gitea") && !strings.HasSuffix(strings.TrimSpace(line), "no") {
			t.Errorf("gitea is not installed but its row says %q", line)
		}
	}
}

func TestCatalogListJSONEmitsTheListAsTheAPIReturnedIt(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, testCatalogList())
	})
	printed, err := runAppCLI(t, sock, "--json", "catalog", "list")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Serial    int64 `json:"serial"`
		Templates []struct {
			ID        string `json:"id"`
			Source    string `json:"source"`
			Installed bool   `json:"installed"`
		} `json:"templates"`
	}
	if err := json.Unmarshal([]byte(printed), &out); err != nil || out.Serial != 42 || len(out.Templates) != 2 || out.Templates[0].Source != "hoserva" || !out.Templates[0].Installed {
		t.Errorf("output %q: %v", printed, err)
	}
}

func TestCatalogShowPrintsThePrivilegesAndTheComposeText(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, &apiv1.CatalogTemplate{
			ID: "risky-agent", Revision: 2, Title: "Risky agent", Categories: []string{"system"}, Docs: "https://example.com/agent",
			Source:  "hoserva",
			Compose: "services:\n  agent:\n    privileged: true\n",
			Privileges: []apiv1.TemplatePrivilege{
				{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "agent", Description: "Runs with full access to the server."},
				{Kind: apiv1.TemplatePrivilegeKindDockerSocket, Service: "agent", Detail: apiv1.NewOptString("/var/run/docker.sock"), Description: "Can control Docker itself."},
			},
		})
	})
	printed, err := runAppCLI(t, sock, "catalog", "show", "risky-agent")
	if err != nil {
		t.Fatalf("catalog show: %v", err)
	}
	if gotRequest != "GET /api/v1/catalog/risky-agent" {
		t.Errorf("request = %q", gotRequest)
	}
	for _, want := range []string{
		"Risky agent (risky-agent), revision 2, from the hoserva source.",
		"- privileged (service agent): Runs with full access to the server.",
		"- docker_socket /var/run/docker.sock (service agent): Can control Docker itself.",
		"compose.yaml:\nservices:\n  agent:\n    privileged: true\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

func TestCatalogShowReportsAnErrorTheDaemonReturns(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, &apiv1.Error{Code: "template_not_found", Message: "no template with that id"})
	})
	printed, err := runAppCLI(t, sock, "catalog", "show", "nope")
	if err == nil || printed != "" {
		t.Fatalf("catalog show of an unknown template: printed %q, err %v, want an error and no output", printed, err)
	}
}

func TestCatalogListReportsTheCatalogBeingUnavailable(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusServiceUnavailable, &apiv1.Error{Code: "catalog_unavailable", Message: "the catalog is not available"})
	})
	printed, err := runAppCLI(t, sock, "catalog", "list")
	if err == nil || printed != "" {
		t.Fatalf("printed %q, err %v, want an error and no table", printed, err)
	}
}

func TestCatalogRefreshPostsOneCheckAndSummarisesEachOutcome(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		res  apiv1.CatalogRefresh
		want []string
		fail string
	}{
		{"updated", apiv1.CatalogRefresh{CheckedAt: at, Outcome: apiv1.CatalogCheckOutcomeUpdated, NewTemplates: apiv1.NewOptInt(3), UpdatedTemplates: apiv1.NewOptInt(1)},
			[]string{"Checked 2026-10-01 12:30 UTC", "updated, 3 new and 1 updated templates"}, ""},
		{"unchanged", apiv1.CatalogRefresh{CheckedAt: at, Outcome: apiv1.CatalogCheckOutcomeUnchanged},
			[]string{"already up to date"}, ""},
		{"failed", apiv1.CatalogRefresh{CheckedAt: at, Outcome: apiv1.CatalogCheckOutcomeFailed, Reason: apiv1.NewOptCatalogRefreshReason(apiv1.CatalogRefreshReasonNotNewer), Message: apiv1.NewOptString("serial 9, installed 9")},
			nil, "the catalog check failed (not_newer): serial 9, installed 9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var requests []string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				writeJSON(t, w, http.StatusOK, &c.res)
			})
			printed, err := runAppCLI(t, sock, "catalog", "refresh")
			if len(requests) != 1 || requests[0] != "POST /api/v1/catalog/refresh" {
				t.Fatalf("requests = %v", requests)
			}
			if c.fail != "" {
				if err == nil || err.Error() != c.fail {
					t.Fatalf("error = %v, want %q (a failed check must exit non-zero)", err, c.fail)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				if !strings.Contains(printed, want) {
					t.Errorf("output lacks %q:\n%s", want, printed)
				}
			}
		})
	}
}

func TestCatalogRefreshJSONEmitsTheResultAndAFailedCheckStillFails(t *testing.T) {
	res := apiv1.CatalogRefresh{CheckedAt: time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC), Outcome: apiv1.CatalogCheckOutcomeFailed,
		Reason: apiv1.NewOptCatalogRefreshReason(apiv1.CatalogRefreshReasonFetchFailed), Message: apiv1.NewOptString("no route")}
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { writeJSON(t, w, http.StatusOK, &res) })
	printed, err := runAppCLI(t, sock, "--json", "catalog", "refresh")
	if err == nil {
		t.Fatal("a failed check returned no error")
	}
	var out struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if jerr := json.Unmarshal([]byte(printed), &out); jerr != nil || out.Outcome != "failed" || out.Reason != "fetch_failed" || out.Message != "no route" {
		t.Errorf("output %q: %v", printed, jerr)
	}
}

func TestCatalogListShowsTheLastCheckWhenThereWasOne(t *testing.T) {
	list := testCatalogList()
	list.LastCheckedAt = apiv1.NewOptDateTime(time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC))
	list.LastOutcome = apiv1.NewOptCatalogCheckOutcome(apiv1.CatalogCheckOutcomeUnchanged)
	if got := catalogListSummary(list); !strings.Contains(got, "Last checked 2026-10-01 12:30 UTC: unchanged.") {
		t.Errorf("summary lacks the last check:\n%s", got)
	}
	if got := catalogListSummary(testCatalogList()); strings.Contains(got, "Last checked") {
		t.Errorf("summary names a check that never ran:\n%s", got)
	}
}
