package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

type containersDaemon struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string]string
	check    apiv1.MigrationContainerCheck
	created  apiv1.MigrationStacksCreated
	jobID    uuid.UUID
	jobState apiv1.JobStatus
}

func (d *containersDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func (d *containersDaemon) body(req string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.bodies[req]
}

func startContainersDaemon(t *testing.T) (string, *containersDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &containersDaemon{bodies: map[string]string{}, jobID: uuid.New(), jobState: apiv1.JobStatusSucceeded,
		check: apiv1.MigrationContainerCheck{Stack: "plain", Running: true, AllOk: true, Paths: []apiv1.MigrationDataPath{{Container: "plain", Path: "/mnt/user/appdata/plain", Destination: "/config", Status: apiv1.MigrationDataPathStatusOk}}}}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := r.Method + " " + r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.requests = append(d.requests, req)
		d.bodies[req] = string(raw)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out []byte
		var err error
		switch req {
		case "GET /api/v1/migrate/containers":
			out, err = (&apiv1.MigrationContainers{
				ParityInitialized: true,
				Templates: []apiv1.MigrationContainerTemplate{
					{Name: "notes", File: "my-notes.xml", Class: apiv1.MigrationTemplateClassAutostart, Status: apiv1.MigrationTemplateStatusWarnings, WarningCount: 1, AutostartPosition: apiv1.NewOptInt(1), Creatable: true, Preselected: true, Stack: apiv1.NewOptString("notes")},
					{Name: "plain", File: "my-plain.xml", Class: apiv1.MigrationTemplateClassAutostart, Status: apiv1.MigrationTemplateStatusClean, AutostartPosition: apiv1.NewOptInt(2), AutostartWaitSeconds: apiv1.NewOptInt(30), Creatable: true, Preselected: true, Stack: apiv1.NewOptString("plain")},
					{Name: "lan", File: "my-lan.xml", Class: apiv1.MigrationTemplateClassRunning, Status: apiv1.MigrationTemplateStatusClean, Creatable: true, Stack: apiv1.NewOptString("lan")},
				},
				ComposeProjects: []apiv1.MigrationContainerProject{{Name: "stack", Containers: []string{"stack-web"}, Status: apiv1.MigrationTemplateStatusPreviewed, Creatable: true, Stack: apiv1.NewOptString("stack")}},
				ByHand:          []apiv1.MigrationByHandContainer{{Name: "handmade", Image: apiv1.NewOptString("fixture/handmade:latest")}},
				Stacks: []apiv1.MigrationContainerStack{
					{Name: "plain", Source: "my-plain.xml", Kind: apiv1.MigrationContainerStackKindTemplate, State: apiv1.MigrationContainerStackStateStarted, WaitSeconds: apiv1.NewOptInt(30), Awaiting: true, Checked: true},
				},
				Awaiting: apiv1.NewOptString("plain"),
			}).MarshalJSON()
		case "GET /api/v1/migrate/templates/my-notes.xml", "GET /api/v1/migrate/templates/my-plain.xml":
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/migrate/templates/")
			pv := apiv1.MigrationTemplatePreview{
				Kind: apiv1.MigrationTemplatePreviewKindTemplate, Name: name, Status: apiv1.MigrationTemplateStatusClean,
				Source: "<Container><Name>x</Name></Container>", Compose: apiv1.NewOptString("services:\n  x:\n    image: x\n"),
				Warnings:   []apiv1.ConversionWarning{{Class: apiv1.ConversionWarningClassWritableLayer, Message: "The source container may hold configuration this Compose file does not reproduce."}},
				Privileges: []apiv1.TemplatePrivilege{},
			}
			if name == "my-notes.xml" {
				pv.Status = apiv1.MigrationTemplateStatusWarnings
			}
			out, err = pv.MarshalJSON()
		case "POST /api/v1/migrate/containers":
			out, err = d.created.MarshalJSON()
		case "POST /api/v1/migrate/containers/plain/start":
			out, err = (&apiv1.Job{ID: d.jobID, Type: apiv1.JobTypeStackStart, Class: apiv1.JobClassService, Status: apiv1.JobStatusQueued, CreatedAt: time.Now()}).MarshalJSON()
		case "GET /api/v1/jobs/" + d.jobID.String():
			out, err = (&apiv1.Job{ID: d.jobID, Type: apiv1.JobTypeStackStart, Class: apiv1.JobClassService, Status: d.jobState, CreatedAt: time.Now()}).MarshalJSON()
		case "POST /api/v1/migrate/containers/plain/check":
			out, err = d.check.MarshalJSON()
		case "POST /api/v1/migrate/containers/plain/confirm":
			out, err = (&apiv1.MigrationContainerStack{Name: "plain", Source: "my-plain.xml", Kind: apiv1.MigrationContainerStackKindTemplate, State: apiv1.MigrationContainerStackStateConfirmed}).MarshalJSON()
		default:
			w.WriteHeader(http.StatusNotFound)
			out, err = (&apiv1.Error{Code: "not_found", Message: "no such thing"}).MarshalJSON()
		}
		if err != nil {
			panic(err)
		}
		_, _ = w.Write(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, d
}

func TestMigrateContainersListsTheOfferTheStacksAndWhatIsNext(t *testing.T) {
	sock, _ := startContainersDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "containers")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"my-notes.xml", "autostart (#1)", "yes", "my-lan.xml", "running", "Compose Manager project",
		"No template, recreate by hand", "handmade  fixture/handmade:latest",
		"Created stacks", "plain", "started (awaiting confirmation)", "30s",
		"plain is started and not confirmed",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
}

// Nothing is created without --yes, whatever else is given, and the Compose and
// the warnings of everything selected are shown first.
func TestMigrateContainersCreateShowsEveryWarningAndCreatesNothingWithoutYes(t *testing.T) {
	sock, d := startContainersDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "containers", "create")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("create without --yes = %v, want a refusal that asks for --yes", err)
	}
	for _, want := range []string{"my-notes.xml", "my-plain.xml", "[writable_layer]", "Generated Compose"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q:\n%s", want, printed)
		}
	}
	for _, req := range d.seen() {
		if req == "POST /api/v1/migrate/containers" {
			t.Fatalf("a create without --yes sent %v", d.seen())
		}
	}
}

func TestMigrateContainersCreateSendsOnlyTheAcknowledgementsTheUserNames(t *testing.T) {
	sock, d := startContainersDaemon(t)
	d.created = apiv1.MigrationStacksCreated{Results: []apiv1.MigrationStackResult{
		{Name: "my-notes.xml", Stack: "notes", Status: apiv1.MigrationStackResultStatusCreated},
		{Name: "my-plain.xml", Stack: "plain", Status: apiv1.MigrationStackResultStatusCreated},
	}}
	if _, err := runBackupCLI(t, sock, "migrate", "containers", "create", "--yes", "--acknowledge", "my-lan.xml"); err == nil || !strings.Contains(err.Error(), "not being created") {
		t.Fatalf("--acknowledge of something not selected = %v, want a refusal", err)
	}

	printed, err := runBackupCLI(t, sock, "migrate", "containers", "create", "--yes")
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	body := d.body("POST /api/v1/migrate/containers")
	if !strings.Contains(body, `"my-notes.xml"`) || !strings.Contains(body, `"my-plain.xml"`) || strings.Contains(body, "acknowledged") {
		t.Errorf("body = %s, want the two pre-selected templates and no acknowledgement the user did not give", body)
	}
	if !strings.Contains(printed, "created stack notes from my-notes.xml (stopped)") {
		t.Errorf("output = %s", printed)
	}

	if _, err := runBackupCLI(t, sock, "migrate", "containers", "create", "my-notes.xml", "--yes", "--acknowledge", "my-notes.xml"); err != nil {
		t.Fatalf("create = %v", err)
	}
	if body := d.body("POST /api/v1/migrate/containers"); !strings.Contains(body, `"acknowledged":true`) || strings.Contains(body, "my-plain.xml") {
		t.Errorf("body = %s, want my-notes.xml alone, acknowledged", body)
	}
}

func TestMigrateContainersCreateFailsWhenAStackWasNotCreatedAndSaysWhich(t *testing.T) {
	sock, d := startContainersDaemon(t)
	d.created = apiv1.MigrationStacksCreated{Results: []apiv1.MigrationStackResult{
		{Name: "my-notes.xml", Stack: "notes", Status: apiv1.MigrationStackResultStatusCreated},
		{Name: "my-plain.xml", Stack: "plain", Status: apiv1.MigrationStackResultStatusFailed, Error: apiv1.NewOptError(apiv1.Error{Code: "stack_exists", Message: "a stack named \"plain\" already exists"})},
	}}
	printed, err := runBackupCLI(t, sock, "migrate", "containers", "create", "--yes")
	if err == nil || !strings.Contains(err.Error(), "1 of 2 stacks could not be created") {
		t.Fatalf("create = %v, want a failure for the stack that was not created", err)
	}
	if !strings.Contains(printed, "stack plain from my-plain.xml was NOT created") || !strings.Contains(printed, "stack_exists") || !strings.Contains(printed, "created stack notes") {
		t.Errorf("output = %s", printed)
	}
}

func TestMigrateContainersStartWaitsForTheJobAndSuggestsTheWait(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	sock, d := startContainersDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "containers", "start", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(printed, "Stack plain was started.") || !strings.Contains(printed, "30 seconds") || !strings.Contains(printed, "migrate containers check plain") {
		t.Errorf("output = %s", printed)
	}

	d.jobState = apiv1.JobStatusFailed
	if _, err := runBackupCLI(t, sock, "migrate", "containers", "start", "plain"); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("start whose job failed = %v, want a failure", err)
	}
}

func TestMigrateContainersCheckExitsNonZeroWhenAPathIsNotWhole(t *testing.T) {
	sock, d := startContainersDaemon(t)
	printed, err := runBackupCLI(t, sock, "migrate", "containers", "check", "plain")
	if err != nil || !strings.Contains(printed, "/mnt/user/appdata/plain") || !strings.Contains(printed, "ok") || !strings.Contains(printed, "confirm plain") {
		t.Fatalf("check = %q, %v", printed, err)
	}
	d.check = apiv1.MigrationContainerCheck{Stack: "plain", Running: true, AllOk: false, Paths: []apiv1.MigrationDataPath{
		{Container: "plain", Path: "/mnt/user/appdata/plain", Destination: "/config", Status: apiv1.MigrationDataPathStatusEmpty},
		{Container: "plain", Path: "/mnt/cache/x", Destination: "/x", Status: apiv1.MigrationDataPathStatusUnreadable, Error: apiv1.NewOptString("permission denied")},
	}}
	printed, err = runBackupCLI(t, sock, "migrate", "containers", "check", "plain")
	if err == nil || !strings.Contains(printed, "empty") || !strings.Contains(printed, "unreadable (permission denied)") {
		t.Fatalf("check = %q, %v, want the problems and a non-zero exit", printed, err)
	}
}

// A failed data check is accepted only when the user says so: the command never
// sends that on its own.
func TestMigrateContainersConfirmNeverAcceptsAFailedCheckOnItsOwn(t *testing.T) {
	sock, d := startContainersDaemon(t)
	if _, err := runBackupCLI(t, sock, "migrate", "containers", "confirm", "plain"); err != nil {
		t.Fatal(err)
	}
	if body := d.body("POST /api/v1/migrate/containers/plain/confirm"); strings.Contains(body, "acceptFailedCheck") {
		t.Errorf("body = %q, want no acceptance the user did not give", body)
	}
	printed, err := runBackupCLI(t, sock, "migrate", "containers", "confirm", "plain", "--accept-failed-check")
	if err != nil || !strings.Contains(printed, "Stack plain is confirmed.") {
		t.Fatalf("confirm = %q, %v", printed, err)
	}
	if body := d.body("POST /api/v1/migrate/containers/plain/confirm"); !strings.Contains(body, `"acceptFailedCheck":true`) {
		t.Errorf("body = %q, want the acceptance the user gave", body)
	}
}

func TestRootCmdHasMigrateContainersSubcommands(t *testing.T) {
	root := rootCmd()
	for _, sub := range []string{"create", "start", "check", "confirm"} {
		cmd, _, err := root.Find([]string{"migrate", "containers", sub})
		if err != nil || cmd.Name() != sub {
			t.Errorf("find migrate containers %s: %v", sub, err)
		}
	}
}
