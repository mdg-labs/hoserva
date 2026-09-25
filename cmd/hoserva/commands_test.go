package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

func TestRootCmdHasUpdateRollbackAndReboot(t *testing.T) {
	root := rootCmd()
	for _, name := range []string{"update", "rollback", "reboot"} {
		if _, _, err := root.Find([]string{name}); err != nil {
			t.Fatalf("find %s: %v", name, err)
		}
	}
}

func TestRootCmdHasNetwork(t *testing.T) {
	root := rootCmd()
	network, _, err := root.Find([]string{"network"})
	if err != nil {
		t.Fatalf("find network: %v", err)
	}
	for _, name := range []string{"apply", "confirm"} {
		if _, _, err := network.Find([]string{name}); err != nil {
			t.Fatalf("find network %s: %v", name, err)
		}
	}
}

func TestRootCmdHasShare(t *testing.T) {
	root := rootCmd()
	share, _, err := root.Find([]string{"share"})
	if err != nil {
		t.Fatalf("find share: %v", err)
	}
	for _, name := range []string{"list", "get", "create", "rm", "rm-data", "browse"} {
		if _, _, err := share.Find([]string{name}); err != nil {
			t.Fatalf("find share %s: %v", name, err)
		}
	}
}

func TestShareRmRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"share", "rm", "media"})
}

func TestRootCmdHasArrayStopAndStart(t *testing.T) {
	root := rootCmd()
	array, _, err := root.Find([]string{"array"})
	if err != nil {
		t.Fatalf("find array: %v", err)
	}
	for _, name := range []string{"stop", "start"} {
		if _, _, err := array.Find([]string{name}); err != nil {
			t.Fatalf("find array %s: %v", name, err)
		}
	}
}

func TestRootCmdHasMoverRun(t *testing.T) {
	root := rootCmd()
	mover, _, err := root.Find([]string{"mover"})
	if err != nil {
		t.Fatalf("find mover: %v", err)
	}
	if _, _, err := mover.Find([]string{"run"}); err != nil {
		t.Fatalf("find mover run: %v", err)
	}
}

func TestRebootRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"reboot"})
}

func TestArrayStopRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"array", "stop"})
}

func TestUpdateRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"update"})
}

func TestRollbackRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"rollback"})
}

func TestUpdateCheckDoesNotRequireConfirm(t *testing.T) {
	root := rootCmd()
	update, _, err := root.Find([]string{"update"})
	if err != nil {
		t.Fatalf("find update: %v", err)
	}
	if update.Flags().Lookup("check") == nil {
		t.Fatal("update has no --check flag")
	}
}

func TestRootCmdHasDiskAddAndReplace(t *testing.T) {
	root := rootCmd()
	disk, _, err := root.Find([]string{"disk"})
	if err != nil {
		t.Fatalf("find disk: %v", err)
	}
	for _, name := range []string{"add", "replace", "upgrade", "remove"} {
		if _, _, err := disk.Find([]string{name}); err != nil {
			t.Fatalf("find disk %s: %v", name, err)
		}
	}
	if _, _, err := root.Find([]string{"disk", "add", "plan"}); err != nil {
		t.Fatalf("find disk add plan: %v", err)
	}
	if _, _, err := root.Find([]string{"disk", "replace", "plan"}); err != nil {
		t.Fatalf("find disk replace plan: %v", err)
	}
	if _, _, err := root.Find([]string{"disk", "upgrade", "plan"}); err != nil {
		t.Fatalf("find disk upgrade plan: %v", err)
	}
}

func TestDiskAddRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "add", "--device", "/dev/sdx"})
}

func TestDiskReplaceRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "replace", "--mountpoint", "/mnt/disk2", "--device", "/dev/sdx"})
}

func TestDiskUpgradeRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "upgrade", "--mountpoint", "/mnt/disk2", "--device", "/dev/sdx"})
}

func TestDiskRemoveFinishRequiresConfirm(t *testing.T) {
	assertRequiresConfirm(t, []string{"disk", "remove", "finish", "--mountpoint", "/mnt/disk2"})
}

// TestDiskRemoveFinishSendsTheRequestAndPrintsTheJob drives `hoserva disk
// remove finish` against a stand-in daemon on a Unix socket: the request
// reaches finishDiskRemoval with the slot and phrase as typed, and the
// queued job's id is printed.
func TestDiskRemoveFinishSendsTheRequestAndPrintsTheJob(t *testing.T) {
	dir, err := os.MkdirTemp("", "hsv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	jobID := uuid.New()
	var gotPath string
	var gotBody apiv1.FinishDiskRemovalRequest
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if err := gotBody.UnmarshalJSON(body); err != nil {
			t.Errorf("decoding the request body %q: %v", body, err)
		}
		j := apiv1.Job{ID: jobID, Type: apiv1.JobTypeDiskRemove, Class: apiv1.JobClassTopology, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()}
		out, err := j.MarshalJSON()
		if err != nil {
			t.Errorf("encoding the job: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	root := rootCmd()
	root.SetArgs([]string{"--socket", sock, "--json", "disk", "remove", "finish", "--mountpoint", "/mnt/disk2", "--confirm", "REMOVE /mnt/disk2"})
	runErr := root.Execute()
	os.Stdout = stdout
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	jsonOutput = false
	if runErr != nil {
		t.Fatalf("disk remove finish: %v", runErr)
	}
	if gotPath != "POST /api/v1/disks/array/remove/finish" {
		t.Fatalf("request = %q, want POST /api/v1/disks/array/remove/finish", gotPath)
	}
	if gotBody.Mountpoint != "/mnt/disk2" || gotBody.Confirmation != "REMOVE /mnt/disk2" {
		t.Fatalf("request body = %+v, want the slot and phrase as typed", gotBody)
	}
	if !strings.Contains(string(printed), jobID.String()) {
		t.Fatalf("output %q does not print the job id %s", printed, jobID)
	}
}

func assertRequiresConfirm(t *testing.T, args []string) {
	t.Helper()
	root := rootCmd()
	cmd, _, err := root.Find(args)
	if err != nil {
		t.Fatalf("find %s: %v", strings.Join(args, " "), err)
	}
	if cmd.Flags().Lookup("confirm") == nil {
		t.Fatalf("%s has no --confirm flag", strings.Join(args, " "))
	}

	var errBuf bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("%s without --confirm: %v", strings.Join(args, " "), err)
	}
}
