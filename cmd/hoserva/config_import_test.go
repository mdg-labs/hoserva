package main

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func testConfigImportPreview() *apiv1.ConfigImportPreview {
	return &apiv1.ConfigImportPreview{
		Archive: apiv1.ConfigImportArchive{
			Timestamp: time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC), Host: "nas", HoservaVersion: "1.2.3", SchemaVersion: "20260901000000",
		},
		LiveSchemaVersion: "20260901000000",
		Blockers: []apiv1.ConfigImportBlocker{{
			Code: apiv1.ConfigImportBlockerCodeArchiveArrayMismatch, Message: "the archive's array differs from the live array",
		}},
		Groups: []apiv1.ConfigImportGroup{
			{
				Category: apiv1.ConfigImportGroupCategoryShares,
				Added:    []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShare, Name: "photos"}},
				Changed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShareUserPermission, Name: "media / alice"}},
				Removed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindShare, Name: "scratch"}},
			},
			{
				Category: apiv1.ConfigImportGroupCategorySystem,
				Changed:  []apiv1.ConfigImportChange{{Kind: apiv1.ConfigImportChangeKindUps}},
			},
			{Category: apiv1.ConfigImportGroupCategorySchedules},
		},
		Notes: []apiv1.ConfigImportNote{{Code: apiv1.ConfigImportNoteCodeSessionsReplaced, Message: "Active sign-in sessions are replaced by the archive's."}},
	}
}

func writeArchiveFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hoserva-config.tar.zst")
	if err := os.WriteFile(path, []byte("archive bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigImportPreview_SendsTheArchiveWithoutConfirmAndPrintsTheGroups(t *testing.T) {
	var gotRequest, gotArchive, gotFields string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading part: %v", err)
				return
			}
			b, _ := io.ReadAll(part)
			if part.FormName() == "archive" {
				gotArchive = string(b)
			} else {
				gotFields += part.FormName() + ";"
			}
		}
		writeJSON(t, w, http.StatusOK, testConfigImportPreview())
	})

	printed, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview: %v", err)
	}
	if gotRequest != "POST /api/v1/config/import/preview" {
		t.Fatalf("request = %q, want POST /api/v1/config/import/preview", gotRequest)
	}
	if gotArchive != "archive bytes" || gotFields != "" {
		t.Fatalf("archive = %q and other fields = %q, want the archive alone (no confirm)", gotArchive, gotFields)
	}
	for _, want := range []string{
		"nas", "1.2.3",
		"Import would be refused (archive_array_mismatch): the archive's array differs from the live array",
		"Shares and share permissions", "+ share: photos", "~ share user permission: media / alice", "- share: scratch",
		"System settings", "~ ups",
		"Active sign-in sessions are replaced by the archive's.",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output does not contain %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "Schedules") || strings.Contains(printed, "No changes") {
		t.Errorf("output lists a category with no changes, or claims none:\n%s", printed)
	}
}

func TestConfigImportPreview_JSONOutputIsTheResponse(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, testConfigImportPreview())
	})
	printed, err := runAppCLI(t, sock, "--json", "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview --json: %v", err)
	}
	var got apiv1.ConfigImportPreview
	if err := json.Unmarshal([]byte(printed), &got); err != nil {
		t.Fatalf("output is not the preview's JSON: %v\n%s", err, printed)
	}
	if len(got.Groups) != 3 || got.Archive.Host != "nas" {
		t.Fatalf("decoded preview = %+v", got)
	}
}

func TestConfigImportPreview_ReportsAnUnchangedArchive(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		p := testConfigImportPreview()
		p.Blockers = []apiv1.ConfigImportBlocker{}
		p.Groups = []apiv1.ConfigImportGroup{{Category: apiv1.ConfigImportGroupCategoryShares}}
		writeJSON(t, w, http.StatusOK, p)
	})
	printed, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err != nil {
		t.Fatalf("config import --preview: %v", err)
	}
	if !strings.Contains(printed, "No changes") || strings.Contains(printed, "refused") {
		t.Fatalf("output = %q, want an unchanged archive reported plainly", printed)
	}
}

func TestConfigImportPreview_RefusalIsACommandFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusBadRequest, &apiv1.Error{Code: "invalid_archive", Message: "checksum mismatch for state.db"})
	})
	_, err := runAppCLI(t, sock, "config", "import", "--preview", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("config import --preview of an invalid archive: %v, want the daemon's refusal", err)
	}
}

func TestConfigImport_PreviewAndConfirmAreExclusive(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := runAppCLI(t, sock, "config", "import", "--preview", "--confirm", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "--preview") {
		t.Fatalf("config import --preview --confirm: %v, want a refusal naming --preview", err)
	}
	if called {
		t.Fatal("the daemon was called")
	}
}

func TestConfigImport_StillRequiresConfirmWithoutPreview(t *testing.T) {
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	_, err := runAppCLI(t, sock, "config", "import", writeArchiveFile(t))
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("config import without --confirm: %v", err)
	}
	if called {
		t.Fatal("the daemon was called")
	}
}
