package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// serveAppAPI starts a stand-in daemon on a Unix socket and returns the
// socket path.
func serveAppAPI(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
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
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// runAppCLI runs the whole `hoserva` command tree against sock and returns
// what it printed to stdout.
func runAppCLI(t *testing.T, sock string, args ...string) (string, error) {
	t.Helper()
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	var errBuf bytes.Buffer
	root := rootCmd()
	root.SetArgs(append([]string{"--socket", sock}, args...))
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	runErr := root.Execute()
	os.Stdout = stdout
	_ = w.Close()
	printed, _ := io.ReadAll(r)
	jsonOutput = false
	return string(printed), runErr
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v interface {
	MarshalJSON() ([]byte, error)
}) {
	t.Helper()
	out, err := v.MarshalJSON()
	if err != nil {
		t.Errorf("encoding the response: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func testApp(state apiv1.AppState) *apiv1.App {
	return &apiv1.App{
		ID: "abc123", Name: "plex", Health: apiv1.AppHealthNone,
		Image: "plexinc/pms-docker", Tag: "latest", State: state, Status: "Up 3 hours",
		Ports: []apiv1.AppPort{}, Mounts: []apiv1.AppMount{},
	}
}

func TestAppStateActionsCallTheirOperationAndPrintTheState(t *testing.T) {
	for _, tc := range []struct {
		verb  string
		state apiv1.AppState
	}{
		{"start", apiv1.AppStateRunning},
		{"stop", apiv1.AppStateExited},
		{"restart", apiv1.AppStateRunning},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			var gotRequest string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotRequest = r.Method + " " + r.URL.Path
				writeJSON(t, w, http.StatusOK, testApp(tc.state))
			})
			printed, err := runAppCLI(t, sock, "app", tc.verb, "plex")
			if err != nil {
				t.Fatalf("app %s: %v", tc.verb, err)
			}
			if want := "POST /api/v1/apps/plex/" + tc.verb; gotRequest != want {
				t.Fatalf("request = %q, want %q", gotRequest, want)
			}
			if !strings.Contains(printed, `"state": "`+string(tc.state)+`"`) {
				t.Fatalf("output %q does not print the returned state %q", printed, tc.state)
			}
		})
	}
}

func TestAppRecreateSubmitsTheJobAndPrintsItsID(t *testing.T) {
	jobID := uuid.New()
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, &apiv1.Job{ID: jobID, Type: apiv1.JobTypeContainerRecreate, Class: apiv1.JobClassService, Status: apiv1.JobStatusQueued, CreatedAt: time.Now().UTC()})
	})
	printed, err := runAppCLI(t, sock, "app", "recreate", "plex")
	if err != nil {
		t.Fatalf("app recreate: %v", err)
	}
	if gotRequest != "POST /api/v1/apps/plex/recreate" {
		t.Fatalf("request = %q, want POST /api/v1/apps/plex/recreate", gotRequest)
	}
	if !strings.Contains(printed, jobID.String()) {
		t.Fatalf("output %q does not print the job id %s", printed, jobID)
	}
}

func TestAppRemoveKeepsAppdataUnlessAskedTo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantQuery string
	}{
		{"without the flag", []string{"app", "remove", "plex"}, ""},
		{"with the flag", []string{"app", "remove", "plex", "--delete-appdata"}, "deleteAppdata=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRequest, gotQuery string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotRequest = r.Method + " " + r.URL.Path
				gotQuery = r.URL.RawQuery
				deleted := []string{}
				if r.URL.Query().Get("deleteAppdata") == "true" {
					deleted = append(deleted, "/mnt/user/appdata/plex")
				}
				writeJSON(t, w, http.StatusOK, &apiv1.RemoveAppResult{DeletedPaths: deleted})
			})
			printed, err := runAppCLI(t, sock, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if gotRequest != "DELETE /api/v1/apps/plex" {
				t.Fatalf("request = %q, want DELETE /api/v1/apps/plex", gotRequest)
			}
			if gotQuery != tc.wantQuery {
				t.Fatalf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
			if tc.wantQuery != "" && !strings.Contains(printed, "/mnt/user/appdata/plex") {
				t.Fatalf("output %q does not print the deleted paths", printed)
			}
		})
	}
}

func TestAppStatsPrintsTheUse(t *testing.T) {
	var gotRequest string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotRequest = r.Method + " " + r.URL.Path
		writeJSON(t, w, http.StatusOK, &apiv1.AppStats{At: time.Now().UTC(), CpuPercent: 12.5, MemoryBytes: 734003200, MemoryLimitBytes: 8589934592})
	})
	printed, err := runAppCLI(t, sock, "app", "stats", "plex")
	if err != nil {
		t.Fatalf("app stats: %v", err)
	}
	if gotRequest != "GET /api/v1/apps/plex/stats" {
		t.Fatalf("request = %q, want GET /api/v1/apps/plex/stats", gotRequest)
	}
	if !strings.Contains(printed, "12.5") || !strings.Contains(printed, "734003200") {
		t.Fatalf("output %q does not print the CPU and memory use", printed)
	}
}

func TestAppLogsReadsTheTailAndPrintsIt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		wantQuery string
	}{
		{"defaults", []string{"app", "logs", "plex"}, ""},
		{"tail", []string{"app", "logs", "plex", "--tail", "50"}, "tail=50"},
		{"tail zero", []string{"app", "logs", "plex", "--tail", "0"}, "tail=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotRequest, gotQuery string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotRequest = r.Method + " " + r.URL.Path
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "first line\nsecond line\n")
			})
			printed, err := runAppCLI(t, sock, tc.args...)
			if err != nil {
				t.Fatalf("%v: %v", tc.args, err)
			}
			if gotRequest != "GET /api/v1/apps/plex/logs" {
				t.Fatalf("request = %q, want GET /api/v1/apps/plex/logs", gotRequest)
			}
			if gotQuery != tc.wantQuery {
				t.Fatalf("query = %q, want %q", gotQuery, tc.wantQuery)
			}
			if printed != "first line\nsecond line\n" {
				t.Fatalf("output = %q, want the log text as sent", printed)
			}
		})
	}
}

func TestAppLogsRejectsAnOutOfRangeTail(t *testing.T) {
	for _, tail := range []string{"-1", "10001"} {
		if _, err := runAppCLI(t, "/nonexistent.sock", "app", "logs", "plex", "--tail", tail); err == nil || !strings.Contains(err.Error(), "--tail") {
			t.Fatalf("--tail %s = %v, want an error naming --tail", tail, err)
		}
	}
}

func TestAppLogsFollowAndJSONAreRefused(t *testing.T) {
	if _, err := runAppCLI(t, "/nonexistent.sock", "--json", "app", "logs", "plex", "--follow"); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("--follow with --json = %v, want an error naming --json", err)
	}
}

// followedLogs is `app logs --follow` running in a goroutine with stdout on
// a pipe, so a test can watch lines arrive while the command still runs.
type followedLogs struct {
	lines *bufio.Reader
	done  chan error
	unset func()
}

func followLogs(t *testing.T, sock string) *followedLogs {
	t.Helper()
	stdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	f := &followedLogs{lines: bufio.NewReader(r), done: make(chan error, 1)}
	f.unset = func() { os.Stdout = stdout; _ = w.Close() }
	root := rootCmd()
	root.SetArgs([]string{"--socket", sock, "app", "logs", "plex", "--follow", "--tail", "1"})
	var errBuf bytes.Buffer
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	go func() { f.done <- root.Execute() }()
	return f
}

func (f *followedLogs) readLine(t *testing.T) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		line, _ := f.lines.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		return line
	case <-time.After(10 * time.Second):
		t.Fatal("no log line arrived while the stream was open")
		return ""
	}
}

func (f *followedLogs) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		f.unset()
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the command did not end")
		return nil
	}
}

// TestAppLogsFollowStreamsIncrementallyAndEndsWhenTheServerCloses proves
// the first line reaches stdout while the server is still holding the
// stream open — which the generated client's own buffering of a text/plain
// body would never do — and that the command then ends cleanly, with no
// error, once the server closes the stream.
func TestAppLogsFollowStreamsIncrementallyAndEndsWhenTheServerCloses(t *testing.T) {
	release := make(chan struct{})
	var gotQuery string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "line one\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "line two\n")
	})
	f := followLogs(t, sock)
	defer f.unset()
	if got := f.readLine(t); got != "line one\n" {
		t.Fatalf("first streamed line = %q, want %q", got, "line one\n")
	}
	select {
	case err := <-f.done:
		t.Fatalf("the command ended (%v) while the server still held the stream open", err)
	default:
	}
	close(release)
	if got := f.readLine(t); got != "line two\n" {
		t.Fatalf("second streamed line = %q, want %q", got, "line two\n")
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("a stream the server closed cleanly ended with %v, want nil", err)
	}
	if gotQuery != "follow=true&tail=1" && gotQuery != "tail=1&follow=true" {
		t.Fatalf("query = %q, want follow=true and tail=1", gotQuery)
	}
}

// TestAppLogsFollowEndsCleanlyOnInterrupt sends the test process itself an
// interrupt once the stream is demonstrably open (so `--follow` has
// installed its handler): the command must return nil, not an error and
// not kill the process.
func TestAppLogsFollowEndsCleanlyOnInterrupt(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "line one\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	f := followLogs(t, sock)
	defer f.unset()
	if got := f.readLine(t); got != "line one\n" {
		t.Fatalf("first streamed line = %q, want %q", got, "line one\n")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("an interrupted --follow ended with %v, want nil", err)
	}
}

// TestAppLogsFollowBrokenStreamIsACommandFailure: a connection cut in the
// middle of the stream (no clean end) fails the command instead of looking
// like a finished log.
func TestAppLogsFollowBrokenStreamIsACommandFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "line one\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	f := followLogs(t, sock)
	defer f.unset()
	if err := f.wait(t); err == nil {
		t.Fatal("a stream cut mid-way ended with nil, want a command failure")
	}
}

// TestAppCommandsSurfaceTheServersErrorAsAFailure drives every subcommand
// against a 404 and requires a non-nil error carrying the server's code —
// for `--follow` too, where the error response is not a stream.
func TestAppCommandsSurfaceTheServersErrorAsAFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, &apiv1.Error{Code: "app_not_found", Message: "no such container"})
	})
	for _, args := range [][]string{
		{"app", "start", "nope"},
		{"app", "stop", "nope"},
		{"app", "restart", "nope"},
		{"app", "recreate", "nope"},
		{"app", "remove", "nope"},
		{"app", "remove", "nope", "--delete-appdata"},
		{"app", "stats", "nope"},
		{"app", "logs", "nope"},
		{"app", "logs", "nope", "--follow"},
	} {
		printed, err := runAppCLI(t, sock, args...)
		if err == nil {
			t.Fatalf("%v against a 404 = nil error, want a command failure (printed %q)", args, printed)
		}
		if !strings.Contains(err.Error(), "app_not_found") {
			t.Fatalf("%v error %q does not carry the server's code", args, err.Error())
		}
		if printed != "" {
			t.Fatalf("%v printed %q on failure, want nothing", args, printed)
		}
	}
}

func TestAppSubcommandsNeedExactlyOneID(t *testing.T) {
	for _, verb := range []string{"start", "stop", "restart", "recreate", "remove", "logs", "stats"} {
		if _, err := runAppCLI(t, "/nonexistent.sock", "app", verb); err == nil || strings.Contains(err.Error(), "could not connect") {
			t.Fatalf("app %s with no id = %v, want an argument error before any request", verb, err)
		}
	}
}
