package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mdg-labs/hoserva/internal/container"
	"github.com/mdg-labs/hoserva/internal/disk"
	"github.com/mdg-labs/hoserva/internal/job"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/internal/store"
)

type wiringCipher struct{}

func (wiringCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte{}, p...), nil }
func (wiringCipher) Decrypt(c []byte) ([]byte, error) { return append([]byte{}, c...), nil }

func hasCode(body []byte, code string) bool { return strings.Contains(string(body), `"`+code+`"`) }

// Phase D's operations are served by the daemon's own server over the stack
// layer main.go wires: refused until the point of no return has finished, then
// the pre-selection, the stacks created stopped (a template with warnings only
// when acknowledged), one start at a time through the stack_start job, and the
// data check and confirmation between them.
func TestMigrationContainersWiring_AreReachableOverHTTPAndGatedOnTheParityInitialisation(t *testing.T) {
	pw := wireParity(t)
	w := pw.w
	ctx := context.Background()
	compose := container.NewFakeRunner()
	wireStacks(w.handler, w.registry, store.NewStackStore(w.db), wiringCipher{}, compose, w.root, w.apps, arrayActionAdmit(w.scheduler))
	if err := wireMigrationContainers(w.handler, w.arrays); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	w.handler.Migration.DataRoots = []string{data}

	refused := func(when, method, path, body string) {
		t.Helper()
		status, got := w.doBody(t, method, path, body)
		if status != http.StatusConflict || !hasCode(got, "parity_not_initialized") {
			t.Fatalf("%s: %s %s = %d %s, want 409 parity_not_initialized", when, method, path, status, got)
		}
	}
	const create = `{"items":[{"name":"my-plain.xml"}]}`

	status, body := w.do(t, http.MethodGet, "/migrate/containers")
	if status != http.StatusNotFound || !hasCode(body, "no_migration_report") {
		t.Fatalf("GET /migrate/containers before a scan = %d %s, want 404 no_migration_report", status, body)
	}

	ini := "[\"parity\"]\nidx=\"0\"\nid=\"M_PARSERIAL\"\nsize=\"1000\"\nstatus=\"DISK_OK\"\ntype=\"Parity\"\n" +
		"[\"disk1\"]\nidx=\"1\"\nid=\"M_DATASERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n" +
		"[\"disk2\"]\nidx=\"2\"\nid=\"M_DATA2SERIAL\"\nsize=\"900\"\nstatus=\"DISK_OK\"\ntype=\"Data\"\nfsType=\"xfs\"\n"
	inspect := func(name, image string, labels string) string {
		return `{"Name":"/` + name + `","State":{"Status":"running","Running":true},"Config":{"Image":"` + image + `","Labels":` + labels + `}}`
	}
	dockerMan := `{"net.unraid.docker.managed":"dockerman"}`
	tmpl := func(name, network string) string {
		return `<Container version="2"><Name>` + name + `</Name><Repository>fixture/` + name + `:1</Repository><Network>` + network + `</Network>` +
			`<Config Name="Web" Target="80" Default="8080" Mode="tcp" Type="Port">8080</Config></Container>`
	}
	zipData := flashBackupZipWith(t, "7.3.2", map[string]string{
		"config/hoserva/disks.ini":                             ini,
		"config/hoserva/capture.json":                          `{"unraid_version":"7.3.2","captured_at":"2026-10-02T17:56:07Z","boot":{"mode":"usb"},"docker":{"state":"running","directory_location":"array","writable_layers":[]},"libvirt_img_location":"none"}`,
		"config/hoserva/containers.json":                       "[" + inspect("plain", "fixture/plain:1", dockerMan) + "," + inspect("lan", "fixture/lan:1", dockerMan) + "," + inspect("handmade", "fixture/handmade:latest", "{}") + "]",
		"config/hoserva/autostart":                             "plain 30\n",
		"config/hoserva/networks.json":                         `[{"Name":"lan","Driver":"macvlan","Scope":"local","IPAM":{"Config":[{"Subnet":"10.0.0.0/24","Gateway":"10.0.0.1"}]},"Options":{"parent":"eth0"}}]`,
		"config/plugins/dockerMan/templates-user/my-plain.xml": tmpl("plain", "bridge"),
		"config/plugins/dockerMan/templates-user/my-lan.xml":   tmpl("lan", "lan"),
	})
	status, body = w.uploadScan(t, zipData, false)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/scan = %d %s", status, body)
	}
	var q struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &q)
	if done := w.awaitJobByID(t, q.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("scan job = %s %s", done.Status, done.ErrorMessage)
	}

	refused("before the import", http.MethodPost, "/migrate/containers", create)
	refused("before the import", http.MethodPost, "/migrate/containers/plain/start", "")
	refused("before the import", http.MethodPost, "/migrate/containers/plain/check", "")
	refused("before the import", http.MethodPost, "/migrate/containers/plain/confirm", "")
	status, body = w.doBody(t, http.MethodPost, "/migrate/import", parityImportBody)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/import = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &q)
	if done := w.awaitJobByID(t, q.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("import job = %s %s", done.Status, done.ErrorMessage)
	}
	refused("with the import pending", http.MethodPost, "/migrate/containers", create)
	refused("with the import pending", http.MethodPost, "/migrate/containers/plain/start", "")
	var list struct {
		ParityInitialized bool `json:"parityInitialized"`
		Templates         []struct {
			File        string `json:"file"`
			Class       string `json:"class"`
			Preselected bool   `json:"preselected"`
			Stack       string `json:"stack"`
		} `json:"templates"`
		ByHand []struct {
			Name  string `json:"name"`
			Image string `json:"image"`
		} `json:"byHand"`
		Stacks []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"stacks"`
		Awaiting string `json:"awaiting"`
		Next     string `json:"next"`
	}
	status, body = w.do(t, http.MethodGet, "/migrate/containers")
	if err := json.Unmarshal(body, &list); status != http.StatusOK || err != nil || list.ParityInitialized {
		t.Fatalf("GET /migrate/containers with the import pending = %d %s (%v), want the offer with parityInitialized false", status, body, err)
	}
	if _, err := os.Stat(filepath.Join(w.root, "stacks")); err == nil {
		t.Fatal("a refused request wrote a stacks directory")
	}

	pw.setVerify(t, migrate.VerifyPassed)
	status, body = w.doBody(t, http.MethodPost, "/migrate/initialize-parity", `{"confirmation":"ERASE /dev/sdb"}`)
	if status != http.StatusOK {
		t.Fatalf("POST /migrate/initialize-parity = %d %s", status, body)
	}
	_ = json.Unmarshal(body, &q)
	if done := w.awaitJobByID(t, q.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("migration_parity job = %s %s", done.Status, done.ErrorMessage)
	}

	status, body = w.do(t, http.MethodGet, "/migrate/containers")
	list.Templates = nil
	if err := json.Unmarshal(body, &list); status != http.StatusOK || err != nil || !list.ParityInitialized {
		t.Fatalf("GET /migrate/containers = %d %s (%v), want parityInitialized", status, body, err)
	}
	var pre []string
	for _, it := range list.Templates {
		if it.Preselected {
			pre = append(pre, it.File)
		}
	}
	if len(pre) != 1 || pre[0] != "my-plain.xml" || len(list.ByHand) != 1 || list.ByHand[0].Name != "handmade" || list.ByHand[0].Image != "fixture/handmade:latest" {
		t.Fatalf("offer = %s, want my-plain.xml pre-selected and handmade listed by hand with its image", body)
	}

	status, body = w.doBody(t, http.MethodPost, "/migrate/containers", `{"items":[{"name":"my-plain.xml"},{"name":"my-lan.xml"}]}`)
	if status != http.StatusConflict || !hasCode(body, "warnings_not_acknowledged") {
		t.Fatalf("create with unacknowledged warnings = %d %s, want 409 warnings_not_acknowledged", status, body)
	}
	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers", `{"items":[]}`); status != http.StatusBadRequest {
		t.Errorf("create with no items = %d %s, want 400", status, body)
	}
	status, body = w.doBody(t, http.MethodPost, "/migrate/containers", `{"items":[{"name":"my-plain.xml"},{"name":"my-lan.xml","acknowledged":true}]}`)
	var created struct {
		Results []struct {
			Name   string `json:"name"`
			Stack  string `json:"stack"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &created); status != http.StatusOK || err != nil || len(created.Results) != 2 || created.Results[0].Status != "created" || created.Results[1].Status != "created" {
		t.Fatalf("create = %d %s (%v), want both created", status, body, err)
	}
	for _, c := range compose.Calls() {
		for _, a := range c.Args {
			if a == "up" {
				t.Fatalf("creating the stacks ran compose up: %v", c.Args)
			}
		}
	}
	if status, body = w.do(t, http.MethodGet, "/stacks"); status != http.StatusOK || !strings.Contains(string(body), `"plain"`) || !strings.Contains(string(body), `"lan"`) {
		t.Fatalf("GET /stacks = %d %s, want both stacks", status, body)
	}

	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers/nothing/start", ""); status != http.StatusNotFound || !hasCode(body, "migrated_stack_not_found") {
		t.Errorf("start of a stack the migration did not create = %d %s, want 404 migrated_stack_not_found", status, body)
	}
	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers/plain/check", ""); status != http.StatusConflict || !hasCode(body, "container_not_started") {
		t.Errorf("check before the start = %d %s, want 409 container_not_started", status, body)
	}
	status, body = w.doBody(t, http.MethodPost, "/migrate/containers/plain/start", "")
	var started struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &started); status != http.StatusOK || err != nil || started.Type != "stack_start" {
		t.Fatalf("start = %d %s (%v), want the stack_start job", status, body, err)
	}
	if done := w.awaitJobByID(t, started.ID); done.Status != job.StatusSucceeded {
		t.Fatalf("stack_start job = %s %s", done.Status, done.ErrorMessage)
	}

	appdata := filepath.Join(data, "appdata", "plain")
	if err := os.MkdirAll(appdata, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appdata, "config.xml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.fake.AddContainer(container.Container{
		ID: "plain-1", Name: "plain", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "plain", "com.docker.compose.project.working_dir": filepath.Join(w.root, "stacks", "plain")},
		Mounts: []container.Mount{{Source: appdata, Destination: "/config", ReadWrite: true}},
	})
	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers/lan/start", ""); status != http.StatusConflict || !hasCode(body, "container_unconfirmed") {
		t.Fatalf("start of lan while plain is unconfirmed = %d %s, want 409 container_unconfirmed", status, body)
	}
	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers/plain/confirm", ""); status != http.StatusConflict || !hasCode(body, "data_check_required") {
		t.Errorf("confirm before the data check = %d %s, want 409 data_check_required", status, body)
	}
	status, body = w.doBody(t, http.MethodPost, "/migrate/containers/plain/check", "")
	var check struct {
		AllOK bool `json:"allOk"`
		Paths []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(body, &check); status != http.StatusOK || err != nil || !check.AllOK || len(check.Paths) != 1 || check.Paths[0].Path != appdata || check.Paths[0].Status != "ok" {
		t.Fatalf("check = %d %s (%v), want its one path ok", status, body, err)
	}
	if status, body = w.doBody(t, http.MethodPost, "/migrate/containers/plain/confirm", ""); status != http.StatusOK || !strings.Contains(string(body), `"confirmed"`) {
		t.Fatalf("confirm = %d %s", status, body)
	}
	status, body = w.doBody(t, http.MethodPost, "/migrate/containers/lan/start", "")
	if err := json.Unmarshal(body, &started); status != http.StatusOK || err != nil {
		t.Fatalf("start of lan once plain is confirmed = %d %s", status, body)
	}
	w.awaitJobByID(t, started.ID)

	if unfinished, err := w.arrays.MigrationUnfinished(ctx); err != nil || unfinished {
		t.Errorf("MigrationUnfinished = %v, %v", unfinished, err)
	}
}

func TestWireMigrationContainers_FailsWithoutTheSessionTheStackLayerOrTheArrayStore(t *testing.T) {
	w := newContainersWiringHarness(t)
	if err := wireMigrationContainers(w.handler, w.arrays); err == nil {
		t.Error("wireMigrationContainers succeeded with no migration session")
	}
	if err := wireMigration(context.Background(), w.handler, w.registry, disk.NewFakeProvider(), disk.NewFakeReadOnlyMounter(), disk.NewFakeRunner(), store.NewMigrationSessionStore(w.db), w.root); err != nil {
		t.Fatal(err)
	}
	if err := wireMigrationContainers(w.handler, w.arrays); err == nil {
		t.Error("wireMigrationContainers succeeded with no stack layer")
	}
	wireStacks(w.handler, w.registry, store.NewStackStore(w.db), wiringCipher{}, container.NewFakeRunner(), w.root, nil, nil)
	w.handler.ArrayStore = w.arrays
	if err := wireMigrationContainers(w.handler, store.NewArrayStore(w.db)); err == nil {
		t.Error("wireMigrationContainers succeeded with an array store that is not the handler's")
	}
	if err := wireMigrationContainers(w.handler, w.arrays); err != nil {
		t.Fatal(err)
	}
	if w.handler.Migration.Stacks == nil || w.handler.Migration.Initialized == nil {
		t.Error("wireMigrationContainers left the migrator without its stack layer or its parity check")
	}
}

// The parity check is the array's own record: no array is not initialised, a
// pending or unfinished migration is not, and an unreadable record is an error.
func TestMigrationInitialized_IsTheArraysOwnRecord(t *testing.T) {
	pw := wireParity(t)
	arrays := pw.w.arrays
	ctx := context.Background()
	check := migrationInitialized(arrays)

	if ok, err := check(ctx); err != nil || ok {
		t.Errorf("with no array = %v, %v, want false", ok, err)
	}
	pw.scanAndImport(t)
	if ok, err := check(ctx); err != nil || ok {
		t.Errorf("with the import pending = %v, %v, want false", ok, err)
	}
	if _, err := pw.w.db.Exec(`UPDATE array_settings SET migration_pending = 0, migration_recorded = '[]'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := check(ctx); err != nil || ok {
		t.Errorf("with the initialisation part-way = %v, %v, want false", ok, err)
	}
	if _, err := pw.w.db.Exec(`UPDATE array_settings SET migration_recorded = ''`); err != nil {
		t.Fatal(err)
	}
	if ok, err := check(ctx); err != nil || !ok {
		t.Errorf("with the initialisation finished = %v, %v, want true", ok, err)
	}
	if _, err := pw.w.db.Exec(`DROP TABLE array_disks`); err != nil {
		t.Fatal(err)
	}
	if ok, err := check(ctx); err == nil || ok {
		t.Errorf("with the record unreadable = %v, %v, want an error", ok, err)
	}
}

// main.go must give the migrator its stack layer and its parity check, after
// the stack layer exists: a handler field left unset would answer 501.
func TestMain_WiresTheMigrationContainers(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stacksAt, containersAt token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok {
			switch {
			case id.Name == "wireStacks":
				stacksAt = call.Pos()
			case id.Name == "wireMigrationContainers" && len(call.Args) == 2:
				a0, _ := call.Args[0].(*ast.Ident)
				a1, _ := call.Args[1].(*ast.Ident)
				if a0 != nil && a1 != nil && a0.Name == "handler" && a1.Name == "arrayStore" {
					containersAt = call.Pos()
				}
			}
		}
		return true
	})
	if containersAt == token.NoPos {
		t.Fatal("main.go does not call wireMigrationContainers(handler, arrayStore)")
	}
	if stacksAt == token.NoPos || stacksAt > containersAt {
		t.Error("main.go calls wireMigrationContainers before wireStacks: the migrator would find no stack layer")
	}
}
