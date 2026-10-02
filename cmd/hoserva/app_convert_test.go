package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/template"
)

func TestAppConvertSendsTheFileAndPrintsSourceComposeAndEveryWarning(t *testing.T) {
	const xml = "<Container><Repository>x/y:1</Repository></Container>\n"
	file := filepath.Join(t.TempDir(), "t.xml")
	if err := os.WriteFile(file, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotRequest string
	var gotBody struct{ XML string }
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		writeJSON(t, w, http.StatusOK, &apiv1.UnraidConversion{
			Source:  xml,
			Compose: "services:\n  y:\n    image: x/y:1\n",
			Clean:   false,
			Warnings: []apiv1.ConversionWarning{
				{Class: apiv1.ConversionWarningClassUntranslatedFlag, Message: "no equivalent", Detail: apiv1.NewOptString("--exotic=1")},
				{Class: apiv1.ConversionWarningClassMissingNetwork, Message: "needs br0", Command: apiv1.NewOptString("docker network create -d macvlan br0")},
			},
			Privileges: []apiv1.TemplatePrivilege{{Kind: apiv1.TemplatePrivilegeKindAddedCapabilities, Service: "y", Detail: apiv1.NewOptString("NET_ADMIN"), Description: "extra capabilities"}},
			Metadata:   apiv1.UnraidTemplateMetadata{Title: "y", Variables: []apiv1.UnraidVariable{}},
		})
	})
	printed, err := runAppCLI(t, sock, "app", "convert", file)
	if err != nil {
		t.Fatalf("app convert: %v", err)
	}
	if gotRequest != "POST /api/v1/apps/convert" || gotBody.XML != xml {
		t.Errorf("sent %s with xml %q, want POST /api/v1/apps/convert with the file's text", gotRequest, gotBody.XML)
	}
	for _, want := range []string{
		"=== Unraid template", "<Container><Repository>x/y:1</Repository></Container>",
		"=== Generated Compose", "image: x/y:1",
		"[untranslated_flag] no equivalent", "--exotic=1",
		"[missing_network] needs br0", "command: docker network create -d macvlan br0",
		"added_capabilities NET_ADMIN (service y)", "needs manual review",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if strings.Index(printed, "=== Unraid template") > strings.Index(printed, "=== Generated Compose") {
		t.Error("the source is not shown before the generated Compose")
	}
}

func TestAppConvertJSONPrintsTheWholeConversion(t *testing.T) {
	file := filepath.Join(t.TempDir(), "t.xml")
	if err := os.WriteFile(file, []byte("<Container/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, &apiv1.UnraidConversion{
			Source: "<Container/>", Compose: "services: {}\n", Clean: true,
			Warnings: []apiv1.ConversionWarning{}, Privileges: []apiv1.TemplatePrivilege{},
			Metadata: apiv1.UnraidTemplateMetadata{Variables: []apiv1.UnraidVariable{}},
		})
	})
	printed, err := runAppCLI(t, sock, "--json", "app", "convert", file)
	if err != nil || !strings.Contains(printed, `"compose": "services: {}\n"`) || !strings.Contains(printed, `"source"`) || !strings.Contains(printed, `"clean": true`) {
		t.Fatalf("app convert --json = %q, %v", printed, err)
	}
}

func TestAppConvertReportsTheDaemonsRefusal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "t.xml")
	if err := os.WriteFile(file, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"invalid_unraid_template","message":"not a usable Unraid template: not valid XML"}`))
	})
	_, err := runAppCLI(t, sock, "app", "convert", file)
	if err == nil || !strings.Contains(err.Error(), "not a usable Unraid template") {
		t.Fatalf("err = %v, want the daemon's message", err)
	}
}

func TestAppConvertRefusesAFileThatIsNotATemplateSizedFileWithoutCallingTheDaemon(t *testing.T) {
	file := filepath.Join(t.TempDir(), "big.xml")
	if err := os.WriteFile(file, []byte(strings.Repeat("a", template.MaxUnraidTemplateBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	if _, err := runAppCLI(t, sock, "app", "convert", file); err == nil || called {
		t.Fatalf("err = %v, daemon called = %v, want a refusal before any request", err, called)
	}
	if _, err := runAppCLI(t, sock, "app", "convert", filepath.Join(t.TempDir(), "missing.xml")); err == nil || called {
		t.Fatalf("a missing file: err = %v, daemon called = %v", err, called)
	}
}
