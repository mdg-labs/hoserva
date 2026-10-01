package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func updateJob() *apiv1.Job {
	return &apiv1.Job{ID: uuid.New(), Type: apiv1.JobTypeContainerUpdate, Class: apiv1.JobClassService, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
}

func TestAppUpdateAndRevertQueueTheirJobs(t *testing.T) {
	for verb, want := range map[string]string{"update": "POST /api/v1/apps/plex/update", "revert": "POST /api/v1/apps/plex/revert"} {
		t.Run(verb, func(t *testing.T) {
			j := updateJob()
			var got string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.Method + " " + r.URL.Path
				writeJSON(t, w, http.StatusOK, j)
			})
			printed, err := runAppCLI(t, sock, "app", verb, "plex")
			if err != nil || got != want || !strings.Contains(printed, j.ID.String()) {
				t.Fatalf("app %s = %q, %v, request %q, want %q and the job id printed", verb, printed, err, got, want)
			}
		})
	}
}

func TestAppUpdateAllNamesTheContainersOrLeavesThemToTheDaemon(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantBody string
	}{
		{"every container with an update", []string{"app", "update-all"}, ""},
		{"named containers", []string{"app", "update-all", "plex", "nginx"}, `{"containers":["plex","nginx"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRequest, gotBody string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotRequest = r.Method + " " + r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = strings.TrimSpace(string(body))
				writeJSON(t, w, http.StatusOK, &apiv1.StartAppUpdatesOK{
					Job: apiv1.NewOptJob(*updateJob()), Containers: []string{"plex"},
					Skipped: []apiv1.AppUpdateSkipped{{Container: "db", Reason: "excluded from bulk updates"}},
				})
			})
			printed, err := runAppCLI(t, sock, tc.args...)
			if err != nil || gotRequest != "POST /api/v1/apps/updates" || gotBody != tc.wantBody {
				t.Fatalf("%v = %q, %v, request %q body %q, want POST /api/v1/apps/updates with body %q", tc.args, printed, err, gotRequest, gotBody, tc.wantBody)
			}
			if !strings.Contains(printed, "excluded from bulk updates") {
				t.Fatalf("output %q does not show what was skipped", printed)
			}
		})
	}
}

func TestAppUpdatePolicyNeedsExactlyOneChoice(t *testing.T) {
	var gotRequest, gotBody string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = strings.TrimSpace(string(body))
		writeJSON(t, w, http.StatusOK, &apiv1.AppUpdatePolicy{Container: "plex", BulkExcluded: strings.Contains(gotBody, "true")})
	})
	for _, args := range [][]string{{"app", "update-policy", "plex"}, {"app", "update-policy", "plex", "--include", "--exclude"}} {
		if _, err := runAppCLI(t, sock, args...); err == nil || gotRequest != "" {
			t.Fatalf("%v = %v, request %q, want a refusal before any request", args, err, gotRequest)
		}
	}
	for flag, want := range map[string]string{"--exclude": `{"bulkExcluded":true}`, "--include": `{"bulkExcluded":false}`} {
		if _, err := runAppCLI(t, sock, "app", "update-policy", "plex", flag); err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if gotRequest != "PUT /api/v1/apps/plex/update-policy" || gotBody != want {
			t.Fatalf("%s sent %s %s, want PUT /api/v1/apps/plex/update-policy %s", flag, gotRequest, gotBody, want)
		}
	}
}

func TestAppUpdateSettingsShowsOrSetsTheKeepPeriod(t *testing.T) {
	var gotRequest, gotBody string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = strings.TrimSpace(string(body))
		days := 7
		if r.Method == http.MethodPut {
			days = 30
		}
		writeJSON(t, w, http.StatusOK, &apiv1.AppSettings{ImageKeepDays: days})
	})
	if printed, err := runAppCLI(t, sock, "app", "update-settings"); err != nil || gotRequest != "GET /api/v1/settings/apps" || !strings.Contains(printed, `"imageKeepDays": 7`) {
		t.Fatalf("app update-settings = %q, %v, request %q", printed, err, gotRequest)
	}
	if printed, err := runAppCLI(t, sock, "app", "update-settings", "--keep-days", "30"); err != nil || gotRequest != "PUT /api/v1/settings/apps" || gotBody != `{"imageKeepDays":30}` || !strings.Contains(printed, "30") {
		t.Fatalf("app update-settings --keep-days 30 = %q, %v, request %q body %q", printed, err, gotRequest, gotBody)
	}
}

func TestAppUpdatesAndHistoryAreRead(t *testing.T) {
	for verb, want := range map[string]string{"updates": "GET /api/v1/apps/updates", "update-history": "GET /api/v1/apps/updates/history"} {
		t.Run(verb, func(t *testing.T) {
			var got string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.Method + " " + r.URL.Path
				if strings.HasSuffix(r.URL.Path, "/history") {
					writeJSON(t, w, http.StatusOK, &apiv1.ListAppUpdateHistoryOK{Available: true, Records: []apiv1.AppUpdateRecord{}})
					return
				}
				writeJSON(t, w, http.StatusOK, &apiv1.ListAppUpdatesOK{Available: true, Updates: []apiv1.AppUpdate{}})
			})
			if _, err := runAppCLI(t, sock, "app", verb); err != nil || got != want {
				t.Fatalf("app %s: %v, request %q, want %q", verb, err, got, want)
			}
		})
	}
}
