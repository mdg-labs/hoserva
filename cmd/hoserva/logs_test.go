package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/google/uuid"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
)

// followJobLogRun is `logs --job ID --follow` running in a goroutine with
// stdout on a pipe, so a test can watch lines arrive while the command runs.
func followJobLogRun(t *testing.T, sock string, id uuid.UUID) *followedLogs {
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
	root.SetArgs([]string{"--socket", sock, "logs", "--job", id.String(), "--follow"})
	var errBuf bytes.Buffer
	root.SetOut(&errBuf)
	root.SetErr(&errBuf)
	go func() { f.done <- root.Execute() }()
	return f
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := io.WriteString(gw, text); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestLogsJobFollowStreamsIncrementallyAndEndsWithTheJob proves the first
// line reaches stdout while the server still holds the stream open, and that
// the command ends cleanly once the server finishes the gzip stream.
func TestLogsJobFollowStreamsIncrementallyAndEndsWithTheJob(t *testing.T) {
	id := uuid.New()
	release := make(chan struct{})
	var gotPath, gotQuery string
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/gzip")
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, "line one\n")
		_ = gw.Flush()
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(gw, "line two\n")
		_ = gw.Close()
	})
	f := followJobLogRun(t, sock, id)
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
		t.Fatalf("a stream the server finished cleanly ended with %v, want nil", err)
	}
	if gotPath != "/api/v1/jobs/"+id.String()+"/log" || gotQuery != "follow=true" {
		t.Fatalf("request = %s?%s, want the job log with follow=true", gotPath, gotQuery)
	}
}

// A server that closes the stream cleanly without the gzip trailer (a job
// whose log was never closed) ends the command with nil after printing
// everything it carried.
func TestLogsJobFollowEndsCleanlyWhenTheServerClosesWithoutATrailer(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, "only line\n")
		_ = gw.Flush()
	})
	f := followJobLogRun(t, sock, uuid.New())
	defer f.unset()
	if got := f.readLine(t); got != "only line\n" {
		t.Fatalf("streamed line = %q, want %q", got, "only line\n")
	}
	if err := f.wait(t); err != nil {
		t.Fatalf("a stream closed without a trailer ended with %v, want nil", err)
	}
}

// A connection cut mid-stream fails the command instead of looking like a
// finished log.
func TestLogsJobFollowBrokenStreamIsACommandFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, "line one\n")
		_ = gw.Flush()
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	f := followJobLogRun(t, sock, uuid.New())
	defer f.unset()
	if err := f.wait(t); err == nil {
		t.Fatal("a stream cut mid-way ended with nil, want a command failure")
	}
}

func TestLogsJobFollowRefusesABodyThatIsNotGzip(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.WriteString(w, "this is plain text, not a gzip stream\n")
	})
	f := followJobLogRun(t, sock, uuid.New())
	defer f.unset()
	if err := f.wait(t); err == nil {
		t.Fatal("a body that is not gzip ended with nil, want a command failure")
	}
}

// An interrupt, sent once the stream is demonstrably open, ends the command
// with nil.
func TestLogsJobFollowEndsCleanlyOnInterrupt(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		gw := gzip.NewWriter(w)
		_, _ = io.WriteString(gw, "line one\n")
		_ = gw.Flush()
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	f := followJobLogRun(t, sock, uuid.New())
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

func TestLogsJobFollowSurfacesTheServersErrorAsAFailure(t *testing.T) {
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusNotFound, &apiv1.Error{Code: "job_not_found", Message: "no such job"})
	})
	printed, err := runAppCLI(t, sock, "logs", "--job", uuid.NewString(), "--follow")
	if err == nil || !strings.Contains(err.Error(), "job_not_found") {
		t.Fatalf("--follow against a 404 = %v, want an error carrying the server's code", err)
	}
	if printed != "" {
		t.Fatalf("printed %q on failure, want nothing", printed)
	}
}

func TestLogsJobFollowAndJSONAreRefused(t *testing.T) {
	if _, err := runAppCLI(t, "/nonexistent.sock", "--json", "logs", "--job", uuid.NewString(), "--follow"); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("--follow with --json = %v, want an error naming --json", err)
	}
}

// Without --follow the command prints the log's decompressed text, in plain
// and --json output, and does not ask the server to follow.
func TestLogsJobWithoutFollowPrintsTheDecompressedText(t *testing.T) {
	body := gzipped(t, "finished log\n")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"plain", []string{"logs", "--job"}, "finished log\n"},
		{"json", []string{"--json", "logs", "--job"}, "\"finished log\\n\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/gzip")
				_, _ = w.Write(body)
			})
			printed, err := runAppCLI(t, sock, append(tc.args, uuid.NewString())...)
			if err != nil {
				t.Fatalf("logs --job: %v", err)
			}
			if printed != tc.want {
				t.Fatalf("printed %q, want %q", printed, tc.want)
			}
			if gotQuery != "" {
				t.Fatalf("query = %q, want none", gotQuery)
			}
		})
	}
}

// A log without its gzip trailer (a job still running) prints what it
// carried, across the members it is made of.
func TestLogsJobWithoutFollowPrintsALogWithoutATrailer(t *testing.T) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = io.WriteString(gw, "first\n")
	if err := gw.Flush(); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(gw, "second\n")
	if err := gw.Flush(); err != nil {
		t.Fatal(err)
	}
	body := buf.Bytes()
	sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(body)
	})
	printed, err := runAppCLI(t, sock, "logs", "--job", uuid.NewString())
	if err != nil {
		t.Fatalf("logs --job on a log without a trailer: %v", err)
	}
	if printed != "first\nsecond\n" {
		t.Fatalf("printed %q, want the text the log carried", printed)
	}
}

// A body that is not gzip is an error and never reaches stdout as raw bytes,
// including one shorter than a gzip header.
func TestLogsJobWithoutFollowRefusesABodyThatIsNotGzip(t *testing.T) {
	for _, body := range []string{"this is plain text, not a gzip stream\n", "oops\n"} {
		for _, args := range [][]string{{"logs", "--job"}, {"--json", "logs", "--job"}} {
			sock := serveAppAPI(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/gzip")
				_, _ = io.WriteString(w, body)
			})
			printed, err := runAppCLI(t, sock, append(args, uuid.NewString())...)
			if err == nil {
				t.Fatalf("%v on the body %q ended with nil, want a command failure", args, body)
			}
			if printed != "" {
				t.Fatalf("%v on the body %q printed %q, want nothing", args, body, printed)
			}
		}
	}
}
