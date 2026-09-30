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

// configBackupDaemon is a stand-in daemon on a Unix socket: it answers
// POST /config/backup with a queued job and GET /jobs/{id} with each of
// statuses in turn, the last one repeating.
type configBackupDaemon struct {
	sock string
	id   uuid.UUID

	mu       sync.Mutex
	requests []string
}

func startConfigBackupDaemon(t *testing.T, statuses []apiv1.Job, refuse *apiv1.Error) *configBackupDaemon {
	t.Helper()
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &configBackupDaemon{sock: filepath.Join(dir, "d.sock"), id: uuid.New()}
	ln, err := net.Listen("unix", d.sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	polls := 0
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.requests = append(d.requests, r.Method+" "+r.URL.Path)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var out []byte
		var err error
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/config/backup":
			if refuse != nil {
				w.WriteHeader(http.StatusConflict)
				out, err = refuse.MarshalJSON()
				break
			}
			j := apiv1.Job{ID: d.id, Type: apiv1.JobTypeConfigBackup, Class: apiv1.JobClassService, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
			out, err = j.MarshalJSON()
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/jobs/"+d.id.String():
			d.mu.Lock()
			i := polls
			if i >= len(statuses) {
				i = len(statuses) - 1
			}
			polls++
			d.mu.Unlock()
			j := statuses[i]
			j.ID, j.Type, j.Class, j.CreatedAt = d.id, apiv1.JobTypeConfigBackup, apiv1.JobClassService, time.Now().UTC()
			out, err = j.MarshalJSON()
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err != nil {
			t.Errorf("encoding the response: %v", err)
		}
		_, _ = w.Write(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return d
}

func (d *configBackupDaemon) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func runBackupCLI(t *testing.T, sock string, args ...string) (string, error) {
	t.Helper()
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	root := rootCmd()
	root.SetArgs(append([]string{"--socket", sock}, args...))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	runErr := root.Execute()
	os.Stdout = stdout
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	jsonOutput = false
	return string(printed), runErr
}

func TestRootCmdHasBackupRunConfig(t *testing.T) {
	root := rootCmd()
	cmd, _, err := root.Find([]string{"backup", "run", "config"})
	if err != nil || cmd.Name() != "config" {
		t.Fatalf("find backup run config: %v", err)
	}
	if cmd.Flags().Lookup("wait") == nil {
		t.Fatal("backup run config has no --wait flag")
	}
}

func TestBackupRunConfigQueuesTheJobAndPrintsItsID(t *testing.T) {
	d := startConfigBackupDaemon(t, nil, nil)

	printed, err := runBackupCLI(t, d.sock, "backup", "run", "config")
	if err != nil {
		t.Fatalf("backup run config: %v", err)
	}
	if strings.TrimSpace(printed) != d.id.String() {
		t.Fatalf("output = %q, want the job id %s alone", printed, d.id)
	}
	if got := d.seen(); len(got) != 1 || got[0] != "POST /api/v1/config/backup" {
		t.Fatalf("requests = %v, want exactly POST /api/v1/config/backup and no waiting", got)
	}
}

func TestBackupRunConfigJSONPrintsTheJob(t *testing.T) {
	d := startConfigBackupDaemon(t, nil, nil)

	printed, err := runBackupCLI(t, d.sock, "--json", "backup", "run", "config")
	if err != nil {
		t.Fatalf("backup run config --json: %v", err)
	}
	if !strings.Contains(printed, d.id.String()) || !strings.Contains(printed, `"config_backup"`) {
		t.Fatalf("output = %q, want the job as JSON", printed)
	}
}

func TestBackupRunConfigWaitPollsUntilTheJobSucceeds(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startConfigBackupDaemon(t, []apiv1.Job{
		{Status: apiv1.JobStatusQueued},
		{Status: apiv1.JobStatusRunning},
		{Status: apiv1.JobStatusSucceeded},
	}, nil)

	printed, err := runBackupCLI(t, d.sock, "backup", "run", "config", "--wait")
	if err != nil {
		t.Fatalf("backup run config --wait: %v", err)
	}
	if !strings.Contains(printed, d.id.String()) || !strings.Contains(printed, "succeeded") {
		t.Fatalf("output = %q, want the job id and that it succeeded", printed)
	}
	got := d.seen()
	if len(got) != 4 || got[0] != "POST /api/v1/config/backup" || got[3] != "GET /api/v1/jobs/"+d.id.String() {
		t.Fatalf("requests = %v, want the POST then three polls up to the terminal status", got)
	}
}

func TestBackupRunConfigWaitFailsWithTheJobsError(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	failed := apiv1.Job{Status: apiv1.JobStatusFailed}
	failed.SetError(apiv1.NewOptNilError(apiv1.Error{Code: "job_failed", Message: `writing destination "boot": no space left`}))
	d := startConfigBackupDaemon(t, []apiv1.Job{{Status: apiv1.JobStatusRunning}, failed}, nil)

	_, err := runBackupCLI(t, d.sock, "backup", "run", "config", "--wait")
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("backup run config --wait over a failed job = %v, want a command failure carrying the job's error", err)
	}
}

func TestBackupRunConfigWaitFailsOnACancelledJob(t *testing.T) {
	old := backupWaitInterval
	backupWaitInterval = time.Millisecond
	t.Cleanup(func() { backupWaitInterval = old })
	d := startConfigBackupDaemon(t, []apiv1.Job{{Status: apiv1.JobStatusCancelled}}, nil)

	if _, err := runBackupCLI(t, d.sock, "backup", "run", "config", "--wait"); err == nil {
		t.Fatal("backup run config --wait over a cancelled job succeeded")
	}
}

func TestBackupRunConfigRefusalIsACommandFailure(t *testing.T) {
	d := startConfigBackupDaemon(t, nil, &apiv1.Error{Code: "backup_no_destination", Message: "no backup destination is enabled"})

	_, err := runBackupCLI(t, d.sock, "backup", "run", "config", "--wait")
	if err == nil || !strings.Contains(err.Error(), "backup_no_destination") {
		t.Fatalf("backup run config against a refusal = %v, want a command failure naming the code", err)
	}
	if got := d.seen(); len(got) != 1 {
		t.Fatalf("requests = %v, want no polling after a refusal", got)
	}
}
