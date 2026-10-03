package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/store"
)

func TestMigrationWiring_TemplatePreviewsAreServedFromTheScan(t *testing.T) {
	w := newContainersWiringHarness(t)
	disks := disk.NewFakeProvider()
	disks.AddDisk("/dev/sdb", disk.Disk{Serial: "WIREDSERIAL", Size: 1 << 40})
	if err := wireMigration(context.Background(), w.handler, w.registry, disks, disk.NewFakeReadOnlyMounter(), disk.NewFakeRunner(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if status, body := w.do(t, http.MethodGet, "/migrate/templates"); status != http.StatusNotFound || !strings.Contains(string(body), "no_migration_report") {
		t.Fatalf("GET /migrate/templates before a scan = %d %s, want 404 no_migration_report", status, body)
	}

	inspect := func(name string) string {
		return `{"Name":"/` + name + `","State":{"Status":"running","Running":true},"Config":{"Labels":{"net.unraid.docker.managed":"dockerman"}}}`
	}
	tmpl := func(name, network string) string {
		return `<Container version="2"><Name>` + name + `</Name><Repository>fixture/` + name + `:1</Repository><Network>` + network + `</Network>` +
			`<Config Name="Web" Target="80" Default="8080" Mode="tcp" Type="Port">8080</Config></Container>`
	}
	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/hoserva/capture.json":                                `{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":{"mode":"usb"},"docker":{"state":"running","directory_location":"array","writable_layers":[]},"libvirt_img_location":"none"}`,
		"config/hoserva/containers.json":                             "[" + inspect("plain") + "," + inspect("lan") + "]",
		"config/hoserva/networks.json":                               `[{"Name":"lan","Driver":"macvlan","Scope":"local","IPAM":{"Config":[{"Subnet":"10.0.0.0/24","Gateway":"10.0.0.1"}]},"Options":{"parent":"eth0"}}]`,
		"config/plugins/dockerMan/templates-user/my-plain.xml":       tmpl("plain", "bridge"),
		"config/plugins/dockerMan/templates-user/my-lan.xml":         tmpl("lan", "lan"),
		"config/plugins/dockerMan/templates-user/my-old.xml":         tmpl("old", "bridge"),
		"config/plugins/compose.manager/projects/stack/compose.yaml": "services:\n  web:\n    image: x\n    privileged: true\n",
	})
	status, body := w.uploadScan(t, zipData, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var queued struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &queued); err != nil {
		t.Fatal(err)
	}
	if done := w.awaitJobByID(t, queued.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/templates")
	var list apiv1.MigrationTemplates
	if err := json.Unmarshal(body, &list); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate/templates = %d %s (%v)", status, body, err)
	}
	if c := list.Counts; c.Clean != 1 || c.WithWarnings != 1 || c.Failed != 0 || c.TemplateOnly != 1 || c.AllTemplates || c.ComposeProjects != 1 {
		t.Errorf("counts = %+v, want 1 clean, 1 with warnings, 1 template only, 1 project", c)
	}
	byFile := map[string]apiv1.MigrationTemplateSummary{}
	for _, it := range list.Templates {
		byFile[it.File] = it
	}
	if it := byFile["my-lan.xml"]; it.Status != apiv1.MigrationTemplateStatusWarnings || it.WarningCount != 1 || !it.Counted || it.Class != apiv1.MigrationTemplateClassRunning {
		t.Errorf("my-lan.xml = %+v", it)
	}
	if it := byFile["my-old.xml"]; it.Counted || it.Class != apiv1.MigrationTemplateClassTemplateOnly {
		t.Errorf("my-old.xml = %+v, want template only and not counted", it)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/templates/my-lan.xml")
	var pv apiv1.MigrationTemplatePreview
	if err := json.Unmarshal(body, &pv); status != http.StatusOK || err != nil {
		t.Fatalf("GET /migrate/templates/my-lan.xml = %d %s (%v)", status, body, err)
	}
	var command string
	for _, wn := range pv.Warnings {
		if wn.Class == apiv1.ConversionWarningClassMissingNetwork {
			command = wn.Command.Or("")
		}
	}
	if want := "docker network create -d macvlan --subnet 10.0.0.0/24 --gateway 10.0.0.1 -o parent=eth0 lan"; command != want {
		t.Errorf("network command = %q, want %q", command, want)
	}
	if !strings.Contains(pv.Source, "<Name>lan</Name>") || !strings.Contains(pv.Compose.Or(""), "lan") {
		t.Errorf("preview lacks its source or Compose: %+v", pv)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/templates/stack")
	pv = apiv1.MigrationTemplatePreview{}
	if err := json.Unmarshal(body, &pv); status != http.StatusOK || err != nil || pv.Kind != apiv1.MigrationTemplatePreviewKindComposeProject || len(pv.Privileges) == 0 {
		t.Errorf("GET /migrate/templates/stack = %d %s (%v)", status, body, err)
	}
	if status, body = w.do(t, http.MethodGet, "/migrate/templates/nothing.xml"); status != http.StatusNotFound || !strings.Contains(string(body), "template_not_found") {
		t.Errorf("GET /migrate/templates/nothing.xml = %d %s", status, body)
	}
	zips, _ := filepath.Glob(filepath.Join(w.root, "migrate", "upload-*.zip"))
	if len(zips) != 1 {
		t.Fatalf("the session keeps %v, want the one zip", zips)
	}
	if err := os.Remove(zips[0]); err != nil {
		t.Fatal(err)
	}
	if status, body = w.do(t, http.MethodGet, "/migrate/templates/my-lan.xml"); status != http.StatusConflict || !strings.Contains(string(body), "template_source_unavailable") {
		t.Errorf("GET /migrate/templates/my-lan.xml without the zip = %d %s, want 409 template_source_unavailable", status, body)
	}
	if status, _ = w.do(t, http.MethodGet, "/migrate/templates"); status != http.StatusOK {
		t.Errorf("GET /migrate/templates without the zip = %d, want the kept facts", status)
	}
	if _, err := os.Stat(filepath.Join(w.root, "stacks")); err == nil {
		t.Error("a scan wrote a stacks directory: nothing is created from a preview")
	}
}
