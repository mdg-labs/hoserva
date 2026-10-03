package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func startTemplatesDaemon(t *testing.T) (sock string, paths func() []string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock = filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out []byte
		var err error
		switch r.URL.Path {
		case "/api/v1/migrate/templates":
			list := apiv1.MigrationTemplates{
				Counts: apiv1.MigrationTemplateCounts{Clean: 1, WithWarnings: 1, Failed: 1, TemplateOnly: 1, ComposeProjects: 1},
				Templates: []apiv1.MigrationTemplateSummary{
					{Name: "plain", File: "my-plain.xml", Class: apiv1.MigrationTemplateClassRunning, Counted: true, Status: apiv1.MigrationTemplateStatusClean},
					{Name: "lan", File: "my-lan.xml", Class: apiv1.MigrationTemplateClassAutostart, Counted: true, Status: apiv1.MigrationTemplateStatusWarnings, WarningCount: 2},
					{Name: "big", File: "my-big.xml", Class: apiv1.MigrationTemplateClassStopped, Counted: true, Status: apiv1.MigrationTemplateStatusFailed, Error: apiv1.NewOptString("the template is larger than 49152 bytes")},
					{Name: "old", File: "my-old.xml", Class: apiv1.MigrationTemplateClassTemplateOnly, Status: apiv1.MigrationTemplateStatusClean},
				},
				ComposeProjects: []apiv1.MigrationComposeProjectSummary{{Name: "stack", Containers: []string{"stack-web"}, Status: apiv1.MigrationTemplateStatusPreviewed}},
			}
			out, err = list.MarshalJSON()
		case "/api/v1/migrate/templates/my-lan.xml":
			pv := apiv1.MigrationTemplatePreview{
				Kind: apiv1.MigrationTemplatePreviewKindTemplate, Name: "my-lan.xml", Status: apiv1.MigrationTemplateStatusWarnings,
				Source: "<Container><Name>lan</Name></Container>", Compose: apiv1.NewOptString("services:\n  lan:\n    image: x\n"),
				Warnings: []apiv1.ConversionWarning{{
					Class: apiv1.ConversionWarningClassMissingNetwork, Message: "The template uses the custom network lan.",
					Detail: apiv1.NewOptString("lan"), Command: apiv1.NewOptString("docker network create -d macvlan lan"),
				}},
				Privileges: []apiv1.TemplatePrivilege{},
			}
			out, err = pv.MarshalJSON()
		case "/api/v1/migrate/templates/stack":
			pv := apiv1.MigrationTemplatePreview{
				Kind: apiv1.MigrationTemplatePreviewKindComposeProject, Name: "stack", Status: apiv1.MigrationTemplateStatusPreviewed,
				Source: "services:\n  web:\n    privileged: true\n", Warnings: []apiv1.ConversionWarning{},
				Privileges: []apiv1.TemplatePrivilege{{Kind: apiv1.TemplatePrivilegeKindPrivileged, Service: "web", Description: "Runs with full access."}},
			}
			out, err = pv.MarshalJSON()
		default:
			w.WriteHeader(http.StatusNotFound)
			out, err = (&apiv1.Error{Code: "template_not_found", Message: "there is no template with this name"}).MarshalJSON()
		}
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestMigrateTemplatesListsEveryTemplateWithTheCounts(t *testing.T) {
	sock, seen := startTemplatesDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1 convert cleanly, 1 with warnings, 1 could not be converted",
		"Template only (previewed, not counted): 1",
		"my-lan.xml", "autostart", "warnings", "my-old.xml", "template_only",
		"my-big.xml could not be converted: the template is larger than 49152 bytes",
		"stack", "previewed",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	if got := seen(); len(got) != 1 || got[0] != "GET /api/v1/migrate/templates" {
		t.Errorf("requests = %v", got)
	}
}

func TestMigrateTemplatesPrintsOnePreviewByName(t *testing.T) {
	sock, seen := startTemplatesDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "templates", "my-lan.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<Name>lan</Name>", "image: x", "[missing_network]", "command: docker network create -d macvlan lan", "needs manual review"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	printed, err = runBackupCLI(t, sock, "migrate", "templates", "stack")
	if err != nil || !strings.Contains(printed, "Compose Manager project stack") || !strings.Contains(printed, "privileged (service web)") {
		t.Errorf("project preview = %q, %v", printed, err)
	}
	if got := seen(); len(got) != 2 || got[0] != "GET /api/v1/migrate/templates/my-lan.xml" {
		t.Errorf("requests = %v", got)
	}
}

func TestMigrateTemplatesRefusalNamesTheCode(t *testing.T) {
	sock, _ := startTemplatesDaemon(t)
	if _, err := runBackupCLI(t, sock, "migrate", "templates", "nothing.xml"); err == nil || !strings.Contains(err.Error(), "template_not_found") {
		t.Errorf("migrate templates nothing.xml = %v, want the refusal's code", err)
	}
	if _, err := runBackupCLI(t, sock, "migrate", "templates", "a", "b"); err == nil {
		t.Error("migrate templates with two names succeeded")
	}
}
