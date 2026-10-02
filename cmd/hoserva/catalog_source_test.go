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

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testCatalogSources() *apiv1.CatalogSourceList {
	return &apiv1.CatalogSourceList{Sources: []apiv1.CatalogSource{
		{ID: "hoserva", URL: "https://catalog.hoserva.dev", Kind: apiv1.CatalogSourceKindCurated, Signed: true, Serial: apiv1.NewOptInt64(42), LastRefreshedAt: apiv1.NewOptDateTime(time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC))},
		{ID: "src-0123456789", URL: "https://example.com/catalog", Kind: apiv1.CatalogSourceKindUserAdded, Signed: false},
	}}
}

func TestCatalogSourceListPrintsEachSourcesBadge(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, testCatalogSources())
	})
	printed, err := runAppCLI(t, sock, "catalog", "source", "list")
	if err != nil {
		t.Fatal(err)
	}
	if gotRequest != "GET /api/v1/catalog-sources" {
		t.Errorf("request = %q", gotRequest)
	}
	var curated, user string
	for _, line := range strings.Split(printed, "\n") {
		switch {
		case strings.HasPrefix(line, "hoserva"):
			curated = line
		case strings.HasPrefix(line, "src-0123456789"):
			user = line
		}
	}
	for _, want := range []string{"curated", "signed", "42", "2026-10-02 08:00 UTC", "https://catalog.hoserva.dev"} {
		if !strings.Contains(curated, want) {
			t.Errorf("curated row %q lacks %q", curated, want)
		}
	}
	for _, want := range []string{"user-added", "unsigned", "never", "https://example.com/catalog"} {
		if !strings.Contains(user, want) {
			t.Errorf("user-added row %q lacks %q", user, want)
		}
	}
}

func TestCatalogSourceAddSendsTheURLAndKeyAndWarnsAboutAnUnsignedSource(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(keyFile, []byte("-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var bodies []apiv1.AddCatalogSourceRequest
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/catalog-sources" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var req apiv1.AddCatalogSourceRequest
		if err := req.UnmarshalJSON(raw); err != nil {
			t.Errorf("body %q: %v", raw, err)
		}
		bodies = append(bodies, req)
		writeJSON(t, w, http.StatusOK, &apiv1.CatalogSource{ID: "src-0123456789", URL: req.URL, Kind: apiv1.CatalogSourceKindUserAdded, Signed: req.PublicKey.Or("") != ""})
	})

	printed, err := runAppCLI(t, sock, "catalog", "source", "add", "https://example.com/catalog")
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 || bodies[0].URL != "https://example.com/catalog" || bodies[0].PublicKey.IsSet() {
		t.Fatalf("request bodies = %+v, want the URL and no key", bodies)
	}
	if !strings.Contains(printed, "Added catalog source src-0123456789 (user-added, unsigned).") || !strings.Contains(printed, "This source is unsigned") {
		t.Errorf("output = %q", printed)
	}

	printed, err = runAppCLI(t, sock, "catalog", "source", "add", "https://example.com/other", "--public-key-file", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !strings.HasPrefix(bodies[1].PublicKey.Or(""), "-----BEGIN PUBLIC KEY-----") {
		t.Fatalf("request bodies = %+v, want the file's PEM as the key", bodies)
	}
	if !strings.Contains(printed, "(user-added, signed)") || strings.Contains(printed, "This source is unsigned") {
		t.Errorf("output = %q", printed)
	}

	if _, err := runAppCLI(t, sock, "catalog", "source", "add", "https://example.com/x", "--public-key", "a", "--public-key-file", keyFile); err == nil {
		t.Error("--public-key and --public-key-file were accepted together")
	}
	blank := filepath.Join(t.TempDir(), "blank.pem")
	if err := os.WriteFile(blank, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--public-key", ""}, {"--public-key", "  "}, {"--public-key-file", blank}} {
		if _, err := runAppCLI(t, sock, append([]string{"catalog", "source", "add", "https://example.com/x"}, args...)...); err == nil {
			t.Errorf("%v added an unsigned source instead of refusing a blank key", args)
		}
	}
	if len(bodies) != 2 {
		t.Errorf("a refused invocation still called the API: %d requests", len(bodies))
	}
}

func TestCatalogSourceAddReportsARefusalAndPrintsNothing(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnprocessableEntity, &apiv1.Error{Code: "catalog_source_rejected", Message: "the signature does not verify"})
	})
	printed, err := runAppCLI(t, sock, "catalog", "source", "add", "https://example.com/catalog")
	if err == nil || printed != "" {
		t.Fatalf("printed %q, err %v, want an error and no output", printed, err)
	}
}

func TestCatalogSourceRefreshAndRemoveCallTheirOperation(t *testing.T) {
	var requests []string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		switch r.Method {
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(t, w, http.StatusOK, &apiv1.CatalogRefresh{CheckedAt: time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC), Outcome: apiv1.CatalogCheckOutcomeUnchanged})
		}
	})
	printed, err := runAppCLI(t, sock, "catalog", "source", "refresh", "src-0123456789")
	if err != nil || !strings.Contains(printed, "already up to date") {
		t.Fatalf("refresh: %q, %v", printed, err)
	}
	printed, err = runAppCLI(t, sock, "catalog", "source", "remove", "src-0123456789")
	if err != nil || !strings.Contains(printed, "Removed catalog source src-0123456789.") {
		t.Fatalf("remove: %q, %v", printed, err)
	}
	want := []string{"POST /api/v1/catalog-sources/src-0123456789/refresh", "DELETE /api/v1/catalog-sources/src-0123456789"}
	if strings.Join(requests, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
}

func TestCatalogSourceRefreshOfAFailedCheckExitsNonZero(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &apiv1.CatalogRefresh{
			CheckedAt: time.Now().UTC(), Outcome: apiv1.CatalogCheckOutcomeFailed,
			Reason: apiv1.NewOptCatalogRefreshReason(apiv1.CatalogRefreshReasonBadSignature), Message: apiv1.NewOptString("no"),
		})
	})
	_, err := runAppCLI(t, sock, "catalog", "source", "refresh", "src-0123456789")
	if err == nil || !strings.Contains(err.Error(), "bad_signature") {
		t.Fatalf("err = %v, want the failed check as an error", err)
	}
}

func TestCatalogSourceRemoveOfTheCuratedCatalogIsReportedAsAnError(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusConflict, &apiv1.Error{Code: "catalog_source_curated", Message: "the curated catalog cannot be removed"})
	})
	printed, err := runAppCLI(t, sock, "catalog", "source", "remove", "hoserva")
	if err == nil || printed != "" {
		t.Fatalf("printed %q, err %v", printed, err)
	}
}

func TestStackTemplateUpdatePrintsTheDiffAndSaysNothingWasChanged(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, &apiv1.StackTemplateUpdate{
			Status: apiv1.StackTemplateUpdateStatusUpdateAvailable, Source: apiv1.NewOptString("src-0123456789"), TemplateId: apiv1.NewOptString("probe"),
			InstalledRevision: apiv1.NewOptInt(2), AvailableRevision: apiv1.NewOptInt(4),
			SourceKind: apiv1.NewOptCatalogSourceKind(apiv1.CatalogSourceKindUserAdded), Signed: apiv1.NewOptBool(false),
			ManuallyEdited: true, Diff: apiv1.NewOptString("--- a\n+++ b\n@@ -1 +1 @@\n-old\n+new\n"),
		})
	})
	printed, err := runAppCLI(t, sock, "stack", "template-update", "probe")
	if err != nil {
		t.Fatal(err)
	}
	if gotRequest != "GET /api/v1/stacks/probe/template-update" {
		t.Errorf("request = %q", gotRequest)
	}
	for _, want := range []string{
		"Template update available for stack probe: revision 2 is installed, revision 4 is available from source src-0123456789 (user-added, unsigned).",
		"edited by hand", "The source is unsigned", "Nothing was changed.", "-old\n+new\n",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

func TestStackTemplateUpdateJSONAndTheOtherStatuses(t *testing.T) {
	status := apiv1.StackTemplateUpdateStatusUpToDate
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &apiv1.StackTemplateUpdate{Status: status, Source: apiv1.NewOptString("hoserva"), TemplateId: apiv1.NewOptString("jellyfin"), InstalledRevision: apiv1.NewOptInt(1), AvailableRevision: apiv1.NewOptInt(1)})
	})
	for st, want := range map[apiv1.StackTemplateUpdateStatus]string{
		apiv1.StackTemplateUpdateStatusUpToDate:        "is up to date",
		apiv1.StackTemplateUpdateStatusNotFromTemplate: "was not installed from a template",
		apiv1.StackTemplateUpdateStatusSourceRemoved:   "is no longer a source",
		apiv1.StackTemplateUpdateStatusTemplateRemoved: "no longer lists the template jellyfin",
	} {
		status = st
		printed, err := runAppCLI(t, sock, "stack", "template-update", "media")
		if err != nil || !strings.Contains(printed, want) {
			t.Errorf("%s: %q, %v, want %q", st, printed, err, want)
		}
	}
	status = apiv1.StackTemplateUpdateStatusUpToDate
	printed, err := runAppCLI(t, sock, "--json", "stack", "template-update", "media")
	var out struct {
		Status string `json:"status"`
	}
	if err != nil || json.Unmarshal([]byte(printed), &out) != nil || out.Status != "up_to_date" {
		t.Fatalf("--json: %q, %v", printed, err)
	}
}
